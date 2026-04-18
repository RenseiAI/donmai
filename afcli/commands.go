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
// to embed AgentFactory functionality (e.g. `rensei dashboard`,
// `rensei agent list`, etc.).
func RegisterCommands(root *cobra.Command, cfg Config) {
	ds := cfg.ClientFactory
	root.AddCommand(newStatusCmd(ds))
	root.AddCommand(newAgentCmd(ds))
	root.AddCommand(newGovernorCmd(ds))
	if cfg.EnableDashboard {
		root.AddCommand(newDashboardCmd(cfg))
	}
}
