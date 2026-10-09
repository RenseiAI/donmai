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

// PiConfinementReadEnv is the host-level read scope for confined pi
// sessions, a fileRead level on the execution-security ladder: "workarea"
// confines reads to the session's workarea plus the runtime, toolchain and
// declared paths, and implies DONMAI_PI_CONFINEMENT=required; "host" or
// unset leaves reads open. Any other value refuses pi at construction. It
// can only tighten.
const PiConfinementReadEnv = "DONMAI_PI_CONFINEMENT_READ"

// PiConfinementReadPathsEnv lists further absolute paths, separated like
// PATH, that confined pi sessions may read under a read scope: host
// configuration or per-session credential files outside the workarea. An
// embedder can set it per session from its pre-spawn hook.
const PiConfinementReadPathsEnv = "DONMAI_PI_CONFINEMENT_READ_PATHS"

// DaemonControlURLEnv is the variable the daemon sets on every worker it
// spawns to name its own control API (daemon.EnvDaemonControlURL is this
// constant). It is daemon-owned already: the daemon composes it last.
const DaemonControlURLEnv = "DONMAI_DAEMON_URL"

// ControlTokenPathEnv is the variable that overrides the daemon's
// control-token file path (afclient.ControlTokenFileEnv is this constant).
// The pi provider reads it in the worker to learn the token path the
// daemon resolves for the operator's CLI — the file-env override wins when
// absolute, else the host state home — and passes that path into the seat
// confinement as a daemon-private deny. It is runner-only: it never reaches
// a spawned session's environment (see IsRunnerOnly), so reading it here
// discloses nothing to the seat.
const ControlTokenPathEnv = "DONMAI_CONTROL_TOKEN_FILE"

// SessionReadTokenEnv names the per-session read credential a supervisor
// states in a spawned worker's environment beside the session id it names.
// It authorizes exactly one session-detail read — the detail for that
// session — and carries no wider privilege. The worker consumes it during
// bootstrap and must not pass it to anything it spawns: every inherited-env
// composition in this package strips it, so it never reaches harness or
// agent child processes through the parent environment. Only an explicit
// supervisor-authored layer (the spawner's daemon-owned env) may state it.
const SessionReadTokenEnv = "DONMAI_SESSION_READ_TOKEN" //nolint:gosec // G101: an env-var NAME, not a credential.

// DefaultDaemonControlPort is the daemon control API's well-known loopback
// port (daemon.DefaultHTTPPort is this constant).
const DefaultDaemonControlPort = 7734

// IsHostOwned reports whether key is a host-owned setting a work item may
// not set.
func IsHostOwned(key string) bool {
	switch key {
	case PiConfinementEnv, PiConfinementReadEnv, PiConfinementReadPathsEnv:
		return true
	default:
		return false
	}
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
