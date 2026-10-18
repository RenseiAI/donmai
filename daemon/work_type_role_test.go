package daemon

import (
	"encoding/json"
	"testing"
)

// TestPollResponse_DecodesWorkTypeRole proves the work-type role survives
// the strict JSON decode of the poll wire shape — the silent-drop
// regression guard (a field on only one struct is dropped by Go's
// decoder). Mirrors TestPollResponse_DecodesLiveWireShape.
func TestPollResponse_DecodesWorkTypeRole(t *testing.T) {
	body := []byte(`{
		"work": [{
			"sessionId": "role-sess-1",
			"workType": "chief-of-staff",
			"workTypeRole": "inflight"
		}]
	}`)

	var resp PollResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode wire shape: %v", err)
	}
	if len(resp.Work) != 1 {
		t.Fatalf("Work len = %d, want 1", len(resp.Work))
	}
	if got := resp.Work[0].WorkType; got != "chief-of-staff" {
		t.Errorf("WorkType = %q, want chief-of-staff", got)
	}
	if got := resp.Work[0].WorkTypeRole; got != "inflight" {
		t.Errorf("WorkTypeRole = %q, want inflight", got)
	}
}

// TestPollItemToSessionDetail_WorkTypeRoleForwarded verifies the role
// survives the PollWorkItem → SessionDetail forwarding step, and that an
// absent role stays absent (mixed-version-safe: an older platform that
// never emits the field behaves exactly as before).
func TestPollItemToSessionDetail_WorkTypeRoleForwarded(t *testing.T) {
	for _, role := range []string{"", "inflight", "review", "intake", "terminal"} {
		t.Run("role="+role, func(t *testing.T) {
			item := PollWorkItem{
				SessionID:    "sess-role",
				WorkType:     "chief-of-staff",
				WorkTypeRole: role,
			}
			detail := PollItemToSessionDetail(item, nil, "", "", "")
			if detail.WorkType != "chief-of-staff" {
				t.Errorf("WorkType = %q, want chief-of-staff", detail.WorkType)
			}
			if detail.WorkTypeRole != role {
				t.Errorf("WorkTypeRole = %q, want %q", detail.WorkTypeRole, role)
			}
		})
	}
}

// TestPollItemToSessionSpec_WorkTypeRoleForwarded verifies the role
// survives the PollWorkItem → SessionSpec forwarding step used by
// embedders driving multi-identity poll loops.
func TestPollItemToSessionSpec_WorkTypeRoleForwarded(t *testing.T) {
	item := PollWorkItem{
		SessionID:    "sess-role",
		WorkType:     "chief-of-staff",
		WorkTypeRole: "review",
	}
	spec := PollItemToSessionSpec(item, nil)
	if spec.WorkTypeRole != "review" {
		t.Errorf("WorkTypeRole = %q, want review", spec.WorkTypeRole)
	}
}
