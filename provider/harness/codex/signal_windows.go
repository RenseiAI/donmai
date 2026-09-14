//go:build windows

package codex

import (
	"os"
	"os/exec"
	"time"
)

// syscallSIGTERM returns os.Interrupt on windows since windows has no
// SIGTERM. Out-of-scope for Phase F (deferred), but the build
// constraint keeps the package buildable on windows for downstream
// consumers.
func syscallSIGTERM() os.Signal { return os.Interrupt }

func configureOwnedProcessGroup(_ *exec.Cmd) {}

func stopOwnedProcessGroup(cmd *exec.Cmd, done <-chan error, grace time.Duration) error {
	_ = cmd.Process.Signal(syscallSIGTERM())
	select {
	case <-done:
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		<-done
	}
	return nil
}
