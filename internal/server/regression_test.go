package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate-server/internal/lane"
	"zen-gate-server/internal/store"
)

// TestConvertResponsesInputDeveloperRole pins the role mapping fix: the
// Responses API speaks "developer" where the chat wire speaks "system". The
// converter passed the role through verbatim, and ToChatMessages switches on
// the four canonical roles — so every developer message was silently dropped
// from the upstream request (system prompts vanished without an error).
func TestConvertResponsesInputDeveloperRole(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"developer","content":"be terse"},
		{"type":"message","role":"user","content":"hi"},
		{"type":"message","role":"system","content":"policy"},
		{"type":"message","role":"assistant","content":"hello"}
	]`)
	got := convertResponsesInput(raw, "")
	if len(got) != 4 {
		t.Fatalf("converted %d messages, want 4 (developer must not be dropped)", len(got))
	}
	if got[0].Role != lane.RoleSystem {
		t.Fatalf("developer role mapped to %q, want %q", got[0].Role, lane.RoleSystem)
	}
	if got[0].TextOf() != "be terse" {
		t.Fatalf("developer content lost: %q", got[0].TextOf())
	}
	if got[1].Role != lane.RoleUser || got[2].Role != lane.RoleSystem || got[3].Role != lane.RoleAssistant {
		t.Fatalf("other roles mangled: %q %q %q", got[1].Role, got[2].Role, got[3].Role)
	}

	// instructions still seed the system turn first
	withInstr := convertResponsesInput(raw, "obey")
	if len(withInstr) != 5 || withInstr[0].Role != lane.RoleSystem || withInstr[0].TextOf() != "obey" {
		t.Fatalf("instructions handling changed: %d messages, first %+v", len(withInstr), withInstr[0])
	}

	// a bare string input still works, and an unknown role is not invented
	if got := convertResponsesInput(json.RawMessage(`"just text"`), ""); len(got) != 1 || got[0].Role != lane.RoleUser {
		t.Fatalf("string input broken: %+v", got)
	}
}

// TestConvertResponsesInputToolItems pins the function-call shapes: the
// developer fix touched the shared item loop, so tool items must keep their
// assistant/tool roles and ids.
func TestConvertResponsesInputToolItems(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"done"}
	]`)
	got := convertResponsesInput(raw, "")
	if len(got) != 2 {
		t.Fatalf("converted %d messages, want 2", len(got))
	}
	if got[0].Role != lane.RoleAssistant {
		t.Fatalf("function_call mapped to %q, want assistant", got[0].Role)
	}
	tc, ok := got[0].Parts[0].(lane.ToolCallPart)
	if !ok || tc.ID != "call_1" || tc.Name != "bash" {
		t.Fatalf("function_call part mangled: %+v", got[0].Parts)
	}
	if got[1].Role != lane.RoleTool {
		t.Fatalf("function_call_output mapped to %q, want tool", got[1].Role)
	}
	tr, ok := got[1].Parts[0].(lane.ToolResultPart)
	if !ok || tr.ToolCallID != "call_1" || tr.Text != "done" {
		t.Fatalf("function_call_output part mangled: %+v", got[1].Parts)
	}
}

// TestAdminSettingsRejectsCustomProxyWithoutURL pins the validation-order
// fix: the custom-proxy check now runs before Store.Update, so a rejected
// save (a) answers exactly one 400 — the old closure-return wrote the 400
// and then the handler's 200 on top of it — and (b) persists nothing.
func TestAdminSettingsRejectsCustomProxyWithoutURL(t *testing.T) {
	s := newTestServer(t)
	// The valid-save half re-points the shared lane transport at a proxy
	// URL; restore the default so later tests keep their direct egress.
	defer lane.SetProxy("env", "")
	before := s.Store.Config()

	body := `{"proxyMode":"custom","proxyUrl":"","defaultEffort":"light"}`
	r := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555" // loopback callers are trusted
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://example.com")
	w := do(t, s, r)
	if w.Code != 400 {
		t.Fatalf("status %d, want 400 for custom proxy without URL", w.Code)
	}
	if !strings.Contains(w.Body.String(), "自定义代理需要填写代理地址") {
		t.Fatalf("body missing the refusal reason: %s", w.Body.String())
	}
	// Exactly one response: no trailing 200 JSON appended after the 400.
	// ("changed" only ever appears in the success body.)
	if strings.Contains(w.Body.String(), `"changed"`) {
		t.Fatalf("a second (success) response was written after the 400: %s", w.Body.String())
	}
	// Nothing was applied or persisted.
	after := s.Store.Config()
	if after.ProxyMode != before.ProxyMode || after.DefaultEffort != before.DefaultEffort {
		t.Fatalf("rejected save mutated the config: %+v -> %+v", before, after)
	}

	// The same save with a URL goes through.
	body2 := `{"proxyMode":"custom","proxyUrl":"http://127.0.0.1:9999","defaultEffort":"light"}`
	r2 := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body2))
	r2.RemoteAddr = "127.0.0.1:5555"
	r2.Header.Set("Content-Type", "application/json")
	r2.Header.Set("Origin", "http://example.com")
	w2 := do(t, s, r2)
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), `"changed":true`) {
		t.Fatalf("valid save rejected: %d %s", w2.Code, w2.Body.String())
	}
	if got := s.Store.Config(); got.ProxyMode != "custom" || got.ProxyURL != "http://127.0.0.1:9999" ||
		got.DefaultEffort != "light" {
		t.Fatalf("valid save not applied: %+v", got)
	}
}

// TestAdminImportAppliesAndPersists pins the import fix: the handler wrote the
// uploaded config onto Store.Config()'s result — a private snapshot, since the
// config became copy-on-write — and then persisted the untouched live config.
// The endpoint answered {"ok":true} while changing nothing, in memory or on
// disk, so a restored backup was silently ignored.
func TestAdminImportAppliesAndPersists(t *testing.T) {
	s := newTestServer(t)
	incoming := *s.Store.Config()
	incoming.DefaultEffort = "deep"
	incoming.DefaultMaxTokens = 12345
	incoming.ProbeIntervalMinutes = 99
	incoming.MainKey = "ofm-imported"
	// bind/port are pinned by the handler and must survive the import
	incoming.Port = 9999
	incoming.ListenHost = "0.0.0.0"
	raw, err := json.Marshal(incoming)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/admin/api/import", strings.NewReader(string(raw)))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Content-Type", "application/json")
	if w := do(t, s, r); w.Code != 200 {
		t.Fatalf("import status %d: %s", w.Code, w.Body.String())
	}

	live := s.Store.Config()
	if live.DefaultEffort != "deep" || live.DefaultMaxTokens != 12345 || live.ProbeIntervalMinutes != 99 {
		t.Fatalf("import was not applied: %+v", live)
	}
	if live.Port == 9999 || live.ListenHost == "0.0.0.0" {
		t.Fatalf("import changed the live bind (port=%d host=%q)", live.Port, live.ListenHost)
	}
	// the running lane must follow the imported values, not just the file
	req := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	w := do(t, s, req)
	if !strings.Contains(w.Body.String(), `"defaultMaxTokens":12345`) {
		t.Fatalf("imported value not visible in /state: %s", w.Body.String())
	}

	// and it must survive a restart
	reopened, err := store.Open()
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if got := reopened.Config(); got.DefaultEffort != "deep" || got.DefaultMaxTokens != 12345 {
		t.Fatalf("import was not persisted: %+v", got)
	}
}

// TestStateShipsEveryFieldTheDashboardReads pins the two /state gaps the SPA
// renders against: model rows carried no regionSensitive flag (so the
// 地区敏感 badge never appeared) and the settings block had no anonymousRpm
// (so the 匿名调用限速 field always showed the fallback 60 and saving the form
// overwrote the real value — silently re-enabling keyless access that had been
// switched off with 0).
func TestStateShipsEveryFieldTheDashboardReads(t *testing.T) {
	s := newTestServer(t)
	s.Store.Update(func(c *store.Config) { c.AnonymousRPM = 7 })

	r := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	w := do(t, s, r)
	if w.Code != 200 {
		t.Fatalf("state status %d", w.Code)
	}
	var payload struct {
		Settings map[string]any   `json:"settings"`
		Models   []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if got, ok := payload.Settings["anonymousRpm"]; !ok {
		t.Error("/state settings is missing anonymousRpm (dashboard would save a stale 60)")
	} else if n, _ := got.(float64); int(n) != 7 {
		t.Errorf("anonymousRpm = %v, want 7", got)
	}
	if len(payload.Models) == 0 {
		t.Fatal("no models in state")
	}
	if _, ok := payload.Models[0]["regionSensitive"]; !ok {
		t.Error("/state model rows are missing regionSensitive (地区敏感 badge can never render)")
	}
}

// TestProxyTestAcceptsSchemelessAddress pins the validation mismatch: the
// settings page happily saves "127.0.0.1:7890" and the lane normalises it to
// http://, but the connectivity test parsed the raw string and rejected the
// very spelling the gateway accepts as "代理地址不合法".
func TestProxyTestAcceptsSchemelessAddress(t *testing.T) {
	if u, err := lane.ParseProxyURL("127.0.0.1:7890"); err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:7890" {
		t.Fatalf("lane.ParseProxyURL(\"127.0.0.1:7890\") = %v, %v", u, err)
	}
	s := newTestServer(t)
	// A reachable proxy is not required: the assertion is that the address is
	// accepted and an upstream attempt is made, rather than a 400.
	withFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	r := httptest.NewRequest(http.MethodPost, "/admin/api/proxy-test",
		strings.NewReader(`{"mode":"custom","url":"127.0.0.1:1"}`))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Content-Type", "application/json")
	w := do(t, s, r)
	if w.Code == 400 {
		t.Fatalf("schemeless proxy address rejected: %s", w.Body.String())
	}
}

// TestAdminImportAppliesProxyPool pins the import fix: importing a config
// with the proxy pool enabled used to persist the numbers but leave the
// running lane on the old egress mode and the pool on the old sources until
// the next restart — the import reported success while nothing took effect.
func TestAdminImportAppliesProxyPool(t *testing.T) {
	s := newTestServer(t)
	s.SetProxyPool(lane.NewProxyPool(nil))

	body := `{"mainKey":"ofm-imported","port":9999,
		"proxyPoolEnabled":true,
		"proxyPoolSources":["http://example.com/list.txt"],
		"proxyPoolMaxTest":42,"proxyPoolConcurrency":7,
		"proxyPoolTimeoutSec":9,"proxyPoolVerifyUpstream":false}`
	r := httptest.NewRequest(http.MethodPost, "/admin/api/import", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Content-Type", "application/json")
	w := do(t, s, r)
	if w.Code != 200 {
		t.Fatalf("import failed: %d %s", w.Code, w.Body.String())
	}

	cfg := s.Store.Config()
	if !cfg.ProxyPoolEnabled || cfg.ProxyPoolMaxTest != 42 || cfg.ProxyPoolTimeoutSec != 9 {
		t.Fatalf("imported config not persisted: %+v", cfg)
	}
	sources, maxTest, conc, timeoutSec, verify := s.pool.Config()
	if len(sources) != 1 || sources[0] != "http://example.com/list.txt" {
		t.Errorf("pool sources not reconfigured: %v", sources)
	}
	if maxTest != 42 || conc != 7 || timeoutSec != 9 || verify {
		t.Errorf("pool parameters not reconfigured: maxTest=%d conc=%d timeout=%ds verify=%v",
			maxTest, conc, timeoutSec, verify)
	}
}
