//go:build darwin

package ptyhost

import (
	"github.com/RenseiAI/donmai/attachwire"
	"golang.org/x/sys/unix"
)

// echoModeOfFd reads the termios ECHO local flag of the PTY (§10, §12.1
// echoMode). On Darwin/BSD the master and slave of a pty pair share one termios,
// so tcgetattr (TIOCGETA) on the master fd reflects the child's cooked/raw
// setting. Returns attachwire.EchoUnknown (0xFF) if the ioctl fails, biasing
// predictive echo to SUPPRESSED (§10).
func echoModeOfFd(fd uintptr) uint8 {
	intFD, err := fdToInt(fd)
	if err != nil {
		return attachwire.EchoUnknown
	}
	t, err := unix.IoctlGetTermios(intFD, unix.TIOCGETA)
	if err != nil {
		return attachwire.EchoUnknown
	}
	if t.Lflag&unix.ECHO != 0 {
		return attachwire.EchoOn
	}
	return attachwire.EchoOff
}
