//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

func restoredDirFor(root string) string {
	return filepath.Join(filepath.Dir(root), filepath.Base(root)+"-restored")
}

// TestWorkareaArchiveRegistry_RestoreV1SidecarCommitRefusesPlantedEntries
// drives the production RestoreV1 commit with entries planted at the exact
// sidecar name the restore will use (the clock is pinned so the fresh
// restore ID is predictable). Neither an outside link nor a pre-existing
// in-root file may be written through.
func TestWorkareaArchiveRegistry_RestoreV1SidecarCommitRefusesPlantedEntries(t *testing.T) {
	root := t.TempDir()
	const archiveID = "wa-sidecar-commit"
	writeFixtureArchive(t, root, fixtureArchive{id: archiveID})
	restoredDir := restoredDirFor(root)
	if err := os.MkdirAll(restoredDir, 0o700); err != nil {
		t.Fatalf("mkdir restored: %v", err)
	}
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	registry.now = func() time.Time { return at }
	sidecarPath := func() string {
		return filepath.Join(restoredDir, fmt.Sprintf("%s-restore-%d.json", archiveID, at.UnixNano()))
	}

	// (a) A link to a file outside the restored dir.
	outsideBody := `{"outside":true}`
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(outsideBody), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, sidecarPath()); err != nil {
		t.Fatalf("plant outside link: %v", err)
	}
	if _, _, err := registry.RestoreV1(archiveID, afclient.WorkareaRestoreRequest{IntoSessionID: "sess-a"}); err != nil {
		t.Fatalf("restore (outside link): %v", err)
	}
	if body, err := os.ReadFile(outside); err != nil || string(body) != outsideBody {
		t.Fatalf("restore commit wrote through planted outside link: %q, %v", body, err)
	}

	// (b) A pre-existing regular file at the fresh name.
	at = at.Add(time.Second)
	existingBody := `{"existing":true}`
	if err := os.WriteFile(sidecarPath(), []byte(existingBody), 0o600); err != nil {
		t.Fatalf("plant existing sidecar: %v", err)
	}
	if _, _, err := registry.RestoreV1(archiveID, afclient.WorkareaRestoreRequest{IntoSessionID: "sess-b"}); err != nil {
		t.Fatalf("restore (existing file): %v", err)
	}
	if body, err := os.ReadFile(sidecarPath()); err != nil || string(body) != existingBody {
		t.Fatalf("restore commit overwrote a pre-existing sidecar: %q, %v", body, err)
	}

	// A genuine fresh name still commits through the production path.
	at = at.Add(time.Second)
	if _, _, err := registry.RestoreV1(archiveID, afclient.WorkareaRestoreRequest{IntoSessionID: "sess-c"}); err != nil {
		t.Fatalf("restore (genuine): %v", err)
	}
	if inUse, err := registry.intoSessionIDInUse("sess-c"); err != nil || !inUse {
		t.Fatalf("genuine sidecar not committed: inUse=%v err=%v", inUse, err)
	}
}

// TestWorkareaArchiveRegistry_RestoreSidecarWriteIsExclusive pins exclusive
// creation: entries inside the restored dir, which root confinement alone
// does not refuse, must never be opened for writing.
func TestWorkareaArchiveRegistry_RestoreSidecarWriteIsExclusive(t *testing.T) {
	restoredDir := t.TempDir()
	restoredRoot, err := os.OpenRoot(restoredDir)
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = restoredRoot.Close() }()

	// (a) An in-root link to another sidecar.
	targetBody := `{"restoreId":"target"}`
	target := filepath.Join(restoredDir, "target.json")
	if err := os.WriteFile(target, []byte(targetBody), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink("target.json", filepath.Join(restoredDir, "linked.json")); err != nil {
		t.Fatalf("plant in-root link: %v", err)
	}
	if err := writeRestoreSidecar(restoredRoot, "linked", "linked.json", []byte(`{"restoreId":"linked"}`)); err == nil {
		t.Fatal("sidecar commit followed an in-root link; want creation refused")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != targetBody {
		t.Fatalf("in-root link target disturbed: %q, %v", body, err)
	}

	// (b) A pre-existing regular file at the fresh name.
	existingBody := `{"restoreId":"existing","pad":"keep-these-bytes"}`
	existing := filepath.Join(restoredDir, "existing.json")
	if err := os.WriteFile(existing, []byte(existingBody), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	if err := writeRestoreSidecar(restoredRoot, "existing", "existing.json", []byte(`{}`)); err == nil {
		t.Fatal("sidecar commit opened a pre-existing file; want creation refused")
	}
	if body, err := os.ReadFile(existing); err != nil || string(body) != existingBody {
		t.Fatalf("pre-existing sidecar disturbed: %q, %v", body, err)
	}

	// (c) An outside link.
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(restoredDir, "fresh.json")); err != nil {
		t.Fatalf("plant outside link: %v", err)
	}
	if err := writeRestoreSidecar(restoredRoot, "fresh", "fresh.json", []byte(`{"restoreId":"fresh"}`)); err == nil {
		t.Fatal("sidecar commit followed an outside link; want creation refused")
	}
	if body, err := os.ReadFile(outside); err != nil || string(body) != `{}` {
		t.Fatalf("outside target disturbed: %q, %v", body, err)
	}

	// Mismatched names are refused before any create.
	for _, tc := range []struct{ id, name string }{{"../x", "../x.json"}, {"a", "b.json"}} {
		if err := writeRestoreSidecar(restoredRoot, tc.id, tc.name, []byte(`{}`)); err == nil {
			t.Fatalf("writeRestoreSidecar(%q, %q) accepted an invalid name", tc.id, tc.name)
		}
	}

	// A genuine fresh name still commits and reads back.
	if err := writeRestoreSidecar(restoredRoot, "genuine", "genuine.json", []byte(`{"restoreId":"genuine"}`)); err != nil {
		t.Fatalf("genuine commit: %v", err)
	}
	if _, err := readRestoreSidecar(restoredRoot, "genuine", "genuine.json"); err != nil {
		t.Fatalf("genuine readback: %v", err)
	}
}

// plantInRootSidecarLink plants <restored>/<id>.json as a relative link to
// a real sidecar under <restored>/sub (which root confinement permits), plus a real <restored>/<id> directory, so
// everything but the regular-file check would accept the entry.
func plantInRootSidecarLink(t *testing.T, restoredDir, id, archiveID, sessionID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(restoredDir, "sub"), 0o700); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(restoredDir, id), 0o700); err != nil {
		t.Fatalf("mkdir restore dir: %v", err)
	}
	body := fmt.Sprintf(`{"restoreId":%q,"archiveId":%q,"intoSessionId":%q,"restoredAt":"2026-01-02T03:04:05Z"}`, id, archiveID, sessionID)
	if err := os.WriteFile(filepath.Join(restoredDir, "sub", "real.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write in-root target: %v", err)
	}
	if err := os.Symlink(filepath.Join("sub", "real.json"), filepath.Join(restoredDir, id+".json")); err != nil {
		t.Fatalf("plant in-root link: %v", err)
	}
}

func TestWorkareaArchiveRegistry_IntoSessionIDSkipsPlantedLinkAndFifo(t *testing.T) {
	root := t.TempDir()
	const archiveID = "wa-sidecar-scan"
	writeFixtureArchive(t, root, fixtureArchive{id: archiveID})
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	if _, _, err := registry.Restore(archiveID, afclient.WorkareaRestoreRequest{IntoSessionID: "sess-real"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restoredDir := restoredDirFor(root)

	// An outside link (refused by root confinement).
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"restoreId":"evil","archiveId":"wa-sidecar-scan","intoSessionId":"sess-evil"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(restoredDir, "evil.json")); err != nil {
		t.Fatal(err)
	}
	// An in-root link (only the regular-file check refuses it).
	plantInRootSidecarLink(t, restoredDir, "inroot", archiveID, "sess-inroot")
	// A fifo that sorts before the genuine sidecar: reading it would block.
	if err := syscall.Mkfifo(filepath.Join(restoredDir, "stall.json"), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	type result struct {
		evil, inroot, real bool
		err                error
	}
	done := make(chan result, 1)
	go func() {
		var res result
		var err error
		if res.evil, err = registry.intoSessionIDInUse("sess-evil"); err != nil {
			res.err = err
		}
		if res.inroot, err = registry.intoSessionIDInUse("sess-inroot"); err != nil {
			res.err = err
		}
		// Scans every entry, so it must pass the fifo without blocking.
		if res.real, err = registry.intoSessionIDInUse("sess-real"); err != nil {
			res.err = err
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("scan: %v", res.err)
		}
		if res.evil {
			t.Error("outside-linked sidecar content reported as in use")
		}
		if res.inroot {
			t.Error("in-root-linked sidecar content reported as in use")
		}
		if !res.real {
			t.Error("genuine sidecar lost")
		}
	case <-time.After(10 * time.Second):
		// Unblock the stalled reader so the goroutine can exit.
		if f, err := os.OpenFile(filepath.Join(restoredDir, "stall.json"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("scan blocked on planted fifo")
	}
}

// TestWorkareaArchiveRegistry_GetV1RefusesInRootLinkedSidecar pins the
// regular-file check on the legacy restore-ledger read used by GetV1.
func TestWorkareaArchiveRegistry_GetV1RefusesInRootLinkedSidecar(t *testing.T) {
	root := t.TempDir()
	const archiveID = "wa-sidecar-get"
	writeFixtureArchive(t, root, fixtureArchive{id: archiveID})
	restoredDir := restoredDirFor(root)
	plantInRootSidecarLink(t, restoredDir, "inroot", archiveID, "sess-inroot")
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})

	got, err := registry.GetV1("inroot")
	if err == nil {
		t.Fatalf("GetV1 served an in-root-linked sidecar: %+v", got)
	}
	if !errors.Is(err, ErrArchiveNotFound) {
		t.Fatalf("GetV1 error = %v, want not found", err)
	}

	// Control: the same bytes as a regular file are served.
	link := filepath.Join(restoredDir, "inroot.json")
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if err := os.Rename(filepath.Join(restoredDir, "sub", "real.json"), link); err != nil {
		t.Fatalf("move real sidecar: %v", err)
	}
	if got, err := registry.GetV1("inroot"); err != nil || got.ID != "inroot" {
		t.Fatalf("GetV1 control: got=%+v err=%v", got, err)
	}
}
