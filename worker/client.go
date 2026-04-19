package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// defaultHTTPTimeout is the per-request timeout applied to the default
// HTTPClient created by NewClient. It is intentionally generous compared to
// afclient because the coordinator's poll endpoint may long-poll.
const defaultHTTPTimeout = 30 * time.Second

// Client is the HTTP client for the AgentFactory worker protocol. A fresh
// Client is created with the long-lived provisioning token; after a
// successful Register call the coordinator-assigned WorkerID and runtime
// JWT are stored on the Client and used for subsequent Poll/Heartbeat
// calls.
//
// Client is not safe for concurrent mutation: callers that share a Client
// across goroutines should not mutate WorkerID or RuntimeJWT after the
// initial Register. Concurrent reads of the HTTPClient are safe.
type Client struct {
	// BaseURL is the coordinator base URL without a trailing slash.
	BaseURL string
	// ProvisioningToken is the long-lived rsp_live_ token used only for
	// the initial Register call.
	ProvisioningToken string
	// RuntimeJWT is the short-lived bearer token returned by Register,
	// used for Poll and Heartbeat.
	RuntimeJWT string
	// WorkerID is the coordinator-assigned worker identifier populated
	// after a successful Register.
	WorkerID string
	// HTTPClient is the underlying HTTP client. NewClient installs one
	// with a 30s timeout; callers may replace it.
	HTTPClient *http.Client
}

// NewClient creates a Client pointing at the given coordinator base URL
// and carrying the given provisioning token. The returned Client has an
// HTTPClient with a 30-second timeout.
func NewClient(baseURL, provisioningToken string) *Client {
	return &Client{
		BaseURL:           strings.TrimRight(baseURL, "/"),
		ProvisioningToken: provisioningToken,
		HTTPClient:        &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// statusToError maps an HTTP status code to a worker-package sentinel
// error for expected failure modes, or a generic error for unexpected
// codes. Returns nil for 2xx. When runtime is true the 401 mapping
// reflects an expired runtime JWT; otherwise it reflects an invalid
// provisioning token.
func statusToError(status int, path string, runtime bool) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized:
		if runtime {
			return fmt.Errorf("%s: %w", path, ErrRuntimeJWTExpired)
		}
		return fmt.Errorf("%s: %w", path, ErrInvalidProvisioningToken)
	case status == http.StatusForbidden:
		return fmt.Errorf("%s: %w", path, ErrRuntimeJWTInvalid)
	case status == http.StatusNotFound:
		return fmt.Errorf("%s: %w", path, ErrNotFound)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", path, ErrRateLimited)
	case status >= 500:
		return fmt.Errorf("%s: %w", path, ErrServerError)
	default:
		return fmt.Errorf("unexpected status %d for %s", status, path)
	}
}

// doPost is the shared POST implementation used by postWithProvisioning
// and postWithRuntime. The runtime flag selects both the Authorization
// header token and the status-code mapping for 401.
func (c *Client) doPost(ctx context.Context, path string, body, target any, token string, runtime bool) error {
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("worker post: marshal: %w", err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("worker post: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("worker post: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := statusToError(resp.StatusCode, path, runtime); err != nil {
		return err
	}

	if target != nil {
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			return fmt.Errorf("worker post: decode: %w", err)
		}
	}
	return nil
}

// postWithProvisioning issues a POST authenticated with the provisioning
// token. Used by Register. A 401 response maps to ErrInvalidProvisioningToken.
func (c *Client) postWithProvisioning(ctx context.Context, path string, body, target any) error {
	return c.doPost(ctx, path, body, target, c.ProvisioningToken, false)
}

// postWithRuntime issues a POST authenticated with the runtime JWT. Used
// by Heartbeat and any future POST endpoints. A 401 response maps to
// ErrRuntimeJWTExpired.
func (c *Client) postWithRuntime(ctx context.Context, path string, body, target any) error {
	return c.doPost(ctx, path, body, target, c.RuntimeJWT, true)
}

// getWithRuntime issues a GET authenticated with the runtime JWT. Used
// by Poll. A 401 response maps to ErrRuntimeJWTExpired.
func (c *Client) getWithRuntime(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("worker get: create request: %w", err)
	}
	if c.RuntimeJWT != "" {
		req.Header.Set("Authorization", "Bearer "+c.RuntimeJWT)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("worker get: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := statusToError(resp.StatusCode, path, true); err != nil {
		return err
	}

	if target != nil {
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			return fmt.Errorf("worker get: decode: %w", err)
		}
	}
	return nil
}
