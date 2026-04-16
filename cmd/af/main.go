// Package main is the unified AgentFactory CLI/TUI entry point.
//
// The bare `af` command launches the Bubble Tea dashboard. Subcommands
// (status, agent, governor, worker, fleet, queue, ...) will be attached
// via Cobra in follow-up issues.
package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/internal/api"
	"github.com/RenseiAI/agentfactory-tui/internal/app"
)

const defaultBaseURL = "http://localhost:3000"

// resolveDefaultURL returns the value of WORKER_API_URL when set,
// otherwise the hard-coded default base URL.
func resolveDefaultURL() string {
	if u := os.Getenv("WORKER_API_URL"); u != "" {
		return u
	}
	return defaultBaseURL
}

// buildContext constructs an *app.Context with the appropriate DataSource
// based on the mock flag. Extracted from RunE so tests can exercise it
// without launching Bubble Tea.
func buildContext(mock bool, url string) *app.Context {
	var ds api.DataSource
	if mock {
		ds = api.NewMockClient()
	} else {
		ds = api.NewClient(url)
	}
	return &app.Context{
		DataSource: ds,
		BaseURL:    url,
		UseMock:    mock,
	}
}

// rootFlags holds the persistent flag values bound to the root command.
// Returned alongside the command so tests can inspect resolved values
// after PersistentPreRunE runs.
type rootFlags struct {
	mock bool
	url  string
}

// newRootCmd constructs the root `af` Cobra command with persistent
// flags and dotenv-loading PersistentPreRunE. It is a factory so tests
// can build fresh commands without global state.
func newRootCmd() (*cobra.Command, *rootFlags) {
	flags := &rootFlags{}

	cmd := &cobra.Command{
		Use:   "af",
		Short: "AgentFactory terminal dashboard and CLI",
		Long: "af is the unified entry point for the AgentFactory TUI and CLI.\n\n" +
			"Running `af` with no subcommand launches the Bubble Tea dashboard.",
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			// Load dotenv files; failure is non-fatal (files are optional).
			// Load each separately because godotenv.Load aborts on the
			// first missing file without loading the rest. Existing env
			// vars always win (godotenv never overrides).
			_ = godotenv.Load(".env.local")
			_ = godotenv.Load(".env")

			// Flag defaults resolve before PersistentPreRunE runs, so if
			// the user did not explicitly pass --url, re-read WORKER_API_URL
			// now that dotenv has populated the environment.
			if !cmd.Flags().Changed("url") {
				if u := os.Getenv("WORKER_API_URL"); u != "" {
					flags.url = u
				}
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx := buildContext(flags.mock, flags.url)
			model := app.New(ctx)
			p := tea.NewProgram(model)
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("run tui: %w", err)
			}
			return nil
		},
	}

	cmd.PersistentFlags().BoolVar(&flags.mock, "mock", false, "Use mock data instead of live API")
	cmd.PersistentFlags().StringVar(&flags.url, "url", resolveDefaultURL(), "AgentFactory server URL")

	return cmd, flags
}

func main() {
	cmd, _ := newRootCmd()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
