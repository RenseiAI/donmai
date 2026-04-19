package afcli

import (
	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/spf13/cobra"
)

// newFleetCmd constructs the `fleet` parent command group. It holds no
// logic of its own; it dispatches to start/stop/status/scale subcommands
// that spawn and supervise multiple `af worker` child processes.
//
// The ds parameter is accepted for signature consistency with newAgentCmd
// but is unused because fleet subcommands work by spawning and signaling
// local processes, not by calling the coordinator API.
func newFleetCmd(_ func() afclient.DataSource) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "fleet",
		Short:        "Manage a fleet of worker processes",
		Long:         "Spawn, scale, and supervise multiple af worker processes.",
		SilenceUsage: true,
	}

	cmd.AddCommand(newFleetStartCmd())
	cmd.AddCommand(newFleetStopCmd())
	cmd.AddCommand(newFleetStatusCmd())
	cmd.AddCommand(newFleetScaleCmd())

	return cmd
}
