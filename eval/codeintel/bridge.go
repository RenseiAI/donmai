package codeintel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBridgePath is the platform endpoint the harness POSTs each
// (run + trace) envelope to. The platform lane is confirming the exact route;
// this default is the agreed target and is overridable via --platform-path (see
// integrationNotes). It is intentionally under /api/evals/ so it inherits the
// getCliOrSessionAuth bearer surface the rest of the eval API uses.
const DefaultBridgePath = "/api/evals/runs/ingest"

// Bridge posts eval_runs/eval_traces envelopes to the platform. A zero BaseURL
// makes every post a no-op (the --dry / offline path) so the harness runs fully
// without a live platform — results are still captured locally.
type Bridge struct {
	BaseURL string
	Token   string
	Path    string
	Client  *http.Client
}

// NewBridge builds a Bridge. baseURL "" disables posting (offline/dry).
func NewBridge(baseURL, token, path string) *Bridge {
	if path == "" {
		path = DefaultBridgePath
	}
	return &Bridge{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Path:    path,
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled reports whether posts will actually be sent.
func (b *Bridge) Enabled() bool { return b != nil && b.BaseURL != "" }

// Post sends one envelope. When the bridge is disabled it returns (false, nil):
// "not posted, not an error". On a live post it returns (true, err).
func (b *Bridge) Post(ctx context.Context, env ReportEnvelope) (posted bool, err error) {
	if !b.Enabled() {
		return false, nil
	}
	body, err := json.Marshal(env)
	if err != nil {
		return false, fmt.Errorf("marshal envelope: %w", err)
	}
	url := b.BaseURL + b.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("post envelope: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("bridge POST %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return true, nil
}
