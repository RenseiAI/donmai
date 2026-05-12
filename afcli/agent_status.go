package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// emDash is the placeholder rendered for nil pointer fields in human output.
const emDash = "—"

// agentStatusJSON is the combined payload emitted by `agent status --json`.
// Activity is a pointer so it is omitted entirely when the session has no
// activity events, matching the spec's `omitempty` requirement.
type agentStatusJSON struct {
	Session  afclient.SessionDetail  `json:"session"`
	Activity *afclient.ActivityEvent `json:"currentActivity,omitempty"`
}

// newAgentStatusCmd constructs the `agent status <session-id>` subcommand.
// It fetches the session detail and latest activity via DataSource and renders
// either an aligned eight-row block (default) or indented JSON (--json).
func newAgentStatusCmd(ds func() afclient.DataSource) *cobra.Command {
	var jsonMode bool

	cmd := &cobra.Command{
		Use:   "status <session-id>",
		Short: "Show detailed status for an agent session",
		Long:  "Show detailed status for a single agent session, including duration, token usage, cost, and current activity.",
		// Custom Args validator instead of cobra.ExactArgs(1) so the bare-
		// command error tells operators where to find a session id rather
		// than the default "accepts 1 arg(s), received 0" which is opaque
		// to anyone who doesn't already know about `agent list`. Keep the
		// "too many args" path delegating to ExactArgs(1) for parity.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf(
					"agent status needs a session id\n\n" +
						"List active sessions to find one:\n" +
						"  agent list\n\n" +
						"Then re-run with the id as the first argument:\n" +
						"  agent status <session-id>",
				)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			client := ds()

			detail, err := client.GetSessionDetail(id)
			if err != nil {
				if errors.Is(err, afclient.ErrNotFound) {
					return fmt.Errorf("session %s: %w", id, afclient.ErrNotFound)
				}
				return fmt.Errorf("get session detail: %w", err)
			}

			activities, err := client.GetActivities(id, nil)
			if err != nil {
				return fmt.Errorf("get activities: %w", err)
			}
			current := latestActivity(activities.Activities)

			out := cmd.OutOrStdout()

			if jsonMode {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(agentStatusJSON{
					Session:  detail.Session,
					Activity: current,
				}); err != nil {
					return fmt.Errorf("encode status: %w", err)
				}
				return nil
			}

			return writeSessionDetail(out, detail.Session, current)
		},
	}

	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output raw JSON (indented)")

	return cmd
}

// latestActivity returns a pointer to the last element of events, or nil when
// the slice is empty. Mock.GetActivities yields events in chronological order,
// so the last element is the most recent.
func latestActivity(events []afclient.ActivityEvent) *afclient.ActivityEvent {
	if len(events) == 0 {
		return nil
	}
	return &events[len(events)-1]
}

// writeSessionDetail renders the eight-row detail block to w using tabwriter
// for column alignment. Nil pointer fields on SessionDetail render as em-dash.
func writeSessionDetail(w io.Writer, s afclient.SessionDetail, current *afclient.ActivityEvent) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	rows := [][2]string{
		{"Session:", s.ID},
		{"Identifier:", s.Identifier},
		{"Status:", string(s.Status)},
		{"Duration:", formatDuration(s.Duration)},
		{"Input Tokens:", intPtrValue(s.InputTokens)},
		{"Output Tokens:", intPtrValue(s.OutputTokens)},
		{"Cost (USD):", costValue(s.CostUsd)},
		{"Current Activity:", activityValue(current)},
	}

	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1]); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush detail: %w", err)
	}
	return nil
}

// intPtrValue renders a *int as a decimal string, or em-dash when nil.
func intPtrValue(v *int) string {
	if v == nil {
		return emDash
	}
	return strconv.Itoa(*v)
}

// costValue renders a *float64 USD cost as "$1.2345", or em-dash when nil.
func costValue(v *float64) string {
	if v == nil {
		return emDash
	}
	return fmt.Sprintf("$%.4f", *v)
}

// activityValue renders an activity as "Type — Content", or em-dash when nil.
func activityValue(a *afclient.ActivityEvent) string {
	if a == nil {
		return emDash
	}
	return fmt.Sprintf("%s — %s", string(a.Type), a.Content)
}
