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

// maxToolResultBytes caps one tool-result text part when a request is
// re-sent after the upstream rejects it with a 400. A harness replays its
// whole context on every turn, so one huge tool result (tens of kilobytes of
// error output) can push the replayed request past what the upstream accepts;
// the retry keeps the head of the output (where the actionable error lines
// live) and marks the cut with truncatedToolResultMarker. First attempts are
// never capped: only the retry path truncates, so a session that fits never
// loses tool output.
const maxToolResultBytes = 32 << 10

// truncatedToolResultMarker marks a tool result the retry path cut to
// maxToolResultBytes. ASCII-only so it survives any upstream charset handling.
const truncatedToolResultMarker = "\n...[truncated by gateway retry: output exceeded 32 KiB]"

// sanitizeRequestForRetry returns a copy of req with the parts an upstream
// has rejected with a 400 removed or cut down: content-less assistant messages
// (empty thinking blocks decode to no text and no tool call, which several
// OpenAI-compatible upstreams reject on replay) are dropped, and tool-result
// text over maxToolResultBytes is cut to the marker. Tool-result messages are
// never dropped — losing the answer to a tool call would strand the turn —
// and nothing else is touched, so the retry still answers every pending call.
func sanitizeRequestForRetry(req ir.Request) (ir.Request, bool) {
	out := req
	out.Messages = make([]ir.Message, 0, len(req.Messages))
	changed := false
	for _, m := range req.Messages {
		switch m.Role {
		case ir.RoleAssistant:
			hasText, hasCall := false, false
			for _, p := range m.Parts {
				switch p.Kind {
				case ir.PartText:
					if p.Text != "" {
						hasText = true
					}
				case ir.PartToolCall:
					hasCall = true
				}
			}
			if !hasText && !hasCall {
				changed = true
				continue
			}
			out.Messages = append(out.Messages, m)
		case ir.RoleTool:
			cp := m
			cp.Parts = make([]ir.Part, 0, len(m.Parts))
			for _, part := range m.Parts {
				if part.Kind == ir.PartToolResult && len(part.Text) > maxToolResultBytes {
					part.Text = truncateToolResult(part.Text)
					changed = true
				}
				cp.Parts = append(cp.Parts, part)
			}
			out.Messages = append(out.Messages, cp)
		default:
			out.Messages = append(out.Messages, m)
		}
	}
	return out, changed
}

// truncateToolResult cuts text to maxToolResultBytes on a rune boundary and
// appends truncatedToolResultMarker.
func truncateToolResult(text string) string {
	cut := maxToolResultBytes
	for cut > 0 && cut < len(text) && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + truncatedToolResultMarker
}

// doPost encodes req and performs one POST against the upstream chat
// completions endpoint, returning the raw response (open on success so the
// caller can stream or decode it; drained and closed on a non-2xx).
func (u *OpenAICompat) doPost(ctx context.Context, req ir.Request, cred pool.Credential) (*http.Response, error) {
	body, err := translate.EncodeRequest(req)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(u.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gateway/upstream: build request: %w", err)
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
	return client.Do(httpReq)
}

// Invoke implements Upstream. A 400 response is retried ONCE with a sanitized
// request (sanitizeRequestForRetry): the failure that motivated this path saw
// the replay of an empty-thinking assistant turn plus a tens-of-kilobytes
// error tool result rejected as invalid parameters, and the turn can continue
// without either. Only a 400 retries, and only once — any other status, a
// transport error, or a second 400 returns as before.
func (u *OpenAICompat) Invoke(ctx context.Context, req ir.Request, cred pool.Credential) (Outcome, error) {
	resp, err := u.doPost(ctx, req, cred)
	if err != nil {
		return Outcome{Status: 0}, fmt.Errorf("gateway/upstream: dial %s: %w", u.Company, err)
	}

	if resp.StatusCode == http.StatusBadRequest {
		if sanitized, ok := sanitizeRequestForRetry(req); ok {
			_ = resp.Body.Close()
			sresp, serr := u.doPost(ctx, sanitized, cred)
			if serr != nil {
				return Outcome{Status: 0}, fmt.Errorf("gateway/upstream: dial %s: %w", u.Company, serr)
			}
			resp = sresp
		}
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
