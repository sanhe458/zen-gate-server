package lane

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveProxyPoolFetch exercises the real network path: it pulls the public
// sources and probes a slice of candidates, reporting what came back.
//
// Opt-in only — it needs unrestricted outbound access and takes about a
// minute, so the default test run (and CI) skips it:
//
//	ZG_LIVE_TEST=1 go test ./internal/lane/ -run TestLiveProxyPoolFetch -v
func TestLiveProxyPoolFetch(t *testing.T) {
	if os.Getenv("ZG_LIVE_TEST") == "" {
		t.Skip("live network test — set ZG_LIVE_TEST=1 to run")
	}
	pool := NewProxyPool(func(f string, a ...any) { t.Logf(f, a...) })
	// keep the live run small: sources are real, probing 400 candidates at 80
	// concurrency takes well under a minute
	pool.Configure(nil, 400, 80, 6, true)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	start := time.Now()
	tested, passed, err := pool.Refresh(ctx)
	el := time.Since(start)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	t.Logf("抓取+测速完成：测试 %d，可用 %d，耗时 %s", tested, passed, el.Round(time.Millisecond))

	cur, entries, _, stats, _, _ := pool.Snapshot()
	t.Logf("当前出口=%q fetched=%d tested=%d passed=%d exhausted=%d",
		cur, stats.Fetched, stats.Tested, stats.Passed, stats.Exhausted)

	shown := 0
	for _, e := range entries {
		if !e.OK {
			continue
		}
		t.Logf("  %-30s %5dms  %-10s status=%d %s exhausted=%v",
			e.URL, e.LatencyMs, e.Scheme, e.Status, e.Detail, e.Exhausted)
		shown++
		if shown >= 15 {
			break
		}
	}
	if passed == 0 {
		t.Logf("⚠ 本轮没有可用代理（免费代理池属正常现象，稍后重试或增大样本）")
	}

	// rotation must land on a different node than the current one
	if passed >= 2 {
		first, err := pool.Rotate(ctx, true)
		if err != nil {
			t.Fatalf("rotate failed: %v", err)
		}
		second, err := pool.Rotate(ctx, true)
		if err != nil {
			t.Fatalf("second rotate failed: %v", err)
		}
		t.Logf("轮换: %q → %q", first, second)
		if first == second {
			t.Errorf("rotation did not move to a new proxy")
		}
		if ActiveProxy() == nil {
			t.Errorf("dynamic proxy not installed after rotate")
		}
	}
}
