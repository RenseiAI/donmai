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
func configureProbeProcessGroup(command *exec.Cmd) {
	if command == nil {
		return
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
