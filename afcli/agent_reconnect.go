package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// newAgentReconnectCmd constructs the `agent reconnect <session-id>` subcommand.
// It posts optional resume hints to the coordinator and renders either a short
// human-readable confirmation or the raw reconnect response as indented JSON.
func newAgentReconnectCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		jsonMode    bool
		cursor      string
		lastEventID string
	)

	cmd := &cobra.Command{
		Use:          "reconnect <session-id>",
		Short:        "Reconnect to an orphaned agent session",
		Long:         "Reconnect to an orphaned agent session by ID. Optional cursor and last-event-id resume hints can be supplied to report how many activity events were missed.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if strings.TrimSpace(id) == "" {
				return errors.New("session id must not be empty")
			}

			req := afclient.ReconnectSessionRequest{}
			if cursor != "" {
				req.Cursor = &cursor
			}
			if lastEventID != "" {
				req.LastEventID = &lastEventID
			}

			resp, err := ds().ReconnectSession(id, req)
			if err != nil {
				return fmt.Errorf("reconnect session %s: %w", id, err)
			}
			if !resp.Reconnected {
				return fmt.Errorf("reconnect declined for %s", id)
			}

			out := cmd.OutOrStdout()
			if jsonMode {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(resp); err != nil {
					return fmt.Errorf("encode reconnect response: %w", err)
				}
				return nil
			}

			_, _ = fmt.Fprintf(out, "reconnected to %s (status: %s, missed: %d %s)\n",
				resp.SessionID, resp.SessionStatus, resp.MissedEvents, missedEventsNoun(resp.MissedEvents))
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output raw JSON (indented)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Resume from this activity cursor")
	cmd.Flags().StringVar(&lastEventID, "last-event-id", "", "Resume after this activity event ID")

	return cmd
}

func missedEventsNoun(n int) string {
	if n == 1 {
		return "event"
	}
	return "events"
}
