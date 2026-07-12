//go:build unix

package ptyhost

import "golang.org/x/sys/unix"

// applyWinsize sets the PTY window size via TIOCSWINSZ (§8). It is invoked from
// within a SyscallConn Control callback so the fd is race-safe against a
// concurrent Close and the read loop's poller is never disturbed (unlike
// os.File.Fd, which pty.Setsize uses internally).
func applyWinsize(fd uintptr, cols, rows, pxW, pxH uint16) error {
	return unix.IoctlSetWinsize(fdToInt(fd), unix.TIOCSWINSZ, &unix.Winsize{
		Row:    rows,
		Col:    cols,
		Xpixel: pxW,
		Ypixel: pxH,
	})
}
