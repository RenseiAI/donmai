//go:build !unix

package runner

import (
	"os"
)

// openTerminalEvidenceFile opens a declared evidence file. Platforms
// without O_NOFOLLOW/O_NONBLOCK have no FIFOs that block open and no
// privileged symlink swap surface; the Lstat plus post-open regular-file
// checks in preserveTerminalEvidence remain the guard there.
func openTerminalEvidenceFile(path string) (*os.File, error) {
	//nolint:gosec // G304: path comes from the session-owned descriptor captured after Stop.
	return os.Open(path)
}
