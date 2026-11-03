package afcli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/stagewire"
)

// stageWireMembers is each 1.0 session field as a control plane emits it.
var stageWireMembers = map[string]string{
	"stage":          `{"id":"stg_1","key":"backlog","name":"Backlog","role":"intake","runRole":"inflight"}`,
	"card":           `{"id":"card_1","handle":"builder","contract":{"pr":true,"verdict":false,"workResult":true}}`,
	"transitions":    `{"start":{"stageKey":"in-progress","writeStatus":"s-start"},"success":{"stageKey":"review","writeStatus":"s-review"},"failure":"stay","blocked":{"stageKey":"icebox","writeStatus":"s-icebox"},"close":{"stageKey":"closed","writeStatus":"s-closed"}}`,
	"retry":          `{"retries":2,"backoffSeconds":60}`,
	"stageContext":   `{"stage":{"key":"backlog","name":"Backlog","role":"intake"},"sourceStatus":null,"card":{"id":"card_1","handle":"builder"},"reason":"default","parent":{"identifier":"PRJ-1","title":"Parent"},"children":[{"identifier":"PRJ-2","status":"Done"},{"identifier":"PRJ-3","status":null}]}`,
	"dispatchReason": `"mention"`,
}

// TestStageWireRoundTripsEveryHop carries each 1.0 field, alone and all
// together, through the poll decode, the poll item's re-encode, the session
// detail (and its JSON), and the runner work item, and checks the typed
// operational projection still equals the wire's, so the admission digest
// is unchanged by decoding.
func TestStageWireRoundTripsEveryHop(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{"all": nil}
	for member := range stageWireMembers {
		cases[member] = []string{member}
	}
	for name, members := range cases {
		if members == nil {
			for member := range stageWireMembers {
				members = append(members, member)
			}
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := []byte(`{"sessionId":"stage-wire","workType":"development","workTypeRole":"inflight","resolvedProfile":{}`)
			for _, member := range members {
				raw = append(raw, []byte(`,"`+member+`":`+stageWireMembers[member])...)
			}
			raw = append(raw, '}')

			var want stagewire.Fields
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(want, stagewire.Fields{}) {
				t.Fatal("fixture decoded no 1.0 field")
			}

			var item daemon.PollWorkItem
			if err := json.Unmarshal(raw, &item); err != nil {
				t.Fatalf("poll decode: %v", err)
			}
			if !reflect.DeepEqual(item.Fields, want) {
				t.Fatalf("poll item fields = %#v; want %#v", item.Fields, want)
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			var decoded daemon.PollWorkItem
			if err := json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(decoded.Fields, want) {
				t.Fatalf("poll item round trip: %v, %#v", err, decoded.Fields)
			}

			detail := daemon.PollItemToSessionDetail(decoded, nil, "", "", "")
			if !reflect.DeepEqual(detail.Fields, want) {
				t.Fatalf("session detail fields = %#v", detail.Fields)
			}
			// The served detail decodes the same members into the
			// embedded fields: no detail field shadows a 1.0 member.
			var servedDetail daemon.SessionDetail
			if err := json.Unmarshal(raw, &servedDetail); err != nil || !reflect.DeepEqual(servedDetail.Fields, want) {
				t.Fatalf("session detail decode: %v, %#v", err, servedDetail.Fields)
			}

			// The restore branch decodes the receipted projection.
			restored, err := detailToQueuedWork(detail)
			if err != nil || !reflect.DeepEqual(restored.Fields, want) {
				t.Fatalf("runner work item (restore branch): %v, %#v", err, restored.Fields)
			}
			// The typed branch forwards the detail's fields.
			detail.OperationalPayload = nil
			work, err := detailToQueuedWork(detail)
			if err != nil || !reflect.DeepEqual(work.Fields, want) {
				t.Fatalf("runner work item (typed branch): %v, %#v", err, work.Fields)
			}
			canonical, err := runner.CanonicalOperationalPayload(work)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonical, item.OperationalPayload) {
				t.Fatalf("typed projection differs from the wire:\n%s\n%s", canonical, item.OperationalPayload)
			}
			wireDigest, err := executioncell.DigestOperationalPayload(item.OperationalPayload)
			if err != nil {
				t.Fatal(err)
			}
			typedDigest, err := runner.DigestOperationalPayload(work)
			if err != nil || typedDigest != wireDigest {
				t.Fatalf("digest mismatch: %v", err)
			}
			// Projections never alias the work item they came from.
			projected := runner.ProjectOperationalPayload(work)
			if projected.Stage != nil && projected.Stage == work.Stage {
				t.Fatal("projection aliases the stage")
			}
		})
	}
}

// TestStageWireAbsentKeepsLegacyBytes pins the additive rule end to end: a
// legacy poll item carries no 1.0 member through any hop.
func TestStageWireAbsentKeepsLegacyBytes(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"sessionId":"legacy","workType":"qa","workTypeRole":"review","resolvedProfile":{}}`)
	var item daemon.PollWorkItem
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	detail := daemon.PollItemToSessionDetail(item, nil, "", "", "")
	detail.OperationalPayload = nil
	work, err := detailToQueuedWork(detail)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(work.Fields, stagewire.Fields{}) {
		t.Fatalf("legacy work item gained 1.0 fields: %#v", work.Fields)
	}
	canonical, err := runner.CanonicalOperationalPayload(work)
	if err != nil || !bytes.Equal(canonical, item.OperationalPayload) {
		t.Fatalf("legacy projection changed: %v\n%s\n%s", err, canonical, item.OperationalPayload)
	}
	for _, member := range []string{"stage", "card", "transitions", "retry", "stageContext", "dispatchReason", "batchKind"} {
		if bytes.Contains(canonical, []byte(`"`+member+`"`)) {
			t.Fatalf("legacy projection carries %q: %s", member, canonical)
		}
	}
}

// TestStageWireSessionListProjection carries the display labels from the
// daemon's session handle into the public client type.
func TestStageWireSessionListProjection(t *testing.T) {
	t.Parallel()
	handle := daemon.SessionHandle{
		SessionID: "s",
		Stage:     &stagewire.Stage{ID: "stg", Key: "verify", Name: "Verify", Role: stagewire.RoleReview},
		Card:      &stagewire.Card{ID: "card", Handle: "reviewer", Contract: &stagewire.Contract{Verdict: true, WorkResult: true}},
	}
	encoded, err := json.Marshal([]daemon.SessionHandle{handle})
	if err != nil {
		t.Fatal(err)
	}
	var listed []afclient.DaemonSessionHandle
	if err := json.Unmarshal(encoded, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !reflect.DeepEqual(listed[0].Stage, handle.Stage) || !reflect.DeepEqual(listed[0].Card, handle.Card) {
		t.Fatalf("client handle = %+v; want stage and card carried", listed)
	}
}

// TestStageWireLandingBatchKindDecodes pins that a landing trigger carries
// the 1.0 batch discriminant through the poll response decode.
func TestStageWireLandingBatchKindDecodes(t *testing.T) {
	t.Parallel()
	var resp daemon.PollResponse
	raw := `{"work":[],"landingWork":[{"batchJobId":"b","workType":"landing","batchKind":"landing-run","orgId":"o","repoId":"r"}]}`
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.LandingWork) != 1 || resp.LandingWork[0].BatchKind != "landing-run" || resp.LandingWork[0].WorkType != "landing" {
		t.Fatalf("landing work = %+v", resp.LandingWork)
	}
}
