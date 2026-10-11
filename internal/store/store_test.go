package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zen-gate-server/internal/lane"
)

// TestRecordAfterReloadWithNilAgentMap pins the panic fix: every DayStat slice
// field is `omitempty`, so a day that carried no anonymous/shared traffic comes
// back from disk with a nil Agents map. Recording into it used to panic *after*
// the upstream call had already succeeded, turning a good answer into a 500.
func TestRecordAfterReloadWithNilAgentMap(t *testing.T) {
	dir := t.TempDir()
	day := "2026-10-10"
	raw := map[string]any{
		"version": 1,
		"days": map[string]any{
			day: map[string]any{
				"requests": 3,
				"models":   map[string]any{"mimo-v2.6-flash-free": 1234},
				// "agents" deliberately absent — only the main key called
			},
		},
	}
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(dir, "stats.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZEN_GATE_HOME", dir)

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local).UnixMilli()
	s.Record(lane.CallRecord{Model: "mimo-v2.6-flash-free", Agent: "anonymous", Ok: true, At: at, Output: 5})

	days, _ := s.SnapshotStats()
	d := days[day]
	if d == nil {
		t.Fatalf("day %s missing from the snapshot", day)
	}
	if got := d.Agents["anonymous"]; got != 5 {
		t.Fatalf("anonymous usage not recorded: got %d, want 5", got)
	}
	if d.Requests != 4 {
		t.Fatalf("requests = %d, want 4", d.Requests)
	}
}

// TestSnapshotStatsDoesNotShareLiveMaps pins the shallow-copy fix: the caller
// reads the snapshot after the store lock is released, so it must not alias the
// maps Record is writing.
func TestSnapshotStatsDoesNotShareLiveMaps(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixMilli()
	s.Record(lane.CallRecord{Model: "m", Agent: "a", Ok: true, At: at, Output: 1})

	days, _ := s.SnapshotStats()
	var snap *DayStat
	for _, v := range days {
		snap = v
	}
	if snap == nil {
		t.Fatal("empty snapshot")
	}

	s.Record(lane.CallRecord{Model: "m", Agent: "a", Ok: true, At: at, Output: 100})

	if snap.Models["m"] != 1 {
		t.Errorf("snapshot aliases the live Models map (got %d after a later write)", snap.Models["m"])
	}
	if snap.Agents["a"] != 1 {
		t.Errorf("snapshot aliases the live Agents map (got %d after a later write)", snap.Agents["a"])
	}
}

// TestSnapshotStatsConcurrentWithRecord is the race-detector guard: the history
// endpoint polls this while traffic keeps recording. Run with -race.
func TestSnapshotStatsConcurrentWithRecord(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.Record(lane.CallRecord{Model: "m", Agent: "share:x", Ok: true, At: time.Now().UnixMilli()})
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			days, _ := s.SnapshotStats()
			for _, d := range days {
				_ = d.Models["m"]
				_ = d.Agents["share:x"]
				_ = d.Shares["x"]
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestKeyForAgentDoesNotDeadlock pins the re-entrant lock fix: KeyForAgent used
// to call Save() (which takes s.mu) while already holding s.mu, hanging forever
// and wedging every later store operation.
func TestKeyForAgentDoesNotDeadlock(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- s.KeyForAgent("agent-a") }()
	select {
	case k := <-done:
		if k == "" {
			t.Fatal("empty agent key")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("KeyForAgent deadlocked")
	}

	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Config().AgentKeys["agent-a"]; got == "" {
		t.Fatal("agent key was not persisted")
	}
}

// TestFreshInstallProxyPoolDefaults pins the migration-order fix: the `< 4`
// gate used to sit behind the `< 5` gate, so a brand-new install never picked
// up the documented proxy-pool defaults.
func TestFreshInstallProxyPoolDefaults(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	c := s.Config()
	if c.SchemaVersion != 5 {
		t.Errorf("SchemaVersion = %d, want 5", c.SchemaVersion)
	}
	if !c.ProxyPoolAutoRotate {
		t.Error("proxyPoolAutoRotate is off on a fresh install (README documents it as on)")
	}
	if !c.ProxyPoolVerifyUpstream {
		t.Error("proxyPoolVerifyUpstream is off on a fresh install (README documents it as on)")
	}
	if c.ProxyPoolRefreshMinutes != 10 {
		t.Errorf("proxyPoolRefreshMinutes = %d, want 10", c.ProxyPoolRefreshMinutes)
	}
	if c.ProxyPoolMaxTest != 1500 || c.ProxyPoolConcurrency != 120 ||
		c.ProxyPoolTimeoutSec != 6 || c.ProxyPoolMinUsable != 3 {
		t.Errorf("pool sizing defaults wrong: maxTest=%d concurrency=%d timeoutSec=%d minUsable=%d",
			c.ProxyPoolMaxTest, c.ProxyPoolConcurrency, c.ProxyPoolTimeoutSec, c.ProxyPoolMinUsable)
	}
	if !c.FailoverEnabled || c.FailoverMax != 2 {
		t.Errorf("v2 failover defaults not applied: enabled=%v max=%d", c.FailoverEnabled, c.FailoverMax)
	}
	if c.AnonymousRPM != 60 {
		t.Errorf("v5 anonymous budget = %d, want 60", c.AnonymousRPM)
	}
}

// TestMigrationRunsEveryStepInOrder walks an old config through the whole
// ladder: every gate must fire exactly once, in ascending order.
func TestMigrationRunsEveryStepInOrder(t *testing.T) {
	dir := t.TempDir()
	raw := map[string]any{"mainKey": "ofm-keep-me", "port": 8787, "schemaVersion": 1}
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZEN_GATE_HOME", dir)

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	c := s.Config()
	if c.SchemaVersion != 5 {
		t.Fatalf("SchemaVersion = %d, want 5", c.SchemaVersion)
	}
	if c.MainKey != "ofm-keep-me" {
		t.Error("migration clobbered the existing main key")
	}
	if !c.FailoverEnabled {
		t.Error("v2 not applied")
	}
	if !c.ProxyPoolAutoRotate || c.ProxyPoolRefreshMinutes != 10 {
		t.Errorf("v4 not applied: autoRotate=%v refresh=%d", c.ProxyPoolAutoRotate, c.ProxyPoolRefreshMinutes)
	}
	if c.AnonymousRPM != 60 {
		t.Errorf("v5 not applied: anonymousRpm=%d", c.AnonymousRPM)
	}
}
