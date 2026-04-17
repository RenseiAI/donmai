package main

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/internal/api"
)

// newAgentCmd constructs the `af agent` parent command. It holds no
// logic of its own; it dispatches to subcommands such as `list`.
// Follows the same factory pattern as newStatusCmd so tests can build
// fresh instances without global state.
func newAgentCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "agent",
		Short:        "Inspect and control agent sessions",
		Long:         "Inspect and control AgentFactory agent sessions. Use subcommands like `list` to query sessions.",
		SilenceUsage: true,
	}

	cmd.AddCommand(newAgentListCmd(flags))
	cmd.AddCommand(newAgentStopCmd(flags))

	return cmd
}

// newAgentListCmd constructs the `af agent list` subcommand. It filters
// sessions from DataSource.GetSessions() and renders them as either a
// human-readable table (default) or indented JSON (--json). The --all
// flag disables the active-only filter.
func newAgentListCmd(flags *rootFlags) *cobra.Command {
	var (
		allMode  bool
		jsonMode bool
	)

	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List agent sessions",
		Long:         "List agent sessions. Defaults to active (queued, parked, working); use --all to include completed, failed, and stopped.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ds api.DataSource
			if flags.mock {
				ds = api.NewMockClient()
			} else {
				ds = api.NewClient(flags.url)
			}

			resp, err := ds.GetSessions()
			if err != nil {
				return fmt.Errorf("get sessions: %w", err)
			}

			filtered := filterSessions(resp.Sessions, allMode)

			out := cmd.OutOrStdout()

			if jsonMode {
				payload := api.SessionsListResponse{
					Sessions:  filtered,
					Count:     len(filtered),
					Timestamp: resp.Timestamp,
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(payload); err != nil {
					return fmt.Errorf("encode sessions: %w", err)
				}
				return nil
			}

			if len(filtered) == 0 {
				if allMode {
					_, _ = fmt.Fprintln(out, "No sessions.")
				} else {
					_, _ = fmt.Fprintln(out, "No active sessions.")
				}
				return nil
			}

			return writeSessionTable(out, filtered)
		},
	}

	cmd.Flags().BoolVar(&allMode, "all", false, "Include completed, failed, and stopped sessions")
	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output raw JSON (indented)")

	return cmd
}

// isActive reports whether a session status is considered active —
// i.e., queued, parked, or working. Terminal states (completed, failed,
// stopped) are not active.
func isActive(status api.SessionStatus) bool {
	switch status {
	case api.StatusQueued, api.StatusParked, api.StatusWorking:
		return true
	case api.StatusCompleted, api.StatusFailed, api.StatusStopped:
		return false
	}
	return false
}

// filterSessions returns sessions filtered by the active predicate
// unless all is true, in which case the slice is returned unchanged.
func filterSessions(sessions []api.SessionResponse, all bool) []api.SessionResponse {
	if all {
		return sessions
	}
	out := make([]api.SessionResponse, 0, len(sessions))
	for _, s := range sessions {
		if isActive(s.Status) {
			out = append(out, s)
		}
	}
	return out
}

// writeSessionTable renders a tab-aligned session table to w with the
// columns: SESSION ID, IDENTIFIER, STATUS, DURATION, WORK TYPE.
func writeSessionTable(w io.Writer, sessions []api.SessionResponse) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "SESSION ID\tIDENTIFIER\tSTATUS\tDURATION\tWORK TYPE"); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, s := range sessions {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.Identifier, string(s.Status), formatDuration(s.Duration), s.WorkType,
		); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush table: %w", err)
	}
	return nil
}

// formatDuration renders a duration in seconds as a compact h/m/s string.
// Examples: 45 -> "45s", 125 -> "2m5s", 3725 -> "1h2m5s", 0 -> "0s".
func formatDuration(seconds int) string {
	if seconds <= 0 {
		return "0s"
	}
	d := time.Duration(seconds) * time.Second
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)

	switch {
	case h > 0 && m > 0 && s > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0 && s > 0:
		return fmt.Sprintf("%dh%ds", h, s)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case m > 0 && s > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
