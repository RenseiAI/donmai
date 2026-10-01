//go:build darwin || linux

package localqueue

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockWriter(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrWriterLocked
		}
		return fmt.Errorf("localqueue: lock writer: %w", err)
	}
	return nil
}

func unlockWriter(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("localqueue: unlock writer: %w", err)
	}
	return nil
}
