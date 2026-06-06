package ollama

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// probe issues a short-deadline GET /api/tags against endpoint. A non-
// 2xx status or transport error is returned to the caller verbatim;
// New wraps it with agent.ErrProviderUnavailable.
//
// The probe is intentionally minimal: it only verifies the server is
// reachable and speaking the Ollama API. It does NOT attempt to
// validate that any specific model is installed — model availability
// is a per-Spawn concern (the chat request will surface a clear 4xx
// from Ollama if the requested model is missing).
func probe(ctx context.Context, client *http.Client, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s/api/tags: %w", endpoint, err)
	}
	defer func() {
		// Drain and close so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s/api/tags returned HTTP %d", endpoint, resp.StatusCode)
	}
	return nil
}
