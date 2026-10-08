//go:build linux

package daemon

import (
	"golang.org/x/sys/unix"
)

// disableShimEchoTerminalEcho clears the ECHO flag on the echo harness's own
// controlling terminal (stdin is the PTY slave): the in-process equivalent of
// the `stty -echo` the shell fixture ran before its read loop.
func disableShimEchoTerminalEcho() {
	termios, err := unix.IoctlGetTermios(0, unix.TCGETS)
	if err != nil {
		return
	}
	termios.Lflag &^= unix.ECHO
	_ = unix.IoctlSetTermios(0, unix.TCSETS, termios)
}
