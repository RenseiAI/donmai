//go:build darwin && amd64

package daemon

import (
	"syscall"
	"unsafe"
)

// This file hand-wraps the two libc entry points the scheduling self-check
// needs, in exactly the shape x/sys's generated zsyscall files use: a
// uintptr trampoline resolved by the linker via cgo_import_dynamic, called
// through the runtime's syscall_syscall ABI bridge (linked by name from the
// syscall package — the same go:linkname x/sys itself relies on, see
// golang.org/x/sys/unix/syscall_darwin_libSystem.go). x/sys does not wrap
// qos_class_self or getiopolicy_np, and the release build runs
// CGO_ENABLED=0, so this is the only cgo-free path to them.

//go:linkname syscall_syscall syscall.syscall

//go:noescape
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

var libc_qos_class_self_trampoline_addr uintptr

//go:cgo_import_dynamic libc_qos_class_self qos_class_self "/usr/lib/libSystem.B.dylib"

// qosClassSelfABI calls qos_class_self(): no arguments, the qos_class_t
// arrives as the return value.
func qosClassSelfABI() uintptr {
	r1, _, _ := syscall_syscall(libc_qos_class_self_trampoline_addr, 0, 0, 0)
	return r1
}

var libc_getiopolicy_np_trampoline_addr uintptr

//go:cgo_import_dynamic libc_getiopolicy_np getiopolicy_np "/usr/lib/libSystem.B.dylib"

// getiopolicy_np calls getiopolicy_np(iotype, scope): the IO policy arrives
// as the return value, -1 with errno set on failure.
func getiopolicy_np(iotype, scope int) (int, error) {
	r1, _, e1 := syscall_syscall(
		libc_getiopolicy_np_trampoline_addr,
		uintptr(iotype), uintptr(scope), 0,
	)
	_ = unsafe.Pointer(nil)
	if e1 != 0 {
		return -1, e1
	}
	return int(r1), nil
}
