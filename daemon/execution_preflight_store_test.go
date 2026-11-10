package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/executioncell"
)

// differingReceipt builds a second valid receipt for sessionID whose bytes
// differ from first by carrying a different worker identity. Both decode
// cleanly, so only the store's spawn gating decides the outcome.
func differingReceipt(t *testing.T, sessionID, workerID string) []byte {
	t.Helper()
	return rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID,
		WorkerID:        workerID,
		PlacementID:     "host-local",
		Decision:        "denied",
		Denial:          "unsupported",
	})
}

func firstReceipt(t *testing.T, sessionID string) []byte {
	t.Helper()
	return differingReceipt(t, sessionID, "worker-1")
}

func writeMarkerFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"contractVersion":"test-marker/v1","requestId":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A redelivered attempt that recompiles different bytes must be published over
// the stale receipt when the session never spawned, and refused once the
// spawn (or start authority) consumed it. Every refusal wraps os.ErrExist and
// leaves the stored bytes untouched.
func TestFileExecutionPreflightStorePersistReplacesOnlyUnspawnedReceipt(t *testing.T) {
	const sessionID = "session-redelivery"
	tests := []struct {
		name string
		// setup runs after the first receipt is persisted; it may record a
		// lifecycle marker or plant a hostile incumbent.
		setup func(t *testing.T, dir string, store *FileExecutionPreflightStore)
		// secondWorker is the worker identity carried by the republished
		// receipt; it always differs from the first receipt's worker.
		secondWorker string
		// wantReplaced reports whether the second persist must win.
		wantReplaced bool
	}{
		{
			name:         "never spawned differing receipt is replaced",
			setup:        func(t *testing.T, dir string, store *FileExecutionPreflightStore) {},
			secondWorker: "worker-2",
			wantReplaced: true,
		},
		{
			name: "spawned session keeps its receipt",
			setup: func(t *testing.T, dir string, store *FileExecutionPreflightStore) {
				if err := store.MarkExecutionPreflightSpawned(sessionID); err != nil {
					t.Fatalf("mark spawned: %v", err)
				}
			},
			secondWorker: "worker-2",
			wantReplaced: false,
		},
		{
			name: "consumed start authority keeps the receipt",
			setup: func(t *testing.T, dir string, store *FileExecutionPreflightStore) {
				writeMarkerFile(t, dir, executionPreflightStartName(sessionID))
			},
			secondWorker: "worker-2",
			wantReplaced: false,
		},
		{
			name: "retired session keeps its receipt",
			setup: func(t *testing.T, dir string, store *FileExecutionPreflightStore) {
				writeMarkerFile(t, dir, executionPreflightRetirementName(sessionID))
			},
			secondWorker: "worker-2",
			wantReplaced: false,
		},
		{
			name: "symlink destination is refused without touching its target",
			setup: func(t *testing.T, dir string, store *FileExecutionPreflightStore) {
				target := filepath.Join(dir, "swap-target.json")
				if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				finalPath := filepath.Join(dir, executionPreflightReceiptName(sessionID))
				if err := os.Remove(finalPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("swap-target.json", finalPath); err != nil {
					t.Fatal(err)
				}
			},
			secondWorker: "worker-2",
			wantReplaced: false,
		},
		{
			name: "non-regular destination is refused",
			setup: func(t *testing.T, dir string, store *FileExecutionPreflightStore) {
				finalPath := filepath.Join(dir, executionPreflightReceiptName(sessionID))
				if err := os.Remove(finalPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(finalPath, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			secondWorker: "worker-2",
			wantReplaced: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewFileExecutionPreflightStore(dir)
			first := firstReceipt(t, sessionID)
			if err := store.Persist(sessionID, first); err != nil {
				t.Fatalf("first persist: %v", err)
			}
			tc.setup(t, dir, store)
			second := differingReceipt(t, sessionID, tc.secondWorker)
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
				entries, readErr := os.ReadDir(dir)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if len(entries) != 1 {
					t.Fatalf("receipt dir entries = %d, want 1 (no leftover pending or marker files)", len(entries))
				}
				return
			}
			if err == nil {
				t.Fatal("differing republish of a guarded receipt succeeded")
			}
			if !errors.Is(err, os.ErrExist) {
				t.Fatalf("differing republish error = %v, want it to wrap os.ErrExist", err)
			}
			var conflict *executionPreflightReceiptConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("differing republish error = %T, want the typed receipt conflict", err)
			}
			if tc.name == "symlink destination is refused without touching its target" {
				if got, readErr := os.ReadFile(filepath.Join(dir, "swap-target.json")); readErr != nil || string(got) != "outside" {
					t.Fatalf("symlink target changed: %q err=%v", got, readErr)
				}
				if info, statErr := os.Lstat(filepath.Join(dir, executionPreflightReceiptName(sessionID))); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("symlink destination was replaced: %+v err=%v", info, statErr)
				}
				return
			}
			if tc.name == "non-regular destination is refused" {
				return
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

// A name swapped from a regular receipt to a symlink between the incumbent
// check and the publish must never write through the link. The rename replaces
// the directory entry itself, so the outside target stays intact and the name
// ends up a regular file holding the new bytes.
func TestFileExecutionPreflightStorePersistReplaceNeverFollowsSwappedSymlink(t *testing.T) {
	dir := t.TempDir()
	const sessionID = "session-replace-swap"
	store := NewFileExecutionPreflightStore(dir)
	first := firstReceipt(t, sessionID)
	if err := store.Persist(sessionID, first); err != nil {
		t.Fatalf("first persist: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalName := executionPreflightReceiptName(sessionID)
	var calls atomic.Int32
	store.preOpenHook = func() {
		// Swap only in the replacement Lstat-to-rename window (the second
		// hook invocation): the first runs inside the equality check and
		// must observe the undisturbed regular file.
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
	second := differingReceipt(t, sessionID, "worker-2")
	if err := store.Persist(sessionID, second); err != nil {
		t.Fatalf("replacement persist across symlink swap: %v", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "unchanged" {
		t.Fatalf("outside symlink target changed: %q err=%v", got, err)
	}
	info, err := os.Lstat(filepath.Join(dir, finalName))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("published name is not a regular file: %+v err=%v", info, err)
	}
	got, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("load after replace: %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("stored receipt = %s, want %s", got, second)
	}
}

// Marking the spawn is idempotent, fsynced, and never alters the receipt: a
// duplicate mark reaffirms the directory, and the marker plus receipt are the
// only entries left behind.
func TestFileExecutionPreflightStoreMarkSpawnedIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	const sessionID = "session-marked"
	store := NewFileExecutionPreflightStore(dir)
	receipt := firstReceipt(t, sessionID)
	if err := store.Persist(sessionID, receipt); err != nil {
		t.Fatalf("persist: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := store.MarkExecutionPreflightSpawned(sessionID); err != nil {
			t.Fatalf("mark spawned (attempt %d): %v", i, err)
		}
	}
	got, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !bytes.Equal(got, receipt) {
		t.Fatalf("stored receipt = %s, want %s", got, receipt)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("receipt dir entries = %v, want receipt plus spawn marker", names)
	}
	if err := store.Persist(sessionID, differingReceipt(t, sessionID, "worker-2")); err == nil {
		t.Fatal("differing republish after spawn mark succeeded")
	} else if !errors.Is(err, os.ErrExist) {
		t.Fatalf("differing republish error = %v, want it to wrap os.ErrExist", err)
	}
	if err := store.Persist(sessionID, receipt); err != nil {
		t.Fatalf("identical republish after spawn mark: %v", err)
	}
}

// An identical republish of a never-spawned receipt stays idempotent: the
// link races, the bytes compare equal, and the directory is reaffirmed without
// creating markers or pending leftovers.
func TestFileExecutionPreflightStorePersistIdenticalUnspawnedIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	const sessionID = "session-identical-unspawned"
	store := NewFileExecutionPreflightStore(dir)
	receipt := firstReceipt(t, sessionID)
	if err := store.Persist(sessionID, receipt); err != nil {
		t.Fatalf("first persist: %v", err)
	}
	if err := store.Persist(sessionID, append([]byte(nil), receipt...)); err != nil {
		t.Fatalf("identical republish: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("receipt dir entries = %v, want exactly the receipt", entries)
	}
	got, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !bytes.Equal(got, receipt) {
		t.Fatalf("stored receipt = %s, want %s", got, receipt)
	}
}
