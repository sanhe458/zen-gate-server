package lane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestQuotaHookFiresOn429 proves the proxy pool gets told when live traffic is
// refused for quota — that hook is what drives automatic egress rotation.
func TestQuotaHookFiresOn429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"FreeUsageLimitError: usage limit reached for this ip"}}`))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	l := NewLane()
	l.SetFailover(false, 1) // take the first verdict, no model failover

	var fired atomic.Int32
	var seenModel atomic.Value
	var seenRetry atomic.Int64
	l.OnQuotaExhausted = func(model string, retryAfter int) {
		fired.Add(1)
		seenModel.Store(model)
		seenRetry.Store(int64(retryAfter))
	}

	_, uerr := l.Complete(context.Background(), Request{
		Model:    "mimo-v2.6-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}}},
	}, func(Chunk) {})

	if uerr == nil {
		t.Fatal("expected an upstream error")
	}
	if uerr.Code != CodeQuota {
		t.Fatalf("expected %s, got %s (%s)", CodeQuota, uerr.Code, uerr.Message)
	}

	// the hook is dispatched on its own goroutine
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("OnQuotaExhausted did not fire on a 429")
	}
	if got, _ := seenModel.Load().(string); got != "mimo-v2.6-flash-free" {
		t.Errorf("hook model = %q, want the refused model id", got)
	}
	if seenRetry.Load() <= 0 {
		t.Errorf("hook retryAfter = %d, want the upstream hint", seenRetry.Load())
	}
}

// TestQuotaHookSilentOnSuccess keeps the hook from firing on healthy traffic.
func TestQuotaHookSilentOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	l := NewLane()
	var fired atomic.Int32
	l.OnQuotaExhausted = func(string, int) { fired.Add(1) }

	if _, uerr := l.Complete(context.Background(), Request{
		Model:    "mimo-v2.6-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}}},
	}, func(Chunk) {}); uerr != nil {
		t.Fatalf("unexpected error: %v", uerr)
	}
	time.Sleep(200 * time.Millisecond)
	if fired.Load() != 0 {
		t.Errorf("OnQuotaExhausted fired %d times on a successful call", fired.Load())
	}
}

// TestQuotaRotateRetriesInPlace pins the no-error-on-quota behavior: when the
// synchronous rotate hook reports a fresh egress, the same turn is retried in
// place and the caller gets a normal response instead of the 429.
func TestQuotaRotateRetriesInPlace(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"FreeUsageLimitError: usage limit reached for this ip"}}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	l := NewLane()
	var rotates atomic.Int32
	l.OnQuotaRotate = func(string) bool {
		rotates.Add(1)
		return true // egress just moved: retry the same turn
	}

	var text string
	out, uerr := l.Complete(context.Background(), Request{
		Model:    "mimo-v2.6-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}}},
	}, func(c Chunk) { text += c.Text + c.Delta })
	if uerr != nil {
		t.Fatalf("quota refusal must be answered by an in-place retry, got: %s (%s)", uerr.Code, uerr.Message)
	}
	if text != "ok" {
		t.Errorf("streamed text = %q, want the retried answer", text)
	}
	if out.Finish != "stop" {
		t.Errorf("finish = %q, want stop", out.Finish)
	}
	if rotates.Load() != 1 {
		t.Errorf("rotate hook fired %d times, want exactly one", rotates.Load())
	}
	if calls.Load() != 2 {
		t.Errorf("upstream saw %d requests, want 1 refused + 1 retry", calls.Load())
	}
}

// TestQuotaRotateFalseSurfacesError keeps the honest path: when the pool has
// nowhere to go (hook returns false) the refusal must still reach the caller.
func TestQuotaRotateFalseSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"FreeUsageLimitError: usage limit reached for this ip"}}`))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	l := NewLane()
	var rotates atomic.Int32
	l.OnQuotaRotate = func(string) bool {
		rotates.Add(1)
		return false // pool dry: nothing to rotate to
	}

	_, uerr := l.Complete(context.Background(), Request{
		Model:    "mimo-v2.6-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}}},
	}, func(Chunk) {})
	if uerr == nil || uerr.Code != CodeQuota {
		t.Fatalf("expected the quota error to surface, got %+v", uerr)
	}
	if rotates.Load() != 3 { // every refused attempt asks the pool once: 1 + failoverMax
		t.Errorf("rotate hook fired %d times, want 3 (1 + failoverMax)", rotates.Load())
	}
}

// TestClearThrottleResetsVerdicts covers the post-rotation reset: quota is per
// IP, so verdicts recorded on the old egress must not survive the switch.
func TestClearThrottleResetsVerdicts(t *testing.T) {
	l := NewLane()
	const model = "mimo-v2.6-flash-free"

	l.MarkThrottled(model, 120)
	if !l.throttle.Throttled(model) {
		t.Fatal("model should be throttled after MarkThrottled")
	}
	if _, av, _ := l.Snapshot(); av[model].State != StateThrottled {
		t.Fatalf("availability state = %q, want %q", av[model].State, StateThrottled)
	}
	if eta := l.RecoveryETA(model); eta == 0 {
		t.Error("expected a recovery estimate while throttled")
	}

	l.ClearThrottle()

	if l.throttle.Throttled(model) {
		t.Error("throttle state survived ClearThrottle")
	}
	if _, av, _ := l.Snapshot(); av[model].State == StateThrottled {
		t.Error("availability still throttled after ClearThrottle")
	}
	if eta := l.RecoveryETA(model); eta != 0 {
		t.Errorf("recovery estimate = %d, want 0 after reset", eta)
	}
}

// TestNormalizeProxy pins the listing-line shapes the pool must accept.
func TestNormalizeProxy(t *testing.T) {
	cases := []struct {
		in, def, want string
		ok            bool
	}{
		{"1.2.3.4:8080", "http", "http://1.2.3.4:8080", true},
		{"http://1.2.3.4:8080", "http", "http://1.2.3.4:8080", true},
		{"  1.2.3.4:8080\n", "http", "http://1.2.3.4:8080", true},
		{"1.2.3.4:1080", "socks5", "socks5://1.2.3.4:1080", true},
		{"socks5h://1.2.3.4:1080", "http", "socks5://1.2.3.4:1080", true},
		{"https://1.2.3.4:3128", "http", "https://1.2.3.4:3128", true},
		{"", "http", "", false},
		{"# comment line", "http", "", false},
		{"not-a-proxy", "http", "", false},
		{"ftp://1.2.3.4:21", "http", "", false},
	}
	for _, c := range cases {
		url, _, _, ok := normalizeProxy(c.in, c.def)
		if ok != c.ok {
			t.Errorf("normalizeProxy(%q, %q) ok = %v, want %v", c.in, c.def, ok, c.ok)
			continue
		}
		if ok && url != c.want {
			t.Errorf("normalizeProxy(%q, %q) = %q, want %q", c.in, c.def, url, c.want)
		}
	}
}

// TestProxyPoolRotationOrder checks that rotation always prefers the fastest
// reachable proxy that has not been burned yet.
func TestProxyPoolRotationOrder(t *testing.T) {
	p := NewProxyPool(nil)
	p.localIPLookup = func(context.Context) string { return "203.0.113.7" } // hermetic: no echo service
	p.mu.Lock()
	for _, e := range []*ProxyEntry{
		{URL: "http://slow:1", OK: true, LatencyMs: 900},
		{URL: "http://fast:1", OK: true, LatencyMs: 50},
		{URL: "http://mid:1", OK: true, LatencyMs: 300},
		{URL: "http://dead:1", OK: false, LatencyMs: 10},
		{URL: "http://burned:1", OK: true, LatencyMs: 20, Exhausted: true},
	} {
		p.entries = append(p.entries, e)
		p.byURL[e.URL] = e
	}
	p.mu.Unlock()

	if got := p.UsableCount(); got != 3 {
		t.Fatalf("UsableCount = %d, want 3 (dead + burned excluded)", got)
	}
	if list := p.Usable(); list[0].URL != "http://fast:1" {
		t.Fatalf("fastest usable = %s, want http://fast:1", list[0].URL)
	}

	ctx := context.Background()
	first, err := p.Rotate(ctx, true)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if first != "http://fast:1" {
		t.Errorf("first rotation = %s, want the fastest node", first)
	}
	second, err := p.Rotate(ctx, true)
	if err != nil {
		t.Fatalf("second rotate: %v", err)
	}
	if second != "http://mid:1" {
		t.Errorf("second rotation = %s, want the next fastest (previous burned)", second)
	}
	third, err := p.Rotate(ctx, true)
	if err != nil {
		t.Fatalf("third rotate: %v", err)
	}
	if third != "http://slow:1" {
		t.Errorf("third rotation = %s, want the last standing node", third)
	}
	// the pool is dry now and has no usable sources to refill from
	p.Configure([]string{"http://127.0.0.1:1/unused"}, 1, 1, 1, false)
	if _, err := p.Rotate(ctx, true); err == nil {
		t.Error("expected an error once every node is burned")
	}
}

// TestPoolModeRoutesThroughDynamicProxy is the mechanism behind egress
// rotation: switching the active proxy must change where requests actually go,
// without rebuilding the transport or dropping the client.
func TestPoolModeRoutesThroughDynamicProxy(t *testing.T) {
	var proxied atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"via":"proxy"}`))
	}))
	defer proxySrv.Close()

	var directHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHits.Add(1)
		_, _ = w.Write([]byte(`{"via":"direct"}`))
	}))
	defer target.Close()

	SetProxy("pool", "")
	t.Cleanup(func() {
		SetActiveProxy(nil)
		SetProxy("direct", "")
	})

	client := Client()

	// 1) pool mode with no selection == direct connection
	SetActiveProxy(nil)
	client.CloseIdleConnections()
	if _, err := client.Get(target.URL); err != nil {
		t.Fatalf("direct request failed: %v", err)
	}
	if proxied.Load() != 0 {
		t.Errorf("direct mode routed through the proxy (%d hits)", proxied.Load())
	}

	// 2) installing a proxy must divert the very same client
	pu, err := neturl.Parse(proxySrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	SetActiveProxy(pu)
	client.CloseIdleConnections()
	before := proxied.Load()
	if _, err := client.Get(target.URL); err != nil {
		t.Fatalf("proxied request failed: %v", err)
	}
	if proxied.Load() == before {
		t.Error("pool mode did not route through the active proxy")
	}

	// 3) clearing it returns to direct without touching the transport
	SetActiveProxy(nil)
	client.CloseIdleConnections()
	before = proxied.Load()
	if _, err := client.Get(target.URL); err != nil {
		t.Fatalf("request after reset failed: %v", err)
	}
	if proxied.Load() != before {
		t.Error("clearing the proxy did not restore the direct connection")
	}
}

// TestSplitAnnotation covers the ProxyClean-style trailing "#cc" annotation,
// which used to make every line of that source fail to parse.
func TestSplitAnnotation(t *testing.T) {
	cases := []struct{ in, addr, note string }{
		{"1.2.3.4:1080 #hk", "1.2.3.4:1080", "hk"},
		{"socks5://1.2.3.4:1080 #us", "socks5://1.2.3.4:1080", "us"},
		{"1.2.3.4:1080", "1.2.3.4:1080", ""},
		{"  1.2.3.4:1080   # nl  ", "1.2.3.4:1080", "nl"},
	}
	for _, c := range cases {
		addr, note := splitAnnotation(c.in)
		if addr != c.addr || note != c.note {
			t.Errorf("splitAnnotation(%q) = (%q, %q), want (%q, %q)", c.in, addr, note, c.addr, c.note)
		}
	}
}

// TestProxyCleanLineParses walks a real ProxyClean line end to end: annotate →
// normalise, the two steps the fetch path performs.
func TestProxyCleanLineParses(t *testing.T) {
	for _, in := range []string{
		"socks5://47.238.126.208:1080 #hk",
		"socks5://192.252.220.89:4145 #us",
		"socks5://45.74.31.25:6420 #nl",
	} {
		addr, note := splitAnnotation(in)
		if note == "" {
			t.Fatalf("%q: country annotation lost", in)
		}
		url, host, scheme, ok := normalizeProxy(addr, "socks5")
		if !ok {
			t.Fatalf("%q: address did not parse (addr=%q)", in, addr)
		}
		if scheme != "socks5" {
			t.Errorf("%q: scheme = %q, want socks5", in, scheme)
		}
		if host == "" || url == "" {
			t.Errorf("%q: empty host/url", in)
		}
	}
}

// TestSplitRepeat covers the "#repeat=N" trailer that API-style sources use to
// accumulate several randomised batches in one refresh.
func TestSplitRepeat(t *testing.T) {
	cases := []struct {
		in, url string
		n       int
	}{
		{"https://a/b", "https://a/b", 1},
		{"https://a/b#repeat=5", "https://a/b", 5},
		{"https://a/b?protocol=socks5&count=20#repeat=3", "https://a/b?protocol=socks5&count=20", 3},
		{"https://a/b#repeat=0", "https://a/b", 1},
		{"https://a/b#repeat=999", "https://a/b", 50},
		{"https://a/b#repeat=abc", "https://a/b#repeat=abc", 1},
	}
	for _, c := range cases {
		url, n := splitRepeat(c.in)
		if url != c.url || n != c.n {
			t.Errorf("splitRepeat(%q) = (%q, %d), want (%q, %d)", c.in, url, n, c.url, c.n)
		}
	}
}

// TestParseProxyBodyScdnJSON pins the exact response shape of the scdn.io API,
// which is a whole JSON document rather than one proxy per line.
func TestParseProxyBodyScdnJSON(t *testing.T) {
	body := []byte(`{"code":200,"message":"success","data":{"proxies":["115.78.135.4:3334","165.225.240.12:30001","18.142.95.213:27142"],"count":3}}`)
	got := parseProxyBody(body)
	if len(got) != 3 {
		t.Fatalf("got %d proxies, want 3: %v", len(got), got)
	}
	if got[0] != "115.78.135.4:3334" {
		t.Errorf("first = %q", got[0])
	}
}

func TestParseProxyBodyPlainText(t *testing.T) {
	body := []byte("1.2.3.4:8080\n5.6.7.8:3128\n\n# comment\n")
	got := parseProxyBody(body)
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3: %v", len(got), got)
	}
}

func TestParseProxyBodyIPPortObjects(t *testing.T) {
	body := []byte(`[{"ip":"1.2.3.4","port":8080},{"ip":"5.6.7.8","port":"3128"}]`)
	got := parseProxyBody(body)
	if len(got) != 2 || got[0] != "1.2.3.4:8080" || got[1] != "5.6.7.8:3128" {
		t.Fatalf("got %v", got)
	}
}

// TestParseProxyBodyGeonodeProtocols pins the geonode shape: one object per
// proxy with a protocols array. Each protocol must become its own
// scheme-prefixed candidate — a bare "ip:port" would inherit the source's
// default scheme and mislabel every socks entry in the mixed list.
func TestParseProxyBodyGeonodeProtocols(t *testing.T) {
	body := []byte(`{"data":[
		{"ip":"1.2.3.4","port":"7891","protocols":["socks5"],"country":"CN"},
		{"ip":"5.6.7.8","port":3128,"protocols":["http","https"],"country":"US"},
		{"ip":"9.9.9.9","port":1080,"protocols":[]}
	],"total":3}`)
	got := parseProxyBody(body)
	// an entry with no protocols falls back to the bare "ip:port" shape and
	// inherits the source's default scheme at normalizeProxy time
	want := []string{"socks5://1.2.3.4:7891", "http://5.6.7.8:3128", "https://5.6.7.8:3128", "9.9.9.9:1080"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// every emitted candidate must survive normalizeProxy with the right scheme
	for _, raw := range got {
		url, _, scheme, ok := normalizeProxy(raw, "http")
		if !ok {
			t.Errorf("normalizeProxy(%q) rejected", raw)
			continue
		}
		if !strings.HasPrefix(url, scheme+"://") {
			t.Errorf("normalizeProxy(%q) = %q scheme %q", raw, url, scheme)
		}
	}
}

// TestSchemeForSource guards against reading the listing's own https:// scheme
// as the proxy protocol — that would mislabel every HTTPS-hosted list.
func TestSchemeForSource(t *testing.T) {
	cases := map[string]string{
		"https://proxy.scdn.io/api/get_proxy.php?protocol=socks5&count=20":              "socks5",
		"https://proxy.scdn.io/api/get_proxy.php?protocol=http&count=20":                "http",
		"https://raw.githubusercontent.com/hookzof/socks5_list/master/proxy.txt":        "socks5",
		"https://raw.githubusercontent.com/roosterkid/openproxylist/main/HTTPS_RAW.txt": "http",
		"https://raw.githubusercontent.com/HankNovic/ProxyClean/main/SOCKS5.txt":        "socks5",
		"https://example.com/http.txt":                                                  "http",
	}
	for in, want := range cases {
		if got := schemeForSource(in); got != want {
			t.Errorf("schemeForSource(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReadLocalListing covers file:// sources — the escape hatch for listings
// that cannot be fetched programmatically (a site behind a Cloudflare
// challenge, an authenticated panel, a hand-picked list).
func TestReadLocalListing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list.txt")
	content := "1.2.3.4:8080\n5.6.7.8:3128 #us\n\n# comment line\n9.9.9.9:1080\n1.2.3.4:8080\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readLocalListing("file://"+path, "http")
	if err != nil {
		t.Fatalf("readLocalListing: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3 (dup dropped, comment skipped): %+v", len(got), got)
	}
	if got[0].Raw != "1.2.3.4:8080" || got[0].Scheme != "http" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Country != "us" {
		t.Errorf("country annotation lost: %+v", got[1])
	}
}

func TestReadLocalListingMissingFile(t *testing.T) {
	if _, err := readLocalListing("file:///nonexistent/xyz.txt", "http"); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestReadLocalListingJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list.json")
	body := `{"code":200,"data":{"proxies":["10.0.0.1:3128","10.0.0.2:8080"]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readLocalListing("file://"+path, "socks5")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d, err %v", len(got), err)
	}
	if got[0].Scheme != "socks5" {
		t.Errorf("scheme = %q, want socks5", got[0].Scheme)
	}
}

// TestEgressFailureHookFiresOnConnectError covers the trigger behind "dead
// proxy → switch away": a request that never reached upstream must be flagged
// as an egress failure (distinct from an upstream-side stream cut).
func TestEgressFailureHookFiresOnConnectError(t *testing.T) {
	old := UpstreamBase
	UpstreamBase = "http://127.0.0.1:1" // nothing listens here
	defer func() { UpstreamBase = old }()

	l := NewLane()
	l.SetFailover(false, 1)
	var fired atomic.Int32
	var detail atomic.Value
	l.OnEgressFailure = func(model, d string) {
		fired.Add(1)
		detail.Store(d)
	}

	_, uerr := l.Complete(context.Background(), Request{
		Model:    "mimo-v2.6-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}}},
	}, func(Chunk) {})

	if uerr == nil {
		t.Fatal("expected an error")
	}
	if !uerr.Egress {
		t.Errorf("connection failure not flagged as egress failure: %+v", uerr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("OnEgressFailure did not fire on a connect error")
	}
	if d, _ := detail.Load().(string); d == "" {
		t.Error("hook detail is empty")
	}
}

// TestUpstreamStreamCutIsNotEgressFailure keeps an upstream-side cut from being
// mistaken for a broken proxy — that would rotate for no reason.
func TestUpstreamStreamCutIsNotEgressFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		// a clean text answer then a cut stream (no finish token)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	l := NewLane()
	l.SetFailover(false, 1)
	var fired atomic.Int32
	l.OnEgressFailure = func(string, string) { fired.Add(1) }

	_, uerr := l.Complete(context.Background(), Request{
		Model:    "mimo-v2.6-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}}},
	}, func(Chunk) {})
	if uerr != nil && uerr.Egress {
		t.Errorf("a stream cut was misread as an egress failure: %+v", uerr)
	}
	time.Sleep(150 * time.Millisecond)
	if fired.Load() != 0 {
		t.Error("OnEgressFailure fired for an upstream-side cut")
	}
}

func TestVerifyCurrent(t *testing.T) {
	var hits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer proxySrv.Close()

	old := UpstreamBase
	UpstreamBase = "http://example.invalid" // reachable only via the proxy stub
	defer func() { UpstreamBase = old }()

	pool := NewProxyPool(nil)
	ctx := context.Background()

	// direct connection: nothing to verify, treat as healthy
	if !pool.VerifyCurrent(ctx) {
		t.Error("direct connection should verify true")
	}

	// reachable proxy: the stub answers for any target
	pool.mu.Lock()
	pool.current = proxySrv.URL
	pool.byURL[proxySrv.URL] = &ProxyEntry{URL: proxySrv.URL, OK: true}
	pool.mu.Unlock()
	if !pool.VerifyCurrent(ctx) {
		t.Error("reachable proxy should verify true")
	}
	if hits.Load() == 0 {
		t.Error("verification did not go through the proxy")
	}

	// dead proxy: nothing listens
	pool.mu.Lock()
	pool.current = "http://127.0.0.1:1"
	pool.mu.Unlock()
	if pool.VerifyCurrent(ctx) {
		t.Error("dead proxy must verify false")
	}
}

func TestMarkCurrentDead(t *testing.T) {
	pool := NewProxyPool(nil)
	const u = "http://10.0.0.9:8080"
	pool.mu.Lock()
	pool.current = u
	pool.byURL[u] = &ProxyEntry{URL: u, OK: true, LatencyMs: 120}
	pool.entries = append(pool.entries, pool.byURL[u])
	pool.mu.Unlock()

	if pool.UsableCount() != 1 {
		t.Fatalf("setup: UsableCount = %d", pool.UsableCount())
	}
	pool.MarkCurrentDead("运行中失效（无法转发）")

	if pool.UsableCount() != 0 {
		t.Error("dead proxy still counted as usable")
	}
	_, entries, _, _, _, _ := pool.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].OK {
		t.Error("entry still marked OK")
	}
	if entries[0].Detail != "运行中失效（无法转发）" {
		t.Errorf("detail = %q", entries[0].Detail)
	}
}

// deadSource configures the pool with one unreachable listing so a dry-pool
// rotation fails fast instead of hitting the public internet.
func deadSource(p *ProxyPool) {
	p.Configure([]string{"http://127.0.0.1:1/unused"}, 1, 1, 1, false)
}

// stubLocalIP makes the direct-line egress lookup hermetic.
func stubLocalIP(p *ProxyPool, ip string) {
	p.localIPLookup = func(context.Context) string { return ip }
}

// TestRotateBlacklistsEgressIP covers the day-scoped rotation blacklist: every
// IP the lane rotates away from is retired, and no later rotation today can
// land on it again — not even through a second port behind the same address,
// because the free lane's quota is per IP.
func TestRotateBlacklistsEgressIP(t *testing.T) {
	p := NewProxyPool(nil)
	stubLocalIP(p, "203.0.113.7")
	p.mu.Lock()
	for _, e := range []*ProxyEntry{
		{URL: "http://a:1", OK: true, LatencyMs: 10, EgressIP: "10.0.0.1"},
		{URL: "http://a:2", OK: true, LatencyMs: 5, EgressIP: "10.0.0.1"}, // same exit, fastest
		{URL: "http://b:1", OK: true, LatencyMs: 20, EgressIP: "10.0.0.2"},
		{URL: "http://c:1", OK: true, LatencyMs: 30, EgressIP: "10.0.0.3"},
	} {
		p.entries = append(p.entries, e)
		p.byURL[e.URL] = e
	}
	p.mu.Unlock()

	ctx := context.Background()

	// rotation 1: direct → fastest node; the local line is retired too
	first, err := p.Rotate(ctx, true)
	if err != nil {
		t.Fatalf("rotate 1: %v", err)
	}
	if first != "http://a:2" {
		t.Errorf("rotate 1 = %s, want the fastest node", first)
	}
	if bl := p.Blacklist(); len(bl) != 1 || bl[0].IP != "203.0.113.7" {
		t.Errorf("blacklist after rotate 1 = %+v, want the local egress IP", bl)
	}

	// rotation 2: a:1 is on the same exit and still faster than b:1, but the
	// exit was retired by rotation 1 — it must be skipped, not used as a
	// last-resort hop
	second, err := p.Rotate(ctx, true)
	if err != nil {
		t.Fatalf("rotate 2: %v", err)
	}
	if second != "http://b:1" {
		t.Errorf("rotate 2 = %s, want the next node on a different exit", second)
	}

	// rotation 3: last standing node
	third, err := p.Rotate(ctx, true)
	if err != nil {
		t.Fatalf("rotate 3: %v", err)
	}
	if third != "http://c:1" {
		t.Errorf("rotate 3 = %s, want the last node", third)
	}

	// every rotated-away exit is on the blacklist now (the current one joins
	// on the next rotation, not before)
	want := map[string]bool{"203.0.113.7": true, "10.0.0.1": true, "10.0.0.2": true}
	for _, b := range p.Blacklist() {
		delete(want, b.IP)
	}
	if len(want) != 0 {
		t.Errorf("blacklist missing %v", want)
	}
	if n := p.UsableCount(); n != 1 {
		t.Errorf("UsableCount = %d, want 1 (only the current node left)", n)
	}

	// rotation 4: nothing left — the error must say why, not just "empty"
	deadSource(p)
	if _, err := p.Rotate(ctx, true); err == nil {
		t.Fatal("expected an error once every node is retired")
	} else if !strings.Contains(err.Error(), "黑名单") {
		t.Errorf("dry error = %q, want it to mention the blacklist", err)
	}
}

// TestRotateSameExitFallback: a manual switch may still hop to a second port
// on the current exit (the user asked for a different node, not a quota
// reset), but a quota rotation must never fall back to the same exit.
func TestRotateSameExitFallbackOnlyForManualSwitch(t *testing.T) {
	p := NewProxyPool(nil)
	stubLocalIP(p, "203.0.113.7")
	p.mu.Lock()
	for _, e := range []*ProxyEntry{
		{URL: "http://a:1", OK: true, LatencyMs: 10, EgressIP: "10.0.0.1"},
		{URL: "http://a:2", OK: true, LatencyMs: 5, EgressIP: "10.0.0.1"},
	} {
		p.entries = append(p.entries, e)
		p.byURL[e.URL] = e
	}
	p.current = "http://a:1"
	p.mu.Unlock()

	ctx := context.Background()

	// manual switch: same-exit node is the last resort, and allowed
	got, err := p.Rotate(ctx, false)
	if err != nil {
		t.Fatalf("manual rotate: %v", err)
	}
	if got != "http://a:2" {
		t.Errorf("manual rotate = %s, want the same-exit fallback", got)
	}
	if n := len(p.Blacklist()); n != 0 {
		t.Errorf("manual rotate blacklisted %d IPs, want 0", n)
	}

	// quota rotation from the same spot: the exit is retired, so there is
	// nowhere to go — no same-exit hop, no matter how fast the node is
	p.mu.Lock()
	p.current = "http://a:1"
	p.mu.Unlock()
	deadSource(p)
	if _, err := p.Rotate(ctx, true); err == nil {
		t.Error("quota rotation fell back to the same exit")
	}
	if bl := p.Blacklist(); len(bl) != 1 || bl[0].IP != "10.0.0.1" {
		t.Errorf("blacklist = %+v, want the retired exit IP", bl)
	}
}

// TestBlacklistDayRollover: the blacklist is day-scoped. A map left over from
// yesterday expires the moment it is read, and ResetDaily drops it explicitly.
func TestBlacklistDayRollover(t *testing.T) {
	p := NewProxyPool(nil)
	p.mu.Lock()
	p.blacklistDay = "1999-12-31" // yesterday
	p.blacklist["10.0.0.1"] = BlacklistEntry{IP: "10.0.0.1", Reason: "额度耗尽", At: 1}
	p.blacklist["10.0.0.2"] = BlacklistEntry{IP: "10.0.0.2", Reason: "额度耗尽", At: 2}
	e := &ProxyEntry{URL: "http://a:1", OK: true, EgressIP: "10.0.0.1"}
	p.entries = append(p.entries, e)
	p.byURL[e.URL] = e
	p.mu.Unlock()

	// a stale blacklist must not hide today's candidates
	if n := p.UsableCount(); n != 1 {
		t.Errorf("UsableCount = %d, want 1 (yesterday's blacklist is stale)", n)
	}
	if n := len(p.Blacklist()); n != 0 {
		t.Errorf("Blacklist = %d entries after rollover, want 0", n)
	}

	// the explicit midnight reset path reports what it dropped
	p.mu.Lock()
	p.blacklist["10.0.0.9"] = BlacklistEntry{IP: "10.0.0.9", At: 3}
	p.mu.Unlock()
	if n := p.ResetDaily(); n != 1 {
		t.Errorf("ResetDaily dropped %d, want 1", n)
	}
	if n := len(p.Blacklist()); n != 0 {
		t.Errorf("Blacklist = %d after ResetDaily, want 0", n)
	}
}

// TestBlacklistPersistsAcrossRestart: the blacklist rides along in
// proxypool.json so a restart does not forget which exits are retired — but a
// snapshot from an earlier day is stale and dropped on load.
func TestBlacklistPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxypool.json")

	p1 := NewProxyPool(nil)
	p1.SetStorage(path)
	p1.mu.Lock()
	for _, e := range []*ProxyEntry{
		{URL: "http://a:1", OK: true, LatencyMs: 10, EgressIP: "10.0.0.1"},
		{URL: "http://b:1", OK: true, LatencyMs: 20, EgressIP: "10.0.0.2"},
	} {
		p1.entries = append(p1.entries, e)
		p1.byURL[e.URL] = e
	}
	p1.current = "http://a:1"
	p1.mu.Unlock()
	deadSource(p1) // a dry pool must refill from nowhere, not the internet
	got, err := p1.Rotate(context.Background(), true)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got != "http://b:1" {
		t.Fatalf("rotate = %s, want http://b:1", got)
	}
	p1.save()

	p2 := NewProxyPool(nil)
	p2.SetStorage(path)
	if err := p2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	bl := p2.Blacklist()
	if len(bl) != 1 || bl[0].IP != "10.0.0.1" {
		t.Fatalf("restored blacklist = %+v, want the retired exit IP", bl)
	}
	if n := p2.UsableCount(); n != 1 {
		t.Errorf("UsableCount = %d after restore, want 1 (b:1 still live, a:1 retired)", n)
	}

	// rewrite the snapshot as if it were written yesterday
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pf poolFile
	if err := json.Unmarshal(data, &pf); err != nil {
		t.Fatal(err)
	}
	pf.BlacklistDay = "1999-12-31"
	out, _ := json.Marshal(pf)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	p3 := NewProxyPool(nil)
	p3.SetStorage(path)
	if err := p3.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := len(p3.Blacklist()); n != 0 {
		t.Errorf("stale blacklist survived load (%d entries)", n)
	}
	// b:1 is still live; a:1 stays exhausted (that flag is per-URL and only a
	// refresh re-tests it) but is no longer blacklist-hidden
	if n := p3.UsableCount(); n != 1 {
		t.Errorf("UsableCount = %d after stale load, want 1 (b:1 live, a:1 exhausted)", n)
	}
}

// TestSnapshotFlagsBlacklistedEntries: the dashboard needs to tell "retired
// for today" apart from "quota burned", so the overlay must be per-entry.
func TestSnapshotFlagsBlacklistedEntries(t *testing.T) {
	p := NewProxyPool(nil)
	p.mu.Lock()
	for _, e := range []*ProxyEntry{
		{URL: "http://a:1", OK: true, LatencyMs: 10, EgressIP: "10.0.0.1"},
		{URL: "http://a:2", OK: true, LatencyMs: 20, EgressIP: "10.0.0.1"},
		{URL: "http://b:1", OK: true, LatencyMs: 30, EgressIP: "10.0.0.2"},
	} {
		p.entries = append(p.entries, e)
		p.byURL[e.URL] = e
	}
	p.blacklist["10.0.0.1"] = BlacklistEntry{IP: "10.0.0.1", Reason: "额度耗尽", At: 1}
	p.mu.Unlock()

	_, entries, _, _, _, _ := p.Snapshot()
	byURL := map[string]ProxyEntry{}
	for _, e := range entries {
		byURL[e.URL] = e
	}
	if !byURL["http://a:1"].Blacklisted || !byURL["http://a:2"].Blacklisted {
		t.Error("entries on a blacklisted exit are not flagged")
	}
	if byURL["http://b:1"].Blacklisted {
		t.Error("entry on a live exit flagged as blacklisted")
	}
	// usable first: the flagged nodes sort behind the live one
	if entries[0].URL != "http://b:1" {
		t.Errorf("first entry = %s, want the non-blacklisted one", entries[0].URL)
	}
}
