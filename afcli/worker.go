package afcli

import (
	"github.com/RenseiAI/donmai/afclient"
	"github.com/spf13/cobra"
)

// newWorkerCmd constructs the `worker` parent command group. It holds no
// logic of its own; it dispatches to subcommands that run a single
// foreground worker process. The ds parameter is accepted for signature
// consistency with newAgentCmd but is unused because the worker package
// owns its own HTTP client rather than using afclient.
//
// Deprecated: legacy single-workspace process supervision, superseded by
// `host` (ADR-2026-08-03-cli-noun-tree-fleet-retirement.md D3). See
// legacyWorkerFleetRemovalVersion (fleet.go) for the removal release.
func newWorkerCmd(_ func() afclient.DataSource, cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run a worker process",
		Long: "Register with the coordinator, poll for work, and heartbeat. Intended as a\n" +
			"single foreground worker process; use `" + bin + " fleet` for multi-process\n" +
			"supervision.\n\n" +
			"Deprecated: superseded by `" + bin + " host`, which supervises agent\n" +
			"sessions via the persistent local daemon instead of a hand-managed\n" +
			"foreground process. This command is removed in " + legacyWorkerFleetRemovalVersion + ".",
		Deprecated: "use `" + bin + " host` instead; `worker` is removed in " +
			legacyWorkerFleetRemovalVersion + ".",
		SilenceUsage: true,
	}

	cmd.AddCommand(newWorkerStartCmd())

	return cmd
}
