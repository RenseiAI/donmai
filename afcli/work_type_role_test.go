package afcli

import (
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

// TestDetailToQueuedWork_ForwardsWorkTypeRole closes the work-type-role
// wire: SessionDetail → runner.QueuedWork. Both branches are asserted
// because the OperationalPayload branch re-decodes the whole struct from
// the canonical projection — if the projection ever carries the role,
// the restore must not drop it; if it does not, the literal branch must
// still forward the detail's value.
func TestDetailToQueuedWork_ForwardsWorkTypeRole(t *testing.T) {
	// In the restore branch the canonical projection is authoritative:
	// the payload itself must carry the name and role (as a real
	// dispatcher emit does), because absent receipted fields stay absent
	// rather than inheriting the compatibility mirror.
	cases := []struct {
		name               string
		operationalPayload []byte
	}{
		{"no operational payload", nil},
		{"operational payload restore branch", []byte(`{"sessionId":"sess_1","projectId":"proj_1","workType":"chief-of-staff","workTypeRole":"inflight"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &daemon.SessionDetail{
				SessionID:          "sess_1",
				WorkerID:           "worker_1",
				PlatformURL:        "https://platform.example.com",
				AuthToken:          "worker-runtime-bearer",
				WorkType:           "chief-of-staff",
				WorkTypeRole:       "inflight",
				OperationalPayload: tc.operationalPayload,
				ResolvedProfile:    &daemon.SessionResolvedProfile{Provider: "stub"},
			}
			qw, err := detailToQueuedWork(d)
			if err != nil {
				t.Fatalf("detailToQueuedWork: %v", err)
			}
			if qw.WorkType != "chief-of-staff" {
				t.Errorf("WorkType = %q, want chief-of-staff", qw.WorkType)
			}
			if qw.WorkTypeRole != "inflight" {
				t.Errorf("WorkTypeRole = %q, want inflight", qw.WorkTypeRole)
			}
		})
	}
}
