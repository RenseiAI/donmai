package ollama

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// eventBufferSize is the buffered capacity of the events channel.
// Sized to absorb a burst of incremental content chunks without
// backpressuring the HTTP body reader. The runner is expected to drain
// promptly; the buffer just smooths over short consumer hiccups.
const eventBufferSize = 64

// Handle is the agent.Handle implementation backed by one streaming
// POST /api/chat HTTP request. The handle owns:
//
//   - the request's context (separate from spawn ctx so Stop can
//     cancel the request without canceling the spawn ctx the caller
//     may still hold),
//   - one goroutine that reads the NDJSON body line-by-line and
//     forwards mapped events onto the events channel,
//   - a sync.Once-guarded Stop path that closes the request, drains
//     the body, and closes the events channel exactly once.
//
// SessionID returns a synthetic per-handle id ("ollama-session-<8 hex>")
// because Ollama has no server-side session concept. The synthetic id
// is stable for the handle's lifetime and is emitted on the InitEvent.
type Handle struct {
	sessionID string
	resp      *http.Response
	cancel    context.CancelFunc

	events  chan agent.Event
	logger  *slog.Logger

	// shutdown is closed by Stop to broadcast termination to the body
	// reader (which selects on it before each channel send).
	shutdown chan struct{}

	// done is closed after the body reader exits and the response
	// body has been closed. Stop blocks on done so callers see the
	// reader fully drained before Stop returns.
	done chan struct{}

	// stopOnce / closeOnce gate the idempotent shutdown path.
	stopOnce  sync.Once
	stopErr   error
	closeOnce sync.Once

	// eventsClosed guards close(h.events) against races between the
	// reader's natural termination and Stop's forced close.
	eventsClosed atomic.Bool
	eventsMu     sync.RWMutex
}

// startStream is the internal entry point for Spawn. It posts the
// request body, validates the HTTP response, and wires up the body
// reader goroutine + Handle. On any pre-stream failure (transport
// error, non-2xx) it returns a wrapped agent.ErrSpawnFailed and
// guarantees the response body (if any) is closed.
func (p *Provider) startStream(parentCtx context.Context, body []byte, _ agent.Spec) (*Handle, error) {
	// Derive a cancelable child ctx for the request. Stop calls
	// cancel() to abort the in-flight HTTP request without canceling
	// the parent ctx (which may still belong to the runner's
	// outer-scope work).
	reqCtx, cancel := context.WithCancel(parentCtx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: build chat request: %w", agent.ErrSpawnFailed, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: POST %s/api/chat: %w", agent.ErrSpawnFailed, p.endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a small slice of the body to surface Ollama's error
		// message; ollama uses {"error":"..."} on 4xx.
		tail := readTail(resp.Body, 1024)
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("%w: POST %s/api/chat returned HTTP %d: %s", agent.ErrSpawnFailed, p.endpoint, resp.StatusCode, tail)
	}

	h := &Handle{
		sessionID: newSessionID(),
		resp:      resp,
		cancel:    cancel,
		events:    make(chan agent.Event, eventBufferSize),
		logger:    slog.With("provider", "ollama", "endpoint", p.endpoint),
		shutdown:  make(chan struct{}),
		done:      make(chan struct{}),
	}

	go h.readBody()

	return h, nil
}

// SessionID returns the synthetic session id assigned at Spawn time.
func (h *Handle) SessionID() string { return h.sessionID }

// Events returns the read-only event channel. Closed by Stop after the
// body reader has exited.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject returns agent.ErrUnsupported. Ollama has no mid-session
// injection mechanism — the chat request is a single HTTP exchange.
// Per the capability matrix, the runner gates on
// SupportsMessageInjection before calling Inject; this branch is the
// safety net for callers that bypass the gate.
func (*Handle) Inject(_ context.Context, _ string) error {
	return fmt.Errorf("provider/ollama: Inject: %w (SupportsMessageInjection=false)", agent.ErrUnsupported)
}

// Stop aborts the streaming request. Idempotent; safe to call after
// the events channel has closed.
//
// Mechanics: Stop cancels the request's ctx (which causes the HTTP
// transport to close the connection and unblock the body reader),
// waits for the reader goroutine to finish, then ensures the events
// channel is closed exactly once. ctx is honored as the upper bound on
// the wait — if the reader does not exit within ctx's deadline, Stop
// returns ctx.Err and the reader goroutine drains in the background
// (the connection is already canceled, so it cannot leak indefinitely).
func (h *Handle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		h.stopErr = h.doStop(ctx)
	})
	return h.stopErr
}

func (h *Handle) doStop(ctx context.Context) error {
	close(h.shutdown)
	h.cancel()

	// Always close the events channel before returning, even if the
	// reader did not finish (it will exit shortly because the request
	// ctx is canceled).
	defer h.closeEvents()

	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeEvents closes the events channel exactly once.
func (h *Handle) closeEvents() {
	h.closeOnce.Do(func() {
		h.eventsMu.Lock()
		defer h.eventsMu.Unlock()
		h.eventsClosed.Store(true)
		close(h.events)
	})
}

// sendEvent delivers one event onto the public channel. Drops the
// event silently when shutdown is in flight or the channel has already
// closed. Producers always go through sendEvent so a slow consumer
// during Stop does not block the body reader.
func (h *Handle) sendEvent(ev agent.Event) {
	h.eventsMu.RLock()
	defer h.eventsMu.RUnlock()
	if h.eventsClosed.Load() {
		return
	}
	select {
	case h.events <- ev:
	case <-h.shutdown:
	}
}

// readBody is the per-handle goroutine that drains the response body.
// It emits exactly one InitEvent (on the first non-empty line — which
// is ollama's first chunk), then 0..N AssistantTextEvent / ErrorEvent
// values, then the terminal ResultEvent, then closes the events
// channel via doStop. Bail-out paths:
//
//   - scanner error other than EOF → ErrorEvent + close.
//   - shutdown signaled            → return immediately, doStop
//     handles the close.
//   - terminal ResultEvent observed → run completed; close cleanly
//     (no synthetic stop event needed).
func (h *Handle) readBody() {
	defer close(h.done)
	defer func() { _ = h.resp.Body.Close() }()

	scanner := bufio.NewScanner(h.resp.Body)
	// Each NDJSON line is one chunk; size the buffer for the largest
	// reasonable terminal-chunk payload. 4MiB matches the claude
	// provider's stdout reader limit so behavior is uniform across
	// providers under "long line" stress.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	initEmitted := false
	terminal := false

	for scanner.Scan() {
		select {
		case <-h.shutdown:
			return
		default:
		}
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		// Copy: scanner reuses its buffer.
		line := append([]byte(nil), raw...)

		if !initEmitted {
			// Emit the synthetic InitEvent before any content so
			// downstream consumers see the canonical preamble; the
			// session id is the synthetic per-handle id.
			h.sendEvent(agent.InitEvent{SessionID: h.sessionID})
			initEmitted = true
		}
		evs, err := mapLine(line)
		if err != nil {
			h.sendEvent(agent.ErrorEvent{
				Message: fmt.Sprintf("provider/ollama: malformed chunk: %v", err),
				Code:    "ollama_decode_error",
			})
			h.closeEvents()
			return
		}
		for _, ev := range evs {
			if _, ok := ev.(agent.ResultEvent); ok {
				terminal = true
			}
			h.sendEvent(ev)
		}
		if terminal {
			h.closeEvents()
			return
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		// Connection-canceled errors during shutdown are expected; only
		// surface unexpected ones.
		select {
		case <-h.shutdown:
			return
		default:
		}
		h.sendEvent(agent.ErrorEvent{
			Message: fmt.Sprintf("provider/ollama: stream read: %v", err),
			Code:    "ollama_stream_read",
		})
		h.closeEvents()
		return
	}
	if !terminal {
		// EOF without a terminal chunk — surface the failure so the
		// runner does not wait silently. Same pattern the claude
		// provider uses for its spawn_no_result synthetic.
		h.sendEvent(agent.ErrorEvent{
			Message: "ollama exited stream without terminal done=true chunk",
			Code:    "spawn_no_result",
		})
		h.closeEvents()
	}
}

// readTail reads up to n bytes from r and returns them as a string.
// Used for non-2xx error bodies; bounded so a hostile server can't
// blow memory.
func readTail(r io.Reader, n int) string {
	buf := make([]byte, n)
	read, _ := io.ReadFull(r, buf)
	return string(bytes.TrimSpace(buf[:read]))
}

// newSessionID returns a synthetic id ("ollama-session-<8 hex bytes>").
// Matches the stub provider's id shape for consistency in logs.
func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ollama-session-fallback"
	}
	return "ollama-session-" + hex.EncodeToString(b[:])
}
