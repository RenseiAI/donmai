//go:build darwin || linux

package tempdoctor

import (
	"os"
	"syscall"
)

func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
