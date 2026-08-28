package daemon

// Provenance: cleared-disposition-acceptance-clear-2026-08-27 — grep for this
// marker to prove a build carries the explicit abandoned-disposition clear.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

func newAcceptanceClearedFixture(t *testing.T, onBatch func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error)) (*Daemon, sessionshim.Identity, shimIncarnation) {
	t.Helper()
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:     t.TempDir(),
		HostIDForOrg:    func(context.Context, string) (string, error) { return "wh_test_host", nil },
		OnAdoptionBatch: onBatch,
	}})
	id := sessionshim.Identity{OrgID: "org-cleared", SessionID: "session-cleared"}
	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-cleared", ProcessEpoch: 9,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineProtocolMismatch, "acceptance fixture", time.Now())
	q.ControllerGeneration = 3
	incarnation := shimIncarnation{identity: id, shimID: "shim-cleared", processEpoch: 9}
	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(q)
	// Our own pid with a deliberately wrong start time: Alive() reports the
	// recorded process as gone without depending on some pid being free.
	d.shims.acceptanceQuarantine[incarnation] = sessionshim.ProcessIdentity{PID: os.Getpid(), StartedAt: 1}
	d.shims.mu.Unlock()
	return d, id, incarnation
}

// An acceptance clear must not make the next complete batch simply OMIT the
// lineage — a composer that still holds an active recovery obligation refuses
// an incomplete batch, and the host then argues with the control plane until it
// is demoted to draining. The clear instead publishes the lineage in the
// batch's cleared section with the explicit abandoned disposition, keeps
// projecting it quarantined on the heartbeat while the commit is in flight, and
// forgets it locally only once the batch receipt echoes it back.
func TestAcceptanceClearPublishesAbandonedDisposition(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var published []SessionShimAdoptionBatch
	var projectedDuringCommit []sessionshim.QuarantinedSession
	var d *Daemon
	d, id, incarnation := newAcceptanceClearedFixture(t,
		func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			published = append(published, cloneSessionShimAdoptionBatch(batch))
			mu.Unlock()
			// Sample the beat projection while the commit is in flight: the
			// last COMMITTED batch still carries the lineage quarantined, so a
			// beat sent now must still carry it too.
			projectedDuringCommit = d.QuarantinedSessions()
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("rev-2"), AdoptionRevision: "2",
				Cleared: append([]SessionShimClearedQuarantine(nil), batch.Cleared...),
			}, nil
		})

	if err := d.clearSessionShimAcceptanceQuarantine(incarnation); err != nil {
		t.Fatalf("clear: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published %d batches, want exactly one", len(published))
	}
	batch := published[0]
	if len(batch.Cleared) != 1 {
		t.Fatalf("published batch carried %d cleared entries, want one — a batch that simply omits "+
			"a lineage the composer still holds is refused as incomplete", len(batch.Cleared))
	}
	want := SessionShimClearedQuarantine{
		OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-cleared", ProcessEpoch: 9, ControllerGeneration: 3,
		Disposition: SessionShimDispositionAbandoned,
		Reason:      SessionShimClearedReasonAcceptanceClearWithoutTerminalEvidence,
	}
	if batch.Cleared[0] != want {
		t.Fatalf("cleared entry = %+v, want %+v", batch.Cleared[0], want)
	}
	if len(batch.Quarantined) != 0 {
		t.Fatalf("cleared lineage still occupies the quarantined section: %+v", batch.Quarantined)
	}
	if len(batch.Tombstoned) != 0 {
		t.Fatalf("acceptance clear manufactured terminal evidence: %+v", batch.Tombstoned)
	}
	if len(projectedDuringCommit) != 1 {
		t.Fatalf("beat projection during the in-flight commit had %d quarantined entries, want 1 — "+
			"dropping the lineage before the receipt confirms is exactly what made the next beat "+
			"disagree with the last committed set", len(projectedDuringCommit))
	}
	if after := d.QuarantinedSessions(); len(after) != 0 {
		t.Fatalf("confirmed clear left %d entries projected quarantined", len(after))
	}
	d.shims.mu.RLock()
	_, stillArmed := d.shims.acceptanceQuarantine[incarnation]
	d.shims.mu.RUnlock()
	if stillArmed {
		t.Fatal("confirmed clear left the acceptance bookkeeping armed")
	}
}

// A receipt that does not echo the cleared entry is refused, and the lineage
// stays projected AND staged: the daemon has no confirmation the composer's
// obligation moved to abandoned, so forgetting the lineage now would be the
// same silent omission the cleared section exists to prevent. The abandonment
// intent survives the refusal — the next publish carries the entry again.
func TestClearedReceiptEchoRefusalRetainsTheQuarantine(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var published []SessionShimAdoptionBatch
	d, id, incarnation := newAcceptanceClearedFixture(t,
		func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			published = append(published, cloneSessionShimAdoptionBatch(batch))
			mu.Unlock()
			// A legacy-shaped receipt: durable, revisioned, and silent about the
			// cleared section it was asked to commit.
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-2"), AdoptionRevision: "2"}, nil
		})

	err := d.clearSessionShimAcceptanceQuarantine(incarnation)
	if err == nil || !strings.Contains(err.Error(), "cleared") {
		t.Fatalf("clear err = %v, want a refusal naming the missing cleared echo", err)
	}

	if beat := d.QuarantinedSessions(); len(beat) != 1 {
		t.Fatalf("refused echo left %d entries projected, want the lineage retained", len(beat))
	}
	d.shims.mu.RLock()
	_, stillArmed := d.shims.acceptanceQuarantine[incarnation]
	d.shims.mu.RUnlock()
	if !stillArmed {
		t.Fatal("refused echo dropped the acceptance bookkeeping")
	}

	// The staged abandonment survives: a later projection publish still carries
	// the cleared entry rather than reverting to a plain quarantined row.
	d.publishSessionShimProjection(context.Background(), id.OrgID)
	mu.Lock()
	defer mu.Unlock()
	if len(published) < 2 {
		t.Fatalf("republished %d batches, want the retry to publish again", len(published))
	}
	last := published[len(published)-1]
	if len(last.Cleared) != 1 || len(last.Quarantined) != 0 {
		t.Fatalf("retry batch carried %d cleared / %d quarantined, want the staged clear to persist",
			len(last.Cleared), len(last.Quarantined))
	}
}

// Control: the production remover is untouched. A quarantined lineage with a
// durable group-reaped tombstone still leaves through terminal evidence — the
// tombstone lane — and never through the cleared section, which exists solely
// for a lineage that has NO terminal evidence to report.
func TestTerminalEvidenceRemoverStillTombstonesNeverClears(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var mu sync.Mutex
	var published []SessionShimAdoptionBatch
	var terminal []SessionShimTerminalEvidence
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:  dir,
		HostIDForOrg: func(context.Context, string) (string, error) { return "wh_test_host", nil },
		OnTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
			mu.Lock()
			defer mu.Unlock()
			terminal = append(terminal, evidence)
			return nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("rev-1"), AdoptionRevision: "1",
				Cleared: append([]SessionShimClearedQuarantine(nil), batch.Cleared...),
			}, nil
		},
	}})

	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-terminal", SessionID: "session-terminal"}
	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-terminal", ProcessEpoch: 5,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineSocketUnreachable, "controller stream ended before a terminal observation", time.Now())
	tombstone := sessionshim.Tombstone{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-terminal", ProcessEpoch: 5,
		GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.PutTombstone(tombstone); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(q)
	d.shims.registry = registry
	d.shims.mu.Unlock()

	d.reconcileQuarantinedTombstones()

	mu.Lock()
	defer mu.Unlock()
	if len(terminal) != 1 {
		t.Fatalf("terminal evidence reported %d times, want exactly once", len(terminal))
	}
	if terminal[0].Absent != nil {
		t.Fatalf("tombstone-backed removal reported an absent attestation: %+v", terminal[0].Absent)
	}
	if !terminal[0].Tombstone.GroupReaped {
		t.Fatalf("terminal evidence lost the reap proof: %+v", terminal[0].Tombstone)
	}
	for i, batch := range published {
		if len(batch.Cleared) != 0 {
			t.Fatalf("batch %d carried cleared entries for a tombstoned lineage: %+v", i, batch.Cleared)
		}
	}
	d.shims.mu.RLock()
	remaining := len(d.shims.quarantined)
	d.shims.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("terminal remover left %d quarantined entries", remaining)
	}
}
