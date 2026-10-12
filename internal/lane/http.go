package lane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	io "io"
	"net/http"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	regionRe   = regexp.MustCompile(`(?i)RegionError|not available in your country|region.?block`)
	quotaRe    = regexp.MustCompile(`(?i)FreeUsageLimitError|usage limit|rate limit`)
	modelErrRe = regexp.MustCompile(`(?i)ModelError|model is unavailable|model is not supported|not supported|Endpoint is unavailable`)
)

// ClassifyFailure maps an upstream status/error payload onto a code.
// Payload patterns are checked before status codes, matching the reference
// implementation: a reverse proxy's generic 503 "Service Unavailable" reason
// phrase must not be read as a per-model verdict.
func ClassifyFailure(status int, payload string, retryAfterSec int) *UpstreamError {
	e := &UpstreamError{Message: truncate(payload, 300), RetryAfter: retryAfterSec}
	switch {
	case regionRe.MatchString(payload):
		e.Code = CodeRegion
	case status == 429 || quotaRe.MatchString(payload):
		e.Code = CodeQuota
		if e.RetryAfter == 0 {
			e.RetryAfter = 60
		}
	// A payload that names the model as unsupported/unknown is a per-model
	// verdict, not a credential problem — the gateway delivers it under 401
	// (observed) as well as 400/404. Check it before the status codes, or the
	// 401 branch below would swallow it: the model would be reported to the
	// client as a bogus authentication error and a healthy model would never
	// be failed over to.
	case status >= 400 && status < 500 && modelErrRe.MatchString(payload):
		e.Code = CodeServer
		e.Unavailable = true
	case status == 401 || status == 403:
		e.Code = CodeCredential
	case status >= 500:
		e.Code = CodeServer
	case status == 404 || status == 400 || status == 422 || modelErrRe.MatchString(payload):
		e.Code = CodeServer
		e.Unavailable = true
	default:
		e.Code = CodeServer
	}
	return e
}

// laneTransport is the process-wide upstream transport. It is created once
// and never swapped: its Proxy hook is fixed at init and consults the
// current egress decider through an atomic pointer, so changing the proxy
// setting is a pointer swap — no data race on the Transport field and no
// dropped connection pool.
var laneTransport = &http.Transport{
	MaxIdleConns:          128,
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
}

// proxyDecider selects the egress URL for one request; swapped atomically by
// SetProxy. It starts on the environment, matching http.DefaultTransport's
// behaviour before any explicit configuration.
var proxyDecider atomic.Pointer[func(*http.Request) (*neturl.URL, error)]

func init() {
	env := http.ProxyFromEnvironment
	proxyDecider.Store(&env)
	laneTransport.Proxy = func(req *http.Request) (*neturl.URL, error) {
		if f := proxyDecider.Load(); f != nil {
			return (*f)(req)
		}
		return nil, nil
	}
}

// laneHTTP is the shared client for all upstream traffic; it always rides
// laneTransport, whose egress is re-pointed hot by SetProxy.
var laneHTTP = &http.Client{Transport: laneTransport}

// dynamicProxy is the proxy selected by the pool at runtime. In "pool" mode
// the transport consults it on every request, so switching egress costs
// nothing — no transport rebuild, no dropped connections.
// nil = direct (the pool always starts on the local connection).
var dynamicProxy atomic.Pointer[neturl.URL]

// SetActiveProxy installs the egress the pool currently wants to use.
// nil selects the local (direct) connection.
func SetActiveProxy(u *neturl.URL) { dynamicProxy.Store(u) }

// ActiveProxy returns the pool-selected proxy, or nil for direct.
func ActiveProxy() *neturl.URL { return dynamicProxy.Load() }

// SetProxy reconfigures the shared client: "env" honors HTTP(S)_PROXY,
// "system" follows the desktop system proxy, "direct" bypasses, "custom" uses
// the given URL, "pool" defers to whatever the proxy pool selected.
func SetProxy(mode, url string) {
	switch mode {
	case "direct":
		storeDecider(func(*http.Request) (*neturl.URL, error) { return nil, nil })
	case "custom":
		u, err := parseProxyURL(url)
		if err != nil {
			storeDecider(http.ProxyFromEnvironment)
		} else {
			storeDecider(func(*http.Request) (*neturl.URL, error) { return u, nil })
		}
	case "system":
		storeDecider(func(*http.Request) (*neturl.URL, error) { return SystemProxyURL(), nil })
	case "pool":
		storeDecider(func(*http.Request) (*neturl.URL, error) { return dynamicProxy.Load(), nil })
	default:
		storeDecider(http.ProxyFromEnvironment)
	}
}

func storeDecider(f func(*http.Request) (*neturl.URL, error)) { proxyDecider.Store(&f) }

// Client returns the shared upstream HTTP client (proxy-aware).
func Client() *http.Client { return laneHTTP }

func parseProxyURL(raw string) (*neturl.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty proxy url")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return neturl.Parse(raw)
}

// ParseProxyURL is the exported form of the normalisation SetProxy applies to
// a user-entered address: a scheme-less "host:port" is accepted and read as
// HTTP. Callers that validate a proxy before it is saved (the dashboard's
// connectivity test) must use it too, or they end up rejecting the very
// spelling the gateway happily accepts.
func ParseProxyURL(raw string) (*neturl.URL, error) {
	u, err := parseProxyURL(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("proxy url has no host")
	}
	return u, nil
}

// PostStreamed posts one completion request and delivers decoded `data:`
// payloads to onData. The first ≤4 KiB are sniffed by body shape — the gateway
// sometimes answers a stream request with SSE under an application/json
// content-type — and the sniffed bytes are replayed ahead of the live reader.
// An idle deadline resets on every chunk; total duration is unbounded.
func PostStreamed(ctx context.Context, path string, body map[string]any, session, requestID string, onData func(payload []byte) error) (*Usage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, UpstreamBase+path, bytes.NewReader(payload))
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	for k, v := range GatewayHeaders(session, requestID, true, "") {
		req.Header.Set(k, v)
	}
	resp, err := laneHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		// never reached upstream: the path itself (proxy included) is broken
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error(), Egress: true}
	}
	defer resp.Body.Close()

	retryAfter := 0
	if ra := resp.Header.Get("retry-after"); ra != "" {
		if n, perr := strconv.Atoi(ra); perr == nil && n > 0 && n < 3600 {
			retryAfter = n
		}
	}

	// One pump owns resp.Body for the whole call; the head bytes are read
	// through it for shape inspection and then unread so decoding sees the
	// full stream.
	pump := newStreamReader(ctx, resp.Body, 5*time.Minute)
	defer pump.Close()
	head := readUpTo(pump, 4096, 20*time.Second)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rest, _ := io.ReadAll(io.LimitReader(pump, 1<<20))
		return nil, ClassifyFailure(resp.StatusCode, string(head)+string(rest), retryAfter)
	}

	switch sniffBody(head) {
	case "empty":
		return nil, &UpstreamError{Code: CodeEmpty, Message: "empty response body"}
	case "json", "unknown":
		rest, _ := io.ReadAll(io.LimitReader(pump, 8<<20))
		full := string(head) + string(rest)
		var parsed any
		if jerr := json.Unmarshal([]byte(full), &parsed); jerr != nil {
			return nil, &UpstreamError{Code: CodeServer, Message: truncate(full, 300)}
		}
		if m, ok := parsed.(map[string]any); ok {
			if _, has := m["error"]; has {
				return nil, ClassifyFailure(resp.StatusCode, jsonString(m["error"]), retryAfter)
			}
		}
		if uerr := onData([]byte(full)); uerr != nil {
			return nil, uerr
		}
		if m, ok := parsed.(map[string]any); ok {
			return usageFromBody(m), nil
		}
		return nil, nil
	}

	pump.Unread(head)
	// SSE body (whatever the header claimed).
	usage, err := readSSE(pump, onData)
	if err != nil {
		if ctx.Err() != nil {
			return usage, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		if ue, ok := err.(*UpstreamError); ok {
			return usage, ue
		}
		return usage, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	return usage, nil
}

// GetJSON performs a fingerprinted GET (model listing).
func GetJSON(ctx context.Context, path string, session, requestID string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UpstreamBase+path, nil)
	if err != nil {
		return err
	}
	for k, v := range GatewayHeaders(session, requestID, false, "application/json") {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneTransport}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ClassifyFailure(resp.StatusCode, string(data), 0)
	}
	return json.Unmarshal(data, out)
}

// --- body-shape sniffing -------------------------------------------------

// sniffBody classifies a response by body shape, not Content-Type:
// 'empty' | 'sse' | 'json' | 'unknown'.
func sniffBody(head []byte) string {
	trimmed := strings.TrimLeft(string(head), " \t\r\n")
	if trimmed == "" {
		return "empty"
	}
	if strings.HasPrefix(trimmed, "data:") || strings.HasPrefix(trimmed, "event:") ||
		strings.HasPrefix(trimmed, "retry:") || strings.HasPrefix(trimmed, "id:") ||
		strings.HasPrefix(trimmed, ":") {
		return "sse"
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return "json"
	}
	return "unknown"
}

// readUpTo consumes up to n bytes from the pump within the deadline. The
// bytes are returned for shape inspection and must be Unread back before the
// stream is decoded, so nothing is lost.
func readUpTo(s *streamReader, n int, deadline time.Duration) []byte {
	buf := make([]byte, 0, n)
	tmp := make([]byte, 2048)
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for len(buf) < n {
		select {
		case <-timer.C:
			return buf
		default:
		}
		k, err := s.ReadTimeout(tmp, deadline)
		if k > 0 {
			buf = append(buf, tmp[:k]...)
		}
		if err != nil {
			return buf
		}
		switch sniffBody(buf) {
		case "sse", "json":
			return buf
		}
	}
	return buf
}

// --- stream pump with idle timeout ---------------------------------------

type streamChunk struct {
	data []byte
	err  error
}

// streamReader turns a blocking body into a channel-fed reader that errors
// when no data arrives within the idle timeout.
type streamReader struct {
	ch      chan streamChunk
	src     io.Reader
	closed  atomic.Bool
	once    sync.Once
	done    chan struct{} // closed by Close; unblocks the pump goroutine
	mu      sync.Mutex
	pending []byte
	idle    time.Duration
}

func newStreamReader(ctx context.Context, src io.Reader, idle time.Duration) *streamReader {
	s := &streamReader{ch: make(chan streamChunk, 512), src: src, idle: idle, done: make(chan struct{})}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			k, err := src.Read(buf)
			if k > 0 {
				cp := make([]byte, k)
				copy(cp, buf[:k])
				if !s.send(streamChunk{cp, nil}) {
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					s.send(streamChunk{nil, io.EOF})
				} else {
					s.send(streamChunk{nil, err})
				}
				return
			}
			if s.closed.Load() {
				return
			}
		}
	}()
	go func() {
		// Also exit when the reader is closed: with a context that never
		// cancels (context.Background(), used by the on-demand /admin probe)
		// ctx.Done() is a nil channel and blocking on it alone would leak this
		// goroutine for every single call.
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.done:
		}
	}()
	return s
}

// send hands one chunk to the consumer, giving up as soon as the reader is
// closed. A bare `s.ch <-` would block forever once the consumer stopped
// reading (e.g. after an idle timeout) and leak this goroutine.
func (s *streamReader) send(c streamChunk) bool {
	select {
	case s.ch <- c:
		return true
	case <-s.done:
		return false
	}
}

func (s *streamReader) Close() {
	s.once.Do(func() {
		s.closed.Store(true)
		close(s.done)
		if c, ok := s.src.(io.Closer); ok {
			_ = c.Close()
		}
	})
}

func (s *streamReader) Read(p []byte) (int, error) {
	return s.readWithTimeout(p, s.idle)
}

// ReadTimeout is Read with an explicit deadline for this call.
func (s *streamReader) ReadTimeout(p []byte, timeout time.Duration) (int, error) {
	return s.readWithTimeout(p, timeout)
}

// Unread pushes already-consumed bytes back to the front of the stream.
func (s *streamReader) Unread(data []byte) {
	s.mu.Lock()
	s.pending = append(append([]byte{}, data...), s.pending...)
	s.mu.Unlock()
}

func (s *streamReader) readWithTimeout(p []byte, timeout time.Duration) (int, error) {
	s.mu.Lock()
	if len(s.pending) > 0 {
		k := copy(p, s.pending)
		s.pending = s.pending[k:]
		s.mu.Unlock()
		return k, nil
	}
	s.mu.Unlock()

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	for {
		select {
		case <-timeoutCh:
			s.Close()
			return 0, errors.New("upstream idle timeout")
		case c := <-s.ch:
			if c.err != nil {
				if errors.Is(c.err, io.EOF) {
					return 0, io.EOF
				}
				return 0, c.err
			}
			s.mu.Lock()
			s.pending = append(s.pending, c.data...)
			k := copy(p, s.pending)
			s.pending = s.pending[k:]
			s.mu.Unlock()
			return k, nil
		}
	}
}

// readSSE parses `data:` frames, dropping comments and [DONE]. Usage-bearing
// frames are merged into the returned total (each field appears on at most a
// couple of terminal frames, so summation matches the reference behaviour).
// A final line without its terminating newline (a stream cut mid-frame) is
// still processed: dropping it could lose the last usage or finish frame.
func readSSE(r io.Reader, onData func([]byte) error) (*Usage, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var usage *Usage
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return usage, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" || strings.HasPrefix(trimmed, ":") || !strings.HasPrefix(trimmed, "data:") {
			if err == io.EOF {
				return usage, nil
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" {
			if err == io.EOF {
				return usage, nil
			}
			continue
		}
		if payload == "[DONE]" {
			return usage, nil
		}
		var frame map[string]any
		if json.Unmarshal([]byte(payload), &frame) == nil {
			if u := mapUsage(frame); u != nil {
				switch {
				case usage == nil:
					usage = u
				case jsonString(frame["type"]) == "message_delta":
					// Anthropic's terminal message_delta repeats the
					// *cumulative* output count, so it overrides the seed that
					// message_start already reported instead of adding to it.
					usage.Override(u)
				default:
					usage.Merge(u)
				}
			}
			if _, has := frame["error"]; has {
				return usage, ClassifyFailure(0, jsonString(frame["error"]), 0)
			}
		}
		if derr := onData([]byte(payload)); derr != nil {
			return usage, derr
		}
		if err == io.EOF {
			return usage, nil
		}
	}
}

// usageFromBody extracts usage from a non-streamed JSON answer.
func usageFromBody(m map[string]any) *Usage {
	return mapUsage(m)
}

// mapUsage normalizes one usage frame; returns nil when nothing is present.
func mapUsage(frame map[string]any) *Usage {
	u := &Usage{}
	changed := false
	if m, ok := frame["usage"].(map[string]any); ok {
		changed = num(&u.Input, m, "prompt_tokens", "input_tokens") || changed
		changed = num(&u.Output, m, "completion_tokens", "output_tokens") || changed
		changed = num(&u.CacheRead, m, "cache_read_input_tokens", "cached_tokens", "cache_read_tokens") || changed
		changed = num(&u.CacheWrite, m, "cache_creation_input_tokens", "cache_write_tokens") || changed
		changed = num(&u.TotalTokens, m, "total_tokens") || changed
		if rd, ok := m["reasoning_tokens"]; ok {
			if f, ok := toNum(rd); ok && f >= 0 {
				u.Reasoning = int(f)
				changed = true
			}
		}
	}
	// Anthropic message_start carries usage at the top of the message object.
	if msg, ok := frame["message"].(map[string]any); ok {
		if mm, ok := msg["usage"].(map[string]any); ok {
			changed = num(&u.Input, mm, "input_tokens") || changed
			changed = num(&u.Output, mm, "output_tokens") || changed
			changed = num(&u.CacheRead, mm, "cache_read_input_tokens") || changed
			changed = num(&u.CacheWrite, mm, "cache_creation_input_tokens") || changed
		}
	}
	if !changed {
		return nil
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.Input + u.Output
	}
	return u
}

func num(dst *int, m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if f, ok := toNum(v); ok && f >= 0 {
				*dst += int(f)
				return true
			}
		}
	}
	return false
}

func toNum(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// Merge folds one partial usage into another (fields present on either side).
func (u *Usage) Merge(o *Usage) {
	if o == nil {
		return
	}
	u.Input += o.Input
	u.Output += o.Output
	u.Reasoning += o.Reasoning
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.TotalTokens += o.TotalTokens
}

// Override folds in a cumulative (terminal) usage frame: every counter the
// frame actually carries replaces the accumulated value rather than adding to
// it, which is what a "this is the final total" frame means.
func (u *Usage) Override(o *Usage) {
	if o == nil {
		return
	}
	if o.Input != 0 {
		u.Input = o.Input
	}
	if o.Output != 0 {
		u.Output = o.Output
	}
	if o.Reasoning != 0 {
		u.Reasoning = o.Reasoning
	}
	if o.CacheRead != 0 {
		u.CacheRead = o.CacheRead
	}
	if o.CacheWrite != 0 {
		u.CacheWrite = o.CacheWrite
	}
	u.TotalTokens = u.Input + u.Output
}

// jsonString reads a JSON field as a string. A missing/null field is "" —
// marshaling nil would yield the literal "null", which callers guarding on
// "" (optional ids, names, argument fragments, deltas) would then leak
// verbatim into the projected request or response.
func jsonString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
