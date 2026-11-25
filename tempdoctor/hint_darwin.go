//go:build darwin

package tempdoctor

import (
	"encoding/binary"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// attrDirEntryCount is ATTR_DIR_ENTRYCOUNT from <sys/attr.h>: the number of
// entries in a directory, as a u_int32_t.
const attrDirEntryCount = 0x00000002

// childCountHint returns a function reporting a directory's direct entry
// count without listing it. macOS keeps the count with the directory and
// returns it through getattrlist(2); stat's link count is not usable for this
// because it is 16 bits wide and saturates at 65535, exactly on the
// directories a report most needs to count.
func childCountHint(string) func(string, os.FileInfo) (int64, bool) {
	return func(path string, info os.FileInfo) (int64, bool) {
		if !info.IsDir() {
			return 0, false
		}
		return dirEntryCount(path)
	}
}

// dirEntryCount reads ATTR_DIR_ENTRYCOUNT for path without following a
// final symlink.
//
//nolint:gosec // G103: the pointers are the syscall's own arguments, converted in the call, and the buffer is the caller's.
func dirEntryCount(path string) (int64, bool) {
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return 0, false
	}
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Dirattr: attrDirEntryCount}
	// The reply is a u_int32_t length followed by the requested attribute.
	var buf [8]byte
	_, _, errno := syscall.Syscall6(syscall.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(pathPtr)), uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unix.FSOPT_NOFOLLOW), 0)
	if errno != 0 {
		return 0, false
	}
	if binary.NativeEndian.Uint32(buf[0:4]) < uint32(len(buf)) {
		return 0, false
	}
	return int64(binary.NativeEndian.Uint32(buf[4:8])), true
}
