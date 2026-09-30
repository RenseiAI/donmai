package daemon

import "testing"

func TestAgentCardAnnotationOnRealShimHandle(t *testing.T) {
	f := newShimSpawnFixture(t)
	spec := f.interactiveSpec("card-shim-session")
	spec.AgentCardID = "card-review"
	spec.AgentCardName = "Reviewer"
	handle, err := f.daemon.spawner.AcceptWork(spec)
	if err != nil {
		t.Fatal(err)
	}
	if handle.AgentCardID != "card-review" || handle.AgentCardName != "Reviewer" {
		t.Fatalf("launched shim handle lost explicit card: %+v", handle)
	}
	handles := f.daemon.sessionShimHandles()
	if len(handles) != 1 || handles[0].AgentCardID != "card-review" || handles[0].AgentCardName != "Reviewer" {
		t.Fatalf("shim index lost explicit card: %+v", handles)
	}
}
