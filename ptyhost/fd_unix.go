//go:build unix

package ptyhost

import "math"

// fdToInt converts an os.File.Fd() uintptr to the int the unix ioctl
// wrappers take. Fd() on a live *os.File is always a small non-negative
// descriptor, but the uintptr->int conversion is flagged by gosec (G115),
// so bound it explicitly: an out-of-range value collapses to -1, which every
// ioctl rejects with EBADF instead of silently truncating.
func fdToInt(fd uintptr) int {
	if fd > math.MaxInt32 {
		return -1
	}
	return int(fd)
}
