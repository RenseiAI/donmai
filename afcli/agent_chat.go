package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
)

// newAgentChatCmd constructs `agent chat <session-id> <message>` (also
// surfaced as `session prompt <session-id> <message>`). It delivers the
// message to a running session via the public sessions prompt endpoint
// (POST /api/public/sessions/:id/prompt) and prints a one-line
// confirmation, or the raw response as indented JSON when --json is set.
//
// The <session-id> argument is the 16-hex public session id — the same id
// `session show` and `session stop` accept and that the SESSION ID column
// of `session list` prints.
func newAgentChatCmd(ds func() afclient.DataSource) *cobra.Command {
	var jsonMode bool

	cmd := &cobra.Command{
		Use:          "chat <session-id> <message>",
		Short:        "Send a prompt to a running agent session",
		Long:         "Send a prompt to a running Donmai session via the public sessions prompt endpoint (POST /api/public/sessions/:id/prompt). The <session-id> is the public session id shown by `session list`/`session show`.",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			message := args[1]
			if strings.TrimSpace(message) == "" {
				return errors.New("message must not be empty")
			}

			client := ds()

			resp, err := client.ChatSession(id, afclient.ChatSessionRequest{Prompt: message})
			if err != nil {
				if errors.Is(err, afclient.ErrNotFound) {
					return fmt.Errorf("prompt session %s: session not found: %w", id, err)
				}
				return fmt.Errorf("prompt session %s: %w", id, err)
			}

			out := cmd.OutOrStdout()
			if jsonMode {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(resp); err != nil {
					return fmt.Errorf("encode prompt response: %w", err)
				}
				return nil
			}

			verb := "delivered"
			if !resp.Delivered {
				verb = "queued"
			}
			_, _ = fmt.Fprintf(out, "Prompt %s %s to %s (status: %s)\n",
				resp.PromptID, verb, resp.SessionID, resp.SessionStatus)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output raw JSON (indented)")

	return cmd
}
