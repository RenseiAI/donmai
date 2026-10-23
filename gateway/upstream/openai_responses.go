package upstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/pool"
	"github.com/RenseiAI/donmai/gateway/translate"
)

// OpenAIResponses uses the same HTTP/auth configuration as OpenAICompat but
// speaks the native Responses wire. It shares the existing route and pool.
type OpenAIResponses struct{ OpenAICompat }

// Invoke translates a stateless Responses exchange through the canonical IR.
func (u *OpenAIResponses) Invoke(ctx context.Context, req ir.Request, cred pool.Credential) (Outcome, error) {
	body, err := translate.EncodeResponsesRequest(req)
	if err != nil {
		return Outcome{Status: http.StatusBadRequest}, err
	}
	endpoint := strings.TrimRight(u.BaseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Outcome{}, fmt.Errorf("gateway/upstream: build responses request: %w", err)
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
		return Outcome{Status: 0}, errors.New("gateway/upstream: responses transport failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return Outcome{Status: resp.StatusCode}, &Error{Status: resp.StatusCode, Message: "responses upstream rejected request"}
	}
	if req.Stream {
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			_ = resp.Body.Close()
			return Outcome{Status: resp.StatusCode}, errors.New("gateway/upstream: responses stream content type")
		}
		return Outcome{Status: resp.StatusCode, Stream: readResponsesStream(ctx, resp)}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 {
		return Outcome{Status: resp.StatusCode}, errors.New("gateway/upstream: responses body read failed")
	}
	decoded, err := translate.DecodeResponsesResponse(raw)
	if err != nil {
		return Outcome{Status: resp.StatusCode}, err
	}
	return Outcome{Status: resp.StatusCode, Response: &decoded}, nil
}

func readResponsesStream(ctx context.Context, resp *http.Response) *ir.Stream {
	deltas := make(chan ir.StreamDelta)
	done := make(chan struct{})
	var streamErr error
	go func() {
		defer close(deltas)
		defer close(done)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var responseID string
		var eventName string
		var data strings.Builder
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if data.Len() == 0 {
					eventName = ""
					continue
				}
				event, err := translate.DecodeResponsesStreamEvent([]byte(data.String()))
				data.Reset()
				if err != nil || eventName != "" && eventName != event.Kind {
					streamErr = errors.New("gateway/upstream: invalid responses stream event")
					return
				}
				eventName = ""
				if event.Kind == "response.created" {
					if responseID != "" {
						streamErr = errors.New("gateway/upstream: duplicate responses creation")
						return
					}
					responseID = event.ResponseID
				} else if responseID == "" || (event.ResponseID != "" && event.ResponseID != responseID) {
					streamErr = errors.New("gateway/upstream: responses lifecycle mismatch")
					return
				}
				select {
				case deltas <- ir.StreamDelta{Responses: &event}:
				case <-ctx.Done():
					streamErr = ctx.Err()
					return
				}
				if event.Kind == "response.completed" {
					return
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if payload == "[DONE]" {
					streamErr = errors.New("gateway/upstream: responses stream ended without completion")
					return
				}
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				if data.Len()+len(payload) > 1<<20 {
					streamErr = errors.New("gateway/upstream: responses event too large")
					return
				}
				data.WriteString(payload)
			case !strings.HasPrefix(line, ":"):
				streamErr = errors.New("gateway/upstream: unsupported responses SSE field")
				return
			}
		}
		if err := sc.Err(); err != nil {
			streamErr = errors.New("gateway/upstream: responses stream read failed")
		} else {
			streamErr = errors.New("gateway/upstream: responses stream closed before completion")
		}
	}()
	return &ir.Stream{Deltas: deltas, Err: func() error { <-done; return streamErr }}
}
