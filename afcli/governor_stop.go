package afcli

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// newGovernorStopCmd constructs the `governor stop` subcommand.
// It reads the saved PID, sends SIGTERM, waits up to 10 seconds
// for graceful shutdown, then SIGKILL if needed.
func newGovernorStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "stop",
		Short:        "Stop the running governor",
		Long:         "Stop the governor process by sending SIGTERM. If it does not exit within 10 seconds, SIGKILL is sent.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			pid, err := loadPID(governorPIDName)
			if err != nil {
				return fmt.Errorf("governor not running: %w", err)
			}

			proc, err := os.FindProcess(pid)
			if err != nil {
				_ = removePIDFile(governorPIDName)
				return fmt.Errorf("find process %d: %w", pid, err)
			}

			// Check if the process is actually running.
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				_ = removePIDFile(governorPIDName)
				return fmt.Errorf("governor (PID %d) is not running (stale pid file removed)", pid)
			}

			// Send SIGTERM for graceful shutdown.
			if err := proc.Signal(syscall.SIGTERM); err != nil {
				_ = removePIDFile(governorPIDName)
				return fmt.Errorf("send SIGTERM to %d: %w", pid, err)
			}

			// Wait up to 10 seconds for the process to exit.
			if waitForExit(proc, 10*time.Second) {
				_ = removePIDFile(governorPIDName)
				fmt.Printf("Governor stopped (PID %d)\n", pid)
				return nil
			}

			// Force kill after timeout.
			if err := proc.Signal(syscall.SIGKILL); err != nil {
				_ = removePIDFile(governorPIDName)
				return fmt.Errorf("send SIGKILL to %d: %w", pid, err)
			}

			_ = removePIDFile(governorPIDName)
			fmt.Printf("Governor killed (PID %d)\n", pid)
			return nil
		},
	}

	return cmd
}

// waitForExit polls the process with Signal(0) at 100ms intervals,
// returning true if the process has exited within the timeout.
func waitForExit(proc *os.Process, timeout time.Duration) bool {
	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				return true
			}
		}
	}
}
