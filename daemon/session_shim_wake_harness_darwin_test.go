//go:build darwin

package daemon

import (
	"golang.org/x/sys/unix"
)

// configureWakeHarnessTerminal puts the wake harness's own terminal
// (stdin is the PTY slave) in the fixture mode names: `-echo -isig` in
// every mode so the carrier observes only the harness's own `ack:`
// answers, `-icanon` additionally cleared in raw mode so every byte
// arrives as data with nothing interpreted on the way in. The kernel
// still owns the line discipline itself — canonical kill handling, raw
// delivery — so the fixture keeps proving what the remediation code
// controls: the exact bytes delivered, and that a kernel-held draft is
// killed rather than submitted.
func configureWakeHarnessTerminal(mode wakeFixtureMode) {
	termios, err := unix.IoctlGetTermios(0, unix.TIOCGETA)
	if err != nil {
		return
	}
	termios.Lflag &^= unix.ECHO | unix.ISIG
	if mode == wakeFixtureRaw {
		termios.Lflag &^= unix.ICANON
		termios.Cc[unix.VMIN] = 1
		termios.Cc[unix.VTIME] = 0
	}
	_ = unix.IoctlSetTermios(0, unix.TIOCSETA, termios)
}
