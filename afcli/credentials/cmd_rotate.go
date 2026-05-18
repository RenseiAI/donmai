package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// rotateEnv bundles the I/O surface + ambient environment lookups the
// rotate command needs, so tests can substitute fake io and avoid touching
// the real filesystem / environment.
type rotateEnv struct {
	// In is the source of any prompt answers. Reserved — rotate does not
	// currently prompt, but kept symmetric with cmd_setup.go so the
	// wizardEnv shape extends to additional credentials commands without
	// repeated env-struct rewrites.
	In io.Reader

	// Out receives normal status output.
	Out io.Writer

	// Err receives warnings/errors that aren't returned via the run error.
	Err io.Writer

	// HomeDir is the home directory used to resolve ~/.rensei/cli.token
	// and ~/.rensei/cli-config.yaml. Production wiring passes
	// os.UserHomeDir()'s result.
	HomeDir string

	// Getenv mirrors os.Getenv and is overridable in tests so we never
	// touch the process environment.
	Getenv func(key string) string

	// ReadFile mirrors os.ReadFile and is overridable so tests inject
	// scripted token/config-file contents.
	ReadFile func(path string) ([]byte, error)

	// HTTPClient is the client used to POST the rotate request. Tests
	// point this at an httptest.NewServer.
	HTTPClient *http.Client
}

// rotateFlags carries the resolved (post-default-merge) flag values.
type rotateFlags struct {
	kind        string
	orgID       string
	platformURL string
	rskToken    string
}

// rotateResponse mirrors the platform /api/daemon/credentials/rotate
// success body. Decoded only on 2xx; non-2xx surfaces the raw body in
// the error message.
type rotateResponse struct {
	OK           bool   `json:"ok"`
	Kind         string `json:"kind"`
	SessionCount int    `json:"sessionCount"`
	RotatedAt    string `json:"rotatedAt"`
}

// platformErrorResponse mirrors the platform's createErrorResponse JSON
// shape so we can surface a useful message on non-2xx codes.
type platformErrorResponse struct {
	Error string `json:"error"`
}

// newRotateCmd builds the `af creds rotate <kind>` Cobra command.
func newRotateCmd() *cobra.Command {
	var (
		orgID       string
		platformURL string
		rskToken    string
	)

	cmd := &cobra.Command{
		Use:   "rotate <kind>",
		Short: "Notify live sessions that an upstream credential rotated",
		Long: "Notifies the platform that the (orgId, kind) credential has been\n" +
			"rotated upstream (in Vault, 1Password, or the encrypted credentials\n" +
			"table). The platform resolves the fresh value via its registry and\n" +
			"emits a rotate event to every live session for the org. This command\n" +
			"does NOT mutate the credential itself; it triggers the fan-out only.\n" +
			"\n" +
			"Reads --platform-url from RENSEI_PLATFORM_URL, --rsk-token from\n" +
			"RENSEI_RSK_TOKEN / WORKER_API_KEY / RENSEI_API_TOKEN /\n" +
			"~/.rensei/cli.token (in that order), and --org-id from\n" +
			"RENSEI_ORG_ID / ~/.rensei/cli-config.yaml.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, _ := os.UserHomeDir()
			env := &rotateEnv{
				In:         cmd.InOrStdin(),
				Out:        cmd.OutOrStdout(),
				Err:        cmd.ErrOrStderr(),
				HomeDir:    home,
				Getenv:     os.Getenv,
				ReadFile:   os.ReadFile,
				HTTPClient: &http.Client{Timeout: 30 * time.Second},
			}
			return runRotate(cmd.Context(), env, rotateFlags{
				kind:        args[0],
				orgID:       orgID,
				platformURL: platformURL,
				rskToken:    rskToken,
			})
		},
	}

	cmd.Flags().StringVar(&orgID, "org-id", "",
		"Target org id (overrides RENSEI_ORG_ID / cli-config.yaml)")
	cmd.Flags().StringVar(&platformURL, "platform-url", "",
		"Platform base URL (overrides RENSEI_PLATFORM_URL)")
	cmd.Flags().StringVar(&rskToken, "rsk-token", "",
		"rsk_* token (overrides RENSEI_RSK_TOKEN / WORKER_API_KEY / "+
			"RENSEI_API_TOKEN / ~/.rensei/cli.token)")

	return cmd
}

// runRotate is the pure-Go entry point. Exported within the package
// (lowercase r) so tests drive it directly with a fake rotateEnv.
func runRotate(ctx context.Context, env *rotateEnv, flags rotateFlags) error {
	if env == nil || env.Out == nil || env.Err == nil ||
		env.Getenv == nil || env.ReadFile == nil || env.HTTPClient == nil {
		return errors.New("rotateEnv: Out/Err/Getenv/ReadFile/HTTPClient must be non-nil")
	}
	if strings.TrimSpace(flags.kind) == "" {
		return errors.New("kind argument is required")
	}

	// Resolve --platform-url. Flag wins; else env.
	if flags.platformURL == "" {
		flags.platformURL = env.Getenv("RENSEI_PLATFORM_URL")
	}
	if flags.platformURL == "" {
		return errors.New("--platform-url required (or set RENSEI_PLATFORM_URL)")
	}

	// Resolve --rsk-token. Flag wins; else env precedence; else token file.
	if flags.rskToken == "" {
		for _, name := range []string{"RENSEI_RSK_TOKEN", "WORKER_API_KEY", "RENSEI_API_TOKEN"} {
			if v := env.Getenv(name); v != "" {
				flags.rskToken = v
				break
			}
		}
	}
	if flags.rskToken == "" && env.HomeDir != "" {
		tokenPath := filepath.Join(env.HomeDir, ".rensei", "cli.token")
		if data, err := env.ReadFile(tokenPath); err == nil {
			flags.rskToken = strings.TrimSpace(string(data))
		}
	}
	if flags.rskToken == "" {
		return errors.New("--rsk-token required (or set RENSEI_RSK_TOKEN / " +
			"WORKER_API_KEY / RENSEI_API_TOKEN, or write to ~/.rensei/cli.token)")
	}

	// Resolve --org-id. Flag wins; else env; else cli-config.yaml.
	if flags.orgID == "" {
		flags.orgID = env.Getenv("RENSEI_ORG_ID")
	}
	if flags.orgID == "" && env.HomeDir != "" {
		configPath := filepath.Join(env.HomeDir, ".rensei", "cli-config.yaml")
		if data, err := env.ReadFile(configPath); err == nil {
			flags.orgID = parseOrgIDFromConfig(data)
		}
	}
	if flags.orgID == "" {
		return errors.New("--org-id required (or set RENSEI_ORG_ID, " +
			"or add an `orgId:` line to ~/.rensei/cli-config.yaml)")
	}

	// Build POST body.
	reqBody, err := json.Marshal(map[string]string{
		"orgId": flags.orgID,
		"kind":  flags.kind,
	})
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	url := strings.TrimRight(flags.platformURL, "/") + "/api/daemon/credentials/rotate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+flags.rskToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := env.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("read response body: %w", readErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAndFormatError(resp.StatusCode, body)
	}

	var success rotateResponse
	if err := json.Unmarshal(body, &success); err != nil {
		return fmt.Errorf("decode 2xx response: %w (body=%q)", err, string(body))
	}

	fmt.Fprintf(env.Out,
		"rotated kind=%s  notified sessions=%d  at=%s\n",
		success.Kind, success.SessionCount, success.RotatedAt)
	return nil
}

// decodeAndFormatError surfaces a useful CLI error message for non-2xx
// responses. The platform returns { "error": "..." } via createErrorResponse;
// when that decode fails we surface the raw body for debugging.
func decodeAndFormatError(status int, body []byte) error {
	var msg string
	var parsed platformErrorResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		msg = parsed.Error
	} else {
		msg = strings.TrimSpace(string(body))
		if msg == "" {
			msg = "(empty response body)"
		}
	}

	// Map well-known codes to friendlier prefixes so the user can act on
	// them without parsing the platform's message verbatim.
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("rotate failed: 401 unauthorized — check RENSEI_RSK_TOKEN: %s", msg)
	case http.StatusForbidden:
		return fmt.Errorf("rotate failed: 403 auth orgId mismatch (or missing admin role): %s", msg)
	case http.StatusNotFound:
		return fmt.Errorf("rotate failed: 404 kind not configured for this org: %s", msg)
	default:
		return fmt.Errorf("rotate failed: HTTP %d: %s", status, msg)
	}
}

// parseOrgIDFromConfig extracts the `orgId:` field from a YAML file
// without pulling in a YAML parser dependency. We accept either
// `orgId: foo` or `orgId: "foo"` / `orgId: 'foo'`. The parse is
// intentionally lenient — any malformed line is ignored, and an
// empty result triggers the "supply --org-id" error path upstream.
func parseOrgIDFromConfig(data []byte) string {
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Match `orgId:` prefix (case-sensitive — YAML is case-sensitive).
		const prefix = "orgId:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimSpace(line[len(prefix):])
		// Strip an optional inline comment.
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		// Strip surrounding quotes.
		value = strings.Trim(value, `"'`)
		return value
	}
	return ""
}
