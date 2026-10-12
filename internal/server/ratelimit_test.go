package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zen-gate-server/internal/lane"
	"zen-gate-server/internal/store"
)

func TestIPRateLimiterBudgetAndRefusal(t *testing.T) {
	l := newIPRateLimiter(time.Minute)

	for i := 0; i < 3; i++ {
		ok, remaining, _ := l.allow("1.2.3.4", 3)
		if !ok {
			t.Fatalf("request %d should have passed", i+1)
		}
		if remaining != 2-i {
			t.Errorf("request %d: remaining = %d, want %d", i+1, remaining, 2-i)
		}
	}

	ok, remaining, retryAfter := l.allow("1.2.3.4", 3)
	if ok {
		t.Fatal("the 4th request should be refused")
	}
	if remaining != 0 {
		t.Errorf("remaining on refusal = %d, want 0", remaining)
	}
	if retryAfter < 1 || retryAfter > 61 {
		t.Errorf("retryAfter = %d, want 1..61", retryAfter)
	}

	// a different address has its own budget
	if ok, _, _ := l.allow("5.6.7.8", 3); !ok {
		t.Error("a second address should not inherit the first one's budget")
	}
}

func TestIPRateLimiterWindowSlides(t *testing.T) {
	l := newIPRateLimiter(80 * time.Millisecond)
	l.allow("x", 2)
	l.allow("x", 2)
	if ok, _, _ := l.allow("x", 2); ok {
		t.Fatal("third request should be refused inside the window")
	}
	time.Sleep(140 * time.Millisecond)
	if ok, _, _ := l.allow("x", 2); !ok {
		t.Error("the window should have slid past the earlier hits")
	}
}

func TestIPRateLimiterZeroLimitMeansUnlimited(t *testing.T) {
	l := newIPRateLimiter(time.Minute)
	for i := 0; i < 50; i++ {
		if ok, _, _ := l.allow("x", 0); !ok {
			t.Fatalf("limit 0 must not refuse anything (failed at %d)", i)
		}
	}
}

// TestClientIPRefusesSpoofedForwarding is the security-relevant case: a public
// caller must not be able to mint a fresh anonymous budget by inventing an
// X-Forwarded-For header.
func TestClientIPRefusesSpoofedForwarding(t *testing.T) {
	pub := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	pub.RemoteAddr = "203.0.113.9:1234"
	pub.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := clientIP(pub); got != "203.0.113.9" {
		t.Errorf("public peer: got %q, want the socket address", got)
	}

	proxy := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.RemoteAddr = "127.0.0.1:1234"
	proxy.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2")
	if got := clientIP(proxy); got != "1.1.1.1" {
		t.Errorf("trusted peer: got %q, want the first XFF entry", got)
	}

	lan := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	lan.RemoteAddr = "192.168.1.5:1234"
	lan.Header.Set("X-Real-IP", "9.9.9.9")
	if got := clientIP(lan); got != "9.9.9.9" {
		t.Errorf("private peer: got %q, want X-Real-IP", got)
	}
}

func TestPublicBaseURLReflectsRequest(t *testing.T) {
	// plain loopback http
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "127.0.0.1:8787"
	if got := publicBaseURL(r); got != "http://127.0.0.1:8787/v1" {
		t.Errorf("got %q", got)
	}

	// TLS reached directly
	r2 := httptest.NewRequest(http.MethodGet, "https://api.example.com/", nil)
	r2.Host = "api.example.com"
	r2.TLS = &tls.ConnectionState{}
	if got := publicBaseURL(r2); got != "https://api.example.com/v1" {
		t.Errorf("tls: got %q", got)
	}

	// behind a trusted reverse proxy that terminates TLS
	r3 := httptest.NewRequest(http.MethodGet, "/", nil)
	r3.RemoteAddr = "127.0.0.1:5555"
	r3.Host = "127.0.0.1:8787"
	r3.Header.Set("X-Forwarded-Proto", "https")
	r3.Header.Set("X-Forwarded-Host", "gate.example.com")
	if got := publicBaseURL(r3); got != "https://gate.example.com/v1" {
		t.Errorf("proxied: got %q", got)
	}

	// an untrusted peer's forwarding headers are ignored
	r4 := httptest.NewRequest(http.MethodGet, "/", nil)
	r4.RemoteAddr = "203.0.113.9:5555"
	r4.Host = "127.0.0.1:8787"
	r4.Header.Set("X-Forwarded-Proto", "https")
	r4.Header.Set("X-Forwarded-Host", "evil.example.com")
	if got := publicBaseURL(r4); got != "http://127.0.0.1:8787/v1" {
		t.Errorf("spoofed headers honoured: got %q", got)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return New(lane.NewLane(), st)
}

func do(t *testing.T, s *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	return w
}

// TestGateAnonymousThenKeyed walks the whole access model: metered without a
// key, unmetered with one, and a plain 401 for a wrong key.
func TestGateAnonymousThenKeyed(t *testing.T) {
	s := newTestServer(t)
	s.Store.Update(func(c *store.Config) { c.AnonymousRPM = 3 })

	anon := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.RemoteAddr = "203.0.113.7:5555"
		return do(t, s, r)
	}
	for i := 0; i < 3; i++ {
		if w := anon(); w.Code != 200 {
			t.Fatalf("anonymous request %d: status %d, want 200 (body %s)", i+1, w.Code, w.Body.String())
		}
	}
	w := anon()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("4th anonymous request: status %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 is missing Retry-After")
	}
	if !strings.Contains(w.Body.String(), "anonymous_rate_limited") {
		t.Errorf("429 body should name the reason: %s", w.Body.String())
	}

	key := s.Store.Config().MainKey
	for i := 0; i < 12; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.RemoteAddr = "203.0.113.7:5555" // same address, already exhausted
		r.Header.Set("Authorization", "Bearer "+key)
		if w := do(t, s, r); w.Code != 200 {
			t.Fatalf("keyed request %d: status %d — keys must bypass the limit", i+1, w.Code)
		}
	}

	bad := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	bad.RemoteAddr = "203.0.113.8:5555"
	bad.Header.Set("Authorization", "Bearer ofm-not-a-real-key")
	if w := do(t, s, bad); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: status %d, want 401", w.Code)
	}
}

// TestKeylessAccessCanBeDisabled covers AnonymousRPM = 0.
func TestKeylessAccessCanBeDisabled(t *testing.T) {
	s := newTestServer(t)
	s.Store.Update(func(c *store.Config) { c.AnonymousRPM = 0 })
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "203.0.113.7:5555"
	if w := do(t, s, r); w.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401 when keyless access is off", w.Code)
	}
}

// TestRoutingSplit pins the layout the request asked for: the landing page at
// the root, the dashboard under /admin.
func TestRoutingSplit(t *testing.T) {
	s := newTestServer(t)

	root := httptest.NewRequest(http.MethodGet, "/", nil)
	root.RemoteAddr = "127.0.0.1:5555"
	w := do(t, s, root)
	if w.Code != 200 {
		t.Fatalf("/ → %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "接入地址") {
		t.Error("/ should serve the outward-facing page (endpoint block missing)")
	}
	if strings.Contains(body, `id="tabs"`) {
		t.Error("/ leaked the dashboard shell (tab bar present)")
	}

	admin := httptest.NewRequest(http.MethodGet, "/admin", nil)
	admin.RemoteAddr = "127.0.0.1:5555"
	aw := do(t, s, admin)
	if aw.Code != 200 || !strings.Contains(aw.Body.String(), `id="tabs"`) {
		t.Errorf("/admin should serve the dashboard shell (status %d)", aw.Code)
	}

	info := httptest.NewRequest(http.MethodGet, "/public-info.json", nil)
	info.RemoteAddr = "127.0.0.1:5555"
	iw := do(t, s, info)
	if iw.Code != 200 {
		t.Fatalf("/public-info.json → %d", iw.Code)
	}
	if strings.Contains(iw.Body.String(), s.Store.Config().MainKey) {
		t.Error("public-info leaked the main API key")
	}
	if !strings.Contains(iw.Body.String(), "anonymousRPM") {
		t.Error("public-info is missing the anonymous budget")
	}
}
