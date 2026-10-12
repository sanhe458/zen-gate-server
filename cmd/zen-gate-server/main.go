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
	rotate := func(reason string, markPrev bool) {
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
			url, err := pool.Rotate(ctx, markPrev)
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
		rotate("请求额度不足", true)
	}

	// live egress failure: the request never reached upstream, which in pool
	// mode usually means the proxy stopped forwarding mid-flight. Verify before
	// rotating — an upstream blip must not cost a switch — and collapse a burst
	// of failing requests into a single verification.
	var verifying atomic.Bool
	ln.OnEgressFailure = func(model, detail string) {
		if !st.Config().ProxyPoolEnabled {
			return
		}
		cur := pool.Current()
		if cur == "" {
			return // direct connection: not an egress-pool concern
		}
		if !verifying.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer verifying.Store(false)
			vctx, vcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer vcancel()
			if pool.VerifyCurrent(vctx) {
				logger.Infof("请求经代理失败（%s），但出口 %s 仍可达 → 判定上游抖动，不切换", model, cur)
				return
			}
			logger.Warnf("出口 %s 已失效（%s），标记失效并切换…", cur, shortDetail(detail))
			pool.MarkCurrentDead("运行中失效（无法转发）")
			rotate("代理失效", false) // already marked: do not also flag it as quota-burned
		}()
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
			rotate("整轮探测额度耗尽", true)
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

	// proxy pool upkeep. A restored snapshot is already usable, so the first
	// pass only tops up when the supply is short. After that a scheduled full
	// refresh re-pulls every listing and re-runs the complete test +
	// egress-dedup pass; between scheduled runs a top-up fires early if the
	// pool drops below the floor. Rotation itself never waits on any of this —
	// it refills in place when it runs dry.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}
		var lastFull, lastTopUp time.Time
		topUp := func(reason string) {
			c := st.Config()
			if !c.ProxyPoolEnabled {
				return
			}
			if n := pool.UsableCount(); n < c.ProxyPoolMinUsable && time.Since(lastTopUp) > 5*time.Minute {
				logger.Infof("可用代理仅 %d 个（阈值 %d），%s…", n, c.ProxyPoolMinUsable, reason)
				lastTopUp = time.Now()
				pool.Refresh(ctx)
			}
		}
		lastFull = time.Now()
		topUp("启动时补充")

		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c := st.Config()
				if !c.ProxyPoolEnabled {
					continue
				}
				every := time.Duration(c.ProxyPoolRefreshMinutes) * time.Minute
				if every > 0 && time.Since(lastFull) >= every {
					logger.Infof("按计划拉取最新代理列表并重新实测（每 %v 一次）…", every)
					lastFull = time.Now()
					pool.Refresh(ctx)
					continue
				}
				topUp("提前补充")
			}
		}
	}()

	// daily quota reset. The free lane's quota refreshes at midnight, so the
	// rotation blacklist — every egress IP rotated away from today — is
	// dropped and the lane returns to the direct connection, whose quota is
	// fresh again. From there the original rotation logic resumes once the
	// direct line burns out.
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).Add(24 * time.Hour)
			t := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			c := st.Config()
			if !c.ProxyPoolEnabled || !c.ProxyPoolAutoRotate {
				continue // nothing rotates automatically, so nothing to reset
			}
			n := pool.ResetDaily()
			_ = pool.Use("") // the local line's quota just refreshed
			logger.Infof("免费额度每日刷新：清空轮换黑名单（%d 个 IP），切回本机直连", n)
			lastRotate.Store(0) // the new egress owes nothing, cooldown included
			ln.ClearThrottle()
			ln.ProbeRound(ctx, true)
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

// shortDetail trims a transport error for a one-line log message.
func shortDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return s
}
