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
					if jsonMode {
						enc := json.NewEncoder(cmd.OutOrStdout())
						enc.SetIndent("", "  ")
						if encodeErr := enc.Encode(promptErr); encodeErr != nil {
							return fmt.Errorf("encode prompt refusal: %w", encodeErr)
						}
					} else {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), formatPromptRefusalLine(id, promptErr))
					}
					return fmt.Errorf("prompt session %s refused: %w", id, promptErr)
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
				_, _ = fmt.Fprintln(out, formatPromptReceiptCodeLine(resp))
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output raw JSON (indented)")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Stable key for safe prompt retries")

	return cmd
}

// formatPromptRefusalLine renders the one-line human refusal for a refused
// prompt: the session id, the refusal code, the reason and the recovery
// action. Prompt text and retry keys are never echoed.
func formatPromptRefusalLine(id string, promptErr *afclient.PromptSessionError) string {
	reason := promptErr.Reason
	if reason == "" {
		reason = "prompt refused"
	}
	if promptErr.Code != "" {
		reason = "(" + promptErr.Code + "): " + reason
	}
	line := "prompt session " + id + " refused " + reason + "."
	if next := humanizePromptRecoveryAction(promptErr.RecoveryAction); next != "" {
		line += " Next step: " + next + "."
	}
	if len(promptErr.MissingFields) > 0 {
		line += " missing: " + strings.Join(promptErr.MissingFields, ", ")
	}
	return line
}

// humanizePromptRecoveryAction renders the wire recoveryAction value in
// human prose. Underscore names print with spaces (start_new_session reads
// as "start a new session" via the article fixup below); unknown values
// pass through unchanged.
func humanizePromptRecoveryAction(action string) string {
	if action == "" {
		return ""
	}
	human := strings.ReplaceAll(action, "_", " ")
	if human == "start new session" {
		return "start a new session"
	}
	return human
}

// formatPromptReceiptCodeLine renders the follow-up line for a 200 receipt
// that still carries a refusal code and recovery hint.
func formatPromptReceiptCodeLine(resp *afclient.ChatSessionResponse) string {
	line := "Refusal code " + resp.Code
	if next := humanizePromptRecoveryAction(resp.RecoveryAction); next != "" {
		line += ": next step " + next
	}
	return line
}
