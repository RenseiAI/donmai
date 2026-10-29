package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
)

// emDash is the placeholder rendered for nil pointer fields in human output.
const emDash = "-"

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

// writeSessionDetail renders the detail block to w using tabwriter
// for column alignment. Nil pointer fields on SessionDetail render as "-".
// Terminal diagnostics (reason, budget breach, pull request, summary)
// render as "unavailable" when absent so a missing diagnostic never
// reads as a successful outcome; the Status row always carries the
// server's status verbatim.
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
		{"Terminal Reason:", terminalReasonValue(s.TerminalReason)},
		{"Budget Breach:", budgetBreachValue(s.BudgetBreach)},
		{"Pull Request:", pullRequestValue(s.PullRequestURL, s.PullRequest)},
		{"Summary:", summaryValue(s.Summary)},
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

// intPtrValue renders a *int as a decimal string, or "-" when nil.
func intPtrValue(v *int) string {
	if v == nil {
		return emDash
	}
	return strconv.Itoa(*v)
}

// costValue renders a *float64 USD cost as "$1.2345", or "-" when nil.
func costValue(v *float64) string {
	if v == nil {
		return emDash
	}
	return fmt.Sprintf("$%.4f", *v)
}

// activityValue renders an activity as "Type: Content", or "-" when nil.
func activityValue(a *afclient.ActivityEvent) string {
	if a == nil {
		return emDash
	}
	return fmt.Sprintf("%s: %s", string(a.Type), a.Content)
}

// terminalReasonValue renders a structured terminal reason as
// "code: message (phase/decidedBy)", or "unavailable" when absent.
func terminalReasonValue(r *afclient.TerminalReason) string {
	if r == nil {
		return "unavailable"
	}
	code := r.Code
	message := r.Message
	switch {
	case code != "" && message != "":
		base := fmt.Sprintf("%s: %s", code, message)
		return appendReasonContext(base, r.Phase, r.DecidedBy)
	case message != "":
		return appendReasonContext(message, r.Phase, r.DecidedBy)
	case code != "":
		return appendReasonContext(code, r.Phase, r.DecidedBy)
	default:
		return "unavailable"
	}
}

func appendReasonContext(base, phase, decidedBy string) string {
	switch {
	case phase != "" && decidedBy != "":
		return fmt.Sprintf("%s (%s/%s)", base, phase, decidedBy)
	case phase != "":
		return fmt.Sprintf("%s (%s)", base, phase)
	case decidedBy != "":
		return fmt.Sprintf("%s (%s)", base, decidedBy)
	default:
		return base
	}
}

// budgetBreachValue renders a budget breach as "cap: detail",
// or "unavailable" when absent.
func budgetBreachValue(b *afclient.SessionBudgetBreach) string {
	if b == nil {
		return "unavailable"
	}
	switch {
	case b.Cap != "" && b.Detail != "":
		return fmt.Sprintf("%s: %s", b.Cap, b.Detail)
	case b.Cap != "":
		return b.Cap
	case b.Detail != "":
		return b.Detail
	default:
		return "unavailable"
	}
}

// pullRequestValue renders the pull-request artifact, preferring the
// structured record and falling back to the flat URL,
// or "unavailable" when neither is present.
func pullRequestValue(flatURL *string, pr *afclient.SessionPullRequest) string {
	url := ""
	if flatURL != nil {
		url = *flatURL
	}
	number := 0
	hasNumber := false
	if pr != nil {
		if pr.URL != "" {
			url = pr.URL
		}
		if pr.Number != nil {
			number = *pr.Number
			hasNumber = true
		}
	}
	switch {
	case hasNumber && url != "":
		return fmt.Sprintf("#%d: %s", number, url)
	case hasNumber:
		return fmt.Sprintf("#%d", number)
	case url != "":
		return url
	default:
		return "unavailable"
	}
}

// summaryValue renders the session summary, or "unavailable" when absent.
func summaryValue(s *string) string {
	if s == nil {
		return "unavailable"
	}
	if trimmed := strings.TrimSpace(*s); trimmed != "" {
		return trimmed
	}
	return "unavailable"
}
