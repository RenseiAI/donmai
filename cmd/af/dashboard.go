package main

import (
	"fmt"
	"io"
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/internal/app"
)

// newDashboardCmd constructs the `af dashboard` subcommand. It shares
// rootFlags for --mock/--url access, following the same pattern as
// newStatusCmd.
func newDashboardCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:          "dashboard",
		Short:        "Launch the TUI dashboard",
		Long:         "Launch the full-screen Bubble Tea TUI dashboard for fleet monitoring.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDashboard(flags)
		},
	}
}

// runDashboard launches the Bubble Tea TUI. Logging is suppressed while
// the TUI is running to avoid corrupting the terminal display.
func runDashboard(flags *rootFlags) error {
	ctx := buildContext(flags.mock, flags.url)

	// Suppress logging while Bubble Tea owns the terminal.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	model := app.New(ctx)
	p := tea.NewProgram(model)

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("run dashboard: %w", err)
	}
	return nil
}
