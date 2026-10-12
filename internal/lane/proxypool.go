package lane

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultProxySources are public, plain-text free-proxy listings. Each line is
// "ip:port" or "scheme://ip:port", optionally with a trailing "#cc" country
// annotation; a source whose URL mentions socks5 supplies the scheme for its
// scheme-less lines. They are fetched directly (never through the pool itself
// — no bootstrapping loop).
//
// Order matters only in that ProxyClean sits first: it is a maintained,
// continuously-revalidated SOCKS5 list (connectivity verified from mainland
// China every hour), so its hit rate is far above the raw scrapes below.
var DefaultProxySources = []string{
	// 持续保活 + 国内连通性验证的清洗池
	"https://raw.githubusercontent.com/HankNovic/ProxyClean/main/SOCKS5.txt",
	// scdn.io 公益池（CDN 服务商维护）。它的 API 每轮随机返回 20 个服务端
	// 已校验的节点，用 #repeat 多轮累积；协议必须分开请求，因为 all 会混入
	// SOCKS 类型，混在一起按 HTTP 测会误判。
	"https://proxy.scdn.io/api/get_proxy.php?protocol=http&count=20#repeat=5",
	"https://proxy.scdn.io/api/get_proxy.php?protocol=socks5&count=20#repeat=3",
	"https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/http.txt",
	"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt",
	"https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/http/data.txt",
	"https://raw.githubusercontent.com/ErcinDedeoglu/proxies/main/proxies/http.txt",
	"https://raw.githubusercontent.com/sunny9577/proxy-scraper/master/proxies.txt",
	"https://raw.githubusercontent.com/mmpx12/proxy-list/master/http.txt",
	"https://raw.githubusercontent.com/ShiftyTR/Proxy-List/master/http.txt",
	"https://raw.githubusercontent.com/roosterkid/openproxylist/main/HTTPS_RAW.txt",
	"https://raw.githubusercontent.com/prxchk/proxy-list/main/http.txt",
	"https://raw.githubusercontent.com/zloi-user/hideip.me/main/http.txt",
	"https://raw.githubusercontent.com/hookzof/socks5_list/master/proxy.txt",
}

// ProxyEntry is one candidate egress.
type ProxyEntry struct {
	URL       string `json:"url"`  // canonical "http://1.2.3.4:8080"
	Host      string `json:"host"` // "1.2.3.4:8080"
	Scheme    string `json:"scheme"`
	LatencyMs int64  `json:"latencyMs"`
	OK        bool   `json:"ok"` // reachable + answered the upstream probe
	Status    int    `json:"status,omitempty"`
	// Exhausted marks an egress whose IP already burned its daily free quota.
	// Such a proxy is reachable but useless for this lane.
	Exhausted bool   `json:"exhausted"`
	Country   string `json:"country,omitempty"`
	// Blacklisted is a display-only overlay: this egress's IP was rotated away
	// from earlier today and is retired until the midnight reset. It is
	// derived from the pool's blacklist on read and never persisted.
	Blacklisted bool `json:"blacklisted,omitempty"`
	// EgressIP is the public address this proxy actually exits from, measured
	// through it. Two listings behind one IP share one daily quota, so this
	// keeps rotation from wasting a hop on a "different" proxy that is not.
	EgressIP string `json:"egressIp,omitempty"`
	Detail   string `json:"detail,omitempty"`
	LastTest int64  `json:"lastTest"`
	Failures int    `json:"failures,omitempty"`
}

// BlacklistEntry records one egress IP retired for the rest of the day. The
// free lane's quota is accounted per IP and refreshes at midnight, so the
// entry carries the day it belongs to: a blacklist read back from disk on a
// later day is stale and dropped.
type BlacklistEntry struct {
	IP     string `json:"ip"`
	Reason string `json:"reason"`
	At     int64  `json:"at"` // epoch ms when it was retired
}

// ProxyPool keeps a tested, latency-sorted set of free proxies and remembers
// which egress the lane is currently using.
type ProxyPool struct {
	mu         sync.RWMutex
	entries    []*ProxyEntry
	byURL      map[string]*ProxyEntry
	current    string // "" = direct (local connection)
	refreshing bool
	lastAt     int64
	lastErr    string
	poolStats  poolStats
	// storagePath persists the tested set so a restart does not throw away a
	// minute of probing (empty = in-memory only).
	storagePath string

	// blacklist holds egress IPs rotated away from today. Rotation never
	// picks them again — a second port behind the same address shares the
	// same daily quota — until the midnight reset clears the map.
	blacklist    map[string]BlacklistEntry
	blacklistDay string // local "2006-01-02" the blacklist belongs to

	// localIPLookup resolves this machine's own public egress address, used to
	// blacklist the direct line when its quota burns out. Overridable in tests.
	localIPLookup func(ctx context.Context) string

	// configuration (guarded by mu)
	sources        []string
	maxTest        int
	concurrency    int
	timeout        time.Duration
	verifyUpstream bool

	log func(format string, args ...any)
}

type poolStats struct {
	Fetched   int `json:"fetched"`
	Tested    int `json:"tested"`
	Passed    int `json:"passed"`
	Exhausted int `json:"exhausted"`
	Rotations int `json:"rotations"`
}

// candidateRe matches "ip:port" and "scheme://ip:port" forms.
var candidateRe = regexp.MustCompile(`^(?:(https?|socks5h?|socks4)://)?([0-9a-zA-Z.\-]+):([0-9]{2,5})$`)

// NewProxyPool builds a pool in direct mode with conservative defaults.
// Free proxies are mostly dead, so the sample size has to be large for a
// rotation to have anywhere to go: ~1500 candidates at 120-way concurrency
// finishes in about a minute on a normal uplink.
func NewProxyPool(log func(string, ...any)) *ProxyPool {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &ProxyPool{
		byURL:          map[string]*ProxyEntry{},
		sources:        append([]string{}, DefaultProxySources...),
		maxTest:        1500,
		concurrency:    120,
		timeout:        6 * time.Second,
		verifyUpstream: true,
		blacklist:      map[string]BlacklistEntry{},
		blacklistDay:   todayDay(),
		log:            log,
	}
}

// Configure applies the dashboard settings. Zero values keep the current one.
// Passing an empty (but non-nil) source list restores the built-in defaults,
// which is what clearing the textarea in the dashboard should mean.
func (p *ProxyPool) Configure(sources []string, maxTest, concurrency, timeoutSec int, verifyUpstream bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sources != nil {
		clean := []string{}
		for _, s := range sources {
			if t := strings.TrimSpace(s); t != "" {
				clean = append(clean, t)
			}
		}
		if len(clean) == 0 {
			p.sources = append([]string{}, DefaultProxySources...)
		} else {
			p.sources = clean
		}
	}
	if maxTest > 0 {
		p.maxTest = maxTest
	}
	if concurrency > 0 {
		p.concurrency = concurrency
	}
	if timeoutSec > 0 {
		p.timeout = time.Duration(timeoutSec) * time.Second
	}
	p.verifyUpstream = verifyUpstream
}

// Config returns the current pool parameters (for the dashboard).
func (p *ProxyPool) Config() (sources []string, maxTest, concurrency, timeoutSec int, verifyUpstream bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string{}, p.sources...), p.maxTest, p.concurrency, int(p.timeout / time.Second), p.verifyUpstream
}

// Snapshot is the dashboard view: the current egress plus every tested entry.
func (p *ProxyPool) Snapshot() (current string, entries []ProxyEntry, refreshing bool, stats poolStats, lastAt int64, lastErr string) {
	p.rollDay()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]ProxyEntry, 0, len(p.entries))
	for _, e := range p.entries {
		cp := *e
		if cp.EgressIP != "" {
			if _, bl := p.blacklist[cp.EgressIP]; bl {
				cp.Blacklisted = true // derived overlay, recomputed on every read
			}
		}
		out = append(out, cp)
	}
	// usable first, shortest latency first
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OK != out[j].OK {
			return out[i].OK
		}
		if out[i].Exhausted != out[j].Exhausted {
			return !out[i].Exhausted
		}
		if out[i].Blacklisted != out[j].Blacklisted {
			return !out[i].Blacklisted
		}
		return out[i].LatencyMs < out[j].LatencyMs
	})
	return p.current, out, p.refreshing, p.poolStats, p.lastAt, p.lastErr
}

// Current returns the proxy URL in use ("" = direct).
func (p *ProxyPool) Current() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.current
}

// Use switches the active egress. url == "" restores the direct connection.
func (p *ProxyPool) Use(url string) error {
	p.mu.Lock()
	if url != "" {
		if _, ok := p.byURL[url]; !ok {
			p.mu.Unlock()
			return fmt.Errorf("代理不在池中: %s", url)
		}
	}
	p.current = url
	p.mu.Unlock()
	p.apply()
	return nil
}

// apply pushes the current selection into the shared transport.
func (p *ProxyPool) apply() {
	cur := p.Current()
	if cur == "" {
		SetActiveProxy(nil)
		p.log("出口切换 → 本机直连")
		return
	}
	u, err := neturl.Parse(cur)
	if err != nil {
		SetActiveProxy(nil)
		return
	}
	SetActiveProxy(u)
	p.log("出口切换 → %s", cur)
}

// MarkExhausted flags the current egress as quota-burned so it is not picked
// again this session.
func (p *ProxyPool) MarkExhausted(url string) {
	if url == "" {
		return
	}
	p.mu.Lock()
	if e, ok := p.byURL[url]; ok {
		e.Exhausted = true
		e.Detail = "本 IP 额度已耗尽"
	}
	p.mu.Unlock()
}

// --- rotation blacklist ------------------------------------------------------

// todayDay is the calendar day the blacklist belongs to. The upstream free
// quota refreshes at 00:00 UTC — not local midnight — so the day tag must be
// computed in UTC for an entry to expire exactly when the quota does.
func todayDay() string {
	return time.Now().UTC().Format("2006-01-02")
}

// rollDayLocked drops a blacklist that belongs to a previous day. Called on
// every read path so a process that was not running at midnight (or was
// suspended across it) still starts the new day with a clean slate.
func (p *ProxyPool) rollDayLocked() {
	day := todayDay()
	if p.blacklistDay == day {
		return
	}
	p.blacklistDay = day
	p.blacklist = map[string]BlacklistEntry{}
}

// rollDay is the lock-taking form for read paths.
func (p *ProxyPool) rollDay() {
	p.mu.Lock()
	p.rollDayLocked()
	p.mu.Unlock()
}

// blacklistAddLocked retires an egress IP for the rest of the day.
func (p *ProxyPool) blacklistAddLocked(ip, reason string) {
	if ip == "" {
		return
	}
	if p.blacklist == nil {
		p.blacklist = map[string]BlacklistEntry{}
	}
	p.blacklist[ip] = BlacklistEntry{IP: ip, Reason: reason, At: time.Now().UnixMilli()}
}

// blacklistedLocked reports whether an egress IP is retired for today. An
// unknown ("") egress IP can never be matched, so it is never blacklisted.
func (p *ProxyPool) blacklistedLocked(ip string) bool {
	if ip == "" || len(p.blacklist) == 0 {
		return false
	}
	_, ok := p.blacklist[ip]
	return ok
}

// Blacklist lists the egress IPs retired for the rest of the day, newest
// first. Rotation skips every one of them until the midnight reset.
func (p *ProxyPool) Blacklist() []BlacklistEntry {
	p.rollDay()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]BlacklistEntry, 0, len(p.blacklist))
	for _, b := range p.blacklist {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

// ResetDaily clears the day's blacklist. The caller (the midnight job) then
// returns the lane to the direct connection, whose quota just refreshed.
// Returns how many IPs were dropped.
func (p *ProxyPool) ResetDaily() int {
	p.mu.Lock()
	n := len(p.blacklist)
	p.blacklist = map[string]BlacklistEntry{}
	p.blacklistDay = todayDay()
	p.mu.Unlock()
	if n > 0 {
		p.save() // persist the cleared state so a restart cannot resurrect it
	}
	return n
}

// localEgressIP resolves this machine's public address on a direct connection,
// so the local line itself can be blacklisted when its quota burns out.
// Non-fatal by design: the direct IP is never a pool candidate anyway, so a
// failed lookup only costs the bookkeeping.
func (p *ProxyPool) localEgressIP(ctx context.Context) string {
	if p.localIPLookup != nil {
		return p.localIPLookup(ctx)
	}
	client := &http.Client{Timeout: 6 * time.Second}
	defer client.CloseIdleConnections()
	return p.lookupEgressIP(ctx, client)
}

// Usable lists reachable, non-exhausted proxies sorted by latency. Egress IPs
// on today's blacklist are excluded: they are reachable but retired, and
// rotating onto one would only buy another quota refusal.
func (p *ProxyPool) Usable() []*ProxyEntry {
	p.rollDay()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []*ProxyEntry{}
	for _, e := range p.entries {
		if e.OK && !e.Exhausted && !p.blacklistedLocked(e.EgressIP) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LatencyMs < out[j].LatencyMs })
	return out
}

// UsableCount reports how many rotation candidates are ready.
func (p *ProxyPool) UsableCount() int {
	p.rollDay()
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, e := range p.entries {
		if e.OK && !e.Exhausted && !p.blacklistedLocked(e.EgressIP) {
			n++
		}
	}
	return n
}

// Rotate moves to the lowest-latency usable proxy that is not the current one.
// markPrev records the outgoing egress as quota-burned — correct when rotation
// was triggered by a 429, wrong when the user just wants a different exit (a
// manual switch must not silently retire nodes). On a quota rotation the
// outgoing exit IP also joins the day's blacklist, so no later rotation today
// can land on it again — not even on a second port behind the same address,
// since the free lane's quota is per IP and only refreshes at midnight. It
// refreshes the pool when it runs dry. Returns the new proxy URL ("" = nothing
// available).
func (p *ProxyPool) Rotate(ctx context.Context, markPrev bool) (string, error) {
	p.mu.Lock()
	p.rollDayLocked()
	prev := p.current
	curIP := ""
	if prev != "" {
		if e, ok := p.byURL[prev]; ok {
			curIP = e.EgressIP
			if markPrev {
				e.Exhausted = true
				e.Detail = "本 IP 额度已耗尽"
			}
		}
	}
	// A dead proxy is not a quota problem: its IP may host other live ports and
	// a later refresh re-tests them, so only a quota rotation retires the exit.
	retireDirect := markPrev && prev == "" // the local line itself just burned out
	if markPrev && curIP != "" {
		p.blacklistAddLocked(curIP, "额度耗尽")
	}
	best, sameExit := p.pickLocked(prev, curIP)
	if best == nil {
		best = sameExit
	}
	blCount := len(p.blacklist)
	p.mu.Unlock()

	if best == nil {
		// pool dry — refill in place, then pick again
		p.log("代理池已无可用节点，正在补充…")
		if _, _, err := p.Refresh(ctx); err != nil {
			p.lastErrSet(err)
		}
		p.mu.Lock()
		best, sameExit = p.pickLocked(prev, curIP)
		if best == nil {
			best = sameExit
		}
		blCount = len(p.blacklist)
		p.mu.Unlock()
	}
	if best == nil {
		if blCount > 0 {
			return "", fmt.Errorf("今日可用节点已全部轮换过（黑名单 %d 个 IP），UTC 0 点额度刷新后自动恢复", blCount)
		}
		return "", fmt.Errorf("代理池中没有可用节点")
	}
	// Commit the switch before the (possibly slow) local-IP lookup so the
	// handover itself never waits on the echo service.
	p.mu.Lock()
	p.current = best.URL
	p.poolStats.Rotations++
	p.mu.Unlock()
	p.apply()
	if retireDirect {
		if ip := p.localEgressIP(ctx); ip != "" {
			p.mu.Lock()
			p.blacklistAddLocked(ip, "额度耗尽")
			p.mu.Unlock()
		}
	}
	return best.URL, nil
}

// pickLocked chooses the rotation target: the lowest-latency usable entry that
// is neither the current URL nor on an exit retired for today. A second port
// on the current egress (sameExit) is only a last resort — and never when that
// egress is blacklisted, because quota is per IP and a same-exit hop would
// just burn another refusal.
func (p *ProxyPool) pickLocked(prevURL, curIP string) (best, sameExit *ProxyEntry) {
	for _, e := range p.entries {
		if !e.OK || e.Exhausted || e.URL == prevURL {
			continue
		}
		if p.blacklistedLocked(e.EgressIP) {
			continue // retired for the rest of the day
		}
		if curIP != "" && e.EgressIP == curIP {
			if sameExit == nil || e.LatencyMs < sameExit.LatencyMs {
				sameExit = e
			}
			continue
		}
		if best == nil || e.LatencyMs < best.LatencyMs {
			best = e
		}
	}
	return best, sameExit
}

func (p *ProxyPool) lastErrSet(err error) {
	p.mu.Lock()
	p.lastErr = err.Error()
	p.mu.Unlock()
}

// Refresh fetches the sources and re-tests candidates. Safe to call
// concurrently: a second call while one is running returns immediately.
func (p *ProxyPool) Refresh(ctx context.Context) (tested, passed int, err error) {
	p.rollDay()
	p.mu.Lock()
	if p.refreshing {
		p.mu.Unlock()
		return 0, 0, fmt.Errorf("正在刷新中")
	}
	p.refreshing = true
	sources := append([]string{}, p.sources...)
	maxTest := p.maxTest
	conc := p.concurrency
	verify := p.verifyUpstream
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.refreshing = false
		p.lastAt = time.Now().UnixMilli()
		p.mu.Unlock()
		p.save()
	}()

	fetchStart := time.Now()
	candidates, fetchErr := fetchProxyList(ctx, sources, p.log)
	fetchDur := time.Since(fetchStart)
	if len(candidates) == 0 {
		if fetchErr != nil {
			p.lastErrSet(fetchErr)
			return 0, 0, fetchErr
		}
		e := fmt.Errorf("代理源未返回任何候选")
		p.lastErrSet(e)
		return 0, 0, e
	}

	// Carry the previous survivors to the front of the candidate list. Free
	// proxy churn is brutal — a node that worked a minute ago is often not in
	// this fetch at all — so a refresh must re-verify what is already known
	// good instead of gambling the whole pool on a fresh random sample.
	carried := 0
	p.mu.RLock()
	for _, e := range p.byURL {
		// a blacklisted exit is retired for the day: re-testing it only wastes
		// a probe slot on an IP that cannot serve until midnight anyway
		if e.OK && !e.Exhausted && !p.blacklistedLocked(e.EgressIP) {
			candidates = append([]proxyCandidate{{Raw: e.URL, Scheme: e.Scheme, Country: e.Country}}, candidates...)
			carried++
		}
	}
	p.mu.RUnlock()
	if carried > 0 {
		p.log("保留上一轮 %d 个可用节点一并复测", carried)
	}

	// shuffle so a truncated run samples across sources instead of taking the
	// head of one list
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	if maxTest > 0 && len(candidates) > maxTest {
		candidates = candidates[:maxTest]
	}

	// note: the previous set stays in place until the new one is ready, so the
	// dashboard keeps showing nodes during a refresh instead of blanking out
	p.mu.Lock()
	p.poolStats.Fetched = len(candidates)
	p.mu.Unlock()

	// concurrent probe
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]*ProxyEntry, 0, len(candidates))
	var okCount, exhaustedCount int

	testStart := time.Now()
	seenURL := map[string]bool{}
	for _, cand := range candidates {
		u, host, scheme, ok := normalizeProxy(cand.Raw, cand.Scheme)
		if !ok || seenURL[u] {
			continue
		}
		seenURL[u] = true
		wg.Add(1)
		go func(u, host, scheme, country string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e := p.testOne(ctx, u, host, scheme, country, verify)
			mu.Lock()
			results = append(results, e)
			if e.OK && !e.Exhausted {
				okCount++
			}
			if e.Exhausted {
				exhaustedCount++
			}
			mu.Unlock()
		}(u, host, scheme, cand.Country)
	}
	wg.Wait()

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].OK != results[j].OK {
			return results[i].OK
		}
		return results[i].LatencyMs < results[j].LatencyMs
	})

	p.mu.Lock()
	p.byURL = map[string]*ProxyEntry{}
	p.entries = results
	for _, e := range results {
		p.byURL[e.URL] = e
	}
	p.poolStats.Tested = len(results)
	p.poolStats.Passed = okCount
	p.poolStats.Exhausted = exhaustedCount
	// keep the current selection if it survived the refresh
	if p.current != "" {
		if e, ok := p.byURL[p.current]; !ok || !e.OK || e.Exhausted {
			p.current = ""
		}
	}
	p.mu.Unlock()
	p.apply()

	p.log("代理池刷新完成：测试 %d 个，可用 %d 个（其中 %d 个额度已耗尽）｜抓取 %s + 测速 %s = 共 %s",
		len(results), okCount, exhaustedCount,
		fetchDur.Round(time.Millisecond), time.Since(testStart).Round(time.Millisecond),
		time.Since(fetchStart).Round(time.Millisecond))
	return len(results), okCount, nil
}

// connectivityTarget is the lightweight reachability check used when upstream
// verification is off: small, cacheable, served worldwide.
const connectivityTarget = "http://www.msftconnecttest.com/connecttest.txt"

// testOne measures one candidate: reachability, latency and — when upstream
// verification is on — whether the lane is actually usable through it.
//
// A 200 from the upstream listing means the proxy works. A 429 means the proxy
// works too, but its IP already burned the daily quota: recorded as reachable
// yet exhausted, so the pool never wastes a rotation on it.
func (p *ProxyPool) testOne(ctx context.Context, url, host, scheme, annotCountry string, verify bool) *ProxyEntry {
	// a listing annotation is a hint, not a measurement: keep it only as a
	// placeholder that a real egress lookup can later replace
	e := &ProxyEntry{URL: url, Host: host, Scheme: scheme, Country: annotCountry, LastTest: time.Now().UnixMilli()}
	u, err := neturl.Parse(url)
	if err != nil {
		e.Detail = "地址不合法"
		return e
	}
	// Configure() writes p.timeout under p.mu, so read it under the lock too:
	// a settings save overlapping a refresh is otherwise a data race.
	p.mu.RLock()
	timeout := p.timeout
	p.mu.RUnlock()
	transport := &http.Transport{
		Proxy:               http.ProxyURL(u),
		DisableKeepAlives:   true,
		MaxIdleConns:        1,
		IdleConnTimeout:     5 * time.Second,
		TLSHandshakeTimeout: timeout,
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	defer client.CloseIdleConnections()

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := UpstreamBase + "/zen/v1/models"
	if !verify {
		target = connectivityTarget
	}

	t0 := time.Now()
	req, err := http.NewRequestWithContext(tctx, http.MethodGet, target, nil)
	if err != nil {
		e.Detail = err.Error()
		return e
	}
	if verify {
		for k, v := range GatewayHeaders(SessionForConversation("pool-test"), MintRequestId(0), false, "application/json") {
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	latency := time.Since(t0).Milliseconds()
	if err != nil {
		e.LatencyMs = latency
		e.Detail = classifyProxyError(err)
		return e
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	e.LatencyMs = latency
	e.Status = resp.StatusCode
	if !verify {
		// connectivity-only run: any HTTP answer proves the tunnel works
		if resp.StatusCode < 400 {
			e.OK = true
			e.Detail = fmt.Sprintf("连通（HTTP %d，未验证上游）", resp.StatusCode)
		} else {
			e.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
			e.Failures = 1
		}
		return e
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		e.OK = true
		e.Detail = "可用"
	case resp.StatusCode == http.StatusTooManyRequests:
		// reachable, but this IP is out of quota
		e.OK = true
		e.Exhausted = true
		e.Detail = "该 IP 额度已耗尽"
	case resp.StatusCode == http.StatusForbidden:
		e.OK = true
		e.Exhausted = true
		e.Detail = "该 IP 被地区/风控拦截"
	case resp.StatusCode >= 500:
		e.Detail = fmt.Sprintf("上游 %d", resp.StatusCode)
		e.Failures = 1
	default:
		if resp.StatusCode < 400 {
			e.OK = true
			e.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			e.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
			e.Failures = 1
		}
	}
	if e.OK && !e.Exhausted {
		e.EgressIP = p.lookupEgressIP(ctx, client)
	}
	return e
}

// classifyProxyError turns a transport failure into something a human can act
// on. The TLS case matters most: a proxy that cannot pass certificate
// validation is intercepting the connection and must never be used.
func classifyProxyError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "failed to verify certificate"),
		strings.Contains(msg, "x509:"),
		strings.Contains(msg, "certificate signed by unknown authority"):
		return "TLS 证书校验失败（疑似中间人窃听，已拒绝）"
	case strings.Contains(msg, "socks connect"),
		strings.Contains(msg, "socks5"):
		return "SOCKS 握手失败：" + shortErr(err)
	case strings.Contains(msg, "proxyconnect"):
		return "代理连接失败：" + shortErr(err)
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "超时"
	case strings.Contains(msg, "connection refused"):
		return "连接被拒绝"
	default:
		return shortErr(err)
	}
}

// egressEchoURL is a tiny public IP echo service used to learn which address a
// proxy actually presents.
const egressEchoURL = "https://api.ipify.org?format=json"

// lookupEgressIP asks the echo service what address the proxy exits from.
// Non-fatal by design: the proxy is still usable when this fails, we just
// cannot advertise which IP it uses.
func (p *ProxyPool) lookupEgressIP(ctx context.Context, client *http.Client) string {
	tctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, http.MethodGet, egressEchoURL, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.IP)
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// normalizeProxy turns a raw listing line into a canonical proxy URL.
// defScheme applies when the line carries no explicit scheme (a socks5 list
// yields scheme-less lines that must not be probed as HTTP proxies).
func normalizeProxy(raw, defScheme string) (url, host, scheme string, ok bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "\"'")
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", "", "", false
	}
	m := candidateRe.FindStringSubmatch(raw)
	if m == nil {
		return "", "", "", false
	}
	scheme, host = m[1], m[2]+":"+m[3]
	if scheme == "" {
		scheme = defScheme
	}
	if scheme == "" {
		scheme = "http"
	}
	if scheme == "socks5h" {
		scheme = "socks5"
	}
	if scheme != "http" && scheme != "https" && scheme != "socks5" && scheme != "socks4" {
		return "", "", "", false
	}
	// skip obviously invalid hosts
	if !validProxyHost(m[2]) {
		return "", "", "", false
	}
	return scheme + "://" + host, host, scheme, true
}

// validProxyHost rejects listing lines whose host part cannot be an address.
// The check has to run on the bare host (m[2]) — testing the composed
// "host:port" would always see a colon and never reject anything.
func validProxyHost(h string) bool {
	h = strings.TrimSpace(h)
	if h == "" || len(h) > 253 {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	// a digits-and-dots string must be a real IPv4 literal, otherwise it is
	// junk ("1.2.3", "999.999.999.999", "0.0.0.0.0")
	if strings.Trim(h, "0123456789.") == "" {
		return false
	}
	// hostname: needs a dot, and no empty/dangling label
	if !strings.Contains(h, ".") {
		return false
	}
	if strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") ||
		strings.HasPrefix(h, "-") || strings.HasSuffix(h, "-") ||
		strings.Contains(h, "..") {
		return false
	}
	return true
}

// proxyFetchUA identifies this service when pulling listings (some CDNs
// throttle anonymous clients).
const proxyFetchUA = "zen-gate-server/1.0 (+https://github.com/sanhe458/zen-gate-server)"

// proxyCandidate is one listing line plus the scheme its source implies and
// any geo annotation the source carried.
type proxyCandidate struct {
	Raw     string
	Scheme  string
	Country string
}

// splitRepeat reads an optional "#repeat=N" trailer off a source URL. Some
// providers (scdn.io among them) expose a verified-but-tiny API endpoint that
// hands back a handful of randomised proxies per call, so one URL has to be
// polled several times to accumulate a useful batch. The fragment is stripped
// before the request — it is never sent to the server.
func splitRepeat(src string) (string, int) {
	const marker = "#repeat="
	if i := strings.LastIndex(src, marker); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(src[i+len(marker):])); err == nil {
			if n < 1 {
				n = 1
			}
			if n > 50 {
				n = 50
			}
			return strings.TrimSpace(src[:i]), n
		}
	}
	return src, 1
}

// looksLikeProxy reports whether a string is a bare "ip:port" address.
func looksLikeProxy(s string) bool {
	m := candidateRe.FindStringSubmatch(strings.TrimSpace(s))
	return m != nil
}

// extractProxyStrings walks an arbitrary JSON value and collects every proxy
// address it can find — both bare "ip:port" strings (scdn.io's shape) and
// {"ip":..,"port":..} object pairs (older free-list APIs).
func extractProxyStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		if looksLikeProxy(t) {
			*out = append(*out, strings.TrimSpace(t))
		}
	case []any:
		for _, x := range t {
			extractProxyStrings(x, out)
		}
	case map[string]any:
		if ip, ok := t["ip"].(string); ok && ip != "" {
			port := ""
			switch pv := t["port"].(type) {
			case float64:
				port = fmt.Sprintf("%.0f", pv)
			case string:
				port = pv
			}
			if port != "" {
				*out = append(*out, ip+":"+port)
				return
			}
		}
		for _, x := range t {
			extractProxyStrings(x, out)
		}
	}
}

// parseProxyBody extracts candidate addresses from a source response, which is
// either a whole JSON document (API endpoints) or one proxy per line.
func parseProxyBody(body []byte) []string {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var v any
		if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
			var out []string
			extractProxyStrings(v, &out)
			if len(out) > 0 {
				return out
			}
		}
	}
	out := []string{}
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// splitAnnotation peels a trailing "#country" annotation off a listing line
// (ProxyClean-style listings carry one) and returns it separately.
func splitAnnotation(line string) (addr, note string) {
	if i := strings.Index(line, "#"); i >= 0 {
		return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
	}
	return strings.TrimSpace(line), ""
}

// fetchProxyList pulls every source concurrently and returns the deduplicated
// candidates. Serial fetching was a real problem: a handful of slow or blocked
// sources used to stall a refresh for minutes, so each source gets its own
// goroutine and a short timeout, and partial success is enough.
func fetchProxyList(ctx context.Context, sources []string, log func(string, ...any)) ([]proxyCandidate, error) {
	type result struct {
		src   string
		cands []proxyCandidate
		err   error
	}
	jobs := []string{}
	for _, src := range sources {
		if s := strings.TrimSpace(src); s != "" {
			jobs = append(jobs, s)
		}
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("未配置代理源")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	ch := make(chan result, len(jobs))
	var wg sync.WaitGroup
	for _, src := range jobs {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			cands, err := fetchOneSource(ctx, client, src)
			ch <- result{src: src, cands: cands, err: err}
		}(src)
	}
	wg.Wait()
	close(ch)

	seen := map[string]bool{}
	out := []proxyCandidate{}
	good := 0
	var lastErr error
	for r := range ch {
		if r.err != nil {
			lastErr = r.err
			log("代理源拉取失败 %s: %v", r.src, r.err)
			continue
		}
		good++
		for _, c := range r.cands {
			if seen[c.Raw] {
				continue
			}
			seen[c.Raw] = true
			out = append(out, c)
		}
	}
	if good == 0 && lastErr != nil {
		return out, fmt.Errorf("全部代理源均拉取失败: %v", lastErr)
	}
	return out, nil
}

// fetchOneSource downloads and parses a single listing. A "#repeat=N" trailer
// makes it poll the same endpoint N times and merge the results, which is what
// API-style sources need: they hand back a random handful per call.
func fetchOneSource(ctx context.Context, client *http.Client, src string) ([]proxyCandidate, error) {
	url, repeat := splitRepeat(src)
	defScheme := schemeForSource(url)

	// file:// sources are how a listing that is not machine-fetchable (a site
	// behind a Cloudflare challenge, an authenticated panel, a hand-picked
	// list) still gets used: fetch it however you can, drop it on disk, point
	// a source at it.
	if strings.HasPrefix(url, "file://") {
		return readLocalListing(url, defScheme)
	}

	seen := map[string]bool{}
	out := []proxyCandidate{}
	var lastErr error
	okCalls := 0
	for i := 0; i < repeat; i++ {
		body, err := httpGetBody(ctx, client, url)
		if err != nil {
			lastErr = err
			if i == 0 {
				return nil, err // the endpoint itself is unreachable
			}
			break // later rounds failing is fine, keep what we have
		}
		okCalls++
		for _, line := range parseProxyBody(body) {
			addr, note := splitAnnotation(line)
			if addr == "" || seen[addr] {
				continue
			}
			seen[addr] = true
			out = append(out, proxyCandidate{Raw: addr, Scheme: defScheme, Country: note})
		}
		if i+1 < repeat {
			select { // be a good citizen between polls
			case <-ctx.Done():
				return out, nil
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
	if okCalls == 0 && lastErr != nil {
		return out, lastErr
	}
	return out, nil
}

// schemeForSource infers a listing's proxy protocol from its URL. Only the
// path and query are inspected: looking at the whole URL would read the
// "https://" scheme of the listing itself as the proxy protocol, which would
// silently mislabel every HTTPS-hosted list.
func schemeForSource(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return "http"
	}
	target := strings.ToLower(u.Path + "?" + u.RawQuery)
	switch {
	case strings.Contains(target, "socks5"):
		return "socks5"
	case strings.Contains(target, "socks4"):
		return "socks4"
	case strings.Contains(target, "protocol=https"), strings.Contains(target, "type=https"):
		return "https"
	}
	return "http"
}

func httpGetBody(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("user-agent", proxyFetchUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// extractJSONStrings salvages "host:port" strings from a JSON line.
func extractJSONStrings(line string) []string {
	var arr []map[string]any
	if err := json.Unmarshal([]byte(line), &arr); err == nil {
		out := []string{}
		for _, m := range arr {
			ip, _ := m["ip"].(string)
			port := ""
			switch v := m["port"].(type) {
			case float64:
				port = fmt.Sprintf("%.0f", v)
			case string:
				port = v
			}
			if ip != "" && port != "" {
				out = append(out, ip+":"+port)
			}
		}
		return out
	}
	var strs []string
	if err := json.Unmarshal([]byte(line), &strs); err == nil {
		return strs
	}
	return nil
}

// --- persistence --------------------------------------------------------------

// SetStorage points the pool at a JSON file used to carry the tested set
// across restarts. Call Load right after.
func (p *ProxyPool) SetStorage(path string) {
	p.mu.Lock()
	p.storagePath = path
	p.mu.Unlock()
}

type poolFile struct {
	SavedAt      int64            `json:"savedAt"`
	BlacklistDay string           `json:"blacklistDay,omitempty"`
	Blacklist    []BlacklistEntry `json:"blacklist,omitempty"`
	Stats        poolStats        `json:"stats"`
	Entries      []*ProxyEntry    `json:"entries"`
}

// Load restores the last saved set. Entries are kept as-is; the next Refresh
// re-verifies every survivor (they are carried to the front of the candidate
// list), so a stale node never survives long. The blacklist is day-scoped: a
// snapshot written on an earlier day carries a blacklist that expired at
// midnight, so it is dropped unless it belongs to today.
func (p *ProxyPool) Load() error {
	p.mu.RLock()
	path := p.storagePath
	p.mu.RUnlock()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var pf poolFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = nil
	p.byURL = map[string]*ProxyEntry{}
	for _, e := range pf.Entries {
		if e == nil || e.URL == "" {
			continue
		}
		e.Blacklisted = false // derived overlay: recomputed from the blacklist
		p.entries = append(p.entries, e)
		p.byURL[e.URL] = e
	}
	p.poolStats = pf.Stats
	p.lastAt = pf.SavedAt
	p.blacklist = map[string]BlacklistEntry{}
	p.blacklistDay = todayDay()
	if pf.BlacklistDay == p.blacklistDay {
		for _, b := range pf.Blacklist {
			if b.IP != "" {
				p.blacklist[b.IP] = b
			}
		}
	}
	return nil
}

// save writes the tested set; only worthwhile entries are kept (reachable
// ones, so exhausted/dead nodes do not accumulate in the file). The day's
// blacklist rides along so a restart does not forget which exits are retired.
func (p *ProxyPool) save() {
	p.mu.RLock()
	path := p.storagePath
	keep := make([]*ProxyEntry, 0, len(p.entries))
	for _, e := range p.entries {
		if e.OK {
			keep = append(keep, e)
		}
	}
	bl := make([]BlacklistEntry, 0, len(p.blacklist))
	for _, b := range p.blacklist {
		bl = append(bl, b)
	}
	sort.Slice(bl, func(i, j int) bool { return bl[i].IP < bl[j].IP })
	pf := poolFile{SavedAt: p.lastAt, BlacklistDay: p.blacklistDay, Blacklist: bl, Stats: p.poolStats, Entries: keep}
	p.mu.RUnlock()
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// readLocalListing loads a proxy list from disk. Accepts "file:///abs/path"
// and "file://relative/path"; the same parser handles it, so JSON exports and
// plain text both work, and a trailing "#cc" annotation is honoured.
func readLocalListing(rawURL, defScheme string) ([]proxyCandidate, error) {
	path := strings.TrimPrefix(rawURL, "file://")
	if path == "" {
		return nil, fmt.Errorf("file:// 源缺少路径")
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	out := []proxyCandidate{}
	seen := map[string]bool{}
	for _, line := range parseProxyBody(data) {
		addr, note := splitAnnotation(line)
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, proxyCandidate{Raw: addr, Scheme: defScheme, Country: note})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("本地文件 %s 中没有可解析的代理", path)
	}
	return out, nil
}

// VerifyCurrent re-tests the active egress, which is how a dead proxy is told
// apart from an upstream blip. Returns true while the tunnel still forwards
// traffic (so the failure came from upstream), false when it is genuinely gone.
//
// Only reachability is judged here, not quota: a 429/403 answer still proves
// the proxy carried the request, and that case is already handled by the quota
// path with its own (different) rotation semantics.
func (p *ProxyPool) VerifyCurrent(ctx context.Context) bool {
	cur := p.Current()
	if cur == "" {
		return true // direct connection: nothing to verify or rotate away from
	}
	u, err := neturl.Parse(cur)
	if err != nil {
		return false
	}
	p.mu.RLock()
	timeout := p.timeout
	p.mu.RUnlock()
	if timeout < 6*time.Second {
		timeout = 6 * time.Second
	}

	transport := &http.Transport{
		Proxy:               http.ProxyURL(u),
		DisableKeepAlives:   true,
		MaxIdleConns:        1,
		IdleConnTimeout:     5 * time.Second,
		TLSHandshakeTimeout: timeout,
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	defer client.CloseIdleConnections()

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, http.MethodGet, UpstreamBase+"/zen/v1/models", nil)
	if err != nil {
		return false
	}
	for k, v := range GatewayHeaders(SessionForConversation("pool-verify"), MintRequestId(0), false, "application/json") {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false // the tunnel itself is broken
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return true
}

// MarkCurrentDead flags the active proxy as no longer forwarding. Unlike
// MarkExhausted — which means "reachable but this IP is out of quota" — this
// means the tunnel is gone, so the entry stops being a rotation candidate until
// a later refresh re-tests it.
func (p *ProxyPool) MarkCurrentDead(detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byURL[p.current]; ok {
		e.OK = false
		e.Detail = detail
		e.LastTest = time.Now().UnixMilli()
		e.Failures++
	}
}
