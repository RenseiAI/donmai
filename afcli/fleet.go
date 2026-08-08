package afcli

import (
	"github.com/RenseiAI/donmai/afclient"
	"github.com/spf13/cobra"
)

// legacyWorkerFleetRemovalVersion is the release in which the standalone OSS
// binary deletes the `worker` and `fleet` command trees outright.
//
// Timeline, per ADR-2026-08-03-cli-noun-tree-fleet-retirement.md D5.3: `host`
// (D2) already shipped as the replacement in v0.57.0, so the precondition
// "the replacement exists before the notice points at it" is already
// satisfied. This marks `worker`/`fleet` Deprecated (and deletes the
// permanent `fleet scale` stub, per D3) in the next OSS minor, v0.58.0 — the
// same release `hostAliasRemovalVersion` already deletes the `daemon` and
// `fleet-watch` aliases in. Per D5.3, `worker`/`fleet` themselves are removed
// "one OSS minor later," i.e. v0.59.0.
//
// Every alias/deprecated tree this package registers MUST name a concrete
// version here rather than "the next release" — see hostAliasRemovalVersion's
// comment in host.go for why: an unfalsifiable promise is how the previous
// alias generation survived dozens of tags past its stated window. This
// constant is declared on THIS repo's own release line so a future
// alias-removal gate built in this repo (see D5.4; none exists yet — see
// commands.go's EnableLegacyWorkerFleet doc comment) can evaluate it against
// donmai's own tags. A removal version borrowed from a downstream embedder's
// version line would never be evaluable by a gate running here.
const legacyWorkerFleetRemovalVersion = "v0.59.0"

// newFleetCmd constructs the `fleet` parent command group. It holds no
// logic of its own; it dispatches to start/stop/status subcommands that
// spawn and supervise multiple `donmai worker` child processes.
//
// Deprecated: legacy single-workspace process supervision, superseded by
// `host` (ADR-2026-08-03-cli-noun-tree-fleet-retirement.md D3). See
// legacyWorkerFleetRemovalVersion for the removal release.
//
// The ds parameter is accepted for signature consistency with newAgentCmd
// but is unused because fleet subcommands work by spawning and signaling
// local processes, not by calling the coordinator API.
func newFleetCmd(_ func() afclient.DataSource, cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Manage a fleet of worker processes",
		Long: "Spawn and supervise multiple `" + bin + " worker` processes.\n\n" +
			"Deprecated: superseded by `" + bin + " host`, which supervises agent\n" +
			"sessions via the persistent local daemon instead of hand-managed child\n" +
			"processes. This command is removed in " + legacyWorkerFleetRemovalVersion + ".",
		Deprecated: "use `" + bin + " host` instead; `fleet` is removed in " +
			legacyWorkerFleetRemovalVersion + ".",
		SilenceUsage: true,
	}

	cmd.AddCommand(newFleetStartCmd(bin))
	cmd.AddCommand(newFleetStopCmd())
	cmd.AddCommand(newFleetStatusCmd(bin))

	return cmd
}
