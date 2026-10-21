package daemon

import "testing"

// TestPollItemToSessionDetail_CarriesTopLevelDurationFallback pins the poll
// boundary wiring for the legacy top-level maxDurationSeconds field: it must
// ride SessionDetail as a fallback carrier without mutating the nested stage
// budget mirror, so runner reconciliation (exact-match against the receipted
// payload) keeps passing and the runner fallback can enforce it afterwards.
func TestPollItemToSessionDetail_CarriesTopLevelDurationFallback(t *testing.T) {
	item := PollWorkItem{
		SessionID:   "sess-top-level",
		MaxDuration: 3600,
	}
	detail := PollItemToSessionDetail(item, nil, "", "", "")
	if detail == nil {
		t.Fatal("PollItemToSessionDetail returned nil")
	}
	if detail.MaxDurationSeconds != 3600 {
		t.Fatalf("detail MaxDurationSeconds = %d, want 3600", detail.MaxDurationSeconds)
	}
	if detail.StageBudget != nil {
		t.Fatalf("detail StageBudget = %+v, want nil (fallback must not synthesize the mirror)", detail.StageBudget)
	}
}

// TestPollItemToSessionDetail_NestedDurationWinsCarrier pins that when both
// sources are present the nested stage budget is carried verbatim and the
// top-level value rides alongside as a fallback carrier only. Precedence
// (nested wins) is decided in the runner, not here.
func TestPollItemToSessionDetail_NestedDurationWinsCarrier(t *testing.T) {
	item := PollWorkItem{
		SessionID:   "sess-both",
		MaxDuration: 900,
		StageBudget: &PollStageBudget{MaxDurationSeconds: 1800, MaxTokens: 1000},
	}
	detail := PollItemToSessionDetail(item, nil, "", "", "")
	if detail == nil {
		t.Fatal("PollItemToSessionDetail returned nil")
	}
	if detail.MaxDurationSeconds != 900 {
		t.Fatalf("detail MaxDurationSeconds = %d, want 900", detail.MaxDurationSeconds)
	}
	if detail.StageBudget == nil || detail.StageBudget.MaxDurationSeconds != 1800 {
		t.Fatalf("detail StageBudget = %+v, want duration 1800", detail.StageBudget)
	}
	if detail.StageBudget.MaxTokens != 1000 {
		t.Fatalf("detail StageBudget tokens = %d, want 1000 (other caps preserved)", detail.StageBudget.MaxTokens)
	}
}

// TestPollItemToSessionDetail_NoDurationStaysAbsent pins the mixed-version-safe
// default: no duration anywhere stays absent on the detail.
func TestPollItemToSessionDetail_NoDurationStaysAbsent(t *testing.T) {
	detail := PollItemToSessionDetail(PollWorkItem{SessionID: "sess-none"}, nil, "", "", "")
	if detail == nil {
		t.Fatal("PollItemToSessionDetail returned nil")
	}
	if detail.MaxDurationSeconds != 0 {
		t.Fatalf("detail MaxDurationSeconds = %d, want 0", detail.MaxDurationSeconds)
	}
	if detail.StageBudget != nil {
		t.Fatalf("detail StageBudget = %+v, want nil", detail.StageBudget)
	}
}
