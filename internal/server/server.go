// Package server serves the OpenAI/Anthropic-compatible API and the admin
// WebUI for zen-gate-server, the Linux headless edition of zen-gate.
//
// Unlike the Windows desktop build (loopback-only, tray-hosted), the server
// edition binds a configurable address — 127.0.0.1 by default, 0.0.0.0 for
// LAN/container deployments. When a non-loopback listener is configured the
// dashboard requires the WebUI password; the /v1 API always requires an API
// key.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "embed"

	"zen-gate-server/internal/lane"
	"zen-gate-server/internal/logx"
	"zen-gate-server/internal/store"
)

// Version is the running build; overridable via -ldflags.
var Version = "1.0.0"

//go:embed web/favicon.png
var faviconPNG []byte

// startedAt records process boot for the uptime display.
var startedAt = time.Now()

// Server is the HTTP surface.
type Server struct {
	Lane  *lane.Lane
	Store *store.Store
	mux   *http.ServeMux
	srv   *http.Server
	addr  atomic.Value // string
	// logger is injected by main; nil-safe helpers fall back to empty.
	logger interface {
		Tail(minLevel string, limit int) []logx.Entry
		Infof(format string, args ...interface{})
		Warnf(format string, args ...interface{})
		Errorf(format string, args ...interface{})
	}
	// webKey is a per-process random salt mixed into dashboard session
	// cookies so tokens never repeat across restarts.
	webKey []byte
}

// SetLogger wires the logx logger (as a structural interface, no import cycle).
func (s *Server) SetLogger(l interface {
	Tail(minLevel string, limit int) []logx.Entry
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}) {
	s.logger = l
}

// New builds a server; Handler is immediately usable (for tests).
func New(l *lane.Lane, st *store.Store) *Server {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	s := &Server{Lane: l, Store: st, webKey: b}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.safeRoute)
	s.mux = mux
	return s
}

// safeRoute keeps a handler panic from killing the connection silently.
func (s *Server) safeRoute(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			if s.logger != nil {
				s.logger.Errorf("handler panic: %v", p)
			}
			writeJSON(w, 500, map[string]any{"error": map[string]any{
				"message": "internal panic (recovered)", "type": "api_error"}})
		}
	}()
	s.route(w, r)
}

// Start binds the configured address and serves in the background.
func (s *Server) Start() error {
	cfg := s.Store.Config()
	host := strings.TrimSpace(cfg.ListenHost)
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(cfg.Port))
	s.srv = &http.Server{Addr: addr, Handler: s.mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.addr.Store(ln.Addr().String())
	if s.logger != nil {
		s.logger.Infof("listening on %s", addr)
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[zen-gate-server] http serve: %v", err)
		}
	}()
	return nil
}

// Stop shuts the listener down.
func (s *Server) Stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

// BaseURL is the externally advertised endpoint.
func (s *Server) BaseURL() string {
	if a, ok := s.addr.Load().(string); ok {
		return "http://" + a + "/v1"
	}
	host := s.Store.Config().ListenHost
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d/v1", host, s.Store.Config().Port)
}

// ListenAddr is the display form of the dashboard origin.
func (s *Server) ListenAddr() string {
	if a, ok := s.addr.Load().(string); ok {
		return a
	}
	host := s.Store.Config().ListenHost
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(s.Store.Config().Port))
}

// loopbackListener reports whether the configured bind is loopback-only.
// An unspecified address (0.0.0.0 / ::) or a hostname counts as exposed.
func (s *Server) loopbackListener() bool {
	host := strings.TrimSpace(s.Store.Config().ListenHost)
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // hostnames: assume exposed
	}
	return ip.IsLoopback()
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	// CORS for the /v1 API surface: any origin may call it with a key.
	if strings.HasPrefix(path, "/v1/") || path == "/v1" {
		w.Header().Set("access-control-allow-origin", "*")
		w.Header().Set("access-control-allow-headers", "authorization, x-api-key, content-type, anthropic-version")
		w.Header().Set("access-control-allow-methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
	}
	switch {
	case path == "/health":
		writeJSON(w, 200, map[string]any{"ok": true, "service": "zen-gate-server", "version": Version})
	case path == "/v1/models" && r.Method == http.MethodGet:
		if !s.authorized(w, r) {
			return
		}
		s.handleListModels(w, r)
	case path == "/v1/codex-catalog" && r.Method == http.MethodGet:
		s.handleCodexCatalog(w, r)
	case path == "/v1/chat/completions" && r.Method == http.MethodPost:
		if !s.authorized(w, r) {
			return
		}
		s.handleChatCompletions(w, r)
	case path == "/v1/responses" && r.Method == http.MethodPost:
		if !s.authorized(w, r) {
			return
		}
		s.handleResponses(w, r)
	case path == "/v1/messages" && r.Method == http.MethodPost:
		if !s.authorized(w, r) {
			return
		}
		s.handleAnthropicMessages(w, r)
	case path == "/favicon.png":
		w.Header().Set("content-type", "image/png")
		_, _ = w.Write(faviconPNG)
	case path == "" || path == "/" || path == "/admin" || path == "/admin/" || path == "/admin/login":
		s.serveDashboard(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		s.handleAdmin(w, r, strings.TrimPrefix(path, "/admin/api/"))
	default:
		writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "not found", "type": "invalid_request_error", "code": "not_found"}})
	}
}

// authorized checks the Bearer / x-api-key key against the main key and every
// agent subkey, returning the caller's label via context-free lookup below.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	key := bearerOf(r)
	if key == "" {
		writeJSON(w, 401, openaiError("missing API key", "authentication_error"))
		return false
	}
	if !s.keyMatches(key) {
		writeJSON(w, 401, openaiError("invalid API key", "authentication_error"))
		return false
	}
	return true
}

// agentOf resolves which label a key belongs to ("" = main key / unknown).
func (s *Server) agentOf(r *http.Request) string {
	key := bearerOf(r)
	cfg := s.Store.Config()
	if subtle.ConstantTimeCompare([]byte(key), []byte(cfg.MainKey)) == 1 {
		return ""
	}
	for id, k := range cfg.AgentKeys {
		if k != "" && subtle.ConstantTimeCompare([]byte(key), []byte(k)) == 1 {
			return id
		}
	}
	return ""
}

func (s *Server) keyMatches(key string) bool {
	cfg := s.Store.Config()
	if len(key) > 0 && subtle.ConstantTimeCompare([]byte(key), []byte(cfg.MainKey)) == 1 {
		return true
	}
	for _, k := range cfg.AgentKeys {
		if k != "" && len(k) == len(key) && subtle.ConstantTimeCompare([]byte(key), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

func bearerOf(r *http.Request) string {
	h := r.Header.Get("authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// handleListModels advertises the servable models in OpenAI shape. Reasoning
// models additionally expose (light)/(deep) variants so a client's model
// picker can select the effort directly.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, m := range s.Lane.ServableModels() {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"owned_by": "zen-gate-server",
		})
		if m.Reasoning {
			for _, suffix := range []string{"(light)", "(deep)"} {
				data = append(data, map[string]any{
					"id":       m.ID + " " + suffix,
					"object":   "model",
					"owned_by": "zen-gate-server",
				})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// handleCodexCatalog serves the model list in Codex's remote-catalog shape so
// the desktop model picker can offer the free models under the zen_gate
// provider. Deliberately unauthenticated: the listing is non-sensitive, and a
// 401 here makes Codex surface a login prompt instead of the models.
func (s *Server) handleCodexCatalog(w http.ResponseWriter, r *http.Request) {
	models, av, _ := s.Lane.Snapshot()
	priority := map[string]int{
		lane.StateAvailable: 100,
		lane.StateUnknown:   50,
		lane.StateThrottled: 20,
	}
	stateOf := func(id string) string {
		if p, ok := av[id]; ok {
			return p.State
		}
		return lane.StateUnknown
	}
	sort.SliceStable(models, func(i, j int) bool {
		pi, pj := priority[stateOf(models[i].ID)], priority[stateOf(models[j].ID)]
		// Region-gated models sink to the bottom: with a CN egress they only
		// ever answer with a RegionError.
		if models[i].RegionSensitive != models[j].RegionSensitive {
			return models[j].RegionSensitive
		}
		return pi > pj
	})
	levels := []map[string]any{
		{"effort": "low", "description": "轻量，最省额度"},
		{"effort": "medium", "description": "均衡"},
		{"effort": "high", "description": "深思"},
	}
	out := []map[string]any{}
	for _, m := range models {
		desc := "免费车道 · 当前不可用"
		switch stateOf(m.ID) {
		case lane.StateAvailable:
			desc = "免费车道 · 实测可用"
		case lane.StateThrottled:
			desc = "免费车道 · 已限额，稍后恢复"
		case lane.StateUnknown:
			desc = "免费车道 · 尚未探测"
		}
		if m.RegionSensitive {
			desc += " · 可能被地区门拦截"
		}
		lv := levels
		if !m.Reasoning {
			lv = []map[string]any{{"effort": "medium", "description": "默认"}}
		}
		mods := []string{"text"}
		if m.Vision {
			mods = append(mods, "image")
		}
		out = append(out, map[string]any{
			"slug":                         m.ID,
			"display_name":                 m.Name,
			"description":                  desc,
			"base_instructions":            fmt.Sprintf("You are %s (model id: %s), a coding agent serving the user's Codex app through the Zen Gate local gateway. Work toward the user's goal with the available tools and verify your changes. Always reply in the same language as the user's latest message — when the user writes Chinese, reply in Simplified Chinese. Be concise.", m.Name, m.ID),
			"default_reasoning_level":      "medium",
			"supported_reasoning_levels":   lv,
			"shell_type":                   "unified_exec",
			"support_verbosity":            false,
			"truncation_policy":            map[string]any{"mode": "tokens", "limit": 10000},
			"experimental_supported_tools": []string{},
			"input_modalities":             mods,
			"visibility":                   "list",
			"supported_in_api":             true,
			"priority":                     priority[stateOf(m.ID)],
			"provider_id":                  "zen_gate",
			"context_window":               m.ContextWindow,
			"max_output_tokens":            m.MaxOutput,
		})
	}
	writeJSON(w, 200, map[string]any{"models": out})
}

// resolveEffort maps a requested model id onto an effort level: an explicit
// "(light)/(balanced)/(deep)" model-name suffix wins, then an effort the
// client declared in the request body (reasoning_effort and friends, as sent
// by ZCode's thought-level selector), then the configured default.
func (s *Server) resolveEffort(model string, declared ...string) string {
	if e := lane.EffortOf(model); e != "" {
		return e
	}
	for _, d := range declared {
		if e := normalizeEffortParam(d); e != "" {
			return e
		}
	}
	if e := s.Store.Config().DefaultEffort; e == "light" || e == "deep" {
		return e
	}
	return "balanced"
}

// normalizeEffortParam maps client effort spellings onto the three budgets.
// The lane cannot truly switch thinking off, so off-ish levels land on light.
func normalizeEffortParam(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "light":
		return "light"
	case "balanced", "medium":
		return "balanced"
	case "deep", "high", "xhigh":
		return "deep"
	case "minimal", "low", "none", "off", "disabled":
		return "light"
	}
	return ""
}

// --- session seeding ---------------------------------------------------------

// SessionSeed derives a stable upstream session identity for one conversation.
// Quota is accounted per session upstream, so the same conversation must keep
// landing on the same session — across restarts too.
func SessionSeed(agent, userField string, messages []lane.Message) string {
	first := ""
	for _, m := range messages {
		if m.Role == lane.RoleUser {
			first = m.TextOf()
			break
		}
	}
	if len(first) > 500 {
		first = first[:500]
	}
	parts := []string{agent, userField, first}
	joined := ""
	for _, p := range parts {
		joined += p + "\x00"
	}
	return joined
}

// TurnSeed is stable across retries of one turn, changes next turn.
func TurnSeed(messages []lane.Message) string {
	total := 0
	for _, m := range messages {
		total += len(m.TextOf()) + 8
	}
	// A cheap, stable fingerprint: roles + total text length + last message.
	last := ""
	if len(messages) > 0 {
		last = messages[len(messages)-1].TextOf()
		if len(last) > 200 {
			last = last[:200]
		}
	}
	return fmt.Sprintf("%d|%d|%s", len(messages), total, last)
}

// --- dashboard auth ----------------------------------------------------------

// dashboardAuthed checks WebUI access. Loopback callers are always trusted;
// remote callers must present a valid session cookie when a password is set.
func (s *Server) dashboardAuthed(r *http.Request) bool {
	if remoteIsLoopback(r) {
		return true
	}
	pw := strings.TrimSpace(s.Store.Config().WebUIPassword)
	if pw == "" {
		// No password configured: the WebUI stays read-only-safe only when
		// the caller is on the loopback; remote access without a password is
		// denied outright.
		return false
	}
	c, err := r.Cookie("zg_session")
	if err != nil {
		return false
	}
	want := s.sessionToken(pw)
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

// sessionToken derives the dashboard session cookie value. It rotates daily
// and is keyed by the process-random webKey, so stolen cookies age out and
// tokens never survive a restart.
func (s *Server) sessionToken(pw string) string {
	day := time.Now().Format("2006-01-02")
	sum := sha256.Sum256([]byte("zen-gate-server-session\x00" + pw + "\x00" + day + "\x00" + string(s.webKey)))
	return hex.EncodeToString(sum[:])
}

// remoteIsLoopback reports whether the request arrived from the loopback
// interface; such callers are always trusted for dashboard access.
func remoteIsLoopback(r *http.Request) bool {
	remote := r.RemoteAddr
	if h, _, err := net.SplitHostPort(remote); err == nil {
		remote = h
	}
	return remote == "127.0.0.1" || remote == "::1"
}

// --- helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func openaiError(message, typ string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": typ, "code": nil}}
}

// setRetryAfter surfaces the upstream Retry-After (already parsed in the lane)
// to clients that back off on 429s.
func setRetryAfter(w http.ResponseWriter, uerr *lane.UpstreamError) {
	if uerr != nil && uerr.Code == lane.CodeQuota && uerr.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(uerr.RetryAfter))
	}
}

// withSuggestions appends failover candidates to a 429 error body so a client
// whose request burned out can immediately retry with a model that answers.
func withSuggestions(body map[string]any, sugg []string) map[string]any {
	if len(sugg) == 0 {
		return body
	}
	if errObj, ok := body["error"].(map[string]any); ok {
		errObj["suggestions"] = sugg
	}
	return body
}

func randomID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
