package daemon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// TestPullRequestRecordSurvivesThePollWire pins the two hops a dispatched
// pull-request record has to survive between the platform and the runner, and
// the exact JSON spelling at each.
//
// The daemon does NOT forward the platform's work item verbatim:
// PollItemToSessionDetail rebuilds SessionDetail field by field, so a new
// optional field is dropped on the floor unless it is mirrored there. That is
// the failure this test exists to catch — it is silent, and it degrades to
// "the agent reviewed the branch instead of the pull request" rather than to
// an error.
func TestPullRequestRecordSurvivesThePollWire(t *testing.T) {
	t.Parallel()

	const wire = `{
	  "sessionId": "sess-1",
	  "repository": "https://example.test/acme/widget.git",
	  "pullRequest": {
	    "owner": "acme",
	    "repo": "widget",
	    "number": 7,
	    "url": "https://example.test/acme/widget/pull/7",
	    "headSha": "0123456789abcdef0123456789abcdef01234567",
	    "headRef": "contributor:feature",
	    "baseSha": "89abcdef0123456789abcdef0123456789abcdef",
	    "baseRef": "main",
	    "title": "Widen the frobnicator",
	    "state": "open",
	    "localRef": "refs/pull/7/head"
	  }
	}`

	var item PollWorkItem
	if err := json.Unmarshal([]byte(wire), &item); err != nil {
		t.Fatalf("decode poll work item: %v", err)
	}
	want := &workarea.PullRequestV1{
		Owner: "acme", Repo: "widget", Number: 7,
		URL:      "https://example.test/acme/widget/pull/7",
		HeadSHA:  "0123456789abcdef0123456789abcdef01234567",
		HeadRef:  "contributor:feature",
		BaseSHA:  "89abcdef0123456789abcdef0123456789abcdef",
		BaseRef:  "main",
		Title:    "Widen the frobnicator",
		State:    "open",
		LocalRef: "refs/pull/7/head",
	}
	if !reflect.DeepEqual(item.PullRequest, want) {
		t.Fatalf("PollWorkItem.PullRequest = %+v, want %+v", item.PullRequest, want)
	}

	detail := PollItemToSessionDetail(item, nil, "https://platform.test", "token", "worker-1")
	if !reflect.DeepEqual(detail.PullRequest, want) {
		t.Fatalf("SessionDetail.PullRequest = %+v, want %+v", detail.PullRequest, want)
	}

	// The runner reads the detail back off the daemon's local HTTP API, so the
	// re-encoded spelling is part of the contract, not an implementation
	// detail. The detail itself is not marshalled here — it carries the
	// worker's auth token, and a test has no business writing that anywhere —
	// so the struct tag is asserted directly and the record is round-tripped
	// on its own.
	field, ok := reflect.TypeOf(SessionDetail{}).FieldByName("PullRequest")
	if !ok {
		t.Fatal("SessionDetail has no PullRequest field")
	}
	if got := field.Tag.Get("json"); got != "pullRequest,omitempty" {
		t.Errorf("SessionDetail.PullRequest json tag = %q, want pullRequest,omitempty", got)
	}
	encoded, err := json.Marshal(detail.PullRequest)
	if err != nil {
		t.Fatalf("encode pull request record: %v", err)
	}
	for _, key := range []string{
		`"owner":`, `"repo":`, `"number":7`, `"url":`,
		`"headSha":`, `"headRef":`, `"baseSha":`, `"baseRef":`,
		`"title":`, `"state":`, `"localRef":`,
	} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("re-encoded pull request record is missing %s", key)
		}
	}
}

// TestPullRequestRecordAbsentIsAbsent pins the tolerance half: a work item from
// a platform that never heard of pull requests carries no record, and the
// detail the runner reads back carries no "pullRequest" key at all — so a
// mixed-version daemon and platform stay byte-compatible.
func TestPullRequestRecordAbsentIsAbsent(t *testing.T) {
	t.Parallel()

	var item PollWorkItem
	if err := json.Unmarshal([]byte(`{"sessionId":"sess-2","repository":"https://example.test/a/b.git"}`), &item); err != nil {
		t.Fatalf("decode poll work item: %v", err)
	}
	if item.PullRequest != nil {
		t.Fatalf("PollWorkItem.PullRequest = %+v, want nil", item.PullRequest)
	}
	detail := PollItemToSessionDetail(item, nil, "https://platform.test", "token", "worker-1")
	if detail.PullRequest != nil {
		t.Fatalf("SessionDetail.PullRequest = %+v, want nil", detail.PullRequest)
	}
	// omitempty is what keeps the key off the wire entirely for the vastly
	// more common branch dispatch, so a platform that never heard of pull
	// requests sees byte-identical details.
	field, ok := reflect.TypeOf(SessionDetail{}).FieldByName("PullRequest")
	if !ok {
		t.Fatal("SessionDetail has no PullRequest field")
	}
	if !strings.Contains(field.Tag.Get("json"), ",omitempty") {
		t.Errorf("SessionDetail.PullRequest json tag = %q, want an omitempty tag", field.Tag.Get("json"))
	}
	encoded, err := json.Marshal(struct {
		PullRequest *workarea.PullRequestV1 `json:"pullRequest,omitempty"`
	}{detail.PullRequest})
	if err != nil {
		t.Fatalf("encode absent record: %v", err)
	}
	if string(encoded) != "{}" {
		t.Errorf("absent record encoded to %s, want {}", encoded)
	}
}
