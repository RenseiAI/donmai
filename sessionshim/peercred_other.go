//go:build !darwin && !linux

package sessionshim

import (
	"errors"
	"net"
)

// peerUID has no trustworthy implementation outside darwin and linux.
//
// It returns an error rather than a permissive default because §D3 makes this
// exact call: a platform without a trustworthy peer-credential primitive keeps
// adoption disabled. Guessing "probably us" would authenticate an unauthenticated
// peer.
func peerUID(*net.UnixConn) (int, error) {
	return -1, errors.New("sessionshim: peer credentials are unavailable on this platform")
}

// peerCredSupported reports whether this platform has a trustworthy peer-
// credential primitive.
func peerCredSupported() bool { return false }
