package geminicli

import (
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// buildArgs translates an agent.Spec into the argv array passed to the
// gemini CLI.
//
// The CLI is always invoked in "headless" mode:
//
//	gemini \
//	   --output-format stream-json \
//	   --yolo \
//	   --skip-trust \
//	   -p "" \
//	   [--model <id>]
//
// Flag rationale:
//
//	--output-format stream-json  JSONL event stream on stdout; required
//	                             for the Handle's JSONL reader.
//	--yolo                       Auto-approve all tool invocations so
//	                             the headless loop never stalls on an
//	                             interactive prompt. Equivalent to
//	                             claude's --dangerously-skip-permissions.
//	--skip-trust                 Trust the current workspace for this
//	                             session. Required in unattended
//	                             environments: without it the CLI exits
//	                             with an error when run outside an
//	                             interactively-trusted folder.
//	                             Equivalent: GEMINI_CLI_TRUST_WORKSPACE=true
//	                             env var (we set both for belt-and-suspenders).
//	-p ""                        Headless mode flag; the actual prompt is
//	                             delivered via stdin (gemini appends stdin
//	                             content to the -p value, so "" + stdin
//	                             gives the full prompt). Mirrors the claude
//	                             provider's writePromptStdin pattern.
//	--model <id>                 Pass the resolved model id when non-empty.
//
// Spec fields not honored by this provider (capability-gated drop):
//
//	AllowedTools        — --allowed-tools is deprecated in v0.44+; policy engine only.
//	DisallowedTools     — no CLI equivalent in headless mode.
//	MCPServers          — handled out-of-band via .gemini/settings.json (see settings.go).
//	MaxTurns            — no headless flag; the CLI uses maxSessionTurns in settings.json.
//	Effort              — SupportsReasoningEffort=false; --thinking-budget not exposed headlessly.
//	SystemPromptAppend  — no CLI flag equivalent for headless runs.
//	BaseInstructions    — NeedsBaseInstructions=false.
//	PermissionConfig    — NeedsPermissionConfig=false.
//	SandboxEnabled      — can be added as --sandbox flag; deferred to future wave.
//	SandboxLevel        — same.
//	SubAgentProvider    — no flag; informational only.
//
// The prompt is always delivered via stdin (returned as stdinPrompt) to
// avoid argv-length limits and to keep large prompts off the process
// listing. The -p "" flag puts the CLI into headless mode without
// consuming an argv slot for the prompt text.
func buildArgs(spec agent.Spec) (argv []string, stdinPrompt string) {
	argv = []string{
		"--output-format", "stream-json",
		"--yolo",
		"--skip-trust",
		"-p", "", // headless mode; actual prompt delivered via stdin
	}

	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}

	// Prompt is delivered via stdin to avoid argv-length limits and
	// to keep large prompts off the process listing. Callers wire
	// this into cmd.Stdin in handle.go.
	stdinPrompt = spec.Prompt
	return argv, stdinPrompt
}

// composeEnv builds the child process environment by merging
// parentEnv (typically os.Environ()) with spec.Env and injecting
// GEMINI_CLI_TRUST_WORKSPACE=true as belt-and-suspenders alongside
// --skip-trust. Per the runner contract the spec.Env is pre-filtered
// by AGENT_ENV_BLOCKLIST before Spawn is called.
//
// Order: parentEnv first, then spec.Env entries appended, then the
// trust override; later entries override earlier ones via standard
// exec.Cmd semantics on Unix.
func composeEnv(parentEnv []string, specEnv map[string]string) []string {
	out := make([]string, 0, len(parentEnv)+len(specEnv)+1)
	out = append(out, parentEnv...)
	// Sort spec.Env keys for deterministic order — important for tests.
	keys := make([]string, 0, len(specEnv))
	for k := range specEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+specEnv[k])
	}
	// Belt-and-suspenders workspace trust: the CLI checks this env var
	// before the --skip-trust flag in some code paths.
	out = append(out, "GEMINI_CLI_TRUST_WORKSPACE=true")
	// Suppress update nag banners from stderr.
	// GEMINI_TELEMETRY_ENABLED=false suppresses optional telemetry.
	if !envContainsKey(out, "GEMINI_TELEMETRY_ENABLED") {
		out = append(out, "GEMINI_TELEMETRY_ENABLED=false")
	}
	return out
}

// envContainsKey reports whether the env slice (os.Environ-style "K=V"
// strings) already contains a definition for key.
func envContainsKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
