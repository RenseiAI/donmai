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
