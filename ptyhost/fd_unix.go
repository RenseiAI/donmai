//go:build unix

package ptyhost

import (
	"errors"
	"fmt"
)

var errFDOutOfRange = errors.New("file descriptor is outside the int range")

// fdToInt converts an os.File.Fd() uintptr to the int required by unix and
// terminal helpers. Rejecting values above MaxInt prevents silent truncation
// while preserving every valid file descriptor on both 32-bit and 64-bit Unix.
func fdToInt(fd uintptr) (int, error) {
	const maxInt = int(^uint(0) >> 1)
	if fd > uintptr(maxInt) {
		return 0, fmt.Errorf("file descriptor %d: %w", fd, errFDOutOfRange)
	}
	return int(fd), nil
}
