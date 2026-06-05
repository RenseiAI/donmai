package afcli

import (
	"github.com/RenseiAI/donmai/afclient"
	"github.com/spf13/cobra"
)

// newGovernorCmd constructs the `governor` parent command group.
// It holds no logic of its own; it dispatches to start/stop/status
// subcommands that manage the governor scan-loop process.
func newGovernorCmd(_ func() afclient.DataSource, cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	cmd := &cobra.Command{
		Use:          "governor",
		Short:        "Manage the Donmai governor (scan loop)",
		Long:         "Manage the Donmai governor process that scans Linear issues and dispatches work to the agent queue.",
		SilenceUsage: true,
	}

	cmd.AddCommand(newGovernorStartCmd())
	cmd.AddCommand(newGovernorStopCmd(bin))
	cmd.AddCommand(newGovernorStatusCmd(bin))

	return cmd
}
