//go:build unix

package ptyhost

import (
	"os/exec"
	"syscall"

	"github.com/RenseiAI/donmai/attachwire"
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
//
// The common set lives in attachwire beside the exit-code convention it pairs
// with, so an Exit payload and a terminal tombstone spell one signal the same
// way. SIGBUS stays here because its NUMBER differs across the unixes (7 on
// Linux, 10 on darwin) and the portable table is keyed by number.
func signalName(sig syscall.Signal) string {
	if sig == 0 {
		return ""
	}
	if name := attachwire.SignalName(int(sig)); name != "" {
		return name
	}
	if sig == syscall.SIGBUS {
		return "SIGBUS"
	}
	// syscall.Signal.String() yields e.g. "user defined signal 1"; the
	// ansi/shell convention wants a SIG-prefixed name, so fall back to one.
	return "SIG" + upperSignal(sig)
}

func upperSignal(sig syscall.Signal) string {
	s := sig.String()
	return s
}
