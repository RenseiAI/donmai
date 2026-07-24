package upstream

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/pool"
	"github.com/RenseiAI/donmai/gateway/translate"
)

// OpenAICompat dials any OpenAI Chat Completions-compatible endpoint. BaseURL
// is the API root (e.g. "https://api.openai.com/v1" or an aggregator's compat
// URL); the client appends "/chat/completions". Company is the model-vendor
// identity used for cost attribution — the gateway serves a vendor's model
// THROUGH this backend, so cost stays company-primary even via an aggregator.
type OpenAICompat struct {
	// Company is the cost-attribution identity (e.g. "openai").
	Company string
	// BaseURL is the OpenAI-compatible API root, no trailing "/chat/completions".
	BaseURL string
	// HTTPClient is the transport. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// AuthHeader overrides the credential header name. Empty uses
	// "Authorization" with a "Bearer " prefix (the OpenAI convention).
	AuthHeader string
	// ExtraHeaders are added verbatim to every request (e.g. an aggregator's
	// referer/title headers). Never carries a secret — secrets ride the
	// credential.
	ExtraHeaders map[string]string
}

// Name implements Upstream.
func (u *OpenAICompat) Name() string { return u.Company }

// Invoke implements Upstream.
func (u *OpenAICompat) Invoke(ctx context.Context, req ir.Request, cred pool.Credential) (Outcome, error) {
	body, err := translate.EncodeRequest(req)
	if err != nil {
		return Outcome{}, err
	}
	endpoint := strings.TrimRight(u.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Outcome{}, fmt.Errorf("gateway/upstream: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	u.applyAuth(httpReq, cred)

	client := u.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Outcome{Status: 0}, fmt.Errorf("gateway/upstream: dial %s: %w", u.Company, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Outcome{Status: resp.StatusCode}, &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}

	if req.Stream {
		stream := u.readStream(resp)
		return Outcome{Status: resp.StatusCode, Stream: stream}, nil
	}

	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Outcome{Status: resp.StatusCode}, fmt.Errorf("gateway/upstream: read body: %w", err)
	}
	irResp, err := translate.DecodeResponse(raw)
	if err != nil {
		return Outcome{Status: resp.StatusCode}, err
	}
	return Outcome{Status: resp.StatusCode, Response: &irResp}, nil
}

// applyAuth sets the credential header. Default is Authorization: Bearer <key>.
func (u *OpenAICompat) applyAuth(r *http.Request, cred pool.Credential) {
	for k, v := range u.ExtraHeaders {
		r.Header.Set(k, v)
	}
	if u.AuthHeader != "" && !strings.EqualFold(u.AuthHeader, "Authorization") {
		r.Header.Set(u.AuthHeader, cred.Secret)
		return
	}
	r.Header.Set("Authorization", "Bearer "+cred.Secret)
}

// readStream consumes the SSE body in a goroutine, decoding each `data:` chunk
// into an ir.StreamDelta. The channel closes when the upstream sends [DONE] or
// EOF; a mid-stream transport error is surfaced via the returned Stream.Err.
func (u *OpenAICompat) readStream(resp *http.Response) *ir.Stream {
	deltas := make(chan ir.StreamDelta)
	var streamErr error
	done := make(chan struct{})

	go func() {
		defer close(deltas)
		defer close(done)
		defer func() { _ = resp.Body.Close() }()

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			delta, isDone, derr := translate.DecodeStreamChunk([]byte(payload))
			if isDone {
				return
			}
			if derr != nil {
				streamErr = derr
				return
			}
			deltas <- delta
		}
		if err := sc.Err(); err != nil {
			streamErr = fmt.Errorf("gateway/upstream: read stream: %w", err)
		}
	}()

	return &ir.Stream{
		Deltas: deltas,
		Err: func() error {
			<-done
			return streamErr
		},
	}
}
