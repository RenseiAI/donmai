package env

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// GatewayUpstreamAPIKeyEnv and GatewayUpstreamBaseURLEnv name the worker-local
// translating gateway's upstream credential and route. They live here, not in
// the gateway's own package, because this package owns both the blocklist they
// belong to and the refusal that keeps them on it (see
// AgentEnvIsolationInvariants); afcli/gateway_bind.go re-exports them.
const (
	GatewayUpstreamAPIKeyEnv  = "DONMAI_GATEWAY_UPSTREAM_API_KEY" //nolint:gosec // G101: an env-var NAME, not a credential.
	GatewayUpstreamBaseURLEnv = "DONMAI_GATEWAY_UPSTREAM_BASE_URL"
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
	GatewayUpstreamAPIKeyEnv,
	GatewayUpstreamBaseURLEnv,
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
// A declaration re-admits a SHELL-LEAK entry of AgentEnvBlocklist and nothing
// else. Three classes are refused no matter what it says — see
// InjectedEnvKeySet.Allows.
//
// The variable is itself runner-only, so it never reaches a child. A harness
// cannot learn which names were injected for it, and cannot re-declare a set of
// its own for whatever it spawns in turn. It is also stripped from every
// caller-supplied env map by the daemon's own spawn composition, so only a
// genuine supervisor — an OnPreSpawn hook, which runs after that composition —
// can author one.
const InjectedEnvKeysVar = "DONMAI_INJECTED_ENV_KEYS"

// AgentEnvIsolationInvariants is the subset of AgentEnvBlocklist whose blocking
// is an ISOLATION INVARIANT rather than the shell-leak heuristic the rest of
// the list encodes. No InjectedEnvKeysVar declaration can re-admit these.
//
// The other entries answer "an operator's interactive shell key must not
// silently become the agent's", and a supervisor that injected one on purpose
// has standing to say so. These two answer something else: the entire point of
// a gateway cell is that the harness child receives ONLY the gateway's
// per-session loopback bearer while the upstream credential and route stay in
// the worker process (afcli/gateway_bind.go). A declaration has no standing to
// undo that, because the party that benefits from undoing it is the child.
var AgentEnvIsolationInvariants = []string{
	GatewayUpstreamAPIKeyEnv,
	GatewayUpstreamBaseURLEnv,
}

// GatewayUpstreamEnvKeysVar is the runner-only variable through which the
// worker-local gateway names, at bind time, the environment variables that hold
// THIS session's upstream credential and route.
//
// It exists because the gateway's upstream credential is not always spelled
// with a gateway name: afcli/gateway_bind.go falls back to OPENAI_API_KEY when
// GatewayUpstreamAPIKeyEnv is unset, and OPENAI_API_KEY is an ordinary
// shell-leak entry a supervisor may legitimately declare for a NON-gateway
// session. Whether a given name holds a gateway upstream credential is
// therefore a property of the SESSION, not of the name, and only the gateway
// knows it. AgentEnvIsolationInvariants alone would miss exactly the path that
// leaks the real upstream key.
//
// The gateway publishes the set into its own worker process with
// DeclareGatewayUpstreamEnvKeys, before any child spawns; every inherited-env
// filter reads it back out of the same environment. Like the declaration
// itself the variable is runner-only, so a child can neither read it nor forge
// one for what it spawns.
const GatewayUpstreamEnvKeysVar = "DONMAI_GATEWAY_UPSTREAM_ENV_KEYS"

// DeclareGatewayUpstreamEnvKeys records, in THIS process, the env-var NAMES
// that hold the gateway cell's upstream credential and route, so no
// InjectedEnvKeysVar declaration can re-admit them into a harness child.
//
// Called by the gateway bind in the worker process. It is process-wide on
// purpose: a worker process runs exactly one session, so "this session is
// gateway-served, dialling with the credential in <name>" is a property of the
// process.
func DeclareGatewayUpstreamEnvKeys(names ...string) error {
	cleaned := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			cleaned = append(cleaned, name)
		}
	}
	return os.Setenv(GatewayUpstreamEnvKeysVar, strings.Join(cleaned, ","))
}

// InjectedEnvKeySet is a parsed InjectedEnvKeysVar declaration together with
// the per-session refusals that outrank it. It holds NAMES, never values. The
// zero value declares nothing, so it admits nothing.
type InjectedEnvKeySet struct {
	declared        map[string]struct{}
	gatewayUpstream map[string]struct{}
}

// Allows reports whether key may cross the inherited-environment blocklist
// because the embedding daemon declared it injected.
//
// A declaration is the ONLY input in this package that widens a boundary, and
// the party that benefits from widening it further is the child — so three
// classes are refused regardless of what it says:
//
//  1. IsRunnerOnly names. They address the SUPERVISOR of the process that would
//     receive them, so no supervisor hands them down.
//  2. AgentEnvIsolationInvariants — the gateway cell's upstream credential and
//     route, by name.
//  3. Whatever the gateway named for THIS session through
//     GatewayUpstreamEnvKeysVar. Same invariant, reached through a name that is
//     ordinarily declarable.
func (s InjectedEnvKeySet) Allows(key string) bool {
	if len(s.declared) == 0 {
		return false
	}
	if IsRunnerOnly(key) || isIsolationInvariant(key) {
		return false
	}
	if _, refused := s.gatewayUpstream[key]; refused {
		return false
	}
	_, ok := s.declared[key]
	return ok
}

// isIsolationInvariant reports whether key is blocked by an isolation
// invariant rather than the shell-leak heuristic, and is therefore never
// declarable.
func isIsolationInvariant(key string) bool {
	for _, invariant := range AgentEnvIsolationInvariants {
		if key == invariant {
			return true
		}
	}
	return false
}

// ParseInjectedEnvKeys parses an InjectedEnvKeysVar declaration and a
// GatewayUpstreamEnvKeysVar refusal list into a set. Both are comma-separated
// lists of exact variable NAMES.
//
// Malformed input is tolerated rather than rejected, and deliberately so:
// tolerance here fails CLOSED. An element that does not parse to an exact name
// admits nothing, so the worst a mangled declaration can do is leave a
// credential stripped — while rejecting the whole value would turn one stray
// comma into a hard spawn failure.
//
// Exact names only, always. A wildcard or prefix form is forbidden by design:
// this is the one input in this package that widens a security filter, and a
// pattern cannot be audited against the set of names it would admit tomorrow.
func ParseInjectedEnvKeys(declaration, gatewayUpstream string) InjectedEnvKeySet {
	return InjectedEnvKeySet{
		declared:        parseEnvNameList(declaration),
		gatewayUpstream: parseEnvNameList(gatewayUpstream),
	}
}

// parseEnvNameList splits a comma-separated list of variable names into a set,
// trimming each element and dropping empties. An empty or all-empty value
// yields nil.
func parseEnvNameList(value string) map[string]struct{} {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, name := range strings.Split(value, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out[name] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// InjectedEnvKeysFrom reads the declaration and the gateway refusal list out of
// a KEY=VALUE slice. A duplicate wins last, matching exec.Cmd.Env's semantics.
func InjectedEnvKeysFrom(entries []string) InjectedEnvKeySet {
	return ParseInjectedEnvKeys(
		lastEnvValue(entries, InjectedEnvKeysVar),
		lastEnvValue(entries, GatewayUpstreamEnvKeysVar),
	)
}

// InjectedEnvKeysFromMap is the map counterpart to InjectedEnvKeysFrom.
func InjectedEnvKeysFromMap(entries map[string]string) InjectedEnvKeySet {
	return ParseInjectedEnvKeys(entries[InjectedEnvKeysVar], entries[GatewayUpstreamEnvKeysVar])
}

// lastEnvValue returns the value of the last KEY=VALUE entry naming key, or ""
// when key is absent. A bare key with no '=' is not a setting and is ignored.
func lastEnvValue(entries []string, key string) string {
	prefix := key + "="
	value := ""
	for _, entry := range entries {
		if strings.HasPrefix(entry, prefix) {
			value = entry[len(prefix):]
		}
	}
	return value
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
	case InjectedEnvKeysVar, GatewayUpstreamEnvKeysVar:
		// Both address this package's own inherited-env filter, which is the
		// supervisor's boundary and not the workload's. A child that could READ
		// the declaration would learn which credential names its supervisor
		// injected; a child that could SET either one could re-admit anything it
		// liked — or clear the gateway refusal — for whatever it spawns next.
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
