package afcli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// newAgentCmd constructs the `agent` parent command. It holds no
// logic of its own; it dispatches to subcommands such as `list`.
// projectFunc is optional; when non-nil and returning a non-empty
// value, `list` scopes results to that project.
func newAgentCmd(ds func() afclient.DataSource, projectFunc func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "agent",
		Short:        "Inspect and control agent sessions",
		Long:         "Inspect and control AgentFactory agent sessions. Use subcommands like `list` to query sessions.",
		SilenceUsage: true,
	}

	cmd.AddCommand(newAgentListCmd(ds, projectFunc))
	cmd.AddCommand(newAgentStatusCmd(ds))
	cmd.AddCommand(newAgentStopCmd(ds))
	cmd.AddCommand(newAgentChatCmd(ds))
	cmd.AddCommand(newAgentReconnectCmd(ds))

	return cmd
}

// newAgentListCmd constructs the `agent list` subcommand. It filters
// sessions from DataSource.GetSessions() and renders them as either a
// human-readable table (default) or indented JSON (--json). The --all
// flag disables the active-only filter. When projectFunc is non-nil and
// returns a non-empty value, the list is scoped to that project via
// GetSessionsFiltered; otherwise the fleet-wide GetSessions is used.
// The --sandbox flag filters results to a specific sandbox provider ID.
func newAgentListCmd(ds func() afclient.DataSource, projectFunc func() string) *cobra.Command {
	var (
		allMode   bool
		jsonMode  bool
		sandboxID string
	)

	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List agent sessions",
		Long:         "List agent sessions. Defaults to active (queued, parked, working); use --all to include completed, failed, and stopped.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := ds()

			project := ""
			if projectFunc != nil {
				project = projectFunc()
			}

			var (
				resp *afclient.SessionsListResponse
				err  error
			)
			if project != "" {
				resp, err = client.GetSessionsFiltered(project)
			} else {
				resp, err = client.GetSessions()
			}
			if err != nil {
				return fmt.Errorf("get sessions: %w", err)
			}

			filtered := filterSessions(resp.Sessions, allMode)
			if sandboxID != "" {
				filtered = filterSessionsBySandbox(filtered, sandboxID)
			}

			out := cmd.OutOrStdout()

			if jsonMode {
				payload := afclient.SessionsListResponse{
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
	cmd.Flags().StringVar(&sandboxID, "sandbox", "", "Filter by sandbox provider ID")

	return cmd
}

// isActive reports whether a session status is considered active —
// i.e., queued, parked, or working. Terminal states (completed, failed,
// stopped) are not active.
func isActive(status afclient.SessionStatus) bool {
	switch status {
	case afclient.StatusQueued, afclient.StatusParked, afclient.StatusWorking:
		return true
	case afclient.StatusCompleted, afclient.StatusFailed, afclient.StatusStopped:
		return false
	}
	return false
}

// filterSessions returns sessions filtered by the active predicate
// unless all is true, in which case the slice is returned unchanged.
func filterSessions(sessions []afclient.SessionResponse, all bool) []afclient.SessionResponse {
	if all {
		return sessions
	}
	out := make([]afclient.SessionResponse, 0, len(sessions))
	for _, s := range sessions {
		if isActive(s.Status) {
			out = append(out, s)
		}
	}
	return out
}

// filterSessionsBySandbox returns sessions with a matching sandbox provider ID.
// Sessions with nil provider are skipped.
func filterSessionsBySandbox(sessions []afclient.SessionResponse, providerID string) []afclient.SessionResponse {
	out := make([]afclient.SessionResponse, 0, len(sessions))
	for _, s := range sessions {
		if s.Provider != nil && *s.Provider == providerID {
			out = append(out, s)
		}
	}
	return out
}

// writeSessionTable renders a tab-aligned session table to w with the
// columns: SESSION ID, IDENTIFIER, STATUS, DURATION, WORK TYPE.
func writeSessionTable(w io.Writer, sessions []afclient.SessionResponse) error {
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
