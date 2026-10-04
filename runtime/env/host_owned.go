package env

// Host-owned settings.
//
// A few variables state a property of the HOST rather than of one session:
// the daemon's own environment (or its embedder's static base environment)
// sets them, and every worker the daemon spawns inherits them. A work item
// must not be able to set them, because the work item's environment is
// copied verbatim from the orchestrator and is applied AFTER the daemon's
// inherited environment — a work item carrying one of these names would
// otherwise override the host's answer for its own session.
//
// Unlike IsRunnerOnly, a host-owned name is not stripped from the daemon's
// own environment: the whole point is that the host's value reaches the
// worker. Only the work item's copy is dropped (FilterHostOwnedMap).

// PiConfinementEnv is the host-level switch that makes every pi session on
// this host request the executor OS confinement
// (ADR-2026-10-03-executor-os-confinement.md D5.1, placement-owned
// configuration). The only recognized value is "required"; the pi provider
// reads it at construction and it can only tighten.
const PiConfinementEnv = "DONMAI_PI_CONFINEMENT"

// DaemonControlURLEnv is the variable the daemon sets on every worker it
// spawns to name its own control API (daemon.EnvDaemonControlURL is this
// constant). It is daemon-owned already: the daemon composes it last.
const DaemonControlURLEnv = "DONMAI_DAEMON_URL"

// DefaultDaemonControlPort is the daemon control API's well-known loopback
// port (daemon.DefaultHTTPPort is this constant).
const DefaultDaemonControlPort = 7734

// IsHostOwned reports whether key is a host-owned setting a work item may
// not set.
func IsHostOwned(key string) bool {
	return key == PiConfinementEnv
}

// FilterHostOwnedMap returns a defensive copy of a work item's environment
// with the host-owned settings removed, so the host's own value — inherited
// from the daemon's environment or set in its base environment — is the one
// the worker sees. Entries whose key is not a valid variable name are
// dropped as well (ValidEnvKey): {"DONMAI_PI_CONFINEMENT=off": ""} would
// otherwise set the host-owned variable through a key that is not it.
func FilterHostOwnedMap(entries map[string]string) map[string]string {
	if entries == nil {
		return nil
	}
	out := make(map[string]string, len(entries))
	for key, value := range entries {
		if !ValidEnvKey(key) || IsHostOwned(key) {
			continue
		}
		out[key] = value
	}
	return out
}
