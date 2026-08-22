//go:build darwin

package sessionshim

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the effective uid of the process on the other end of conn.
//
// LOCAL_PEERCRED is answered by the KERNEL from the connecting process's
// credentials at connect time. That is what makes it usable as authentication:
// unlike anything the peer sends us, it cannot be forged by the peer.
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
		xu, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			credEr = err
			return
		}
		uid = int(xu.Uid)
	}); cerr != nil {
		return -1, fmt.Errorf("sessionshim: peer cred control: %w", cerr)
	}
	if credEr != nil {
		return -1, fmt.Errorf("sessionshim: peer cred: %w", credEr)
	}
	return uid, nil
}

// peerCredSupported reports whether this platform has a trustworthy peer-
// credential primitive. §D3: a platform without one keeps adoption DISABLED.
func peerCredSupported() bool { return true }
