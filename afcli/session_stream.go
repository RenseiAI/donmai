package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// newSessionStreamCmd constructs the `session stream <session-id>` subcommand.
// It tails live activity for a running session by polling the coordinator's
// GetActivities endpoint every --interval (default 2s). The initial poll
// establishes a cursor and, unless --follow-only is set, prints the returned
// historical activities. Subsequent polls use the cursor returned by the
// server (or the last event's ID as a fallback) so events are never reprinted.
//
// The loop exits cleanly when the session reaches a terminal status AND the
// response contained no new activities (so any pending events have been
// flushed), or when the command's context is cancelled (e.g., SIGINT). With
// --json, each activity is emitted as a standalone JSON object on its own
// line (ndjson) for pipe-friendly consumption.
func newSessionStreamCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		interval   time.Duration
		followOnly bool
		jsonMode   bool
	)

	cmd := &cobra.Command{
		Use:          "stream <session-id>",
		Short:        "Stream live activity for an agent session",
		Long:         "Stream live activity events for an agent session by polling the coordinator. Exits when the session reaches a terminal state (completed, failed, stopped) or on SIGINT.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			client := ds()
			out := cmd.OutOrStdout()

			// First poll — nil cursor establishes the initial baseline.
			resp, err := client.GetActivities(id, nil)
			if err != nil {
				if errors.Is(err, afclient.ErrNotFound) {
					return fmt.Errorf("session %s: %w", id, afclient.ErrNotFound)
				}
				return fmt.Errorf("get activities: %w", err)
			}

			// Print historical events unless --follow-only was passed.
			if !followOnly {
				if err := writeActivities(out, resp.Activities, jsonMode); err != nil {
					return fmt.Errorf("write activities: %w", err)
				}
			}

			cursor := advanceCursor("", resp)

			// Terminal with zero events — nothing to tail, exit immediately.
			if isTerminal(resp.SessionStatus) && len(resp.Activities) == 0 {
				return nil
			}

			for {
				// Context cancelled during or after the previous poll.
				if ctx.Err() != nil {
					return nil
				}

				// Sleep first — between polls, not before the first one.
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(interval):
				}

				if ctx.Err() != nil {
					return nil
				}

				var cursorArg *string
				if cursor != "" {
					c := cursor
					cursorArg = &c
				}

				resp, err := client.GetActivities(id, cursorArg)
				if err != nil {
					if errors.Is(err, afclient.ErrNotFound) {
						return fmt.Errorf("session %s: %w", id, afclient.ErrNotFound)
					}
					return fmt.Errorf("get activities: %w", err)
				}

				if err := writeActivities(out, resp.Activities, jsonMode); err != nil {
					return fmt.Errorf("write activities: %w", err)
				}

				cursor = advanceCursor(cursor, resp)

				if isTerminal(resp.SessionStatus) && len(resp.Activities) == 0 {
					return nil
				}
			}
		},
	}

	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Polling interval between activity fetches")
	cmd.Flags().BoolVar(&followOnly, "follow-only", false, "Skip historical activities and only print events after the command starts")
	cmd.Flags().BoolVar(&jsonMode, "json", false, "Emit one JSON object per activity per line (ndjson)")

	return cmd
}

// advanceCursor picks the new cursor value for the next poll, preferring the
// server-supplied Cursor field when non-nil, then falling back to the last
// activity's ID, and finally leaving the existing cursor unchanged when the
// response contains no new information.
func advanceCursor(current string, resp *afclient.ActivityListResponse) string {
	if resp == nil {
		return current
	}
	if resp.Cursor != nil && *resp.Cursor != "" {
		return *resp.Cursor
	}
	if len(resp.Activities) > 0 {
		return resp.Activities[len(resp.Activities)-1].ID
	}
	return current
}

// isTerminal reports whether the given status is a terminal session state.
func isTerminal(status afclient.SessionStatus) bool {
	switch status {
	case afclient.StatusCompleted, afclient.StatusFailed, afclient.StatusStopped:
		return true
	}
	return false
}

// writeActivities renders events to w using the configured output format.
// In jsonMode each event is written as a single-line JSON object (ndjson);
// otherwise each event is rendered as "{timestamp}  [{type}] {content}".
func writeActivities(w io.Writer, events []afclient.ActivityEvent, jsonMode bool) error {
	if jsonMode {
		enc := json.NewEncoder(w)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return fmt.Errorf("encode activity: %w", err)
			}
		}
		return nil
	}
	for _, e := range events {
		if _, err := fmt.Fprintf(w, "%s  [%s] %s\n", e.Timestamp, string(e.Type), e.Content); err != nil {
			return fmt.Errorf("write activity: %w", err)
		}
	}
	return nil
}
