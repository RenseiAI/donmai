// Package statepath provides helpers for resolving OSS state directory paths
// under the brand state directory (~/.donmai/ by default).
//
// Policy:
//   - Canonical path is ~/.<brand>/<file>, where <brand> defaults to "donmai".
//   - The brand is configured once at process init via the
//     github.com/RenseiAI/donmai/runtime/statehome seam; library code never
//     reads the environment to discover it.
//   - New writes always go to the brand state dir.
package statepath

import (
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// Resolve returns the path to use for a state file under the brand state
// directory.
//
// suffix is the path suffix under the state dir, e.g. "daemon.yaml".
//
// tmpFallback is used when the home directory cannot be resolved (e.g. in
// some test envs), matching the historical os.UserHomeDir() failure path.
func Resolve(suffix, tmpFallback string) string {
	if dir := statehome.StateDir(suffix); dir != "" {
		return dir
	}
	return tmpFallback
}
