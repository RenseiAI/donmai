//go:build darwin || linux

package repokeeper

import (
	"fmt"
	"os"
	"syscall"
)

// deviceOf reports the filesystem device holding path. The path must
// already exist: callers create owner-only parents first so a fresh state
// home still yields a device to compare.
func deviceOf(path string) (uint64, error) {
	info, err := os.Stat(path) //nolint:gosec // G304: the store's own state/worktree roots.
	if err != nil {
		return 0, fmt.Errorf("repo keeper: stat filesystem root: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("repo keeper: filesystem identity unavailable")
	}
	// The kernel never reports a negative device number. Dev is an unsigned
	// width on 64-bit Linux, where a `< 0` guard is dead code that trips
	// SA4003, and a small signed integer on darwin, so convert directly.
	return uint64(stat.Dev), nil //nolint:gosec // G115: device numbers are non-negative on both platforms.
}
