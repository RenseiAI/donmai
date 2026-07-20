package workarea_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

const (
	sessionA    = "11111111-1111-4111-8111-111111111111"
	sessionB    = "22222222-2222-4222-8222-222222222222"
	invocationA = "33333333-3333-4333-8333-333333333333"
	invocationB = "44444444-4444-4444-8444-444444444444"
	claimA      = "55555555-5555-4555-8555-555555555555"
	claimB      = "66666666-6666-4666-8666-666666666666"
	resultA     = "tr_11111111111111111111111111111111"
	resultB     = "tr_22222222222222222222222222222222"
	workareaA   = "wa_11111111111111111111111111111111"
	workareaB   = "wa_22222222222222222222222222222222"
	receiverA   = "rcv_11111111111111111111111111111111"
)

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

func (c *testClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

func (c *testClock) Advance(delta time.Duration) { c.Set(c.Now().Add(delta)) }

func acquireSpec(root, sessionID, resultID, workareaID string) workarea.AcquireSpec {
	return workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: resultID, WorkareaID: workareaID,
		WorkareaPath: filepath.Join(root, workareaID), Policy: workarea.DefaultLeasePolicy(),
		ReleaseRequested: true, ReleaseDisposition: "destroy",
	}
}

func claimSpec(lease *workarea.TerminalLease, invocationID, claimID string) workarea.ExecutionClaimSpec {
	return workarea.ExecutionClaimSpec{
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: invocationID, ClaimID: claimID,
	}
}

func acknowledgement(lease *workarea.TerminalLease, invocationID, claimID string) workarea.TerminalResultAcknowledgement {
	return workarea.TerminalResultAcknowledgement{
		SchemaVersion: workarea.TerminalLeaseAcknowledgementSchemaV1, Acknowledged: true,
		InvocationID: invocationID, ClaimID: claimID, LeaseID: lease.LeaseID,
		SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID, WorkareaID: lease.WorkareaID,
	}
}

func TestLeaseAcquireIsDurableInvariantCompleteAndExclusive(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases"), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	spec := acquireSpec(root, sessionA, resultA, workareaA)
	first, err := store.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if first.LeaseID != second.LeaseID || !first.AcquiredAt.Equal(second.AcquiredAt) {
		t.Fatalf("idempotent acquire changed lease: first=%+v second=%+v", first, second)
	}
	changed := spec
	changed.WorkareaPath = filepath.Join(root, "other")
	if _, err := store.Acquire(context.Background(), changed); !errors.Is(err, workarea.ErrLeaseConflict) {
		t.Fatalf("changed invariant error = %v", err)
	}
	competing := acquireSpec(root, sessionB, resultB, workareaA)
	if _, err := store.Acquire(context.Background(), competing); !errors.Is(err, workarea.ErrWorkareaLeased) {
		t.Fatalf("competing workarea error = %v", err)
	}
	reopened, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: store.Dir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if retained, err := reopened.RetainedPath(spec.WorkareaPath); err != nil || !retained {
		t.Fatalf("retained path=%v err=%v", retained, err)
	}
}

func TestClaimUsesStrictBoundaryAndReturnsCommittedClockSample(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, remainingMS int64, wantAccepted bool) {
		t.Helper()
		clock := newTestClock()
		root := t.TempDir()
		store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases"), Now: clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		lease, err := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
		if err != nil {
			t.Fatal(err)
		}
		clock.Set(time.UnixMilli(lease.ExpiresAt.UnixMilli() - remainingMS).UTC())
		got, err := store.ClaimExecution(context.Background(), claimSpec(lease, invocationA, claimA))
		if !wantAccepted {
			if !errors.Is(err, workarea.ErrInsufficientLeaseTime) {
				t.Fatalf("remaining=%d error=%v", remainingMS, err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.ClaimNowMS != got.Claim.ClaimedAt.UnixMilli() || got.ClaimNowMS != clock.Now().UnixMilli() {
			t.Fatalf("claim clock mismatch: %+v clock=%d", got, clock.Now().UnixMilli())
		}
		replayed, err := store.ClaimExecution(context.Background(), claimSpec(lease, invocationA, claimA))
		if err != nil || replayed.ClaimNowMS != got.ClaimNowMS {
			t.Fatalf("idempotent claim = %+v err=%v", replayed, err)
		}
	}
	t.Run("1037000 rejected", func(t *testing.T) { run(t, 1_037_000, false) })
	t.Run("1037001 accepted", func(t *testing.T) { run(t, 1_037_001, true) })
}

func TestQueueAdmissionUsesStrictOneMillisecondBoundary(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, remainingMS int64, wantAccepted bool) {
		t.Helper()
		clock := newTestClock()
		root := t.TempDir()
		store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases"), Now: clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		lease, err := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
		if err != nil {
			t.Fatal(err)
		}
		clock.Set(time.UnixMilli(lease.ExpiresAt.UnixMilli() - remainingMS).UTC())
		_, err = store.CheckQueueAdmission(context.Background(), lease.LeaseID, lease.SessionID, lease.TerminalResultID, lease.WorkareaID)
		if wantAccepted && err != nil {
			t.Fatal(err)
		}
		if !wantAccepted && !errors.Is(err, workarea.ErrInsufficientLeaseTime) {
			t.Fatalf("remaining=%d error=%v", remainingMS, err)
		}
	}
	t.Run("1097000 rejected", func(t *testing.T) { run(t, 1_097_000, false) })
	t.Run("1097001 accepted", func(t *testing.T) { run(t, 1_097_001, true) })
}

func TestClaimIsExclusiveAndCanonicalIdentityBound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, spec := range []workarea.ExecutionClaimSpec{claimSpec(lease, invocationA, claimA), claimSpec(lease, invocationB, claimB)} {
		spec := spec
		go func() {
			<-start
			_, err := store.ClaimExecution(context.Background(), spec)
			errs <- err
		}()
	}
	close(start)
	var accepted, conflicted int
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, workarea.ErrLeaseExecutionClaimed):
			conflicted++
		default:
			t.Fatalf("claim error = %v", err)
		}
	}
	if accepted != 1 || conflicted != 1 {
		t.Fatalf("accepted=%d conflicted=%d", accepted, conflicted)
	}
}

func TestRenewalAndBodySaveUseCASAndBodyFreezesExpiry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := store.Renew(context.Background(), workarea.RenewSpec{
			LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
			WorkareaID: lease.WorkareaID, Extension: time.Minute,
		})
		results <- err
	}()
	go func() {
		<-start
		_, err := store.SaveTerminalStatus(context.Background(), workarea.TerminalStatusSaveSpec{
			LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
			WorkareaID: lease.WorkareaID, ReceiverKey: receiverA, Body: []byte(`{"status":"completed"}`),
			ExpectedExpiresAt: lease.ExpiresAt,
		})
		results <- err
	}()
	close(start)
	var success, conflict int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			success++
		case errors.Is(err, workarea.ErrLeaseConflict) || errors.Is(err, workarea.ErrRenewalAfterBodySave):
			conflict++
		default:
			t.Fatalf("race error = %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	persisted, err := store.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.TerminalStatus != nil {
		if _, err := store.Renew(context.Background(), workarea.RenewSpec{
			LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
			WorkareaID: lease.WorkareaID, Extension: time.Minute,
		}); !errors.Is(err, workarea.ErrRenewalAfterBodySave) {
			t.Fatalf("renew after body save error = %v", err)
		}
	} else if !persisted.ExpiresAt.Equal(lease.ExpiresAt.Add(time.Minute)) {
		t.Fatalf("renewal did not extend from prior expiry: %s", persisted.ExpiresAt)
	}
}

func TestOutboxRetainsImmutableBytesAndReceiverAffinityAcrossRestart(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases"), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"workerId":"immutable-worker","terminalWorkareaLease":{"leaseId":"` + lease.LeaseID + `"}}`)
	if _, err := store.SaveTerminalStatus(context.Background(), workarea.TerminalStatusSaveSpec{
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, ReceiverKey: receiverA, Body: body, ExpectedExpiresAt: lease.ExpiresAt,
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: store.Dir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	var gotKey string
	var gotBody []byte
	considered, err := reopened.ReplayTerminalResults(context.Background(), 1, time.Second, func(_ context.Context, key string, replay []byte) error {
		gotKey = key
		gotBody = append([]byte(nil), replay...)
		return nil
	})
	if err != nil || considered != 1 {
		t.Fatalf("replay considered=%d err=%v", considered, err)
	}
	if gotKey != receiverA || string(gotBody) != string(body) {
		t.Fatalf("replay key=%q body=%q", gotKey, gotBody)
	}
	persisted, err := reopened.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.TerminalStatus.DeliveryState != workarea.TerminalStatusDelivered {
		t.Fatalf("delivery state = %s", persisted.TerminalStatus.DeliveryState)
	}
}

func TestReceiverRotationPreservesAffinityAndMissingKeyNeverFallsBack(t *testing.T) {
	t.Parallel()
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer first.Close()
	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer second.Close()
	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterReceiver(receiverA, first.URL); err != nil {
		t.Fatal(err)
	}
	sender := store.TerminalStatusHTTPSender(first.Client(), nil)
	if err := sender(context.Background(), receiverA, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterReceiver(receiverA, second.URL); err != nil {
		t.Fatal(err)
	}
	if err := sender(context.Background(), receiverA, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("receiver calls first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
	missing := "rcv_99999999999999999999999999999999"
	if err := sender(context.Background(), missing, []byte(`{}`)); err == nil {
		t.Fatal("missing receiver key fell back")
	}
	if secondCalls.Load() != 1 {
		t.Fatal("missing receiver key used configured fallback")
	}
}

func TestNonSemanticAcknowledgementCannotAuthorizeRelease(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecution(context.Background(), claimSpec(lease, invocationA, claimA)); err != nil {
		t.Fatal(err)
	}
	ack := acknowledgement(lease, invocationA, claimA)
	ack.Acknowledged = false
	if _, err := store.Acknowledge(context.Background(), ack); !errors.Is(err, workarea.ErrAcknowledgementRequired) {
		t.Fatalf("non-semantic acknowledgement error = %v", err)
	}
	persisted, err := store.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != workarea.LeaseActive || len(persisted.AcknowledgementBytes) != 0 {
		t.Fatalf("non-semantic acknowledgement changed lease: %+v", persisted)
	}
}

func TestAcknowledgementOutcomesAreDurableAndReleaseIsSeparate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.Acknowledge(context.Background(), acknowledgement(lease, invocationA, claimA))
	if err != nil || missing.Outcome != workarea.AcknowledgementRejected || *missing.Reason != workarea.AcknowledgementClaimMissing {
		t.Fatalf("missing claim outcome=%+v err=%v", missing, err)
	}
	if _, err := store.ClaimExecution(context.Background(), claimSpec(lease, invocationA, claimA)); err != nil {
		t.Fatal(err)
	}
	bad := acknowledgement(lease, invocationB, claimB)
	mismatch, err := store.Acknowledge(context.Background(), bad)
	if err != nil || mismatch.Outcome != workarea.AcknowledgementRejected || *mismatch.Reason != workarea.AcknowledgementIdentityMismatch {
		t.Fatalf("mismatch outcome=%+v err=%v", mismatch, err)
	}
	ack := acknowledgement(lease, invocationA, claimA)
	applied, err := store.Acknowledge(context.Background(), ack)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Outcome != workarea.AcknowledgementApplied || applied.LeaseState != workarea.LeaseReleasePending || applied.ProviderReleaseComplete {
		t.Fatalf("applied outcome=%+v", applied)
	}
	persisted, err := store.Get(lease.LeaseID)
	if err != nil || persisted.State != workarea.LeaseReleasePending {
		t.Fatalf("durable state=%+v err=%v", persisted, err)
	}
	duplicate, err := store.Acknowledge(context.Background(), ack)
	if err != nil || duplicate.Outcome != workarea.AcknowledgementAlreadyApplied {
		t.Fatalf("duplicate outcome=%+v err=%v", duplicate, err)
	}
	var releases atomic.Int32
	considered, err := store.ReapExpired(context.Background(), 1, time.Second, func(context.Context, workarea.TerminalLease) error {
		releases.Add(1)
		return nil
	})
	if err != nil || considered != 1 || releases.Load() != 1 {
		t.Fatalf("release considered=%d calls=%d err=%v", considered, releases.Load(), err)
	}
	completed, _ := store.Get(lease.LeaseID)
	if completed.State != workarea.LeaseReleased || completed.AcknowledgementOutcome == nil || !completed.AcknowledgementOutcome.ProviderReleaseComplete {
		t.Fatalf("completed lease=%+v", completed)
	}
}

func TestExpiryAndRepeatedProviderReleaseRemainFailClosed(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases"), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(workarea.DefaultLeaseDuration + time.Millisecond)
	providerErr := errors.New("provider unavailable")
	if _, err := store.ReapExpired(context.Background(), 1, time.Second, func(context.Context, workarea.TerminalLease) error { return providerErr }); !errors.Is(err, providerErr) {
		t.Fatalf("first release error=%v", err)
	}
	pending, _ := store.Get(lease.LeaseID)
	if pending.State != workarea.LeaseReleasePending || pending.ReleaseReason != "expiry" || pending.NextReleaseAttempt == nil {
		t.Fatalf("pending lease=%+v", pending)
	}
	if retained, err := store.Retained(workareaA); err != nil || !retained {
		t.Fatalf("retained=%v err=%v", retained, err)
	}
	clock.Set(*pending.NextReleaseAttempt)
	var calls atomic.Int32
	if _, err := store.ReapExpired(context.Background(), 1, time.Second, func(context.Context, workarea.TerminalLease) error {
		calls.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("retry calls=%d", calls.Load())
	}
}

func TestLogicalClockClampsRollbackAndForwardJumpReaps(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(root, "leases"), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Minute)
	claim, err := store.ClaimExecution(context.Background(), claimSpec(lease, invocationA, claimA))
	if err != nil {
		t.Fatal(err)
	}
	clock.Set(clock.Now().Add(-5 * time.Minute))
	if _, err := store.Renew(context.Background(), workarea.RenewSpec{
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, Extension: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	persisted, _ := store.Get(lease.LeaseID)
	if persisted.ClockHighWatermarkMS < claim.ClaimNowMS {
		t.Fatalf("clock rolled back: %d < %d", persisted.ClockHighWatermarkMS, claim.ClaimNowMS)
	}
	clock.Set(lease.MaxExpiresAt.Add(time.Hour))
	if _, err := store.ReapExpired(context.Background(), 1, time.Second, func(context.Context, workarea.TerminalLease) error { return nil }); err != nil {
		t.Fatal(err)
	}
	released, _ := store.Get(lease.LeaseID)
	if released.State != workarea.LeaseReleased {
		t.Fatalf("forward jump did not reap: %s", released.State)
	}
}

func TestLeaseWriteFailureRetainsQuarantineAndFailsRootClosed(t *testing.T) {
	root := t.TempDir()
	leaseDir := filepath.Join(root, "leases")
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(leaseDir, "records")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaseDir, "records"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, acquireErr := store.Acquire(context.Background(), acquireSpec(root, sessionA, resultA, workareaA))
	if !errors.Is(acquireErr, workarea.ErrProviderRootUnready) {
		t.Fatalf("acquire error=%v", acquireErr)
	}
	items, err := store.Quarantines()
	if err != nil || len(items) != 1 || items[0].State != workarea.QuarantineQuarantined {
		t.Fatalf("quarantine=%+v err=%v acquireErr=%v", items, err, acquireErr)
	}
	if _, err := store.Acquire(context.Background(), acquireSpec(root, sessionB, resultB, workareaB)); !errors.Is(err, workarea.ErrProviderRootUnready) {
		t.Fatalf("second acquire error=%v", err)
	}
}

func TestNewWorkareaIdentityChangesAfterDestruction(t *testing.T) {
	t.Parallel()
	first, err := workarea.NewWorkareaID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := workarea.NewWorkareaID()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("acquisition identities reused: %s", first)
	}
}
