package workarea

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestBoundedSchedulerUsesSerialFullBatchesAndConcurrency(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	clock := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases"), Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	const total = 7
	for i := 0; i < total; i++ {
		lease, err := store.Acquire(context.Background(), internalAcquireSpec(root, i))
		if err != nil {
			t.Fatal(err)
		}
		lease.State = LeaseReleasePending
		if err := store.saveUnlocked(*lease); err != nil {
			t.Fatal(err)
		}
	}
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	var calls atomic.Int32
	considered, err := store.ReapSnapshot(context.Background(), SchedulerOptions{
		BatchSize: 3, Concurrency: 2, AttemptTimeout: time.Second,
	}, func(context.Context, TerminalLease) error {
		current := inFlight.Add(1)
		for {
			maximum := maxInFlight.Load()
			if current <= maximum || maxInFlight.CompareAndSwap(maximum, current) {
				break
			}
		}
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	if err != nil || considered != total || calls.Load() != total {
		t.Fatalf("considered=%d calls=%d err=%v", considered, calls.Load(), err)
	}
	if maxInFlight.Load() != 2 {
		t.Fatalf("max concurrency=%d", maxInFlight.Load())
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

func TestReaperAndReplayerLoopsCancel(t *testing.T) {
	root := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: filepath.Join(root, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		store.RunReaper(ctx, ReaperOptions{Interval: time.Millisecond}, func(context.Context, TerminalLease) error { return nil })
	}()
	go func() {
		defer wg.Done()
		store.RunTerminalResultReplayer(ctx, TerminalResultReplayOptions{Interval: time.Millisecond}, func(context.Context, string, []byte) error { return nil })
	}()
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background loops did not stop")
	}
}
