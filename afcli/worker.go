package afcli

import (
	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/spf13/cobra"
)

// newWorkerCmd constructs the `worker` parent command group. It holds no
// logic of its own; it dispatches to subcommands that run a single
// foreground worker process. The ds parameter is accepted for signature
// consistency with newAgentCmd but is unused because the worker package
// owns its own HTTP client rather than using afclient.
func newWorkerCmd(_ func() afclient.DataSource) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "worker",
		Short:        "Run an AgentFactory worker process",
		Long:         "Register with the coordinator, poll for work, and heartbeat. Intended as a single foreground worker process; use `af fleet` for multi-process supervision.",
		SilenceUsage: true,
	}

	cmd.AddCommand(newWorkerStartCmd())

	return cmd
}
