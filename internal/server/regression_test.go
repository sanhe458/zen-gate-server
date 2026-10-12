package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate-server/internal/lane"
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
