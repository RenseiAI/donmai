package afcli

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/worker"
)

// newFleetStopCmd constructs the `fleet stop` subcommand. It reads the
// fleet PID file, sends SIGTERM to each recorded process, waits up to
// --grace for graceful exit, then SIGKILLs survivors. The PID file is
// always removed at the end so subsequent `fleet status` is clean.
func newFleetStopCmd() *cobra.Command {
	var grace time.Duration

	cmd := &cobra.Command{
		Use:          "stop",
		Short:        "Stop all running fleet workers",
		Long:         "Send SIGTERM to every worker recorded in the fleet PID file, wait up to --grace for graceful exit, then SIGKILL any survivors.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			pids, err := worker.ReadFleetPIDs()
			if err != nil {
				return fmt.Errorf("fleet stop: %w", err)
			}
			if len(pids) == 0 {
				return fmt.Errorf("fleet stop: no fleet PID file (is a fleet running?)")
			}

			stopped := 0
			for _, pid := range pids {
				proc, ferr := os.FindProcess(pid)
				if ferr != nil {
					// Not fatal; log and continue so other workers still get
					// signaled.
					fmt.Fprintf(os.Stderr, "fleet stop: find process %d: %v\n", pid, ferr)
					continue
				}
				// Skip processes that are already gone.
				if sigErr := proc.Signal(syscall.Signal(0)); sigErr != nil {
					continue
				}
				if sigErr := proc.Signal(syscall.SIGTERM); sigErr != nil && !errors.Is(sigErr, os.ErrProcessDone) {
					fmt.Fprintf(os.Stderr, "fleet stop: sigterm %d: %v\n", pid, sigErr)
					continue
				}
				if waitForExit(proc, grace) {
					stopped++
					continue
				}
				// Graceful window elapsed — force kill.
				if killErr := proc.Signal(syscall.SIGKILL); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					fmt.Fprintf(os.Stderr, "fleet stop: sigkill %d: %v\n", pid, killErr)
					continue
				}
				stopped++
			}

			if err := worker.RemoveFleetPIDFile(); err != nil {
				return fmt.Errorf("fleet stop: %w", err)
			}

			fmt.Printf("Fleet stopped: %d workers\n", stopped)
			return nil
		},
	}

	cmd.Flags().DurationVar(&grace, "grace", 10*time.Second, "Grace period before escalating to SIGKILL")

	return cmd
}
