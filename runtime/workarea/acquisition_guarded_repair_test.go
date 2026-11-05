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
		//nolint:gosec // intentional unsafe mode to prove diagnosis and repair fail closed.
		if err := os.Chmod(storeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		//nolint:gosec // restore private mode for TempDir cleanup after the unsafe-mode probe.
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

	t.Run("differing old devices", func(t *testing.T) {
		parent := t.TempDir()
		committedReadyForReadback(t, parent)
		_, physical := legacyDriftForGuardedTest(t, parent)
		storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
		anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
		storeRecord := readStoreIdentityForTest(t, storePath)
		anchorRecord := readStoreIdentityForTest(t, anchorPath)
		// Same inode and store ID on both documents, but different old
		// devices: one side is forged. Diagnosis still reports
		// repair-required (inode alone matches while birth is absent);
		// only the locked differing-old-device refusal stops repair
		// from laundering the forgery.
		storeDevice := physical.Device + 1000
		if storeDevice == physical.Device {
			storeDevice++
		}
		anchorDevice := physical.Device + 2000
		if anchorDevice == physical.Device || anchorDevice == storeDevice {
			anchorDevice += 1000
		}
		storeRecord.Identity = FileIdentity{Device: storeDevice, Inode: physical.Inode}
		anchorRecord.Identity = FileIdentity{Device: anchorDevice, Inode: physical.Inode}
		writeStoreIdentityForTest(t, storePath, storeRecord)
		writeStoreIdentityForTest(t, anchorPath, anchorRecord)
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if !diagnosis.NeedsReanchor {
			t.Fatalf("differing old devices must still diagnose as repair-required: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted differing old devices")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("malformed identity", func(t *testing.T) {
		parent := t.TempDir()
		committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
		if err := os.WriteFile(storePath, []byte("{invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if diagnosis.Healthy || diagnosis.NeedsReanchor {
			t.Fatalf("malformed identity must fail closed without repair: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted a malformed identity document")
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
	// The parent anchor is left untouched so only the post-lock
	// path-identity re-check (SameFile against the pinned root plus the
	// diagnosed inode) can reject the swap: removing that check must let
	// the swap through. The locked re-check must still fail closed on
	// the pinned inode.
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
		// The parent anchor is deliberately left on the diagnosed legacy
		// identity: the pinned root still carries the same legacy
		// documents, so only the path-identity re-check distinguishes the
		// swap. An earlier revision also rewrote the anchor here, which
		// let the later anchor-mismatch reject mask deletion of the
		// path-identity guard.
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
		// Restore the pinned original when the hook ran so TempDir
		// cleanup sees the original tree; when repair correctly fails
		// before any write the restored tree must equal the pre-swap
		// snapshot asserted below.
		if _, err := os.Lstat(storeDir + ".orig"); err == nil {
			_ = os.RemoveAll(storeDir)
			_ = os.Rename(storeDir+".orig", storeDir)
		}
	})

	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted a swapped store directory")
	}
	// The failure must happen before any write to the pinned original:
	// the swapped forgery must carry no backups (repair must not have
	// consumed the replacement), and restoring the pinned original must
	// yield the exact pre-swap tree (repair must not have written to the
	// pinned root either).
	swappedEntries, err := os.ReadDir(filepath.Join(parent, acquisitionStoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range swappedEntries {
		if strings.Contains(entry.Name(), ".backup-") {
			t.Fatalf("repair wrote backups into the swapped replacement: %q", entry.Name())
		}
	}
	storeDir := filepath.Join(parent, acquisitionStoreDirName)
	if _, err := os.Lstat(storeDir + ".orig"); err == nil {
		_ = os.RemoveAll(storeDir)
		if err := os.Rename(storeDir+".orig", storeDir); err != nil {
			t.Fatal(err)
		}
		repairSwapHook = nil
		assertAcquisitionParentUnchanged(t, parent, before)
	}
}

// TestGuardedRepairPrePinSwapFails pins the diagnosed-inode pin: a swap
// planted after diagnosis but before the repair opens the store directory
// reuses the diagnosed store ID with a different-inode forgery. Only the
// diagnosed-inode re-check under the exclusive lock can reject it: the
// post-open SameFile checks pin the replacement itself once the swapped
// directory is opened, so removing the inode pin must let the swap through.
func TestGuardedRepairPrePinSwapFails(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	storeID, _ := legacyDriftForGuardedTest(t, parent)

	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnosis.NeedsReanchor || diagnosis.StoreID != storeID || diagnosis.Inode == 0 {
		t.Fatalf("pre-swap diagnosis = %+v, want needs-reanchor for %q", diagnosis, storeID)
	}
	before := snapshotAcquisitionParent(t, parent)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	origAnchorBody, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatal(err)
	}

	repairPreOpenHook = func() {
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
		oldDevice := forgedPhysical.Device + 1000
		if oldDevice == forgedPhysical.Device {
			oldDevice++
		}
		legacy := FileIdentity{Device: oldDevice, Inode: forgedPhysical.Inode}
		forged := acquisitionStoreIdentity{SchemaVersion: acquisitionStoreSchemaV1, StoreID: storeID, Identity: legacy}
		forgedBody, err := json.Marshal(forged)
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(storeDir, acquisitionStoreIdentityName), forgedBody, 0o600); err != nil {
			t.Error(err)
		}
		// Rewrite the parent anchor to the same forged legacy identity so
		// every check except the diagnosed-inode pin still passes: both
		// documents share the forged inode and device with matching store
		// IDs, and the empty forgery carries no acquisition rows to refuse.
		//nolint:gosec // test-only TempDir anchor path from Join(parent, constant).
		if err := os.WriteFile(anchorPath, forgedBody, 0o600); err != nil {
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
		repairPreOpenHook = nil
		storeDir := filepath.Join(parent, acquisitionStoreDirName)
		if _, err := os.Lstat(storeDir + ".orig"); err == nil {
			_ = os.RemoveAll(storeDir)
			_ = os.Rename(storeDir+".orig", storeDir)
			//nolint:gosec // test-only TempDir anchor restore; path from Join(parent, constant).
			_ = os.WriteFile(anchorPath, origAnchorBody, 0o600)
		}
	})

	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted a pre-pin swapped store directory")
	}
	swappedEntries, err := os.ReadDir(filepath.Join(parent, acquisitionStoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range swappedEntries {
		if strings.Contains(entry.Name(), ".backup-") {
			t.Fatalf("repair wrote backups into the pre-pin replacement: %q", entry.Name())
		}
	}
	storeDir := filepath.Join(parent, acquisitionStoreDirName)
	if _, err := os.Lstat(storeDir + ".orig"); err == nil {
		_ = os.RemoveAll(storeDir)
		if err := os.Rename(storeDir+".orig", storeDir); err != nil {
			t.Fatal(err)
		}
		//nolint:gosec // test-only TempDir anchor restore; path from Join(parent, constant).
		if err := os.WriteFile(anchorPath, origAnchorBody, 0o600); err != nil {
			t.Fatal(err)
		}
		repairPreOpenHook = nil
		assertAcquisitionParentUnchanged(t, parent, before)
	}
}

// TestGuardedRepairLockedStoreIDRecheckFails pins the locked storeID
// re-check: a swap after the pin-plus-lock that rewrites one locked
// document's store ID in place (same directory and inode) must fail closed
// with no writes. The forged record is re-pointed at the forged store ID
// so only the storeID re-check can reject the swap: removing it must let
// the swap through to a successful (laundering) repair.
func TestGuardedRepairLockedStoreIDRecheckFails(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	storeID, _ := legacyDriftForGuardedTest(t, parent)
	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	recordPath := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionRecordFileName)
	origStoreBody, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	origRecordBody, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotAcquisitionParent(t, parent)
	forgedStoreID := "wstore_ffffffffffffffffffffffffffffffff"
	if forgedStoreID == storeID {
		forgedStoreID = "wstore_00000000000000000000000000000000"
	}
	repairSwapHook = func() {
		body, err := os.ReadFile(storePath)
		if err != nil {
			t.Error(err)
			return
		}
		var rec acquisitionStoreIdentity
		if err := json.Unmarshal(body, &rec); err != nil {
			t.Error(err)
			return
		}
		rec.StoreID = forgedStoreID
		forged, err := json.Marshal(rec)
		if err != nil {
			t.Error(err)
			return
		}
		//nolint:gosec // test-only TempDir swap-hook forgery; path from Join(parent, constant).
		if err := os.WriteFile(storePath, forged, 0o600); err != nil {
			t.Error(err)
			return
		}
		recordBody, err := os.ReadFile(recordPath)
		if err != nil {
			t.Error(err)
			return
		}
		var doc map[string]any
		if err := json.Unmarshal(recordBody, &doc); err != nil {
			t.Error(err)
			return
		}
		doc["storeId"] = forgedStoreID
		reforged, err := json.Marshal(doc)
		if err != nil {
			t.Error(err)
			return
		}
		//nolint:gosec // test-only TempDir swap-hook forgery; path from Join(parent, constant).
		if err := os.WriteFile(recordPath, reforged, 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() {
		repairSwapHook = nil
		//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
		_ = os.WriteFile(storePath, origStoreBody, 0o600)
		//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
		_ = os.WriteFile(recordPath, origRecordBody, 0o600)
	})
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted a locked storeID mismatch")
	}
	for _, dir := range []string{filepath.Join(parent, acquisitionStoreDirName), parent} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), ".backup-") {
				t.Fatalf("repair wrote backups after a locked storeID mismatch: %q", entry.Name())
			}
		}
	}
	repairSwapHook = nil
	//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
	if err := os.WriteFile(storePath, origStoreBody, 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
	if err := os.WriteFile(recordPath, origRecordBody, 0o600); err != nil {
		t.Fatal(err)
	}
	assertAcquisitionParentUnchanged(t, parent, before)
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

// TestGuardedRepairRefusesStoreSideLegacyAlone pins the store-side legacy
// refusal: a current anchor (pinned to the present device) with a legacy
// store document (same inode, different device, no birth) must fail closed
// on the writable door and require explicit repair. Deleting the
// store-side guard lets the writable open auto-heal anchor-first without
// explicit repair. On platforms without birth the inode-only tolerance
// still heals, so only the diagnosis and repair legs are pinned there.
func TestGuardedRepairRefusesStoreSideLegacyAlone(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	storeID, physical := legacyDriftForGuardedTest(t, parent)
	if !physical.HasBirth {
		t.Skip("platform does not record birth; store-side legacy refusal needs birth")
	}
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)
	anchorRecord.Identity = physical
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)
	before := snapshotAcquisitionParent(t, parent)
	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Healthy || !diagnosis.NeedsReanchor || diagnosis.StoreID != storeID {
		t.Fatalf("store-side legacy diagnosis = %+v, want needs-reanchor for %q", diagnosis, storeID)
	}
	if _, err := NewAcquisitionStore(parent, nil); err == nil {
		t.Fatal("writable open auto-healed anchor-first without explicit repair")
	}
	assertAcquisitionParentUnchanged(t, parent, before)
	if err := RepairAcquisitionStore(parent); err != nil {
		t.Fatalf("repair of store-side legacy: %v", err)
	}
	after, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Healthy || after.NeedsReanchor || after.StoreID != storeID {
		t.Fatalf("post-repair diagnosis = %+v, want healthy for %q", after, storeID)
	}
}

// TestGuardedRepairRefusesAnchorSideLegacyAlone pins the anchor-side legacy
// refusal: a current store document (pinned to the present device) with a
// legacy parent anchor (same inode, old device, no birth) must fail closed
// on the writable door and require explicit repair. Deleting the
// anchor-side guard lets the writable open auto-heal the same directory
// without explicit repair or acquisition-record validation. On platforms
// without birth the inode-only tolerance still heals, so only the
// diagnosis and repair legs are pinned there.
func TestGuardedRepairRefusesAnchorSideLegacyAlone(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	storeID, physical := legacyDriftForGuardedTest(t, parent)
	if !physical.HasBirth {
		t.Skip("platform does not record birth; anchor-side legacy refusal needs birth")
	}
	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	storeRecord.Identity = physical
	writeStoreIdentityForTest(t, storePath, storeRecord)
	before := snapshotAcquisitionParent(t, parent)
	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Healthy || !diagnosis.NeedsReanchor || diagnosis.StoreID != storeID {
		t.Fatalf("anchor-side legacy diagnosis = %+v, want needs-reanchor for %q", diagnosis, storeID)
	}
	if _, err := NewAcquisitionStore(parent, nil); err == nil {
		t.Fatal("writable open auto-healed store-current without explicit repair")
	}
	assertAcquisitionParentUnchanged(t, parent, before)
	if err := RepairAcquisitionStore(parent); err != nil {
		t.Fatalf("repair of anchor-side legacy: %v", err)
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
		t.Fatalf("reopen after anchor-side repair: %v", err)
	}
	if reopened.storeID != storeID {
		t.Fatalf("store ID changed across anchor-side repair: %q vs %q", reopened.storeID, storeID)
	}
}

// TestGuardedRepairRefusesFIFORecord pins the file-type refusal: a FIFO at
// acquisition.json must fail closed without blocking while holding the
// exclusive store lock. Opening it with ReadFile alone blocks with no
// writer and wedges repair plus subsequent store users.
func TestGuardedRepairRefusesFIFORecord(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	recordPath := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionRecordFileName)
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(recordPath, 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotAcquisitionParent(t, parent)
	done := make(chan error, 1)
	go func() { done <- RepairAcquisitionStore(parent) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("repair admitted a FIFO acquisition record")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("repair blocked on FIFO acquisition record while holding exclusive store lock")
	}
	// Repair must not have replaced the FIFO or written backups.
	if fi, err := os.Lstat(recordPath); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("repair disturbed the FIFO record: %+v %v", fi, err)
	}
	_ = before
}

// TestGuardedRepairRefusesForeignUnpublishedClaim pins the unpublished-claim
// refusal: a valid foreign-store record under .claim-wac_* must fail closed
// with no writes, otherwise re-anchoring launders it and the next writable
// open deletes the foreign claim and its payload through recovery.
func TestGuardedRepairRefusesForeignUnpublishedClaim(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	foreignAcquisitionID := "wac_ffffffffffffffffffffffffffffffff"
	claimLeaf := ".claim-" + foreignAcquisitionID
	claimDir := filepath.Join(parent, acquisitionStoreDirName, claimLeaf)
	if err := os.Mkdir(claimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := committed
	foreign.AcquisitionID = foreignAcquisitionID
	foreign.StoreID = "wstore_ffffffffffffffffffffffffffffffff"
	foreign.State = AcquisitionClaiming
	foreignBody, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimDir, acquisitionRecordFileName), foreignBody, 0o600); err != nil {
		t.Fatal(err)
	}
	// Convert both identity documents to legacy drift so repair is attempted.
	_, _ = legacyDriftForGuardedTest(t, parent)
	before := snapshotAcquisitionParent(t, parent)
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted foreign unpublished acquisition")
	}
	assertAcquisitionParentUnchanged(t, parent, before)
}

// TestGuardedRepairRefusesSymlinkLock pins the canonical-lock check: a
// .store.lock symlink (for example to store.json) must fail closed even
// when the original exclusive lock is still held elsewhere. Locking the
// identity file instead would let repair rewrite both documents while the
// original writer lock remains held.
func TestGuardedRepairRefusesSymlinkLock(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	lockPath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreLockName)
	holder, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	fd, err := intFileDescriptor(holder.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	// While the original exclusive lock stays held on the renamed inode,
	// swap the path-visible lock for a symlink to store.json. Repair must
	// refuse the symlink rather than lock the identity file and rewrite
	// while the original writer lock is still held.
	if err := os.Rename(lockPath, lockPath+".orig"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(lockPath)
		if _, err := os.Lstat(lockPath + ".orig"); err == nil {
			_ = os.Rename(lockPath+".orig", lockPath)
		}
	})
	if err := os.Symlink(acquisitionStoreIdentityName, lockPath); err != nil {
		t.Fatal(err)
	}
	before := snapshotAcquisitionParent(t, parent)
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair succeeded through symlink lock while original exclusive store lock remained held")
	}
	assertAcquisitionParentUnchanged(t, parent, before)
}

// TestGuardedRepairRefusesMissingPublishedRecord pins the published-row
// completeness guard: a published wac_* directory without its
// acquisition.json must fail closed with no writes. An empty unpublished
// claim is an abandoned claim that recovery removes, but a published row
// without a record cannot prove ownership: re-anchoring would report
// healthy while the writable open still cannot read the row.
func TestGuardedRepairRefusesMissingPublishedRecord(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	recordPath := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionRecordFileName)
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	before := snapshotAcquisitionParent(t, parent)
	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnosis.NeedsReanchor {
		t.Fatalf("missing published record must still diagnose as repair-required at the identity layer: %+v", diagnosis)
	}
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted a published row without its acquisition record")
	}
	assertAcquisitionParentUnchanged(t, parent, before)
}

// TestGuardedRepairRefusesHardlinkedLock pins the canonical-lock hard-link
// guard: a .store.lock hard link to store.json must fail closed even when
// the original exclusive lock is still held elsewhere. Locking the
// identity-file inode instead would let repair rewrite both documents
// while the original writer lock remains held.
func TestGuardedRepairRefusesHardlinkedLock(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	lockPath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreLockName)
	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	holder, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	fd, err := intFileDescriptor(holder.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	// While the original exclusive lock stays held on the renamed inode,
	// swap the path-visible lock for a hard link to store.json. Repair
	// must refuse the aliased lock rather than flock the identity file
	// and rewrite while the original writer lock is still held.
	if err := os.Rename(lockPath, lockPath+".orig"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(lockPath)
		if _, err := os.Lstat(lockPath + ".orig"); err == nil {
			_ = os.Rename(lockPath+".orig", lockPath)
		}
	})
	if err := os.Link(storePath, lockPath); err != nil {
		t.Skipf("hard-link probe needs link support: %v", err)
	}
	before := snapshotAcquisitionParent(t, parent)
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair succeeded through hard-linked lock while original exclusive store lock remained held")
	}
	assertAcquisitionParentUnchanged(t, parent, before)
}

// TestGuardedRepairBusyLockTimesOut pins the bounded lock wait: a writer
// that never releases must not wedge explicit repair indefinitely. Repair
// polls the exclusive lock and refuses with a busy error after its
// deadline instead of blocking forever.
func TestGuardedRepairBusyLockTimesOut(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	lockPath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreLockName)
	holder, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	fd, err := intFileDescriptor(holder.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before := snapshotAcquisitionParent(t, parent)
	done := make(chan error, 1)
	go func() { done <- RepairAcquisitionStore(parent) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("repair succeeded while the exclusive store lock was held by a stuck writer")
		}
		if !strings.Contains(err.Error(), "busy") {
			t.Fatalf("stuck-writer repair error = %q, want a busy refusal", err)
		}
	case <-time.After(repairLockTimeout + 15*time.Second):
		t.Fatal("repair blocked indefinitely behind a stuck writer; want a bounded busy refusal")
	}
	assertAcquisitionParentUnchanged(t, parent, before)
}

// TestGuardedRepairRefusesBothDocumentsForgedAwayFromDiagnosedID pins the
// diagnosed-ID re-check: rewriting both locked identity documents to the
// same forged store ID (with the acquisition rows repointed to match, so
// only the diagnosed pin can refuse) must fail closed with no writes.
// Changing only one document is already refused by the
// document-to-document comparison; this test kills the mutant that drops
// the comparison against the diagnosed store ID.
func TestGuardedRepairRefusesBothDocumentsForgedAwayFromDiagnosedID(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	storeID, _ := legacyDriftForGuardedTest(t, parent)
	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	recordPath := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionRecordFileName)
	origStoreBody, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	origAnchorBody, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	origRecordBody, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotAcquisitionParent(t, parent)
	forgedStoreID := "wstore_ffffffffffffffffffffffffffffffff"
	if forgedStoreID == storeID {
		forgedStoreID = "wstore_00000000000000000000000000000000"
	}
	repairSwapHook = func() {
		for _, path := range []string{storePath, anchorPath} {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
				return
			}
			var rec acquisitionStoreIdentity
			if err := json.Unmarshal(body, &rec); err != nil {
				t.Error(err)
				return
			}
			rec.StoreID = forgedStoreID
			forged, err := json.Marshal(rec)
			if err != nil {
				t.Error(err)
				return
			}
			//nolint:gosec // test-only TempDir swap-hook forgery; path from Join(parent, constant).
			if err := os.WriteFile(path, forged, 0o600); err != nil {
				t.Error(err)
				return
			}
		}
		recordBody, err := os.ReadFile(recordPath)
		if err != nil {
			t.Error(err)
			return
		}
		var doc map[string]any
		if err := json.Unmarshal(recordBody, &doc); err != nil {
			t.Error(err)
			return
		}
		doc["storeId"] = forgedStoreID
		reforged, err := json.Marshal(doc)
		if err != nil {
			t.Error(err)
			return
		}
		//nolint:gosec // test-only TempDir swap-hook forgery; path from Join(parent, constant).
		if err := os.WriteFile(recordPath, reforged, 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() {
		repairSwapHook = nil
		//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
		_ = os.WriteFile(storePath, origStoreBody, 0o600)
		//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
		_ = os.WriteFile(anchorPath, origAnchorBody, 0o600)
		//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
		_ = os.WriteFile(recordPath, origRecordBody, 0o600)
	})
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted both locked identity documents changed away from diagnosed store ID")
	}
	for _, dir := range []string{filepath.Join(parent, acquisitionStoreDirName), parent} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), ".backup-") {
				t.Fatalf("repair wrote backups after a diagnosed-ID mismatch: %q", entry.Name())
			}
		}
	}
	repairSwapHook = nil
	//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
	if err := os.WriteFile(storePath, origStoreBody, 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
	if err := os.WriteFile(anchorPath, origAnchorBody, 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
	if err := os.WriteFile(recordPath, origRecordBody, 0o600); err != nil {
		t.Fatal(err)
	}
	assertAcquisitionParentUnchanged(t, parent, before)
}

// TestGuardedRepairRefusesAcquisitionIDMismatch pins the record-ID guard:
// an acquisition record whose ID differs from its directory name (with a
// matching store ID, so only the directory-identity comparison can refuse)
// must fail closed with no writes.
func TestGuardedRepairRefusesAcquisitionIDMismatch(t *testing.T) {
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
	forgedAcquisitionID := "wac_ffffffffffffffffffffffffffffffff"
	if forgedAcquisitionID == committed.AcquisitionID {
		forgedAcquisitionID = "wac_00000000000000000000000000000000"
	}
	doc["acquisitionId"] = forgedAcquisitionID
	forged, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, forged, 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotAcquisitionParent(t, parent)
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted an acquisition ID that differs from its directory")
	}
	assertAcquisitionParentUnchanged(t, parent, before)
}

// TestGuardedRepairRefusesSemanticallyInvalidRecords pins the three
// fail-closed record-content checks in the repair scan: semantic
// validation, trailing-data rejection, and unknown-field rejection. The
// pre-existing "malformed record" case feeds invalid JSON, which fails at
// decoding before any of these checks run, so disabling any one of them
// leaves that case (and the rest of the suite) green while admitting a
// forged record. Each subtest below forges a record that passes decoding
// but is refused by exactly one of the three checks, so removing that
// check lets repair through (RED on revert).
func TestGuardedRepairRefusesSemanticallyInvalidRecords(t *testing.T) {
	t.Run("invalid state passes decoding but fails validation", func(t *testing.T) {
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
		// Keep every identity field intact so only the semantic
		// validation can refuse: valid JSON, no trailing data, no
		// unknown fields, matching store and acquisition IDs, but a
		// state outside the closed set.
		doc["state"] = "bogus-state"
		forged, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(recordPath, forged, 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if !diagnosis.NeedsReanchor {
			t.Fatalf("invalid-state record must still diagnose as repair-required at the identity layer: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted an acquisition record with an invalid state")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("trailing data after valid record", func(t *testing.T) {
		parent := t.TempDir()
		committed := committedReadyForReadback(t, parent)
		_, _ = legacyDriftForGuardedTest(t, parent)
		recordPath := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionRecordFileName)
		body, err := os.ReadFile(recordPath)
		if err != nil {
			t.Fatal(err)
		}
		// The first JSON value is the intact valid record (so decoding
		// and semantic validation of that value would pass); only the
		// trailing-data check can refuse the extra value.
		forged := append(append([]byte{}, body...), []byte(" {}")...)
		//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
		if err := os.WriteFile(recordPath, forged, 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if !diagnosis.NeedsReanchor {
			t.Fatalf("trailing-data record must still diagnose as repair-required at the identity layer: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted an acquisition record with trailing data")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})

	t.Run("unknown field in valid record", func(t *testing.T) {
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
		// Every known field stays valid so decoding (without the
		// unknown-field guard), trailing-data, semantic validation, and
		// both identity comparisons would all pass; only the closed
		// decoder can refuse the forged extra field.
		doc["extra"] = 0
		forged, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(recordPath, forged, 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotAcquisitionParent(t, parent)
		diagnosis, err := DiagnoseAcquisitionStore(parent)
		if err != nil {
			t.Fatal(err)
		}
		if !diagnosis.NeedsReanchor {
			t.Fatalf("unknown-field record must still diagnose as repair-required at the identity layer: %+v", diagnosis)
		}
		if err := RepairAcquisitionStore(parent); err == nil {
			t.Fatal("repair admitted an acquisition record with an unknown field")
		}
		assertAcquisitionParentUnchanged(t, parent, before)
	})
}

// TestGuardedRepairRefusesReplacedLock pins the post-lock pathname check:
// replacing the canonical lock after the repair acquires its hold lets a
// concurrent holder lock the replacement. The repair must refuse instead
// of rewriting identity without mutual exclusion.
func TestGuardedRepairRefusesReplacedLock(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	oldHook := repairSwapHook
	defer func() { repairSwapHook = oldHook }()
	repairSwapHook = func() {
		storeDir := filepath.Join(parent, acquisitionStoreDirName)
		lockPath := filepath.Join(storeDir, acquisitionStoreLockName)
		if err := os.Rename(lockPath, lockPath+".old"); err != nil {
			t.Errorf("rename lock: %v", err)
			return
		}
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			t.Errorf("create replacement lock: %v", err)
			return
		}
		fd, err := intFileDescriptor(f.Fd())
		if err != nil {
			t.Errorf("lock replacement fd: %v", err)
			_ = f.Close()
			return
		}
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Errorf("lock replacement: %v", err)
		}
		go func() {
			time.Sleep(2 * time.Second)
			_ = syscall.Flock(fd, syscall.LOCK_UN)
			_ = f.Close()
		}()
	}
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair rewrote identity while a concurrent holder owned the replacement lock")
	}
}

// TestGuardedRepairDoesNotBlockOnRecordFIFORace pins the non-blocking
// record scan: a concurrent swap of a regular record for a FIFO between
// the check and the read must be refused, never blocked on, while the
// exclusive store lock is held.
func TestGuardedRepairDoesNotBlockOnRecordFIFORace(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	recordPath := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionRecordFileName)
	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 150; attempt++ {
		_, _ = legacyDriftForGuardedTest(t, parent)
		stop := make(chan struct{})
		swapped := make(chan struct{})
		go func() {
			defer close(swapped)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = os.Remove(recordPath)
				_ = syscall.Mkfifo(recordPath, 0o600)
				_ = os.Remove(recordPath)
				//nolint:gosec // test-only TempDir race swap; path from Join(parent, constant).
				_ = os.WriteFile(recordPath, body, 0o600)
			}
		}()
		done := make(chan error, 1)
		go func() { done <- RepairAcquisitionStore(parent) }()
		select {
		case repairErr := <-done:
			if repairErr != nil {
				t.Logf("attempt %d refused as expected: %v", attempt, repairErr)
			}
			close(stop)
			<-swapped
			// Restore the regular record before the next re-drift so
			// the fixture stays well-formed; either outcome (repaired
			// or refused on a FIFO window) is accepted, blocking is not.
			// The swapper has exited here, so the remove+write below
			// cannot race a fresh FIFO into a blocking open.
			_ = os.Remove(recordPath)
			//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
			_ = os.WriteFile(recordPath, body, 0o600)
		case <-time.After(5 * time.Second):
			close(stop)
			<-swapped
			_ = os.Remove(recordPath)
			//nolint:gosec // test-only TempDir restore; path from Join(parent, constant).
			_ = os.WriteFile(recordPath, body, 0o600)
			t.Fatalf("repair blocked on swapped FIFO at attempt %d while holding the exclusive lock", attempt)
		}
	}
}

// TestGuardedRepairRefusesSymlinkRecord pins the descriptor-relative
// no-follow record open: an in-root symlink for acquisition.json (pointing
// at a regular file with valid bytes in the same directory) must fail
// closed. os.Root's OpenFile follows such a link even with O_NOFOLLOW and
// the descriptor Stat then sees the regular target, so only a
// descriptor-relative openat with O_NOFOLLOW can refuse it. Deleting that
// open lets the repair follow the link and rewrite identity.
func TestGuardedRepairRefusesSymlinkRecord(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	recordDir := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID)
	recordPath := filepath.Join(recordDir, acquisitionRecordFileName)
	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(recordDir, "real.json")
	//nolint:gosec // test-only TempDir fixture; path from Join(parent, constant).
	if err := os.WriteFile(targetPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.json", recordPath); err != nil {
		t.Fatal(err)
	}
	before := snapshotAcquisitionParent(t, parent)
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted a symlinked acquisition record")
	}
	assertAcquisitionParentUnchanged(t, parent, before)
}

// TestGuardedRepairRefusesStoreDirSwapDuringScan pins the post-scan
// directory check: a canonical store-directory swap during the record scan
// leaves the pinned root pointing at the moved-away directory while the
// parent path no longer resolves to it. The pre-scan checks already passed
// before the swap and the lock re-check resolves through the pinned root,
// so only the post-scan directory comparison can refuse. Deleting it lets
// the repair write backups into the moved store and succeed.
func TestGuardedRepairRefusesStoreDirSwapDuringScan(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	oldHook := repairPostScanHook
	defer func() { repairPostScanHook = oldHook }()
	repairPostScanHook = func() {
		storeDir := filepath.Join(parent, acquisitionStoreDirName)
		if err := os.Rename(storeDir, storeDir+".moved"); err != nil {
			t.Errorf("rename store dir: %v", err)
		}
	}
	storeBefore, err := os.ReadFile(filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName))
	if err != nil {
		t.Fatal(err)
	}
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair succeeded after canonical store was replaced during record scan")
	}
	movedStore, err := os.ReadFile(filepath.Join(parent, acquisitionStoreDirName+".moved", acquisitionStoreIdentityName))
	if err != nil {
		t.Fatalf("moved store identity unreadable: %v", err)
	}
	if string(movedStore) != string(storeBefore) {
		t.Fatal("repair wrote into the moved store before refusing the swapped directory")
	}
}

// TestGuardedRepairRefusesReplacedLockAfterScan pins the final
// lock-canonical re-check: replacing the canonical lock after the record
// scan (so the post-lock re-check has already passed) must still fail
// closed. Deleting the final check lets the repair rewrite identity while
// a concurrent holder owns the replacement lock.
func TestGuardedRepairRefusesReplacedLockAfterScan(t *testing.T) {
	parent := t.TempDir()
	committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	oldHook := repairPostScanHook
	defer func() { repairPostScanHook = oldHook }()
	repairPostScanHook = func() {
		storeDir := filepath.Join(parent, acquisitionStoreDirName)
		lockPath := filepath.Join(storeDir, acquisitionStoreLockName)
		if err := os.Rename(lockPath, lockPath+".old"); err != nil {
			t.Errorf("rename lock: %v", err)
			return
		}
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			t.Errorf("create replacement lock: %v", err)
			return
		}
		fd, err := intFileDescriptor(f.Fd())
		if err != nil {
			t.Errorf("lock replacement fd: %v", err)
			_ = f.Close()
			return
		}
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Errorf("lock replacement: %v", err)
		}
		go func() {
			time.Sleep(2 * time.Second)
			_ = syscall.Flock(fd, syscall.LOCK_UN)
			_ = f.Close()
		}()
	}
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair rewrote identity while a concurrent holder owned the replacement lock")
	}
}

// TestGuardedRepairRefusesReadableFIFORecord pins the regular-file refusal
// in the record scan: a FIFO carrying valid record bytes is readable, so
// decoding, validation, and store-ID checks would all pass and only the
// regular-file check can refuse. Deleting it lets the repair admit the
// FIFO and rewrite identity.
func TestGuardedRepairRefusesReadableFIFORecord(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	_, _ = legacyDriftForGuardedTest(t, parent)
	recordPath := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionRecordFileName)
	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(recordPath, 0o600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(recordPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if _, err := holder.Write(body); err != nil {
		t.Fatal(err)
	}
	before := snapshotAcquisitionParent(t, parent)
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair admitted a FIFO acquisition record carrying valid bytes")
	}
	// The repair must not have replaced the FIFO or written backups.
	if fi, err := os.Lstat(recordPath); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("repair disturbed the FIFO record: %+v %v", fi, err)
	}
	after := snapshotAcquisitionParent(t, parent)
	for name := range after {
		if _, ok := before[name]; !ok && strings.Contains(name, ".backup-") {
			t.Fatalf("repair wrote backup %q for a refused FIFO record", name)
		}
	}
}
