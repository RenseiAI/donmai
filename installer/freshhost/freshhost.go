// Package freshhost owns the shared fresh-host seed contract: what a
// machine with no authored config starts from, and how install treats an
// existing config. Both platform installers (systemd on Linux, launchd on
// macOS) delegate here so empty-file and whitespace handling cannot
// diverge between them.
package freshhost

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/RenseiAI/donmai/daemon"
)

// EnsureConfig writes the fresh-host seed config the non-interactive
// first-run path starts from, unless a config already exists at path. An
// existing config is validated and left untouched: install must never
// silently widen or narrow a machine the operator already configured. The
// seed — a local file queue with an explicit execution policy, no harness
// or repository profile — starts without the interactive wizard and refuses
// with the operator action (`host setup`) instead of crash-looping on a
// missing answer.
//
// brand names the supervisor in error text ("systemd" or "launchd").
func EnsureConfig(path, brand string) (seeded bool, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator config path
	if err == nil {
		if len(bytes.TrimSpace(raw)) == 0 {
			return false, fmt.Errorf("%s: daemon config %s is empty — move it aside or run `host setup`", brand, path)
		}
		if _, err := daemon.LoadConfig(path); err != nil {
			return false, fmt.Errorf("%s: daemon config %s does not load: %w", brand, path, err)
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%s: read daemon config %s: %w", brand, path, err)
	}
	cfg := daemon.FreshHostConfig()
	if err := daemon.WriteConfig(path, cfg); err != nil {
		return false, fmt.Errorf("%s: write fresh-host daemon config %s: %w", brand, path, err)
	}
	if _, err := daemon.LoadConfig(path); err != nil {
		return false, fmt.Errorf("%s: fresh-host daemon config %s does not load: %w", brand, path, err)
	}
	return true, nil
}
