//go:build unix

package daemon

import (
	"os"
	"syscall"
)

// sessionTmpOwnerUID reads the owning uid out of an Lstat observation. Unix
// only: other platforms cannot prove ownership from a stat, so reuse of an
// existing directory is refused there (see session_tmpdir_owner_other.go).
func sessionTmpOwnerUID(fi os.FileInfo) (int, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errSessionTmpOwnershipUnavailable
	}
	return int(st.Uid), nil //nolint:gosec // G115: uids are small non-negative values on every unix this builds for.
}
