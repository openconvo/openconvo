package http

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// carrierGrade is 100.64.0.0/10. Go does not count it as private, but it is
// what Tailscale and similar overlays hand out, and those carry their own
// encryption. Treating that as an exposed archive would teach operators to
// ignore the one warning that matters.
var carrierGrade = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// exposureMonitor records whether the archive has answered a client outside
// this machine's own networks over plain HTTP.
//
// OpenConvo cannot know at startup whether a reverse proxy sits in front of
// it: the bind address says nothing, because under Docker Compose the
// container always listens on every interface of its own namespace. The only
// honest evidence is a request that actually arrived from off-network with no
// TLS anywhere in its path — which is exactly the deployment mistake no
// documentation can catch, an archive of private conversations published in
// the clear.
//
// A caller can forge X-Forwarded-For and raise this itself. That costs it a
// banner in an administrator's dashboard and nothing else, which is a fair
// price for catching the proxy that forwards without X-Forwarded-Proto.
type exposureMonitor struct {
	logger   *slog.Logger
	insecure atomic.Bool
	warnOnce sync.Once
}

func newExposureMonitor(logger *slog.Logger) *exposureMonitor {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &exposureMonitor{logger: logger}
}

// middleware observes every request before it is served.
func (m *exposureMonitor) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.observe(r)
		next.ServeHTTP(w, r)
	})
}

func (m *exposureMonitor) observe(r *http.Request) {
	if m.insecure.Load() || secureRequest(r) || !publicClient(reportedClient(r)) {
		return
	}
	m.insecure.Store(true)
	m.warnOnce.Do(func() {
		m.logger.Warn("the archive answered a request from outside this network over plain HTTP;"+
			" terminate TLS in front of OpenConvo before exposing it",
			"component", "http")
	})
}

// reportedClient is evidence for the advisory exposure warning only. Unlike
// authentication throttling, accepting a forged forwarding address here can
// only raise a conservative warning, never grant access or weaken a control.
func reportedClient(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		entries := strings.Split(forwarded, ",")
		if closest := strings.TrimSpace(entries[len(entries)-1]); closest != "" {
			return closest
		}
	}
	return remoteHost(r.RemoteAddr)
}

// insecurePublicAccess reports whether that has happened since startup.
func (m *exposureMonitor) insecurePublicAccess() bool {
	return m.insecure.Load()
}

// publicClient reports whether an address belongs to the wider internet
// rather than this machine, a local network, or an encrypted overlay. An
// address that will not parse is not evidence of anything, so it stays quiet.
func publicClient(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	return !carrierGrade.Contains(ip)
}
