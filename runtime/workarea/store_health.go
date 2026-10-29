package workarea

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// repairSwapHook is a test-only seam invoked after the repair pins and
// exclusively locks the store root, before it re-reads every premise.
// Tests use it to swap the path-visible store directory and prove the
// scan still consumes the pinned root rather than a replacement.
var repairSwapHook func()

// StoreDiagnosis describes the health of the workarea acquisition-store
// identity for one worktree parent.
type StoreDiagnosis struct {
	// Healthy is true when the store opens without repair.
	Healthy bool
	// Present is false when no store metadata exists yet (nothing to repair).
	Present bool
	// StoreID is the durable store identifier when present.
	StoreID string
	// NeedsReanchor is true when the store directory is the same directory
	// (same inode, compatible birth, matching store IDs) on a renumbered
	// device and can be safely re-anchored.
	NeedsReanchor bool
	// Inode pins the expected store-directory inode when NeedsReanchor is
	// true. Repair re-checks it under the exclusive lock so a path swap
	// after diagnosis cannot pass.
	Inode uint64
	// Reason is a human-readable one-line summary.
	Reason string
}

// DiagnoseAcquisitionStore inspects the acquisition-store identity without
// mutating it. It never creates directories or files, never repairs, and
// never runs recovery: it reports whether the store would open, whether it
// only needs a device re-anchor, or whether it is a genuine replacement
// that must fail closed.
func DiagnoseAcquisitionStore(parent string) (StoreDiagnosis, error) {
	abs, err := filepath.Abs(parent)
	if err != nil {
		return StoreDiagnosis{}, fmt.Errorf("runtime/workarea: resolve acquisition parent: %w", err)
	}
	parentRoot, err := os.OpenRoot(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return StoreDiagnosis{Healthy: true, Present: false, Reason: "no worktree parent"}, nil
	}
	if err != nil {
		return StoreDiagnosis{}, fmt.Errorf("runtime/workarea: open acquisition parent: %w", err)
	}
	defer func() { _ = parentRoot.Close() }()
	info, err := parentRoot.Lstat(acquisitionStoreDirName)
	if errors.Is(err, fs.ErrNotExist) {
		return StoreDiagnosis{Healthy: true, Present: false, Reason: "no acquisition store"}, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return StoreDiagnosis{Healthy: false, Present: true, Reason: "acquisition store is not a private real directory"}, nil
	}
	root, err := parentRoot.OpenRoot(acquisitionStoreDirName)
	if err != nil {
		return StoreDiagnosis{}, fmt.Errorf("runtime/workarea: open acquisition store root: %w", err)
	}
	defer func() { _ = root.Close() }()
	openedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return StoreDiagnosis{Healthy: false, Present: true, Reason: "acquisition store identity changed while opening"}, nil
	}
	physical, err := fileIdentity(openedInfo)
	if err != nil {
		return StoreDiagnosis{}, err
	}
	storeRecord, storeExists, err := readAcquisitionStoreIdentity(root, acquisitionStoreIdentityName)
	if err != nil {
		return StoreDiagnosis{Healthy: false, Present: true, Reason: "acquisition store identity file is unsafe or invalid"}, nil
	}
	anchorRecord, anchorExists, err := readAcquisitionStoreIdentity(parentRoot, acquisitionStoreAnchorName)
	if err != nil {
		return StoreDiagnosis{Healthy: false, Present: true, Reason: "acquisition store anchor is unsafe or invalid"}, nil
	}
	if !storeExists && !anchorExists {
		return StoreDiagnosis{Healthy: true, Present: false, Reason: "no acquisition store identity"}, nil
	}
	if !storeExists || !anchorExists {
		return StoreDiagnosis{Healthy: false, Present: true, Reason: "acquisition store identity is incomplete"}, nil
	}
	if storeRecord.StoreID != anchorRecord.StoreID {
		return StoreDiagnosis{
			Healthy: false, Present: true, StoreID: storeRecord.StoreID,
			Reason: "acquisition store and parent anchor disagree",
		}, nil
	}
	storeMatches := fileIdentityMatches(storeRecord.Identity, physical)
	anchorMatches := fileIdentityMatches(anchorRecord.Identity, physical)
	if !storeMatches || !anchorMatches {
		return StoreDiagnosis{
			Healthy: false, Present: true, StoreID: storeRecord.StoreID,
			Reason: "acquisition store replacement differs from durable parent anchor",
		}, nil
	}
	if storeRecord.Identity != physical || anchorRecord.Identity != physical {
		// Records written before birth tracking carry the same device and
		// inode but no birth time. That is the same directory, not a
		// renumber: report a healthy upgradable store so doctor stays
		// green. The daemon pins birth on its next open.
		if storeRecord.Identity.Device == physical.Device && anchorRecord.Identity.Device == physical.Device &&
			storeRecord.Identity.Inode == physical.Inode && anchorRecord.Identity.Inode == physical.Inode {
			return StoreDiagnosis{
				Healthy: true, Present: true, StoreID: storeRecord.StoreID,
				Reason: "acquisition store identity is pinned; birth time upgrade pending",
			}, nil
		}
		return StoreDiagnosis{
			Healthy: false, Present: true, StoreID: storeRecord.StoreID,
			NeedsReanchor: true, Inode: physical.Inode,
			Reason: "acquisition store device renumbered; same directory needs re-anchor",
		}, nil
	}
	return StoreDiagnosis{
		Healthy: true, Present: true, StoreID: storeRecord.StoreID,
		Reason: "acquisition store identity is pinned",
	}, nil
}

// RepairAcquisitionStore re-anchors the acquisition-store identity after a
// device-number renumber. It only acts when DiagnoseAcquisitionStore reports
// NeedsReanchor: the same inode (and compatible birth time) with matching
// store IDs on a different device. The repair opens and pins the private
// real store directory, acquires the existing exclusive store lock, then
// re-reads every premise while holding the lock: both identity documents
// must still be valid with the same schema, store ID, device, and inode
// (allowing a recoverable half-repaired state where one document already
// matches the current device), the current inode must match and only the
// device may have changed, and every existing acquisition record must
// belong to that store. It keeps timestamped backups of both documents
// beside the originals and then pins both to the current device using the
// interruption-safe temp-file plus rename transition. A swap after
// diagnosis or a concurrent writer cannot pass the locked re-check; an
// interruption between the two replicated writes stays recoverable via
// retry. Any other condition fails closed without writing: no missing
// store is created, no acquisition row is changed, and no other mismatch
// is converted.
func RepairAcquisitionStore(parent string) error {
	diagnosis, err := DiagnoseAcquisitionStore(parent)
	if err != nil {
		return err
	}
	if diagnosis.Healthy {
		return nil
	}
	if !diagnosis.NeedsReanchor {
		return fmt.Errorf("runtime/workarea: acquisition store needs manual review: %s", diagnosis.Reason)
	}
	abs, err := filepath.Abs(parent)
	if err != nil {
		return fmt.Errorf("runtime/workarea: resolve acquisition parent: %w", err)
	}
	parentRoot, err := os.OpenRoot(abs)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open acquisition parent: %w", err)
	}
	defer func() { _ = parentRoot.Close() }()
	info, err := parentRoot.Lstat(acquisitionStoreDirName)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("runtime/workarea: acquisition store is not a private real directory")
	}
	root, err := parentRoot.OpenRoot(acquisitionStoreDirName)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open acquisition store root: %w", err)
	}
	defer func() { _ = root.Close() }()
	openedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return fmt.Errorf("runtime/workarea: acquisition store identity changed while opening")
	}
	// Acquire the existing exclusive store lock without creating it. A
	// missing lock file means the store was never fully created or was
	// tampered with: fail closed without writing rather than minting
	// fresh lock state that could launder a replacement. The lock must be
	// the canonical regular file: a symlink (for example to store.json)
	// must not be followed, otherwise locking the identity file would
	// succeed while the original writer lock is still held elsewhere.
	lockInfo, err := root.Lstat(acquisitionStoreLockName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("runtime/workarea: acquisition store lock is missing")
		}
		return fmt.Errorf("runtime/workarea: open acquisition store lock: %w", err)
	}
	if !lockInfo.Mode().IsRegular() || lockInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime/workarea: acquisition store lock is unsafe")
	}
	lock, err := root.OpenFile(acquisitionStoreLockName, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("runtime/workarea: acquisition store lock is missing")
		}
		return fmt.Errorf("runtime/workarea: open acquisition store lock: %w", err)
	}
	openedLockInfo, err := lock.Stat()
	if err != nil || !openedLockInfo.Mode().IsRegular() || !os.SameFile(lockInfo, openedLockInfo) {
		_ = lock.Close()
		return fmt.Errorf("runtime/workarea: acquisition store lock changed while opening")
	}
	if err := flockExclusive(lock, false); err != nil {
		_ = lock.Close()
		return fmt.Errorf("runtime/workarea: lock acquisition store: %w", err)
	}
	defer releaseFlock(lock)
	// Locked re-check: re-read every premise while holding the exclusive
	// lock so a swap after diagnosis or a concurrent writer cannot slip
	// between the check and the rewrite. The test seam below swaps the
	// path-visible directory after the pin+lock to prove the re-check
	// still consumes the pinned root rather than the replacement.
	if repairSwapHook != nil {
		repairSwapHook()
	}
	current, err := parentRoot.Lstat(acquisitionStoreDirName)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, openedInfo) {
		return fmt.Errorf("runtime/workarea: acquisition store directory identity changed")
	}
	if current.Mode().Perm() != 0o700 {
		return fmt.Errorf("runtime/workarea: acquisition store is not a private real directory")
	}
	lockedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(openedInfo, lockedInfo) {
		return fmt.Errorf("runtime/workarea: acquisition store identity changed while locking")
	}
	physical, err := fileIdentity(lockedInfo)
	if err != nil {
		return err
	}
	// The pinned inode must still be the diagnosed inode: a path swap
	// after diagnosis that plants a different directory (different inode)
	// with matching documents cannot pass, even when the forged
	// documents reuse the diagnosed store ID.
	if diagnosis.Inode != 0 && physical.Inode != diagnosis.Inode {
		return fmt.Errorf("runtime/workarea: acquisition store directory identity changed")
	}
	storeRecord, storeExists, err := readAcquisitionStoreIdentity(root, acquisitionStoreIdentityName)
	if err != nil || !storeExists {
		return fmt.Errorf("runtime/workarea: acquisition store identity changed before repair")
	}
	anchorRecord, anchorExists, err := readAcquisitionStoreIdentity(parentRoot, acquisitionStoreAnchorName)
	if err != nil || !anchorExists {
		return fmt.Errorf("runtime/workarea: acquisition store anchor changed before repair")
	}
	if storeRecord.StoreID != anchorRecord.StoreID || storeRecord.StoreID != diagnosis.StoreID {
		return fmt.Errorf("runtime/workarea: acquisition store changed before repair")
	}
	if !fileIdentityMatches(storeRecord.Identity, physical) || !fileIdentityMatches(anchorRecord.Identity, physical) {
		return fmt.Errorf("runtime/workarea: acquisition store replacement differs from durable parent anchor")
	}
	// Permit only the device-only drift: the current inode must match and
	// only the device may have changed. Both documents must agree on the
	// inode; when both still carry the old device they must agree on it
	// too, otherwise one side is forged or swapped. A half-repaired state
	// (one document already pinned to the current device after an
	// interruption between the two replicated writes) is allowed so the
	// retry can finish the second write.
	if storeRecord.Identity.Inode == 0 || anchorRecord.Identity.Inode == 0 || physical.Inode == 0 {
		return fmt.Errorf("runtime/workarea: acquisition store replacement differs from durable parent anchor")
	}
	if storeRecord.Identity.Inode != physical.Inode || anchorRecord.Identity.Inode != physical.Inode {
		return fmt.Errorf("runtime/workarea: acquisition store replacement differs from durable parent anchor")
	}
	if storeRecord.Identity.Inode != anchorRecord.Identity.Inode {
		return fmt.Errorf("runtime/workarea: acquisition store replacement differs from durable parent anchor")
	}
	storePinned := storeRecord.Identity.Device == physical.Device
	anchorPinned := anchorRecord.Identity.Device == physical.Device
	if storePinned && anchorPinned {
		// A concurrent repair already finished while this caller waited
		// for the exclusive lock: idempotent success without writing.
		return nil
	}
	if !storePinned && !anchorPinned && storeRecord.Identity.Device != anchorRecord.Identity.Device {
		return fmt.Errorf("runtime/workarea: acquisition store replacement differs from durable parent anchor")
	}
	// Existing acquisition rows must already belong to this store before
	// the stable identity is rewritten. A malformed row or a row from
	// another store refuses the repair without writing, so a forged
	// store directory cannot be laundered by re-anchoring its identity.
	if err := validateAcquisitionRecordsBelongToStore(root, storeRecord.StoreID); err != nil {
		return err
	}
	// All premises re-verified under lock: back up, then durably replace
	// both documents with the interruption-safe temp-file plus rename
	// transition. No store is created, no row is changed, no root is
	// removed.
	stamp := time.Now().UTC().Format("20060102T150405.999999999Z")
	if err := backupAcquisitionIdentityFile(root, acquisitionStoreIdentityName, acquisitionStoreIdentityName+".backup-"+stamp); err != nil {
		return err
	}
	if err := backupAcquisitionIdentityFile(parentRoot, acquisitionStoreAnchorName, acquisitionStoreAnchorName+".backup-"+stamp); err != nil {
		return err
	}
	repaired := acquisitionStoreIdentity{SchemaVersion: acquisitionStoreSchemaV1, StoreID: storeRecord.StoreID, Identity: physical}
	// Reuse the store's atomic temp+rename writer via a short-lived handle.
	// The root descriptors above already pin the same directory while the
	// exclusive lock is held, so the rewrite cannot launder a path-swapped
	// replacement.
	store := &AcquisitionStore{parent: abs, dir: filepath.Join(abs, acquisitionStoreDirName), parentRoot: parentRoot, root: root, identity: lockedInfo}
	if err := store.writeStoreIdentity(repaired); err != nil {
		return err
	}
	if err := overwriteAcquisitionStoreAnchor(parentRoot, repaired); err != nil {
		return err
	}
	return nil
}

// validateAcquisitionRecordsBelongToStore scans the pinned store root for
// durable acquisition rows and proves each one belongs to the expected
// store. It covers both published rows (wac_*) and unpublished claiming
// rows (.claim-wac_*) written durably by Begin and handled by recovery:
// a foreign-store claim must refuse just like a foreign published row,
// otherwise re-anchoring would launder it and the next writable open
// would delete the foreign claim and its payload through recovery. It
// reads through the already-pinned root only, refuses non-regular record
// files (FIFOs, sockets, devices, symlinks) without blocking, validates
// the closed record independently, and requires the store ID and the
// directory identity to match. Any malformed row or foreign-store row
// refuses the caller without writing. An empty claim directory with no
// record file is skipped: recovery removes such abandoned claims and
// there is no store identity to check.
func validateAcquisitionRecordsBelongToStore(root *os.Root, storeID string) error {
	if storeID == "" || !strings.HasPrefix(storeID, "wstore_") {
		return fmt.Errorf("runtime/workarea: acquisition store identity is invalid")
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	if closeErr := directory.Close(); readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		return readErr
	}
	for _, entry := range entries {
		name := entry.Name()
		isPublished := entry.IsDir() && strings.HasPrefix(name, "wac_")
		isClaim := entry.IsDir() && strings.HasPrefix(name, ".claim-wac_")
		if !isPublished && !isClaim {
			continue
		}
		expectedID := name
		if isClaim {
			expectedID = strings.TrimPrefix(name, ".claim-")
		}
		dirInfo, err := root.Lstat(name)
		if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
		}
		acquisitionRoot, err := root.OpenRoot(name)
		if err != nil {
			return fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
		}
		recordInfo, err := acquisitionRoot.Lstat(acquisitionRecordFileName)
		if err != nil {
			_ = acquisitionRoot.Close()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
		}
		if !recordInfo.Mode().IsRegular() || recordInfo.Mode()&os.ModeSymlink != 0 {
			_ = acquisitionRoot.Close()
			return fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
		}
		body, err := acquisitionRoot.ReadFile(acquisitionRecordFileName)
		_ = acquisitionRoot.Close()
		if err != nil {
			return fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
		}
		var record AcquisitionRecord
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			return fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
		}
		if err := record.Validate(); err != nil {
			return fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
		}
		if record.AcquisitionID != expectedID || record.StoreID != storeID {
			return fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
		}
	}
	return nil
}

func backupAcquisitionIdentityFile(root *os.Root, name, backupName string) error {
	body, err := root.ReadFile(name)
	if err != nil {
		return err
	}
	file, err := root.OpenFile(backupName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncRoot(root)
}
