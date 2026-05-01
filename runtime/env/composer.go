package env

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// AgentEnvBlocklist is the set of environment variable names that must
// never propagate from the daemon's host environment into an agent
// provider subprocess.
//
// The list mirrors AGENT_ENV_BLOCKLIST in
// ../agentfactory/packages/core/src/orchestrator/orchestrator.ts and
// agent-spawner.ts verbatim. It captures the sensitive Anthropic auth
// surface plus the OpenClaw gateway token; provider implementations
// inject their credential of choice through Spec.Env (which is NOT
// blocked — see Composer.Compose).
//
// When the legacy TS adds a new entry, port it here and update the
// inline comment in package env's README.
var AgentEnvBlocklist = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"OPENCLAW_GATEWAY_TOKEN",
}

// Composer builds the KEY=VALUE slice handed to exec.Cmd.Env for an
// agent provider subprocess.
//
// A nil Composer is valid: zero-value methods use AgentEnvBlocklist.
// Tests can override Blocklist to validate filtering behavior without
// touching the package-level constant.
type Composer struct {
	// Blocklist overrides AgentEnvBlocklist for this Composer. A nil
	// or empty slice falls back to AgentEnvBlocklist; pass an empty
	// non-nil slice ([]string{}) to disable blocklisting entirely.
	Blocklist []string
}

// NewComposer returns a Composer using the package-level
// AgentEnvBlocklist. Equivalent to &Composer{}.
func NewComposer() *Composer {
	return &Composer{}
}

// effectiveBlocklist returns the Composer's blocklist, falling back to
// the package-level constant when Blocklist is nil. An explicitly empty
// slice ([]string{}) bypasses the constant — useful in tests.
func (c *Composer) effectiveBlocklist() []string {
	if c == nil || c.Blocklist == nil {
		return AgentEnvBlocklist
	}
	return c.Blocklist
}

// Compose returns a deterministic []string in KEY=VALUE form suitable
// for exec.Cmd.Env.
//
// Precedence (lowest to highest, last write wins):
//
//  1. base — typically os.Environ() parsed into a map. Entries whose
//     key is in the blocklist are dropped before merge so a daemon
//     operator's ANTHROPIC_API_KEY does not bleed into the agent
//     subprocess.
//  2. spec.Env — the per-session env map carried on agent.Spec. NOT
//     subject to the blocklist (the runner sets these intentionally).
//
// Within each layer the merge is map-iteration-order-stable: keys are
// sorted lexicographically to keep golden tests reproducible.
//
// Empty values (V == "") are preserved — exec.Cmd treats KEY= as
// "set to empty", which differs from "unset". The runner uses this to
// override an inherited host variable.
//
// Returns the merged []string. The caller can append additional
// runner-internal entries before handing it to exec.Cmd.Env.
func (c *Composer) Compose(base map[string]string, spec agent.Spec) []string {
	blocklist := c.effectiveBlocklist()
	blockSet := make(map[string]struct{}, len(blocklist))
	for _, k := range blocklist {
		blockSet[k] = struct{}{}
	}

	merged := make(map[string]string, len(base)+len(spec.Env))
	for k, v := range base {
		if _, blocked := blockSet[k]; blocked {
			continue
		}
		merged[k] = v
	}
	// spec.Env wins — runner-set credentials and session metadata
	// override anything inherited from the host.
	for k, v := range spec.Env {
		merged[k] = v
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%s", k, merged[k]))
	}
	return out
}

// IsBlocked reports whether key is in the effective blocklist for this
// Composer. Useful for callers that want to log a warning when an
// operator-supplied env attempted to set a sensitive variable.
func (c *Composer) IsBlocked(key string) bool {
	for _, k := range c.effectiveBlocklist() {
		if k == key {
			return true
		}
	}
	return false
}

// LooksSensitive reports whether key matches a heuristic pattern for a
// likely-sensitive env var (token, secret, key, password). The runner
// uses this to emit a soft warning when a Spec.Env entry may have been
// set by mistake. It is not a security boundary — the blocklist is.
func LooksSensitive(key string) bool {
	upper := strings.ToUpper(key)
	for _, frag := range sensitiveFragments {
		if strings.Contains(upper, frag) {
			return true
		}
	}
	return false
}

// sensitiveFragments is the substring list LooksSensitive matches
// against (upper-case). Kept short so the heuristic stays useful.
var sensitiveFragments = []string{
	"TOKEN",
	"SECRET",
	"PASSWORD",
	"PASSWD",
	"PRIVATE_KEY",
	"API_KEY",
}
