package daemon

import (
	"net"
	"strconv"
	"strings"
)

// EnvDaemonControlURL is the environment variable a spawned worker reads to
// learn which daemon control API to fetch its session detail from
// (`<url>/api/daemon/sessions/<id>`).
//
// The daemon SETS this on every worker it spawns. That is the whole point:
// a worker that has to guess the address guesses the well-known default, and
// a daemon running as a named instance on any other port is then invisible to
// its own children — they dial a port nothing is listening on, fail preflight,
// and die before they can report why.
const EnvDaemonControlURL = "DONMAI_DAEMON_URL"

// PublishControlAddr records the host:port the control listener actually bound.
//
// The listener's own address is the only truthful answer whenever the
// configured port is not the bound one: an ephemeral bind (Options.HTTPPort ==
// 0) has no configured port at all, and a second, named daemon instance binds
// a port that is deliberately not the well-known default. Workers are told
// this value, so it must be the real one.
//
// An unspecified bind host (0.0.0.0 / ::) is narrowed to loopback: the control
// API is loopback-only by contract (ResolveControlBind), and a worker cannot
// dial the wildcard address.
func (d *Daemon) PublishControlAddr(addr string) {
	url := controlURLFromAddr(addr)
	if url == "" {
		return
	}
	d.controlURL.Store(&url)
}

// ControlURL returns the base URL of this daemon's local control API — the
// address a worker this daemon spawns must dial.
//
// It prefers the address the listener really bound (PublishControlAddr) and
// falls back to the configured address. The fallback goes through the same
// ResolveControlBind the listener itself uses (server.go), because the two must
// not disagree about what an address IS: that function accepts a host carrying
// its own port ("localhost:7735") and a bracketed IPv6 literal ("[::1]"), and
// refuses any non-loopback bind. Hand-joining host and port instead would render
// the first as "[localhost:7735]:7735", answer "" for the second — silently
// turning this fix back into a no-op for exactly the named instance it was
// written for — and confidently name an address the listener would have refused.
//
// It returns "" when the address is not knowable (no listener yet and no
// resolvable configured port). An empty answer leaves the worker's own
// resolution and its provenance label untouched; a confidently wrong one would
// point it at a port this daemon is not serving.
func (d *Daemon) ControlURL() string {
	if published := d.controlURL.Load(); published != nil && *published != "" {
		return *published
	}
	host, port, err := ResolveControlBind(d.opts.HTTPHost, d.opts.HTTPPort, d.opts.HTTPPort != 0)
	if err != nil || port <= 0 {
		return ""
	}
	return controlURLFromAddr(net.JoinHostPort(host, strconv.Itoa(port)))
}

// controlURLFromAddr renders a listener address as the http:// base URL a
// worker dials. Empty for an address it cannot make sense of — callers treat
// that as "unknown", never as a default.
func controlURLFromAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if port == "" || port == "0" {
		return ""
	}
	if parsed := net.ParseIP(host); parsed != nil && parsed.IsUnspecified() {
		if parsed.To4() != nil {
			host = DefaultHTTPHost
		} else {
			host = "::1"
		}
	}
	if host == "" {
		host = DefaultHTTPHost
	}
	return "http://" + net.JoinHostPort(host, port)
}
