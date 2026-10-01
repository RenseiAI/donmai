//go:build unix

package afcli

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

// This private probe follows provider/harness/claude/model_version_process_unix.go:
// keep the exact child unreaped as a PID/group anchor, inspect only that group,
// stop its writers, then call Wait once. No PID-name or global cleanup applies.
const (
	gitHubProbeGrace     = 250 * time.Millisecond
	gitHubProbeStopLimit = 2 * time.Second
	gitHubProbeObserve   = 10 * time.Millisecond
	gitHubProbePSTimeout = 500 * time.Millisecond
	gitHubProbePSLimit   = 1 << 20
)

type gitHubProbeGroupState struct{ anchored, leaderRunning, writersRunning bool }

type boundedGitHubPSOutput struct{ buffer bytes.Buffer }

func (b *boundedGitHubPSOutput) Write(data []byte) (int, error) {
	if len(data) > gitHubProbePSLimit-b.buffer.Len() {
		return 0, errors.New("local GitHub process inspection exceeds bound")
	}
	return b.buffer.Write(data)
}

func runOwnedGitHubTokenProbe(ctx context.Context, command *exec.Cmd) (quiescent bool, runErr error) {
	if _, err := localGitHubPSTable(); err != nil {
		return true, fmt.Errorf("local GitHub process inspection unavailable: %w", err)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return true, err
	}
	group := command.Process.Pid
	tick := time.NewTicker(gitHubProbeObserve)
	defer tick.Stop()
	var observationErr error
observe:
	for {
		state, err := inspectLocalGitHubGroup(group)
		if err != nil {
			observationErr = err
			break
		}
		if !state.anchored {
			observationErr = errors.New("local GitHub probe lost its unreaped child anchor")
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
	stopErr := stopLocalGitHubGroup(group)
	// No group operation follows Wait: reaping the leader could reuse its PID.
	waitErr := command.Wait()
	if observationErr != nil || stopErr != nil {
		return false, errors.Join(observationErr, stopErr, waitErr)
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return false, waitErr
	}
	return true, waitErr
}

func localGitHubPSTable() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitHubProbePSTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,pgid=,stat=")
	command.Dir = "/"
	command.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	command.Stderr = io.Discard
	var output boundedGitHubPSOutput
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("inspect local GitHub probe: %w", err)
	}
	return output.buffer.String(), nil
}

func inspectLocalGitHubGroup(group int) (gitHubProbeGroupState, error) {
	table, err := localGitHubPSTable()
	if err != nil {
		return gitHubProbeGroupState{}, err
	}
	var state gitHubProbeGroupState
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 4 {
			return state, errors.New("could not parse local GitHub process status")
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, parentErr := strconv.Atoi(fields[1])
		pgid, groupErr := strconv.Atoi(fields[2])
		if err := errors.Join(pidErr, parentErr, groupErr); err != nil {
			return state, errors.New("could not parse local GitHub process identity")
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

func stopLocalGitHubGroup(group int) error {
	selfGroup, err := syscall.Getpgid(os.Getpid())
	if err != nil || group <= 1 || group == selfGroup {
		return errors.New("refusing unsafe local GitHub process group")
	}
	inspect := func() (bool, error) {
		state, inspectErr := inspectLocalGitHubGroup(group)
		if inspectErr != nil {
			return false, inspectErr
		}
		if !state.anchored {
			return false, errors.New("local GitHub probe lost its unreaped child anchor")
		}
		return state.writersRunning, nil
	}
	running, err := inspect()
	if err != nil || !running {
		return err
	}
	send := func(signal syscall.Signal) error {
		if err := syscall.Kill(-group, signal); err != nil {
			stillRunning, inspectErr := inspect()
			if inspectErr == nil && !stillRunning {
				return nil
			}
			return errors.Join(errors.New("signal owned local GitHub process group failed"), inspectErr)
		}
		return nil
	}
	if err := send(syscall.SIGTERM); err != nil {
		return err
	}
	grace := time.NewTimer(gitHubProbeGrace)
	defer grace.Stop()
	deadline := time.NewTimer(gitHubProbeStopLimit)
	defer deadline.Stop()
	tick := time.NewTicker(gitHubProbeObserve)
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
			return errors.New("local GitHub process group did not quiesce")
		case <-tick.C:
		}
	}
}
