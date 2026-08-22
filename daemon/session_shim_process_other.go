//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package daemon

import "os/exec"

// configureShimProcess is a no-op on platforms without POSIX sessions. Those
// platforms also have no trustworthy peer-credential primitive, so
// sessionshim.Start refuses with ErrShimUnsupported before a shim is ever
// launched — §D3's "keep adoption disabled rather than run unauthenticated".
func configureShimProcess(*exec.Cmd) {}
