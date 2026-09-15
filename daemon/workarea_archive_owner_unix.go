//go:build darwin || linux

package daemon

import (
	"os"
	"syscall"
)

func archiveRootOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
