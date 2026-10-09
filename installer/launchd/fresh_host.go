package launchd

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/internal/statepath"
)

// DefaultConfigPath is the daemon config path `host run` reads on macOS:
// ~/.donmai/daemon.yaml under the host state home (a temp fallback only
// when the home cannot be resolved, mirroring the daemon's own default).
func DefaultConfigPath() string {
	return statepath.Resolve("daemon.yaml", "/tmp/.donmai/daemon.yaml")
}

// EnsureFreshHostConfig writes the fresh-host seed config the non-interactive
// first-run path starts from, unless a config already exists at path (empty
// means DefaultConfigPath, or opts.ConfigPath when the install names one).
// An existing config is validated and left untouched: install must never
// silently widen or narrow a machine the operator already configured. The
// seed — a local file queue with an explicit execution policy, no harness
// or repository profile — starts without the interactive wizard and refuses
// with the operator action (`host setup`) instead of crash-looping on a
// missing answer.
func EnsureFreshHostConfig(path string) (seeded bool, err error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	raw, err := os.ReadFile(path) //nolint:gosec // operator config path
	if err == nil {
		if len(bytes.TrimSpace(raw)) == 0 {
			return false, fmt.Errorf("launchd: daemon config %s is empty — move it aside or run `host setup`", path)
		}
		if _, err := daemon.LoadConfig(path); err != nil {
			return false, fmt.Errorf("launchd: daemon config %s does not load: %w", path, err)
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("launchd: read daemon config %s: %w", path, err)
	}
	cfg := daemon.FreshHostConfig()
	if err := daemon.WriteConfig(path, cfg); err != nil {
		return false, fmt.Errorf("launchd: write fresh-host daemon config %s: %w", path, err)
	}
	if _, err := daemon.LoadConfig(path); err != nil {
		return false, fmt.Errorf("launchd: fresh-host daemon config %s does not load: %w", path, err)
	}
	return true, nil
}
