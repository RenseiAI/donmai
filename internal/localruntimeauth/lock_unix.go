//go:build darwin || linux

package localruntimeauth

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"syscall"
)

func lockWriter(file *os.File) error {
	fd := file.Fd()
	if fd > uintptr(math.MaxInt) {
		return fmt.Errorf("lock private auth writer: %w", syscall.EBADF)
	}
	if err := syscall.Flock(int(fd), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock private auth writer: %w", err)
	}
	return nil
}

func lockReader(file *os.File) error {
	fd := file.Fd()
	if fd > uintptr(math.MaxInt) {
		return fmt.Errorf("lock private auth reader: %w", syscall.EBADF)
	}
	if err := syscall.Flock(int(fd), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("lock private auth reader: %w", err)
	}
	return nil
}

func unlockWriter(file *os.File) error {
	fd := file.Fd()
	if fd > uintptr(math.MaxInt) {
		return fmt.Errorf("unlock private auth writer: %w", syscall.EBADF)
	}
	if err := syscall.Flock(int(fd), syscall.LOCK_UN); err != nil {
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
