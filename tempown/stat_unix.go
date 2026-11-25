//go:build darwin || linux

package tempown

import (
	"os"
	"syscall"
	"time"
)

// changedAt is the later of an entry's modification and status-change times.
// The status-change time cannot be set back from user space, so it is a
// reliable upper bound on when the entry was created.
func changedAt(info os.FileInfo) time.Time {
	mod := info.ModTime()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return mod
	}
	ctime := statusChangeTime(st)
	if ctime.After(mod) {
		return ctime
	}
	return mod
}

// ownedExclusively reports whether info belongs to this process's user and
// grants no write access to group or other, so nothing but this user could
// have placed or changed what is inside.
func ownedExclusively(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if int(st.Uid) != os.Getuid() {
		return false
	}
	return info.Mode().Perm()&0o022 == 0
}
