package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// readoptFixture is one live shim adopted by a daemon whose durable adoption
// callback is scripted per attempt, so a test can say exactly which
// re-adoption attempt the composing carrier accepts.
type readoptFixture struct {
	daemon     *Daemon
	registry   *sessionshim.Registry
	id         sessionshim.Identity
	controller *sessionshim.Controller

	mu        sync.Mutex
	adoptions int
	batches   []SessionShimAdoptionBatch
}

// newReadoptFixture starts a real shim, adopts it once through the ordinary
// sessionshim.Adopt path, and seeds the daemon's adopted entry with the exact
// evidence a startup adoption would have recorded. adoptionOutcome answers the
// n-th OnAdoption call (1-based) with nil or the error it should refuse with.
func newReadoptFixture(t *testing.T, policy SessionShimReadoptionPolicy, adoptionOutcome func(attempt int) error) *readoptFixture {
	t.Helper()
	// A Unix socket path has a short platform limit, and t.TempDir() bakes
	// the test name into the path. Keep the registry short.
	dir, err := os.MkdirTemp("/tmp", "drd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	f := &readoptFixture{registry: registry}
	f.id = sessionshim.Identity{OrgID: "org-readopt", SessionID: "session-readopt"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: f.id, Registry: registry, ProcessEpoch: 5,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`}},
		WorkareaPath: filepath.Join(dir, "workarea"),
		// Long enough that a test never races the shim's own reaper; the
		// point of re-adoption is that the deadline is never reached.
		Orphan: sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second, PropagationMargin: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})
	adoption, err := sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "controller-readopt-first",
	})
	if err != nil || len(adoption.Adopted) != 1 {
		t.Fatalf("Adopt = %+v, %v", adoption, err)
	}
	f.controller = adoption.Adopted[0]

	f.daemon = New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:     filepath.Join(dir, "registry"),
		CallbackTimeout: 5 * time.Second,
		HostIDForOrg:    func(context.Context, string) (string, error) { return "wh_readopt_host", nil },
		OnAdoption: func(_ context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			f.mu.Lock()
			f.adoptions++
			attempt := f.adoptions
			f.mu.Unlock()
			if err := adoptionOutcome(attempt); err != nil {
				return SessionShimAdoptionReceipt{}, err
			}
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("readopt-" + evidence.Identity.Key())}, nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.batches = append(f.batches, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-readopt"), AdoptionRevision: "readopt-revision"}, nil
		},
		Readoption: policy,
	}})
	t.Cleanup(f.daemon.ReleaseAdoptedSessionShims)
	evidence, err := f.daemon.sessionShimAdoptionEvidence(context.Background(), f.controller, SessionShimAdoptionPreparationResult{}, "wh_readopt_host")
	if err != nil {
		t.Fatalf("adoption evidence: %v", err)
	}
	evidence.SnapshotProxy = nil
	f.daemon.shims.mu.Lock()
	f.daemon.shims.registry = registry
	f.daemon.shims.adopted[f.id] = adoptedShim{
		controller: f.controller, shimID: f.controller.Hello().ShimID,
		adoption: evidence, adoptionReceipt: SessionShimAdoptionReceipt{DurableCorrelation: []byte("first")},
	}
	f.daemon.shims.mu.Unlock()
	return f
}

func (f *readoptFixture) snapshot() (int, []SessionShimAdoptionBatch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adoptions, append([]SessionShimAdoptionBatch(nil), f.batches...)
}

func (f *readoptFixture) recordPhase(t *testing.T) shimwire.Phase {
	t.Helper()
	rec, err := f.registry.Get(f.id)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	return rec.Phase
}

// TestControllerLossReadoptsALiveShimBeforeQuarantining pins the recovery a
// relay restart needs.
//
// The controller stream ended, but the shim and its harness are alive and the
// discovery record is live: the loss was the CARRIER's, not the shim's. Before
// this change the disconnect path quarantined the lineage `socket_unreachable`
// at once and nothing ever dialled it again, so every carrier blip cost a
// healthy harness its orphan deadline. The first re-adoption attempt is refused
// (the carrier is still coming back); the second is accepted. No quarantine may
// reach the composer, the lineage must stay adopted under a strictly newer
// generation, and the shim's own orphan clock must be disarmed.
func TestControllerLossReadoptsALiveShimBeforeQuarantining(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 10 * time.Millisecond}, func(attempt int) error {
		if attempt == 1 {
			return errors.New("carrier candidate dial refused: relay restarting")
		}
		return nil
	})
	d := f.daemon
	previousGeneration := f.controller.Generation()

	d.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	adoptions, batches := f.snapshot()
	if adoptions != 2 {
		t.Fatalf("durable adoption attempted %d times, want 2 — one refused, the second accepted", adoptions)
	}
	for i, batch := range batches {
		if len(batch.Quarantined) != 0 {
			t.Fatalf("batch %d quarantined a lineage whose shim was alive and re-adoptable: %+v", i, batch.Quarantined)
		}
	}
	if len(batches) == 0 {
		t.Fatal("no batch was published for the re-adoption; the receiver never learned the new generation")
	}
	last := batches[len(batches)-1]
	if len(last.Adopted) != 1 || last.Adopted[0].Evidence.Identity != f.id {
		t.Fatalf("last batch adopted = %+v, want exactly the re-adopted lineage", last.Adopted)
	}
	if got := last.Adopted[0].Evidence.ControllerGeneration; got <= uint64(previousGeneration) {
		t.Fatalf("re-adopted generation = %d, want strictly newer than the lost controller's %d", got, previousGeneration)
	}
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("re-adoption left %d lineages projected quarantined: %+v", len(projected), projected)
	}
	entry, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("the lineage left the adopted set: %v", err)
	}
	if entry.controller == f.controller || entry.controller.Generation() <= previousGeneration {
		t.Fatalf("adopted entry still names the lost controller (generation %d)", entry.controller.Generation())
	}
	if entry.adoption.ControllerGeneration != uint64(entry.controller.Generation()) {
		t.Fatalf("adopted evidence generation %d disagrees with the live controller's %d",
			entry.adoption.ControllerGeneration, entry.controller.Generation())
	}
	waitFor(t, 5*time.Second, "the shim to disarm its orphan clock", func() bool {
		return f.recordPhase(t) == shimwire.PhaseRunning
	})
}

// TestControllerLossQuarantinesWhenEveryReadoptionFails keeps the fallback
// exactly what it was: a shim that cannot be re-adopted inside the bound is
// quarantined `socket_unreachable`, capacity-charged, and published — never
// killed, never released.
func TestControllerLossQuarantinesWhenEveryReadoptionFails(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return errors.New("carrier candidate dial refused")
	})
	d := f.daemon

	d.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	adoptions, batches := f.snapshot()
	if adoptions != 3 {
		t.Fatalf("durable adoption attempted %d times, want the policy's 3 before giving up", adoptions)
	}
	if _, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID); err == nil {
		t.Fatal("an unre-adoptable lineage stayed in the adopted set")
	}
	projected := d.QuarantinedSessions()
	if len(projected) != 1 || projected[0].Reason != sessionshim.QuarantineSocketUnreachable || !projected[0].ConsumesCapacity {
		t.Fatalf("quarantine projection = %+v, want one capacity-charged socket_unreachable lineage", projected)
	}
	if len(batches) == 0 {
		t.Fatal("the quarantine was never published")
	}
	last := batches[len(batches)-1]
	if len(last.Quarantined) != 1 || last.Quarantined[0].SessionID != f.id.SessionID || len(last.Adopted) != 0 {
		t.Fatalf("last batch = adopted:%+v quarantined:%+v, want the lineage quarantined and nothing adopted",
			last.Adopted, last.Quarantined)
	}
}

// TestControllerLossReadoptionCanBeDisabled keeps the embedder in charge: a
// composition that declares no re-adoption gets the pre-existing disposition
// with no dial at all.
func TestControllerLossReadoptionCanBeDisabled(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Disabled: true}, func(int) error { return nil })

	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	adoptions, _ := f.snapshot()
	if adoptions != 0 {
		t.Fatalf("durable adoption attempted %d times with re-adoption disabled, want none", adoptions)
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 1 {
		t.Fatalf("quarantine projection = %+v, want the lineage quarantined as before", projected)
	}
}
