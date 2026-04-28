// Package afcli provides Cobra command factories for the AgentFactory CLI.
// Downstream projects can import this package and call
// RegisterCommands to add all af subcommands to their own root command.
package afcli

import (
	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/spf13/cobra"
)

// Config controls how af commands are wired into a parent CLI.
type Config struct {
	// ClientFactory returns an afclient.DataSource for API calls.
	// Required.
	ClientFactory func() afclient.DataSource

	// DefaultURLFunc is a lazy URL resolution function (for flag-based callers).
	// Optional. Checked before DefaultURL.
	DefaultURLFunc func() string

	// DefaultURL is the fallback API base URL if DefaultURLFunc is nil.
	DefaultURL string

	// EnableDashboard registers the dashboard command when true.
	EnableDashboard bool

	// ProjectFunc returns the active project slug (or ID) used to scope
	// list endpoints that support filtering. Returning an empty string
	// means fleet-wide (no scope), preserving the default behavior.
	// Optional. When nil, all commands behave fleet-wide.
	ProjectFunc func() string
}

// resolveURL returns the base URL to use, checking DefaultURLFunc first,
// then DefaultURL, then "http://localhost:3000".
func (c Config) resolveURL() string {
	if c.DefaultURLFunc != nil {
		if u := c.DefaultURLFunc(); u != "" {
			return u
		}
	}
	if c.DefaultURL != "" {
		return c.DefaultURL
	}
	return "http://localhost:3000"
}

// RegisterCommands adds all AgentFactory subcommands to the given root
// command. The commands use cfg to resolve API clients and defaults.
//
// This is the primary integration point for downstream CLIs that want
// to embed AgentFactory functionality under their own root command
// (e.g. `mycli dashboard`, `mycli agent list`, etc.).
func RegisterCommands(root *cobra.Command, cfg Config) {
	ds := cfg.ClientFactory
	root.AddCommand(newStatusCmd(ds))
	root.AddCommand(newAgentCmd(ds, cfg.ProjectFunc))
	root.AddCommand(newSessionCmd(ds, cfg.ProjectFunc))
	root.AddCommand(newGovernorCmd(ds))
	root.AddCommand(newWorkerCmd(ds))
	root.AddCommand(newFleetCmd(ds))
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newProjectCmd())
	if cfg.EnableDashboard {
		root.AddCommand(newDashboardCmd(cfg))
	}
}
