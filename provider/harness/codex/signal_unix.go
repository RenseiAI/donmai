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

// syscallSIGTERM returns the SIGTERM signal value for the current
// platform. On unix, that's syscall.SIGTERM. On windows the Provider
// falls back to os.Interrupt because windows lacks SIGTERM.
//
// macOS-only Phase F per HANDOFF (Windows is deferred), so
// the unix branch is the load-bearing path.
func syscallSIGTERM() os.Signal { return syscall.SIGTERM }

// Give each headless provider its own group; never signal the caller's group.
// Node launchers and native Codex can both leave bootstrap children behind.
func configureOwnedProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopOwnedProcessGroup(cmd *exec.Cmd, done <-chan error, grace time.Duration) error {
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 || cmd.Process.Pid <= 0 {
		return errors.New("refusing to signal a process group not created for this Codex provider")
	}
	group := cmd.Process.Pid // Setpgid made this child the group leader.
	send := func(signal syscall.Signal) error {
		if err := syscall.Kill(-group, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal owned Codex process group: %w", err)
		}
		return nil
	}
	if err := send(syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	// This polls process exit, not filesystem cleanup. There is one removal
	// attempt, after the same TERM/KILL grace used by direct-child shutdown.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		running, err := ownedProcessGroupRunning(group)
		if err != nil {
			return err // No proof of quiescence: retain the home.
		}
		if !running {
			<-done
			return nil
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

func ownedProcessGroupRunning(group int) (bool, error) {
	if err := syscall.Kill(-group, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect owned Codex process group: %w", err)
	}
	// An orphaned zombie cannot write and may await a host init's reaper.
	// Read only group IDs and status, never command arguments or environments.
	out, err := exec.Command("ps", "-axo", "pgid=,stat=").Output()
	if err != nil {
		return false, fmt.Errorf("inspect owned Codex writer status: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return false, errors.New("could not parse owned Codex writer status")
		}
		if fields[0] != strconv.Itoa(group) {
			continue
		}
		if !strings.HasPrefix(fields[1], "Z") {
			return true, nil
		}
	}
	return false, nil
}
