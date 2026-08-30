package daemon

// Provenance: shim-commit-reconciliation-2026-08-27 — grep a build for this
// marker to prove it carries commit-outcome reconciliation.
//
// The measured strand these tests pin: an adoption-batch commit whose answer
// was lost to a transport flake AFTER the control plane stamped it. Treating
// that ambiguous outcome like a refusal kept the daemon beating the superseded
// revision forever — every beat answered with the closed revision-stale
// conflict, every clear republish refused on the stale expected revision.

import (
	"context"
	"errors"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// transportLostCommitAnswer is the deterministic ambiguity injection: a
// *url.Error (what an HTTP round trip actually returns, and a net.Error) whose
// request the fake control plane has ALREADY applied by the time the fixture
// returns it.
func transportLostCommitAnswer() error {
	return &url.Error{
		Op: "Post", URL: "https://durable-store.invalid/adoption-batch",
		Err: errors.New("read tcp: connection reset by peer"),
	}
}

// reconciliationFixture owns the swappable batch-commit behavior behind one
// installed composition, recording every batch the daemon publishes.
type reconciliationFixture struct {
	mu      sync.Mutex
	batches []SessionShimAdoptionBatch
	commit  func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error)
}

func (f *reconciliationFixture) setCommit(commit func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commit = commit
}

func (f *reconciliationFixture) committedBatches() []SessionShimAdoptionBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionShimAdoptionBatch(nil), f.batches...)
}

func (f *reconciliationFixture) lastBatch(t *testing.T) SessionShimAdoptionBatch {
	t.Helper()
	batches := f.committedBatches()
	if len(batches) == 0 {
		t.Fatal("no batch was ever committed")
	}
	return batches[len(batches)-1]
}

// confirmedReceipt is the fixture's honest commit: durable and revisioned.
func confirmedReceipt(_ SessionShimAdoptionBatch, revision string) SessionShimAdoptionBatchReceipt {
	return SessionShimAdoptionBatchReceipt{
		DurableCorrelation: []byte(revision), AdoptionRevision: revision,
	}
}

// newReconciliationFixture installs a composition over the fake control plane
// and leaves the daemon serving at retained revision "revision-1".
func newReconciliationFixture(ctx context.Context, t *testing.T) (*compositionHarness, *reconciliationFixture) {
	t.Helper()
	h := newCompositionHarness(t)
	h.start(ctx)
	f := &reconciliationFixture{}
	f.commit = func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return confirmedReceipt(batch, "revision-1"), nil
	}
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		f.mu.Lock()
		commit := f.commit
		f.batches = append(f.batches, cloneSessionShimAdoptionBatch(batch))
		f.mu.Unlock()
		return commit(batch)
	})
	// The reconciliation bounds are DERIVED from the callback bound (backoff =
	// callbackTimeout, budget = adoptionPublicationTimeout = stages × it), so
	// shrinking the one unit keeps the whole loop test-sized without inventing
	// a second number.
	cfg.CallbackTimeout = 200 * time.Millisecond
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("composition install: %v", err)
	}
	h.setRefreshReceiptState(SessionShimCredentialStateReady)
	if receipt, ok := h.daemon.SessionShimScopeAuthority(h.orgID); !ok || receipt.AdoptionRevision != "revision-1" {
		t.Fatalf("retained authority after install = %+v (%v), want revision-1", receipt, ok)
	}
	return h, f
}

// stageReconciliationQuarantine plants one quarantined lineage plus the
// acceptance bookkeeping a later clear needs (a recorded process identity
// provably not running, and no registry record).
//
// No tombstone yet: the acceptance helper publishes its terminal proof on the
// way out, so the lineage is live-and-quarantined until
// publishReconciliationTombstone runs.
func stageReconciliationQuarantine(t *testing.T, d *Daemon, orgID string) shimIncarnation {
	t.Helper()
	id := sessionshim.Identity{OrgID: orgID, SessionID: "session-reconcile"}
	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-reconcile", ProcessEpoch: 7,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineProtocolMismatch, "reconciliation fixture protocol range has no overlap", time.Now())
	q.ControllerGeneration = 2
	incarnation := shimIncarnation{identity: id, shimID: "shim-reconcile", processEpoch: 7}
	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(q)
	d.shims.acceptanceQuarantine[incarnation] = sessionshim.ProcessIdentity{PID: os.Getpid(), StartedAt: 1}
	d.shims.mu.Unlock()
	return incarnation
}

// publishReconciliationTombstone writes the group-reaped tombstone the
// acceptance helper leaves behind when it reaps its own harness process group.
func publishReconciliationTombstone(t *testing.T, d *Daemon, incarnation shimIncarnation) {
	t.Helper()
	registry, err := d.sessionShimRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if err := registry.PutTombstone(sessionshim.Tombstone{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         incarnation.identity.OrgID, SessionID: incarnation.identity.SessionID,
		ShimID: incarnation.shimID, ProcessEpoch: incarnation.processEpoch,
		HarnessPID: os.Getpid(), HarnessStartedAt: 1,
		ExitCode: 143, Signal: "SIGTERM",
		GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (d *Daemon) reconcilingScopes() int {
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return len(d.shims.reconciling)
}

// TestAmbiguousArmCommitReconcilesToTheCommittedRevision is the measured
// strand end to end. A quarantine-arm batch commits on the control plane while
// the daemon's copy of the answer is lost to the same transport flake.
// Pre-fix, the daemon latched the superseded revision and every beat 409ed
// forever. Post-fix, reconciliation refreshes through the credential
// refresher, republishes the COMPLETE batch at the refreshed revision, the
// beat re-attests, and a subsequent clear proceeds.
func TestAmbiguousArmCommitReconcilesToTheCommittedRevision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, f := newReconciliationFixture(ctx, t)
	d := h.daemon
	incarnation := stageReconciliationQuarantine(t, d, h.orgID)
	h.setHeartbeatRequireRevision("revision-1")

	f.setCommit(func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		// SERVER COMMITTED: the control plane stamps the batch and moves to
		// revision-2 — refreshes answer it, beats must present it…
		h.setRefreshReceiptRevision("revision-2")
		h.setHeartbeatRequireRevision("revision-2")
		f.setCommit(func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			// …and the reconciliation republish commits at revision-3.
			h.setRefreshReceiptRevision("revision-3")
			h.setHeartbeatRequireRevision("revision-3")
			return confirmedReceipt(batch, "revision-3"), nil
		})
		// …while the daemon's copy of the answer is lost.
		return SessionShimAdoptionBatchReceipt{}, transportLostCommitAnswer()
	})

	// The quarantine-arm tail: publish the changed projection.
	d.publishSessionShimProjection(ctx, h.orgID)

	// The strand, undone: the daemon learns the committed revision through the
	// refresher and republishes; presenting revision-1 forever is the pre-fix
	// failure this wait times out on.
	waitForCondition(t, 5*time.Second, "the retained authority to reach the reconciled revision", func() bool {
		receipt, ok := d.SessionShimScopeAuthority(h.orgID)
		return ok && receipt.AdoptionRevision == "revision-3"
	})
	reconcileBatch := f.lastBatch(t)
	if len(reconcileBatch.Quarantined) != 1 || len(reconcileBatch.Adopted) != 0 || len(reconcileBatch.Cleared) != 0 {
		t.Fatalf("reconciliation republished %d quarantined / %d adopted / %d cleared, want the COMPLETE "+
			"current batch with exactly the armed quarantine", len(reconcileBatch.Quarantined),
			len(reconcileBatch.Adopted), len(reconcileBatch.Cleared))
	}
	if state := d.State(); state != StateRunning {
		t.Fatalf("daemon state after reconciliation = %q, want %q", state, StateRunning)
	}

	// The beat re-attests at the committed revision instead of 409ing forever.
	if err := d.heartbeat.SendNow(ctx); err != nil {
		t.Fatalf("beat after reconciliation: %v", err)
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil || beat.SessionShim.AdoptionRevision != "revision-3" {
		t.Fatalf("beat after reconciliation = %+v, want the reconciled revision-3 projection", beat.SessionShim)
	}

	// And a subsequent clear proceeds against the reconciled revision.
	f.setCommit(func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		h.setRefreshReceiptRevision("revision-4")
		h.setHeartbeatRequireRevision("revision-4")
		return confirmedReceipt(batch, "revision-4"), nil
	})
	publishReconciliationTombstone(t, d, incarnation)
	if err := d.clearSessionShimAcceptanceQuarantine(incarnation); err != nil {
		t.Fatalf("clear after reconciliation: %v", err)
	}
	clearBatch := f.lastBatch(t)
	if len(clearBatch.Tombstoned) != 1 || len(clearBatch.Quarantined) != 0 || len(clearBatch.Cleared) != 0 {
		t.Fatalf("post-reconciliation clear published %d tombstoned / %d quarantined / %d cleared, want the "+
			"lineage leaving through its terminal tombstone",
			len(clearBatch.Tombstoned), len(clearBatch.Quarantined), len(clearBatch.Cleared))
	}
	if remaining := d.QuarantinedSessions(); len(remaining) != 0 {
		t.Fatalf("confirmed clear left %d lineages projected", len(remaining))
	}
}

// TestBeatRevisionStaleTriggersReconciliation covers the trigger's other side:
// a daemon that lands revision-behind with NO ambiguity flag (the divergence
// observed only from the heartbeat refusal) reconciles off the 409 instead of
// skipping forever.
func TestBeatRevisionStaleTriggersReconciliation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, f := newReconciliationFixture(ctx, t)
	d := h.daemon

	// The control plane advanced out-of-band; this daemon still holds
	// revision-1 and has no local reason to doubt it.
	h.setRefreshReceiptRevision("revision-5")
	h.setHeartbeatRequireRevision("revision-5")
	f.setCommit(func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		h.setRefreshReceiptRevision("revision-6")
		h.setHeartbeatRequireRevision("revision-6")
		return confirmedReceipt(batch, "revision-6"), nil
	})

	err := d.heartbeat.SendNow(ctx)
	if err == nil {
		t.Fatal("a beat presenting a superseded revision was acknowledged — the fixture proves nothing")
	}

	waitForCondition(t, 5*time.Second, "the heartbeat refusal to drive reconciliation", func() bool {
		receipt, ok := d.SessionShimScopeAuthority(h.orgID)
		return ok && receipt.AdoptionRevision == "revision-6"
	})
	if err := d.heartbeat.SendNow(ctx); err != nil {
		t.Fatalf("beat after heartbeat-triggered reconciliation: %v", err)
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil || beat.SessionShim.AdoptionRevision != "revision-6" {
		t.Fatalf("beat after reconciliation = %+v, want revision-6", beat.SessionShim)
	}
}

// TestRefusedBatchCommitKeepsTodaysBehavior is the control: a decoded refusal
// (a closed-code answer, not a lost one) keeps exactly the pre-existing shape —
// the failure surfaces, the lineage stays projected, the retained revision is
// untouched, and NO reconciliation (no refresh, no republish) is armed.
func TestRefusedBatchCommitKeepsTodaysBehavior(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, f := newReconciliationFixture(ctx, t)
	d := h.daemon
	stageReconciliationQuarantine(t, d, h.orgID)
	refreshesBefore := len(h.refreshes())
	commitsBefore := len(f.committedBatches())

	f.setCommit(func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return SessionShimAdoptionBatchReceipt{},
			errors.New("adoption revision compare-and-swap refused (closed code, decoded 4xx)")
	})

	err := d.republishSessionShimProjection(ctx, h.orgID)
	if err == nil {
		t.Fatal("a refused batch commit reported success")
	}
	if errors.Is(err, errSessionShimAmbiguousBatchCommit) {
		t.Fatalf("a decoded refusal was classified outcome-unknown: %v", err)
	}
	if got := d.reconcilingScopes(); got != 0 {
		t.Fatalf("a decoded refusal armed %d reconciliation passes, want none", got)
	}

	// A negative that has to outlast the loop it forbids: the reconciliation
	// backoff unit is callbackTimeout, so three of them is longer than any
	// immediately-armed pass would stay silent.
	time.Sleep(3 * d.sessionShimConfig().callbackTimeout())
	if got := len(h.refreshes()); got != refreshesBefore {
		t.Fatalf("a decoded refusal drove %d credential refreshes, want none", got-refreshesBefore)
	}
	if got := len(f.committedBatches()); got != commitsBefore+1 {
		t.Fatalf("a decoded refusal was retried: %d commits, want exactly the refused one", got-commitsBefore)
	}
	if receipt, ok := d.SessionShimScopeAuthority(h.orgID); !ok || receipt.AdoptionRevision != "revision-1" {
		t.Fatalf("retained authority after a refusal = %+v (%v), want revision-1 untouched", receipt, ok)
	}
	if projected := d.QuarantinedSessions(); len(projected) != 1 {
		t.Fatalf("refusal left %d lineages projected, want the quarantine retained", len(projected))
	}
}

// TestReconciliationExhaustsItsDerivedBoundAndKeepsServing: a control plane
// that refuses every republish exhausts the derived attempt bound
// (sessionShimAdoptionPublicationStages) and leaves the daemon serving and
// beating the last-committed projection — no crash, no silence, no unbounded
// retry.
func TestReconciliationExhaustsItsDerivedBoundAndKeepsServing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, f := newReconciliationFixture(ctx, t)
	d := h.daemon
	stageReconciliationQuarantine(t, d, h.orgID)
	refreshesBefore := len(h.refreshes())
	commitsBefore := len(f.committedBatches())

	// The trigger is ambiguous; every republish after it is REFUSED (the
	// composer's compare-and-swap keeps saying no). The server never actually
	// committed, so refreshes keep answering the retained revision.
	h.setRefreshReceiptRevision("revision-1")
	var ambiguousOnce sync.Once
	f.setCommit(func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		err := errors.New("adoption revision compare-and-swap refused (closed code, decoded 4xx)")
		ambiguousOnce.Do(func() { err = transportLostCommitAnswer() })
		return SessionShimAdoptionBatchReceipt{}, err
	})

	if err := d.republishSessionShimProjection(ctx, h.orgID); err == nil {
		t.Fatal("the ambiguous trigger reported success")
	}

	wantCommits := commitsBefore + 1 + sessionShimAdoptionPublicationStages
	waitForCondition(t, 10*time.Second, "the reconciliation loop to spend its derived attempt bound", func() bool {
		return len(f.committedBatches()) >= wantCommits && d.reconcilingScopes() == 0
	})
	// Outlast one more would-be backoff+attempt: the bound must hold, not lag.
	time.Sleep(3 * d.sessionShimConfig().callbackTimeout())
	if got := len(f.committedBatches()); got != wantCommits {
		t.Fatalf("republish commits = %d, want exactly %d — the initial ambiguous trigger plus the "+
			"derived attempt bound (sessionShimAdoptionPublicationStages)", got-commitsBefore, wantCommits-commitsBefore)
	}
	if got := len(h.refreshes()) - refreshesBefore; got != sessionShimAdoptionPublicationStages {
		t.Fatalf("reconciliation drove %d refreshes, want one per bounded attempt (%d)",
			got, sessionShimAdoptionPublicationStages)
	}
	if got := d.reconcilingScopes(); got != 0 {
		t.Fatalf("%d reconciliation passes still armed after exhaustion", got)
	}

	// Serving and beating the last-committed projection: no crash, no silence.
	if state := d.State(); state != StateRunning {
		t.Fatalf("daemon state after exhaustion = %q, want %q", state, StateRunning)
	}
	if err := d.heartbeat.SendNow(ctx); err != nil {
		t.Fatalf("beat after exhaustion: %v", err)
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil || beat.SessionShim.AdoptionRevision != "revision-1" {
		t.Fatalf("beat after exhaustion = %+v, want the last-committed revision-1 projection", beat.SessionShim)
	}
}
