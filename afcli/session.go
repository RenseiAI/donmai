package afcli

import (
	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// newSessionCmd constructs the `session` parent command, a sibling
// top-level group to `agent` that exposes the same behavior under
// session-oriented names. It reuses the existing agent factory
// functions and only overrides the returned cobra.Command's Use,
// Short, and Long fields so behavior, flags, and RunE stay
// authoritative in one place.
//
// projectFunc mirrors newAgentCmd and scopes `session list` results to
// a project when non-nil and returning a non-empty value.
func newSessionCmd(ds func() afclient.DataSource, projectFunc func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "session",
		Short:        "Inspect and control agent sessions",
		Long:         "Inspect and control AgentFactory agent sessions. Use subcommands like `list` to query sessions.",
		SilenceUsage: true,
	}

	listSub := newAgentListCmd(ds, projectFunc)
	listSub.Use = "list"
	listSub.Short = "List agent sessions"
	listSub.Long = "List agent sessions. Defaults to active (queued, parked, working); use --all to include completed, failed, and stopped."

	showSub := newAgentStatusCmd(ds)
	showSub.Use = "show <session-id>"
	showSub.Short = "Show detailed status for an agent session"
	showSub.Long = "Show detailed status for a single agent session, including duration, token usage, cost, and current activity."

	stopSub := newAgentStopCmd(ds)
	stopSub.Use = "stop <session-id>"
	stopSub.Short = "Stop a running agent session"
	stopSub.Long = "Stop a running agent session by ID. The coordinator transitions the session to the stopped state and returns the status transition."

	promptSub := newAgentChatCmd(ds)
	promptSub.Use = "prompt <session-id> <message>"
	promptSub.Short = "Forward a prompt to a running agent session"
	promptSub.Long = "Forward a prompt message to a running AgentFactory session via the coordinator forward-prompt API."

	cmd.AddCommand(listSub)
	cmd.AddCommand(showSub)
	cmd.AddCommand(stopSub)
	cmd.AddCommand(promptSub)
	cmd.AddCommand(newSessionStreamCmd(ds))

	return cmd
}
