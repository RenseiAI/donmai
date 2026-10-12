//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runner

import (
	"os"
	"os/exec"
	"os/user"
	"syscall"
	"time"
)

// homeDir returns the daemon user's home for the install env's HOME.
// It prefers $HOME when set (the daemon host's own value) and falls
// back to the user database.
func homeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if current, err := user.Current(); err == nil && current.HomeDir != "" {
		return current.HomeDir
	}
	return ""
}

// asExitError unwraps runErr to an *exec.ExitError when the command ran
// and exited non-zero.
func asExitError(runErr error) *exec.ExitError {
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		return exitErr
	}
	return nil
}

// setNewProcessGroup puts the install command in its own process group
// so an expired install deadline kills the whole tree, not just the
// leader.
func setNewProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killDepsInstallGroup terminates the install command's process group:
// SIGTERM for a cooperative cleanup window, then SIGKILL. Failures are
// best-effort — the step already expired — but a live group after
// SIGKILL is waited out briefly so the tail log cannot interleave with
// the agent's first turn.
func killDepsInstallGroup(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 {
		return
	}
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid != pid {
		return
	}
	selfPGID, selfErr := syscall.Getpgid(os.Getpid())
	if selfErr == nil && pgid == selfPGID {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err != nil {
			return
		}
		<-tick.C
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
