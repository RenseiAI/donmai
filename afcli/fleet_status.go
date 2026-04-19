package afcli

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/worker"
)

// newFleetStatusCmd constructs the `fleet status` subcommand. It reads
// the fleet PID file and reports a simple running/dead table by probing
// each recorded PID with Signal(0).
func newFleetStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Report the liveness of each fleet worker",
		Long:         "Read the fleet PID file and probe each worker with Signal(0) to determine whether it is running or dead. A dead entry does not affect the exit code.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pids, err := worker.ReadFleetPIDs()
			if err != nil {
				return fmt.Errorf("fleet status: %w", err)
			}
			out := cmd.OutOrStdout()
			if len(pids) == 0 {
				_, _ = fmt.Fprintln(out, "Fleet is not running")
				return nil
			}
			return writeFleetStatusTable(out, pids)
		},
	}
	return cmd
}

// writeFleetStatusTable renders a tab-aligned PID | STATE table to w.
// STATE is "running" if Signal(0) succeeds; "dead" otherwise.
func writeFleetStatusTable(w io.Writer, pids []int) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "PID\tSTATE"); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, pid := range pids {
		state := "dead"
		if proc, err := os.FindProcess(pid); err == nil {
			if sigErr := proc.Signal(syscall.Signal(0)); sigErr == nil {
				state = "running"
			}
		}
		if _, err := fmt.Fprintf(tw, "%d\t%s\n", pid, state); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush table: %w", err)
	}
	return nil
}
