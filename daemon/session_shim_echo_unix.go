//go:build linux

package daemon

import (
	"golang.org/x/sys/unix"
)

// disableShimEchoTerminalEcho clears the ECHO flag on the echo harness's own
// controlling terminal (stdin is the PTY slave), so the carrier observes each
// input line once — on the `ack:` answer — instead of twice. The old shell
// fixture never disabled echo in this interactive shape (only the
// consumed-recovery fixture runs stty -echo), so this is a fidelity
// difference from it, not an equivalent: the tests match on the `ack:`
// answer, which only this harness produces, so they hold either way.
func disableShimEchoTerminalEcho() {
	termios, err := unix.IoctlGetTermios(0, unix.TCGETS)
	if err != nil {
		return
	}
	termios.Lflag &^= unix.ECHO
	_ = unix.IoctlSetTermios(0, unix.TCSETS, termios)
}
