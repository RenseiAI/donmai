package afcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/a2a"
	"github.com/spf13/cobra"
)

type a2aCommandOptions struct {
	cardURL        string
	peer           string
	bearerFile     string
	extensions     []string
	pollInterval   time.Duration
	cardURLChanged bool
}

func newA2ACmd(cfg Config) *cobra.Command {
	options := &a2aCommandOptions{}
	cmd := &cobra.Command{
		Use:          "a2a",
		Short:        "Call agents over the formal A2A v1 protocol",
		Long:         "Call a peer's formal A2A v1 JSON-RPC interface selected from an explicit Agent Card. See docs/A2A-CLIENT.md.",
		SilenceUsage: true,
	}
	cmd.PersistentFlags().StringVar(&options.cardURL, "card", "", "explicit Agent Card URL")
	cmd.PersistentFlags().StringVar(&options.peer, "peer", "", "embedder-owned peer reference resolved to an Agent Card URL")
	cmd.PersistentFlags().StringVar(&options.bearerFile, "bearer-token-file", "", "file containing a bearer token; reread for every request")
	cmd.PersistentFlags().StringSliceVar(&options.extensions, "extension", nil, "implemented extension URI to negotiate (repeatable)")
	cmd.PersistentFlags().DurationVar(&options.pollInterval, "poll-interval", time.Second, "task polling interval used by send --wait")
	cmd.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		options.cardURLChanged = cmd.Flags().Changed("card")
		if options.cardURLChanged && strings.TrimSpace(options.peer) != "" {
			return errors.New("a2a: --card and --peer are mutually exclusive")
		}
		return nil
	}
	cmd.AddCommand(newA2ASendCmd(cfg, options))
	cmd.AddCommand(newA2AGetCmd(cfg, options))
	cmd.AddCommand(newA2AListCmd(cfg, options))
	cmd.AddCommand(newA2ACancelCmd(cfg, options))
	return cmd
}

func newA2ASendCmd(cfg Config, options *a2aCommandOptions) *cobra.Command {
	var (
		message           string
		bodyFile          string
		messageID         string
		contextID         string
		taskID            string
		metadataJSON      string
		acceptedModes     []string
		historyLength     int
		returnImmediately bool
		wait              bool
		jsonOutput        bool
	)
	cmd := &cobra.Command{
		Use:   "send [text]",
		Short: "Send a formal A2A v1 Message",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text, err := resolveA2AMessageBody(cmd, message, bodyFile, args)
			if err != nil {
				return err
			}
			if messageID == "" {
				messageID, err = newA2AMessageID()
				if err != nil {
					return err
				}
			}
			metadata, err := decodeA2AMetadata(metadataJSON)
			if err != nil {
				return err
			}
			configuration := &a2a.SendMessageConfiguration{
				AcceptedOutputModes: acceptedModes,
				ReturnImmediately:   returnImmediately,
			}
			if historyLength >= 0 {
				value, err := int32Flag(historyLength, "history-length")
				if err != nil {
					return err
				}
				configuration.HistoryLength = &value
			}
			if len(acceptedModes) == 0 && historyLength < 0 && !cmd.Flags().Changed("return-immediately") {
				configuration = nil
			}
			client, err := resolveA2AProtocolClient(cmd.Context(), cfg, options)
			if err != nil {
				return err
			}
			response, err := client.SendMessage(cmd.Context(), a2a.SendMessageRequest{
				Message: a2a.Message{
					MessageID:  messageID,
					ContextID:  contextID,
					TaskID:     taskID,
					Role:       a2a.RoleUser,
					Parts:      []a2a.Part{a2a.TextPart(text)},
					Metadata:   metadata,
					Extensions: client.ActivatedExtensions(),
				},
				Configuration: configuration,
			})
			if err != nil {
				return fmt.Errorf("a2a send: %w", err)
			}
			if wait && response.Task != nil && !response.Task.Status.State.StopsPolling() {
				finalTask, err := client.WaitTask(cmd.Context(), response.Task.ID)
				if err != nil {
					return fmt.Errorf("a2a send wait: %w", err)
				}
				response.Task = finalTask
			}
			if jsonOutput {
				return encodeA2AJSON(cmd.OutOrStdout(), response)
			}
			return writeA2ASendHuman(cmd.OutOrStdout(), response)
		},
	}
	cmd.Flags().StringVar(&message, "message", "", "message text")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read exact message text from a file, or - for stdin")
	cmd.Flags().StringVar(&messageID, "message-id", "", "stable message id (generated when omitted)")
	cmd.Flags().StringVar(&contextID, "context-id", "", "optional A2A context id")
	cmd.Flags().StringVar(&taskID, "task-id", "", "optional existing A2A task id")
	cmd.Flags().StringVar(&metadataJSON, "metadata-json", "", "message metadata as a JSON object")
	cmd.Flags().StringSliceVar(&acceptedModes, "accepted-output-mode", nil, "accepted output media type (repeatable)")
	cmd.Flags().IntVar(&historyLength, "history-length", -1, "history messages to return (-1 leaves unset)")
	cmd.Flags().BoolVar(&returnImmediately, "return-immediately", false, "return before the task reaches a terminal or interrupted state")
	cmd.Flags().BoolVar(&wait, "wait", false, "poll a returned non-terminal task until it stops")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the native SendMessageResponse JSON")
	return cmd
}

func newA2AGetCmd(cfg Config, options *a2aCommandOptions) *cobra.Command {
	var id string
	var historyLength int
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Get a formal A2A task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(id) == "" {
				return errors.New("a2a get: --id is required")
			}
			request := a2a.GetTaskRequest{ID: id}
			if historyLength >= 0 {
				value, err := int32Flag(historyLength, "history-length")
				if err != nil {
					return err
				}
				request.HistoryLength = &value
			}
			client, err := resolveA2AProtocolClient(cmd.Context(), cfg, options)
			if err != nil {
				return err
			}
			task, err := client.GetTask(cmd.Context(), request)
			if err != nil {
				return fmt.Errorf("a2a get: %w", err)
			}
			if jsonOutput {
				return encodeA2AJSON(cmd.OutOrStdout(), task)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", task.ID, task.Status.State)
			return err
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "task id (required)")
	cmd.Flags().IntVar(&historyLength, "history-length", -1, "history messages to return (-1 leaves unset)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the native Task JSON")
	return cmd
}

func newA2AListCmd(cfg Config, options *a2aCommandOptions) *cobra.Command {
	var (
		contextID        string
		status           string
		pageSize         int
		pageToken        string
		historyLength    int
		timestampAfter   string
		includeArtifacts bool
		jsonOutput       bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List formal A2A tasks visible to the caller",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request := a2a.ListTasksRequest{ContextID: contextID, Status: a2a.TaskState(status), PageToken: pageToken}
			if pageSize > 0 {
				value, err := int32Flag(pageSize, "page-size")
				if err != nil {
					return err
				}
				request.PageSize = &value
			}
			if historyLength >= 0 {
				value, err := int32Flag(historyLength, "history-length")
				if err != nil {
					return err
				}
				request.HistoryLength = &value
			}
			if timestampAfter != "" {
				request.StatusTimestampAfter = a2a.Timestamp(timestampAfter)
			}
			if cmd.Flags().Changed("include-artifacts") {
				request.IncludeArtifacts = &includeArtifacts
			}
			client, err := resolveA2AProtocolClient(cmd.Context(), cfg, options)
			if err != nil {
				return err
			}
			response, err := client.ListTasks(cmd.Context(), request)
			if err != nil {
				return fmt.Errorf("a2a list: %w", err)
			}
			if jsonOutput {
				return encodeA2AJSON(cmd.OutOrStdout(), response)
			}
			sort.Slice(response.Tasks, func(i, j int) bool { return response.Tasks[i].ID < response.Tasks[j].ID })
			for _, task := range response.Tasks {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", task.ID, task.Status.State); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&contextID, "context-id", "", "filter by context id")
	cmd.Flags().StringVar(&status, "status", "", "filter by TaskState enum name")
	cmd.Flags().IntVar(&pageSize, "page-size", 0, "maximum tasks to return (1-100; 0 leaves unset)")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "pagination token")
	cmd.Flags().IntVar(&historyLength, "history-length", -1, "history messages per task (-1 leaves unset)")
	cmd.Flags().StringVar(&timestampAfter, "status-timestamp-after", "", "inclusive status timestamp filter (literal uppercase Z)")
	cmd.Flags().BoolVar(&includeArtifacts, "include-artifacts", false, "include task artifacts")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the native ListTasksResponse JSON")
	return cmd
}

func newA2ACancelCmd(cfg Config, options *a2aCommandOptions) *cobra.Command {
	var id string
	var metadataJSON string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "cancel",
		Short: "Cancel a formal A2A task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(id) == "" {
				return errors.New("a2a cancel: --id is required")
			}
			metadata, err := decodeA2AMetadata(metadataJSON)
			if err != nil {
				return err
			}
			client, err := resolveA2AProtocolClient(cmd.Context(), cfg, options)
			if err != nil {
				return err
			}
			task, err := client.CancelTask(cmd.Context(), a2a.CancelTaskRequest{ID: id, Metadata: metadata})
			if err != nil {
				return fmt.Errorf("a2a cancel: %w", err)
			}
			if jsonOutput {
				return encodeA2AJSON(cmd.OutOrStdout(), task)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", task.ID, task.Status.State)
			return err
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "task id (required)")
	cmd.Flags().StringVar(&metadataJSON, "metadata-json", "", "cancellation metadata as a JSON object")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the native Task JSON")
	return cmd
}

func resolveA2AProtocolClient(ctx context.Context, cfg Config, options *a2aCommandOptions) (*a2a.Client, error) {
	cardURL := strings.TrimSpace(options.cardURL)
	if cardURL == "" && strings.TrimSpace(options.peer) != "" {
		if cfg.A2ACardURL == nil {
			return nil, errors.New("a2a: --peer requires an embedder Agent Card resolver; use --card")
		}
		resolved, err := cfg.A2ACardURL(ctx, options.peer)
		if err != nil {
			return nil, fmt.Errorf("a2a: resolve peer Agent Card: %w", err)
		}
		cardURL = strings.TrimSpace(resolved)
	}
	if cardURL == "" {
		return nil, errors.New("a2a: --card is required")
	}
	card, err := a2a.FetchAgentCard(ctx, cardURL, cfg.A2AHTTPClient)
	if err != nil {
		return nil, fmt.Errorf("a2a: fetch Agent Card: %w", err)
	}
	clientOptions := []a2a.Option{a2a.WithExtensions(options.extensions...), a2a.WithPollInterval(options.pollInterval)}
	if cfg.A2AHTTPClient != nil {
		clientOptions = append(clientOptions, a2a.WithHTTPClient(cfg.A2AHTTPClient))
	}
	authorization := cfg.A2AAuthorization
	if options.bearerFile != "" {
		authorization = bearerTokenFile(options.bearerFile)
	}
	if authorization != nil {
		clientOptions = append(clientOptions, a2a.WithAuthorizationProvider(authorization))
	}
	client, err := a2a.NewClientFromCard(*card, clientOptions...)
	if err != nil {
		return nil, fmt.Errorf("a2a: select Agent Card interface: %w", err)
	}
	return client, nil
}

func bearerTokenFile(path string) a2a.AuthorizationProvider {
	return func(context.Context) (string, error) {
		raw, err := readA2AOperatorFile(path, 64<<10)
		if err != nil {
			return "", fmt.Errorf("read bearer token file: %w", err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" || strings.ContainsAny(token, "\r\n\x00") {
			return "", errors.New("read bearer token file: token is empty or contains a line break")
		}
		return "Bearer " + token, nil
	}
}

func resolveA2AMessageBody(cmd *cobra.Command, message, bodyFile string, args []string) (string, error) {
	if bodyFile != "" {
		if message != "" || len(args) > 0 {
			return "", errors.New("a2a send: --body-file cannot be combined with --message or positional text")
		}
		var raw []byte
		var err error
		if bodyFile == "-" {
			raw, err = io.ReadAll(cmd.InOrStdin())
		} else {
			raw, err = readA2AOperatorFile(bodyFile, 4<<20)
		}
		if err != nil {
			return "", fmt.Errorf("a2a send: read message body: %w", err)
		}
		if len(raw) == 0 {
			return "", errors.New("a2a send: message text is required")
		}
		return string(raw), nil
	}
	if message != "" && len(args) > 0 {
		return "", errors.New("a2a send: --message cannot be combined with positional text")
	}
	if message != "" {
		return message, nil
	}
	if len(args) == 1 && args[0] != "" {
		return args[0], nil
	}
	return "", errors.New("a2a send: message text is required")
}

func readA2AOperatorFile(path string, maxBytes int64) ([]byte, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve file path: %w", err)
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("open file directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(absolute))
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("read file: content exceeds %d bytes", maxBytes)
	}
	return raw, nil
}

func decodeA2AMetadata(value string) (map[string]any, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var metadata map[string]any
	decoder := json.NewDecoder(strings.NewReader(value))
	if err := decoder.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("a2a: decode metadata JSON: %w", err)
	}
	if metadata == nil {
		return nil, errors.New("a2a: metadata JSON must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("a2a: metadata JSON contains trailing data")
		}
		return nil, fmt.Errorf("a2a: decode metadata trailing data: %w", err)
	}
	return metadata, nil
}

func int32Flag(value int, name string) (int32, error) {
	if value < 0 || value > math.MaxInt32 {
		return 0, fmt.Errorf("a2a: --%s must be between 0 and %d", name, math.MaxInt32)
	}
	return int32(value), nil
}

func newA2AMessageID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("a2a send: generate message id: %w", err)
	}
	return "msg-" + hex.EncodeToString(random[:]), nil
}

func encodeA2AJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("a2a: encode output: %w", err)
	}
	return nil
}

func writeA2ASendHuman(out io.Writer, response *a2a.SendMessageResponse) error {
	if response.Task != nil {
		_, err := fmt.Fprintf(out, "%s\t%s\n", response.Task.ID, response.Task.Status.State)
		return err
	}
	parts := make([]string, 0, len(response.Message.Parts))
	for _, part := range response.Message.Parts {
		if text, ok := part.Text(); ok {
			parts = append(parts, text)
		}
	}
	_, err := fmt.Fprintln(out, strings.Join(parts, "\n"))
	return err
}
