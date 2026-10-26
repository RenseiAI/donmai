package workarea

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readStoreIdentityForTest(t *testing.T, path string) acquisitionStoreIdentity {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record acquisitionStoreIdentity
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func writeStoreIdentityForTest(t *testing.T, path string, record acquisitionStoreIdentity) {
	t.Helper()
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAcquisitionStoreToleratesDeviceRenumberWithSameInode(t *testing.T) {
	parent := t.TempDir()
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}
	storeID := readStoreIdentityForTest(t, filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)).StoreID

	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)
	if storeRecord.StoreID != storeID || anchorRecord.StoreID != storeID {
		t.Fatalf("store IDs drifted: %q vs %q vs %q", storeRecord.StoreID, anchorRecord.StoreID, storeID)
	}
	if storeRecord.Identity.Inode == 0 {
		t.Fatal("store identity has no inode")
	}
	renumbered := storeRecord.Identity
	renumbered.Device += 1000
	if renumbered.Device == storeRecord.Identity.Device {
		renumbered.Device++
	}
	storeRecord.Identity = renumbered
	anchorRecord.Identity = renumbered
	writeStoreIdentityForTest(t, storePath, storeRecord)
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)

	reopened, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatalf("renumbered store refused to open: %v", err)
	}
	if reopened.storeID != storeID {
		t.Fatalf("store ID changed across renumber: %q vs %q", reopened.storeID, storeID)
	}

	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnosis.Healthy || !diagnosis.Present || diagnosis.NeedsReanchor {
		t.Fatalf("store should be re-anchored healthy after open: %+v", diagnosis)
	}
	if diagnosis.StoreID != storeID {
		t.Fatalf("diagnosed store ID = %q, want %q", diagnosis.StoreID, storeID)
	}
}

func TestAcquisitionStoreRefusesDifferentInode(t *testing.T) {
	parent := t.TempDir()
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}

	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)
	forged := storeRecord.Identity
	forged.Inode++
	storeRecord.Identity = forged
	anchorRecord.Identity = forged
	writeStoreIdentityForTest(t, storePath, storeRecord)
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)

	if _, err := NewAcquisitionStore(parent, nil); err == nil {
		t.Fatal("replacement directory with a different inode opened without error")
	}
	if _, _, err := ReadExistingReadyRecordsFound(parent); err == nil {
		t.Fatal("read-only scan accepted a replacement directory")
	}
	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Healthy || diagnosis.NeedsReanchor {
		t.Fatalf("different inode must not be repairable: %+v", diagnosis)
	}
}

func TestAcquisitionReadOnlyScanToleratesRenumberWithoutMutating(t *testing.T) {
	parent := t.TempDir()
	committed := committedReadyForReadback(t, parent)
	before := snapshotAcquisitionParent(t, parent)

	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)
	renumbered := storeRecord.Identity
	renumbered.Device += 777
	if renumbered.Device == storeRecord.Identity.Device {
		renumbered.Device++
	}
	storeRecord.Identity = renumbered
	anchorRecord.Identity = renumbered
	// Bypass the store handle so no auto-heal runs; rewrite the files directly.
	writeStoreIdentityForTest(t, storePath, storeRecord)
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)
	renumberedSnapshot := snapshotAcquisitionParent(t, parent)

	records, err := ReadExistingReadyRecords(parent)
	if err != nil {
		t.Fatalf("read-only scan refused a renumbered store: %v", err)
	}
	if len(records) != 1 || records[0].AcquisitionID != committed.AcquisitionID {
		t.Fatalf("read-only scan lost the ready row: %+v", records)
	}
	assertAcquisitionParentUnchanged(t, parent, renumberedSnapshot)
	_ = before
}

func TestAcquisitionDiagnoseAndRepairRenumber(t *testing.T) {
	parent := t.TempDir()
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}
	storeID := readStoreIdentityForTest(t, filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)).StoreID

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

	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Healthy || !diagnosis.NeedsReanchor || diagnosis.StoreID != storeID {
		t.Fatalf("renumber diagnosis = %+v, want needs-reanchor for %q", diagnosis, storeID)
	}
	if err := RepairAcquisitionStore(parent); err != nil {
		t.Fatalf("repair: %v", err)
	}
	after, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Healthy || after.NeedsReanchor {
		t.Fatalf("post-repair diagnosis = %+v, want healthy", after)
	}
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatalf("reopen after repair: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(parent, acquisitionStoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	storeBackups := 0
	for _, entry := range entries {
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
}

func TestOpenRootExactToleratesDeviceRenumber(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "root")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	renumbered := identity
	renumbered.Device += 9999
	if renumbered.Device == identity.Device {
		renumbered.Device++
	}
	handle, err := OpenRootExact(RootPath(dir), renumbered)
	if err != nil {
		t.Fatalf("OpenRootExact refused a device-only renumber: %v", err)
	}
	_ = handle.Close()

	forged := identity
	forged.Inode++
	if _, err := OpenRootExact(RootPath(dir), forged); err == nil {
		t.Fatal("OpenRootExact accepted a different inode")
	}
}
