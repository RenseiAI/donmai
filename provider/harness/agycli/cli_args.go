package agycli

import (
	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// Result-envelope markers. The provider appends an instruction asking `agy`
// to print its final result between these markers; the stdout reader extracts
// the JSON in between as the clean final result. Probe-confirmed that `agy`
// reproduces the markers verbatim (CONTRACT.md §2).
const (
	resultEnvelopeBegin = "<<<DONMAI_RESULT>>>"
	resultEnvelopeEnd   = "<<<END_DONMAI_RESULT>>>"
)

// resultEnvelopeInstruction is appended to the prompt when
// Options.InjectResultEnvelope is enabled. It is intentionally explicit about
// the exact marker lines so the parser in envelope.go can find them. It does
// NOT replace the platform's own WORK_RESULT marker convention — the two
// coexist; the runner scans assistant text for WORK_RESULT independently.
const resultEnvelopeInstruction = "\n\n" +
	"---\n" +
	"When you have completed the task, end your response by printing your final " +
	"result as a single JSON object on its own lines, wrapped EXACTLY by these two " +
	"marker lines (and put nothing else on the marker lines):\n" +
	resultEnvelopeBegin + "\n" +
	`{"status": "passed" or "failed", "summary": "<one concise sentence>"}` + "\n" +
	resultEnvelopeEnd + "\n"

// buildArgs translates an agent.Spec into the argv passed to `agy`.
//
// The CLI is always invoked headless:
//
//	agy -p "<prompt>" --dangerously-skip-permissions
//
// Flag rationale (CONTRACT.md §1):
//
//	-p "<prompt>"                   Headless single-shot. The prompt is the
//	                                flag VALUE (not stdin — agy hangs on a
//	                                piped stdin and has no stdin-prompt mode).
//	--dangerously-skip-permissions  Auto-approve all tool permission requests
//	                                so the unattended loop never stalls.
//	                                Equivalent to claude's flag of the same
//	                                name / gemini's --yolo.
//
// Spec fields NOT honored (capability-gated drop, CONTRACT.md §4/§5):
//
//	Model               — no --model flag; model is a persisted agy setting.
//	MCPServers          — global-only mcpServers; deferred.
//	AllowedTools/Disallowed/MaxTurns/Effort/PermissionConfig — no headless flag.
//
// The full prompt (with the optional result-envelope instruction) is returned
// as the -p value. agy has no stdin-prompt mode; the prompt is always a flag value.
func buildArgs(spec agent.Spec, injectEnvelope bool) []string {
	prompt := buildPrompt(spec.Prompt, injectEnvelope)
	return []string{
		"-p", prompt,
		"--dangerously-skip-permissions",
	}
}

// buildPrompt returns the prompt to hand to `agy -p`, optionally appending the
// result-envelope instruction. The envelope is harness-owned: caller text that
// happens to contain its marker is data, never an opt-out of injection.
func buildPrompt(prompt string, injectEnvelope bool) string {
	if !injectEnvelope {
		return prompt
	}
	return prompt + resultEnvelopeInstruction
}

// composeEnv builds the child environment by merging parentEnv (typically
// os.Environ()) with spec.Env. No API key is injected — `agy` authenticates via
// its own host OAuth. spec.Env keys are sorted for deterministic test output.
// Runner-only attach controls are removed from both layers even when this
// provider is used directly outside the runner.
func composeEnv(parentEnv []string, specEnv map[string]string) []string {
	return runtimeenv.ComposeChildEnv(parentEnv, specEnv)
}
