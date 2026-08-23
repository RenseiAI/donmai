package workarea

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func internalAcquireSpec(root string, index int) AcquireSpec {
	return AcquireSpec{
		SessionID:        fmt.Sprintf("%08d-1111-4111-8111-%012d", index, index),
		TerminalResultID: fmt.Sprintf("tr_%032x", index+1),
		WorkareaID:       fmt.Sprintf("wa_%032x", index+1),
		WorkareaPath:     filepath.Join(root, fmt.Sprintf("workarea-%d", index)),
		Policy:           DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "destroy",
	}
}

func TestIntFileDescriptorBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		fd      uintptr
		want    int
		wantErr error
	}{
		{name: "zero", fd: 0, want: 0},
		{name: "maximum int", fd: uintptr(math.MaxInt), want: math.MaxInt},
		{name: "above maximum int", fd: uintptr(math.MaxInt) + 1, wantErr: errFileDescriptorOutOfRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := intFileDescriptor(tt.fd)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("intFileDescriptor(%d) error = %v, want %v", tt.fd, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("intFileDescriptor(%d) = %d, want %d", tt.fd, got, tt.want)
			}
		})
	}
}

func TestActionableIndexRebuildsBeforeAdmission(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), internalAcquireSpec(root, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.actionablePath(lease.LeaseID)); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewLeaseStore(StoreOptions{Dir: store.Dir()})
	if err != nil {
		t.Fatal(err)
	}
	if retained, err := reopened.Retained(lease.WorkareaID); err != nil || !retained {
		t.Fatalf("retained=%v err=%v", retained, err)
	}
}

func TestActionableIndexRecoveryRemovesStaleReleasedMarker(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), internalAcquireSpec(root, 91))
	if err != nil {
		t.Fatal(err)
	}
	lease.State = LeaseReleased
	if err := store.saveUnlocked(*lease); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash in which the authoritative released record reached disk
	// but the derived marker deletion did not.
	if err := store.writeActionableMarker(lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewLeaseStore(StoreOptions{Dir: store.Dir()})
	if err != nil {
		t.Fatal(err)
	}
	if retained, err := reopened.Retained(lease.WorkareaID); err != nil || retained {
		t.Fatalf("released stale-marker workarea retained=%v err=%v", retained, err)
	}
	if _, err := os.Stat(reopened.actionablePath(lease.LeaseID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale actionable marker survived recovery: %v", err)
	}
}

func TestReconcileClearsOnlyGuardForExactDurableLease(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	spec := internalAcquireSpec(root, 1)
	if _, err := store.Acquire(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	mismatched := spec
	mismatched.TerminalResultID = "tr_ffffffffffffffffffffffffffffffff"
	mismatched.WorkareaPath = filepath.Join(root, "different-workarea")
	guard, err := store.quarantine.createGuard(mismatched, time.Now().UTC().Truncate(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := NewLeaseStore(StoreOptions{Dir: store.Dir()})
	if err != nil {
		t.Fatal(err)
	}
	guards, err := reopened.Quarantines()
	if err != nil {
		t.Fatal(err)
	}
	if len(guards) != 1 || guards[0].QuarantineID != guard.QuarantineID {
		t.Fatalf("mismatched guard was cleared during reconciliation: %+v", guards)
	}
}

func TestActionableIndexReapBatchScalesWithActionableRecords(t *testing.T) {
	root := t.TempDir()
	clock := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases"), Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	const actionableCount = 128
	for i := 0; i < actionableCount; i++ {
		lease, err := store.Acquire(context.Background(), internalAcquireSpec(root, i))
		if err != nil {
			t.Fatal(err)
		}
		lease.State = LeaseReleasePending
		if err := store.saveUnlocked(*lease); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	considered, err := store.ReapExpired(context.Background(), 32, 3*time.Second, func(context.Context, TerminalLease) error { return nil })
	elapsed := time.Since(started)
	if err != nil || considered != 32 {
		t.Fatalf("ReapExpired considered=%d err=%v elapsed=%s", considered, err, elapsed)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("ReapExpired elapsed=%s, exceeded provider-attempt timeout", elapsed)
	}
}

// errAttemptsNeverOverlapped marks a provider attempt that waited out its whole
// attempt budget without the scheduler ever dispatching a peer alongside it.
var errAttemptsNeverOverlapped = errors.New("scheduler never dispatched concurrent provider attempts")

func TestBoundedSchedulerUsesSerialFullBatchesAndConcurrency(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	clock := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases"), Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	const (
		total       = 7
		batchSize   = 3
		concurrency = 2
	)
	leaseIDs := make([]string, 0, total)
	for i := 0; i < total; i++ {
		lease, err := store.Acquire(context.Background(), internalAcquireSpec(root, i))
		if err != nil {
			t.Fatal(err)
		}
		lease.State = LeaseReleasePending
		if err := store.saveUnlocked(*lease); err != nil {
			t.Fatal(err)
		}
		leaseIDs = append(leaseIDs, lease.LeaseID)
	}
	// The scheduler drains its snapshot in declared order: release attempts
	// ascending, then lease ID. Every lease's dispatch position is known up front.
	slices.Sort(leaseIDs)
	positionOf := make(map[string]int, total)
	for i, id := range leaseIDs {
		positionOf[id] = i
	}

	// Closing overlapped is the synchronization point that proves concurrency:
	// the first attempt cannot return until the scheduler dispatches a peer
	// alongside it. A scheduler that serialized burns its attempt budget instead,
	// so the property is decided by a rendezvous rather than by whether two
	// attempt windows happened to overlap in wall-clock time.
	//
	// The seven leases still span three batches, but the batch boundary itself is
	// deliberately not asserted here: a releaser can observe that batch k+1 began
	// early, yet it can never observe that the scheduler declined to start it, so
	// the drain-before-advance half of the contract has no positive signal to wait
	// on. Cover that on the batch arithmetic instead of by observing attempts.
	overlapped := make(chan struct{})
	var entered, inFlight, maxInFlight atomic.Int32
	var mu sync.Mutex
	var order []string
	considered, err := store.ReapSnapshot(context.Background(), SchedulerOptions{
		BatchSize: batchSize, Concurrency: concurrency,
		// Bounds how long the rendezvous may stall before the test reports
		// serialization. It paces no assertion: the pass path never waits on it.
		AttemptTimeout: 5 * time.Second,
	}, func(ctx context.Context, lease TerminalLease) error {
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			maximum := maxInFlight.Load()
			if current <= maximum || maxInFlight.CompareAndSwap(maximum, current) {
				break
			}
		}
		mu.Lock()
		order = append(order, lease.LeaseID)
		mu.Unlock()

		if entered.Add(1) == concurrency {
			close(overlapped)
		}
		select {
		case <-overlapped:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("%w: lease %s: %w", errAttemptsNeverOverlapped, lease.LeaseID, ctx.Err())
		}
	})
	if errors.Is(err, errAttemptsNeverOverlapped) {
		t.Fatalf("concurrency=%d requested but attempts serialized: %v", concurrency, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if err != nil || considered != total || len(order) != total {
		t.Fatalf("considered=%d attempts=%d err=%v", considered, len(order), err)
	}
	// Bounded above by the worker count and reached exactly: the rendezvous
	// forces the floor, so a surplus can only be an over-dispatch.
	if got := maxInFlight.Load(); got != concurrency {
		t.Fatalf("max in-flight attempts=%d, want the requested bound %d", got, concurrency)
	}
	// Every actionable lease is attempted exactly once, and the scheduler never
	// runs ahead of its declared order by more than the concurrency window.
	seen := make(map[string]bool, total)
	for position, id := range order {
		declared, ok := positionOf[id]
		if !ok {
			t.Fatalf("attempt %d released unknown lease %s", position, id)
		}
		if seen[id] {
			t.Fatalf("lease %s was attempted more than once", id)
		}
		seen[id] = true
		if declared-position >= concurrency || position-declared >= concurrency {
			t.Fatalf("lease %s declared at position %d was attempted at position %d, outside the concurrency window %d",
				id, declared, position, concurrency)
		}
	}
	if len(seen) != total {
		t.Fatalf("attempted %d distinct leases, want %d", len(seen), total)
	}
}

func TestReleaseAttemptTimeoutStartsAtProviderInvocation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), internalAcquireSpec(root, 1))
	if err != nil {
		t.Fatal(err)
	}
	lease.State = LeaseReleasePending
	if err := store.saveUnlocked(*lease); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	_, err = store.ReapExpired(context.Background(), 1, 30*time.Millisecond, func(ctx context.Context, _ TerminalLease) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	<-entered
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release error=%v", err)
	}
}

func TestIndependentReleaseCallbacksDoNotHoldStoreLocks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		lease, err := store.Acquire(context.Background(), internalAcquireSpec(root, i))
		if err != nil {
			t.Fatal(err)
		}
		lease.State = LeaseReleasePending
		if err := store.saveUnlocked(*lease); err != nil {
			t.Fatal(err)
		}
	}
	allEntered := make(chan struct{})
	var entered atomic.Int32
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, reapErr := store.ReapExpired(context.Background(), 2, time.Second, func(context.Context, TerminalLease) error {
			if entered.Add(1) == 2 {
				close(allEntered)
			}
			<-release
			return nil
		})
		done <- reapErr
	}()
	select {
	case <-allEntered:
	case <-time.After(time.Second):
		t.Fatal("independent provider callbacks did not enter concurrently")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReaperAndReplayerLoopsCancelUnderClockContention(t *testing.T) {
	root := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), internalAcquireSpec(root, 40))
	if err != nil {
		t.Fatal(err)
	}

	releaseClock := holdLogicalClockLock(t, store)
	defer releaseClock()

	originalReadFile := store.readFile
	bothScanned := make(chan struct{})
	var recordReads atomic.Int32
	store.readFile = func(path string) ([]byte, error) {
		data, readErr := originalReadFile(path)
		if path == store.recordPath(lease.LeaseID) && recordReads.Add(1) == 2 {
			close(bothScanned)
		}
		return data, readErr
	}

	ctx, cancel := context.WithCancel(context.Background())
	reaperDone := make(chan struct{})
	replayerDone := make(chan struct{})
	loopErrors := make(chan error, 2)
	go func() {
		defer close(reaperDone)
		store.RunReaper(ctx, ReaperOptions{
			Interval: time.Hour,
			OnError:  func(err error) { loopErrors <- err },
		}, func(context.Context, TerminalLease) error { return nil })
	}()
	go func() {
		defer close(replayerDone)
		store.RunTerminalResultReplayer(ctx, TerminalResultReplayOptions{
			Interval: time.Hour,
			OnError:  func(err error) { loopErrors <- err },
		}, func(context.Context, string, []byte) error { return nil })
	}()

	select {
	case <-bothScanned:
	case <-time.After(time.Second):
		t.Fatal("background loops did not reach the contended logical clock")
	}
	cancel()
	waitForLoopExit(t, "terminal lease reaper", reaperDone)
	waitForLoopExit(t, "terminal result replayer", replayerDone)
	close(loopErrors)
	for loopErr := range loopErrors {
		t.Errorf("shutdown cancellation reached OnError: %v", loopErr)
	}
	if err := store.Ready(); err != nil {
		t.Fatalf("shutdown cancellation poisoned store: %v", err)
	}

	releaseClock()
	if _, err := store.Acquire(context.Background(), internalAcquireSpec(root, 41)); err != nil {
		t.Fatalf("lease operation after shutdown cancellation: %v", err)
	}
}

func TestSampleClockDeadlineWhileContendedDoesNotPoisonStore(t *testing.T) {
	root := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	releaseClock := holdLogicalClockLock(t, store)
	defer releaseClock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := store.sampleClock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sample clock error = %v, want deadline exceeded", err)
	}
	if err := store.Ready(); err != nil {
		t.Fatalf("deadline cancellation poisoned store: %v", err)
	}

	releaseClock()
	if _, err := store.Acquire(context.Background(), internalAcquireSpec(root, 42)); err != nil {
		t.Fatalf("lease operation after deadline cancellation: %v", err)
	}
}

func TestOperationContextCancellationDoesNotHideJoinedAuthorityFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !isOnlyOperationContextCancellation(ctx, fmt.Errorf("wait for lock: %w", ctx.Err())) {
		t.Fatal("wrapped operation cancellation was not recognized")
	}
	authorityErr := errors.New("durable authority failed")
	if isOnlyOperationContextCancellation(ctx, errors.Join(ctx.Err(), authorityErr)) {
		t.Fatal("operation cancellation hid a joined durable-authority failure")
	}
}

func TestLogicalClockLockFailurePoisonsStoreAndCanceledLoopsReportIt(t *testing.T) {
	root := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.locks); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.locks, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sampleClock(context.Background()); !errors.Is(err, ErrProviderRootUnready) {
		t.Fatalf("logical clock lock error = %v, want provider root unready", err)
	}
	if err := store.Ready(); !errors.Is(err, ErrProviderRootUnready) {
		t.Fatalf("ready error = %v, want provider root unready", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reaperDone := make(chan struct{})
	replayerDone := make(chan struct{})
	loopErrors := make(chan error, 2)
	go func() {
		defer close(reaperDone)
		store.RunReaper(ctx, ReaperOptions{
			Interval: time.Hour,
			OnError:  func(err error) { loopErrors <- err },
		}, func(context.Context, TerminalLease) error { return nil })
	}()
	go func() {
		defer close(replayerDone)
		store.RunTerminalResultReplayer(ctx, TerminalResultReplayOptions{
			Interval: time.Hour,
			OnError:  func(err error) { loopErrors <- err },
		}, func(context.Context, string, []byte) error { return nil })
	}()
	waitForLoopExit(t, "terminal lease reaper", reaperDone)
	waitForLoopExit(t, "terminal result replayer", replayerDone)
	close(loopErrors)

	reported := 0
	for loopErr := range loopErrors {
		reported++
		if !errors.Is(loopErr, ErrProviderRootUnready) {
			t.Errorf("OnError = %v, want provider root unready", loopErr)
		}
	}
	if reported != 2 {
		t.Fatalf("OnError calls = %d, want 2 pre-existing authority failures", reported)
	}
	if _, err := store.Acquire(context.Background(), internalAcquireSpec(root, 43)); !errors.Is(err, ErrProviderRootUnready) {
		t.Fatalf("lease operation after lock failure = %v, want provider root unready", err)
	}
}

func holdLogicalClockLock(t *testing.T, store *LeaseStore) func() {
	t.Helper()
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.withLocks(context.Background(), []string{clockLockKey}, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-done:
		t.Fatalf("hold logical clock lock: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out acquiring logical clock lock")
	}

	var once sync.Once
	return func() {
		t.Helper()
		once.Do(func() {
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("release logical clock lock: %v", err)
				}
			case <-time.After(time.Second):
				t.Error("timed out releasing logical clock lock")
			}
		})
	}
}

func waitForLoopExit(t *testing.T, name string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s did not stop promptly", name)
	}
}
