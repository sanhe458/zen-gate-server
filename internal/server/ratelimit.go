package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ipRateLimiter meters anonymous traffic with a sliding window per address.
// Callers holding a valid API key bypass it entirely.
//
// A sliding window (rather than a fixed one) is used deliberately: a fixed
// per-minute bucket lets a caller spend the whole budget at the end of one
// minute and again at the start of the next, doubling the effective rate.
type ipRateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	window time.Duration
	lastGC time.Time
}

func newIPRateLimiter(window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{hits: map[string][]time.Time{}, window: window, lastGC: time.Now()}
}

// allow records one hit and reports whether it fits under limit. remaining is
// the budget left afterwards; retryAfter (seconds) is set only when refused.
func (l *ipRateLimiter) allow(ip string, limit int) (ok bool, remaining, retryAfter int) {
	if limit <= 0 {
		return true, 0, 0
	}
	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)

	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		wait := kept[0].Add(l.window).Sub(now)
		l.hits[ip] = kept
		secs := int(wait.Seconds()) + 1
		if secs < 1 {
			secs = 1
		}
		return false, 0, secs
	}
	kept = append(kept, now)
	l.hits[ip] = kept
	return true, limit - len(kept), 0
}

// gcLocked forgets addresses whose window has fully expired, so a busy server
// does not keep one slice per IP forever. Runs at most once a minute.
func (l *ipRateLimiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	cutoff := now.Add(-l.window)
	for ip, ts := range l.hits {
		alive := false
		for _, t := range ts {
			if t.After(cutoff) {
				alive = true
				break
			}
		}
		if !alive {
			delete(l.hits, ip)
		}
	}
}

// tracked reports how many addresses the limiter is currently holding (used by
// tests and the dashboard).
func (l *ipRateLimiter) tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}

// clientIP resolves the caller's address. Forwarding headers are honoured only
// when the direct peer sits on a trusted network (loopback or private range) —
// otherwise a public caller could spoof X-Forwarded-For and mint itself a fresh
// anonymous budget on every request.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !trustedPeer(host) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, part := range strings.Split(xff, ",") {
			if p := strings.TrimSpace(part); p != "" {
				return p
			}
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	return host
}

// trustedPeer reports whether an address may be believed about forwarding
// headers: loopback (a local reverse proxy) or an RFC1918 private range (a
// proxy on the LAN).
func trustedPeer(host string) bool {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// publicBaseURL reconstructs the address the caller actually used, so a client
// arriving over a domain and TLS is shown that same address back rather than
// the listener's loopback form.
func publicBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if v := r.Header.Get("Forwarded"); v != "" { // RFC 7239, lowest precedence
		for _, part := range strings.Split(v, ";") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			val := strings.Trim(kv[1], `"`)
			switch strings.ToLower(kv[0]) {
			case "proto":
				scheme = val
			case "host":
				host = val
			}
		}
	}
	// A reverse proxy publishes the original scheme/host in these headers.
	// They are only believed when the hop itself is trusted.
	if peer, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && trustedPeer(peer) {
		if p := firstToken(r.Header.Get("X-Forwarded-Proto")); p != "" {
			scheme = p
		}
		if h := firstToken(r.Header.Get("X-Forwarded-Host")); h != "" {
			host = h
		}
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return scheme + "://" + host + "/v1"
}

// forwardedPrefix is the path prefix a reverse proxy mounted us under, used so
// the public page prints the right base URL for sub-path deployments.
func forwardedPrefix(r *http.Request) string {
	p := strings.TrimSpace(r.Header.Get("X-Forwarded-Prefix"))
	if p == "" || p == "/" {
		return ""
	}
	return "/" + strings.Trim(p, "/")
}

func firstToken(v string) string {
	if v == "" {
		return ""
	}
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// rateLimitHeaders advertises the anonymous budget on every /v1 response so a
// client can pace itself without waiting for a 429.
func rateLimitHeaders(w http.ResponseWriter, limit, remaining int) {
	if limit <= 0 {
		return
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
}
