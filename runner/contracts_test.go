package runner

import "testing"

// TestGetCompletionContract_KnownTypes asserts the contract lookup
// returns the expected required fields for the canonical work types.
// Pinned values mirror the per-workType contract from
// completion-contracts.ts after the REN-1467 update (development /
// inflight / coordination / inflight-coordination now require
// FieldWorkResult).
func TestGetCompletionContract_KnownTypes(t *testing.T) {
	cases := []struct {
		workType     string
		wantRequired []CompletionFieldType
	}{
		{
			workType:     WorkTypeDevelopmentStr,
			wantRequired: []CompletionFieldType{FieldCommitsPresent, FieldBranchPushed, FieldPRURL, FieldWorkResult},
		},
		{
			workType:     WorkTypeInflight,
			wantRequired: []CompletionFieldType{FieldCommitsPresent, FieldBranchPushed, FieldPRURL, FieldWorkResult},
		},
		{
			workType:     WorkTypeCoordination,
			wantRequired: []CompletionFieldType{FieldWorkResult, FieldCommentPosted},
		},
		{
			workType:     WorkTypeInflightCoordination,
			wantRequired: []CompletionFieldType{FieldWorkResult, FieldCommentPosted},
		},
		{
			workType:     WorkTypeQAStr,
			wantRequired: []CompletionFieldType{FieldWorkResult, FieldCommentPosted},
		},
		{
			workType:     WorkTypeAcceptance,
			wantRequired: []CompletionFieldType{FieldWorkResult, FieldPRMergedOrEnqueued},
		},
		{
			workType:     WorkTypeMerge,
			wantRequired: []CompletionFieldType{FieldPRMerged},
		},
		{
			workType:     WorkTypeBacklogCreation,
			wantRequired: []CompletionFieldType{FieldSubIssuesCreated},
		},
		{
			workType:     WorkTypeRefinement,
			wantRequired: []CompletionFieldType{FieldCommentPosted},
		},
	}
	for _, c := range cases {
		contract, ok := GetCompletionContract(c.workType)
		if !ok {
			t.Errorf("GetCompletionContract(%q) = !ok; want ok", c.workType)
			continue
		}
		if len(contract.Required) != len(c.wantRequired) {
			t.Errorf("[%s] Required len = %d; want %d (got %v)",
				c.workType, len(contract.Required), len(c.wantRequired), contract.Required)
			continue
		}
		got := make(map[CompletionFieldType]bool, len(contract.Required))
		for _, f := range contract.Required {
			got[f.Type] = true
		}
		for _, want := range c.wantRequired {
			if !got[want] {
				t.Errorf("[%s] missing required field %q", c.workType, want)
			}
		}
	}
}

// TestGetCompletionContract_UnknownReturnsFalse asserts the lookup
// returns ok=false for a work type the table doesn't know about.
// Callers treat this as "no contract" (best-effort completion).
func TestGetCompletionContract_UnknownReturnsFalse(t *testing.T) {
	if _, ok := GetCompletionContract("imaginary-future-type"); ok {
		t.Errorf("GetCompletionContract(unknown) = ok; want !ok")
	}
}

// TestRequiresWorkResult_DevAndCoord asserts development /
// inflight / coordination / inflight-coordination + qa + acceptance
// all require WORK_RESULT — the REN-1467 acceptance criterion this
// guards.
func TestRequiresWorkResult_DevAndCoord(t *testing.T) {
	requiresMarker := []string{
		WorkTypeDevelopmentStr,
		WorkTypeInflight,
		WorkTypeCoordination,
		WorkTypeInflightCoordination,
		WorkTypeQAStr,
		WorkTypeAcceptance,
	}
	for _, wt := range requiresMarker {
		if !RequiresWorkResult(wt) {
			t.Errorf("RequiresWorkResult(%q) = false; want true", wt)
		}
	}
}

// TestRequiresWorkResult_NonResultSensitive asserts non-result-
// sensitive types do NOT require a WORK_RESULT marker.
func TestRequiresWorkResult_NonResultSensitive(t *testing.T) {
	for _, wt := range []string{
		WorkTypeResearch,
		WorkTypeBacklogCreation,
		WorkTypeRefinement,
		WorkTypeMerge,
		WorkTypeRefinementCoordination,
	} {
		if RequiresWorkResult(wt) {
			t.Errorf("RequiresWorkResult(%q) = true; want false", wt)
		}
	}
}

// TestRequiresWorkResult_UnknownTypeFalse asserts an unknown work
// type returns false (no contract → no marker requirement).
func TestRequiresWorkResult_UnknownTypeFalse(t *testing.T) {
	if RequiresWorkResult("imaginary-type") {
		t.Errorf("RequiresWorkResult(unknown) = true; want false")
	}
}

// TestCompletionFieldsBackstopCapability pins the backstop-capable
// flag on each field. Backstop-capable fields are auto-recovered by
// runner/backstop.go; not-backstop-capable fields surface as a
// diagnostic comment to Linear (the orchestrator can't synthesise an
// agent's pass/fail verdict).
func TestCompletionFieldsBackstopCapability(t *testing.T) {
	cases := []struct {
		field        CompletionField
		wantBackstop bool
	}{
		{fieldPRURL, true},
		{fieldBranchPushed, true},
		{fieldCommitsPresent, true},
		{fieldWorkResult, false},
		{fieldIssueUpdated, false},
		{fieldCommentPosted, false},
		{fieldSubIssuesCreated, false},
		{fieldPRMerged, false},
		{fieldPRMergedOrEnqueued, false},
	}
	for _, c := range cases {
		if c.field.BackstopCapable != c.wantBackstop {
			t.Errorf("%s BackstopCapable = %v; want %v",
				c.field.Type, c.field.BackstopCapable, c.wantBackstop)
		}
	}
}
