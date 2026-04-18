// Package afcli provides Cobra command factories for the AgentFactory CLI.
// Downstream projects (e.g. rensei-tui) can import this package and call
// RegisterCommands to add all af subcommands to their own root command.
package afcli

import (
	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/spf13/cobra"
)

// Config controls how af commands are wired into a parent CLI.
type Config struct {
	// ClientFactory returns an afclient.DataSource for API calls.
	// If nil, commands will build their own client from flags/env.
	ClientFactory func() afclient.DataSource

	// DefaultURL is the default API base URL. If empty, uses
	// http://localhost:3000 or WORKER_API_URL env var.
	DefaultURL string
}

// RegisterCommands adds all AgentFactory subcommands to the given root
// command. The commands use cfg to resolve API clients and defaults.
//
// This is the primary integration point for downstream CLIs that want
// to embed AgentFactory functionality (e.g. `rensei dashboard`,
// `rensei agent list`, etc.).
func RegisterCommands(root *cobra.Command, cfg Config) {
	// TODO: Wire command factories here as they are extracted from cmd/af/.
	// For now this is a stub that will be populated incrementally as
	// commands are refactored from cmd/af/ main package into afcli/.
	_ = root
	_ = cfg
}
