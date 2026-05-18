// Package credentials exposes the `af creds` Cobra command surface.
//
// Today the only subcommand is `af creds setup` — an interactive wizard
// that walks an operator through setting up standalone-mode credentials
// (optional 1Password integration + a sample ${gitRoot}/.env.local).
// Additional subcommands (e.g. `af creds doctor`, `af creds list`) can
// be wired into NewCmd in future revisions without breaking the parent
// surface.
package credentials

import "github.com/spf13/cobra"

// NewCmd returns the parent `creds` cobra.Command with every credentials
// subcommand pre-registered. The embedder (cmd/af/main.go via
// afcli.RegisterCommands) calls this once and adds the returned command
// to the root.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "creds",
		Short: "Manage standalone-mode credentials for af",
		Long: "Manage the credentials af forwards to spawned agents when running\n" +
			"outside of rensei-tui (no daemon credential pipeline, no platform\n" +
			"session). Subcommands help bootstrap a .env.local file and (optionally)\n" +
			"the 1Password CLI integration.",
		SilenceUsage: true,
	}
	cmd.AddCommand(newSetupCmd())
	cmd.AddCommand(newRotateCmd())
	return cmd
}
