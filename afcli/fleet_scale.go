package afcli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newFleetScaleCmd constructs the `fleet scale` subcommand.
//
// TODO(REN-1074): fleet scale currently returns an error advising the
// operator to stop and start with a new --count. A full implementation
// requires a supervisor daemon (or equivalent IPC) so scale can reach
// the live Fleet instance to either spawn additional children or
// gracefully shrink the running set. The detached-scale approach (re-
// read the PID file, spawn/kill diff processes, rewrite the PID file)
// is viable but loses the workerEntry bookkeeping that Fleet uses to
// wait for clean child exit. Deferring that design to a follow-up.
func newFleetScaleCmd() *cobra.Command {
	var count int

	cmd := &cobra.Command{
		Use:          "scale",
		Short:        "Scale the running fleet to --count workers",
		Long:         "Scale the running fleet. Not yet supported: stop and start with the desired count instead.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if count <= 0 {
				return fmt.Errorf("fleet scale: --count must be > 0")
			}
			return fmt.Errorf("fleet scale: not yet supported — stop and start with new count")
		},
	}

	cmd.Flags().IntVar(&count, "count", 0, "Target worker count (required, > 0)")
	_ = cmd.MarkFlagRequired("count")

	return cmd
}
