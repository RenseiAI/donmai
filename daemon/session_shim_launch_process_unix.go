//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// shimAbandonedLaunchStopGrace is the cooperative window a stopped-but-abandoned
// launch gets between SIGTERM and SIGKILL, and again between SIGKILL and giving
// up on the reap.
//
// Longer than sessionTerminationGrace (250ms, the direct-child lane's window)
// because this process is a shim: its own teardown closes a PTY and reaps a
// harness group of its own. Still short, because the accept goroutine is
// blocked on it — the whole point of stopping here rather than handing the
// process to an asynchronous pass is that the abort is true when it is reported.
const shimAbandonedLaunchStopGrace = 2 * time.Second

// shimAbandonedLaunchReapInterval paces the non-blocking waitpid polls inside
// that grace.
const shimAbandonedLaunchReapInterval = 10 * time.Millisecond

// osShimLaunchProcess is the production shimLaunchProcess: the launched worker
// addressed by the identity startShimProcess pinned at spawn time.
type osShimLaunchProcess struct {
	identity sessionshim.ProcessIdentity
	// signalGroupFn, when set, replaces the syscall-backed group signal. Nil in
	// every production path: it exists so a test can present the one refusal
	// that cannot be staged from outside the kernel — a group whose only member
	// died between the liveness probe and the signal, which BSD/macOS answers
	// EPERM rather than ESRCH.
	signalGroupFn func(syscall.Signal) error
}

func newShimLaunchProcess(started sessionshim.ProcessIdentity) shimLaunchProcess {
	return osShimLaunchProcess{identity: started}
}

// Alive reports whether the launched worker is still running, reaping it when it
// has exited.
//
// The reap is what makes the answer trustworthy AND what keeps the process table
// clean. sessionshim.ProcessIdentity.Alive alone cannot do it: a zombie is still
// in the process table with its original start time, so an identity probe
// reports a defunct child as ALIVE — forever, since nobody is waiting on it.
// waitpid is the only observation that distinguishes them, and this daemon is
// the one process entitled to make it, because the abandoned worker is its own
// child.
//
// The wait is targeted at exactly this pid and non-blocking: it can never reap
// another subsystem's child, and it can never block the accept goroutine. ECHILD
// means this process has no such child — either it was already reaped or it was
// never ours — so the identity probe answers instead, which is also the right
// answer after this daemon has exited and the shim has been reparented.
func (p osShimLaunchProcess) Alive() (bool, error) {
	if p.identity.PID <= 1 {
		return false, fmt.Errorf("session shim: refusing to probe pid %d", p.identity.PID)
	}
	var status syscall.WaitStatus
	wpid, err := syscall.Wait4(p.identity.PID, &status, syscall.WNOHANG, nil)
	switch {
	case err == nil && wpid == p.identity.PID:
		return false, nil // exited, and reaped right here
	case err == nil:
		return true, nil // still running as our child; pid reuse is impossible while unreaped
	case errors.Is(err, syscall.EINTR):
		return p.identity.Alive()
	case errors.Is(err, syscall.ECHILD):
		return p.identity.Alive()
	default:
		return p.identity.Alive()
	}
}

// StopAndReap terminates the launched worker's process group and reaps the
// direct child.
//
// The GROUP, not the pid: startShimProcess puts every shim in a session of its
// own (configureShimProcess), so the worker is a group leader and its harness
// descends from it. Signalling only the leader would leave that harness running
// — which is the exact shape of the incident this exists to end.
//
// SIGTERM first, then SIGKILL after a bounded grace, then a bounded reap. The
// reap is not optional politeness: without it the daemon's own child list keeps
// a defunct entry for as long as this daemon lives.
//
// A REFUSED SIGNAL IS NOT AUTOMATICALLY A FAILURE. ESRCH is the obvious "nothing
// there", but it is not the only one: a group whose last member died between the
// probe above and the signal below is zombie-only, and BSD/macOS answers a
// signal to it EPERM, not ESRCH. Reporting that as a failed stop would make
// stopAbandonedShimLaunch log "may keep running un-adopted" about a worker that
// is already gone — the exact false statement this whole change exists to
// prevent, inverted. So every signal failure is classified the same way
// waitSessionProcessGroup classifies the direct-child lane's: by re-probing, and
// by treating a reaped child as the success it is.
func (p osShimLaunchProcess) StopAndReap() error {
	alive, err := p.Alive()
	if err != nil {
		if p.identity.StartedAt == 0 {
			// UNPINNED AND UNPROBEABLE. ADR-2026-08-17's crash matrix requires the
			// recorded leader's start identity to be verified before a SIGTERM →
			// SIGKILL, and neither half is available here: startShimProcess could
			// not pin a start time (so the identity cannot be verified even in
			// principle) and waitpid cannot answer either. Signalling anyway would
			// address a bare PID, which is exactly the reuse-unsafe comparison
			// sessionshim.ProcessIdentity exists to forbid. Refuse, loudly, and
			// leave the pid in the caller's log.
			return fmt.Errorf("session shim: refusing to stop abandoned launch %s: its start identity was never "+
				"pinned and it could not be probed: %w", p.identity, err)
		}
		// An unprobeable but PINNED process is still worth signalling — the
		// signal reports ESRCH if there is nothing there, and the classification
		// below re-probes before calling anything a failure.
		alive = true
	}
	if !alive {
		return nil // already exited and reaped by the probe above
	}
	if signalErr := p.signalGroup(syscall.SIGTERM); signalErr != nil {
		if p.stopFailureMeansGone(signalErr) {
			return nil
		}
		return fmt.Errorf("session shim: terminate abandoned launch %s: %w", p.identity, signalErr)
	}
	if p.awaitReap(shimAbandonedLaunchStopGrace) {
		return nil
	}
	if signalErr := p.signalGroup(syscall.SIGKILL); signalErr != nil {
		if p.stopFailureMeansGone(signalErr) {
			return nil
		}
		return fmt.Errorf("session shim: kill abandoned launch %s: %w", p.identity, signalErr)
	}
	if p.awaitReap(shimAbandonedLaunchStopGrace) {
		return nil
	}
	return fmt.Errorf("session shim: abandoned launch %s did not exit within %s of SIGKILL",
		p.identity, shimAbandonedLaunchStopGrace)
}

// stopFailureMeansGone reports whether a refused group signal only means the
// launched child is already gone — in which case the stop SUCCEEDED and must not
// be reported as a failure.
//
// ESRCH says so outright. Every other refusal is decided by the same evidence
// this file trusts everywhere else: waitpid. A zombie-only group answers EPERM on
// BSD/macOS, and its leader is precisely the child a re-probe reaps, so the
// re-probe both settles the question and discharges the reap. The bounded window
// is the same stop grace: the member that died between the probe and the signal
// may still be a moment from being reportable.
//
// What it deliberately cannot prove is a surviving DESCENDANT: nothing can wait
// on a process that is not this daemon's child. That limit is the same one
// waitSessionProcessGroup carries, and the conservative half of it is already
// paid — the leader is dead and reaped, so nothing here holds a session open.
func (p osShimLaunchProcess) stopFailureMeansGone(signalErr error) bool {
	// The reap runs FIRST, and for every refusal including ESRCH: "there was
	// nothing to signal" does not mean "there is nothing to reap". A child that
	// exited a moment ago is not signallable and is still a defunct entry until
	// waitpid collects it, and this is the last place that can collect it.
	if p.awaitReap(shimAbandonedLaunchStopGrace) {
		return true
	}
	return errors.Is(signalErr, syscall.ESRCH)
}

// signalGroup sends sig to the launched worker's whole process group.
//
// It carries the same refusals signalSessionProcessGroup applies on the
// direct-child lane — never pid 0/1, never this daemon, never this daemon's own
// group — re-expressed against a PID because the shim lane deliberately keeps no
// exec.Cmd to address: startShimProcess released it, which is the ownership move
// §D1 is built on.
func (p osShimLaunchProcess) signalGroup(sig syscall.Signal) error {
	if p.signalGroupFn != nil {
		return p.signalGroupFn(sig)
	}
	pid := p.identity.PID
	if pid <= 1 {
		return fmt.Errorf("session shim: refusing to signal pid %d", pid)
	}
	if pid == os.Getpid() {
		return errors.New("session shim: refusing to signal the daemon process")
	}
	if selfPGID, selfErr := syscall.Getpgid(os.Getpid()); selfErr == nil && pid == selfPGID {
		return fmt.Errorf("session shim: refusing to signal this daemon's own process group %d", pid)
	}
	// configureShimProcess's setsid makes the launched worker its own session and
	// group leader, so the group id IS the pid. A leader that has already exited
	// makes Getpgid report ESRCH while descendants can still need the signal, so
	// the known group id is addressed either way — the same reasoning
	// signalSessionProcessGroup documents.
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid != pid {
		return fmt.Errorf("session shim: refusing unsafe process group %d for launched worker %d", pgid, pid)
	}
	return syscall.Kill(-pid, sig)
}

// awaitReap polls waitpid until the child is reaped or maxWait elapses.
func (p osShimLaunchProcess) awaitReap(maxWait time.Duration) bool {
	deadline := time.Now().Add(maxWait)
	for {
		alive, err := p.Alive()
		if err == nil && !alive {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(shimAbandonedLaunchReapInterval)
	}
}
