package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Concurrent deliveries during credential preparation must not leave the
// winner holding the loser's bytes. Delivery A publishes its receipt and
// blocks in the pre-spawn credential hook; delivery B must lose the session
// reservation before compiling or publishing, so the durable receipt stays
// A's. Channel ordering selects the admission order without sleeps.
func TestConcurrentDeliveriesWinnerReceiptNeverOverwritten(t *testing.T) {
	tmp := t.TempDir()
	const sessionID = "session-concurrent-winner"
	if err := WriteConfig(filepath.Join(tmp, "daemon.yaml"), &Config{
		APIVersion: "donmai.dev/v1", Kind: "LocalDaemon", ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		ProjectAdmissionMode: ProjectAdmissionModeAllRouted, Machine: MachineConfig{ID: "concurrent-test"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 8},
		Orchestrator: OrchestratorConfig{URL: "https://example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	provider := &varyingReadyExecutionPreflight{sessionID: sessionID, workerID: "worker-local", placement: "host-local"}
	store := NewFileExecutionPreflightStore(filepath.Join(tmp, "receipts"))
	aEntered := make(chan struct{})
	releaseA := make(chan struct{})
	var hookCalls atomic.Int32
	var enteredOnce atomic.Bool
	d := New(Options{
		ConfigPath: filepath.Join(tmp, "daemon.yaml"), JWTPath: filepath.Join(tmp, "daemon.jwt"),
		SkipWizard: true, SkipRegistration: true, ProviderRegistry: provider, ExecutionPreflightStore: store,
		SpawnerOptions: SpawnerOptions{
			WorkerCommand:        []string{"/bin/sh", "-c", "exit 0"},
			ProjectAdmissionMode: ProjectAdmissionModeAllRouted,
			OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
				if hookCalls.Add(1) == 1 && enteredOnce.CompareAndSwap(false, true) {
					close(aEntered)
					<-releaseA
				}
				return env, nil
			},
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })

	spec := SessionSpec{SessionID: sessionID, ProjectID: "project-test"}
	type acceptResult struct {
		handle *SessionHandle
		err    error
	}
	aResult := make(chan acceptResult, 1)
	go func() {
		handle, err := d.AcceptWorkWithDetail(spec, readyRedeliveryDetail(t, sessionID))
		aResult <- acceptResult{handle: handle, err: err}
	}()
	<-aEntered
	_, errB := d.AcceptWorkWithDetail(spec, readyRedeliveryDetail(t, sessionID))
	if errB == nil {
		close(releaseA)
		<-aResult
		t.Fatal("concurrent second delivery succeeded")
	}
	close(releaseA)
	aRes := <-aResult
	if aRes.err != nil {
		t.Fatalf("winning delivery failed: %v", aRes.err)
	}
	if aRes.handle == nil {
		t.Fatal("winning delivery has no handle")
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("preflight compilations = %d, want 1 (loser must not compile)", got)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Fatalf("pre-spawn hook calls = %d, want 1 (loser must not reach spawn)", got)
	}
	first := varyingReadyReceipt(t, sessionID, 1)
	stored, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("load after concurrent deliveries: %v", err)
	}
	if !bytes.Equal(stored, first) {
		t.Fatalf("stored receipt = %s, want winner's first compilation %s", stored, first)
	}
	if _, err := os.Lstat(filepath.Join(tmp, "receipts", executionPreflightAttemptPendingName(sessionID))); err != nil {
		t.Fatalf("attempt-pending marker missing after winning delivery: %v", err)
	}
}

// A markerless legacy receipt stays immutable: without the attempt-pending
// marker the store cannot prove the session never spawned, so a differing
// republish is refused with a host-defect NACK and the bytes are untouched.
func TestLegacyMarkerlessReceiptRefusedNotReplaced(t *testing.T) {
	const sessionID = "session-legacy-markerless"
	tests := []struct {
		name string
		// setup plants the on-disk state before the second persist.
		setup func(t *testing.T, dir string, store *FileExecutionPreflightStore, first, second []byte)
		// second differs from first unless identical is true.
		identical bool
		// wantReplaced reports whether the second persist must win.
		wantReplaced bool
		// wantHostDefect reports whether refusal must project host defect.
		wantHostDefect bool
	}{
		{
			name: "legacy markerless differing receipt is refused",
			setup: func(t *testing.T, dir string, _ *FileExecutionPreflightStore, first, _ []byte) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, executionPreflightReceiptName(sessionID)), first, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantReplaced:   false,
			wantHostDefect: true,
		},
		{
			name: "legacy markerless identical receipt stays idempotent",
			setup: func(t *testing.T, dir string, _ *FileExecutionPreflightStore, first, _ []byte) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, executionPreflightReceiptName(sessionID)), first, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			identical:    true,
			wantReplaced: true,
		},
		{
			name: "pending marker without spawn is replaceable",
			setup: func(t *testing.T, _ string, store *FileExecutionPreflightStore, first, _ []byte) {
				t.Helper()
				if err := store.Persist(sessionID, first); err != nil {
					t.Fatalf("first persist: %v", err)
				}
			},
			wantReplaced:   true,
			wantHostDefect: false,
		},
		{
			name: "spawned receipt with pending stays immutable",
			setup: func(t *testing.T, _ string, store *FileExecutionPreflightStore, first, _ []byte) {
				t.Helper()
				if err := store.Persist(sessionID, first); err != nil {
					t.Fatalf("first persist: %v", err)
				}
				if err := store.MarkExecutionPreflightSpawned(sessionID); err != nil {
					t.Fatalf("mark spawned: %v", err)
				}
			},
			wantReplaced:   false,
			wantHostDefect: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewFileExecutionPreflightStore(dir)
			first := firstReceipt(t, sessionID)
			var second []byte
			if tc.identical {
				second = append([]byte(nil), first...)
			} else {
				second = differingReceipt(t, sessionID, "worker-2")
			}
			tc.setup(t, dir, store, first, second)
			err := store.Persist(sessionID, second)
			if tc.wantReplaced {
				if err != nil {
					t.Fatalf("replacement persist: %v", err)
				}
				got, loadErr := store.Load(sessionID)
				if loadErr != nil {
					t.Fatalf("load after replace: %v", loadErr)
				}
				if !bytes.Equal(got, second) {
					t.Fatalf("stored receipt = %s, want %s", got, second)
				}
				return
			}
			if err == nil {
				t.Fatal("differing republish of a guarded receipt succeeded")
			}
			if !errors.Is(err, os.ErrExist) {
				t.Fatalf("differing republish error = %v, want it to wrap os.ErrExist", err)
			}
			if tc.wantHostDefect {
				var legacy *executionPreflightReceiptLegacyConflictError
				if !errors.As(err, &legacy) {
					t.Fatalf("differing republish error = %T, want the legacy receipt conflict", err)
				}
				if defect := acceptDefectNackReasonForError(err); defect == nil || defect.Defect != acceptDefectHost {
					t.Fatalf("legacy refusal defect = %+v, want the host-defect NACK reason", defect)
				}
			} else {
				var conflict *executionPreflightReceiptConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("differing republish error = %T, want the typed receipt conflict", err)
				}
				if defect := acceptDefectNackReasonForError(err); defect == nil || defect.Defect != acceptDefectSession {
					t.Fatalf("spawned refusal defect = %+v, want the session-defect NACK reason", defect)
				}
			}
			got, loadErr := store.Load(sessionID)
			if loadErr != nil {
				t.Fatalf("load after refused replace: %v", loadErr)
			}
			if !bytes.Equal(got, first) {
				t.Fatalf("stored receipt = %s, want %s", got, first)
			}
		})
	}
}

// A redelivery after a pre-spawn failure still succeeds under the new
// lifecycle and leaves the attempt-pending marker behind. The marker is the
// affirmative replaceability evidence the legacy path lacks.
func TestPendingMarkerRedeliveryAfterPreSpawnFailureSucceeds(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
	}{
		{name: "first session", sessionID: "session-pending-redelivery-1"},
		{name: "second session", sessionID: "session-pending-redelivery-2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			if err := WriteConfig(filepath.Join(tmp, "daemon.yaml"), &Config{
				APIVersion: "donmai.dev/v1", Kind: "LocalDaemon", ProjectAdmissionVersion: ProjectAdmissionVersionV2,
				ProjectAdmissionMode: ProjectAdmissionModeAllRouted, Machine: MachineConfig{ID: "pending-test"},
				Capacity:     CapacityConfig{MaxConcurrentSessions: 8},
				Orchestrator: OrchestratorConfig{URL: "https://example.test"},
			}); err != nil {
				t.Fatal(err)
			}
			provider := &varyingReadyExecutionPreflight{sessionID: tc.sessionID, workerID: "worker-local", placement: "host-local"}
			store := NewFileExecutionPreflightStore(filepath.Join(tmp, "receipts"))
			var hookCalls atomic.Int32
			failHook := atomic.Bool{}
			failHook.Store(true)
			d := New(Options{
				ConfigPath: filepath.Join(tmp, "daemon.yaml"), JWTPath: filepath.Join(tmp, "daemon.jwt"),
				SkipWizard: true, SkipRegistration: true, ProviderRegistry: provider, ExecutionPreflightStore: store,
				SpawnerOptions: SpawnerOptions{
					WorkerCommand:        []string{"/bin/sh", "-c", "exit 0"},
					ProjectAdmissionMode: ProjectAdmissionModeAllRouted,
					OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
						if hookCalls.Add(1) == 1 && failHook.Load() {
							return nil, errors.New("keyed session requires a credential snapshot and none is available")
						}
						return env, nil
					},
				},
			})
			if err := d.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Stop(context.Background()) })

			spec := SessionSpec{SessionID: tc.sessionID, ProjectID: "project-test"}
			if _, err := d.AcceptWorkWithDetail(spec, readyRedeliveryDetail(t, tc.sessionID)); err == nil {
				t.Fatal("first accept with a failing pre-spawn hook succeeded")
			}
			if _, err := os.Lstat(filepath.Join(tmp, "receipts", executionPreflightAttemptPendingName(tc.sessionID))); err != nil {
				t.Fatalf("attempt-pending marker missing after failed first accept: %v", err)
			}
			failHook.Store(false)
			handle, err := d.AcceptWorkWithDetail(spec, readyRedeliveryDetail(t, tc.sessionID))
			if err != nil {
				t.Fatalf("redelivery after pre-spawn failure: %v", err)
			}
			if handle == nil {
				t.Fatal("redelivery accepted without a session handle")
			}
			stored, err := store.Load(tc.sessionID)
			if err != nil {
				t.Fatalf("load after redelivery: %v", err)
			}
			if !bytes.Equal(stored, varyingReadyReceipt(t, tc.sessionID, 2)) {
				t.Fatalf("stored receipt = %s, want second compilation %s", stored, varyingReadyReceipt(t, tc.sessionID, 2))
			}
			if _, err := os.Lstat(filepath.Join(tmp, "receipts", executionPreflightAttemptPendingName(tc.sessionID))); err != nil {
				t.Fatalf("attempt-pending marker missing after redelivery: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(tmp, "receipts"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 3 {
				t.Fatalf("receipt dir entries = %d, want 3 (receipt plus pending plus spawn markers)", len(entries))
			}
		})
	}
}

// A name swapped to a symlink between the checks must never be traversed:
// the rename replaces the directory entry itself. The pending marker stays
// behind as the affirmative replaceability evidence.
func TestReplaceWithPendingNeverFollowsSwappedSymlink(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
	}{
		{name: "first session", sessionID: "session-pending-swap-1"},
		{name: "second session", sessionID: "session-pending-swap-2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewFileExecutionPreflightStore(dir)
			first := firstReceipt(t, tc.sessionID)
			if err := store.Persist(tc.sessionID, first); err != nil {
				t.Fatalf("first persist: %v", err)
			}
			outside := filepath.Join(t.TempDir(), "outside.json")
			if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			finalName := executionPreflightReceiptName(tc.sessionID)
			var calls atomic.Int32
			store.preOpenHook = func() {
				if calls.Add(1) != 2 {
					return
				}
				finalPath := filepath.Join(dir, finalName)
				if err := os.Remove(finalPath); err != nil {
					t.Errorf("swap setup remove: %v", err)
					return
				}
				if err := os.Symlink(outside, finalPath); err != nil {
					t.Errorf("swap setup symlink: %v", err)
				}
			}
			defer func() { store.preOpenHook = nil }()
			second := differingReceipt(t, tc.sessionID, "worker-2")
			if err := store.Persist(tc.sessionID, second); err != nil {
				t.Fatalf("replacement persist across symlink swap: %v", err)
			}
			if got, err := os.ReadFile(outside); err != nil || string(got) != "unchanged" {
				t.Fatalf("outside symlink target changed: %q err=%v", got, err)
			}
			info, err := os.Lstat(filepath.Join(dir, finalName))
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("published name is not a regular file: %+v err=%v", info, err)
			}
			got, err := store.Load(tc.sessionID)
			if err != nil {
				t.Fatalf("load after replace: %v", err)
			}
			if !bytes.Equal(got, second) {
				t.Fatalf("stored receipt = %s, want %s", got, second)
			}
			if _, err := os.Lstat(filepath.Join(dir, executionPreflightAttemptPendingName(tc.sessionID))); err != nil {
				t.Fatalf("attempt-pending marker missing after swap replace: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("receipt dir entries = %d, want 2 (receipt plus attempt-pending marker)", len(entries))
			}
		})
	}
}
