package daemon

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The top-level maxDurationSeconds on the poll wire used to be decoded and
// never read: a populated value took effect nowhere, silently. These tests
// pin the wiring that replaced it — the poll boundary backfills an absent
// nested stageBudget.maxDurationSeconds from the top-level value, so the
// wire means what it says through the enforced stage-budget path.

// TestPollItemToSessionDetail_TopLevelDurationBackfillsAbsentStageBudget
// asserts that a legacy dispatch (no operational payload, no nested budget)
// carrying a top-level duration produces a detail whose StageBudget carries
// that duration — the value the worker's outer timeout and budget enforcer
// derive from.
func TestPollItemToSessionDetail_TopLevelDurationBackfillsAbsentStageBudget(t *testing.T) {
	t.Parallel()

	item := PollWorkItem{
		SessionID:   "sess-top-level-duration",
		ProjectName: "alpha",
		MaxDuration: 3600,
	}
	detail := PollItemToSessionDetail(item, nil, "https://platform.example", "token", "worker-1")

	if detail.StageBudget == nil {
		t.Fatal("StageBudget is nil, want backfill from top-level maxDurationSeconds")
	}
	if detail.StageBudget.MaxDurationSeconds != 3600 {
		t.Errorf("StageBudget.MaxDurationSeconds = %d, want 3600", detail.StageBudget.MaxDurationSeconds)
	}
}

// TestPollItemToSessionDetail_BackfilledBudgetMatchesProjectedPayload
// asserts the reconcile invariant the runner enforces: for legacy work the
// backfilled sibling budget must agree with the stage budget carried inside
// the projected operational payload, or the runner refuses the session as a
// compatibility-mirror mismatch. The backfill runs before projection, so the
// payload carries the backfilled value by construction.
func TestPollItemToSessionDetail_BackfilledBudgetMatchesProjectedPayload(t *testing.T) {
	t.Parallel()

	item := PollWorkItem{
		SessionID:   "sess-backfill-payload-agree",
		ProjectName: "alpha",
		MaxDuration: 3600,
	}
	detail := PollItemToSessionDetail(item, nil, "https://platform.example", "token", "worker-1")

	if len(detail.OperationalPayload) == 0 {
		t.Fatal("OperationalPayload is empty, want projected payload carrying the backfill")
	}
	var payload struct {
		StageBudget *PollStageBudget `json:"stageBudget"`
	}
	if err := json.Unmarshal(detail.OperationalPayload, &payload); err != nil {
		t.Fatalf("decode projected payload: %v", err)
	}
	if !reflect.DeepEqual(payload.StageBudget, detail.StageBudget) {
		t.Errorf("payload stageBudget = %+v, sibling = %+v, want agreement", payload.StageBudget, detail.StageBudget)
	}
}

// TestPollItemToSessionDetail_TopLevelDurationPreservesSiblingCaps asserts
// that backfilling into a present-but-uncapped nested budget keeps the other
// caps: only the absent duration is filled, sub-agent and token caps ride
// through untouched.
func TestPollItemToSessionDetail_TopLevelDurationPreservesSiblingCaps(t *testing.T) {
	t.Parallel()

	item := PollWorkItem{
		SessionID:   "sess-backfill-preserves-caps",
		ProjectName: "alpha",
		MaxDuration: 3600,
		StageBudget: &PollStageBudget{MaxSubAgents: ptrMaxSubAgents(2), MaxTokens: 10_000},
	}
	detail := PollItemToSessionDetail(item, nil, "https://platform.example", "token", "worker-1")

	want := &PollStageBudget{MaxDurationSeconds: 3600, MaxSubAgents: ptrMaxSubAgents(2), MaxTokens: 10_000}
	if !reflect.DeepEqual(detail.StageBudget, want) {
		t.Errorf("StageBudget = %+v, want %+v", detail.StageBudget, want)
	}
}

// TestPollItemToSessionDetail_ExplicitNestedDurationWins asserts that an
// explicitly stamped nested duration is never overridden by the top-level
// value: the nested stage-instance cap is the more specific authority.
func TestPollItemToSessionDetail_ExplicitNestedDurationWins(t *testing.T) {
	t.Parallel()

	item := PollWorkItem{
		SessionID:   "sess-nested-wins",
		ProjectName: "alpha",
		MaxDuration: 3600,
		StageBudget: &PollStageBudget{MaxDurationSeconds: 7200},
	}
	detail := PollItemToSessionDetail(item, nil, "https://platform.example", "token", "worker-1")

	if detail.StageBudget == nil || detail.StageBudget.MaxDurationSeconds != 7200 {
		t.Errorf("StageBudget = %+v, want nested duration 7200 to win over top-level 3600", detail.StageBudget)
	}
}

// TestPollItemToSessionDetail_ReceiptedPayloadSkipsBackfill asserts that
// receipt-bearing work is untouched: synthesising a sibling budget the
// operational payload does not carry would trip the runner's
// compatibility-mirror reconciliation and refuse an authoritative session.
func TestPollItemToSessionDetail_ReceiptedPayloadSkipsBackfill(t *testing.T) {
	t.Parallel()

	item := PollWorkItem{
		SessionID:          "sess-receipted-untouched",
		ProjectName:        "alpha",
		MaxDuration:        3600,
		OperationalPayload: json.RawMessage(`{"sessionId":"sess-receipted-untouched"}`),
	}
	detail := PollItemToSessionDetail(item, nil, "https://platform.example", "token", "worker-1")

	if detail.StageBudget != nil {
		t.Errorf("StageBudget = %+v, want nil (receipted payload without a nested budget stays unbudgeted)", detail.StageBudget)
	}
}

// TestPollItemToSessionDetail_NoDurationLeavesBudgetAbsent asserts the
// zero-state: a dispatch with neither a top-level duration nor a nested
// budget still yields no budget — the backfill never invents authority.
func TestPollItemToSessionDetail_NoDurationLeavesBudgetAbsent(t *testing.T) {
	t.Parallel()

	item := PollWorkItem{
		SessionID:   "sess-no-budget",
		ProjectName: "alpha",
	}
	detail := PollItemToSessionDetail(item, nil, "https://platform.example", "token", "worker-1")

	if detail.StageBudget != nil {
		t.Errorf("StageBudget = %+v, want nil when neither top-level nor nested duration is stamped", detail.StageBudget)
	}
}
