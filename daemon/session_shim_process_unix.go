//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
	"os/exec"
	"syscall"
)

// configureShimProcess puts a session shim in its OWN session, not merely its
// own process group (ADR-2026-08-17 §D1).
//
// Setpgid — what an ordinary worker gets — is not enough here. The whole point
// of the shim is to outlive the daemon, and a service manager that stops the
// daemon job reaps the job's descendants; a new SESSION is what takes the shim
// out of that kill scope. The ADR is explicit that setsid alone is not accepted
// as PROOF on macOS — the launchd job definition has to cooperate and the
// real-service smoke is the gate — but without the new session there is nothing
// for that smoke to prove.
func configureShimProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	// Setsid already makes the child a group leader; Setpgid alongside it is
	// rejected on some platforms, so it is deliberately NOT set.
}
