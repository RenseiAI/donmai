//go:build !unix

package codex

import (
	"fmt"
	"os"
)

// openRolloutFile opens a native session file for reading, refusing anything
// that is not a regular file. The non-blocking open guard lives in the unix
// build (rollout_open_unix.go); this fallback keeps the same refusal without
// the flag the platform lacks.
func openRolloutFile(path string) (*os.File, error) {
	f, err := os.Open(path) //nolint:gosec // path is the session's own rollout file under its CODEX_HOME; non-regular files are refused below
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
