package daemon

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
)

// TestQuarantineChangeIsPublished is the control for the defect P51 hit at
// layer 13. The platform's heartbeat preflight compares the beat's quarantine
// set against the snapshot the last adoption-batch commit stored, byte for
// byte, and demotes the host to `draining` when they disagree. A quarantine
// that changes the daemon's own projection without publishing it therefore
// takes the host out of service until something else republishes.
//
// The assertion is the agreement itself: whatever QuarantinedSessions reports
// must equal what the last published batch carried.
func TestQuarantineChangeIsPublished(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var published []SessionShimAdoptionBatch
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		HostIDForOrg: func(context.Context, string) (string, error) { return "wh_test_host", nil },
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-1"), AdoptionRevision: "1"}, nil
		},
	}})

	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         "org-publish", SessionID: "session-publish",
		ShimID: "shim-publish", ProcessEpoch: 4,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineSocketUnreachable, "controller stream ended before a terminal observation", time.Now())
	q.ControllerGeneration = 2

	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(q)
	d.shims.mu.Unlock()

	d.publishSessionShimProjection(context.Background(), "org-publish")

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published %d batches, want exactly one", len(published))
	}
	batch := published[0]
	if batch.OrgID != "org-publish" || batch.HostID != "wh_test_host" {
		t.Fatalf("batch scope = %q/%q, want org-publish/wh_test_host", batch.OrgID, batch.HostID)
	}
	beat := d.QuarantinedSessions()
	if len(beat) != len(batch.Quarantined) {
		t.Fatalf("heartbeat projection has %d quarantined, published batch has %d — the platform refuses a beat that disagrees",
			len(beat), len(batch.Quarantined))
	}
	for i := range beat {
		if beat[i] != batch.Quarantined[i] {
			t.Fatalf("quarantine entry %d differs between the beat and the published batch:\n beat=%+v\n batch=%+v",
				i, beat[i], batch.Quarantined[i])
		}
	}
}

// TestQuarantinePublishIsScopedToOneOrganization keeps the republish from
// leaking one organization's sessions into another's durable projection.
func TestQuarantinePublishIsScopedToOneOrganization(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var published []SessionShimAdoptionBatch
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		HostIDForOrg: func(_ context.Context, org string) (string, error) { return "wh_" + org, nil },
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-1"), AdoptionRevision: "1"}, nil
		},
	}})
	for _, org := range []string{"org-a", "org-b"} {
		q := sessionshim.NewQuarantinedSession(sessionshim.Record{
			SchemaVersion: sessionshim.RecordSchemaVersion,
			OrgID:         org, SessionID: "session-" + org, ShimID: "shim-" + org, ProcessEpoch: 1,
			CreatedAtUnixNano: time.Now().UnixNano(),
		}, sessionshim.QuarantineSocketUnreachable, "test", time.Now())
		d.shims.mu.Lock()
		d.upsertShimQuarantineLocked(q)
		d.shims.mu.Unlock()
	}

	d.publishSessionShimProjection(context.Background(), "org-a")

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published %d batches, want one", len(published))
	}
	if got := len(published[0].Quarantined); got != 1 {
		t.Fatalf("org-a batch carried %d quarantined sessions, want only its own", got)
	}
	if published[0].Quarantined[0].OrgID != "org-a" {
		t.Fatalf("org-a batch carried %q", published[0].Quarantined[0].OrgID)
	}
}

// TestEveryQuarantineMutationPublishes is the invariant, not one instance of
// it. Two of the four sites that change the quarantine set published it and two
// did not, and nothing failed when they did not — which is how a dropped
// controller connection came to drain its own host. Any new call site must
// either publish, or assemble a batch that will be published, in the same
// function.
func TestEveryQuarantineMutationPublishes(t *testing.T) {
	t.Parallel()
	// Both directions mutate the set the receiver compares byte for byte: an
	// upsert ADDS a lineage, the durable-handoff withdrawal REMOVES one. The
	// withdrawal used to be invisible to this guard, and a host whose
	// quarantined lineage was tombstoned then beat `quarantined=[]` against a
	// row still holding `[X]` at the same revision — refused, and demoted to
	// draining, on every beat until a restart republished.
	mutators := []string{"upsertShimQuarantineLocked", "withdrawQuarantinedLineageAfterDurableHandoff"}
	const (
		publishes = "publishSessionShimProjection"
		assembles = "completeSessionShimAdoptionBatch"
		// The disconnect path publishes through a named helper whose ORDER is
		// the contract — consume any terminal proof, then publish — so it
		// counts as a publisher here. Everything the guard is watching for
		// still applies: a new mutation site must name one of these three.
		ordered = "publishQuarantineAfterConsumingTerminalProof"
	)
	entries, err := os.ReadDir("./")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, filepath.Join("./", name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || contains(mutators, fn.Name.Name) {
				continue
			}
			var calls []string
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					calls = append(calls, fun.Sel.Name)
				case *ast.Ident:
					calls = append(calls, fun.Name)
				}
				return true
			})
			mutates := false
			for _, mutator := range mutators {
				if contains(calls, mutator) {
					mutates = true
				}
			}
			if !mutates {
				continue
			}
			checked++
			if !contains(calls, publishes) && !contains(calls, assembles) && !contains(calls, ordered) {
				t.Errorf("%s: %s changes the quarantine projection without publishing it — "+
					"the platform demotes a host whose heartbeat disagrees with the last published batch",
					name, fn.Name.Name)
			}
		}
	}
	if checked < 2 {
		t.Fatalf("found %d call sites of %v, want at least the upsert and the withdrawal — the guard is not watching what it claims to",
			checked, mutators)
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// TestAdoptedBatchOrderMatchesTheReceiverComparator pins the daemon's adopted
// ordering to the exact tuple the platform re-checks on arrival. The receiving
// side refuses a batch whose rows are not in ITS order, so a comparator that
// agrees on a prefix and diverges on the tail produces a batch that is correct
// here and rejected there. The tail is ControllerGeneration, which this
// comparator used to omit.
func TestAdoptedBatchOrderMatchesTheReceiverComparator(t *testing.T) {
	t.Parallel()
	outcome := func(org, session, shim string, epoch, generation uint64) SessionShimAdoptionOutcome {
		return SessionShimAdoptionOutcome{Evidence: SessionShimAdoptionEvidence{
			Identity:     sessionshim.Identity{OrgID: org, SessionID: session},
			ShimID:       shim,
			ProcessEpoch: epoch, ControllerGeneration: generation,
		}}
	}
	// Deliberately reversed on every key, innermost first.
	in := []SessionShimAdoptionOutcome{
		outcome("org-b", "session-a", "shim-a", 1, 1),
		outcome("org-a", "session-a", "shim-a", 1, 9),
		outcome("org-a", "session-a", "shim-a", 1, 2),
		outcome("org-a", "session-a", "shim-a", 0, 5),
		outcome("org-a", "session-a", "shim-0", 1, 5),
		outcome("org-a", "session-0", "shim-a", 1, 5),
	}
	sortSessionShimAdoptionOutcomes(in)

	// The receiver's comparator, transcribed from the platform's
	// compareBatchCorrelation: org, session, shim, processEpoch, generation.
	for i := 1; i < len(in); i++ {
		prev, cur := in[i-1].Evidence, in[i].Evidence
		switch {
		case prev.Identity.OrgID != cur.Identity.OrgID:
			if prev.Identity.OrgID > cur.Identity.OrgID {
				t.Fatalf("entry %d: org out of order (%q after %q)", i, cur.Identity.OrgID, prev.Identity.OrgID)
			}
		case prev.Identity.SessionID != cur.Identity.SessionID:
			if prev.Identity.SessionID > cur.Identity.SessionID {
				t.Fatalf("entry %d: session out of order (%q after %q)", i, cur.Identity.SessionID, prev.Identity.SessionID)
			}
		case prev.ShimID != cur.ShimID:
			if prev.ShimID > cur.ShimID {
				t.Fatalf("entry %d: shim out of order (%q after %q)", i, cur.ShimID, prev.ShimID)
			}
		case prev.ProcessEpoch != cur.ProcessEpoch:
			if prev.ProcessEpoch > cur.ProcessEpoch {
				t.Fatalf("entry %d: process epoch out of order (%d after %d)", i, cur.ProcessEpoch, prev.ProcessEpoch)
			}
		default:
			if prev.ControllerGeneration > cur.ControllerGeneration {
				t.Fatalf("entry %d: controller generation out of order (%d after %d) — "+
					"the receiver refuses a batch whose rows are not in its order",
					i, cur.ControllerGeneration, prev.ControllerGeneration)
			}
		}
	}
}

// TestBatchSectionsAreOrderedForTheReceiver extends the same pin to the whole
// batch. The receiver checks the order of ALL FOUR sections and refuses one
// that disagrees, and the tombstoned section was ordered by nothing but append
// order — invisible while a host only ever carried one tombstone, and a refused
// batch the moment one lifecycle identity had two terminal incarnations, which
// is exactly what an acceptance quarantine plus its session's own end produce.
func TestBatchSectionsAreOrderedForTheReceiver(t *testing.T) {
	t.Parallel()
	terminal := func(org, session, shim string, epoch uint64) SessionShimTerminalEvidence {
		return SessionShimTerminalEvidence{
			Identity: sessionshim.Identity{OrgID: org, SessionID: session},
			ShimID:   shim, ProcessEpoch: epoch,
			Tombstone: sessionshim.Tombstone{
				SchemaVersion: sessionshim.RecordSchemaVersion,
				OrgID:         org, SessionID: session, ShimID: shim, ProcessEpoch: epoch,
				GroupReaped: true, ObservedAtUnixNano: 1,
			},
		}
	}
	// Deliberately reversed on every key, innermost first.
	batch := SessionShimAdoptionBatch{Tombstoned: []SessionShimTerminalEvidence{
		terminal("org-b", "session-a", "shim-a", 1),
		terminal("org-a", "session-a", "shim-a", 9),
		terminal("org-a", "session-a", "shim-0", 1),
		terminal("org-a", "session-0", "shim-a", 1),
	}}
	sortSessionShimAdoptionBatch(&batch)

	// The receiver's comparator, transcribed from the platform's
	// compareBatchCorrelation: org, session, shim, processEpoch.
	for i := 1; i < len(batch.Tombstoned); i++ {
		prev, cur := batch.Tombstoned[i-1], batch.Tombstoned[i]
		switch {
		case prev.Identity.OrgID != cur.Identity.OrgID:
			if prev.Identity.OrgID > cur.Identity.OrgID {
				t.Fatalf("entry %d: org out of order (%q after %q)", i, cur.Identity.OrgID, prev.Identity.OrgID)
			}
		case prev.Identity.SessionID != cur.Identity.SessionID:
			if prev.Identity.SessionID > cur.Identity.SessionID {
				t.Fatalf("entry %d: session out of order (%q after %q)", i, cur.Identity.SessionID, prev.Identity.SessionID)
			}
		case prev.ShimID != cur.ShimID:
			if prev.ShimID > cur.ShimID {
				t.Fatalf("entry %d: shim out of order (%q after %q)", i, cur.ShimID, prev.ShimID)
			}
		default:
			if prev.ProcessEpoch > cur.ProcessEpoch {
				t.Fatalf("entry %d: process epoch out of order (%d after %d) — "+
					"the receiver refuses a batch whose rows are not in its order",
					i, cur.ProcessEpoch, prev.ProcessEpoch)
			}
		}
	}
}

// TestBatchOrderingIsEnforcedAtTheCommitChokePoint is the pin the two
// comparator tests above do NOT provide: they call the sort helpers directly,
// so replacing the choke point's call with a no-op leaves them green while
// every real batch ships in Go map order and the receiver refuses it.
func TestBatchOrderingIsEnforcedAtTheCommitChokePoint(t *testing.T) {
	t.Parallel()
	const scope = "org-choke"
	var mu sync.Mutex
	var published []SessionShimAdoptionBatch
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		OrgID: scope, HostID: "wh_choke_host",
		HostIDForOrg: func(context.Context, string) (string, error) { return "wh_choke_host", nil },
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			published = append(published, cloneSessionShimAdoptionBatch(batch))
			mu.Unlock()
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-1"), AdoptionRevision: "1"}, nil
		},
	}})
	adopted := func(session, shim string, epoch, generation uint64) SessionShimAdoptionOutcome {
		return SessionShimAdoptionOutcome{Evidence: SessionShimAdoptionEvidence{
			Identity:     sessionshim.Identity{OrgID: scope, SessionID: session},
			ShimID:       shim,
			ProcessEpoch: epoch, ControllerGeneration: generation,
		}}
	}
	terminal := func(session, shim string, epoch uint64) SessionShimTerminalEvidence {
		return SessionShimTerminalEvidence{
			Identity: sessionshim.Identity{OrgID: scope, SessionID: session},
			ShimID:   shim, ProcessEpoch: epoch,
			Tombstone: sessionshim.Tombstone{
				SchemaVersion: sessionshim.RecordSchemaVersion,
				OrgID:         scope, SessionID: session, ShimID: shim, ProcessEpoch: epoch,
				GroupReaped: true, ObservedAtUnixNano: 1,
			},
		}
	}
	quarantined := func(session, shim string, epoch uint64) sessionshim.QuarantinedSession {
		return sessionshim.QuarantinedSession{
			OrgID: scope, SessionID: session, ShimID: shim, ProcessEpoch: epoch,
			Reason: sessionshim.QuarantineSocketUnreachable, ConsumesCapacity: true,
		}
	}
	// Every section reversed on its innermost key, exactly what Go map order
	// produces on a host carrying more than one lineage.
	batch := SessionShimAdoptionBatch{
		OrgID: scope, HostID: "wh_choke_host", ExpectedRevision: []byte("0"),
		Adopted:     []SessionShimAdoptionOutcome{adopted("session-b", "shim-b", 2, 9), adopted("session-a", "shim-a", 1, 1)},
		Tombstoned:  []SessionShimTerminalEvidence{terminal("session-b", "shim-b", 2), terminal("session-a", "shim-a", 1)},
		Quarantined: []sessionshim.QuarantinedSession{quarantined("session-b", "shim-b", 2), quarantined("session-a", "shim-a", 1)},
	}
	if _, err := d.completeSessionShimAdoptionBatch(context.Background(), batch); err != nil {
		t.Fatalf("commit: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published %d batches, want one", len(published))
	}
	got := published[0]
	if len(got.Adopted) != 2 || got.Adopted[0].Evidence.Identity.SessionID != "session-a" {
		t.Fatalf("adopted section left the choke point unordered: %+v", got.Adopted)
	}
	if len(got.Tombstoned) != 2 || got.Tombstoned[0].Identity.SessionID != "session-a" {
		t.Fatalf("tombstoned section left the choke point unordered: %+v", got.Tombstoned)
	}
	if len(got.Quarantined) != 2 || got.Quarantined[0].SessionID != "session-a" {
		t.Fatalf("quarantined section left the choke point unordered: %+v", got.Quarantined)
	}
}

// TestQuarantinePublishRetainsTheAdoptionRevision is the control for the second
// half of the same divergence. Committing a batch advances the host's adoption
// revision; the heartbeat attests the revision this daemon believes it is at,
// and the platform refuses the beat and demotes the host when the two disagree.
//
// A republish that publishes correctly but discards its own receipt therefore
// trades one divergence for another — observed on a real host as the quarantine
// projection landing and every following heartbeat answering
// SESSION_SHIM_ADOPTION_REVISION_STALE.
func TestQuarantinePublishRetainsTheAdoptionRevision(t *testing.T) {
	t.Parallel()
	const scope = "org-revision"
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true,
		OrgID:          scope,
		HostID:         "wh_revision_host",
		OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("rev-7"),
				AdoptionRevision:   "7",
			}, nil
		},
	}})
	enableHostedFullHostFramesForTest(t, d, scope)
	d.shims.mu.Lock()
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.shims.mu.Unlock()
	recorder, baselineBeats := startQuarantinePublishBeatRecorder(t, d, scope)

	d.shims.mu.RLock()
	before := d.shims.credentialReceipts[scope].AdoptionRevision
	d.shims.mu.RUnlock()
	if before == "7" {
		t.Fatalf("precondition: the seeded revision is already the one the batch returns (%q)", before)
	}

	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         scope, SessionID: "session-revision",
		ShimID: "shim-revision", ProcessEpoch: 1,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineSocketUnreachable, "controller stream ended before a terminal observation", time.Now())
	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(q)
	d.shims.mu.Unlock()

	publishedAt := time.Now()
	d.publishSessionShimProjection(context.Background(), scope)

	d.shims.mu.RLock()
	after := d.shims.credentialReceipts[scope].AdoptionRevision
	pending, ackPending := d.shims.pendingHeartbeatAcks[scope]
	d.shims.mu.RUnlock()
	if after != "7" {
		t.Fatalf("retained adoption revision = %q, want the batch receipt's %q — "+
			"the next heartbeat attests this value and the platform demotes the host when it is stale",
			after, "7")
	}
	if ackPending {
		t.Fatalf("republish left a heartbeat acknowledgement pending (%q); it activates no carrier, "+
			"so it must not withdraw readiness", pending)
	}
	// The receiver demotes the host's readiness on EVERY batch commit until a
	// matching beat re-attests it, so a republish that waits for the periodic
	// ticker leaves the host refusing polls for up to a whole interval. The
	// interval here is the production default: a beat inside two seconds can
	// only be the immediate one.
	waitFor(t, 2*time.Second, "the immediate beat after the republish", func() bool {
		return recorder.count() >= baselineBeats+1
	})
	if elapsed := time.Since(publishedAt); elapsed >= HeartbeatDefaultInterval {
		t.Fatalf("the beat after the republish took %s — the ticker, not an immediate beat", elapsed)
	}
	revisions := recorder.revisions()
	if len(revisions) == 0 || revisions[len(revisions)-1] != "7" {
		t.Fatalf("revisions on the wire = %v, want the immediate beat to attest the republished %q", revisions, "7")
	}
}

// startQuarantinePublishBeatRecorder puts a real heartbeat lane on d against a
// control plane that acknowledges every beat, waits for the lane's first
// periodic beat, and returns the recorder plus the beats already sent.
func startQuarantinePublishBeatRecorder(t *testing.T, d *Daemon, scope string) (*immediateBeatRecorder, int) {
	t.Helper()
	recorder := &immediateBeatRecorder{}
	server := httptest.NewServer(recorder.handler(t))
	t.Cleanup(server.Close)
	service := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker-" + scope, OrchestratorURL: server.URL,
		RuntimeJWT:      "runtime-" + scope,
		IntervalSeconds: int(HeartbeatDefaultInterval / time.Second),
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     d.heartbeatMaxConcurrentSessions,
		GetStatus:       d.RegistrationStatus,
		GetSessionShim: func() (SessionShimHeartbeatProjection, error) {
			return d.SessionShimHeartbeatProjection(scope)
		},
		HTTPClient: server.Client(),
	})
	d.lifecycleMu.Lock()
	d.heartbeat = service
	d.lifecycleMu.Unlock()
	service.Start()
	t.Cleanup(service.Stop)
	waitFor(t, 5*time.Second, "the periodic lane's first beat", func() bool {
		return recorder.count() >= 1
	})
	return recorder, recorder.count()
}

// TestTombstoneWithdrawalRepublishesTheProjection is the behavioural half of
// the guard above, driven through the surface every heartbeat already runs.
//
// A quarantined lineage whose harness the shim reaped hands its tombstone over
// durably and leaves the quarantine projection. The receiver's row still holds
// that lineage in ITS quarantine set at the same adoption revision, and nothing
// on the receiver prunes it: only a new batch moves the row. Measured on an
// installed host as `quarantined=[]` beating against `[X]` every thirty seconds,
// each beat refused stale and the host demoted to draining, until a restart.
func TestTombstoneWithdrawalRepublishesTheProjection(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "dtw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	const scope = "org-withdrawal"
	id := sessionshim.Identity{OrgID: scope, SessionID: "session-withdrawal"}

	var mu sync.Mutex
	var batches []SessionShimAdoptionBatch
	var terminals []SessionShimTerminalEvidence
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:  filepath.Join(dir, "registry"),
		HostIDForOrg: func(context.Context, string) (string, error) { return "wh_withdrawal_host", nil },
		OnTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
			mu.Lock()
			defer mu.Unlock()
			terminals = append(terminals, evidence)
			return nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			batches = append(batches, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-41"), AdoptionRevision: "41"}, nil
		},
	}})
	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-withdrawal", ProcessEpoch: 3,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineSocketUnreachable, "controller stream ended before a terminal observation", time.Now())
	d.shims.mu.Lock()
	d.shims.registry = registry
	d.upsertShimQuarantineLocked(q)
	d.shims.mu.Unlock()

	// The shim reached its orphan deadline, reaped the harness group, and left
	// its proof on disk exactly as §D8 has it do.
	tombstone := sessionshim.Tombstone{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: q.ShimID, ProcessEpoch: q.ProcessEpoch,
		ExitCode: 143, Signal: "SIGTERM",
		GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.PutTombstone(tombstone); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}

	d.reconcileQuarantinedTombstones()

	mu.Lock()
	reported := append([]SessionShimTerminalEvidence(nil), terminals...)
	published := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	if len(reported) != 1 || reported[0].Tombstone != tombstone {
		t.Fatalf("terminal evidence reported %d times (%+v), want the exact proof once", len(reported), reported)
	}
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("the handoff left %d lineages projected quarantined", len(projected))
	}
	if len(published) != 1 {
		t.Fatalf("published %d batches after the tombstone withdrawal, want exactly one — "+
			"the receiver's quarantine set advances only through a batch", len(published))
	}
	batch := published[0]
	if batch.OrgID != scope || len(batch.Quarantined) != 0 {
		t.Fatalf("withdrawal batch = %+v, want scope %q with an empty quarantine set", batch, scope)
	}
	if len(batch.Tombstoned) != 1 || batch.Tombstoned[0].Tombstone != tombstone {
		t.Fatalf("withdrawal batch tombstoned = %+v, want the handed-over lineage", batch.Tombstoned)
	}
}

// TestReleaseShimIfLiveConsumesTerminalProofBeforePublishing pins the order at
// the CALL SITE, not in the helper.
//
// The helper's name states the contract, and a test that drives the helper
// directly proves only that the helper honours its own name: inlining a plain
// publish back into releaseShimIfLive would keep such a test green while
// restoring the exact defect. This drives the disconnect path itself, with a
// real adopted controller and a real tombstone on disk, and asserts that no
// quarantine ever reaches the composer.
func TestReleaseShimIfLiveConsumesTerminalProofBeforePublishing(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "dp8")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-release-order", SessionID: "session-release-order"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 11,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`}},
		WorkareaPath: filepath.Join(dir, "workarea"),
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
		Registry: registry, ControllerID: "controller-release-order",
	})
	if err != nil || len(adoption.Adopted) != 1 {
		t.Fatalf("Adopt = %+v, %v", adoption, err)
	}
	controller := adoption.Adopted[0]
	hello := controller.Hello()

	var mu sync.Mutex
	var batches []SessionShimAdoptionBatch
	var terminals []SessionShimTerminalEvidence
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:  filepath.Join(dir, "registry"),
		HostIDForOrg: func(context.Context, string) (string, error) { return "wh_test_host", nil },
		OnTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
			mu.Lock()
			defer mu.Unlock()
			terminals = append(terminals, evidence)
			return nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			batches = append(batches, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-1"), AdoptionRevision: "1"}, nil
		},
	}})
	d.shims.mu.Lock()
	d.shims.registry = registry
	d.shims.adopted[id] = adoptedShim{controller: controller, shimID: hello.ShimID}
	d.shims.mu.Unlock()

	// The shim finalized between its last frame and this disconnect: its proof
	// is already on disk when the consumer notices the stream is gone. That is
	// the measured race — the shim answers a late acknowledgement with `exited`
	// and closes.
	want := sessionshim.Tombstone{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: hello.ShimID, ProcessEpoch: hello.ProcessEpoch,
		HarnessPID: hello.HarnessPID, HarnessStartedAt: hello.HarnessStartedAt,
		ExitCode: 143, Signal: "SIGTERM",
		GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.PutTombstone(want); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}

	d.releaseShimIfLive(id, controller, shimStreamCarrierLost)

	mu.Lock()
	reported := append([]SessionShimTerminalEvidence(nil), terminals...)
	published := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	if len(reported) != 1 || reported[0].Tombstone != want {
		t.Fatalf("terminal evidence reported %d times (%+v), want the shim's exact proof once", len(reported), reported)
	}
	for i, batch := range published {
		if len(batch.Quarantined) != 0 {
			t.Fatalf("batch %d published a quarantine for a lineage whose tombstone was already on disk: %+v — "+
				"that publication costs an adoption revision the next one has to undo", i, batch.Quarantined)
		}
	}
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("the disconnect left %d lineages projected quarantined", len(projected))
	}
}

// TestTerminalDisposalWithdrawsTheRecordFirst pins the disposal ORDER at every
// site that disposes a tombstone.
//
// A shim publishes its tombstone and then removes its discovery record, so a
// crash between the two leaves BOTH on disk by design. Disposing the tombstone
// first collapses "terminal, proven" into "a record whose process is gone",
// which §D10 classifies as stale and leaves unresolved forever — there is no
// proof left to reach the other conclusion with. finishAdoptedShim documents
// this order; the quarantine reconcile disposed without withdrawing at all,
// which is the same defect with the first step missing.
func TestTerminalDisposalWithdrawsTheRecordFirst(t *testing.T) {
	t.Parallel()
	const (
		withdraw = "RemoveIncarnation"
		dispose  = "RemoveTombstoneIncarnation"
	)
	fset := token.NewFileSet()
	entries, err := os.ReadDir("./")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join("./", name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			firstWithdraw, firstDispose := token.NoPos, token.NoPos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				sel, isSel := call.Fun.(*ast.SelectorExpr)
				if !isSel {
					return true
				}
				switch sel.Sel.Name {
				case withdraw:
					if !firstWithdraw.IsValid() {
						firstWithdraw = call.Pos()
					}
				case dispose:
					if !firstDispose.IsValid() {
						firstDispose = call.Pos()
					}
				}
				return true
			})
			if !firstDispose.IsValid() {
				continue
			}
			checked++
			if !firstWithdraw.IsValid() {
				t.Errorf("%s: %s disposes a terminal tombstone without withdrawing the discovery record — "+
					"the surviving record then reads as stale liveness with no proof left to resolve it",
					name, fn.Name.Name)
				continue
			}
			if firstWithdraw > firstDispose {
				t.Errorf("%s: %s disposes the tombstone before withdrawing the record", name, fn.Name.Name)
			}
		}
	}
	if checked < 2 {
		t.Fatalf("found %d tombstone-disposing functions, want at least the adopted and quarantined paths — "+
			"the guard is not watching what it claims to", checked)
	}
}
