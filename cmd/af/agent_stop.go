package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/internal/api"
)

// newAgentStopCmd constructs the `af agent stop <session-id>` subcommand.
// It posts to /api/public/sessions/:id/stop via DataSource.StopSession and
// renders either a one-line confirmation (default) or indented JSON (--json).
func newAgentStopCmd(flags *rootFlags) *cobra.Command {
	var jsonMode bool

	cmd := &cobra.Command{
		Use:          "stop <session-id>",
		Short:        "Stop a running agent session",
		Long:         "Stop a running agent session by ID. The coordinator transitions the session to the stopped state and returns the status transition.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			var ds api.DataSource
			if flags.mock {
				ds = api.NewMockClient()
			} else {
				ds = api.NewClient(flags.url)
			}

			resp, err := ds.StopSession(id)
			if err != nil {
				if errors.Is(err, api.ErrNotFound) {
					return fmt.Errorf("stop agent %s: session not found: %w", id, err)
				}
				return fmt.Errorf("stop agent %s: %w", id, err)
			}

			out := cmd.OutOrStdout()

			if jsonMode {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(resp); err != nil {
					return fmt.Errorf("encode stop response: %w", err)
				}
				return nil
			}

			_, _ = fmt.Fprintf(out, "Stopped %s (%s → %s)\n", resp.SessionID, resp.PreviousStatus, resp.NewStatus)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output raw JSON (indented)")

	return cmd
}
