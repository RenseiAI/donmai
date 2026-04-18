package afcli

import (
	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/spf13/cobra"
)

// newGovernorCmd constructs the `governor` parent command group.
// It holds no logic of its own; it dispatches to start/stop/status
// subcommands that manage the governor scan-loop process.
func newGovernorCmd(_ func() afclient.DataSource) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "governor",
		Short:        "Manage the AgentFactory governor (scan loop)",
		Long:         "Manage the AgentFactory governor process that scans Linear issues and dispatches work to the agent queue.",
		SilenceUsage: true,
	}

	cmd.AddCommand(newGovernorStartCmd())
	cmd.AddCommand(newGovernorStopCmd())
	cmd.AddCommand(newGovernorStatusCmd())

	return cmd
}
