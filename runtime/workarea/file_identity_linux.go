//go:build linux

package workarea

import (
	"fmt"
	"math"
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (FileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, fmt.Errorf("runtime/workarea: filesystem identity unavailable")
	}
	return FileIdentity{Device: stat.Dev, Inode: stat.Ino}, nil
}

func allocatedFileBytes(info os.FileInfo) (int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("runtime/workarea: allocated block count unavailable")
	}
	if stat.Blocks < 0 {
		return 0, fmt.Errorf("runtime/workarea: negative allocated block count")
	}
	if uint64(stat.Blocks) > uint64(math.MaxInt64/512) {
		return 0, fmt.Errorf("runtime/workarea: allocated block count overflows bytes")
	}
	return int64(stat.Blocks) * 512, nil
}
