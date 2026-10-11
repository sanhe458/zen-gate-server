package lane

import (
	"context"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
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
