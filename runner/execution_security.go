package runner

import (
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// executionSecurityReport returns the per-dimension execution-security report
// for the spec about to be spawned, or the typed refusal. It refuses what the
// exact harness cannot render whether or not the work is stamped, but it only
// returns a report for stamped work: work without the section keeps every
// record it produced before the field existed, and its absent report achieves
// exactly index 0.
//
// A receipt-bearing run reports the host-compiled plan's report after
// checking it against the session's own stamp — the plan binds the stamp in
// its authority digest and the provider's PrepareHarness re-derives the
// report byte-for-byte before spawn. Every other run renders here, with the
// same function the host compiler and PrepareHarness use, so the two lanes
// cannot disagree about what a harness achieves.
func executionSecurityReport(spec agent.Spec, provider agent.Provider, plan *agent.PreparedHarness) (*agent.ExecutionSecurityReport, error) {
	if plan != nil {
		if err := agent.ExecutionSecurityReportMeets(plan.ExecutionSecurity, agent.EffectiveExecutionSecurityLevels(spec.ExecutionSecurity)); err != nil {
			return nil, err
		}
		if plan.ExecutionSecurity == nil || spec.ExecutionSecurity == nil {
			return nil, nil
		}
		report := *plan.ExecutionSecurity
		return &report, nil
	}
	var manifest agent.HarnessManifest
	if harness, ok := provider.(agent.HarnessProvider); ok {
		manifest = harness.Manifest()
	}
	report, err := agent.RenderExecutionSecurity(spec, manifest)
	if err != nil {
		return nil, err
	}
	if spec.ExecutionSecurity == nil {
		return nil, nil
	}
	return &report, nil
}

// ValidateExecutionSecurityMember reads a raw queued-work object's
// executionSecurity member with the closed decoder. encoding/json maps an
// explicit null onto an absent pointer, so every lane that builds QueuedWork
// from raw JSON calls this first: a present member — null included — must
// decode, or the work is refused as execution_security_unresolvable.
func ValidateExecutionSecurityMember(raw json.RawMessage) error {
	if _, err := agent.ExecutionSecurityFromOperationalPayload(raw); err != nil {
		return fmt.Errorf("runner: execution security: %w", err)
	}
	return nil
}

// refuseForExecutionSecurity records a pre-spawn execution-security refusal
// on res: a terminal failure with the typed refusal the status post carries.
func refuseForExecutionSecurity(res *Result, err error) {
	res.Status, res.FailureMode, res.Error = "failed", FailureExecutionSecurity, err.Error()
	res.ExecutionSecurityRefusal = agent.ExecutionSecurityRefusalFromError(err)
}
