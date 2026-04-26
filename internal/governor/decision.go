package governor

import (
	"fmt"

	"github.com/RenseiAI/agentfactory-tui/internal/linear"
)

// DispatchDecision is the outcome of evaluating whether a Linear issue should
// be enqueued for automated processing.
type DispatchDecision int

const (
	// DecisionDispatch means the issue should be enqueued.
	DecisionDispatch DispatchDecision = iota

	// DecisionSkip means the issue should NOT be enqueued this cycle.
	DecisionSkip
)

// phase names used in log messages and decision reasons.
const (
	phaseResearch        = "research"
	phaseBacklogCreation = "backlog-creation"
	phaseDevelopment     = "development"
	phaseQA              = "qa"
	phaseAcceptance      = "acceptance"
)

// Decide maps the issue's current state to an automation phase, checks the
// corresponding feature toggle in cfg, and returns either DecisionDispatch or
// DecisionSkip along with a human-readable reason.
//
// TODO: richer decision logic from
// packages/core/src/governor/decision-engine.ts to follow.
func Decide(issue linear.Issue, cfg Config) (DispatchDecision, string) {
	var phase string
	var enabled bool

	switch issue.State.Name {
	case "Triage":
		phase = phaseResearch
		enabled = cfg.AutoResearch

	case "Backlog":
		if issue.Project.Name != "" {
			phase = phaseDevelopment
			enabled = cfg.AutoDevelopment
		} else {
			phase = phaseBacklogCreation
			enabled = cfg.AutoBacklogCreation
		}

	case "Started":
		phase = phaseQA
		enabled = cfg.AutoQA

	case "In Review":
		phase = phaseAcceptance
		enabled = cfg.AutoAcceptance

	default:
		return DecisionSkip, fmt.Sprintf("state %q has no mapped phase", issue.State.Name)
	}

	if !enabled {
		return DecisionSkip, fmt.Sprintf("auto-%s disabled", phase)
	}

	return DecisionDispatch, fmt.Sprintf("phase %s", phase)
}
