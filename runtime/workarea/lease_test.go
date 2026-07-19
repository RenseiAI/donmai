package workarea_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// Preserve the task-#78 public source shape for downstream unkeyed literals.
var _ = workarea.StoreOptions{"", nil}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testPolicy() workarea.LeasePolicy {
	return workarea.LeasePolicy{
		SettlementBudget: time.Minute,
		SafetyMargin:     time.Second,
		LeaseDuration:    2 * time.Minute,
		MaxLeaseDuration: 5 * time.Minute,
	}
}

func acquireSpec(sessionID, resultID, workareaID string) workarea.AcquireSpec {
	return workarea.AcquireSpec{
		SessionID:        sessionID,
		TerminalResultID: resultID,
		WorkareaID:       workareaID,
		WorkareaPath:     filepath.Join("/tmp", workareaID),
		Policy:           testPolicy(),
	}
}

func claimLease(t *testing.T, store *workarea.LeaseStore, lease *workarea.TerminalLease, invocationID, claimID string) {
	t.Helper()
	if _, err := store.ClaimExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID:          lease.LeaseID,
		SessionID:        lease.SessionID,
		TerminalResultID: lease.TerminalResultID,
		WorkareaID:       lease.WorkareaID,
		InvocationID:     invocationID,
		ClaimID:          claimID,
	}); err != nil {
		t.Fatalf("ClaimExecution: %v", err)
	}
}

func acknowledgement(lease *workarea.TerminalLease, invocationID, claimID string) workarea.TerminalResultAcknowledgement {
	return workarea.TerminalResultAcknowledgement{
		SchemaVersion:    workarea.TerminalLeaseAcknowledgementSchemaV1,
		LeaseID:          lease.LeaseID,
		SessionID:        lease.SessionID,
		TerminalResultID: lease.TerminalResultID,
		WorkareaID:       lease.WorkareaID,
		InvocationID:     invocationID,
		ClaimID:          claimID,
		Acknowledged:     true,
	}
}

func TestLeaseAcquireIsDurableAndIdempotent(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	dir := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	second, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatalf("Acquire replay: %v", err)
	}
	if first.LeaseID != second.LeaseID || !first.AcquiredAt.Equal(second.AcquiredAt) {
		t.Fatalf("idempotent acquire changed lease: first=%+v second=%+v", first, second)
	}

	reopened, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	recovered, err := reopened.Get(first.LeaseID)
	if err != nil {
		t.Fatalf("recover lease: %v", err)
	}
	if recovered.State != workarea.LeaseActive || recovered.WorkareaID != "workarea-1" {
		t.Fatalf("recovered lease = %+v", recovered)
	}
}

func TestLeaseSameWorkareaExcludesRacingSession(t *testing.T) {
	t.Parallel()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, spec := range []workarea.AcquireSpec{
		acquireSpec("session-1", "result-1", "shared-workarea"),
		acquireSpec("session-2", "result-2", "shared-workarea"),
	} {
		spec := spec
		go func() {
			<-start
			_, acquireErr := store.Acquire(context.Background(), spec)
			results <- acquireErr
		}()
	}
	close(start)

	var acquired, excluded int
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			acquired++
		case errors.Is(err, workarea.ErrWorkareaLeased):
			excluded++
		default:
			t.Fatalf("Acquire error = %v", err)
		}
	}
	if acquired != 1 || excluded != 1 {
		t.Fatalf("acquired=%d excluded=%d, want 1/1", acquired, excluded)
	}
}

func TestLeaseReleaseRequiresMatchingSemanticAcknowledgementAndIsIdempotent(t *testing.T) {
	t.Parallel()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatal(err)
	}
	if retained, err := store.RequestRelease(context.Background(), lease.WorkareaID); err != nil || !retained {
		t.Fatalf("RequestRelease retained=%v err=%v", retained, err)
	}

	claimLease(t, store, lease, "invocation-1", "claim-1")
	var releases atomic.Int32
	releaser := func(context.Context, workarea.TerminalLease) error {
		releases.Add(1)
		return nil
	}
	missingSchema := acknowledgement(lease, "invocation-1", "claim-1")
	missingSchema.SchemaVersion = ""
	if _, err := store.Acknowledge(context.Background(), missingSchema, releaser); err == nil {
		t.Fatal("missing acknowledgement schema was accepted")
	}

	bad := acknowledgement(lease, "invocation-1", "claim-1")
	bad.TerminalResultID = "different-result"
	if _, err := store.Acknowledge(context.Background(), bad, releaser); !errors.Is(err, workarea.ErrLeaseConflict) {
		t.Fatalf("mismatched acknowledgement error = %v, want ErrLeaseConflict", err)
	}
	if releases.Load() != 0 {
		t.Fatal("mismatched acknowledgement invoked release")
	}

	ack := acknowledgement(lease, "invocation-1", "claim-1")
	for i := 0; i < 2; i++ {
		released, err := store.Acknowledge(context.Background(), ack, releaser)
		if err != nil {
			t.Fatalf("Acknowledge %d: %v", i+1, err)
		}
		if released.State != workarea.LeaseReleased {
			t.Fatalf("Acknowledge %d state = %q", i+1, released.State)
		}
	}
	if releases.Load() != 1 {
		t.Fatalf("release calls = %d, want 1", releases.Load())
	}
}

func TestLeaseAcknowledgementPersistsReleasePendingBeforeDisposition(t *testing.T) {
	t.Parallel()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatal(err)
	}
	claimLease(t, store, lease, "invocation-1", "claim-1")

	ack := acknowledgement(lease, "invocation-1", "claim-1")
	released, err := store.Acknowledge(context.Background(), ack, func(_ context.Context, pending workarea.TerminalLease) error {
		if pending.State != workarea.LeaseReleasePending || pending.AcknowledgedAt == nil {
			t.Fatalf("callback lease = %+v, want acknowledged release-pending", pending)
		}
		durable, getErr := store.Get(lease.LeaseID)
		if getErr != nil {
			return getErr
		}
		if durable.State != workarea.LeaseReleasePending || durable.AcknowledgedAt == nil {
			t.Fatalf("durable lease during release = %+v", durable)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if released.State != workarea.LeaseReleased || released.ReleasedAt == nil {
		t.Fatalf("released lease = %+v", released)
	}
}

func TestConcurrentAcknowledgementReleasesOnce(t *testing.T) {
	t.Parallel()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatal(err)
	}
	claimLease(t, store, lease, "invocation-1", "claim-1")
	ack := acknowledgement(lease, "invocation-1", "claim-1")

	var releases atomic.Int32
	releaseStarted := make(chan struct{})
	allowRelease := make(chan struct{})
	releaser := func(context.Context, workarea.TerminalLease) error {
		if releases.Add(1) == 1 {
			close(releaseStarted)
		}
		<-allowRelease
		return nil
	}
	errs := make(chan error, 2)
	go func() {
		_, releaseErr := store.Acknowledge(context.Background(), ack, releaser)
		errs <- releaseErr
	}()
	<-releaseStarted
	go func() {
		_, releaseErr := store.Acknowledge(context.Background(), ack, releaser)
		errs <- releaseErr
	}()
	close(allowRelease)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("Acknowledge %d: %v", i+1, err)
		}
	}
	if releases.Load() != 1 {
		t.Fatalf("release calls = %d, want 1", releases.Load())
	}
}

func TestLeaseExecutionClaimIsExclusiveAndAcknowledgementIdentityBound(t *testing.T) {
	t.Parallel()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatal(err)
	}

	type claimResult struct {
		invocationID string
		claimID      string
		err          error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	for _, identity := range [][2]string{{"invocation-a", "claim-a"}, {"invocation-b", "claim-b"}} {
		identity := identity
		go func() {
			<-start
			_, claimErr := store.ClaimExecution(context.Background(), workarea.ExecutionClaimSpec{
				LeaseID:          lease.LeaseID,
				SessionID:        lease.SessionID,
				TerminalResultID: lease.TerminalResultID,
				WorkareaID:       lease.WorkareaID,
				InvocationID:     identity[0],
				ClaimID:          identity[1],
			})
			results <- claimResult{invocationID: identity[0], claimID: identity[1], err: claimErr}
		}()
	}
	close(start)

	var winner, loser claimResult
	for i := 0; i < 2; i++ {
		result := <-results
		switch {
		case result.err == nil:
			winner = result
		case errors.Is(result.err, workarea.ErrLeaseExecutionClaimed):
			loser = result
		default:
			t.Fatalf("ClaimExecution: %v", result.err)
		}
	}
	if winner.invocationID == "" || loser.invocationID == "" {
		t.Fatalf("winner=%+v loser=%+v", winner, loser)
	}
	recovered, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: store.Dir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.ClaimExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID:          lease.LeaseID,
		SessionID:        lease.SessionID,
		TerminalResultID: lease.TerminalResultID,
		WorkareaID:       lease.WorkareaID,
		InvocationID:     winner.invocationID,
		ClaimID:          winner.claimID,
	}); err != nil {
		t.Fatalf("recovered idempotent claim: %v", err)
	}

	var releases atomic.Int32
	releaser := func(context.Context, workarea.TerminalLease) error {
		releases.Add(1)
		return nil
	}
	if _, err := recovered.Acknowledge(context.Background(), acknowledgement(lease, loser.invocationID, loser.claimID), releaser); !errors.Is(err, workarea.ErrLeaseExecutionConflict) {
		t.Fatalf("losing acknowledgement error = %v", err)
	}
	if releases.Load() != 0 {
		t.Fatal("losing verifier released the lease")
	}
	if _, err := recovered.Acknowledge(context.Background(), acknowledgement(lease, winner.invocationID, winner.claimID), releaser); err != nil {
		t.Fatalf("winning acknowledgement: %v", err)
	}
	if releases.Load() != 1 {
		t.Fatalf("release calls = %d, want 1", releases.Load())
	}
	if _, err := recovered.Acknowledge(context.Background(), acknowledgement(lease, loser.invocationID, loser.claimID), releaser); !errors.Is(err, workarea.ErrLeaseExecutionConflict) {
		t.Fatalf("wrong duplicate acknowledgement error = %v", err)
	}
	if releases.Load() != 1 {
		t.Fatalf("wrong duplicate acknowledgement repeated release: %d", releases.Load())
	}
}

func TestTerminalResultOutboxReplaysAfterRestart(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	dir := t.TempDir()
	spec := acquireSpec("session-1", "result-1", "workarea-1")
	spec.TerminalResultPayload = []byte(`{"status":"completed"}`)
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if lease.TerminalResultPost == nil || lease.TerminalResultPost.State != workarea.TerminalResultPostPending {
		t.Fatalf("outbox was not acquired atomically: %+v", lease.TerminalResultPost)
	}

	recovered, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.Acquire(context.Background(), spec); err != nil {
		t.Fatalf("idempotent outbox acquire after restart: %v", err)
	}
	var calls atomic.Int32
	considered, err := recovered.ReplayTerminalResults(context.Background(), 1, time.Second, func(_ context.Context, got workarea.TerminalLease, payload json.RawMessage) error {
		calls.Add(1)
		var compact bytes.Buffer
		if compactErr := json.Compact(&compact, payload); compactErr != nil {
			return compactErr
		}
		if got.LeaseID != lease.LeaseID || compact.String() != string(spec.TerminalResultPayload) {
			return errors.New("replayed terminal result identity or payload differs")
		}
		return nil
	})
	if err != nil || considered != 1 || calls.Load() != 1 {
		t.Fatalf("ReplayTerminalResults considered=%d calls=%d err=%v", considered, calls.Load(), err)
	}
	observed, err := recovered.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.TerminalResultPost.State != workarea.TerminalResultPostObserved || observed.TerminalResultPost.ObservedAt == nil {
		t.Fatalf("observed outbox = %+v", observed.TerminalResultPost)
	}
}

func TestTerminalResultOutboxExpiresWithoutReplay(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	spec := acquireSpec("session-1", "result-1", "workarea-1")
	spec.TerminalResultPayload = []byte(`{"status":"completed"}`)
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(3 * time.Minute)
	considered, err := store.ReplayTerminalResults(context.Background(), 1, time.Second, func(context.Context, workarea.TerminalLease, json.RawMessage) error {
		t.Fatal("expired terminal result was replayed")
		return nil
	})
	if err != nil || considered != 1 {
		t.Fatalf("ReplayTerminalResults considered=%d err=%v", considered, err)
	}
	expired, err := store.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if expired.TerminalResultPost.State != workarea.TerminalResultPostExpired || expired.TerminalResultPost.ExpiredAt == nil {
		t.Fatalf("expired outbox = %+v", expired.TerminalResultPost)
	}
}

func TestExpiredExecutionClaimCannotAcknowledgeAndReaperReclaims(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatal(err)
	}
	claimLease(t, store, lease, "invocation-1", "claim-1")
	clock.Advance(3 * time.Minute)
	var releases atomic.Int32
	releaser := func(context.Context, workarea.TerminalLease) error {
		releases.Add(1)
		return nil
	}
	if _, err := store.Acknowledge(context.Background(), acknowledgement(lease, "invocation-1", "claim-1"), releaser); !errors.Is(err, workarea.ErrLeaseExpired) {
		t.Fatalf("expired acknowledgement error = %v", err)
	}
	if releases.Load() != 0 {
		t.Fatal("expired acknowledgement released workarea")
	}
	considered, err := store.ReapExpired(context.Background(), 1, time.Second, releaser)
	if err != nil || considered != 1 || releases.Load() != 1 {
		t.Fatalf("ReapExpired considered=%d releases=%d err=%v", considered, releases.Load(), err)
	}
}

func TestLeaseRenewHonorsAbsoluteMaximum(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	renewed, err := store.Renew(context.Background(), workarea.RenewSpec{
		LeaseID:          lease.LeaseID,
		SessionID:        lease.SessionID,
		TerminalResultID: lease.TerminalResultID,
		WorkareaID:       lease.WorkareaID,
		Duration:         2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if !renewed.ExpiresAt.Equal(clock.Now().Add(2 * time.Minute)) {
		t.Fatalf("renewed expiry = %s", renewed.ExpiresAt)
	}
	_, err = store.Renew(context.Background(), workarea.RenewSpec{
		LeaseID:          lease.LeaseID,
		SessionID:        lease.SessionID,
		TerminalResultID: lease.TerminalResultID,
		WorkareaID:       lease.WorkareaID,
		Duration:         5 * time.Minute,
	})
	if err == nil {
		t.Fatal("renewal beyond max expiry succeeded")
	}
}

func TestExpiredLeaseReaperReleasesAfterCrashBeforeTeardownRequest(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(3 * time.Minute)

	var releases atomic.Int32
	considered, err := store.ReapExpired(context.Background(), 1, time.Second, func(context.Context, workarea.TerminalLease) error {
		releases.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if considered != 1 || releases.Load() != 1 {
		t.Fatalf("considered=%d releases=%d, want 1/1", considered, releases.Load())
	}
	reaped, err := store.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if reaped.State != workarea.LeaseReleased || reaped.AcknowledgedAt != nil {
		t.Fatalf("reaped lease = %+v", reaped)
	}
}

func TestReleaseFailureStaysUnavailableForRetry(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec("session-1", "result-1", "workarea-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.RequestRelease(context.Background(), lease.WorkareaID)
	clock.Advance(3 * time.Minute)

	wantErr := errors.New("provider unavailable")
	if _, err := store.ReapExpired(context.Background(), 1, time.Second, func(context.Context, workarea.TerminalLease) error {
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("ReapExpired error = %v, want provider error", err)
	}
	failed, err := store.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != workarea.LeaseReleasePending || !failed.RetainsWorkarea() || failed.LastReleaseError == "" || failed.NextReleaseAttempt == nil {
		t.Fatalf("failed release did not remain unavailable with bounded retry: %+v", failed)
	}
	considered, err := store.ReapExpired(context.Background(), 1, time.Second, func(context.Context, workarea.TerminalLease) error {
		t.Fatal("reaper retried before backoff elapsed")
		return nil
	})
	if err != nil || considered != 0 {
		t.Fatalf("backoff pass considered=%d err=%v", considered, err)
	}
}

func TestFailedReapDoesNotStarveAnotherExpiredWorkarea(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Acquire(context.Background(), acquireSpec("session-a", "result-a", "workarea-a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Acquire(context.Background(), acquireSpec("session-b", "result-b", "workarea-b"))
	if err != nil {
		t.Fatal(err)
	}
	if second.LeaseID < first.LeaseID {
		first, second = second, first
	}
	clock.Advance(3 * time.Minute)

	providerErr := errors.New("provider unavailable")
	if _, err := store.ReapExpired(context.Background(), 1, time.Second, func(_ context.Context, lease workarea.TerminalLease) error {
		if lease.LeaseID == first.LeaseID {
			return providerErr
		}
		return nil
	}); !errors.Is(err, providerErr) {
		t.Fatalf("first ReapExpired error = %v, want provider error", err)
	}
	considered, err := store.ReapExpired(context.Background(), 1, time.Second, func(_ context.Context, lease workarea.TerminalLease) error {
		if lease.LeaseID != second.LeaseID {
			t.Fatalf("second pass retried %s before untouched %s", lease.LeaseID, second.LeaseID)
		}
		return nil
	})
	if err != nil || considered != 1 {
		t.Fatalf("second ReapExpired considered=%d err=%v", considered, err)
	}
	released, err := store.Get(second.LeaseID)
	if err != nil || released.State != workarea.LeaseReleased {
		t.Fatalf("second lease = %+v err=%v", released, err)
	}
}

func TestReapExpiredStartsBatchReleaseAttemptsConcurrently(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		lease, acquireErr := store.Acquire(context.Background(), acquireSpec("session-"+id, "result-"+id, "workarea-"+id))
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		if retained, releaseErr := store.RequestRelease(context.Background(), lease.WorkareaID); releaseErr != nil || !retained {
			t.Fatalf("RequestRelease %s retained=%v err=%v", id, retained, releaseErr)
		}
	}
	clock.Advance(3 * time.Minute)

	allStarted := make(chan struct{})
	var started atomic.Int32
	var timedOutBeforePeer atomic.Bool
	considered, err := store.ReapExpired(context.Background(), 2, 2*time.Second, func(ctx context.Context, _ workarea.TerminalLease) error {
		if started.Add(1) == 2 {
			close(allStarted)
		}
		select {
		case <-allStarted:
			return nil
		case <-ctx.Done():
			timedOutBeforePeer.Store(true)
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if considered != 2 || started.Load() != 2 {
		t.Fatalf("considered=%d started=%d", considered, started.Load())
	}
	if timedOutBeforePeer.Load() {
		t.Fatal("reaper ran provider attempts sequentially")
	}
}

func TestDifferentWorkareasRemainParallelDuringRelease(t *testing.T) {
	t.Parallel()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	leaseA, err := store.Acquire(context.Background(), acquireSpec("session-a", "result-a", "workarea-a"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.RequestRelease(context.Background(), leaseA.WorkareaID)
	claimLease(t, store, leaseA, "invocation-a", "claim-a")

	releaseStarted := make(chan struct{})
	allowRelease := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, releaseErr := store.Acknowledge(context.Background(), acknowledgement(leaseA, "invocation-a", "claim-a"), func(context.Context, workarea.TerminalLease) error {
			close(releaseStarted)
			<-allowRelease
			return nil
		})
		done <- releaseErr
	}()
	<-releaseStarted

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := store.Acquire(ctx, acquireSpec("session-b", "result-b", "workarea-b")); err != nil {
		t.Fatalf("independent Acquire blocked by another workarea release: %v", err)
	}
	close(allowRelease)
	if err := <-done; err != nil {
		t.Fatalf("release A: %v", err)
	}
}
