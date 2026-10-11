package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate-server/internal/lane"
)

// withFakeUpstream points the lane at a local stub so the HTTP handlers can be
// exercised without touching the real gateway.
func withFakeUpstream(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	old := lane.UpstreamBase
	lane.UpstreamBase = up.URL
	t.Cleanup(func() { lane.UpstreamBase = old })
}

// TestPublicBaseURLIgnoresForwardedFromUntrustedPeer pins the trust fix: RFC
// 7239's Forwarded header was parsed before the trusted-peer check, so any
// public caller could rewrite the address the landing page advertises.
func TestPublicBaseURLIgnoresForwardedFromUntrustedPeer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:5555" // public peer
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Forwarded", `proto=https;host=evil.example.com`)
	if got := publicBaseURL(r); got != "http://127.0.0.1:8787/v1" {
		t.Errorf("untrusted Forwarded header honoured: got %q", got)
	}

	// a trusted reverse proxy may still publish the original address
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = "127.0.0.1:5555"
	r2.Host = "127.0.0.1:8787"
	r2.Header.Set("Forwarded", `proto=https;host=gate.example.com`)
	if got := publicBaseURL(r2); got != "https://gate.example.com/v1" {
		t.Errorf("trusted Forwarded header ignored: got %q", got)
	}
}

// TestBaseURLNeverAdvertisesUnspecifiedBind pins the address fix: a bind on
// 0.0.0.0/:: used to be advertised verbatim as "http://[::]:8787/v1".
func TestBaseURLNeverAdvertisesUnspecifiedBind(t *testing.T) {
	s := newTestServer(t)

	for _, bound := range []string{"[::]:8787", "0.0.0.0:8787"} {
		s.addr.Store(bound)
		if got := s.BaseURL(); got != "http://127.0.0.1:8787/v1" {
			t.Errorf("bound %s: BaseURL = %q, want a dialable loopback address", bound, got)
		}
	}

	// a concrete bind is advertised as-is
	s.addr.Store("192.0.2.5:8787")
	if got := s.BaseURL(); got != "http://192.0.2.5:8787/v1" {
		t.Errorf("concrete bind: BaseURL = %q", got)
	}

	// ListenAddr keeps describing where it actually listens
	s.addr.Store("[::]:8787")
	s.Store.Config().ListenHost = "0.0.0.0"
	if got := s.ListenAddr(); got != "0.0.0.0:8787" {
		t.Errorf("ListenAddr = %q, want 0.0.0.0:8787", got)
	}
	s.addr.Store("127.0.0.1:8787")
	if got := s.ListenAddr(); got != "127.0.0.1:8787" {
		t.Errorf("ListenAddr = %q", got)
	}
}

// TestFrontendStateKeysMatchServerConstants pins the badge fix: the pages keyed
// their badge tables on "region_block" while the API sends "region-blocked", so
// region-gated models rendered as "unknown" with the raw English state string.
func TestFrontendStateKeysMatchServerConstants(t *testing.T) {
	for name, page := range map[string][]byte{
		"index.html":  dashboardHTML,
		"public.html": publicHTML,
	} {
		body := string(page)
		if !strings.Contains(body, `"`+lane.StateRegionBlock+`"`) {
			t.Errorf("%s does not map the %q state the API actually sends", name, lane.StateRegionBlock)
		}
		if strings.Contains(body, "region_block") {
			t.Errorf("%s still uses the stale region_block key", name)
		}
	}
}

// TestStreamingChatSetsServedByHeader pins the served-by fix for streams: the
// opening frame used to be flushed before the model was known, so the header
// could never be set on the path clients actually use most.
func TestStreamingChatSetsServedByHeader(t *testing.T) {
	withFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	})

	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	r.RemoteAddr = "127.0.0.1:5555"
	w := do(t, s, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("x-zen-gate-served-by"); got != "mimo-v2.6-flash-free" {
		t.Errorf("streaming response is missing x-zen-gate-served-by: got %q", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") || !strings.Contains(body, "[DONE]") {
		t.Errorf("unexpected stream body: %s", body)
	}
	// the role frame must still lead the content
	if i, j := strings.Index(body, `"role":"assistant"`), strings.Index(body, `"content":"hi"`); i < 0 || j < 0 || i > j {
		t.Errorf("role frame is not the first chunk: %s", body)
	}
}

// TestResponsesSetsServedByHeader pins the same fix on the Responses wire, which
// never set the header at all.
func TestResponsesSetsServedByHeader(t *testing.T) {
	withFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w,
			`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	})

	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"mimo-v2.6-flash-free","input":"hi"}`))
	r.RemoteAddr = "127.0.0.1:5555"
	w := do(t, s, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("x-zen-gate-served-by"); got != "mimo-v2.6-flash-free" {
		t.Errorf("responses response is missing x-zen-gate-served-by: got %q", got)
	}
}
