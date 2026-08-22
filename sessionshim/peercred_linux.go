//go:build linux

package sessionshim

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the effective uid of the process on the other end of conn.
// SO_PEERCRED is filled in by the kernel at connect time and cannot be forged
// by the peer.
func peerUID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, fmt.Errorf("sessionshim: peer cred conn: %w", err)
	}
	var (
		uid    int
		credEr error
	)
	if cerr := raw.Control(func(fd uintptr) {
		if fd > uintptr(^uint(0)>>1) {
			credEr = fmt.Errorf("socket descriptor %d exceeds int range", fd)
			return
		}
		socketFD := int(fd) //nolint:gosec // G115: bounded by the int-range check above.
		ucred, err := unix.GetsockoptUcred(socketFD, unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			credEr = err
			return
		}
		uid = int(ucred.Uid)
	}); cerr != nil {
		return -1, fmt.Errorf("sessionshim: peer cred control: %w", cerr)
	}
	if credEr != nil {
		return -1, fmt.Errorf("sessionshim: peer cred: %w", credEr)
	}
	return uid, nil
}

// peerCredSupported reports whether this platform has a trustworthy peer-
// credential primitive.
func peerCredSupported() bool { return true }
