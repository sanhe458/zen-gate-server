package lane

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNextSeqCriticalSection is the compile-coupled pin for the mint fix:
// nextSeq is the extracted check-reset-increment critical section, and it did
// not exist before the fix (the steps were inlined and racy), so this test
// cannot even build against the unfixed code. It also hammers the section
// concurrently — with the mutex removed, a lost increment or a handed-out
// sequence twice eventually surfaces here (the window is nanoseconds wide, so
// treat the concurrent half as a smoke guard, not a guarantee). Run with -race.
func TestNextSeqCriticalSection(t *testing.T) {
	// Sequential invariant: a new stamp restarts the sequence at 1, the
	// same stamp continues it, and no increment is ever lost.
	base := time.Now().UnixMilli()
	for stamp := int64(0); stamp < 4; stamp++ {
		first := nextSeq(base + stamp)
		if first != 1 {
			t.Fatalf("stamp %d: first sequence = %d, want 1", stamp, first)
		}
		for want := int64(2); want <= 50; want++ {
			if got := nextSeq(base + stamp); got != want {
				t.Fatalf("stamp %d: sequence = %d, want %d", stamp, got, want)
			}
		}
	}

	// Concurrent hammer: every caller for one stamp must observe a distinct
	// sequence, and the count must match exactly (no lost increments).
	const n = 16
	const per = 250 // n*per stays under the 12-bit seq field
	stamp := base + 100
	seqs := make([]int64, 0, n*per)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int64, 0, per)
			for j := 0; j < per; j++ {
				local = append(local, nextSeq(stamp))
			}
			mu.Lock()
			seqs = append(seqs, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	seen := make(map[int64]bool, len(seqs))
	for _, v := range seqs {
		if seen[v] {
			t.Fatalf("sequence %d handed out twice for one millisecond", v)
		}
		seen[v] = true
	}
	if len(seen) != n*per {
		t.Fatalf("distinct sequences = %d, want %d (an increment was lost)", len(seen), n*per)
	}
}

// TestMintSessionIdConcurrentUnique is the end-to-end guard for the same fix
// through the public API: concurrent mints for one millisecond must decode to
// distinct sequences. The seq field is 12 bits wide, so the hammer stays
// under 4096 mints or the encoding itself wraps and the check false-alarms.
// Run with -race.
func TestMintSessionIdConcurrentUnique(t *testing.T) {
	now := time.Now().UnixMilli()
	const n = 16
	const per = 250
	ids := make([]string, 0, n*per)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, per)
			for j := 0; j < per; j++ {
				local = append(local, MintSessionId(now))
			}
			mu.Lock()
			ids = append(ids, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		seq, ok := mintedSeqOf(id, now)
		if !ok {
			t.Fatalf("id %s does not carry the expected stamp", id)
		}
		if seen[seq] {
			t.Fatalf("sequence %d handed out twice for one millisecond (id %s)", seq, id)
		}
		seen[seq] = true
	}
	if len(seen) != n*per {
		t.Fatalf("distinct sequences = %d, want %d (a reset must not lose increments)", len(seen), n*per)
	}
}

// mintedSeqOf decodes the sequence counter out of a session id minted for
// nowMS: the first 12 hex chars are the low 48 bits of ^(nowMS<<12|seq&0xFFF)
// — the complement was taken over 64 bits before truncation, so the decode
// must complement inside the 48-bit window, and only the low 36 bits of the
// stamp survive.
func mintedSeqOf(id string, nowMS int64) (uint64, bool) {
	if !strings.HasPrefix(id, "ses_") || len(id) < 16 {
		return 0, false
	}
	raw, err := hex.DecodeString(id[4:16])
	if err != nil || len(raw) != 6 {
		return 0, false
	}
	var value uint64
	for _, b := range raw {
		value = value<<8 | uint64(b)
	}
	value = ^value & 0xFFFFFFFFFFFF
	if int64(value>>12) != nowMS&0xFFFFFFFFF {
		return 0, false
	}
	return value & 0xFFF, true
}

// TestDecoderMessagesStopReasonNotOverwritten pins the message_stop guard:
// Anthropic sends message_delta (with the real stop_reason) BEFORE
// message_stop, so an unconditional setFinish(FinishStop) on message_stop
// used to report a max_tokens truncation to the client as a clean finish.
func TestDecoderMessagesStopReasonNotOverwritten(t *testing.T) {
	dec := NewDecoder("messages", nil, func(Chunk) {})
	dec.Decode([]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`))
	dec.Decode([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`))
	dec.Decode([]byte(`{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`))
	dec.Decode([]byte(`{"type":"message_stop"}`))
	res := dec.Finish()
	if res.Finish != FinishMaxTokens {
		t.Fatalf("truncated turn must stay max-tokens, got %s", res.Finish)
	}

	// A stream that never carried a reason still defaults to stop.
	dec2 := NewDecoder("messages", nil, func(Chunk) {})
	dec2.Decode([]byte(`{"type":"message_stop"}`))
	if res2 := dec2.Finish(); res2.Finish != FinishStop {
		t.Fatalf("reason-less stream must default to stop, got %s", res2.Finish)
	}
}

// TestDecoderChatToolContinuationWithoutID pins the deterministic fallback:
// a bare argument fragment (no index, no id) must attach to the most recently
// opened tool block. The old code iterated the blocks map — random order — so
// with several tools open the fragment could land on any of them.
func TestDecoderChatToolContinuationWithoutID(t *testing.T) {
	const open = 8
	argsByIdx := map[int]string{}
	dec := NewDecoder("chat", nil, func(c Chunk) {
		if c.Kind == ChunkToolCallDelta {
			argsByIdx[c.Index] += c.Delta
		}
	})
	// eight parallel calls, each opened by id (no index)
	for i := 0; i < open; i++ {
		frame := `{"choices":[{"delta":{"tool_calls":[{"id":"call_` +
			string(rune('a'+i)) + `","function":{"name":"bash","arguments":"{\"x` +
			string(rune('0'+i)) + `\":"}}]}}]}`
		dec.Decode([]byte(frame))
	}
	// bare continuations, no id and no index: every one belongs to the LAST
	// opened tool block (highest index).
	for i := 0; i < open; i++ {
		dec.Decode([]byte(`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]}}]}`))
	}
	last := open - 1
	want := `{"x` + string(rune('0'+last)) + `":` + strings.Repeat("1}", open)
	if got := argsByIdx[last]; got != want {
		t.Fatalf("bare continuations must all attach to block %d: got %q, want %q", last, got, want)
	}
	for i := 0; i < last; i++ {
		if got := argsByIdx[i]; strings.Contains(got, "1}") {
			t.Fatalf("a bare continuation landed on block %d (%q), not the most recent one", i, got)
		}
	}
}

// TestSetProxyConcurrentWithRequests is the race-detector guard for the
// transport swap: SetProxy used to write laneHTTP.Transport while Do() read
// it. The transport is now stable and the egress decider swaps atomically.
// Run with -race.
func TestSetProxyConcurrentWithRequests(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	old := UpstreamBase
	UpstreamBase = up.URL
	defer func() { UpstreamBase = old }()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// config writer: hot-swap the egress while traffic flows
	wg.Add(1)
	go func() {
		defer wg.Done()
		modes := []struct{ mode, url string }{
			{"direct", ""}, {"env", ""}, {"custom", up.URL}, {"pool", ""}, {"system", ""},
		}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m := modes[i%len(modes)]
			SetProxy(m.mode, m.url)
			SetActiveProxy(nil)
		}
	}()
	// readers: GetJSON shares the same transport
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out map[string]any
			for j := 0; j < 50; j++ {
				_ = GetJSON(context.Background(), "/v1/models", "ses_x", "msg_x", &out)
			}
		}()
	}
	// small sleep to let the writers and readers overlap, then stop
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	// leave the package in the default state for other tests
	SetProxy("env", "")
}

// TestDetectEgressHangingServerTimesOut pins the per-attempt timeout: the
// shared client has no overall timeout, so a hanging echo service used to
// block the probe round forever. Run with -race.
func TestDetectEgressHangingServerTimesOut(t *testing.T) {
	old := egressAttemptTimeout
	egressAttemptTimeout = 150 * time.Millisecond
	defer func() { egressAttemptTimeout = old }()

	// A server that accepts and then never answers, for every echo service.
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hung.Close()

	// The hanging fixture must own every attempt: with the real echo
	// endpoints in the list, an internet-connected runner would occasionally
	// get a live answer first and break the assertion below.
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"203.0.113.7"}`))
	}))
	defer live.Close()
	oldSvc := echoServices
	echoServices = []struct {
		url   string
		parse func([]byte) Egress
	}{
		{url: hung.URL, parse: func(b []byte) Egress { return Egress{IP: "203.0.113.7"} }},
		{url: hung.URL, parse: func(b []byte) Egress { return Egress{IP: "203.0.113.7"} }},
		{url: hung.URL, parse: func(b []byte) Egress { return Egress{IP: "203.0.113.7"} }},
	}
	defer func() { echoServices = oldSvc }()

	client := &http.Client{} // no timeout of its own, like laneHTTP
	done := make(chan Egress, 1)
	go func() { done <- DetectEgressVia(context.Background(), client) }()
	select {
	case e := <-done:
		if e.IP != "" {
			t.Fatalf("a hanging echo service must not yield an egress, got %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DetectEgressVia blocked on a hanging server: per-attempt timeout missing")
	}
}

// TestThrottleSnapshotsDoNotAliasLedger pins the deep-copy fix: values that
// leave the book (fire callbacks, Note, All) used to share the Episodes
// backing array with the ledger, which mutates those elements in place
// (closing open episodes). Run with -race.
func TestThrottleSnapshotsDoNotAliasLedger(t *testing.T) {
	b := NewThrottleBook()
	var fired []ThrottleNote
	var mu sync.Mutex
	b.OnUpdate = func(model string, note ThrottleNote) {
		mu.Lock()
		fired = append(fired, note)
		mu.Unlock()
	}
	b.MarkThrottled("m", 60)
	b.MarkOK("m")

	note := b.Note("m")
	if len(note.Episodes) == 0 {
		t.Fatal("episode history must survive the throttle cycle")
	}
	// Mutating a returned snapshot must not reach back into the ledger.
	note.Episodes[0].Start = 1
	note.Episodes[0].End = 2
	if again := b.Note("m"); again.Episodes[0].Start == 1 || again.Episodes[0].End == 2 {
		t.Fatal("Note() returned a snapshot aliasing the ledger's Episodes array")
	}
	all := b.All()
	all["m"].Episodes[0].Start = 3
	if again := b.All(); again["m"].Episodes[0].Start == 3 {
		t.Fatal("All() returned a snapshot aliasing the ledger's Episodes array")
	}

	// Concurrent readers while the ledger mutates: the race detector is the
	// assertion here.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = b.All()
				_ = b.Note("m")
				_ = b.RecoveryETA("m")
				_ = b.Throttled("m")
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.MarkThrottled("m", 0)
				b.MarkOK("m")
			}
		}()
	}
	wg.Wait()
}

// TestSetProxyCustomDeciderReachesTransport checks the rebuilt egress path:
// a custom proxy URL must be the one the per-request decider returns, and the
// decider must be swappable while the transport itself never changes.
func TestSetProxyCustomDeciderReachesTransport(t *testing.T) {
	SetProxy("custom", "127.0.0.1:9999")
	defer SetProxy("env", "")

	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	f := proxyDecider.Load()
	if f == nil {
		t.Fatal("decider must be installed")
	}
	u, err := (*f)(req)
	if err != nil {
		t.Fatalf("custom decider errored: %v", err)
	}
	if u == nil || u.Host != "127.0.0.1:9999" || u.Scheme != "http" {
		t.Fatalf("custom decider returned %v, want http://127.0.0.1:9999", u)
	}

	// An unparsable URL falls back to the environment decider, matching the
	// old transport-rebuild behaviour. (Compared by function pointer: the
	// env result itself depends on cached process-wide state.)
	SetProxy("custom", "://nope")
	f2 := proxyDecider.Load()
	if f2 == nil || reflect.ValueOf(*f2).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Fatal("unparsable custom URL must fall back to the environment decider")
	}
}

// TestLaneSettingsConcurrentWithListings is the race-detector guard for the
// exposeRegion fix: the admin settings handler writes lane settings while
// /v1 listings read them. ServableModels read l.exposeRegion outside the
// mutex, which the race detector flags the moment a settings save overlaps a
// model listing (found by stress-running the real server with -race).
// Run with -race.
func TestLaneSettingsConcurrentWithListings(t *testing.T) {
	l := NewLane()
	// Seed a catalog with one region-blocked and one healthy model.
	l.mu.Lock()
	l.catalog = []ModelInfo{
		{ID: "healthy-free", Name: "healthy", Wire: "chat"},
		{ID: "blocked-free", Name: "blocked", Wire: "chat"},
	}
	l.availability = map[string]ProbeResult{
		"healthy-free": {Model: "healthy-free", State: StateAvailable},
		"blocked-free": {Model: "blocked-free", State: StateRegionBlock},
	}
	l.mu.Unlock()

	// Sequential behaviour: the flag decides whether the blocked id shows.
	l.SetExposeRegion(false)
	for _, m := range l.ServableModels() {
		if m.ID == "blocked-free" {
			t.Fatal("region-blocked model listed while exposeRegion is off")
		}
	}
	l.SetExposeRegion(true)
	found := false
	for _, m := range l.ServableModels() {
		if m.ID == "blocked-free" {
			found = true
		}
	}
	if !found {
		t.Fatal("region-blocked model missing from listing while exposeRegion is on")
	}

	// Concurrent settings writes vs listings: the race detector is the
	// assertion (the old lock-free read tripped it within seconds).
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			l.SetExposeRegion(i%2 == 0)
			l.SetDefaultMaxTokens(1024 + i%1024)
			l.SetFailover(i%2 == 0, 1+i%5)
		}
	}()
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = l.ServableModels()
				_, _, _ = l.Snapshot()
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestCallRecordOncePerPhysicalRequest pins the usage-accounting fix: the
// record for a completed attempt was emitted twice — once unconditionally
// before the outcome branches and once on the success path — so every
// finished turn doubled in the stats (requests, input/output tokens and the
// recent-calls list the dashboard renders). One physical upstream request
// must produce exactly one CallRecord.
func TestCallRecordOncePerPhysicalRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, chatSSE(
			`{"choices":[{"delta":{"role":"assistant","content":"hi"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
		))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	l := NewLane()
	var calls []CallRecord
	l.OnCall = func(rec CallRecord) { calls = append(calls, rec) }

	out, uerr := l.Complete(context.Background(), Request{
		Model:    "mimo-v2.6-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}}},
	}, func(Chunk) {})
	if uerr != nil {
		t.Fatalf("Complete: %v", uerr)
	}
	if out.Finish != FinishStop {
		t.Fatalf("finish = %q", out.Finish)
	}
	if len(calls) != 1 {
		t.Fatalf("one upstream request produced %d records, want 1: %+v", len(calls), calls)
	}
	if !calls[0].Ok || calls[0].Input != 5 || calls[0].Output != 3 {
		t.Fatalf("record mangled: %+v", calls[0])
	}
}

// TestStreamReaderWatcherExitsWithReader pins the goroutine leak: the pump
// spawned a watcher that only waited on ctx.Done(). With a context that never
// cancels — context.Background(), which the on-demand /admin probe passes —
// ctx.Done() is a nil channel, so every single call leaked that goroutine for
// the life of the process. It must also exit when the reader is closed, which
// is what this counts: N calls must not leave N goroutines behind.
func TestStreamReaderWatcherExitsWithReader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, chatSSE(
			`{"choices":[{"delta":{"content":"hi"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	body := map[string]any{"model": "m", "stream": true}
	// warm the transport up so its own lazily-started goroutines are not
	// counted as a leak
	if _, err := PostStreamed(context.Background(), "/zen/v1/chat/completions", body,
		"ses_warm", "msg_warm", func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	const n = 20
	for i := 0; i < n; i++ {
		if _, err := PostStreamed(context.Background(), "/zen/v1/chat/completions", body,
			"ses_leak", "msg_leak", func([]byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	leaked := 0
	for i := 0; i < 40; i++ {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		if leaked = runtime.NumGoroutine() - before; leaked <= 2 {
			return
		}
	}
	t.Fatalf("%d calls leaked %d goroutines (watcher does not exit with the reader)", n, leaked)
}

// TestToolResultInUserTurnProjectedToEveryWire pins the cross-protocol fix:
// the Anthropic wire carries tool results as tool_result blocks on a *user*
// turn (there is no tool role). ToChatMessages/ToResponseInput only looked for
// them on a tool-role message, so a Claude-protocol client talking to a
// chat- or responses-wire model had every tool result silently dropped — and
// an empty user turn appended in its place. Upstream then answers 400 for the
// unanswered tool_call.
func TestToolResultInUserTurnProjectedToEveryWire(t *testing.T) {
	unified := RepairToolPairing([]Message{
		{Role: RoleUser, Parts: []Part{TextPart{Text: "list files"}}},
		{Role: RoleAssistant, Parts: []Part{ToolCallPart{ID: "toolu_1", Name: "bash", Arguments: `{"cmd":"ls"}`}}},
		{Role: RoleUser, Parts: []Part{
			ToolResultPart{ToolCallID: "toolu_1", Text: "a.txt"},
			TextPart{Text: "go on"},
		}},
	})

	chat, _ := json.Marshal(ToChatMessages(unified))
	if !strings.Contains(string(chat), `"role":"tool"`) || !strings.Contains(string(chat), "a.txt") {
		t.Errorf("chat wire lost the tool result: %s", chat)
	}
	if strings.Contains(string(chat), `"content":"","role":"user"`) {
		t.Errorf("chat wire appended an empty user turn: %s", chat)
	}
	// the tool message must follow the assistant tool_call directly, ahead of
	// the user text of the same turn, or upstream rejects the request
	toolAt := strings.Index(string(chat), `"role":"tool"`)
	textAt := strings.Index(string(chat), "go on")
	if toolAt < 0 || textAt < 0 || toolAt > textAt {
		t.Errorf("chat wire ordered the tool result after the user text: %s", chat)
	}

	_, resp := ToResponseInput(unified)
	raw, _ := json.Marshal(resp)
	if !strings.Contains(string(raw), "function_call_output") || !strings.Contains(string(raw), "a.txt") {
		t.Errorf("responses wire lost the tool result: %s", raw)
	}
	outAt := strings.Index(string(raw), "function_call_output")
	textAt = strings.Index(string(raw), "go on")
	if outAt < 0 || textAt < 0 || outAt > textAt {
		t.Errorf("responses wire ordered function_call_output after the user item: %s", raw)
	}

	// The native Anthropic projection was already correct and must stay so.
	_, claude := ToClaudeMessages(unified)
	craw, _ := json.Marshal(claude)
	if !strings.Contains(string(craw), "tool_result") || !strings.Contains(string(craw), "a.txt") {
		t.Errorf("claude wire lost the tool result: %s", craw)
	}
}

// TestServableModelsFallbackKeepsDecisionModelsOut pins the fallback fix: when
// every model is unavailable or region-gated the picker falls back to the
// whole catalog, which also re-admitted the System One decision models the
// main loop deliberately skips — a chat request against one can only fail.
func TestServableModelsFallbackKeepsDecisionModelsOut(t *testing.T) {
	l := NewLane()
	l.mu.Lock()
	l.catalog = BuildCatalog([]string{"mimo-v2.6-flash-free", "jev-1.13-free"})
	l.availability = map[string]ProbeResult{
		"mimo-v2.6-flash-free": {Model: "mimo-v2.6-flash-free", State: StateUnavailable},
		"jev-1.13-free":        {Model: "jev-1.13-free", State: StateUnavailable},
	}
	l.mu.Unlock()

	got := l.ServableModels()
	for _, m := range got {
		if m.SystemOne {
			t.Fatalf("fallback advertised the decision model %s as a chat model", m.ID)
		}
	}
	if len(got) != 1 {
		t.Fatalf("fallback listed %d models, want the 1 non-decision model", len(got))
	}
}
