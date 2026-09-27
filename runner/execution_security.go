package runner

import (
	"github.com/RenseiAI/donmai/agent"
)

// executionSecurityReport returns the per-dimension execution-security report
// for the spec about to be spawned, or the typed refusal. A receipt-bearing
// run reports the host-compiled plan's report — the plan binds the stamp in
// its authority digest and the provider's PrepareHarness re-derives the
// report byte-for-byte before spawn. Every other run renders here, with the
// same function the host compiler and PrepareHarness use, so the two lanes
// cannot disagree about what a harness achieves.
func executionSecurityReport(spec agent.Spec, provider agent.Provider, plan *agent.PreparedHarness) (*agent.ExecutionSecurityReport, error) {
	if plan != nil && plan.ExecutionSecurity != nil {
		report := *plan.ExecutionSecurity
		if err := agent.ExecutionSecurityReportMeets(&report, agent.EffectiveExecutionSecurityLevels(spec.ExecutionSecurity)); err != nil {
			return nil, err
		}
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
	return &report, nil
}
