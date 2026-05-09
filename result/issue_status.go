package result

// Issue-tracker proxy calls (REN-1467).
//
// The platform exposes a generic issue-tracker proxy at
// /api/issue-tracker-proxy that accepts worker bearer auth and forwards
// the call to the org's resolved Linear client. The proxy translates
// the generic operation into the platform-specific GraphQL mutation
// (issueUpdate / commentCreate) so the runner does not have to know the
// target platform.
//
// Reference (TS): packages/linear/src/issue-tracker-proxy.ts and
// platform/src/app/api/issue-tracker-proxy/route.ts.
//
// Wire shape (request):
//
//	POST /api/issue-tracker-proxy
//	Authorization: Bearer <worker-token>
//	Content-Type: application/json
//
//	{
//	  "method": "updateIssueStatus",
//	  "args":   ["<issue-uuid>", "Finished"]
//	}
//
// Wire shape (response):
//
//	200 OK
//	{ "success": true, "data": { ...serialized issue... } }
//
//	4xx
//	{ "success": false, "error": { "code": "...", "message": "...", "retryable": false } }
//
// Supported methods today: updateIssueStatus, createComment. Extend the
// switch in proxyCall when porting more methods.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// proxyEndpoint is the relative path of the platform's issue-tracker
// proxy. Joined with Poster.platformURL via urlFor (preserves any base
// path the platform sits behind).
const proxyEndpoint = "/api/issue-tracker-proxy"

// proxyRequest mirrors ProxyRequest from
// packages/linear/src/issue-tracker-proxy.ts.
type proxyRequest struct {
	Method         string `json:"method"`
	Args           []any  `json:"args"`
	OrganizationID string `json:"organizationId,omitempty"`
}

// proxyResponse mirrors ProxyResponse<T>. Data is decoded as raw JSON
// and surfaced to callers via *json.RawMessage when they need it.
type proxyResponse struct {
	Success bool             `json:"success"`
	Data    *json.RawMessage `json:"data,omitempty"`
	Error   *proxyError      `json:"error,omitempty"`
}

// proxyError mirrors the inner error envelope.
type proxyError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// UpdateIssueStatus transitions the issue identified by issueID to the
// named workflow status (e.g. "Finished", "Rejected"). The platform's
// issue-tracker proxy resolves the named status to the team's stateId
// via getTeamStatuses + matches the name to a workflow state.
//
// Errors:
//
//   - returns nil when the proxy responds 200 with success=true
//   - returns a wrapped [PermanentError] on a 4xx response or
//     success=false with retryable=false
//   - returns a wrapped [TransientError] when retries are exhausted on
//     a transient failure (5xx, network timeout)
//   - returns ctx.Err() when the context is cancelled
func (p *Poster) UpdateIssueStatus(ctx context.Context, issueID, targetStatus string) error {
	if strings.TrimSpace(issueID) == "" {
		return errors.New("result: issueID is required for UpdateIssueStatus")
	}
	if strings.TrimSpace(targetStatus) == "" {
		return errors.New("result: targetStatus is required for UpdateIssueStatus")
	}
	body := proxyRequest{
		Method: "updateIssueStatus",
		Args:   []any{issueID, targetStatus},
	}
	return p.proxyCall(ctx, body)
}

// CreateIssueComment posts a comment to the issue identified by
// issueID. Used by the post-session unknown-WORK_RESULT diagnostic
// path (sdlc.go::diagnosticCommentBody) and any other runner-side
// best-effort comments.
//
// Errors mirror UpdateIssueStatus.
func (p *Poster) CreateIssueComment(ctx context.Context, issueID, body string) error {
	if strings.TrimSpace(issueID) == "" {
		return errors.New("result: issueID is required for CreateIssueComment")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("result: body is required for CreateIssueComment")
	}
	req := proxyRequest{
		Method: "createComment",
		Args:   []any{issueID, body},
	}
	return p.proxyCall(ctx, req)
}

// proxyCall sends one issue-tracker-proxy request with retry/backoff
// matching Poster.doRetried. Decodes the wire envelope and surfaces
// success=false as the appropriate error type. Credentials are
// re-resolved before every attempt so a daemon-side runtime-JWT refresh
// between retries (e.g. after a 401 from an expired token) propagates
// here — see SUP-1823.
func (p *Poster) proxyCall(ctx context.Context, body proxyRequest) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal proxy body: %w", err)
	}
	endpoint := p.urlFor(proxyEndpoint)

	var lastErr error
	for attempt := 1; attempt <= p.maxAttempts; attempt++ {
		creds := p.credentials(ctx)
		err := p.proxyOnce(ctx, endpoint, payload, creds.AuthToken)
		if err == nil {
			return nil
		}
		lastErr = err

		var perm *PermanentError
		if errors.As(err, &perm) {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if attempt < p.maxAttempts {
			if waitErr := p.sleep(ctx, attempt); waitErr != nil {
				return waitErr
			}
		}
	}
	return &TransientError{Attempts: p.maxAttempts, Last: lastErr}
}

// proxyOnce performs a single POST against the proxy endpoint with the
// supplied bearer token. Returns PermanentError for non-401 4xx or
// success=false retryable=false; transient error otherwise. 401 is
// treated as retryable so the next iteration of [Poster.proxyCall] can
// re-resolve credentials and retry with a fresh token.
func (p *Poster) proxyOnce(ctx context.Context, endpoint string, payload []byte, authToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	bodyText := strings.TrimSpace(string(bodyBytes))

	// 5xx — transient
	if resp.StatusCode >= 500 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, bodyText)
	}

	// 401 — retryable so the credential provider can supply a fresher
	// token on the next attempt. Mirrors the result-poster's doOnce.
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("http 401 unauthorized: %s", bodyText)
	}

	// 4xx — permanent
	if resp.StatusCode >= 400 {
		return &PermanentError{StatusCode: resp.StatusCode, Body: bodyText}
	}

	// 2xx — decode envelope; success=false still counts as failure.
	var envelope proxyResponse
	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		return fmt.Errorf("decode proxy response: %w (body=%q)", err, bodyText)
	}
	if envelope.Success {
		return nil
	}
	if envelope.Error != nil {
		errMsg := fmt.Sprintf("proxy error %s: %s", envelope.Error.Code, envelope.Error.Message)
		if envelope.Error.Retryable {
			return errors.New(errMsg)
		}
		return &PermanentError{StatusCode: resp.StatusCode, Body: errMsg}
	}
	return &PermanentError{StatusCode: resp.StatusCode, Body: "proxy returned success=false with no error envelope"}
}

// _ keeps time imported for future jitter knobs without touching the
// import block.
var _ = time.Second
