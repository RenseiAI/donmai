//go:build darwin

package confinement

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// xattrShowCompression is getxattr's option that returns the attributes a
// transparently compressed file keeps its contents in
// (com.apple.ResourceFork and com.apple.decmpfs), which are hidden
// otherwise.
const xattrShowCompression = unix.XATTR_SHOWCOMPRESSION

// getxattr reads one extended attribute with getxattr's options, which the
// library wrapper does not expose. A nil dest asks for the size only.
//
//nolint:gosec // G103: the pointers are the syscall's own arguments, converted in the call, and the buffer is the caller's.
func getxattr(path, name string, dest []byte, options int) (int, error) {
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return -1, err
	}
	namePtr, err := unix.BytePtrFromString(name)
	if err != nil {
		return -1, err
	}
	var destPtr unsafe.Pointer
	if len(dest) > 0 {
		destPtr = unsafe.Pointer(&dest[0])
	}
	// The library wrapper fixes the options at zero and nothing else exposes
	// them, so the call is made directly; the probe is the only caller.
	n, _, errno := syscall.Syscall6(syscall.SYS_GETXATTR,
		uintptr(unsafe.Pointer(pathPtr)), uintptr(unsafe.Pointer(namePtr)),
		uintptr(destPtr), uintptr(len(dest)), 0, uintptr(options))
	if errno != 0 {
		return -1, errno
	}
	return int(n), nil
}
