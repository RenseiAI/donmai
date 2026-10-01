//go:build darwin || linux

package localruntimeauth

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func lockWriter(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock private auth writer: %w", err)
	}
	return nil
}

func lockReader(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("lock private auth reader: %w", err)
	}
	return nil
}

func unlockWriter(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock private auth writer: %w", err)
	}
	return nil
}

func fileLinkCount(info fs.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}
