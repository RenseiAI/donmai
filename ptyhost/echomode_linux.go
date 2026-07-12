//go:build linux

package ptyhost

import (
	"github.com/RenseiAI/donmai/attachwire"
	"golang.org/x/sys/unix"
)

// echoModeOfFd reads the termios ECHO local flag of the PTY (§10, §12.1
// echoMode). On Linux the reliable source is the slave's termios; a tcgetattr
// (TCGETS) on the master fd may return EINVAL. When it does, echoMode reports
// EchoUnknown (0xFF), which biases predictive echo to SUPPRESSED (§10) — the
// safe default. Callers that hold the slave fd should read that instead.
func echoModeOfFd(fd uintptr) uint8 {
	t, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	if err != nil {
		return attachwire.EchoUnknown
	}
	if t.Lflag&unix.ECHO != 0 {
		return attachwire.EchoOn
	}
	return attachwire.EchoOff
}
