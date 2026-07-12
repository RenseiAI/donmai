//go:build unix

package ptyhost

import (
	"os/exec"
	"syscall"
)

// signalProcessGroup sends sig to the entire process group whose leader is
// cmd.Process. pty.StartWithSize sets Setsid=true, so the child is a session +
// process-group leader (pgid == pid); signalling the group reaches any tool
// subprocesses it forked (generalized from the agycli PTY precedent). Falls
// back to the leader alone when the pgid cannot be resolved.
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

// signalName maps a syscall.Signal to its conventional name for the Exit
// payload (§12.2). It returns the empty string for a nil / zero signal.
func signalName(sig syscall.Signal) string {
	if sig == 0 {
		return ""
	}
	// syscall.Signal.String() yields e.g. "killed"; the ansi/shell convention
	// wants "SIGKILL". Map the common ones and fall back to the numeric form.
	switch sig {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGILL:
		return "SIGILL"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGFPE:
		return "SIGFPE"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGALRM:
		return "SIGALRM"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGBUS:
		return "SIGBUS"
	default:
		return "SIG" + upperSignal(sig)
	}
}

func upperSignal(sig syscall.Signal) string {
	s := sig.String()
	return s
}
