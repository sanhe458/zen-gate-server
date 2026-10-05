package lane

import (
	neturl "net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// System proxy support, Linux edition. Servers rarely run a desktop
// environment, so the lookup order is:
//  1. GNOME/gsettings (desktop sessions where a proxy tool publishes itself)
//  2. KDE proxiesettings (plasma)
//  3. nothing (nil) — the "env" proxy mode already honours http_proxy et al.
//
// The result is cached for a few seconds so a change made in the desktop's
// proxy UI is picked up without restarting zen-gate-server.

const sysProxyCacheTTL = 3 * time.Second

var (
	sysProxyMu      sync.Mutex
	sysProxyCached  *neturl.URL // nil = system proxy off
	sysProxyCacheAt time.Time
)

// SystemProxyURL returns the system proxy as a URL, or nil when disabled.
func SystemProxyURL() *neturl.URL {
	sysProxyMu.Lock()
	defer sysProxyMu.Unlock()
	if time.Since(sysProxyCacheAt) < sysProxyCacheTTL {
		return sysProxyCached
	}
	sysProxyCacheAt = time.Now()
	sysProxyCached = readSystemProxy()
	return sysProxyCached
}

func readSystemProxy() *neturl.URL {
	if u := gnomeProxy(); u != nil {
		return u
	}
	return kdeProxy()
}

// gnomeProxy reads org.gnome.system.proxy via gsettings. mode "manual" with
// an https host/port is what desktop proxy tools toggle.
func gnomeProxy() *neturl.URL {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return nil
	}
	mode := strings.TrimSpace(gsettings("get", "org.gnome.system.proxy", "mode"))
	if mode != "'manual'" && mode != "manual" {
		return nil
	}
	host := strings.Trim(gsettings("get", "org.gnome.system.proxy.https", "host"), "'\"")
	port := strings.Trim(gsettings("get", "org.gnome.system.proxy.https", "port"), "'\"")
	if host == "" || port == "" || port == "0" {
		host = strings.Trim(gsettings("get", "org.gnome.system.proxy.http", "host"), "'\"")
		port = strings.Trim(gsettings("get", "org.gnome.system.proxy.http", "port"), "'\"")
	}
	if host == "" || port == "" || port == "0" {
		return nil
	}
	u, err := neturl.Parse("http://" + host + ":" + port)
	if err != nil {
		return nil
	}
	return u
}

func gsettings(args ...string) string {
	out, err := exec.Command("gsettings", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// kdeProxy reads the plasma proxy from ~/.config/kdeglobals via kreadconfig5/6.
func kdeProxy() *neturl.URL {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	conf := home + "/.config/kdeglobals"
	if _, err := os.Stat(conf); err != nil {
		return nil
	}
	bin := ""
	for _, c := range []string{"kreadconfig6", "kreadconfig5"} {
		if _, err := exec.LookPath(c); err == nil {
			bin = c
			break
		}
	}
	if bin == "" {
		return nil
	}
	host := runOut(bin, "--file", conf, "--group", "Proxy Settings", "--key", "httpsProxy")
	port := runOut(bin, "--file", conf, "--group", "Proxy Settings", "--key", "httpsPort")
	if host == "" || port == "" {
		return nil
	}
	u, err := neturl.Parse("http://" + host + ":" + port)
	if err != nil {
		return nil
	}
	return u
}

func runOut(bin string, args ...string) string {
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
