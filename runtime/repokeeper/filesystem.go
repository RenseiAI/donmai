package repokeeper

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// mirrorsRelPath joins one leaf under the mirrors directory, relative to
// the store root. The leaf is either a validated 64-hex digest (mirror
// directories) or one of the store's own fixed staging patterns — never
// caller input — and os.Root confines the result to the state dir
// regardless.
func mirrorsRelPath(leaf string) string {
	return filepath.Join(keeperDirName, mirrorsDirName, leaf)
}

// locksDirName is the leaf under the keeper root holding one flock file
// per mirror digest. Locks live beside mirrors/, not in it, so directory
// listings of mirrors/ see only mirrors and the store's own staging
// patterns.
const locksDirName = "locks"

// locksRelPath joins one lock file name under the locks directory,
// relative to the store root. Names come from lockFileNameFor (validated
// digest plus suffix), never caller input.
func locksRelPath(name string) string {
	return filepath.Join(keeperDirName, locksDirName, name)
}

// checkSameFilesystem proves stateDir and worktreeRoot share a filesystem
// before the store publishes any mirror: later consumers clone worktrees
// from these mirrors, and a cross-filesystem layout would silently change
// copy cost and sharing behavior. Missing parents are created owner-only
// so the check can run on a fresh state home; an existing path that is a
// symlink or not a directory fails.
func checkSameFilesystem(stateDir, worktreeRoot string) error {
	stateDir, err := ensureRealDir(stateDir)
	if err != nil {
		return err
	}
	worktreeRoot, err = ensureRealDir(worktreeRoot)
	if err != nil {
		return err
	}
	stateDev, err := deviceOf(stateDir)
	if err != nil {
		return err
	}
	workDev, err := deviceOf(worktreeRoot)
	if err != nil {
		return err
	}
	if stateDev != workDev {
		return fmt.Errorf("%w: state and worktree roots do not share a filesystem", ErrSameFilesystem)
	}
	return nil
}

// ensureRealDir creates path owner-only when absent and returns it. A path
// that exists as a symlink, or as anything but a directory, is refused:
// following a symlinked root would plant every mirror outside the state
// dir the caller believes it owns. A symlinked parent above path is
// followed by MkdirAll itself — confining those needs os.Root, which the
// store holds for everything below the roots — but the roots themselves
// are pinned here.
func ensureRealDir(path string) (string, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("repo keeper: refusing symlinked root %q", path)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("repo keeper: root %q exists and is not a directory", path)
		}
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("repo keeper: stat root: %w", err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("repo keeper: create root: %w", err)
	}
	// MkdirAll follows a pre-existing symlinked parent to create the leaf
	// elsewhere; re-check that what now exists at path is a real
	// directory, not a symlink planted between the two calls.
	if info, err := os.Lstat(path); err != nil {
		return "", fmt.Errorf("repo keeper: stat created root: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("repo keeper: refusing symlinked root %q", path)
	} else if !info.IsDir() {
		return "", fmt.Errorf("repo keeper: root %q exists and is not a directory", path)
	}
	return path, nil
}

// dirBytes sums the apparent sizes of every regular file under dir. It is
// best-effort catalog metadata: a late error still reports the bytes
// counted so far.
func dirBytes(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error { //nolint:gosec // G122: metadata size summation only; no reads or writes.

		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// dirModTime reports dir's own modification time as the catalog's fetch
// approximation for a freshly published mirror.
func dirModTime(dir string) time.Time {
	if info, err := os.Stat(dir); err == nil {
		return info.ModTime()
	}
	return time.Now()
}
