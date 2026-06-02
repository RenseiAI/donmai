// Package credentials owns the env-forwarding blocklist that prevents the
// daemon's own auth surface from leaking into agent subprocesses.
//
// Single source of truth. The rensei-tui daemon hardcodes the same list in
// daemon/credentials/socket.go; keep them in sync until the OSS boundary
// allows for a shared import or generated codepath.
// See REN-FOLLOWUP: blocklist-sync.
//
// This list is intentionally distinct from
// runtime/env.AgentEnvBlocklist — that one blocks the host operator's
// upstream LLM auth (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, ...)
// from racing the runner's injected per-session credentials. The list
// here blocks the daemon's *own* auth surface (the JWT it uses to talk
// to the platform, the worker-protocol API key, the WorkOS session
// password) so that even if those values are present in process env or
// .env.local they are never forwarded into a child agent.
package credentials

import "strings"

// AgentEnvBlocklist is the canonical list of environment variable names
// that must never propagate from AF-TUI's process env (or .env.local)
// into a child agent subprocess.
//
// Entries:
//   - DONMAI_DAEMON_JWT            — daemon's platform JWT
//   - DONMAI_DAEMON_API_KEY        — daemon's platform API key
//   - M2M_JWT_SECRET               — machine-to-machine JWT signing secret
//   - AUDIT_SIGNING_KEY_PRIVATE    — audit-chain Ed25519 signing key (private)
//   - AUDIT_SIGNING_KEY_PUBLIC     — audit-chain Ed25519 signing key (public)
//   - WORKOS_API_KEY               — WorkOS server key (control plane)
//   - WORKOS_COOKIE_PASSWORD       — WorkOS session cookie password
//   - DONMAI_RUNTIME_JWT           — runtime JWT minted on registration
//   - WORKER_API_KEY               — worker-protocol bearer (rsk_*)
var AgentEnvBlocklist = []string{
	"DONMAI_DAEMON_JWT",
	"DONMAI_DAEMON_API_KEY",
	"M2M_JWT_SECRET",
	"AUDIT_SIGNING_KEY_PRIVATE",
	"AUDIT_SIGNING_KEY_PUBLIC",
	"WORKOS_API_KEY",
	"WORKOS_COOKIE_PASSWORD",
	"DONMAI_RUNTIME_JWT",
	"WORKER_API_KEY",
}

// IsBlocked reports whether name is in AgentEnvBlocklist.
//
// Comparison is exact (case-sensitive) — POSIX env var names are
// case-sensitive on all supported platforms.
func IsBlocked(name string) bool {
	for _, k := range AgentEnvBlocklist {
		if k == name {
			return true
		}
	}
	return false
}

// Filter returns a copy of env (in os.Environ() "KEY=VALUE" form) with
// any blocked entries removed.
//
// Malformed entries (those without "=") are passed through unchanged —
// Filter is permissive so callers can use it as a drop-in os.Environ()
// post-processor without losing odd-but-valid entries (e.g. some shells
// preserve a bare "VAR" with no "=" assignment).
func Filter(env []string) []string {
	if len(env) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		idx := strings.IndexByte(entry, '=')
		if idx < 0 {
			// Malformed — pass through.
			out = append(out, entry)
			continue
		}
		name := entry[:idx]
		if IsBlocked(name) {
			continue
		}
		out = append(out, entry)
	}
	return out
}
