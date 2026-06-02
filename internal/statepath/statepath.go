// Package statepath provides helpers for resolving OSS state directory paths
// under the ~/.donmai/ directory.
//
// Policy:
//   - Canonical path is ~/.donmai/<file>
//   - New writes always go to ~/.donmai/.
//   - ~/Library/Logs/rensei/ is shared with the closed-source rensei binary
//     and is NOT resolved here.
package statepath

import (
	"os"
	"path/filepath"
)

// Resolve returns the path to use for a state file under the ~/.donmai/
// directory.
//
// suffix is the path suffix under the state dir, e.g. "daemon.yaml".
//
// tmpFallback is used when os.UserHomeDir() fails (e.g. in some test envs).
func Resolve(suffix, tmpFallback string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return tmpFallback
	}
	return filepath.Join(home, ".donmai", suffix)
}
