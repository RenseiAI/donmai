package workarea

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

// TestFileIdentityMatchesRejectsBirthMismatch pins the birth guard in
// fileIdentityMatches: the same inode on a renumbered device matches only
// when the birth time matches too. Deleting the birth comparison must turn
// this test RED; otherwise an inode-reusing replacement would be
// misclassified as a renumber and auto-repaired.
func TestFileIdentityMatchesRejectsBirthMismatch(t *testing.T) {
	stored := FileIdentity{Device: 11, Inode: 1001, BirthSec: 1000, BirthNsec: 2, HasBirth: true}
	renumbered := FileIdentity{Device: 12, Inode: 1001, BirthSec: 1000, BirthNsec: 2, HasBirth: true}
	if !fileIdentityMatches(stored, renumbered) {
		t.Fatal("same inode and birth on a renumbered device must match")
	}
	reused := renumbered
	reused.BirthSec++
	if fileIdentityMatches(stored, reused) {
		t.Fatal("same inode with a different birth time must not match (inode reuse)")
	}
	reusedNsec := renumbered
	reusedNsec.BirthNsec++
	if fileIdentityMatches(stored, reusedNsec) {
		t.Fatal("same inode with a different birth nanosecond must not match")
	}
	// Records written before birth tracking carry no birth time; the inode
	// alone decides so old records migrate forward instead of crash-looping.
	legacy := FileIdentity{Device: 12, Inode: 1001}
	if !fileIdentityMatches(stored, legacy) {
		t.Fatal("stored birth with a birthless current directory must match on inode")
	}
	if !fileIdentityMatches(legacy, renumbered) {
		t.Fatal("birthless stored identity must match on inode")
	}
	otherInode := renumbered
	otherInode.Inode++
	if fileIdentityMatches(stored, otherInode) {
		t.Fatal("different inode must not match even with the same birth")
	}
	if fileIdentityMatches(FileIdentity{}, renumbered) {
		t.Fatal("zero stored identity must not match")
	}
}

// TestAcquisitionStoreRefusesBirthMismatch pins the fail-closed side of the
// birth guard at the store level: the same inode on a renumbered device
// with a different birth time (inode reuse) is a replacement, not a
// renumber, so open fails, diagnosis refuses repair, and repair refuses.
func TestAcquisitionStoreRefusesBirthMismatch(t *testing.T) {
	parent := t.TempDir()
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Lstat(filepath.Join(parent, acquisitionStoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	physical, err := fileIdentity(dirInfo)
	if err != nil {
		t.Fatal(err)
	}
	if !physical.HasBirth {
		if runtime.GOOS == "darwin" {
			t.Fatal("fileIdentity must record birth time on darwin; HasBirth=false breaks renumber vs replacement distinction")
		}
		t.Skip("platform does not record birth time; store-level birth refusal is covered by TestFileIdentityMatchesRejectsBirthMismatch")
	}

	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)
	if !storeRecord.Identity.HasBirth || !anchorRecord.Identity.HasBirth {
		t.Fatal("store identity should carry birth time on this platform")
	}
	reused := storeRecord.Identity
	reused.Device += 1000
	if reused.Device == storeRecord.Identity.Device {
		reused.Device++
	}
	reused.BirthSec++
	storeRecord.Identity = reused
	anchorRecord.Identity = reused
	writeStoreIdentityForTest(t, storePath, storeRecord)
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)

	if _, err := NewAcquisitionStore(parent, nil); err == nil {
		t.Fatal("store with a mismatched birth time opened without error")
	}
	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Healthy || diagnosis.NeedsReanchor {
		t.Fatalf("birth mismatch must fail closed without repair: %+v", diagnosis)
	}
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair accepted a birth-mismatched directory")
	}
}

// TestAcquisitionStoreRefusesStoreIDMismatchWithRenumber pins the StoreID
// second factor: a store.json StoreID that differs from the anchor, with a
// device-only drift that still matches on inode (and birth), must refuse on
// open, on the read-only scan, and in diagnosis, and must not be repairable.
// Deleting any of the StoreID guards must turn this test RED; otherwise a
// forged store ID would open, scan, or diagnose as healthy on inode alone.
func TestAcquisitionStoreRefusesStoreIDMismatchWithRenumber(t *testing.T) {
	parent := t.TempDir()
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}

	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)

	renumbered := storeRecord.Identity
	renumbered.Device += 1000
	if renumbered.Device == storeRecord.Identity.Device {
		renumbered.Device++
	}
	storeRecord.Identity = renumbered
	anchorRecord.Identity = renumbered
	storeRecord.StoreID += "-forged"
	writeStoreIdentityForTest(t, storePath, storeRecord)
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)

	if _, err := NewAcquisitionStore(parent, nil); err == nil {
		t.Fatal("store with a mismatched store ID opened without error")
	}
	if _, _, err := ReadExistingReadyRecordsFound(parent); err == nil {
		t.Fatal("read-only scan accepted a mismatched store ID")
	}
	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Healthy || diagnosis.NeedsReanchor {
		t.Fatalf("store ID mismatch must fail closed without repair: %+v", diagnosis)
	}
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair accepted a mismatched store ID")
	}
}

// TestAcquisitionStoreRefusesSingleSidedForgery pins the one-sided identity
// guards: a store.json-only forged inode with the anchor intact must refuse
// on open, on the read-only scan, and in diagnosis, and must not be
// repairable. Deleting the store-side identity check or weakening the
// diagnose disjunction must turn this test RED; otherwise a forged store
// document would open or be repaired while the anchor still matches.
func TestAcquisitionStoreRefusesSingleSidedForgery(t *testing.T) {
	parent := t.TempDir()
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}

	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)
	if storeRecord.StoreID != anchorRecord.StoreID {
		t.Fatalf("store IDs drifted: %q vs %q", storeRecord.StoreID, anchorRecord.StoreID)
	}

	forged := storeRecord.Identity
	forged.Inode++
	storeRecord.Identity = forged
	writeStoreIdentityForTest(t, storePath, storeRecord)
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)

	if _, err := NewAcquisitionStore(parent, nil); err == nil {
		t.Fatal("store with a forged store.json identity opened without error")
	}
	if _, _, err := ReadExistingReadyRecordsFound(parent); err == nil {
		t.Fatal("read-only scan accepted a forged store.json identity")
	}
	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Healthy || diagnosis.NeedsReanchor {
		t.Fatalf("single-sided forgery must fail closed without repair: %+v", diagnosis)
	}
	if err := RepairAcquisitionStore(parent); err == nil {
		t.Fatal("repair accepted a forged store.json identity")
	}
}

// TestAcquisitionDiagnoseHealthyForBirthOnlyUpgrade pins the upgrade path:
// when device and inode are equal and only the birth fields are missing
// (records written before birth tracking), the store is the same directory
// and must diagnose healthy without requiring a re-anchor. The daemon
// records birth on its next open; doctor must stay green in between.
func TestAcquisitionDiagnoseHealthyForBirthOnlyUpgrade(t *testing.T) {
	parent := t.TempDir()
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Lstat(filepath.Join(parent, acquisitionStoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	physical, err := fileIdentity(dirInfo)
	if err != nil {
		t.Fatal(err)
	}
	if !physical.HasBirth {
		t.Skip("platform does not record birth time; no birth-only upgrade to pin")
	}

	storePath := filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName)
	anchorPath := filepath.Join(parent, acquisitionStoreAnchorName)
	storeRecord := readStoreIdentityForTest(t, storePath)
	anchorRecord := readStoreIdentityForTest(t, anchorPath)
	// Strip birth but keep device and inode equal: a pre-birth record for
	// the same directory.
	storeRecord.Identity.HasBirth = false
	storeRecord.Identity.BirthSec = 0
	storeRecord.Identity.BirthNsec = 0
	anchorRecord.Identity.HasBirth = false
	anchorRecord.Identity.BirthSec = 0
	anchorRecord.Identity.BirthNsec = 0
	if storeRecord.Identity.Device != physical.Device || anchorRecord.Identity.Device != physical.Device {
		t.Fatal("test setup requires the same device; device drift is a renumber, not an upgrade")
	}
	writeStoreIdentityForTest(t, storePath, storeRecord)
	writeStoreIdentityForTest(t, anchorPath, anchorRecord)

	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnosis.Healthy || diagnosis.NeedsReanchor {
		t.Fatalf("birth-only upgrade must diagnose healthy without re-anchor: %+v", diagnosis)
	}
	if err := RepairAcquisitionStore(parent); err != nil {
		t.Fatalf("healthy birth-only upgrade must need no repair: %v", err)
	}
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatalf("store with a birth-only upgrade refused to open: %v", err)
	}
	after, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Healthy || after.NeedsReanchor {
		t.Fatalf("post-open diagnosis = %+v, want healthy", after)
	}
}
