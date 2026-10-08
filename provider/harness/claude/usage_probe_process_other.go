//go:build !unix

package claude

import (
	"os/exec"
)

// configureProbeProcessGroup is a no-op where process groups do not
// exist; CommandContext plus WaitDelay still bound the child.
func configureProbeProcessGroup(_ *exec.Cmd) {}
