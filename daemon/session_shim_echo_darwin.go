//go:build darwin

package daemon

import (
	"golang.org/x/sys/unix"
)

// disableShimEchoTerminalEcho clears the ECHO flag on the echo harness's own
// controlling terminal (stdin is the PTY slave): the in-process equivalent of
// the `stty -echo` the shell fixture ran before its read loop.
func disableShimEchoTerminalEcho() {
	termios, err := unix.IoctlGetTermios(0, unix.TIOCGETA)
	if err != nil {
		return
	}
	termios.Lflag &^= unix.ECHO
	_ = unix.IoctlSetTermios(0, unix.TIOCSETA, termios)
}
