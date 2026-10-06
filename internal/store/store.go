// Package store persists zen-gate-server configuration and usage statistics
// as atomic JSON files under ${XDG_DATA_HOME:-~/.local/share}/zen-gate
// (or $ZEN_GATE_HOME).
package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"zen-gate-server/internal/lane"
)

// Home returns the data directory.
func Home() string {
	if h := os.Getenv("ZEN_GATE_HOME"); h != "" {
		return h
	}
	if h := os.Getenv("XDG_DATA_HOME"); h != "" {
		return filepath.Join(h, "zen-gate")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "zen-gate")
}

// Config is the persisted service configuration.
type WindowState struct {
	X         int  `json:"x"`
	Y         int  `json:"y"`
	W         int  `json:"w"`
	H         int  `json:"h"`
	Maximized bool `json:"maximized"`
}

type Config struct {
	SchemaVersion        int    `json:"schemaVersion"`
	Port                 int    `json:"port"`
	ListenHost           string `json:"listenHost,omitempty"` // "" → loopback; "0.0.0.0" exposes the LAN
	WebUIPassword        string `json:"webuiPassword,omitempty"` // empty → dashboard open to listeners only
	MainKey              string `json:"mainKey"`
	AgentKeys            map[string]string `json:"agentKeys"`
	// ShareKeys are the hand-out keys (label → key). They are equivalent to
	// the main key for auth, but usage is attributed per label so the
	// 共享 page can show each recipient's own numbers.
	ShareKeys            map[string]string `json:"shareKeys,omitempty"`
	DefaultMaxTokens     int    `json:"defaultMaxTokens"`
	DefaultEffort        string `json:"defaultEffort"`
	ProbeIntervalMinutes int    `json:"probeIntervalMinutes"`
	ExposeRegion         bool   `json:"exposeRegion"`
	EnabledAgents        map[string]bool   `json:"enabledAgents"`
	CloseToTray          bool   `json:"closeToTray"`
	Notifications        bool   `json:"notifications"`
	UpdateFeed           string `json:"updateFeed"`
	LastVersion          string `json:"lastVersion,omitempty"`
	StatsServerURL       string `json:"statsServerUrl,omitempty"`
	InstallID            string `json:"installId,omitempty"`
	ProxyMode            string `json:"proxyMode"` // env | system | direct | custom
	ProxyURL             string `json:"proxyUrl,omitempty"`
	FailoverEnabled      bool   `json:"failoverEnabled"`
	FailoverMax          int    `json:"failoverMax"`
	Window               WindowState `json:"window"`
}

// DayStat aggregates one calendar day.
type DayStat struct {
	Requests  int            `json:"requests"`
	Failed    int            `json:"failed"`
	Input     int            `json:"input"`
	Output    int            `json:"output"`
	Models    map[string]int `json:"models,omitempty"`
	ModelReqs map[string]int `json:"modelReqs,omitempty"`
	Agents    map[string]int `json:"agents,omitempty"`
	Shares    map[string]ShareStat `json:"shares,omitempty"`
}

// ShareStat is one shared key's usage on a single day.
type ShareStat struct {
	Requests int `json:"requests"`
	Failed   int `json:"failed"`
	Input    int `json:"input"`
	Output   int `json:"output"`
}

// Stats is the persisted usage accounting.
type Stats struct {
	Version int                   `json:"version"`
	Days    map[string]*DayStat   `json:"days"`
	Recent  []lane.CallRecord     `json:"recent,omitempty"`
	mu      sync.Mutex
	dirty   bool
}

const statsVersion = 1
const maxRecent = 40

// QuotaEpisode is one observed throttle window, in epoch ms.
type QuotaEpisode struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // 0 = still open
}

// QuotaNote persists one model's throttle state and episode history so
// recovery-time estimates survive restarts.
type QuotaNote struct {
	ThrottledAt   int64          `json:"throttledAt,omitempty"`
	CooldownUntil int64          `json:"cooldownUntil,omitempty"`
	LastOK        int64          `json:"lastOk,omitempty"`
	Episodes      []QuotaEpisode `json:"episodes,omitempty"`
}

const quotaEpisodeTTL = 14 * 24 * time.Hour

// Store owns config + stats files.
type Store struct {
	Home      string
	cfg       *Config
	stats     *Stats
	quota     map[string]QuotaNote
	perf      map[string][]int64 // per-model probe first-token samples (ms)
	perfDirty bool
	mu        sync.Mutex
}

const perfMaxSamples = 30

// AddTTFTSample records one probe first-token latency (ms). Samples are a
// per-model ring; the mean over them is the "平均首字" shown in the picker.
func (s *Store) AddTTFTSample(model string, ms int64) {
	if ms <= 0 || ms > 120000 { // ignore absurd outliers
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perf[model] = append(s.perf[model], ms)
	if len(s.perf[model]) > perfMaxSamples {
		s.perf[model] = s.perf[model][len(s.perf[model])-perfMaxSamples:]
	}
	s.perfDirty = true
}

// TTFTStats returns the sample mean and count for one model.
func (s *Store) TTFTStats(model string) (avg int64, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	samples := s.perf[model]
	if len(samples) == 0 {
		return 0, 0
	}
	var sum int64
	for _, v := range samples {
		sum += v
	}
	return sum / int64(len(samples)), len(samples)
}

// SnapshotPerf copies every model's sample ring.
func (s *Store) SnapshotPerf() map[string][]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]int64, len(s.perf))
	for k, v := range s.perf {
		cp := make([]int64, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// FlushPerf persists the sample rings when they changed.
func (s *Store) FlushPerf() {
	s.mu.Lock()
	if !s.perfDirty {
		s.mu.Unlock()
		return
	}
	s.perfDirty = false
	snapshot := make(map[string][]int64, len(s.perf))
	for k, v := range s.perf {
		cp := make([]int64, len(v))
		copy(cp, v)
		snapshot[k] = cp
	}
	s.mu.Unlock()
	_ = writeJSON(filepath.Join(s.Home, "perf.json"), snapshot)
}

// Open loads (or seeds) the store.
func Open() (*Store, error) {
	home := Home()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	s := &Store{Home: home}
	cfg, err := loadJSON[Config](filepath.Join(home, "config.json"))
	if err != nil || cfg.MainKey == "" {
		def := defaultConfig()
		cfg = &def
		if err := writeJSON(filepath.Join(home, "config.json"), cfg); err != nil {
			return nil, err
		}
	}
	if cfg.AgentKeys == nil {
		cfg.AgentKeys = map[string]string{}
	}
	if cfg.ShareKeys == nil {
		cfg.ShareKeys = map[string]string{}
	}
	if cfg.EnabledAgents == nil {
		cfg.EnabledAgents = map[string]bool{}
	}
	if cfg.Port == 0 {
		cfg.Port = 8787
	}
	if cfg.DefaultMaxTokens <= 0 {
		cfg.DefaultMaxTokens = 32768
	}
	if cfg.DefaultEffort != "light" && cfg.DefaultEffort != "deep" {
		cfg.DefaultEffort = "balanced"
	}
	if cfg.ProxyMode == "" {
		cfg.ProxyMode = "env"
	}
	// v2: failover ships enabled; the version gate keeps an explicitly
	// disabled setting sticky on later boots.
	if cfg.SchemaVersion < 2 {
		cfg.SchemaVersion = 2
		cfg.FailoverEnabled = true
		cfg.FailoverMax = 2
	}
	if cfg.FailoverMax <= 0 {
		cfg.FailoverMax = 2
	}
	// v3: point the update feed at the project's GitHub Releases by default
	// (a custom feed set by the user wins) and mint the anonymous install id.
	if cfg.SchemaVersion < 3 {
		cfg.SchemaVersion = 3
		if strings.TrimSpace(cfg.UpdateFeed) == "" {
			cfg.UpdateFeed = "https://api.github.com/repos/LAGcomcom/zen-gate/releases/latest"
		}
	}
	if strings.TrimSpace(cfg.InstallID) == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		cfg.InstallID = hex.EncodeToString(b)
	}
	if cfg.SchemaVersion == 0 {
		// first migration to the versioned schema: mature defaults
		cfg.SchemaVersion = 1
		cfg.CloseToTray = true
		cfg.Notifications = true
	}
	if cfg.ProbeIntervalMinutes <= 0 {
		cfg.ProbeIntervalMinutes = 15
	}
	s.cfg = cfg

	stats, err := loadJSON[Stats](filepath.Join(home, "stats.json"))
	if err != nil || stats.Version != statsVersion {
		stats = &Stats{Version: statsVersion, Days: map[string]*DayStat{}}
		_ = writeJSON(filepath.Join(home, "stats.json"), stats)
	}
	if stats.Days == nil {
		stats.Days = map[string]*DayStat{}
	}
	s.stats = stats

	quota, err := loadJSON[map[string]QuotaNote](filepath.Join(home, "quota.json"))
	if err != nil || quota == nil {
		quota = &map[string]QuotaNote{}
	}
	s.quota = *quota

	perf, err := loadJSON[map[string][]int64](filepath.Join(home, "perf.json"))
	if err != nil || perf == nil {
		perf = &map[string][]int64{}
	}
	s.perf = *perf
	return s, nil
}

// SetQuotaNote replaces one model's persisted quota note.
func (s *Store) SetQuotaNote(model string, n QuotaNote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-quotaEpisodeTTL).UnixMilli()
	kept := n.Episodes[:0]
	for _, e := range n.Episodes {
		if e.End == 0 || e.End >= cutoff {
			kept = append(kept, e)
		}
	}
	n.Episodes = kept
	s.quota[model] = n
	_ = writeJSON(filepath.Join(s.Home, "quota.json"), s.quota)
}

// SnapshotQuota copies the persisted quota notes.
func (s *Store) SnapshotQuota() map[string]QuotaNote {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]QuotaNote, len(s.quota))
	for k, v := range s.quota {
		out[k] = v
	}
	return out
}

func defaultConfig() Config {
	return Config{
		Port:                 8787,
		MainKey:              GenerateKey(""),
		AgentKeys:            map[string]string{},
		ShareKeys:            map[string]string{},
		DefaultMaxTokens:     32768,
		ProbeIntervalMinutes: 15,
		ExposeRegion:         true,
		EnabledAgents:        map[string]bool{},
	}
}

// GenerateKey mints "ofm-…" (zen-gate keeps the same prefix shape).
func GenerateKey(prefix string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	if prefix != "" {
		return "ofm-" + prefix + "-" + base64.RawURLEncoding.EncodeToString(b)
	}
	return "ofm-" + base64.RawURLEncoding.EncodeToString(b)
}

// ShareAgentPrefix marks the usage label of a shared key inside CallRecord.
const ShareAgentPrefix = "share:"

// GenerateShareKey mints a hand-out key with its own prefix so a leaked key
// is immediately recognisable as a shared one (rotatable without touching
// the main key).
func GenerateShareKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "ofs-" + base64.RawURLEncoding.EncodeToString(b)
}

// CreateShareKey mints and stores a new shared key for one label.
func (s *Store) CreateShareKey(label string) string {
	s.mu.Lock()
	if s.cfg.ShareKeys == nil {
		s.cfg.ShareKeys = map[string]string{}
	}
	k := GenerateShareKey()
	s.cfg.ShareKeys[label] = k
	s.mu.Unlock()
	_ = s.Save()
	return k
}

// DeleteShareKey removes one shared key; it reports whether it existed.
func (s *Store) DeleteShareKey(label string) bool {
	s.mu.Lock()
	_, ok := s.cfg.ShareKeys[label]
	if ok {
		delete(s.cfg.ShareKeys, label)
	}
	s.mu.Unlock()
	if ok {
		_ = s.Save()
	}
	return ok
}

// Config returns the live config (mutable via Save).
func (s *Store) Config() *Config { return s.cfg }

// Save persists the config.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(filepath.Join(s.Home, "config.json"), s.cfg)
}

// KeyForAgent returns (creating if needed) the stable subkey of one agent.
func (s *Store) KeyForAgent(agentID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.cfg.AgentKeys[agentID]; ok && k != "" {
		return k
	}
	k := GenerateKey(agentID)
	s.cfg.AgentKeys[agentID] = k
	_ = s.Save()
	return k
}

// Record folds one call into the stats.
func (s *Store) Record(rec lane.CallRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.UnixMilli(rec.At).Format("2006-01-02")
	d, ok := s.stats.Days[day]
	if !ok {
		d = &DayStat{Models: map[string]int{}, Agents: map[string]int{}, Shares: map[string]ShareStat{}}
		s.stats.Days[day] = d
	}
	d.Requests++
	if !rec.Ok {
		d.Failed++
	}
	d.Input += rec.Input
	d.Output += rec.Output
	if rec.Model != "" {
		d.Models[rec.Model] += rec.Output
		if d.ModelReqs == nil {
			d.ModelReqs = map[string]int{}
		}
		d.ModelReqs[rec.Model]++
	}
	if rec.Agent != "" {
		d.Agents[rec.Agent] += rec.Output
		if label, ok := strings.CutPrefix(rec.Agent, ShareAgentPrefix); ok {
			if d.Shares == nil {
				d.Shares = map[string]ShareStat{}
			}
			sh := d.Shares[label]
			sh.Requests++
			if !rec.Ok {
				sh.Failed++
			}
			sh.Input += rec.Input
			sh.Output += rec.Output
			d.Shares[label] = sh
		}
	}
	s.stats.Recent = append(s.stats.Recent, rec)
	if len(s.stats.Recent) > maxRecent {
		s.stats.Recent = s.stats.Recent[len(s.stats.Recent)-maxRecent:]
	}
	pruneDays(s.stats.Days, 120)
	s.stats.dirty = true
}

// pruneDays drops day buckets older than the keep window, mirroring the
// reference store — the heatmap only ever shows ~17 weeks.
func pruneDays(days map[string]*DayStat, keep int) {
	if len(days) <= keep {
		return
	}
	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys[:len(keys)-keep] {
		delete(days, k)
	}
}

// FlushStats persists stats if dirty.
func (s *Store) FlushStats() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stats.dirty {
		return nil
	}
	s.stats.dirty = false
	return writeJSON(filepath.Join(s.Home, "stats.json"), s.stats)
}

// SnapshotStats returns a copy for display.
func (s *Store) SnapshotStats() (map[string]*DayStat, []lane.CallRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	days := map[string]*DayStat{}
	for k, v := range s.stats.Days {
		clone := *v
		days[k] = &clone
	}
	recent := make([]lane.CallRecord, len(s.stats.Recent))
	copy(recent, s.stats.Recent)
	return days, recent
}

// BackupDir is where agent adapters stash pre-injection copies.
func (s *Store) BackupDir(agentID string) string {
	dir := filepath.Join(s.Home, "backups", agentID)
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// Backup copies a file into the agent's backup dir with a timestamp suffix;
// returns the backup path.
func (s *Store) Backup(agentID, file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	name := time.Now().Format("20060102-150405") + "-" + filepath.Base(file)
	p := filepath.Join(s.BackupDir(agentID), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// LatestBackup returns the newest backup file for an agent, if any.
func (s *Store) LatestBackup(agentID, base string) string {
	dir := s.BackupDir(agentID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	for _, e := range entries {
		if base == "" || filepath.Base(e.Name()) == base || hasSuffixName(e.Name(), base) {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

func hasSuffixName(name, base string) bool {
	return len(name) > len(base) && name[len(name)-len(base):] == base
}

// --- atomic JSON persistence ----------------------------------------------

func loadJSON[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		// preserve the unreadable file for forensics, like the reference store
		_ = os.Rename(path, path+".corrupt-"+time.Now().Format("20060102150405"))
		return nil, err
	}
	return &v, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
