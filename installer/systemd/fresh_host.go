package systemd

import (
	"github.com/RenseiAI/donmai/installer/freshhost"
	"github.com/RenseiAI/donmai/internal/statepath"
)

// DefaultConfigPath is the daemon config path `host run` reads when the
// unit names no explicit config: ~/.donmai/daemon.yaml under the host state
// home (a temp fallback only when the home cannot be resolved, mirroring
// the daemon's own default).
func DefaultConfigPath() string {
	return statepath.Resolve("daemon.yaml", "/tmp/.donmai/daemon.yaml")
}

// EnsureFreshHostConfig writes the fresh-host seed config the non-interactive
// first-run path starts from, unless a config already exists at path (empty
// means DefaultConfigPath, or opts.ConfigPath when the unit names one). An
// existing config is validated and left untouched: install must never
// silently widen or narrow a machine the operator already configured. The
// seed — a local file queue with an explicit execution policy, no harness
// or repository profile — starts without the interactive wizard and refuses
// with the operator action (`host setup`) instead of crash-looping on a
// missing answer.
func EnsureFreshHostConfig(path string) (seeded bool, err error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	return freshhost.EnsureConfig(path, "systemd")
}
