//go:build unix

package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	modelVersionGroupGrace   = 250 * time.Millisecond
	modelVersionGroupMaxWait = 2 * time.Second
	modelVersionObserveStep  = 10 * time.Millisecond
	modelVersionPSTimeout    = 500 * time.Millisecond
	modelVersionPSOutputMax  = 1 << 20
)

type modelVersionGroupState struct {
	anchored, leaderRunning, writersRunning bool
}

// Keep the buffer named so io.Copy cannot bypass Write through promoted ReadFrom.
type boundedModelVersionPSOutput struct{ buffer bytes.Buffer }

func (b *boundedModelVersionPSOutput) Len() int       { return b.buffer.Len() }
func (b *boundedModelVersionPSOutput) String() string { return b.buffer.String() }

func (b *boundedModelVersionPSOutput) Write(p []byte) (int, error) {
	if len(p) > modelVersionPSOutputMax-b.Len() {
		return 0, errors.New("owned process inspection exceeds bound")
	}
	return b.buffer.Write(p)
}

// runOwnedModelVersionProbe is the only waiter. A started child is kept
// unreaped through every group inspection and signal, pinning its PID against
// reuse. The group's writers must be observed quiet before the private home
// can be removed; an uncertain result is an error with quiescent=false.
func runOwnedModelVersionProbe(ctx context.Context, cmd *exec.Cmd) (quiescent bool, runErr error) {
	if err := checkModelVersionPS(); err != nil {
		return true, err // no probe process was started
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return true, err // no probe process was started
	}
	group := cmd.Process.Pid
	tick := time.NewTicker(modelVersionObserveStep)
	defer tick.Stop()
	var observationErr error
observe:
	for {
		state, err := inspectModelVersionGroup(group)
		if err != nil {
			observationErr = err
			break
		}
		if !state.anchored {
			observationErr = errors.New("version probe lost its unreaped child anchor")
			break
		}
		if !state.leaderRunning {
			break
		}
		select {
		case <-ctx.Done():
			break observe
		case <-tick.C:
		}
	}
	stopErr := stopModelVersionGroup(group)
	// No group operation follows Wait, including on inspection failure.
	waitErr := cmd.Wait()
	if observationErr != nil || stopErr != nil {
		return false, errors.Join(observationErr, stopErr, waitErr)
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return false, waitErr // inherited pipes may still have writers
	}
	return true, waitErr
}

func checkModelVersionPS() error {
	if _, err := modelVersionPSTable(); err != nil {
		return fmt.Errorf("inspect support for owned version probe: %w", err)
	}
	return nil
}

func modelVersionPSTable() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), modelVersionPSTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,pgid=,stat=")
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	cmd.Stderr = io.Discard
	var out boundedModelVersionPSOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("inspect owned version processes: %w", err)
	}
	return out.String(), nil
}

func inspectModelVersionGroup(group int) (modelVersionGroupState, error) {
	table, err := modelVersionPSTable()
	if err != nil {
		return modelVersionGroupState{}, err
	}
	var state modelVersionGroupState
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 4 {
			return state, errors.New("could not parse owned version process status")
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, parentErr := strconv.Atoi(fields[1])
		pgid, groupErr := strconv.Atoi(fields[2])
		if err := errors.Join(pidErr, parentErr, groupErr); err != nil {
			return state, fmt.Errorf("parse owned version process identity: %w", err)
		}
		if pgid != group {
			continue
		}
		running := !strings.HasPrefix(fields[3], "Z")
		if pid == group {
			state.anchored = ppid == os.Getpid()
			state.leaderRunning = running
		}
		state.writersRunning = state.writersRunning || running
	}
	return state, nil
}

func stopModelVersionGroup(group int) error {
	selfGroup, err := syscall.Getpgid(os.Getpid())
	if err != nil || group <= 1 || group == selfGroup {
		return errors.New("refusing unsafe version probe process group")
	}
	inspect := func() (bool, error) {
		state, err := inspectModelVersionGroup(group)
		if err != nil {
			return false, err
		}
		if !state.anchored {
			return false, errors.New("version probe lost its unreaped child anchor")
		}
		return state.writersRunning, nil
	}
	running, err := inspect()
	if err != nil || !running {
		return err
	}
	send := func(sig syscall.Signal) error {
		if err := syscall.Kill(-group, sig); err != nil {
			stillRunning, inspectErr := inspect()
			if inspectErr == nil && !stillRunning {
				return nil
			}
			return errors.Join(fmt.Errorf("signal owned version probe group: %w", err), inspectErr)
		}
		return nil
	}
	if err := send(syscall.SIGTERM); err != nil {
		return err
	}
	grace := time.NewTimer(modelVersionGroupGrace)
	defer grace.Stop()
	deadline := time.NewTimer(modelVersionGroupMaxWait)
	defer deadline.Stop()
	tick := time.NewTicker(modelVersionObserveStep)
	defer tick.Stop()
	killed := false
	for {
		running, err := inspect()
		if err != nil || !running {
			return err
		}
		select {
		case <-grace.C:
			if !killed {
				if err := send(syscall.SIGKILL); err != nil {
					return err
				}
				killed = true
			}
		case <-deadline.C:
			return errors.New("owned version probe group did not quiesce")
		case <-tick.C:
		}
	}
}
