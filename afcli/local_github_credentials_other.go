//go:build !unix

package afcli

import (
	"context"
	"errors"
	"os/exec"
)

func runOwnedGitHubTokenProbe(_ context.Context, _ *exec.Cmd) (bool, error) {
	return true, errors.New("local GitHub CLI credential lookup lacks an owned-process observer on this platform")
}
