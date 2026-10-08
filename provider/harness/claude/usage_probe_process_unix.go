//go:build unix

package claude

import (
	"os/exec"
	"syscall"
)

// configureProbeProcessGroup detaches the usage-probe child into its own
// process group so context expiry kills the whole group (the CLI plus
// any wrapper it forked), never just the leader. It mirrors the owned
// login/version probes' group discipline without their ps-observation
// machinery: the usage read needs a bounded kill, not a quiescence
// proof — its output is scanned, never trusted.
//
// Detaching alone is not the kill: CommandContext's default cancel
// signals only the leader, orphaning any forked child past the probe
// timeout. Cancel therefore signals the negative pid — the group —
// so the timeout reaps the CLI and everything it forked together.
func configureProbeProcessGroup(command *exec.Cmd) {
	if command == nil {
		return
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}
