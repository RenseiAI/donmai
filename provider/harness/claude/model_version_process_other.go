//go:build !unix

package claude

import (
	"context"
	"errors"
	"os/exec"
)

// A platform without the owned-group observer refuses this model-specific
// probe before starting a child. It must not claim descendant quiescence from
// CommandContext or Cmd.Wait alone.
func runOwnedModelVersionProbe(_ context.Context, _ *exec.Cmd) (bool, error) {
	return true, errors.New("owned Claude Code version probe is unavailable on this platform")
}
