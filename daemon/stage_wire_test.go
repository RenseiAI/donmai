package daemon

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/stagewire"
)

// TestPollWorkItemStageWireDecodeIsBounded pins that the poll boundary
// applies the 1.0 field bounds: an unknown role is dropped, the rest of the
// stage survives, and the detail carries the bounded value.
func TestPollWorkItemStageWireDecodeIsBounded(t *testing.T) {
	t.Parallel()
	raw := `{"sessionId":"s","stage":{"key":"verify","role":"supervisor","runRole":"review"},"dispatchReason":"psychic","retry":{"retries":50,"backoffSeconds":-1}}`
	var item PollWorkItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatal(err)
	}
	if item.Stage == nil || item.Stage.Key != "verify" || item.Stage.Role != "" || item.Stage.RunRole != stagewire.RoleReview {
		t.Fatalf("stage = %+v; want the unknown role dropped and the rest kept", item.Stage)
	}
	if item.DispatchReason != "" || *item.Retry != (stagewire.Retry{Retries: stagewire.MaxRetries}) {
		t.Fatalf("dispatchReason %q retry %+v; want dropped and clamped", item.DispatchReason, *item.Retry)
	}
	detail := PollItemToSessionDetail(item, nil, "", "", "")
	if detail.Stage == item.Stage || *detail.Stage != *item.Stage {
		t.Fatal("session detail must carry an equal, unaliased stage")
	}
}

// TestProjectSessionLabels pins the session list's display projection: the
// stage and card come from the stored detail, copied, and a handle that
// already carries its labels never reads the detail.
func TestProjectSessionLabels(t *testing.T) {
	t.Parallel()
	stored := &SessionDetail{IssueIdentifier: "PRJ-1"}
	stored.Stage = &stagewire.Stage{Key: "verify", Role: stagewire.RoleReview}
	stored.Card = &stagewire.Card{ID: "card", Handle: "reviewer"}
	reads := 0
	detailOf := func(id string) (*SessionDetail, bool) {
		reads++
		return stored, id == "s"
	}

	handle := SessionHandle{SessionID: "s"}
	projectSessionLabels(&handle, detailOf)
	if handle.IssueIdentifier != "PRJ-1" || handle.Stage == nil || handle.Stage.Key != "verify" || handle.Card == nil || handle.Card.Handle != "reviewer" {
		t.Fatalf("handle = %+v; want identifier, stage and card projected", handle)
	}
	if handle.Stage == stored.Stage || handle.Card == stored.Card {
		t.Fatal("projection aliases the stored detail")
	}

	reads = 0
	projectSessionLabels(&handle, detailOf)
	if reads != 0 {
		t.Fatalf("a fully labelled handle read the detail %d times", reads)
	}

	legacy := SessionHandle{SessionID: "other"}
	projectSessionLabels(&legacy, detailOf)
	if legacy.Stage != nil || legacy.Card != nil || legacy.IssueIdentifier != "" {
		t.Fatalf("missing detail produced labels: %+v", legacy)
	}
}
