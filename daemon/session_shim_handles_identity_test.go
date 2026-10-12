package daemon

import (
	"context"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestAdoptedHandleCarriesSessionIdentityAfterRestart pins the restart
// adoption contract: a session launched under shim ownership, then adopted
// by a replacement daemon, keeps its admitted Repository, ProjectName and
// Harness on the operator-facing handle. Before the session sidecar, the
// re-adopted handle carried none of them and a repo-scoped reader dropped
// exactly the interactive sessions every restart re-adopts.
func TestAdoptedHandleCarriesSessionIdentityAfterRestart(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-restart-identity"
	sessionID := "lineage-restarted"
	spec := f.interactiveSpec(sessionID)
	spec.OrganizationID = orgID
	spec.ProjectName = "p1"
	spec.Harness = "stub-harness"
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("launch %s: %v", sessionID, err)
	}
	f.daemon.ReleaseAdoptedSessionShims()

	replacement := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			RegistryDir:    f.registry,
			HostID:         "host-restart-identity",
			OrgID:          orgID,
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("restart-identity-batch")}, nil
			},
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error {
				return nil
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup adoption: %v", err)
	}

	handles := replacement.sessionShimHandles()
	if len(handles) != 1 {
		t.Fatalf("sessionShimHandles = %+v, want the one re-adopted session", handles)
	}
	got := handles[0]
	if got.SessionID != spec.SessionID {
		t.Fatalf("handle SessionID = %q, want %q", got.SessionID, spec.SessionID)
	}
	if got.Repository == "" {
		t.Errorf("re-adopted handle Repository is empty; a repo-scoped reader drops this session")
	}
	if got.ProjectName == "" {
		t.Errorf("re-adopted handle ProjectName is empty; a project-scoped reader drops this session")
	}
	if got.Harness == "" {
		t.Errorf("re-adopted handle Harness is empty")
	}
	if want := spec.Repository; got.Repository != want {
		t.Errorf("re-adopted handle Repository = %q, want the launch-time %q", got.Repository, want)
	}
	if want := spec.ProjectName; want != "" && got.ProjectName != want {
		t.Errorf("re-adopted handle ProjectName = %q, want the launch-time %q", got.ProjectName, want)
	}
}

// TestQuarantinedHandleCarriesSessionIdentity pins the same contract for the
// quarantined population: a quarantined row is a running harness an operator
// still needs scoped, so it carries the sidecar identity too. It builds the
// quarantined row directly against a registry holding the launch's sidecar,
// the same shape a startup pass produces when adoption is refused.
func TestQuarantinedHandleCarriesSessionIdentity(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-quarantine-identity"
	spec := launchOneAdoptableLineage(t, f, orgID, "lineage-quarantined")
	f.daemon.ReleaseAdoptedSessionShims()

	reg, err := sessionshim.NewRegistry(f.registry)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	// The launch wrote the sidecar at process epoch 1; confirm it is there
	// so this test pins the wire it reads, not an absence fallback.
	id := sessionshim.Identity{OrgID: orgID, SessionID: spec.SessionID}
	if _, found, err := reg.SessionSidecar(id, 1); err != nil || !found {
		t.Fatalf("SessionSidecar = found %v, err %v; want the launch's sidecar", found, err)
	}

	d := New(Options{SkipRegistration: true})
	t.Cleanup(d.ReleaseAdoptedSessionShims)
	d.shims.mu.Lock()
	d.shims.registry = reg
	d.shims.quarantined = []sessionshim.QuarantinedSession{{
		OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-quarantined", ProcessEpoch: 1,
		Reason: sessionshim.QuarantineSocketUnreachable, ConsumesCapacity: true,
	}}
	d.shims.mu.Unlock()

	handles := d.sessionShimHandles()
	if len(handles) != 1 {
		t.Fatalf("sessionShimHandles = %+v, want the one quarantined session", handles)
	}
	got := handles[0]
	if got.Repository == "" {
		t.Errorf("quarantined handle Repository is empty; a repo-scoped reader drops this session")
	}
	if want := spec.Repository; got.Repository != want {
		t.Errorf("quarantined handle Repository = %q, want the launch-time %q", got.Repository, want)
	}
}

// TestActiveSessionsDedupesBySessionID pins the list contract: one session
// appears exactly once even when the spawner, the adopted set and the
// quarantine each name it. The adopted handle wins over the quarantined row
// for the same session, so the list never shows a live session twice and
// never prefers the row with no controller.
func TestActiveSessionsDedupesBySessionID(t *testing.T) {
	d := New(Options{SkipRegistration: true})
	d.spawner = NewWorkerSpawner(SpawnerOptions{MaxConcurrentSessions: 8})
	d.spawner.Resume()
	d.spawner.sessions["dup"] = &spawnedSession{
		handle: SessionHandle{SessionID: "dup", State: SessionRunning, Repository: "direct"},
		spec:   SessionSpec{SessionID: "dup"},
	}
	id := sessionshim.Identity{OrgID: "org-dedupe", SessionID: "dup"}
	d.shims.mu.Lock()
	d.shims.adopted[id] = adoptedShim{
		shimID: "shim-adopted",
		handle: SessionHandle{SessionID: "dup", State: SessionRunning, Repository: "adopted", ProjectName: "p", Harness: "h"},
	}
	d.shims.quarantined = []sessionshim.QuarantinedSession{{
		OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-quarantined", ProcessEpoch: 1,
		Reason: sessionshim.QuarantineSocketUnreachable, ConsumesCapacity: true,
	}}
	d.shims.mu.Unlock()

	got := d.ActiveSessions()
	count := 0
	for _, h := range got {
		if h.SessionID == "dup" {
			count++
			if h.Repository != "direct" {
				t.Errorf("deduped handle Repository = %q, want the direct child's (first row wins)", h.Repository)
			}
		}
	}
	if count != 1 {
		t.Fatalf("ActiveSessions lists session %q %d times (%+v), want exactly once", "dup", count, got)
	}
}

// TestSpawnerSeesShimOwnedSessionIDs pins the admission half of the dedupe:
// the spawner's external session ids include adopted and quarantined shim
// sessions, so re-dispatching a session id already live under a shim is
// refused (and capacity-charged) rather than accepted a second time.
func TestSpawnerSeesShimOwnedSessionIDs(t *testing.T) {
	d := New(Options{SkipRegistration: true})
	d.spawner = NewWorkerSpawner(SpawnerOptions{MaxConcurrentSessions: 8})
	d.spawner.Resume()
	d.spawner.opts.ExternalSessionIDs = func() []string { return d.shimOwnedSessionIDs() }
	id := sessionshim.Identity{OrgID: "org-guard", SessionID: "guarded"}
	d.shims.mu.Lock()
	d.shims.adopted[id] = adoptedShim{shimID: "shim-adopted"}
	d.shims.quarantined = []sessionshim.QuarantinedSession{{
			OrgID: "org-guard", SessionID: "held",
			ShimID: "shim-quarantined", ProcessEpoch: 1,
			Reason: sessionshim.QuarantineSocketUnreachable, ConsumesCapacity: true,
		}}
	d.shims.mu.Unlock()
	d.spawner.mu.Lock()
	count := d.spawner.externalSessionOccupancyLocked()
	d.spawner.mu.Unlock()
	if count != 2 {
		t.Fatalf("external session occupancy = %d, want 2 (one adopted, one quarantined)", count)
	}
}

// TestActiveSessionsAdoptedBeatsQuarantined pins the precedence inside the
// shim projection: with no direct child, the adopted handle (which has a
// controller) wins over the quarantined row for the same session.
func TestActiveSessionsAdoptedBeatsQuarantined(t *testing.T) {
	d := New(Options{SkipRegistration: true})
	d.spawner = NewWorkerSpawner(SpawnerOptions{MaxConcurrentSessions: 8})
	d.spawner.Resume()
	id := sessionshim.Identity{OrgID: "org-dedupe", SessionID: "dup"}
	d.shims.mu.Lock()
	d.shims.adopted[id] = adoptedShim{
		shimID: "shim-adopted",
		handle: SessionHandle{SessionID: "dup", State: SessionRunning, Repository: "adopted", ProjectName: "p", Harness: "h"},
	}
	d.shims.quarantined = []sessionshim.QuarantinedSession{{
		OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-quarantined", ProcessEpoch: 1,
		Reason: sessionshim.QuarantineSocketUnreachable, ConsumesCapacity: true,
	}}
	d.shims.mu.Unlock()

	got := d.ActiveSessions()
	count := 0
	for _, h := range got {
		if h.SessionID == "dup" {
			count++
			if h.Repository != "adopted" {
				t.Errorf("deduped handle Repository = %q, want the adopted handle's (adopted beats quarantined)", h.Repository)
			}
		}
	}
	if count != 1 {
		t.Fatalf("ActiveSessions lists session %q %d times (%+v), want exactly once", "dup", count, got)
	}
}
