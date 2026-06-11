package afcli

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/RenseiAI/donmai/internal/process"
	"github.com/spf13/cobra"
)

// newGovernorStatusCmd constructs the `governor status` subcommand.
// It checks whether a governor process is currently running by reading
// the saved PID (the same process.PIDFile `governor start` writes) and
// probing it with Signal(0).
func newGovernorStatusCmd(bin string) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Check if the governor is running",
		Long:         "Report whether the governor process is currently running, along with its PID.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			pf, err := process.NewPIDFile(governorPIDName)
			if err != nil {
				return fmt.Errorf("governor status: %w", err)
			}

			pid, err := pf.Read()
			switch {
			case errors.Is(err, process.ErrStalePID):
				_ = pf.Remove()
				fmt.Printf("Governor is not running (stale pid file cleaned up) — start with `%s governor start`\n", bin)
				return nil
			case err != nil:
				fmt.Printf("Governor is not running — start with `%s governor start`\n", bin)
				return nil
			}

			proc, err := os.FindProcess(pid)
			if err != nil {
				fmt.Printf("Governor is not running — start with `%s governor start`\n", bin)
				return nil
			}

			// Re-probe liveness — platforms where PIDFile.Read does
			// not probe (Windows) still need the stale check.
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				_ = pf.Remove()
				fmt.Printf("Governor is not running (stale pid file cleaned up) — start with `%s governor start`\n", bin)
				return nil
			}

			fmt.Printf("Governor is running (PID %d)\n", pid)
			return nil
		},
	}

	return cmd
}
