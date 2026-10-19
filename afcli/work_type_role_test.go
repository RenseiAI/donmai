package afcli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runner"
)

func TestWorkTypeRoleWireAndDigest(t *testing.T) {
	t.Parallel()
	var absentDigest string
	for _, role := range []string{"", "inflight", "review", "intake", "terminal", "future"} {
		t.Run(role, func(t *testing.T) {
			original := prompt.QueuedWork{SessionID: "role-wire", WorkType: "chief-of-staff", WorkTypeRole: role}
			raw, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip prompt.QueuedWork
			if err = json.Unmarshal(raw, &roundTrip); err != nil || roundTrip.WorkTypeRole != role {
				t.Fatalf("queued round trip: %v, %q", err, roundTrip.WorkTypeRole)
			}
			if bytes.Contains(raw, []byte(`"workTypeRole"`)) != (role != "") {
				t.Fatal("incorrect role omission")
			}
			// The typed runner projection always includes the profile object.
			raw = append(raw[:len(raw)-1], []byte(`,"resolvedProfile":{}}`)...)
			var item daemon.PollWorkItem
			if err = json.Unmarshal(raw, &item); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			var decoded daemon.PollWorkItem
			if err = json.Unmarshal(encoded, &decoded); err != nil || decoded.WorkTypeRole != role {
				t.Fatalf("poll round trip: %v, %q", err, decoded.WorkTypeRole)
			}
			spec := daemon.PollItemToSessionSpec(decoded, nil)
			detail := daemon.PollItemToSessionDetail(decoded, nil, "", "", "")
			if spec.WorkTypeRole != role || detail.WorkTypeRole != role {
				t.Fatal("role lost at daemon forwarding boundary")
			}
			// Exercise typed forwarding without letting the raw override restore a lost field.
			detail.OperationalPayload = nil
			work, err := detailToQueuedWork(detail)
			if err != nil || work.WorkTypeRole != role {
				t.Fatalf("role lost at runner boundary: %v", err)
			}
			wireDigest, err := executioncell.DigestOperationalPayload(item.OperationalPayload)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := runner.CanonicalOperationalPayload(work)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonical, item.OperationalPayload) {
				t.Fatalf("typed projection differs: %s != %s", canonical, item.OperationalPayload)
			}
			typedDigest, err := runner.DigestOperationalPayload(work)
			if err != nil || typedDigest != wireDigest {
				t.Fatalf("digest mismatch: %v", err)
			}
			if role == "" {
				absentDigest = typedDigest
			} else if typedDigest == absentDigest {
				t.Fatal("role did not change digest")
			}
		})
	}
}

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
