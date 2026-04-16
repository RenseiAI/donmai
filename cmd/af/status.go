package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/internal/api"
	"github.com/RenseiAI/agentfactory-tui/internal/inline"
)

// newStatusCmd constructs the `af status` subcommand. It reads the
// persistent `--mock`/`--url` values from the parent rootFlags struct
// rather than consulting the environment or flag lookup, so tests can
// build fresh commands without global state.
func newStatusCmd(flags *rootFlags) *cobra.Command {
	var (
		jsonMode bool
		watch    bool
		interval string
	)

	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show a one-line fleet summary",
		Long:         "Print a concise fleet status summary. Use --json for raw stats, --watch for auto-refresh.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			var ds api.DataSource
			if flags.mock {
				ds = api.NewMockClient()
			} else {
				ds = api.NewClient(flags.url)
			}

			// JSON mode (non-watch): fetch and print stats as indented JSON.
			if jsonMode && !watch {
				stats, err := ds.GetStats()
				if err != nil {
					return fmt.Errorf("get stats: %w", err)
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(stats); err != nil {
					return fmt.Errorf("encode stats: %w", err)
				}
				return nil
			}

			// Watch mode (with or without --json).
			if watch {
				dur, err := time.ParseDuration(interval)
				if err != nil {
					return fmt.Errorf("invalid interval %q: %w", interval, err)
				}
				if err := inline.RunWatch(ds, inline.WatchConfig{
					Interval: dur,
					JSON:     jsonMode,
				}); err != nil {
					return fmt.Errorf("run watch: %w", err)
				}
				return nil
			}

			// Default: one-line human-readable summary.
			if err := inline.PrintStatus(ds); err != nil {
				return fmt.Errorf("print status: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output raw JSON stats")
	cmd.Flags().BoolVar(&watch, "watch", false, "Auto-refresh mode")
	cmd.Flags().StringVar(&interval, "interval", "3s", "Watch refresh interval")

	return cmd
}
