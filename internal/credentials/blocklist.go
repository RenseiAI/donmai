// Package credentials owns the env-forwarding blocklist that prevents the
// daemon's own auth surface from leaking into agent subprocesses.
//
// Single source of truth. AgentEnvBlocklist below is the canonical list;
// cross-repo consumers vendor the generated, language-neutral rendering in
// blocklist.json (produced by `make generate`, verified by
// `make verify-generated`) instead of hand-copying names:
//   - rensei-tui renders a Go baseline from the same var
//     (scripts/gen-blocklist.go → daemon/credentials/generated_blocklist.go);
//   - platform vendors blocklist.json and checks it in CI.
//
// The JSON digest uses the identical sha256-over-newline-terminated-names
// normalization as rensei-tui, so all three repos tie to one hash. A drift
// on any side fails that repo's freshness check loudly.
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

//go:generate go run ./gen

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
//   - DONMAI_DAEMON_TOKEN          — orchestrator auth token (daemon registration / re-auth)
//   - DONMAI_ORCHESTRATOR_URL      — orchestrator base URL (internal routing surface)
//   - DONMAI_CREDENTIAL_CAPABILITY — per-session credential-socket capability
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
	"DONMAI_DAEMON_TOKEN",
	"DONMAI_ORCHESTRATOR_URL",
	"DONMAI_CREDENTIAL_CAPABILITY",
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
