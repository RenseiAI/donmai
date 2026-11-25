//go:build linux

package tempdoctor

import (
	"os"
	"syscall"
)

const (
	tmpfsMagic = 0x01021994
	// tmpfsDirentSize is the size tmpfs adds to a directory per entry
	// (BOGO_DIRENT_SIZE); a directory starts at two of them.
	tmpfsDirentSize = 20
)

// childCountHint returns a function reporting a directory's direct entry
// count without listing it, when root's filesystem keeps one. tmpfs grows a
// directory's size by a fixed amount per entry.
func childCountHint(root string) func(string, os.FileInfo) (int64, bool) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil || int64(fs.Type) != tmpfsMagic {
		return nil
	}
	return func(_ string, info os.FileInfo) (int64, bool) {
		if !info.IsDir() {
			return 0, false
		}
		n := info.Size()/tmpfsDirentSize - 2
		return n, n >= 0
	}
}
