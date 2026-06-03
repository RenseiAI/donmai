//go:build unix

package agycli

import (
	"os/exec"
	"syscall"
)

// signalProcessGroup sends sig to the entire process group whose leader is
// cmd.Process. `agy` is started by pty.Start with Setsid=true, so the child is
// a session + process-group leader (pgid == pid); signalling the group reaches
// any tool subprocesses it forked. Falls back to the leader alone when the
// pgid cannot be resolved.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		_ = cmd.Process.Signal(sig)
		return
	}
	_ = syscall.Kill(-pgid, sig)
}
