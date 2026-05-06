package gemini

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// eventBufferSize matches provider/claude. Sized to absorb a burst of
// streamed text events without backpressuring the HTTP body reader.
const eventBufferSize = 64

// maxLineSize caps how big a single SSE data line can be. Gemini
// streams a small chunk per network frame, so 1 MiB is generous.
const maxLineSize = 1 * 1024 * 1024

// sessionParams bundles the pre-spawn inputs Provider.Spawn passes
// into startSession. Pulled out so handle.go stays free of *Provider
// references.
type sessionParams struct {
	apiKey    string
	url       string
	body      []byte
	client    *http.Client
	sessionID string
}

// Handle is the agent.Handle implementation backed by an in-flight
// HTTPS streaming response from generativelanguage.googleapis.com.
//
// One Handle owns one *http.Response.Body. Stop closes the body which
// unblocks the SSE reader goroutine; the goroutine then closes the
// events channel exactly once.
type Handle struct {
	sessionID string

	events   chan agent.Event
	resp     *http.Response
	cancel   context.CancelFunc
	shutdown chan struct{}

	stopOnce     sync.Once
	stopErr      error
	closeOnce    sync.Once
	eventsClosed atomic.Bool
}

// startSession opens the HTTPS streaming POST and returns a wired
// Handle. The InitEvent is enqueued before the goroutine launches so
// callers always observe it first.
func startSession(ctx context.Context, p sessionParams) (*Handle, error) {
	// Tie the request to a derived ctx we can cancel from Stop.
	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.url, bytes.NewReader(p.body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: build request: %w", agent.ErrSpawnFailed, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", p.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: http: %w", agent.ErrSpawnFailed, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("%w: gemini HTTP %d: %s",
			agent.ErrSpawnFailed, resp.StatusCode, bytes.TrimSpace(body))
	}

	h := &Handle{
		sessionID: p.sessionID,
		events:    make(chan agent.Event, eventBufferSize),
		resp:      resp,
		cancel:    cancel,
		shutdown:  make(chan struct{}),
	}

	// Init event first; channel is buffered so this never blocks.
	h.events <- agent.InitEvent{SessionID: p.sessionID}

	go h.readSSE()

	return h, nil
}

// SessionID returns the synthetic session id assigned at spawn time.
// Always non-empty for the lifetime of the Handle.
func (h *Handle) SessionID() string { return h.sessionID }

// Events returns the read-only event channel. Closed by the SSE
// reader goroutine after a terminal event is observed (or after Stop
// closes the body, whichever comes first).
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject returns ErrUnsupported. Gemini's REST endpoint is stateless;
// follow-up turns require a fresh Spawn with the previous turn folded
// into Spec.Prompt.
func (*Handle) Inject(_ context.Context, _ string) error {
	return fmt.Errorf("provider/gemini: Inject: %w", agent.ErrUnsupported)
}

// Stop aborts the streaming response. Idempotent. Safe after the
// events channel has closed.
func (h *Handle) Stop(_ context.Context) error {
	h.stopOnce.Do(func() {
		close(h.shutdown)
		h.cancel()              // unblocks Body.Read
		_ = h.resp.Body.Close() // belt-and-braces
	})
	return h.stopErr
}

// readSSE consumes the SSE response body, decoding each `data: ...`
// line via mapChunk and forwarding events. Closes the events channel
// exactly once when the loop exits.
//
// Wire shape (Gemini SSE):
//
//	data: {"candidates":[...],"usageMetadata":{...}}
//	data: {"candidates":[{"finishReason":"STOP"}], ...}
//
// Empty lines and lines without the `data: ` prefix are ignored
// (per the SSE spec). On EOF without a terminal event we synthesise
// an ErrorEvent so the runner observes a failure rather than waiting
// silently.
func (h *Handle) readSSE() {
	defer h.closeEvents()
	defer func() { _ = h.resp.Body.Close() }()

	scanner := bufio.NewScanner(h.resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	terminal := false
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		// SSE event boundary is a blank line; we don't need to track
		// event types because Gemini streams one event-per-line of
		// type "message" with the data payload already parseable.
		const dataPrefix = "data: "
		if !bytes.HasPrefix(raw, []byte(dataPrefix)) {
			continue
		}
		data := bytes.TrimSpace(raw[len(dataPrefix):])
		if len(data) == 0 {
			continue
		}
		// Gemini uses `data: [DONE]` only on some endpoints; check
		// defensively.
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}

		evs, isTerminal := mapChunk(data)
		for _, ev := range evs {
			if !h.sendEvent(ev) {
				return
			}
		}
		if isTerminal {
			terminal = true
			break
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		// Only emit a transport-error event if Stop hasn't already
		// initiated shutdown — otherwise the user sees a confusing
		// "scan: context canceled" trailing event.
		select {
		case <-h.shutdown:
			return
		default:
		}
		_ = h.sendEvent(agent.ErrorEvent{
			Message: fmt.Sprintf("provider/gemini: stream read: %v", err),
			Code:    "stream_read",
		})
		return
	}
	if !terminal {
		select {
		case <-h.shutdown:
			return
		default:
		}
		_ = h.sendEvent(agent.ErrorEvent{
			Message: "gemini stream ended without finishReason",
			Code:    "spawn_no_result",
		})
	}
}

// sendEvent forwards one event onto the public events channel.
// Returns false when shutdown has been signalled — callers exit
// promptly so a slow consumer can't deadlock the goroutine.
func (h *Handle) sendEvent(ev agent.Event) bool {
	if h.eventsClosed.Load() {
		return false
	}
	select {
	case h.events <- ev:
		return true
	case <-h.shutdown:
		return false
	}
}

// closeEvents closes the events channel exactly once.
func (h *Handle) closeEvents() {
	h.closeOnce.Do(func() {
		h.eventsClosed.Store(true)
		close(h.events)
	})
}
