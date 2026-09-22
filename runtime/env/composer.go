package env

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// sessionShimEnvPrefix is the shared prefix of the session-shim launch contract
// (ADR-2026-08-17 §D1 — sessionshim.EnvKeys).
//
// A prefix rather than an enumerated list, and the key constants are NOT
// imported from sessionshim: that package reaches ptyhost, which reaches this
// one, so importing it here would close an import cycle. Matching on the prefix
// also means a key added to the contract later is runner-only the moment it
// exists rather than the moment someone remembers to list it here.
// sessionshim's own suite asserts every key it defines is refused by
// IsRunnerOnly, so the two halves cannot drift apart silently.
const sessionShimEnvPrefix = "DONMAI_SESSION_SHIM"

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

// InjectedEnvKeysVar is the runner-only environment variable through which an
// EMBEDDING DAEMON declares — by NAME ONLY, never by value — which
// AgentEnvBlocklist names it placed in the worker process environment on
// purpose.
//
// Two layers feed a harness child's environment, and until this declaration
// existed they were distinguishable only by which argument they arrived on:
//
//   - The INHERITED layer (the worker process's own os.Environ()). It is
//     filtered against AgentEnvBlocklist because its usual author is an
//     operator's interactive shell, and a shell's ANTHROPIC_API_KEY must not
//     silently become the agent's (see the package doc).
//   - The EXPLICIT layers (agent.Spec.Env, the ComposeChildEnv override maps).
//     They are runner-set and trusted, so they bypass the blocklist entirely —
//     that is how a provider injects the credential it resolved for a session.
//
// A supervising daemon that resolves per-session credentials out of band and
// merges them into the worker's environment writes into the INHERITED layer,
// where the filter cannot tell a deliberate injection from a shell leak and
// strips it: the worker's own provider probe sees the credential and the
// harness child never does. Declaring the names here is how a supervisor says
// "these inherited names are mine, not the operator's shell's".
//
// The value is a comma-separated list of variable NAMES and carries no secret.
// It is parsed defensively: surrounding whitespace is trimmed, empty elements
// are ignored, and matching is on the exact name — no globs, no prefixes.
//
// A declaration can only re-admit an AgentEnvBlocklist name. IsRunnerOnly names
// are refused at every layer and a declaration that lists one changes nothing:
// those variables address the SUPERVISOR of the process that would receive
// them, so no supervisor may hand them down.
//
// The variable is itself runner-only, so it never reaches a child. A harness
// cannot learn which names were injected for it, and cannot re-declare a set of
// its own for whatever it spawns in turn.
const InjectedEnvKeysVar = "DONMAI_INJECTED_ENV_KEYS"

// InjectedEnvKeySet is a parsed InjectedEnvKeysVar declaration: a set of
// environment variable NAMES. It never holds a value.
type InjectedEnvKeySet map[string]struct{}

// Allows reports whether key may cross the inherited-environment blocklist
// because the embedding daemon declared it injected. Runner-only names are
// refused no matter what the declaration says.
func (s InjectedEnvKeySet) Allows(key string) bool {
	if len(s) == 0 || IsRunnerOnly(key) {
		return false
	}
	_, ok := s[key]
	return ok
}

// ParseInjectedEnvKeys parses an InjectedEnvKeysVar value into a set. Malformed
// input is tolerated rather than rejected — a declaration is an optimization of
// the filter, not a security boundary, and a supervisor that spells it with
// stray spaces or a trailing comma should still get the names it asked for.
// An empty or all-empty value yields a nil set, which Allows treats as "nothing
// declared".
func ParseInjectedEnvKeys(value string) InjectedEnvKeySet {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	out := make(InjectedEnvKeySet)
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out[name] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// InjectedEnvKeysFrom reads the declaration out of a KEY=VALUE slice. A
// duplicate wins last, matching exec.Cmd.Env's own semantics.
func InjectedEnvKeysFrom(entries []string) InjectedEnvKeySet {
	prefix := InjectedEnvKeysVar + "="
	value := ""
	for _, entry := range entries {
		if strings.HasPrefix(entry, prefix) {
			value = entry[len(prefix):]
		}
	}
	return ParseInjectedEnvKeys(value)
}

// InjectedEnvKeysFromMap reads the declaration out of a KEY->VALUE map. It is
// the map counterpart to InjectedEnvKeysFrom.
func InjectedEnvKeysFromMap(entries map[string]string) InjectedEnvKeySet {
	return ParseInjectedEnvKeys(entries[InjectedEnvKeysVar])
}

// IsRunnerOnly reports whether key is a host-side interactive attach control
// that the runner consumes but must never expose to provider processes, PTY
// children, or model-invoked tool subprocesses. Unlike AgentEnvBlocklist, this
// boundary applies to every input layer: an explicit Spec.Env entry cannot
// override it.
//
// The session-shim launch contract (ADR-2026-08-17 §D1) belongs here for the
// same reason the attach controls do. Those values carry no secret — a
// lifecycle identity, a registry directory, four durations — but they address
// the SUPERVISOR of the process that would receive them. A harness child that
// inherited them and re-execed anything shim-aware would be pointed at its own
// session's registry, and a workload has no business reaching the boundary that
// owns it.
func IsRunnerOnly(key string) bool {
	switch key {
	case "ATTACH_TOKEN", "ATTACH_TOKEN_FILE", "ATTACH_URL":
		return true
	case InjectedEnvKeysVar:
		// The declaration addresses this package's own inherited-env filter,
		// which is the supervisor's boundary and not the workload's. A child
		// that could READ it would learn which credential names its supervisor
		// injected; a child that could SET it would re-admit anything it liked
		// into whatever it spawns next.
		return true
	default:
		return strings.HasPrefix(key, sessionShimEnvPrefix)
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
	// The declaration travels in the inherited layer itself: a supervisor that
	// injected a blocklisted name into this process also named it here.
	declared := InjectedEnvKeysFrom(entries)
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if IsRunnerOnly(key) {
			continue
		}
		if isAgentEnvBlocked(key) && !declared.Allows(key) {
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
//     key is in the agent-auth or runner-only blocklist are dropped, unless
//     base itself declares the name in InjectedEnvKeysVar (an embedding
//     daemon saying "I put this here on purpose"). A runner-only name is
//     dropped regardless of any declaration.
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
	declared := InjectedEnvKeysFromMap(base)

	merged := make(map[string]string)
	for k, v := range base {
		if IsRunnerOnly(k) {
			continue
		}
		if _, blocked := blockSet[k]; blocked && !declared.Allows(k) {
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
