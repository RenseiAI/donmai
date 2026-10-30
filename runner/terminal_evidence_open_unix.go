//go:build unix

package runner

import (
	"os"
	"syscall"
)

// openTerminalEvidenceFile opens a declared evidence file without following
// a trailing symlink and without blocking on a FIFO. O_NOFOLLOW makes a
// swapped-in symlink fail with ELOOP instead of archiving its target;
// O_NONBLOCK makes a swapped-in FIFO open without blocking so the
// regular-file check in preserveTerminalEvidence can reject it.
func openTerminalEvidenceFile(path string) (*os.File, error) {
	//nolint:gosec // G304: path comes from the session-owned descriptor captured after Stop.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
