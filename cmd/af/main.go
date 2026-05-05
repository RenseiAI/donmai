// Package main is the unified AgentFactory CLI/TUI entry point.
//
// The bare `af` command launches the Bubble Tea dashboard when stdin is
// a TTY, or prints help otherwise. Subcommands (dashboard, status, ...)
// are attached via Cobra.
package main

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afcli"
	"github.com/RenseiAI/agentfactory-tui/afclient"
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

// buildDataSource constructs the appropriate DataSource from root flags.
// Used by the ClientFactory passed to afcli.RegisterCommands.
func buildDataSource(flags *rootFlags) afclient.DataSource {
	switch {
	case flags.mock:
		return afclient.NewMockClient()
	case flags.apiKey != "":
		return afclient.NewAuthenticatedClient(flags.url, flags.apiKey)
	default:
		return afclient.NewClient(flags.url)
	}
}

// resolveAPIKey returns the first valid API token found in the environment.
// It checks WORKER_API_KEY and RENSEI_API_TOKEN, preferring tokens with the
// rsk_ prefix (API auth tokens) over registration tokens (rsp_).
func resolveAPIKey() string {
	for _, env := range []string{"WORKER_API_KEY", "RENSEI_API_TOKEN"} {
		if v := os.Getenv(env); v != "" && strings.HasPrefix(v, "rsk_") {
			return v
		}
	}
	// Fall back to any non-empty value if no rsk_ token was found.
	for _, env := range []string{"WORKER_API_KEY", "RENSEI_API_TOKEN"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}

// stdinIsTerminal reports whether stdin is connected to a terminal.
// It is a variable so tests can override it.
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// rootFlags holds the persistent flag values bound to the root command.
// Returned alongside the command so tests can inspect resolved values
// after PersistentPreRunE runs.
type rootFlags struct {
	mock   bool
	url    string
	apiKey string
	debug  bool
	quiet  bool
}

// configureLogging sets the default slog logger based on --debug/--quiet flags.
func configureLogging(flags *rootFlags) {
	var level slog.Level
	var w io.Writer = os.Stderr

	switch {
	case flags.quiet:
		w = io.Discard
		level = slog.LevelError + 4 // effectively silent
	case flags.debug:
		level = slog.LevelDebug
	default:
		level = slog.LevelWarn
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
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

			if !cmd.Flags().Changed("api-key") {
				flags.apiKey = resolveAPIKey()
			}

			configureLogging(flags)
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Bare `af` in a TTY launches the dashboard; non-TTY shows help.
			if !stdinIsTerminal() {
				return cmd.Help()
			}
			// Delegate to the registered dashboard subcommand so the bare
			// `af` path and `af dashboard` share identical wiring. The
			// dashboard subcommand is registered below via
			// afcli.RegisterCommands with EnableDashboard: true.
			for _, sub := range cmd.Commands() {
				if sub.Name() == "dashboard" {
					return sub.RunE(sub, args)
				}
			}
			return cmd.Help()
		},
	}

	cmd.PersistentFlags().BoolVar(&flags.mock, "mock", false, "Use mock data instead of live API")
	cmd.PersistentFlags().StringVar(&flags.url, "url", resolveDefaultURL(), "AgentFactory server URL")
	cmd.PersistentFlags().StringVar(&flags.apiKey, "api-key", "", "API key for authenticated requests (env: WORKER_API_KEY)")
	cmd.PersistentFlags().BoolVar(&flags.debug, "debug", false, "Enable debug logging")
	cmd.PersistentFlags().BoolVar(&flags.quiet, "quiet", false, "Suppress all log output")

	afcli.RegisterCommands(cmd, afcli.Config{
		ClientFactory:           func() afclient.DataSource { return buildDataSource(flags) },
		DefaultURLFunc:          func() string { return flags.url },
		EnableDashboard:         true,
		EnableLegacyWorkerFleet: true,
	})

	return cmd, flags
}

func main() {
	cmd, _ := newRootCmd()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
