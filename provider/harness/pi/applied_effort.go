package pi

import (
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// appliedEffortFromModelPin reports the effort the headless launch actually
// pinned, for the post-launch applied-effort evidence. It reads the final
// launch configuration's model pin — modelPinArgs' `--model <id>[:<level>]`
// — never Spec.Effort directly, so raw profile input that bypassed
// translation cannot leak into the receipt. Absent pin, absent suffix, or an
// unrecognized suffix reports not-configured (empty) rather than a guessed
// level: only the applied ladder (minimal plus every known EffortLevel) is
// ever reported.
//
// The pin is shared by both headless call sites (rpcArgs) and the
// interactive lane (interactiveArgs in interactive.go), but only the headless
// launchNotices path calls this: interactive callers remain unchanged and are
// never credited as covered by headless tests.
func appliedEffortFromModelPin(spec agent.Spec) agent.EffortLevel {
	args := modelPinArgs(spec)
	for i, a := range args {
		if a != "--model" || i+1 >= len(args) {
			continue
		}
		modelArg := args[i+1]
		idx := strings.LastIndex(modelArg, ":")
		if idx < 0 || idx+1 >= len(modelArg) {
			return ""
		}
		level := agent.EffortLevel(modelArg[idx+1:])
		if level == agent.EffortLevel("minimal") || level.Known() {
			return level
		}
		return ""
	}
	return ""
}

// appliedEffortEvent builds the headless launch's applied-effort notice for
// launchNotices: the translated pin on the new activity key, or an empty
// (not-configured) marker when the launch pinned no level.
func appliedEffortEvent(spec agent.Spec) agent.Event {
	return agent.AppliedReasoningEffortEvent(appliedEffortFromModelPin(spec))
}
