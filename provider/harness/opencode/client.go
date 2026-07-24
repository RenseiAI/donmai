package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ─── serverClient: the opencode-serve REST/SSE absorber (07 §4) ──────────────
//
// Everything HTTP lives behind one package-internal interface so an opencode
// endpoint reshape is absorbed by swapping one implementation. clientV1
// targets the surface the pinned binary (opencode 1.17.18) actually serves.
//
// DRIFT NOTE (code wins over design §4): 07 §4 was written against the older
// FLAT 1.x REST surface (`/session`, `/session/:id/abort`, `/event`). The
// pinned binary already ships the v2-style API — verified live against
// opencode 1.17.18's own OpenAPI (`GET /doc`): every route is under `/api/`,
// session-scoped (not yet project-nested), abort is `/interrupt`, the prompt
// lane is the admission-model `POST /api/session/:id/prompt` returning a
// durable `SessionInputAdmitted`, and permission adjudication is
// `GET /api/permission/request` + `POST /api/session/:id/permission/:id/reply`.
// clientV1 targets THOSE endpoints. A future clientV2 (project-nested,
// durable `sessions.log` replay) selects by probed server version — 07 §4's
// "do not chase v2 until it ships as default" now reads "the interface is the
// insurance; the shipping surface is already what clientV1 speaks."
type serverClient interface {
	// Health returns nil when the server reports healthy.
	Health(ctx context.Context) error
	// CreateSession creates a session and returns its provider-native id.
	CreateSession(ctx context.Context, req createSessionReq) (sessionID string, err error)
	// Prompt admits a prompt onto a session (async admission model).
	Prompt(ctx context.Context, sessionID string, req promptReq) error
	// Abort interrupts the active turn on a session.
	Abort(ctx context.Context, sessionID string) error
	// Events subscribes to the global SSE feed. Returns the event channel, a
	// stop func that cancels the subscription and returns any stream error,
	// and a construction error.
	Events(ctx context.Context) (<-chan serverEvent, func() error, error)
	// PendingPermissions lists pending permission requests scoped to
	// sessionID.
	PendingPermissions(ctx context.Context, sessionID string) ([]permissionRequest, error)
	// RespondPermission replies to one pending permission request.
	RespondPermission(ctx context.Context, sessionID, permissionID string, resp permissionResponse) error
	// Messages replays session messages after the given cursor (backfill on
	// SSE drop). Empty after starts from the beginning.
	Messages(ctx context.Context, sessionID, after string) ([]serverMessage, error)
}

// modelRef is the v2 create-session model selector.
type modelRef struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
	Variant    string `json:"variant,omitempty"`
}

type locationRef struct {
	Directory string `json:"directory"`
}

type createSessionReq struct {
	Model    modelRef    `json:"model"`
	Location locationRef `json:"location"`
	Agent    string      `json:"agent,omitempty"`
}

// promptInput is the v2 prompt payload.
type promptInput struct {
	Text string `json:"text"`
}

type promptReq struct {
	Prompt   promptInput `json:"prompt"`
	Delivery string      `json:"delivery,omitempty"` // "steer" (default) | "queue"
	Resume   bool        `json:"resume,omitempty"`
}

// permissionRequest is one pending permission adjudication, as returned by
// GET /api/permission/request (filtered to a session by the caller).
type permissionRequest struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	Action    string          `json:"action"`
	Resources []string        `json:"resources"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// permissionResponse is the reply body. Reply is "once" | "always" | "reject".
type permissionResponse struct {
	Reply   string `json:"reply"`
	Message string `json:"message,omitempty"`
}

// serverEvent is one raw SSE frame from /api/event. Properties is decoded by
// events_sse.go into the typed agent.Event vocabulary.
type serverEvent struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// serverMessage is one durable session message (backfill). Kept intentionally
// loose — the replay lane only needs the id (for dedup) and the raw payload.
type serverMessage struct {
	ID   string          `json:"id"`
	Raw  json.RawMessage `json:"-"`
	Type string          `json:"type,omitempty"`
}

// clientV1 speaks the pinned binary's /api/ surface.
type clientV1 struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// newClientV1 constructs a clientV1 for the given server base URL (no trailing
// slash). A nil httpClient gets a default with no overall timeout (the SSE
// stream is long-lived; per-request calls carry their own ctx deadlines).
func newClientV1(baseURL, apiKey string, httpClient *http.Client) *clientV1 {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &clientV1{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    httpClient,
	}
}

var _ serverClient = (*clientV1)(nil)

func (c *clientV1) newReq(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

// doJSON issues a request and decodes a JSON response into out (may be nil).
// Non-2xx responses become errors carrying a bounded body tail.
func (c *clientV1) doJSON(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newReq(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024))
		return fmt.Errorf("opencode %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(tail)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *clientV1) Health(ctx context.Context) error {
	var out struct {
		Healthy bool `json:"healthy"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/health", nil, &out); err != nil {
		return err
	}
	if !out.Healthy {
		return fmt.Errorf("opencode server reported not healthy")
	}
	return nil
}

func (c *clientV1) CreateSession(ctx context.Context, req createSessionReq) (string, error) {
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/session", req, &out); err != nil {
		return "", err
	}
	if out.Data.ID == "" {
		return "", fmt.Errorf("opencode create session: empty session id in response")
	}
	return out.Data.ID, nil
}

func (c *clientV1) Prompt(ctx context.Context, sessionID string, req promptReq) error {
	if req.Delivery == "" {
		req.Delivery = "steer"
	}
	return c.doJSON(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/prompt", req, nil)
}

func (c *clientV1) Abort(ctx context.Context, sessionID string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/interrupt", struct{}{}, nil)
}

func (c *clientV1) PendingPermissions(ctx context.Context, sessionID string) ([]permissionRequest, error) {
	var out struct {
		Data []permissionRequest `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/permission/request", nil, &out); err != nil {
		return nil, err
	}
	if sessionID == "" {
		return out.Data, nil
	}
	filtered := out.Data[:0]
	for _, p := range out.Data {
		if p.SessionID == sessionID {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

func (c *clientV1) RespondPermission(ctx context.Context, sessionID, permissionID string, resp permissionResponse) error {
	path := fmt.Sprintf("/api/session/%s/permission/%s/reply", url.PathEscape(sessionID), url.PathEscape(permissionID))
	return c.doJSON(ctx, http.MethodPost, path, resp, nil)
}

func (c *clientV1) Messages(ctx context.Context, sessionID, after string) ([]serverMessage, error) {
	path := "/api/session/" + url.PathEscape(sessionID) + "/message"
	if after != "" {
		path += "?cursor=" + url.QueryEscape(after)
	}
	var out struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	msgs := make([]serverMessage, 0, len(out.Data))
	for _, raw := range out.Data {
		var hdr struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &hdr)
		msgs = append(msgs, serverMessage{ID: hdr.ID, Type: hdr.Type, Raw: raw})
	}
	return msgs, nil
}

// sseEventBuffer sizes the SSE event channel — large enough to absorb a burst
// of message-part deltas without backpressuring the HTTP read loop.
const sseEventBuffer = 256

// Events subscribes to /api/event and streams decoded frames. The returned
// stop func cancels the subscription; it returns the terminal stream error
// (nil on a clean context cancel / EOF).
func (c *clientV1) Events(ctx context.Context) (<-chan serverEvent, func() error, error) {
	subCtx, cancel := context.WithCancel(ctx)
	req, err := c.newReq(subCtx, http.MethodGet, "/api/event", nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024))
		_ = resp.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("opencode subscribe /api/event: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(tail)))
	}

	out := make(chan serverEvent, sseEventBuffer)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		errCh <- readSSE(resp.Body, out, subCtx.Done())
		_ = resp.Body.Close()
	}()

	stop := func() error {
		cancel()
		select {
		case e := <-errCh:
			return e
		case <-time.After(2 * time.Second):
			return nil
		}
	}
	return out, stop, nil
}

// readSSE parses an SSE stream into serverEvent frames. Frames are separated
// by a blank line; only `data:` lines are consumed (opencode does not use
// event: names on this feed). A `data:` payload is a JSON serverEvent.
func readSSE(r io.Reader, out chan<- serverEvent, done <-chan struct{}) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var data strings.Builder
	flush := func() bool {
		if data.Len() == 0 {
			return true
		}
		payload := data.String()
		data.Reset()
		var ev serverEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			// Skip un-decodable frames rather than tearing down the stream —
			// opencode emits control frames the adapter does not model.
			return true
		}
		select {
		case out <- ev:
			return true
		case <-done:
			return false
		}
	}
	for scanner.Scan() {
		select {
		case <-done:
			return nil
		default:
		}
		line := scanner.Text()
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(line[len("data:"):]))
		default:
			// ignore id:/event:/retry:/comment lines
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
