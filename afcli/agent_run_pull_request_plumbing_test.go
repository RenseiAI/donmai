package afcli

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func dispatchedPullRequest() *workarea.PullRequestV1 {
	return &workarea.PullRequestV1{
		Owner: "acme", Repo: "widget", Number: 7,
		URL:     "https://example.test/acme/widget/pull/7",
		HeadSHA: "0123456789abcdef0123456789abcdef01234567",
		HeadRef: "contributor:feature",
		BaseSHA: "89abcdef0123456789abcdef0123456789abcdef",
		BaseRef: "main", Title: "Widen the frobnicator", State: "open",
		LocalRef: "refs/pull/7/head",
	}
}

// TestDispatchedPullRequestSurvivesSessionDetailToRunner covers the last hop
// before provisioning: `donmai agent run` reads the daemon's SessionDetail and
// translates it into the runner's QueuedWork. A record dropped here is silent —
// the session provisions a plain branch clone and the agent reviews the wrong
// code — so both the payload-free and the receipted path are pinned.
func TestDispatchedPullRequestSurvivesSessionDetailToRunner(t *testing.T) {
	want := dispatchedPullRequest()

	queued, err := detailToQueuedWork(&daemon.SessionDetail{SessionID: "sess-1", PullRequest: want})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(queued.PullRequest, want) {
		t.Fatalf("runner projection PullRequest = %+v, want %+v", queued.PullRequest, want)
	}

	payload, err := json.Marshal(map[string]any{"sessionId": "sess-1", "pullRequest": want})
	if err != nil {
		t.Fatal(err)
	}
	queued, err = detailToQueuedWork(&daemon.SessionDetail{
		SessionID: "sess-1", OperationalPayload: payload, PullRequest: want,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(queued.PullRequest, want) {
		t.Fatalf("receipted projection PullRequest = %+v, want %+v", queued.PullRequest, want)
	}
}

// TestDispatchedPullRequestCannotDivergeFromTheReceiptedPayload pins the guard.
// The record decides WHICH COMMITS the workarea materializes, which is exactly
// the intent the receipted operational payload is authoritative for. Were the
// daemon's compatibility mirror allowed to differ, the quiet outcome would be
// the one this whole feature exists to prevent: a session running against a
// head nobody admitted.
func TestDispatchedPullRequestCannotDivergeFromTheReceiptedPayload(t *testing.T) {
	admitted := dispatchedPullRequest()
	payload, err := json.Marshal(map[string]any{"sessionId": "sess-1", "pullRequest": admitted})
	if err != nil {
		t.Fatal(err)
	}

	moved := dispatchedPullRequest()
	moved.HeadSHA = "fedcba9876543210fedcba9876543210fedcba98"
	if _, err := detailToQueuedWork(&daemon.SessionDetail{
		SessionID: "sess-1", OperationalPayload: payload, PullRequest: moved,
	}); err == nil {
		t.Fatal("a mirror naming a different head than the receipted payload was accepted")
	}

	if _, err := detailToQueuedWork(&daemon.SessionDetail{
		SessionID: "sess-1", OperationalPayload: payload,
	}); err == nil {
		t.Fatal("a mirror that dropped the receipted pull request was accepted")
	}
}
