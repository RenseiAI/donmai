package afcli

import (
	"fmt"
	"os"
	"syscall"

	"github.com/spf13/cobra"
)

// newGovernorStatusCmd constructs the `governor status` subcommand.
// It checks whether a governor process is currently running by
// reading the saved PID and probing it with Signal(0).
func newGovernorStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Check if the governor is running",
		Long:         "Report whether the governor process is currently running, along with its PID.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			pid, err := loadPID(governorPIDName)
			if err != nil {
				fmt.Println("Governor is not running")
				return nil
			}

			proc, err := os.FindProcess(pid)
			if err != nil {
				fmt.Println("Governor is not running")
				return nil
			}

			if err := proc.Signal(syscall.Signal(0)); err != nil {
				// Process not running; clean up stale PID file.
				_ = removePIDFile(governorPIDName)
				fmt.Println("Governor is not running (stale pid file cleaned up)")
				return nil
			}

			fmt.Printf("Governor is running (PID %d)\n", pid)
			return nil
		},
	}

	return cmd
}
