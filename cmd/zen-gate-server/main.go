// zen-gate-server: the Linux headless edition of zen-gate.
//
// It exposes the OpenCode Zen free lane as OpenAI / Anthropic-compatible APIs
// and ships a browser dashboard (WebUI) — no tray, no desktop: one binary,
// systemd-friendly, Docker-ready.
//
// Protocol behaviour is ported from the MIT-licensed dsh-our-free-model
// plugin (github.com/zouyuxuan122/dsh-our-free-model); usage of the free lane
// remains subject to the upstream provider's terms.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"zen-gate-server/internal/lane"
	"zen-gate-server/internal/logx"
	"zen-gate-server/internal/server"
	"zen-gate-server/internal/store"
)

var buildVersion = "1.0.0" // overridable via -ldflags

func main() {
	host := flag.String("host", "", "listen address (default 127.0.0.1; 0.0.0.0 exposes the LAN)")
	port := flag.Int("port", 0, "override listen port")
	dataDir := flag.String("data", "", "data directory (default $ZEN_GATE_HOME or ~/.local/share/zen-gate)")
	password := flag.String("password", "", "WebUI password (required for remote dashboard access)")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *version {
		fmt.Println("zen-gate-server", buildVersion)
		return
	}

	if *dataDir != "" {
		_ = os.Setenv("ZEN_GATE_HOME", *dataDir)
	}

	st, err := store.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	logger := logx.New(st.Home)
	defer logger.Close()

	cfg := st.Config()
	applied := []string{}
	if *port > 0 {
		cfg.Port = *port
		_ = st.Save()
		applied = append(applied, fmt.Sprintf("port=%d", *port))
	}
	if *host != "" && *host != cfg.ListenHost {
		cfg.ListenHost = strings.TrimSpace(*host)
		_ = st.Save()
		applied = append(applied, "host="+cfg.ListenHost)
	}
	if *password != "" && *password != cfg.WebUIPassword {
		cfg.WebUIPassword = strings.TrimSpace(*password)
		_ = st.Save()
		applied = append(applied, "webui-password=set")
	}
	bindHost := cfg.ListenHost
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}

	logger.Infof("zen-gate-server %s 启动 (listen %s:%d, data %s, proxy %s)",
		buildVersion, bindHost, cfg.Port, st.Home, cfg.ProxyMode)

	ln := lane.NewLane()
	ln.SetDefaultMaxTokens(cfg.DefaultMaxTokens)
	ln.SetExposeRegion(cfg.ExposeRegion)
	ln.SetFailover(cfg.FailoverEnabled, cfg.FailoverMax)
	ln.LoadThrottleNotes(quotaNotesFromStore(st.SnapshotQuota()))
	ln.SetThrottleUpdate(func(model string, note lane.ThrottleNote) {
		qn := store.QuotaNote{ThrottledAt: note.ThrottledAt, CooldownUntil: note.CooldownUntil, LastOK: note.LastOK}
		for _, e := range note.Episodes {
			qn.Episodes = append(qn.Episodes, store.QuotaEpisode{Start: e.Start, End: e.End})
		}
		st.SetQuotaNote(model, qn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- 代理池 ------------------------------------------------------------
	// The free lane accounts quota per IP: once the local connection is burned,
	// another egress is the only way to keep serving. The pool starts on the
	// direct line and rotates only when a quota refusal shows up.
	pool := lane.NewProxyPool(func(f string, a ...any) { logger.Infof(f, a...) })
	pool.SetStorage(filepath.Join(st.Home, "proxypool.json"))
	if err := pool.Load(); err != nil && !os.IsNotExist(err) {
		logger.Warnf("读取代理池快照失败: %v", err)
	} else if n := pool.UsableCount(); n > 0 {
		logger.Infof("已从上次快照恢复 %d 个可用代理（下次刷新会重新验证）", n)
	}
	pool.Configure(cfg.ProxyPoolSources, cfg.ProxyPoolMaxTest, cfg.ProxyPoolConcurrency,
		cfg.ProxyPoolTimeoutSec, cfg.ProxyPoolVerifyUpstream)
	if cfg.ProxyPoolEnabled {
		lane.SetProxy("pool", "")
		src, _, _, _, _ := pool.Config()
		logger.Infof("代理池已启用：起始出口为本机直连，额度耗尽时自动切换（源 %d 个）", len(src))
	} else {
		lane.SetProxy(cfg.ProxyMode, cfg.ProxyURL)
	}

	// rotate debounce: one request can burn through several failover attempts,
	// so a burst of 429s must still produce exactly one egress switch.
	var lastRotate atomic.Int64
	rotate := func(reason string) {
		c := st.Config()
		if !c.ProxyPoolEnabled || !c.ProxyPoolAutoRotate {
			return
		}
		now := time.Now().UnixMilli()
		prev := lastRotate.Load()
		if now-prev < 20_000 { // 20s cooldown
			return
		}
		if !lastRotate.CompareAndSwap(prev, now) {
			return
		}
		go func() {
			old := pool.Current()
			url, err := pool.Rotate(ctx, true) // triggered by a quota refusal
			if err != nil {
				logger.Warnf("出口切换失败（%s）：%v", reason, err)
				return
			}
			from := old
			if from == "" {
				from = "本机直连"
			}
			logger.Infof("因%s，出口 %s → %s", reason, from, url)
			// quota is per IP: the new egress owes nothing, so every throttle
			// verdict recorded on the old one is void
			ln.ClearThrottle()
			ln.ProbeRound(ctx, true)
		}()
	}

	// quota refusal on live traffic
	ln.OnQuotaExhausted = func(model string, retryAfter int) {
		if !st.Config().ProxyPoolEnabled {
			return
		}
		logger.Warnf("请求额度不足（%s，Retry-After %ds），尝试切换出口", model, retryAfter)
		rotate("请求额度不足")
	}

	ln.OnCall = func(rec lane.CallRecord) {
		st.Record(rec)
		_ = st.FlushStats()
	}
	// Probe first-token samples feed the persisted per-model average.
	ln.OnProbeResult = func(r lane.ProbeResult) {
		st.AddTTFTSample(r.Model, r.TTFTMs)
	}

	gw := server.New(ln, st)
	gw.SetLogger(logger)
	gw.SetProxyPool(pool)

	stateText := func(s string) string {
		return map[string]string{lane.StateAvailable: "可用", lane.StateUnknown: "未知",
			lane.StateThrottled: "已限额", lane.StateRegionBlock: "地区受限",
			lane.StateUnavailable: "不可用"}[s]
	}
	ln.OnProbeEdge = func(model, from, to string) {
		if model == "" {
			logger.Warnf("免费车道整轮 429 限额，探测进入指数退避")
			// the whole-lane quota wall is the strongest signal that this IP
			// has nothing left today
			rotate("整轮探测额度耗尽")
			return
		}
		logger.Infof("模型状态变化: %s %s → %s", model, stateText(from), stateText(to))
	}
	ln.OnChange = func() {} // server edition has no tray to sync
	ln.StartLoops(ctx, time.Duration(cfg.ProbeIntervalMinutes)*time.Minute)

	if err := gw.Start(); err != nil {
		logger.Errorf("listen on %s:%d: %v", bindHost, cfg.Port, err)
		fmt.Fprintln(os.Stderr, "listen error:", err)
		os.Exit(1)
	}
	dashURL := strings.TrimSuffix(gw.BaseURL(), "/v1")
	logger.Infof("API ready at %s (main key …%s)", gw.BaseURL(), tail7(cfg.MainKey))
	logger.Infof("dashboard ready at %s", dashURL)
	if bindHost != "127.0.0.1" && strings.TrimSpace(cfg.WebUIPassword) == "" {
		logger.Warnf("已绑定非回环地址但未设置 WebUI 密码：远程管理页将拒绝访问；用 -password 或 WebUI 设置页配置密码")
	}

	// periodic stats flush
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = st.FlushStats()
				st.FlushPerf()
			}
		}
	}()

	// proxy pool upkeep: a restored snapshot is already usable, so only refill
	// when the supply is actually short. Rotation itself never blocks on this —
	// it refills in place when it runs dry.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}
		check := func() {
			c := st.Config()
			if !c.ProxyPoolEnabled {
				return
			}
			if n := pool.UsableCount(); n < c.ProxyPoolMinUsable {
				logger.Infof("可用代理仅 %d 个（阈值 %d），补充代理池…", n, c.ProxyPoolMinUsable)
				pool.Refresh(ctx)
			}
		}
		check()
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				check()
			}
		}
	}()

	// graceful shutdown on SIGINT/SIGTERM (systemd sends SIGTERM)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	logger.Infof("serving %s (dashboard %s) — Ctrl+C to stop", gw.BaseURL(), dashURL)
	<-sig
	logger.Infof("收到退出信号，正在关闭…")
	cancel()
	gw.Stop()
	_ = st.FlushStats()
	st.FlushPerf()
	logger.Infof("zen-gate-server 已退出")
}

func tail7(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[len(key)-7:]
}

// quotaNotesFromStore converts persisted quota notes back into the lane's
// shape for boot-time seeding.
func quotaNotesFromStore(in map[string]store.QuotaNote) map[string]lane.ThrottleNote {
	out := map[string]lane.ThrottleNote{}
	for m, n := range in {
		note := lane.ThrottleNote{ThrottledAt: n.ThrottledAt, CooldownUntil: n.CooldownUntil, LastOK: n.LastOK}
		for _, e := range n.Episodes {
			note.Episodes = append(note.Episodes, lane.ThrottleEpisode{Start: e.Start, End: e.End})
		}
		out[m] = note
	}
	return out
}
