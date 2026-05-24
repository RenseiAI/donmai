// Package statepath provides helpers for resolving OSS state directory paths
// with one-release backward-compat migration from ~/.rensei/ to ~/.donmai/.
//
// Migration policy:
//   - Canonical path is ~/.donmai/<file>
//   - If ~/.donmai/ does not exist but ~/.rensei/ does, the legacy path
//     is returned with a one-time stderr warning.
//   - Once ~/.donmai/ exists, the legacy path is ignored.
//   - New writes always go to ~/.donmai/.
//   - ~/Library/Logs/rensei/ is shared with the closed-source rensei binary
//     and is NOT migrated here.
package statepath

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// warnOnce guards the one-time migration warning per process.
var warnOnce sync.Once

// warnLegacy emits the migration warning once.
func warnLegacy(legacy, canonical string) {
	warnOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"warning: using legacy state directory %s; migrate to %s by running `donmai daemon install`\n",
			filepath.Dir(legacy), filepath.Dir(canonical),
		)
	})
}

// Resolve returns the path to use for a state file under the ~/.donmai/
// directory, with a one-release fallback to ~/.rensei/ if the new directory
// does not yet exist.
//
// fallbackSuffix is the path suffix under the home dir, e.g. "daemon.yaml".
// The function constructs both ~/.donmai/<suffix> and ~/.rensei/<suffix>,
// checks which base directory exists, and returns the appropriate path.
//
// tmpFallback is used when os.UserHomeDir() fails (e.g. in some test envs).
func Resolve(suffix, tmpFallback string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return tmpFallback
	}

	canonical := filepath.Join(home, ".donmai", suffix)
	legacy := filepath.Join(home, ".rensei", suffix)

	// If canonical dir already exists, always use it.
	if _, err := os.Stat(filepath.Dir(canonical)); err == nil {
		return canonical
	}

	// If legacy dir exists (and canonical doesn't), fall back with warning.
	if _, err := os.Stat(filepath.Dir(legacy)); err == nil {
		warnLegacy(legacy, canonical)
		return legacy
	}

	// Neither exists yet — return canonical (new installs default to donmai).
	return canonical
}
