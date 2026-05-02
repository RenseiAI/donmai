package runner

import (
	"strings"
	"testing"
)

// TestWorkTypeStatusMappings_Exhaustive asserts every entry in
// AllWorkTypes has a recorded mapping in BOTH workTypeCompleteStatus
// and workTypeFailStatus. A missing entry would silently no-op the
// post-session block — the platform would never see a transition for
// the missing work type. This test fails loudly when a new work type
// is added to AllWorkTypes without updating the mappings.
func TestWorkTypeStatusMappings_Exhaustive(t *testing.T) {
	for _, wt := range AllWorkTypes {
		if _, ok := workTypeCompleteStatus[wt]; !ok {
			t.Errorf("workTypeCompleteStatus missing entry for %q", wt)
		}
		if _, ok := workTypeFailStatus[wt]; !ok {
			t.Errorf("workTypeFailStatus missing entry for %q", wt)
		}
	}
	// And the reverse — no orphan entries in the maps.
	known := make(map[string]bool, len(AllWorkTypes))
	for _, wt := range AllWorkTypes {
		known[wt] = true
	}
	for wt := range workTypeCompleteStatus {
		if !known[wt] {
			t.Errorf("workTypeCompleteStatus has orphan entry %q (not in AllWorkTypes)", wt)
		}
	}
	for wt := range workTypeFailStatus {
		if !known[wt] {
			t.Errorf("workTypeFailStatus has orphan entry %q (not in AllWorkTypes)", wt)
		}
	}
}

// TestWorkTypeCompleteStatus_KnownValues pins the canonical complete
// targets for the work types where dispatch downstream depends on
// them. Mirrors the legacy TS WORK_TYPE_COMPLETE_STATUS table so a
// silent change here breaks the test before it breaks production.
func TestWorkTypeCompleteStatus_KnownValues(t *testing.T) {
	cases := []struct {
		wt   string
		want string
	}{
		{WorkTypeDevelopmentStr, "Finished"},
		{WorkTypeInflight, "Finished"},
		{WorkTypeQAStr, "Delivered"},
		{WorkTypeAcceptance, "Accepted"},
		{WorkTypeRefinement, "Backlog"},
		{WorkTypeRefinementCoordination, "Backlog"},
		{WorkTypeSecurity, "Finished"},
		{WorkTypeCoordination, "Finished"},
		{WorkTypeInflightCoordination, "Finished"},
		// PM/operational types: no auto-transition.
		{WorkTypeResearch, ""},
		{WorkTypeBacklogCreation, ""},
		{WorkTypeBacklogGroomer, ""},
		{WorkTypeMerge, ""},
		{WorkTypeImprovementLoop, ""},
		{WorkTypeOutcomeAuditor, ""},
		{WorkTypeGAReadiness, ""},
		{WorkTypeDocumentationSteward, ""},
		{WorkTypeOperationalScannerVercel, ""},
		{WorkTypeOperationalScannerAudit, ""},
		{WorkTypeOperationalScannerCI, ""},
	}
	for _, c := range cases {
		got := workTypeCompleteStatus[c.wt]
		if got != c.want {
			t.Errorf("workTypeCompleteStatus[%q] = %q; want %q", c.wt, got, c.want)
		}
	}
}

// TestWorkTypeFailStatus_KnownValues pins the canonical fail targets.
// Only QA + acceptance currently transition on failure (Rejected); the
// rest are no-ops. A new entry here means the agent's failure path
// gained a new Linear-side reaction.
func TestWorkTypeFailStatus_KnownValues(t *testing.T) {
	cases := []struct {
		wt   string
		want string
	}{
		{WorkTypeQAStr, "Rejected"},
		{WorkTypeAcceptance, "Rejected"},
		// Everything else: no transition on failure.
		{WorkTypeDevelopmentStr, ""},
		{WorkTypeInflight, ""},
		{WorkTypeRefinement, ""},
		{WorkTypeRefinementCoordination, ""},
		{WorkTypeMerge, ""},
		{WorkTypeSecurity, ""},
		{WorkTypeResearch, ""},
		{WorkTypeBacklogCreation, ""},
		{WorkTypeCoordination, ""},
		{WorkTypeInflightCoordination, ""},
	}
	for _, c := range cases {
		got := workTypeFailStatus[c.wt]
		if got != c.want {
			t.Errorf("workTypeFailStatus[%q] = %q; want %q", c.wt, got, c.want)
		}
	}
}

// TestIsResultSensitive pins the set of work types whose post-session
// transition is gated on a parsed WORK_RESULT marker. Adding a type to
// the gate or removing one is intentional — assert the canonical set.
func TestIsResultSensitive(t *testing.T) {
	sensitive := []string{
		WorkTypeQAStr,
		WorkTypeAcceptance,
		WorkTypeDevelopmentStr,
		WorkTypeInflight,
		WorkTypeMerge,
		WorkTypeCoordination,
		WorkTypeInflightCoordination,
	}
	notSensitive := []string{
		WorkTypeResearch,
		WorkTypeBacklogCreation,
		WorkTypeBacklogGroomer,
		WorkTypeRefinement,
		WorkTypeRefinementCoordination,
		WorkTypeSecurity,
		WorkTypeImprovementLoop,
		WorkTypeOutcomeAuditor,
		WorkTypeGAReadiness,
		WorkTypeDocumentationSteward,
		WorkTypeOperationalScannerVercel,
		WorkTypeOperationalScannerAudit,
		WorkTypeOperationalScannerCI,
	}
	for _, wt := range sensitive {
		if !isResultSensitive(wt) {
			t.Errorf("isResultSensitive(%q) = false; want true", wt)
		}
	}
	for _, wt := range notSensitive {
		if isResultSensitive(wt) {
			t.Errorf("isResultSensitive(%q) = true; want false", wt)
		}
	}
}

// TestShouldDeferAcceptanceTransition mirrors the TS port at
// orchestrator/dispatcher.ts:74. Only acceptance + merge-queue-enabled
// returns true; every other combination returns false.
func TestShouldDeferAcceptanceTransition(t *testing.T) {
	cases := []struct {
		workType string
		hasMQ    bool
		want     bool
	}{
		{WorkTypeAcceptance, true, true},
		{WorkTypeAcceptance, false, false},
		{WorkTypeQAStr, true, false},
		{WorkTypeDevelopmentStr, true, false},
		{WorkTypeRefinementCoordination, true, false},
		{WorkTypeMerge, true, false},
		{"", true, false},
	}
	for _, c := range cases {
		got := shouldDeferAcceptanceTransition(c.workType, c.hasMQ)
		if got != c.want {
			t.Errorf("shouldDeferAcceptanceTransition(%q,%v) = %v; want %v",
				c.workType, c.hasMQ, got, c.want)
		}
	}
}

// TestResolveTargetStatus_Passed asserts the passed branch promotes
// the issue to the work-type-specific complete status.
func TestResolveTargetStatus_Passed(t *testing.T) {
	cases := []struct {
		workType   string
		wantTarget string
	}{
		{WorkTypeDevelopmentStr, "Finished"},
		{WorkTypeInflight, "Finished"},
		{WorkTypeCoordination, "Finished"},
		{WorkTypeInflightCoordination, "Finished"},
		{WorkTypeQAStr, "Delivered"},
		{WorkTypeAcceptance, "Accepted"},
	}
	for _, c := range cases {
		d := resolveTargetStatus(c.workType, "completed", "passed", false)
		if !d.ShouldTransition {
			t.Errorf("[%s/passed] ShouldTransition=false; want true", c.workType)
		}
		if d.TargetStatus != c.wantTarget {
			t.Errorf("[%s/passed] TargetStatus = %q; want %q", c.workType, d.TargetStatus, c.wantTarget)
		}
		if d.Reason != "passed" {
			t.Errorf("[%s/passed] Reason = %q; want passed", c.workType, d.Reason)
		}
		if d.PostDiagnostic {
			t.Errorf("[%s/passed] PostDiagnostic = true; want false", c.workType)
		}
	}
}

// TestResolveTargetStatus_Failed asserts the failed branch transitions
// QA + acceptance to Rejected and no-ops the others.
func TestResolveTargetStatus_Failed(t *testing.T) {
	cases := []struct {
		workType        string
		wantTarget      string
		wantShouldTrans bool
	}{
		{WorkTypeQAStr, "Rejected", true},
		{WorkTypeAcceptance, "Rejected", true},
		// Development/inflight have no fail target — no transition.
		{WorkTypeDevelopmentStr, "", false},
		{WorkTypeInflight, "", false},
		{WorkTypeCoordination, "", false},
	}
	for _, c := range cases {
		d := resolveTargetStatus(c.workType, "completed", "failed", false)
		if d.ShouldTransition != c.wantShouldTrans {
			t.Errorf("[%s/failed] ShouldTransition = %v; want %v",
				c.workType, d.ShouldTransition, c.wantShouldTrans)
		}
		if d.TargetStatus != c.wantTarget {
			t.Errorf("[%s/failed] TargetStatus = %q; want %q",
				c.workType, d.TargetStatus, c.wantTarget)
		}
	}
}

// TestResolveTargetStatus_Unknown asserts the unknown branch posts a
// diagnostic comment and never transitions, for every result-sensitive
// work type.
func TestResolveTargetStatus_Unknown(t *testing.T) {
	for _, wt := range []string{
		WorkTypeQAStr,
		WorkTypeAcceptance,
		WorkTypeDevelopmentStr,
		WorkTypeInflight,
		WorkTypeMerge,
		WorkTypeCoordination,
		WorkTypeInflightCoordination,
	} {
		d := resolveTargetStatus(wt, "completed", "unknown", false)
		if d.ShouldTransition {
			t.Errorf("[%s/unknown] ShouldTransition = true; want false", wt)
		}
		if !d.PostDiagnostic {
			t.Errorf("[%s/unknown] PostDiagnostic = false; want true", wt)
		}
		if d.Reason != "unknown" {
			t.Errorf("[%s/unknown] Reason = %q; want unknown", wt, d.Reason)
		}
	}
}

// TestResolveTargetStatus_Empty asserts an empty workResult on a
// result-sensitive type follows the unknown path (post diagnostic, no
// transition). This is the "agent exited without emitting the marker"
// path the REN-1467 issue called out.
func TestResolveTargetStatus_Empty(t *testing.T) {
	d := resolveTargetStatus(WorkTypeDevelopmentStr, "completed", "", false)
	if d.ShouldTransition {
		t.Errorf("ShouldTransition = true; want false")
	}
	if !d.PostDiagnostic {
		t.Errorf("PostDiagnostic = false; want true")
	}
	if d.Reason != "unknown" {
		t.Errorf("Reason = %q; want unknown", d.Reason)
	}
}

// TestResolveTargetStatus_AgentFailedTreatedAsFailMarker mirrors the
// TS event-processor.ts:340-344 behaviour: when the runner classifies
// the session as "failed" (crash/error/timeout) on a result-sensitive
// type, the post-session block transitions to the work-type-specific
// fail status as if the marker said "failed".
func TestResolveTargetStatus_AgentFailedTreatedAsFailMarker(t *testing.T) {
	// QA crash → still transitions to Rejected.
	d := resolveTargetStatus(WorkTypeQAStr, "failed", "", false)
	if !d.ShouldTransition {
		t.Errorf("QA failed: ShouldTransition = false; want true")
	}
	if d.TargetStatus != "Rejected" {
		t.Errorf("QA failed: TargetStatus = %q; want Rejected", d.TargetStatus)
	}
	if d.Reason != "agent-failed" {
		t.Errorf("QA failed: Reason = %q; want agent-failed", d.Reason)
	}
}

// TestResolveTargetStatus_NonResultSensitive_PromotesOnComplete
// asserts the event-processor.ts:409-413 fast-path: non-result-
// sensitive work types promote on completion regardless of marker.
func TestResolveTargetStatus_NonResultSensitive_PromotesOnComplete(t *testing.T) {
	// Refinement promotes Rejected → Backlog on completion regardless
	// of WORK_RESULT marker.
	d := resolveTargetStatus(WorkTypeRefinement, "completed", "", false)
	if !d.ShouldTransition {
		t.Errorf("ShouldTransition = false; want true")
	}
	if d.TargetStatus != "Backlog" {
		t.Errorf("TargetStatus = %q; want Backlog", d.TargetStatus)
	}
	if d.Reason != "completed-non-sensitive" {
		t.Errorf("Reason = %q; want completed-non-sensitive", d.Reason)
	}
}

// TestResolveTargetStatus_NonResultSensitive_NoMappingNoOps asserts
// that non-sensitive types with no complete mapping (e.g. research,
// PM agents) do NOT transition — the user moves the issue manually.
func TestResolveTargetStatus_NonResultSensitive_NoMappingNoOps(t *testing.T) {
	d := resolveTargetStatus(WorkTypeResearch, "completed", "", false)
	if d.ShouldTransition {
		t.Errorf("ShouldTransition = true; want false (research has no auto-transition)")
	}
	if d.Reason != "no-mapping" {
		t.Errorf("Reason = %q; want no-mapping", d.Reason)
	}
}

// TestResolveTargetStatus_AcceptancePassed_DefersToMergeQueue covers
// the REN-503/REN-1153 deferral: a passing acceptance with the local
// merge queue configured does NOT transition to Accepted — the merge
// worker drives that after the PR lands.
func TestResolveTargetStatus_AcceptancePassed_DefersToMergeQueue(t *testing.T) {
	d := resolveTargetStatus(WorkTypeAcceptance, "completed", "passed", true)
	if d.ShouldTransition {
		t.Errorf("ShouldTransition = true; want false (deferred to merge queue)")
	}
	if !d.Deferred {
		t.Errorf("Deferred = false; want true")
	}
	if d.Reason != "deferred-merge-queue" {
		t.Errorf("Reason = %q; want deferred-merge-queue", d.Reason)
	}
	if d.PostDiagnostic {
		t.Errorf("PostDiagnostic = true; want false (deferred is silent)")
	}
}

// TestResolveTargetStatus_AcceptancePassed_NoMergeQueueTransitions
// covers the no-adapter path: acceptance passes → Accepted directly.
func TestResolveTargetStatus_AcceptancePassed_NoMergeQueueTransitions(t *testing.T) {
	d := resolveTargetStatus(WorkTypeAcceptance, "completed", "passed", false)
	if !d.ShouldTransition {
		t.Errorf("ShouldTransition = false; want true")
	}
	if d.TargetStatus != "Accepted" {
		t.Errorf("TargetStatus = %q; want Accepted", d.TargetStatus)
	}
	if d.Deferred {
		t.Errorf("Deferred = true; want false")
	}
}

// TestResolveTargetStatus_EmptyWorkType asserts the safety net — an
// empty work type returns a no-op decision with reason "no-work-type".
func TestResolveTargetStatus_EmptyWorkType(t *testing.T) {
	d := resolveTargetStatus("", "completed", "passed", false)
	if d.ShouldTransition || d.PostDiagnostic || d.Deferred {
		t.Errorf("expected no-op decision; got %+v", d)
	}
	if d.Reason != "no-work-type" {
		t.Errorf("Reason = %q; want no-work-type", d.Reason)
	}
}

// TestDiagnosticCommentBody asserts the WORK_RESULT diagnostic comment
// includes both expected marker mentions so operators reading the
// Linear thread know exactly what shape the agent should emit.
func TestDiagnosticCommentBody(t *testing.T) {
	body := diagnosticCommentBody()
	if !strings.Contains(body, "WORK_RESULT:passed") {
		t.Errorf("body missing WORK_RESULT:passed mention")
	}
	if !strings.Contains(body, "WORK_RESULT:failed") {
		t.Errorf("body missing WORK_RESULT:failed mention")
	}
	if !strings.Contains(body, "Issue status was NOT updated") {
		t.Errorf("body missing 'NOT updated' callout")
	}
}
