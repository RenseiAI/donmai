package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// newAgentChatCmd constructs `af agent chat <session-id> <message>`. It
// forwards the message to the coordinator's forward-prompt API and
// prints a one-line confirmation, or the raw response as indented JSON
// when --json is set.
func newAgentChatCmd(flags *rootFlags) *cobra.Command {
	var jsonMode bool

	cmd := &cobra.Command{
		Use:          "chat <session-id> <message>",
		Short:        "Forward a message to a running agent session",
		Long:         "Forward a chat message to a running AgentFactory session via the coordinator forward-prompt API.",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID := args[0]
			message := args[1]
			if strings.TrimSpace(message) == "" {
				return errors.New("message must not be empty")
			}

			ds := buildDataSource(flags)

			resp, err := ds.ForwardPrompt(afclient.ForwardPromptRequest{
				TaskID:  taskID,
				Message: message,
			})
			if err != nil {
				return fmt.Errorf("forward prompt: %w", err)
			}

			out := cmd.OutOrStdout()
			if jsonMode {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(resp); err != nil {
					return fmt.Errorf("encode forward-prompt response: %w", err)
				}
				return nil
			}

			_, _ = fmt.Fprintf(out, "forwarded prompt %s to %s (status: %s)\n",
				resp.PromptID, resp.TaskID, resp.SessionStatus)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output raw JSON (indented)")

	return cmd
}
