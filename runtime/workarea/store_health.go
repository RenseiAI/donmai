package workarea

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

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
			NeedsReanchor: true,
			Reason:        "acquisition store device renumbered; same directory needs re-anchor",
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
// store IDs on a different device. It keeps timestamped backups of both
// documents beside the originals and then pins both to the current device.
// Any other condition fails closed without writing.
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
	root, err := parentRoot.OpenRoot(acquisitionStoreDirName)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open acquisition store root: %w", err)
	}
	defer func() { _ = root.Close() }()
	openedInfo, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("runtime/workarea: stat acquisition store: %w", err)
	}
	physical, err := fileIdentity(openedInfo)
	if err != nil {
		return err
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
	stamp := time.Now().UTC().Format("20060102T150405Z")
	if err := backupAcquisitionIdentityFile(root, acquisitionStoreIdentityName, acquisitionStoreIdentityName+".backup-"+stamp); err != nil {
		return err
	}
	if err := backupAcquisitionIdentityFile(parentRoot, acquisitionStoreAnchorName, acquisitionStoreAnchorName+".backup-"+stamp); err != nil {
		return err
	}
	repaired := acquisitionStoreIdentity{SchemaVersion: acquisitionStoreSchemaV1, StoreID: storeRecord.StoreID, Identity: physical}
	// Reuse the store's atomic temp+rename writer via a short-lived handle.
	// The root descriptors above already pin the same directory, so the
	// rewrite cannot launder a path-swapped replacement.
	store := &AcquisitionStore{parent: abs, dir: filepath.Join(abs, acquisitionStoreDirName), parentRoot: parentRoot, root: root, identity: openedInfo}
	if err := store.writeStoreIdentity(repaired); err != nil {
		return err
	}
	if err := overwriteAcquisitionStoreAnchor(parentRoot, repaired); err != nil {
		return err
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
