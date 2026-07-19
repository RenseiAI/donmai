package workarea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestActionableIndexLargeReleasedHistoryReadsOnlyActionableRecords(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	store := newInternalTestStore(t, dir, func() time.Time { return now })

	const releasedCount = 10_000
	for i := 0; i < releasedCount; i++ {
		writeInternalTestLease(t, store, internalTestLease(i, LeaseReleased, now))
	}
	const activeCount = 3
	for i := 0; i < activeCount; i++ {
		lease := internalTestLease(releasedCount+i, LeaseActive, now)
		lease.ExpiresAt = now.Add(time.Hour)
		writeInternalTestLease(t, store, lease)
	}
	rebuildInternalTestIndex(t, store)

	var recordReads atomic.Int64
	countedRead := func(path string) ([]byte, error) {
		if filepath.Dir(path) == store.records && filepath.Ext(path) == ".json" {
			recordReads.Add(1)
		}
		return os.ReadFile(path) //nolint:gosec // test paths live under t.TempDir.
	}
	reopened, err := newLeaseStore(StoreOptions{Dir: dir, Now: func() time.Time { return now }}, leaseStoreDependencies{readFile: countedRead})
	if err != nil {
		t.Fatalf("NewLeaseStore: %v", err)
	}
	if got := recordReads.Load(); got != activeCount {
		t.Fatalf("startup record reads = %d, want %d actionable records (history=%d)", got, activeCount, releasedCount)
	}

	recordReads.Store(0)
	considered, err := reopened.ReapExpired(context.Background(), DefaultReaperBatchSize, time.Second, func(context.Context, TerminalLease) error {
		t.Fatal("future active lease was released")
		return nil
	})
	if err != nil || considered != 0 {
		t.Fatalf("ReapExpired considered=%d err=%v", considered, err)
	}
	if got := recordReads.Load(); got != activeCount {
		t.Fatalf("reaper record reads = %d, want %d actionable records (history=%d)", got, activeCount, releasedCount)
	}

	recordReads.Store(0)
	considered, err = reopened.ReplayTerminalResults(context.Background(), DefaultTerminalResultReplayBatchSize, time.Second, func(context.Context, TerminalLease, json.RawMessage) error {
		t.Fatal("lease without a pending outbox was replayed")
		return nil
	})
	if err != nil || considered != 0 {
		t.Fatalf("ReplayTerminalResults considered=%d err=%v", considered, err)
	}
	if got := recordReads.Load(); got != activeCount {
		t.Fatalf("replayer record reads = %d, want %d actionable records (history=%d)", got, activeCount, releasedCount)
	}
}

func TestActionableIndexCorruptionRebuildsFromAuthoritativeRecords(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, store *LeaseStore, activeLeaseID string)
	}{
		{
			name: "marker",
			corrupt: func(t *testing.T, store *LeaseStore, _ string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(store.actionableIndex, actionableIndexMarker), []byte(`{"schemaVersion":"broken"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "entry",
			corrupt: func(t *testing.T, store *LeaseStore, activeLeaseID string) {
				t.Helper()
				if err := os.WriteFile(store.actionableIndexEntryPath(activeLeaseID), []byte(`{"leaseId":"wrong"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
			store := newInternalTestStore(t, dir, func() time.Time { return now })
			for i := 0; i < 4; i++ {
				state := LeaseReleased
				if i >= 2 {
					state = LeaseActive
				}
				writeInternalTestLease(t, store, internalTestLease(i, state, now))
			}
			rebuildInternalTestIndex(t, store)
			activeLeaseID := internalTestLease(2, LeaseActive, now).LeaseID
			tt.corrupt(t, store, activeLeaseID)

			var recordReads atomic.Int64
			rebuilt, err := newLeaseStore(StoreOptions{
				Dir: dir,
				Now: func() time.Time { return now },
			}, leaseStoreDependencies{readFile: func(path string) ([]byte, error) {
				if filepath.Dir(path) == store.records && filepath.Ext(path) == ".json" {
					recordReads.Add(1)
				}
				return os.ReadFile(path) //nolint:gosec // test paths live under t.TempDir.
			}})
			if err != nil {
				t.Fatalf("rebuild NewLeaseStore: %v", err)
			}
			if got := recordReads.Load(); got != 4 {
				t.Fatalf("rebuild record reads = %d, want all 4 authoritative records", got)
			}
			if retained, err := rebuilt.Retained("workarea-2"); err != nil || !retained {
				t.Fatalf("active lease retained=%v err=%v", retained, err)
			}
			if retained, err := rebuilt.Retained("workarea-0"); err != nil || retained {
				t.Fatalf("released lease retained=%v err=%v", retained, err)
			}

			recordReads.Store(0)
			if _, err := newLeaseStore(StoreOptions{Dir: dir, Now: func() time.Time { return now }}, leaseStoreDependencies{readFile: func(path string) ([]byte, error) {
				if filepath.Dir(path) == store.records && filepath.Ext(path) == ".json" {
					recordReads.Add(1)
				}
				return os.ReadFile(path) //nolint:gosec // test paths live under t.TempDir.
			}}); err != nil {
				t.Fatalf("second NewLeaseStore: %v", err)
			}
			if got := recordReads.Load(); got != 2 {
				t.Fatalf("post-rebuild startup record reads = %d, want 2 actionable records", got)
			}
		})
	}
}

func TestActionableIndexRejectsSelfConsistentOmission(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	store := newInternalTestStore(t, dir, func() time.Time { return now })
	first := internalTestLease(1, LeaseActive, now)
	second := internalTestLease(2, LeaseActive, now)
	writeInternalTestLease(t, store, first)
	writeInternalTestLease(t, store, second)
	rebuildInternalTestIndex(t, store)

	if err := store.withLocks(context.Background(), []string{actionableIndexLockKey}, func() error {
		state, err := store.readActionableStateFileLocked()
		if err != nil {
			return err
		}
		if err := os.Remove(store.actionableIndexEntryPath(second.LeaseID)); err != nil {
			return err
		}
		state.EntryCount--
		state.EntriesDigest, err = toggleActionableIndexDigest(state.EntriesDigest, second.LeaseID)
		if err != nil {
			return err
		}
		if err := store.writeActionableStateLocked(state); err != nil {
			return err
		}
		return store.writeActionableIndexMetadataLocked(state)
	}); err != nil {
		t.Fatalf("create self-consistent omitted index entry: %v", err)
	}

	recovered := newInternalTestStore(t, dir, func() time.Time { return now })
	if retained, err := recovered.Retained(second.WorkareaID); err != nil || !retained {
		t.Fatalf("omitted authoritative lease retained=%v err=%v", retained, err)
	}
	_, err := recovered.Acquire(context.Background(), AcquireSpec{
		SessionID:        "competing-session",
		TerminalResultID: "competing-result",
		WorkareaID:       second.WorkareaID,
		WorkareaPath:     second.WorkareaPath,
		Policy: LeasePolicy{
			SettlementBudget: time.Minute,
			SafetyMargin:     time.Second,
			LeaseDuration:    2 * time.Minute,
			MaxLeaseDuration: 5 * time.Minute,
		},
	})
	if !errors.Is(err, ErrWorkareaLeased) {
		t.Fatalf("competing acquire error = %v, want ErrWorkareaLeased", err)
	}
}

func TestLeaseRetryPreservesPriorErrorUntilOutcome(t *testing.T) {
	t.Run("release", func(t *testing.T) {
		now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
		store := newInternalTestStore(t, t.TempDir(), func() time.Time { return now })
		lease := internalTestLease(10, LeaseReleasePending, now)
		lease.ReleaseAttempts = 1
		lease.LastReleaseError = "prior release failure"
		writeInternalTestLease(t, store, lease)
		rebuildInternalTestIndex(t, store)

		entered := make(chan struct{})
		unblock := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := store.ReapExpired(context.Background(), 1, time.Second, func(context.Context, TerminalLease) error {
				close(entered)
				<-unblock
				return errors.New("new release failure")
			})
			done <- err
		}()
		<-entered
		persisted := readInternalTestLease(t, store, lease.LeaseID)
		if persisted.LastReleaseError != "prior release failure" {
			t.Fatalf("LastReleaseError at callback entry = %q", persisted.LastReleaseError)
		}
		close(unblock)
		if err := <-done; err == nil {
			t.Fatal("ReapExpired unexpectedly succeeded")
		}
		persisted = readInternalTestLease(t, store, lease.LeaseID)
		if persisted.LastReleaseError != "new release failure" {
			t.Fatalf("LastReleaseError after callback = %q", persisted.LastReleaseError)
		}
	})

	t.Run("terminal result post", func(t *testing.T) {
		now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
		store := newInternalTestStore(t, t.TempDir(), func() time.Time { return now })
		lease := internalTestLease(11, LeaseActive, now)
		lease.TerminalResultPost = &TerminalResultPost{
			State: TerminalResultPostPending, Payload: json.RawMessage(`{"status":"completed"}`),
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastError: "prior post failure",
		}
		writeInternalTestLease(t, store, lease)
		rebuildInternalTestIndex(t, store)

		entered := make(chan struct{})
		unblock := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := store.ReplayTerminalResults(context.Background(), 1, time.Second, func(context.Context, TerminalLease, json.RawMessage) error {
				close(entered)
				<-unblock
				return errors.New("new post failure")
			})
			done <- err
		}()
		<-entered
		persisted := readInternalTestLease(t, store, lease.LeaseID)
		if persisted.TerminalResultPost == nil || persisted.TerminalResultPost.LastError != "prior post failure" {
			t.Fatalf("LastError at callback entry = %+v", persisted.TerminalResultPost)
		}
		close(unblock)
		if err := <-done; err == nil {
			t.Fatal("ReplayTerminalResults unexpectedly succeeded")
		}
		persisted = readInternalTestLease(t, store, lease.LeaseID)
		if persisted.TerminalResultPost == nil || persisted.TerminalResultPost.LastError != "new post failure" {
			t.Fatalf("LastError after callback = %+v", persisted.TerminalResultPost)
		}
	})
}

func TestActionableIndexReapBatchScalesWithActionableRecords(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	store := newInternalTestStore(t, t.TempDir(), func() time.Time { return now })
	const actionableCount = 1_000
	for i := 0; i < actionableCount; i++ {
		lease := internalTestLease(i, LeaseReleasePending, now)
		lease.ReleaseAttempts = 1
		writeInternalTestLease(t, store, lease)
	}
	rebuildInternalTestIndex(t, store)

	started := time.Now()
	considered, err := store.ReapExpired(context.Background(), 32, time.Second, func(context.Context, TerminalLease) error { return nil })
	elapsed := time.Since(started)
	if err != nil || considered != 32 {
		t.Fatalf("ReapExpired considered=%d err=%v elapsed=%s", considered, err, elapsed)
	}
	if elapsed >= time.Second {
		t.Fatalf("ReapExpired elapsed=%s, want under 1s", elapsed)
	}
}

func TestActionableIndexRecoversRecordIndexCrashWindows(t *testing.T) {
	t.Run("record became released before index deletion", func(t *testing.T) {
		dir := t.TempDir()
		now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
		store := newInternalTestStore(t, dir, func() time.Time { return now })
		lease := internalTestLease(1, LeaseActive, now)
		writeInternalTestLease(t, store, lease)
		rebuildInternalTestIndex(t, store)

		lease.State = LeaseReleased
		releasedAt := now.Add(time.Minute)
		lease.ReleasedAt = &releasedAt
		writeInternalTestLease(t, store, lease)
		removeInternalTestIndexMarker(t, store)

		recovered := newInternalTestStore(t, dir, func() time.Time { return now })
		if retained, err := recovered.Retained(lease.WorkareaID); err != nil || retained {
			t.Fatalf("retained=%v err=%v after released-record crash window", retained, err)
		}
		audit, err := recovered.List()
		if err != nil {
			t.Fatalf("List audit records: %v", err)
		}
		if len(audit) != 1 || audit[0].LeaseID != lease.LeaseID || audit[0].State != LeaseReleased || audit[0].ReleasedAt == nil {
			t.Fatalf("released audit record lost or changed: %+v", audit)
		}
	})

	t.Run("release pending record was committed before index insertion", func(t *testing.T) {
		dir := t.TempDir()
		now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
		store := newInternalTestStore(t, dir, func() time.Time { return now })
		lease := internalTestLease(2, LeaseReleased, now)
		writeInternalTestLease(t, store, lease)
		rebuildInternalTestIndex(t, store)

		lease.State = LeaseReleasePending
		lease.ReleasedAt = nil
		lease.ReleaseAttempts = 1
		writeInternalTestLease(t, store, lease)
		removeInternalTestIndexMarker(t, store)

		recovered := newInternalTestStore(t, dir, func() time.Time { return now })
		if retained, err := recovered.Retained(lease.WorkareaID); err != nil || !retained {
			t.Fatalf("retained=%v err=%v after release-pending-record crash window", retained, err)
		}
		considered, err := recovered.ReapExpired(context.Background(), 1, time.Second, func(context.Context, TerminalLease) error { return nil })
		if err != nil || considered != 1 {
			t.Fatalf("ReapExpired considered=%d err=%v", considered, err)
		}
	})

	t.Run("legacy writer replaced a released record without index update", func(t *testing.T) {
		dir := t.TempDir()
		now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
		store := newInternalTestStore(t, dir, func() time.Time { return now })
		active := internalTestLease(3, LeaseReleased, now)
		writeInternalTestLease(t, store, active)
		rebuildInternalTestIndex(t, store)

		forcedGeneration := time.Unix(1, 0)
		if err := os.Chtimes(store.records, forcedGeneration, forcedGeneration); err != nil {
			t.Fatal(err)
		}
		if err := store.withLocks(context.Background(), []string{actionableIndexLockKey}, func() error {
			state, err := store.readActionableStateFileLocked()
			if err != nil {
				return err
			}
			state.RecordsModTimeUnixNano = forcedGeneration.UnixNano()
			if err := store.writeActionableStateLocked(state); err != nil {
				return err
			}
			return store.writeActionableIndexMetadataLocked(state)
		}); err != nil {
			t.Fatalf("write forced actionable index generation: %v", err)
		}

		active.State = LeaseActive
		active.ReleasedAt = nil
		data, err := json.Marshal(active)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(store.records, store.recordPath(active.LeaseID), ".legacy-lease-*.tmp", data); err != nil {
			t.Fatal(err)
		}
		recordsInfo, err := os.Stat(store.records)
		if err != nil {
			t.Fatal(err)
		}
		if recordsInfo.ModTime().Equal(forcedGeneration) {
			t.Fatal("legacy atomic record replacement did not advance the records generation")
		}

		recovered := newInternalTestStore(t, dir, func() time.Time { return now })
		if retained, err := recovered.Retained(active.WorkareaID); err != nil || !retained {
			t.Fatalf("retained=%v err=%v after legacy record-only write", retained, err)
		}
	})
}

func TestActionableIndexConcurrentMutationAndScan(t *testing.T) {
	root := t.TempDir()
	store := newInternalTestStore(t, root, time.Now)
	const workers = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			spec := AcquireSpec{
				SessionID:        fmt.Sprintf("session-%d", i),
				TerminalResultID: fmt.Sprintf("result-%d", i),
				WorkareaID:       fmt.Sprintf("workarea-%d", i),
				WorkareaPath:     filepath.Join(root, fmt.Sprintf("workarea-%d", i)),
				Policy: LeasePolicy{
					SettlementBudget: time.Minute,
					SafetyMargin:     time.Second,
					LeaseDuration:    2 * time.Minute,
					MaxLeaseDuration: 5 * time.Minute,
				},
			}
			lease, err := store.Acquire(context.Background(), spec)
			if err != nil {
				errs <- err
				return
			}
			if _, err := store.Retained(lease.WorkareaID); err != nil {
				errs <- err
				return
			}
			if i%2 == 0 {
				claim := ExecutionClaimSpec{
					LeaseID: lease.LeaseID, SessionID: lease.SessionID,
					TerminalResultID: lease.TerminalResultID, WorkareaID: lease.WorkareaID,
					InvocationID: fmt.Sprintf("invocation-%d", i), ClaimID: fmt.Sprintf("claim-%d", i),
				}
				if _, err := store.ClaimExecution(context.Background(), claim); err != nil {
					errs <- err
					return
				}
				ack := TerminalResultAcknowledgement{
					SchemaVersion: TerminalLeaseAcknowledgementSchemaV1,
					Acknowledged:  true, LeaseID: lease.LeaseID, SessionID: lease.SessionID,
					TerminalResultID: lease.TerminalResultID, WorkareaID: lease.WorkareaID,
					InvocationID: claim.InvocationID, ClaimID: claim.ClaimID,
				}
				if _, err := store.Acknowledge(context.Background(), ack, func(context.Context, TerminalLease) error { return nil }); err != nil {
					errs <- err
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent lease operation: %v", err)
	}
	if t.Failed() {
		return
	}

	reopened := newInternalTestStore(t, store.Dir(), time.Now)
	leases, err := reopened.listActionable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != workers/2 {
		t.Fatalf("actionable leases = %d, want %d", len(leases), workers/2)
	}
}

func TestReaperAndReplayerLoopsCancelAndJoin(t *testing.T) {
	t.Run("reaper", func(t *testing.T) {
		now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
		store := newInternalTestStore(t, t.TempDir(), func() time.Time { return now })
		lease := internalTestLease(1, LeaseActive, now)
		lease.ExpiresAt = now.Add(-time.Second)
		writeInternalTestLease(t, store, lease)
		rebuildInternalTestIndex(t, store)

		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			store.RunReaper(ctx, ReaperOptions{Interval: time.Hour, BatchSize: 1, AttemptTimeout: time.Hour}, func(ctx context.Context, _ TerminalLease) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			})
		}()
		<-started
		releaseIndex := holdInternalTestIndexLock(t, store)
		cancel()
		waitForInternalTestLoop(t, done)
		releaseIndex()
	})

	t.Run("replayer", func(t *testing.T) {
		now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
		store := newInternalTestStore(t, t.TempDir(), func() time.Time { return now })
		lease := internalTestLease(2, LeaseActive, now)
		lease.ExpiresAt = now.Add(time.Hour)
		lease.TerminalResultPost = &TerminalResultPost{
			State: TerminalResultPostPending, Payload: json.RawMessage(`{"status":"completed"}`),
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		writeInternalTestLease(t, store, lease)
		rebuildInternalTestIndex(t, store)

		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			store.RunTerminalResultReplayer(ctx, TerminalResultReplayOptions{Interval: time.Hour, BatchSize: 1, AttemptTimeout: time.Hour}, func(ctx context.Context, _ TerminalLease, _ json.RawMessage) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			})
		}()
		<-started
		releaseIndex := holdInternalTestIndexLock(t, store)
		cancel()
		waitForInternalTestLoop(t, done)
		releaseIndex()
	})
}

func newInternalTestStore(t *testing.T, dir string, now func() time.Time) *LeaseStore {
	t.Helper()
	store, err := NewLeaseStore(StoreOptions{Dir: dir, Now: now})
	if err != nil {
		t.Fatalf("NewLeaseStore: %v", err)
	}
	return store
}

func internalTestLease(index int, state LeaseState, now time.Time) TerminalLease {
	leaseID := fmt.Sprintf("twl_%032x", index)
	lease := TerminalLease{
		LeaseID: leaseID, SessionID: fmt.Sprintf("session-%d", index),
		TerminalResultID: fmt.Sprintf("result-%d", index), WorkareaID: fmt.Sprintf("workarea-%d", index),
		WorkareaPath: filepath.Join(os.TempDir(), fmt.Sprintf("workarea-%d", index)),
		AcquiredAt:   now, ExpiresAt: now.Add(time.Hour), MaxExpiresAt: now.Add(2 * time.Hour),
		SettlementBudget: time.Minute, State: state,
	}
	if state == LeaseReleased {
		releasedAt := now
		lease.ReleasedAt = &releasedAt
	}
	return lease
}

func writeInternalTestLease(t *testing.T, store *LeaseStore, lease TerminalLease) {
	t.Helper()
	data, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.recordPath(lease.LeaseID), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readInternalTestLease(t *testing.T, store *LeaseStore, leaseID string) TerminalLease {
	t.Helper()
	data, err := os.ReadFile(store.recordPath(leaseID)) //nolint:gosec // test path lives under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	var lease TerminalLease
	if err := json.Unmarshal(data, &lease); err != nil {
		t.Fatal(err)
	}
	return lease
}

func rebuildInternalTestIndex(t *testing.T, store *LeaseStore) {
	t.Helper()
	if err := store.withLocks(context.Background(), []string{actionableIndexLockKey}, func() error {
		return store.rebuildActionableIndexLocked(context.Background())
	}); err != nil {
		t.Fatalf("rebuild actionable index: %v", err)
	}
}

func removeInternalTestIndexMarker(t *testing.T, store *LeaseStore) {
	t.Helper()
	if err := os.Remove(filepath.Join(store.actionableIndex, actionableIndexMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := syncDir(store.actionableIndex); err != nil {
		t.Fatal(err)
	}
}

func holdInternalTestIndexLock(t *testing.T, store *LeaseStore) func() {
	t.Helper()
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.withLocks(context.Background(), []string{actionableIndexLockKey}, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	return func() {
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("hold actionable index lock: %v", err)
		}
	}
}

func waitForInternalTestLoop(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background lease loop did not join after cancellation")
	}
}
