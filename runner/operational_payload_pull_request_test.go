package runner

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// TestDispatchedPullRequestIsAdmissionBound pins the record into the canonical
// operational projection. It belongs there for the same reason the rest of the
// workarea intent does: the projection is what producer and verifier digest, so
// a field the runner acts on but the digest does not cover could be changed in
// flight without invalidating the admission — and for this field, changing it
// in flight IS the attack the head verification exists to stop.
func TestDispatchedPullRequestIsAdmissionBound(t *testing.T) {
	base := exactReceiptQueuedWork("operational-pull-request-binding")

	absent, err := CanonicalOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(absent), `"pullRequest"`) {
		t.Fatalf("absent record changed the legacy operational shape: %s", absent)
	}
	withoutRecord, err := DigestOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}

	base.PullRequest = &workarea.PullRequestV1{
		Owner: "acme", Repo: "widget", Number: 7,
		HeadSHA: "0123456789abcdef0123456789abcdef01234567",
		BaseSHA: "89abcdef0123456789abcdef0123456789abcdef",
		BaseRef: "main", LocalRef: "refs/pull/7/head",
	}
	if got := ProjectOperationalPayload(base).PullRequest; got != base.PullRequest {
		t.Fatalf("projected PullRequest = %+v, want the queued record", got)
	}
	withRecord, err := DigestOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	if withRecord == withoutRecord {
		t.Fatal("the dispatched pull request did not change the admission digest")
	}

	// A moved head must be a DIFFERENT admission, not the same one with a
	// different commit in it.
	moved := *base.PullRequest
	moved.HeadSHA = "fedcba9876543210fedcba9876543210fedcba98"
	base.PullRequest = &moved
	movedDigest, err := DigestOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	if movedDigest == withRecord {
		t.Fatal("changing the recorded head did not change the admission digest")
	}
}
