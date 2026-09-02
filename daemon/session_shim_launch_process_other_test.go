//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package daemon

// Provenance: shim-discovery-deadline-2026-09-02.

import "testing"

// assertShimChildReaped has no meaning on a platform with no waitpid: those
// platforms also have no per-session shim (sessionshim.Start refuses before a
// shim is ever launched — see configureShimProcess), so there is no launched
// child whose reaping could be asserted.
func assertShimChildReaped(t *testing.T, _ int) {
	t.Helper()
	t.Skip("no waitpid on this operating system; the shim launch path is unsupported here")
}
