package pi

import (
	"github.com/RenseiAI/donmai/agent"
)

// appliedEffortFromModelPin reports the effort the headless launch actually
// pinned, for the post-launch applied-effort evidence. It returns the same
// translated value modelPinArgs appended to the final launch configuration's
// `--model <id>[:<level>]` pin (pinnedThinkingLevel) — never Spec.Effort
// directly, and never by reparsing the model-ID text — so raw profile input
// that bypassed translation cannot leak into the receipt, and a
// colon-suffixed model ID with no configured effort (for example a
// tag-style "local-model:high" with omitted effort) reports not-configured
// (empty) rather than a guessed level. Empty means the launch pinned no
// level: only levels the builder actually appended are ever reported.
//
// The pin is shared by both headless call sites (rpcArgs) and the
// interactive lane (interactiveArgs in interactive.go), but only the headless
// launchNotices path calls this: interactive callers remain unchanged and are
// never credited as covered by headless tests.
func appliedEffortFromModelPin(spec agent.Spec) agent.EffortLevel {
	return pinnedThinkingLevel(spec)
}

// appliedEffortEvent builds the headless launch's applied-effort notice for
// launchNotices: the translated pin on the new activity key, or an empty
// (not-configured) marker when the launch pinned no level.
func appliedEffortEvent(spec agent.Spec) agent.Event {
	return agent.AppliedReasoningEffortEvent(appliedEffortFromModelPin(spec))
}
