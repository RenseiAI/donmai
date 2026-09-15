//go:build !windows

package codex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func syscallSIGTERM() os.Signal { return syscall.SIGTERM }

func configureOwnedProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// ownedProcess is the sole waiter and signal authority for this child. Keeping
// the direct child unreaped reserves its PID, even when the launcher exits
// before its descendants. No group operation is permitted after Wait.
// stopResult is published before callbacks, including on inspection failure.
type ownedProcess struct {
	cmd     *exec.Cmd
	request chan time.Duration
	stopped chan struct{}
	stopErr error
}

func newOwnedProcess(cmd *exec.Cmd) *ownedProcess {
	return &ownedProcess{cmd: cmd, request: make(chan time.Duration, 1), stopped: make(chan struct{})}
}

func (p *ownedProcess) stop(grace time.Duration) error {
	select {
	case p.request <- grace:
	default:
	}
	<-p.stopped
	return p.stopErr
}

func (p *ownedProcess) wait() error {
	exited := make(chan error, 1)
	go func() { exited <- observeOwnedProcessExit(p.cmd.Process.Pid) }()
	grace := 5 * time.Second
	observed := false
	select {
	case grace = <-p.request:
	case err := <-exited:
		observed = true
		if err != nil {
			p.stopErr = fmt.Errorf("observe owned Codex exit: %w", err)
		}
	}
	if p.stopErr == nil {
		p.stopErr = stopOwnedProcessGroup(p.cmd, grace, ownedProcessGroupState, syscall.Kill)
	}
	// On failure there is no quiescence proof. Return the error promptly and
	// retain the home; the sole waiter still reaps the child when it exits.
	if p.stopErr != nil {
		close(p.stopped)
	}
	// Join the non-reaping observer too, so it cannot attach to a reused PID.
	if !observed {
		<-exited
	}
	err := p.cmd.Wait()
	if p.stopErr == nil {
		close(p.stopped)
	}
	return err
}

type ownedGroupState struct {
	anchored       bool
	leaderRunning  bool
	writersRunning bool
}

// The syscall seams exercise disappearance and refused signals without ever
// targeting a foreign host process. They are local to this lifecycle operation.
func stopOwnedProcessGroup(cmd *exec.Cmd, grace time.Duration,
	inspect func(int) (ownedGroupState, error), signal func(int, syscall.Signal) error,
) error {
	if cmd.Process == nil || cmd.ProcessState != nil || cmd.SysProcAttr == nil ||
		!cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 || cmd.Process.Pid <= 0 {
		return errors.New("refusing to signal a process group without an unreaped owned leader")
	}
	group := cmd.Process.Pid
	state := func() (bool, error) {
		s, err := inspect(group)
		if err != nil {
			return false, fmt.Errorf("inspect owned Codex writers: %w", err)
		}
		if !s.anchored {
			return false, errors.New("owned Codex process group lost its unreaped leader")
		}
		return s.writersRunning, nil
	}
	send := func(sig syscall.Signal) error {
		if err := signal(-group, sig); err != nil {
			// Some kernels refuse signals to a zombie-only group. Errno alone
			// proves neither death nor ownership; inspect the still-pinned group.
			running, inspectErr := state()
			if inspectErr == nil && !running {
				return nil
			}
			return errors.Join(fmt.Errorf("signal owned Codex process group: %w", err), inspectErr)
		}
		return nil
	}
	running, err := state()
	if err != nil || !running {
		return err
	}
	if err := send(syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		running, err := state()
		if err != nil || !running {
			return err
		}
		select {
		case <-deadline.C:
			if err := send(syscall.SIGKILL); err != nil {
				return err
			}
		case <-tick.C:
		}
	}
}

func ownedProcessGroupState(group int) (ownedGroupState, error) {
	// No signal-0 probe: it can return EPERM for a zombie-only group. Only
	// non-secret process IDs and execution states are read.
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,pgid=,stat=").Output()
	if err != nil {
		return ownedGroupState{}, err
	}
	return parseOwnedProcessGroupState(string(out), group, os.Getpid())
}

func parseOwnedProcessGroupState(out string, group, parent int) (ownedGroupState, error) {
	var state ownedGroupState
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 4 {
			return state, errors.New("could not parse owned Codex writer status")
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, parentErr := strconv.Atoi(fields[1])
		pgid, groupErr := strconv.Atoi(fields[2])
		if err := errors.Join(pidErr, parentErr, groupErr); err != nil {
			return state, err
		}
		if pgid != group {
			continue
		}
		running := !strings.HasPrefix(fields[3], "Z")
		if pid == group {
			state.anchored = ppid == parent
			state.leaderRunning = running
		}
		state.writersRunning = state.writersRunning || running
	}
	return state, nil
}
