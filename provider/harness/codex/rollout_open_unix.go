//go:build unix

package codex

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openRolloutFile opens a native session file for reading, refusing anything
// that is not a regular file: the read runs inside ptycli run() before
// cleanup, and opening a FIFO for reading would block for a writer — leaving
// the terminal unsent and the session's resources unreleased. The open
// itself is non-blocking (opening a FIFO otherwise waits); the flag does not
// change how a regular file reads. The fstat after the open closes the
// walk-to-open race: only the file the walk saw is read.
func openRolloutFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0) //nolint:gosec // path is the session's own rollout file under its CODEX_HOME; non-regular files are refused below
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("codex rollout: %s is not a regular file", path)
	}
	return f, nil
}
