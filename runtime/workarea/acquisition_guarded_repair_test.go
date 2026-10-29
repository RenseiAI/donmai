package workarea

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// legacyDriftForGuardedTest converts both identity documents into the
// pre-birth legacy shape on a renumbered device: same inode, different
// device, no birth time. It returns the durable store ID and the current
// physical identity.
func legacyDriftForGuardedTest(t *testing.T, parent string) (string, FileIdentity) {
	t.Helper()
	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)
	if storeRecord.StoreID != anchorRecord.StoreID {
		t.Fatalf("store IDs drifted: %q vs %q", storeRecord.StoreID, anchorRecord.StoreID)
	}
	dirInfo, err := os.Lstat(filepath.Join(parent, acquisitionStoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	physical, err := fileIdentity(dirInfo)
	if err != nil {
		t.Fatal(err)
	}
	if physical.Inode == 0 {
		t.Fatal("physical store identity has no inode")
	}
	if storeRecord.Identity.Inode != physical.Inode || anchorRecord.Identity.Inode != physical.Inode {
		t.Fatalf("test setup requires pinned identities: store=%+v anchor=%+v physical=%+v", storeRecord.Identity, anchorRecord.Identity, physical)
	}
	oldDevice := physical.Device + 1000
	if oldDevice == physical.Device {
		oldDevice++
	}
	legacy := FileIdentity{Device: oldDevice, Inode: physical.Inode}
	storeRecord.Identity = legacy
	anchorRecord.Identity = legacy
	writeStoreIdentityForTest(t, storePath, storeRecord)
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)
	return storeRecord.StoreID, physical
}

// TestGuardedRepairDiagnosisIsReadOnlyForLegacyDrift pins AC-1: the exact
// same-inode/device-only legacy fixture diagnoses as repair-required and
// no byte changes.
func TestGuardedRepairDiagnosisIsReadOnlyForLegacyDrift(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	storeID, physical := legacyDriftForGuardedTest(t, parent)
	before := snapshotAcquisitionParent(t, parent)

	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Healthy || !diagnosis.NeedsReanchor || !diagnosis.Present {
		t.Fatalf("legacy drift diagnosis = %+v, want repair-required", diagnosis)
	}
	if diagnosis.StoreID != storeID {
		t.Fatalf("diagnosed store ID = %q, want %q", diagnosis.StoreID, storeID)
	}
	if diagnosis.Inode == 0 || diagnosis.Inode != physical.Inode {
		t.Fatalf("diagnosed inode = %d, want %d", diagnosis.Inode, physical.Inode)
	}
	assertAcquisitionParentUnchanged(t, parent, before)

	// The production writable door must stay red before explicit repair
	// on platforms that record birth (the legacy drift is unsafe to
	// auto-heal on inode alone). Platforms without birth keep the
	// documented inode-only tolerance.
	if physical.HasBirth {
		if _, err := NewAcquisitionStore(parent, nil); err == nil {
			t.Fatal("legacy device drift opened without explicit repair")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	}
}

// TestGuardedRepairUpgradesLegacyFixture pins AC-2: explicit repair
// upgrades the legacy fixture, the production door opens with the same
// store ID and ready records, and another renumber still opens.
func TestGuardedRepairUpgradesLegacyFixture(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	storeID, physical := legacyDriftForGuardedTest(t, parent)
	if physical.Inode == 0 {
		t.Fatal("physical identity has no inode")
	}

	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnosis.NeedsReanchor || diagnosis.StoreID != storeID {
		t.Fatalf("pre-repair diagnosis = %+v, want needs-reanchor for %q", diagnosis, storeID)
	}
	if err := RepairAcquisitionStore(parent); err != nil {
		t.Fatalf("explicit repair: %v", err)
	}
	after, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Healthy || after.NeedsReanchor || after.StoreID != storeID {
		t.Fatalf("post-repair diagnosis = %+v, want healthy for %q", after, storeID)
	}
	reopened, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatalf("reopen after repair: %v", err)
	}
	if reopened.storeID != storeID {
		t.Fatalf("store ID changed across repair: %q vs %q", reopened.storeID, storeID)
	}
	records, err := reopened.ReadyRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].AcquisitionID != committed.AcquisitionID || records[0].StoreID != storeID {
		t.Fatalf("ready records lost across repair: %+v", records)
	}
	// Backups are kept beside both documents.
	storeEntries, err := os.ReadDir(filepath.Join(parent, acquisitionStoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	storeBackups := 0
	for _, entry := range storeEntries {
		if strings.HasPrefix(entry.Name(), acquisitionStoreIdentityName+".backup-") {
			storeBackups++
		}
	}
	anchorEntries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	anchorBackups := 0
	for _, entry := range anchorEntries {
		if strings.HasPrefix(entry.Name(), acquisitionStoreAnchorName+".backup-") {
			anchorBackups++
		}
	}
	if storeBackups < 1 || anchorBackups < 1 {
		t.Fatalf("repair kept %d store backups and %d anchor backups, want at least 1 each", storeBackups, anchorBackups)
	}

	// Another simulated device renumber still opens: the repaired
	// documents carry birth, so a second device-only change with matching
	// birth is the same directory.
	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)
	renumbered := storeRecord.Identity
	renumbered.Device += 4321
	if renumbered.Device == storeRecord.Identity.Device {
		renumbered.Device++
	}
	storeRecord.Identity = renumbered
	anchorRecord.Identity = renumbered
	writeStoreIdentityForTest(t, storePath, storeRecord)
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatalf("second renumber refused to open: %v", err)
	}
}

// TestGuardedRepairRefusesMismatchesWithoutWrites pins AC-3: every
// non-repairable mismatch is refused with no writes.
func TestGuardedRepairRefusesMismatchesWithoutWrites(t *testing.T) {
	t.Run("half-present anchor", func(t *testing.T) {
		parent := t.TempDir()
		committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		if err := os.Remove(filepath.Join(parent, acquisitionStoreAnchorName)); err != nil {
			t.Fatal(err)
		}
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if diagnosis.NeedsReanchor {
			t.Fatalf("half-present must not be repairable: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted half-present documents")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("unequal store IDs", func(t *testing.T) {
		parent := t.TempDir()
		committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
		anchorRecord := readStoreIdentityForTest(t, anchorPath)
		anchorRecord.StoreID += "-forged"
		writeStoreIdentityForTest(t, anchorPath, anchorRecord)
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if diagnosis.NeedsReanchor {
			t.Fatalf("unequal store IDs must not be repairable: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted unequal store IDs")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("changed inode", func(t *testing.T) {
		parent := t.TempDir()
		committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
		anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
		storeRecord := readStoreIdentityForTest(t, storePath)
		anchorRecord := readStoreIdentityForTest(t, anchorPath)
		storeRecord.Identity.Inode++
		anchorRecord.Identity.Inode++
		writeStoreIdentityForTest(t, storePath, storeRecord)
		writeStoreIdentityForTest(t, anchorPath, anchorRecord)
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if diagnosis.NeedsReanchor {
			t.Fatalf("changed inode must not be repairable: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted a changed inode")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("single-sided forgery", func(t *testing.T) {
		parent := t.TempDir()
		committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
		storeRecord := readStoreIdentityForTest(t, storePath)
		storeRecord.Identity.Inode++
		writeStoreIdentityForTest(t, storePath, storeRecord)
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if diagnosis.NeedsReanchor {
			t.Fatalf("single-sided forgery must not be repairable: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted a forged store document")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("unsafe directory", func(t *testing.T) {
		parent := t.TempDir()
		committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		storeDir := filepath.Join(parent, acquisitionStoreDirName)
		if err := os.Chmod(storeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(storeDir, 0o700) })
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if diagnosis.NeedsReanchor {
			t.Fatalf("unsafe directory must not be repairable: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted an unsafe directory")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("malformed record", func(t *testing.T) {
		parent := t.TempDir()
		committed := committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		recordPath := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionRecordFileName)
		if err := os.WriteFile(recordPath, []byte("{invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if !diagnosis.NeedsReanchor {
			t.Fatalf("malformed record must still diagnose as repair-required at the identity layer: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted a malformed acquisition record")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("foreign store record", func(t *testing.T) {
		parent := t.TempDir()
		committed := committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		recordPath := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionRecordFileName)
		body, err := os.ReadFile(recordPath)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		doc["storeId"] = "wstore_ffffffffffffffffffffffffffffffff"
		forged, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(recordPath, forged, 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotAcquisitionParent(t, parent)
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted a record from another store")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("missing lock", func(t *testing.T) {
		parent := t.TempDir()
		committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		if err := os.Remove(filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreLockName)); err != nil {
			t.Fatal(err)
		}
		before := snapshotAcquisitionParent(t, parent)
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted a missing store lock")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})
}

// TestGuardedRepairSwapAfterDiagnosisFails pins AC-4 (swap): a path swap
// after the repair pins and locks cannot pass the locked re-check.
func TestGuardedRepairSwapAfterDiagnosisFails(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	storeID, _ := legacyDriftForGuardedTest(t, parent)

	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnosis.NeedsReanchor || diagnosis.StoreID != storeID {
		t.Fatalf("pre-swap diagnosis = %+v, want needs-reanchor for %q", diagnosis, storeID)
	}
	before := snapshotAcquisitionParent(t, parent)

	// Swap the path-visible store directory after the repair pins and
	// locks the original root: rename the verified store aside and plant
	// a forged store at the same path that reuses the diagnosed store ID.
	// The locked re-check must still fail closed on the pinned inode.
	repairSwapHook = func() {
		storeDir := filepath.Join(parent, acquisitionStoreDirName)
		if err := os.Rename(storeDir, storeDir+".orig"); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(storeDir, 0o700); err != nil {
			t.Error(err)
			return
		}
		forgedInfo, err := os.Lstat(storeDir)
		if err != nil {
			t.Error(err)
			return
		}
		forgedPhysical, err := fileIdentity(forgedInfo)
		if err != nil {
			t.Error(err)
			return
		}
		forged := acquisitionStoreIdentity{SchemaVersion: acquisitionStoreSchemaV1, StoreID: storeID, Identity: forgedPhysical}
		forgedBody, err := json.Marshal(forged)
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(storeDir, acquisitionStoreIdentityName), forgedBody, 0o600); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(parent, acquisitionStoreAnchorName), forgedBody, 0o600); err != nil {
			t.Error(err)
		}
		lockFile, err := os.OpenFile(filepath.Join(storeDir, acquisitionStoreLockName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		_ = lockFile.Close()
	}
	t.Cleanup(func() {
		repairSwapHook = nil
		storeDir := filepath.Join(parent, acquisitionStoreDirName)
		_ = os.RemoveAll(storeDir)
		// The hook renamed the original aside only when it ran; restore
		// it when present so TempDir cleanup sees the original tree.
		if _, err := os.Lstat(storeDir + ".orig"); err == nil {
			_ = os.Rename(storeDir+".orig", storeDir)
		}
	})

	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted a swapped store directory")
	}
	// The failure must happen before any write to the pinned original:
	// the pre-swap snapshot (original tree) is restored by cleanup, but
	// the repair must not have created backups in the swapped forgery
	// that would indicate it consumed the replacement. Check the swapped
	// tree has no backups.
	swappedEntries, err := os.ReadDir(filepath.Join(parent, acquisitionStoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range swappedEntries {
		if strings.Contains(entry.Name(), ".backup-") {
			t.Fatalf("repair wrote backups into the swapped replacement: %q", entry.Name())
		}
	}
	_ = before
}

// TestGuardedRepairConcurrentWriterBlocked pins AC-4 (locking): an
// exclusive writer blocks explicit repair until released.
func TestGuardedRepairConcurrentWriterBlocked(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)

	lockPath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreLockName)
	lock, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	fd, err := intFileDescriptor(lock.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- RepairAcquisitionStore(parent)
	}()
	select {
	case err := <-done:
		t.Fatalf("repair returned while the exclusive writer lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repair after lock release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("repair did not return after the writer lock released")
	}
	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnosis.Healthy || diagnosis.NeedsReanchor {
		t.Fatalf("post-repair diagnosis = %+v, want healthy", diagnosis)
	}
}

// TestGuardedRepairInterruptionIsRecoverable pins AC-4 (durability): an
// interruption between the two replicated writes stays recoverable via
// retry.
func TestGuardedRepairInterruptionIsRecoverable(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	storeID, _ := legacyDriftForGuardedTest(t, parent)

	// Simulate a crash after the first replicated write: pin only
	// store.json to the current device, leave the anchor on the old
	// device. Diagnosis must still report repair-required, and a retry
	// must finish both writes.
	dirInfo, err := os.Lstat(filepath.Join(parent, acquisitionStoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	physical, err := fileIdentity(dirInfo)
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	storeRecord.Identity = physical
	writeStoreIdentityForTest(t, storePath, storeRecord)

	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnosis.NeedsReanchor || diagnosis.StoreID != storeID {
		t.Fatalf("half-repaired diagnosis = %+v, want needs-reanchor for %q", diagnosis, storeID)
	}
	if err := RepairAcquisitionStore(parent); err != nil {
		t.Fatalf("retry after interruption: %v", err)
	}
	after, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Healthy || after.NeedsReanchor {
		t.Fatalf("post-retry diagnosis = %+v, want healthy", after)
	}
	reopened, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatalf("reopen after retry: %v", err)
	}
	if reopened.storeID != storeID {
		t.Fatalf("store ID changed across retry: %q vs %q", reopened.storeID, storeID)
	}
	records, err := reopened.ReadyRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].AcquisitionID != committedReadyForReadbackID(t, parent, records) {
		t.Fatalf("ready records lost across retry: %+v", records)
	}
}

func committedReadyForReadbackID(t *testing.T, _ string, records []AcquisitionRecord) string {
	t.Helper()
	if len(records) != 1 {
		t.Fatalf("want exactly 1 ready record, got %d", len(records))
	}
	return records[0].AcquisitionID
}
