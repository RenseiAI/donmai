package env

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// AgentEnvBlocklist is the set of environment variable names that must
// never propagate from the daemon's host environment into an agent
// provider subprocess.
//
// The list mirrors AGENT_ENV_BLOCKLIST in
// ../donmai-libraries/packages/core/src/orchestrator/orchestrator.ts and
// agent-spawner.ts verbatim. It captures the sensitive Anthropic auth
// surface plus the OpenClaw gateway token; provider implementations
// inject their credential of choice through Spec.Env (which is NOT
// blocked — see Composer.Compose).
//
// When the legacy TS adds a new entry, port it here and update the
// inline comment in package env's README.
//
// DONMAI_GATEWAY_UPSTREAM_API_KEY and DONMAI_GATEWAY_UPSTREAM_BASE_URL have
// no legacy-TS counterparts: they configure the donmai-native worker-local
// translating gateway (afcli/gateway_bind.go). They MUST be blocked here,
// because the entire point of a gateway cell is that the harness child
// receives only the gateway's per-session loopback bearer while the upstream
// credential and route stay in the worker process. An inherited copy in the
// child would silently undo that isolation.
//
// AMP_API_KEY likewise has no legacy-TS counterpart: it is the amp harness's
// personal access token (provider/harness/amp, EnvAPIKey), a model-provider
// credential like the Anthropic/OpenAI/Gemini keys above. The platform's
// suppressed model-provider set already includes it; blocking it here keeps
// the runtime layer in parity so a host operator's Amp token cannot leak
// into an agent subprocess. Per-session Amp credentials still ride Spec.Env,
// which is trusted (see Composer.Compose).
var AgentEnvBlocklist = []string{
	"AMP_API_KEY",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"DONMAI_GATEWAY_UPSTREAM_API_KEY",
	"DONMAI_GATEWAY_UPSTREAM_BASE_URL",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"OPENCLAW_GATEWAY_TOKEN",
	"OPENAI_API_KEY",
}

// IsRunnerOnly reports whether key is a host-side interactive attach control
// that the runner consumes but must never expose to provider processes, PTY
// children, or model-invoked tool subprocesses. Unlike AgentEnvBlocklist, this
// boundary applies to every input layer: an explicit Spec.Env entry cannot
// override it.
func IsRunnerOnly(key string) bool {
	switch key {
	case "ATTACH_TOKEN", "ATTACH_TOKEN_FILE", "ATTACH_URL":
		return true
	default:
		return false
	}
}

// FilterRunnerOnly returns a copy of entries with runner-owned controls
// removed. entries use the os.Environ KEY=VALUE form; a bare blocked key is
// removed as well so malformed input cannot bypass the boundary.
func FilterRunnerOnly(entries []string) []string {
	if len(entries) == 0 {
		return entries
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if IsRunnerOnly(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// FilterRunnerOnlyMap returns a defensive copy of entries with runner-owned
// controls removed. It is the serialization counterpart to FilterRunnerOnly:
// callers use it before placing explicit environment maps in child configs.
func FilterRunnerOnlyMap(entries map[string]string) map[string]string {
	if entries == nil {
		return nil
	}
	out := make(map[string]string, len(entries))
	for key, value := range entries {
		if IsRunnerOnly(key) {
			continue
		}
		out[key] = value
	}
	return out
}

func filterInheritedChildEnv(entries []string) []string {
	if len(entries) == 0 {
		return entries
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if IsRunnerOnly(key) || isAgentEnvBlocked(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isAgentEnvBlocked(key string) bool {
	for _, blocked := range AgentEnvBlocklist {
		if key == blocked {
			return true
		}
	}
	return false
}

// ComposeChildEnv merges an inherited KEY=VALUE environment with zero or more
// explicit override layers. Runner-owned controls are removed from every layer,
// while agent-auth entries are removed only from the inherited parent. Trusted
// explicit layers may inject the provider credential selected for this session.
// Later explicit layers win. Explicit keys are sorted before appending so they
// deterministically win over inherited duplicates under exec.Cmd's
// last-entry-wins semantics.
func ComposeChildEnv(parent []string, explicit ...map[string]string) []string {
	filteredParent := filterInheritedChildEnv(parent)
	mergedExplicit := make(map[string]string)
	for _, layer := range explicit {
		for key, value := range FilterRunnerOnlyMap(layer) {
			mergedExplicit[key] = value
		}
	}
	// Explicit entries grow through append's checked runtime path; avoid an
	// overflow-prone capacity sum over independently controlled collections.
	out := make([]string, 0, len(filteredParent))
	out = append(out, filteredParent...)

	keys := make([]string, 0, len(mergedExplicit))
	for key := range mergedExplicit {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, fmt.Sprintf("%s=%s", key, mergedExplicit[key]))
	}
	return out
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
//     key is in the agent-auth or runner-only blocklist are dropped.
//  2. spec.Env — the per-session env map carried on agent.Spec. Agent-auth
//     entries are trusted here, but runner-only controls are still dropped.
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

	// No capacity hint: a Go map grows on demand, so pre-sizing buys nothing
	// here, and summing len()s as an allocation size is exactly the shape a
	// static scanner (go/allocation-size-overflow) flags as a potential overflow.
	merged := make(map[string]string)
	for k, v := range base {
		if IsRunnerOnly(k) {
			continue
		}
		if _, blocked := blockSet[k]; blocked {
			continue
		}
		merged[k] = v
	}
	// spec.Env wins for session credentials and metadata, except for
	// runner-only attach controls: those remain host-side at every layer.
	for k, v := range spec.Env {
		if IsRunnerOnly(k) {
			continue
		}
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
