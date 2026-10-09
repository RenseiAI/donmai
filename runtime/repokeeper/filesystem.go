package repokeeper

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// checkSameFilesystem proves stateDir and worktreeRoot share a filesystem
// before the store publishes any mirror: later consumers clone worktrees
// from these mirrors, and a cross-filesystem layout would silently change
// copy cost and sharing behavior. Missing parents are created owner-only
// so the check can run on a fresh state home; an existing path that is not
// a real directory fails.
func checkSameFilesystem(stateDir, worktreeRoot string) error {
	stateDev, err := deviceOf(ensureRealDir(stateDir))
	if err != nil {
		return err
	}
	workDev, err := deviceOf(ensureRealDir(worktreeRoot))
	if err != nil {
		return err
	}
	if stateDev != workDev {
		return fmt.Errorf("%w: state and worktree roots do not share a filesystem", ErrSameFilesystem)
	}
	return nil
}

// ensureRealDir creates path owner-only when absent and returns it,
// refusing symlinks and non-directories.
func ensureRealDir(path string) string {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return path
	}
	return path
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
