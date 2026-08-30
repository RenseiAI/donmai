package pi

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// PiKeyEnvVar is the env var the materialized provider pin references for the
// resolved cell's API key (extension.go providerPinConfig). The key rides this
// env var into the child process; it is never written to disk.
const PiKeyEnvVar = "DONMAI_PI_KEY"

// SpecFieldNote names an agent.Spec field the pi provider does not honor and
// the reason — the codex/spec_translation.go pattern (its ignoredSpecFields):
// a field the provider cannot deliver is named explicitly rather than
// silently dropped, so a caller who set it is told instead of left to
// discover the gap by absence.
type SpecFieldNote struct {
	Field  string
	Reason string
}

// codeIntelEnforcementUnsupportedSubtype is the SystemEvent.Subtype pi.go
// emits, once per session and before the turn is dispatched, when
// Spec.CodeIntelEnforcement is set. See codeIntelEnforcementNote.
const codeIntelEnforcementUnsupportedSubtype = "code_intel_enforcement_unsupported"

// codeIntelEnforcementNote returns the typed drop note for
// Spec.CodeIntelEnforcement, or nil when the field is unset. pi has no
// canUseTool-equivalent callback: the injected policy extension's tool_call
// hook adjudicates allow/deny against the compiled PermissionConfig, but it
// has no hook for redirecting a native Grep/Glob call to af_code_* tools
// first. Every other in-tree harness drops this field the same way (see
// codex/spec_translation.go's own CodeIntelEnforcement note, and the
// SupportsCodeIntelligenceEnforcement:false comments on claude/gemini/amp/
// ollama/opencode/agycli); pi previously dropped it with no note at all.
func codeIntelEnforcementNote(spec agent.Spec) *SpecFieldNote {
	if spec.CodeIntelEnforcement == nil {
		return nil
	}
	return &SpecFieldNote{
		Field:  "CodeIntelEnforcement",
		Reason: "pi has no canUseTool-equivalent callback; the injected policy extension adjudicates tool_call allow/deny only, not a Grep/Glob af_code_* redirect",
	}
}

// rpcArgs is the argv for the headless RPC lane. It loads the donmai policy
// extension FIRST, then every entry in extensionPaths (spec.AdditionalExtensions,
// already materialized and digest-verified by materializeAdditionalExtensions;
// ADR-2026-08-12 D1) in caller order, and nothing else:
//
//   - `-e <path>`, once per extension (boundary first, undisplaceable — D1). A
//     CLI `-e` extension loads regardless of project trust (docs/extensions.md:
//     project-local `.pi/extensions` copies only load after the project is
//     trusted), which is why the harness passes explicit paths here rather
//     than relying on auto-discovery — this is the operator-injected trust
//     bypass D2 formalizes, and it applies identically to every `-e` entry.
//   - `--no-extensions` disables all OTHER extension discovery (explicit `-e`
//     paths still load), so no user/global/project extension can shadow or
//     race the trust boundary — the other half of D2: workspace-discovered
//     extensions never load in an autonomous session.
//   - `--approve` trusts project-local files for this run (autonomous sessions
//     have no user to answer a trust prompt).
//   - `--session-dir <layout.root>` keeps session storage inside the worktree.
//
// The model + reasoning effort are pinned HERE, on the CLI, so the session
// starts on the resolved cell's model (design §6). `--provider donmai --model
// <id>[:<thinking>]` selects the "donmai" provider the policy extension
// registers from env; because the pin is applied at startup, the first turn
// cannot run on pi's default model (a runtime set_model races the prompt —
// verified against the real binary). When no endpoint is bound (no baseURL for
// the extension to register a provider), only `--model <id>` is passed and pi
// resolves the provider from its own config.
//
// For a resume, `--session <id>` selects the session to reload; get_entries
// then replays from the caller's cursor. The single-shot structured lane
// (`pi --mode json`, StructuredVia:"spawn-collect", design §7) is a deferred
// follow-up — SupportsOneShot is advertised but the SpawnComplete projection
// is not wired in this cut.
func rpcArgs(layout sessionLayout, extensionPaths []string, mode launchMode, sessionID string, spec agent.Spec) []string {
	args := []string{"--mode", "rpc"}
	for _, path := range extensionPaths {
		args = append(args, "-e", path)
	}
	args = append(args,
		"--no-extensions",
		"--approve",
		"--session-dir", layout.root,
	)
	if spec.SessionName != "" && mode == launchPrompt {
		args = append(args, "--name", spec.SessionName)
	}
	args = append(args, modelPinArgs(spec)...)
	if spec.SystemPromptAppend != "" {
		args = append(args, "--append-system-prompt", spec.SystemPromptAppend)
	}
	if mode == launchResume && sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	return args
}

// modelPinArgs builds the `--model` / `--provider donmai --model` pin shared by
// the headless RPC lane (rpcArgs) and the interactive PTY lane (interactive.go
// interactiveArgs), so both spawn modes select the resolved cell's model — and,
// when the cell binds an endpoint, ONLY that endpoint — through one code path.
//
// The provider pin fires iff the endpoint binding names a BaseURL: the embedded
// policy extension registers the single "donmai" provider from env only when a
// baseURL is present (extensions/donmai-policy.ts), so `--provider donmai` is
// resolvable exactly in that case. An unbound session passes plain `--model` and
// lets pi resolve the provider from its own config. Empty Spec.Model emits
// nothing, leaving pi on its own default. The reasoning-effort suffix mirrors
// pi's `--model <id>[:<thinking>]` grammar.
func modelPinArgs(spec agent.Spec) []string {
	if spec.Model == "" {
		return nil
	}
	modelArg := spec.Model
	if lvl := thinkingLevelForEffort(spec.Effort); lvl != "" {
		modelArg += ":" + lvl
	}
	if spec.Endpoint != nil && spec.Endpoint.BaseURL != "" {
		return []string{"--provider", pinnedProviderName, "--model", modelArg}
	}
	return []string{"--model", modelArg}
}

// pinnedProviderName is the provider name the policy extension registers from
// env and the harness pins the model against (design §6).
const pinnedProviderName = "donmai"

// composeChildEnv builds the child process env with the env-hygiene posture
// design §5.3 requires: because pi runs tools with the FULL permissions of the
// spawning user, it must NEVER see broader host credentials. The Composer
// drops every AgentEnvBlocklist key (ANTHROPIC_API_KEY, OPENAI_API_KEY, …) and
// every runner-only control from the PARENT env, while still trusting the
// resolved cell's credentials delivered on Spec.Env (applyEndpoint mirrors the
// cell key onto PiKeyEnvVar). The PI_CODING_AGENT_DIR/_SESSION_DIR redirect
// layer is appended last (wins) so the child's pi config/auth/session home
// resolves inside the session worktree — a fleet box's personal
// ~/.pi/agent/auth.json is never visible.
//
// It also carries the trust-boundary handshake token (piHandshakeEnvVar) and
// the non-secret provider-pin vars (providerPinEnv) the policy extension reads
// at load; the key itself already rides Spec.Env under PiKeyEnvVar.
func composeChildEnv(spec agent.Spec, layout sessionLayout, token string) []string {
	out := runtimeenv.NewComposer().Compose(envSliceToMap(os.Environ()), spec)
	// Redirect pi's config/agent home and session home into the session dir
	// using the EXACT documented variable names (docs/environment-variables.md:
	// PI_CODING_AGENT_DIR overrides the config directory, default
	// ~/.pi/agent; PI_CODING_AGENT_SESSION_DIR overrides session storage,
	// itself overridden by --session-dir which rpcArgs already passes). This
	// replaces the four undocumented candidate names (PI_HOME, PI_CONFIG_DIR,
	// PI_STATE_DIR, XDG_CONFIG_HOME) a prior cut set on the unverified
	// assumption that one of them was load-bearing — none of them were
	// (ADR-2026-08-12 F3/D4.1). The two documented vars point at DIFFERENT
	// subdirectories (layout.agentHome vs layout.root) rather than the same
	// path — see sessionLayout.agentHome's doc comment for why collapsing
	// them breaks pi's own `--session <id>` resume lookup. Appended last ⇒
	// these win under exec's last-entry-wins semantics even if the host set
	// them.
	out = append(
		out,
		piCodingAgentDirEnvVar+"="+layout.agentHome,
		piCodingAgentSessionDirEnvVar+"="+layout.root,
	)
	out = append(out, offlinePostureEnv(spec)...)
	out = append(out, providerPinEnv(spec)...)
	if token != "" {
		out = append(out, piHandshakeEnvVar+"="+token)
	}
	return out
}

const (
	// piCodingAgentDirEnvVar overrides pi's config directory (default
	// ~/.pi/agent). This is the ONE documented agent-directory variable
	// (docs/environment-variables.md), replacing four undocumented guesses
	// per ADR-2026-08-12 D4.1.
	piCodingAgentDirEnvVar = "PI_CODING_AGENT_DIR"
	// piCodingAgentSessionDirEnvVar overrides pi's session storage directory;
	// --session-dir (rpcArgs/interactiveArgs) already wins over it, but D4.1
	// names both the agent-directory AND session-directory variables, and
	// setting it defends any nested pi invocation a tool might make that the
	// CLI flag would not reach.
	piCodingAgentSessionDirEnvVar = "PI_CODING_AGENT_SESSION_DIR"
	// piOfflineEnvVar disables pi's startup network operations (update
	// checks, package updates, install/update telemetry) — documented in
	// docs/environment-variables.md, and equivalent to the --offline flag.
	piOfflineEnvVar = "PI_OFFLINE"
	// piSkipVersionCheckEnvVar disables pi's pi.dev latest-version request.
	piSkipVersionCheckEnvVar = "PI_SKIP_VERSION_CHECK"
)

// offlinePostureEnv returns the PI_OFFLINE / PI_SKIP_VERSION_CHECK default
// bindings (ADR-2026-08-12 D4.3: defaulted on for every execution-layer-
// spawned session, headless and interactive alike — the binary pin and the
// resolved model catalog are already the spawning side's to own, so a spawn
// that reaches pi.dev to refresh either is a second, unpinned source of
// truth). An entry the caller already bound explicitly on spec.Env is left
// alone: per D4.3 "a session may re-enable either, recorded as an explicit
// environment-binding entry rather than acquired by omission" — the default
// must never override an explicit choice.
func offlinePostureEnv(spec agent.Spec) []string {
	var out []string
	if _, explicit := spec.Env[piOfflineEnvVar]; !explicit {
		out = append(out, piOfflineEnvVar+"=1")
	}
	if _, explicit := spec.Env[piSkipVersionCheckEnvVar]; !explicit {
		out = append(out, piSkipVersionCheckEnvVar+"=1")
	}
	return out
}

// envSliceToMap parses an os.Environ()-style KEY=VALUE slice into a map.
func envSliceToMap(entries []string) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		if i := strings.IndexByte(e, '='); i >= 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}

// thinkingLevelForEffort maps the normalized reasoning-effort tier to pi's
// set_thinking_level argument (design §3: off…max). Empty effort returns ""
// (no set_thinking_level command issued).
func thinkingLevelForEffort(e agent.EffortLevel) string {
	switch e {
	case agent.EffortLow:
		return "low"
	case agent.EffortMedium:
		return "medium"
	case agent.EffortHigh:
		return "high"
	case agent.EffortXHigh:
		return "max"
	default:
		return ""
	}
}

// applyEndpoint projects a resolved Spec.Endpoint onto the spec the pi session
// runs under (design §6/§7; clones claude/opencode endpoint.go's posture).
// It (a) fails loudly on a company/host outside pi's declared Drive surface,
// (b) honors Endpoint.Model over Spec.Model, and (c) surfaces the resolved
// cell key on spec.Env under PiKeyEnvVar so the materialized provider pin's
// env reference resolves in the child (key rides env, never disk).
//
// Routable surface = the pi manifest's Drives × DrivesHosts intersected with
// the authored cells: anthropic-messages / openai-chat / openai-responses /
// gemini-generate over direct/local/bedrock/vertex, plus validated worker-local
// loopback gateway bindings. Anything else is not routable — silently spawning
// against a default would mis-bill and mis-route.
func applyEndpoint(spec agent.Spec) (agent.Spec, error) {
	ep := spec.Endpoint
	if ep == nil {
		return spec, nil
	}
	switch ep.Company {
	case "", agent.CompanyAnthropic, agent.CompanyOpenAI, agent.CompanyGoogle, agent.CompanyLocal, agent.CompanyStub:
		// routable
	default:
		return spec, fmt.Errorf("endpoint company %q is not routable by the pi harness", ep.Company)
	}
	switch ep.Host {
	case "", agent.HostDirect, agent.HostLocal, agent.HostBedrock, agent.HostVertex:
		// routable
	case agent.HostGateway:
		if !isLoopbackHTTPURL(ep.BaseURL) {
			return spec, fmt.Errorf("gateway endpoint BaseURL %q must be an absolute HTTP(S) URL with a loopback hostname", ep.BaseURL)
		}
	default:
		return spec, fmt.Errorf("serving host %q is not routable by the pi harness", ep.Host)
	}
	switch ep.Protocol {
	case "", agent.ProtoAnthropicMessages, agent.ProtoOpenAIChat, agent.ProtoOpenAIResponses, agent.ProtoGeminiGenerate:
		// drivable
	default:
		return spec, fmt.Errorf("wire protocol %q is not drivable by the pi harness", ep.Protocol)
	}
	if ep.Model != "" {
		spec.Model = ep.Model
	}

	// Merge the cell's resolved credential values onto a COPY of spec.Env and
	// mirror the API key onto PiKeyEnvVar for the pin's env reference.
	// No capacity hint: a Go map grows on demand, so pre-sizing buys nothing
	// here, and summing len()s as an allocation size is exactly the shape a
	// static scanner (go/allocation-size-overflow) flags as a potential overflow.
	env := make(map[string]string)
	for k, v := range spec.Env {
		env[k] = v
	}
	for k, v := range ep.Env {
		if v != "" {
			env[k] = v
		}
	}
	if _, ok := env[PiKeyEnvVar]; !ok {
		if key := pickAPIKey(ep.Env); key != "" {
			env[PiKeyEnvVar] = key
		}
	}
	spec.Env = env
	return spec, nil
}

// isLoopbackHTTPURL reports whether rawURL is an absolute HTTP(S) URL whose
// hostname is localhost or a loopback IP address.
func isLoopbackHTTPURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	hostname := u.Hostname()
	return strings.EqualFold(hostname, "localhost") || net.ParseIP(hostname).IsLoopback()
}

// pickAPIKey selects the cell's API key from the binding env, preferring
// well-known names and falling back to the first non-empty value
// (deterministic via sorted iteration). Mirrors opencode/endpoint.go.
func pickAPIKey(envVals map[string]string) string {
	for _, name := range []string{PiKeyEnvVar, "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "API_KEY"} {
		if v := envVals[name]; v != "" {
			return v
		}
	}
	keys := make([]string, 0, len(envVals))
	for k := range envVals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if envVals[k] != "" {
			return envVals[k]
		}
	}
	return ""
}
