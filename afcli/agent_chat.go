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
	var idempotencyKey string

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

			resp, err := client.ChatSession(id, afclient.ChatSessionRequest{Prompt: message, IdempotencyKey: idempotencyKey})
			if err != nil {
				var promptErr *afclient.PromptSessionError
				if errors.As(err, &promptErr) {
					out := cmd.OutOrStdout()
					if jsonMode {
						enc := json.NewEncoder(out)
						enc.SetIndent("", "  ")
						if encodeErr := enc.Encode(promptErr); encodeErr != nil {
							return fmt.Errorf("encode prompt refusal: %w", encodeErr)
						}
						return fmt.Errorf("prompt session %s: %w", id, err)
					}
					line := fmt.Sprintf("prompt session %s refused (%s): %s", id, promptErr.Code, promptErr.Reason)
					if promptErr.RecoveryAction != "" {
						recovery := promptErr.RecoveryAction
						if !strings.HasSuffix(recovery, ".") && !strings.HasSuffix(recovery, "!") && !strings.HasSuffix(recovery, "?") {
							recovery += "."
						}
						if !strings.HasSuffix(promptErr.Reason, ".") && !strings.HasSuffix(promptErr.Reason, "!") && !strings.HasSuffix(promptErr.Reason, "?") {
							line += "."
						}
						line += fmt.Sprintf(" Next step: %s", recovery)
					}
					if len(promptErr.MissingFields) > 0 {
						line += fmt.Sprintf(" (missing: %s)", strings.Join(promptErr.MissingFields, ", "))
					}
					_, _ = fmt.Fprintln(out, line)
					return fmt.Errorf("prompt session %s: %w", id, err)
				}
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

			verb := resp.Disposition
			if verb == "" {
				verb = "queued"
			}
			if resp.Delivered {
				verb = "delivered"
			}
			_, _ = fmt.Fprintf(out, "Prompt %s %s to %s (status: %s)\n",
				resp.PromptID, verb, resp.SessionID, resp.SessionStatus)
			if resp.Code != "" {
				switch {
				case resp.RecoveryAction != "" && resp.Error != "":
					_, _ = fmt.Fprintf(out, "Code %s: %s Next step: %s\n", resp.Code, resp.Error, ensureTrailingPeriod(resp.RecoveryAction))
				case resp.RecoveryAction != "":
					_, _ = fmt.Fprintf(out, "Code %s Next step: %s\n", resp.Code, ensureTrailingPeriod(resp.RecoveryAction))
				case resp.Error != "":
					_, _ = fmt.Fprintf(out, "Code %s: %s\n", resp.Code, resp.Error)
				default:
					_, _ = fmt.Fprintf(out, "Code %s\n", resp.Code)
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output raw JSON (indented)")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Stable key for safe prompt retries")

	return cmd
}

func ensureTrailingPeriod(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return s
	}
	return s + "."
}
