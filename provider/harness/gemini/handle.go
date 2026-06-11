package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/RenseiAI/donmai/agent"
)

// eventBufferSize matches provider/claude. Sized to absorb a burst of
// text + tool events without backpressuring the driver goroutine.
const eventBufferSize = 64

// maxResponseSize caps how big a single generateContent JSON response
// can be. Gemini responses with a 1M-context input are still small on
// the output side; 8 MiB is generous.
const maxResponseSize = 8 * 1024 * 1024

// ErrSessionNotReady is returned by Handle.Inject when called before the
// driver has started (no InitEvent observed yet). Callers should consume
// events from Events() until they observe an agent.InitEvent first.
var ErrSessionNotReady = errors.New("provider/gemini: session not ready; wait for InitEvent before calling Inject")

// sessionParams bundles the pre-spawn inputs Provider.Spawn passes into
// startSession.
type sessionParams struct {
	apiKey string
	// turnURL is the fully-resolved generateContent URL for this session
	// (base endpoint + host-shaped path + model), computed once by
	// Provider.Spawn via spawnURL so the driver never re-derives routing.
	turnURL   string
	model     string
	plan      spawnPlan
	client    *http.Client
	sessionID string
	// cwd / env configure the session-local toolExecutor (working
	// directory native tools run in + per-session environment for Bash).
	cwd string
	env map[string]string
	// mcp is the per-session MCP bridge (nil when the spec declares no
	// MCP servers). The executor routes mcp__* calls through it; the
	// driver closes it on exit.
	mcp *mcpBridge
	// maxTurns is the maximum number of generateContent round-trips
	// (agentic turns) allowed before the driver terminates with an
	// error_max_turns result. 0 means uncapped.
	maxTurns int
}

// injectMsg carries an injected steering message into the driver loop.
// It is appended verbatim as a user turn after a completed (final) turn.
//
// Tool results are NOT delivered through Inject: the Gemini provider
// runs functionCalls itself via the session-local toolExecutor and
// folds the functionResponse back into the loop automatically (see
// drive → executeToolCalls). Inject is steering-only.
type injectMsg struct {
	// text is the raw injected payload, appended verbatim as a user turn.
	text string
}

// Handle is the agent.Handle implementation backed by a multi-turn
// generateContent conversation. The Handle owns the contents history
// (the REST endpoint is stateless) and a single driver goroutine that
// runs turns, surfaces events, executes the model's functionCalls via a
// session-local toolExecutor (auto-folding the functionResponse back
// into the loop), and pauses for post-completion steering injects.
type Handle struct {
	sessionID string
	apiKey    string
	turnURL   string
	model     string
	plan      spawnPlan
	client    *http.Client

	// executor runs the model's functionCalls (native filesystem / shell
	// tools + bridged MCP tools) and folds the functionResponse back into
	// the loop. The Gemini REST endpoint does not execute tools itself.
	executor *toolExecutor

	// mcp is the per-session MCP bridge (nil without MCP servers); closed
	// by the driver on exit so server subprocesses/sessions are released.
	mcp *mcpBridge

	// maxTurns caps the number of generateContent round-trips (agentic
	// turns). 0 means uncapped. When the cap is hit the driver emits a
	// terminal ResultEvent with Success=false / ErrorSubtype
	// "error_max_turns" so the runner acceptance gate can distinguish a
	// deliberate cap from a model-side truncation.
	maxTurns int

	events chan agent.Event
	cancel context.CancelFunc

	// inject delivers post-completion steering turns to the driver.
	// Buffered so a caller's Inject does not block on the driver.
	inject chan injectMsg

	// contentsMu guards the conversation history.
	contentsMu sync.Mutex
	contents   []requestContent

	// started flips true once the driver goroutine has launched (and the
	// InitEvent has been enqueued). Read by Inject for ErrSessionNotReady.
	started atomic.Bool

	state *turnState

	// shutdown is closed by Stop to unblock the driver and any pending
	// Inject. The driver closes the events channel on exit.
	shutdownOnce sync.Once
	shutdown     chan struct{}

	closeOnce    sync.Once
	eventsClosed atomic.Bool
}

// startSession builds a wired Handle, enqueues the InitEvent, and
// launches the driver goroutine. The InitEvent is enqueued before the
// goroutine launches so callers always observe it first.
func startSession(ctx context.Context, p sessionParams) (*Handle, error) {
	driverCtx, cancel := context.WithCancel(ctx) //nolint:gosec // cancel is retained on Handle.cancel and invoked by Stop()/driver exit (h.cancel())

	client := p.client
	if client == nil {
		client = http.DefaultClient
	}

	h := &Handle{
		sessionID: p.sessionID,
		apiKey:    p.apiKey,
		turnURL:   p.turnURL,
		model:     p.model,
		plan:      p.plan,
		client:    client,
		executor:  newToolExecutor(p.cwd, p.env, p.mcp),
		mcp:       p.mcp,
		maxTurns:  p.maxTurns,
		events:    make(chan agent.Event, eventBufferSize),
		cancel:    cancel,
		inject:    make(chan injectMsg, 4),
		contents:  append([]requestContent(nil), p.plan.initialContents...),
		shutdown:  make(chan struct{}),
		state:     &turnState{model: p.model},
	}

	// Init event first; channel is buffered so this never blocks.
	h.events <- agent.InitEvent{SessionID: p.sessionID}
	h.started.Store(true)

	go h.drive(driverCtx)

	return h, nil
}

// SessionID returns the synthetic session id assigned at spawn time.
func (h *Handle) SessionID() string { return h.sessionID }

// Events returns the read-only event channel. Closed by the driver
// goroutine on exit (terminal turn + no further injects, or Stop).
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject appends a post-completion steering turn (a plain user message)
// and re-drives the conversation.
//
// Tool results are NOT delivered through Inject — the provider runs
// functionCalls itself via the session-local toolExecutor and folds the
// functionResponse back automatically (see drive → executeToolCalls).
// Inject is therefore steering-only: it appends text verbatim as a user
// turn after a completed (final) turn.
//
// Returns ErrSessionNotReady if the driver has not started. Returns nil
// once the message is queued; the caller observes the resulting events
// on Events().
func (h *Handle) Inject(ctx context.Context, text string) error {
	if !h.started.Load() {
		return fmt.Errorf("provider/gemini: Inject: %w", ErrSessionNotReady)
	}
	select {
	case h.inject <- injectMsg{text: text}:
		return nil
	case <-h.shutdown:
		return fmt.Errorf("provider/gemini: Inject: %w", agent.ErrUnsupported)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop aborts the conversation. Idempotent. Safe after the events
// channel has closed.
func (h *Handle) Stop(_ context.Context) error {
	h.shutdownOnce.Do(func() {
		close(h.shutdown)
		h.cancel()
	})
	return nil
}

// drive is the single conversation-driver goroutine. It runs turns,
// surfaces events, and pauses for injected tool results / steering.
// Closes the events channel exactly once on exit.
func (h *Handle) drive(ctx context.Context) {
	defer h.closeEvents()
	defer h.mcp.Close() // nil-safe; releases bridged MCP server connections

	for {
		select {
		case <-ctx.Done():
			h.sendEvent(agent.ErrorEvent{
				Message: "provider/gemini: session cancelled: " + ctx.Err().Error(),
				Code:    "context_cancelled",
			})
			return
		case <-h.shutdown:
			return
		default:
		}

		body, err := h.buildTurnBody()
		if err != nil {
			h.sendEvent(agent.ErrorEvent{
				Message: fmt.Sprintf("provider/gemini: build turn: %v", err),
				Code:    "build_turn",
			})
			return
		}

		respBody, err := h.postTurn(ctx, body)
		if err != nil {
			// Honor shutdown so a Stop-cancelled request does not emit a
			// confusing trailing transport error.
			select {
			case <-h.shutdown:
				return
			default:
			}
			h.sendEvent(agent.ErrorEvent{
				Message: fmt.Sprintf("provider/gemini: generateContent: %v", err),
				Code:    "http",
			})
			return
		}

		turn := mapResponse(respBody, h.state)
		for _, ev := range turn.events {
			if !h.sendEvent(ev) {
				return
			}
		}

		// MaxTurns cap: if the caller set a turn ceiling and we have
		// reached it, terminate regardless of outcome. This prevents
		// unbounded API spend when a model loops on functionCalls. The
		// cap applies after the turn's events are emitted so the caller
		// always sees the model output that consumed the final turn.
		if h.maxTurns > 0 && h.state.turnCount >= h.maxTurns {
			h.sendEvent(agent.ResultEvent{
				Success:      false,
				Message:      fmt.Sprintf("provider/gemini: max turns reached (%d)", h.maxTurns),
				Errors:       []string{fmt.Sprintf("max turns exceeded: limit %d", h.maxTurns)},
				ErrorSubtype: "error_max_turns",
				Cost:         buildCost(h.state),
			})
			return
		}

		switch turn.outcome {
		case outcomeError:
			h.sendEvent(turn.result)
			return
		case outcomeContinue:
			// Model wants tools. Append its turn to history, run each
			// functionCall via the session-local executor, surface the
			// results, and fold the functionResponse turn back in. No
			// runner Inject is required — the Gemini REST endpoint does
			// not execute tools, so the provider does it here.
			h.appendModelTurn(turn.modelParts)
			if !h.executeToolCalls(ctx, turn.funcCalls) {
				return
			}
			// Loop: the tool-result turn was appended; run the next turn.
		case outcomeFinal:
			// Append the model's final turn so a post-completion steering
			// inject continues from the full history.
			h.appendModelTurn(turn.modelParts)
			h.sendEvent(turn.result)
			// Keep the channel open for post-completion steering. Wait
			// for an inject (user turn) or shutdown.
			if !h.awaitSteering(ctx) {
				return
			}
		}
	}
}

// executeToolCalls runs every pending functionCall via the session-local
// executor, surfaces a ToolResultEvent per call, and folds the matching
// functionResponse parts back into the conversation history as a single
// tool-result turn. Returns false when the loop should exit (shutdown /
// cancellation observed before the results were folded in).
//
// This is the piece that makes autonomous Gemini sessions complete: the
// REST endpoint returns functionCalls but does not run them, so the
// provider executes them in-box and re-drives without waiting for the
// runner to deliver results.
func (h *Handle) executeToolCalls(ctx context.Context, calls []candidateFuncCall) bool {
	parts := make([]requestPart, 0, len(calls))
	for _, c := range calls {
		// Honor shutdown / cancellation between calls so a Stop during a
		// multi-tool turn exits promptly.
		select {
		case <-ctx.Done():
			h.sendEvent(agent.ErrorEvent{
				Message: "provider/gemini: cancelled while executing tools: " + ctx.Err().Error(),
				Code:    "context_cancelled",
			})
			return false
		case <-h.shutdown:
			return false
		default:
		}

		res := h.executor.execute(ctx, c)
		if !h.sendEvent(agent.ToolResultEvent{
			ToolName:  c.Name,
			ToolUseID: c.ID,
			Content:   res.text,
			IsError:   res.isError,
		}) {
			return false
		}
		parts = append(parts, requestPart{
			FunctionResponse: &functionResponse{
				ID:       c.ID,
				Name:     c.Name,
				Response: res.response,
			},
		})
	}

	h.appendToolResultTurn(parts)
	return true
}

// awaitSteering blocks after a final turn for a post-completion steering
// inject (appended as a user turn) or shutdown. Returns false when the
// loop should exit (shutdown / cancellation).
func (h *Handle) awaitSteering(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-h.shutdown:
		return false
	case msg := <-h.inject:
		h.appendUserTurn(msg.text)
		return true
	}
}

// buildTurnBody marshals the current request body from the static plan
// scaffold + the running contents history.
func (h *Handle) buildTurnBody() ([]byte, error) {
	h.contentsMu.Lock()
	contents := append([]requestContent(nil), h.contents...)
	h.contentsMu.Unlock()

	body := requestBody{
		Contents:          contents,
		SystemInstruction: h.plan.systemInstruction,
		GenerationConfig:  h.plan.generationConfig,
		Tools:             h.plan.tools,
		ToolConfig:        h.plan.toolConfig,
	}
	return json.Marshal(body)
}

// postTurn POSTs one generateContent request and returns the response
// body. Non-200 responses surface as an error wrapping the status + body
// tail.
func (h *Handle) postTurn(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.turnURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", h.apiKey)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(tail))
	}

	return io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
}

// appendModelTurn appends the model's turn (text + functionCall parts)
// to the conversation history with role "model".
func (h *Handle) appendModelTurn(parts []requestPart) {
	if len(parts) == 0 {
		return
	}
	h.contentsMu.Lock()
	h.contents = append(h.contents, requestContent{Role: "model", Parts: parts})
	h.contentsMu.Unlock()
}

// appendUserTurn appends a plain user message turn (steering).
func (h *Handle) appendUserTurn(text string) {
	h.contentsMu.Lock()
	h.contents = append(h.contents, requestContent{
		Role:  "user",
		Parts: []requestPart{{Text: text}},
	})
	h.contentsMu.Unlock()
}

// appendToolResultTurn appends the tool-result turn carrying one
// functionResponse part per executed call.
//
// CRITICAL wire-role fact: the public generativelanguage.googleapis.com
// generateContent REST API accepts ONLY "user" or "model" in
// Content.role — the legacy "function"/"tool" roles were removed. The
// functionResponse parts therefore ride inside a user-role turn (only
// the model turn carrying the functionCall is role "model"). A
// role="function" turn returns HTTP 400 ("Please ensure that function
// response turn comes immediately after a function call turn") against
// the live API. Verified against ai.google.dev/gemini-api/docs/function-calling.
func (h *Handle) appendToolResultTurn(parts []requestPart) {
	if len(parts) == 0 {
		return
	}
	h.contentsMu.Lock()
	h.contents = append(h.contents, requestContent{Role: "user", Parts: parts})
	h.contentsMu.Unlock()
}

// sendEvent forwards one event onto the events channel. Returns false
// when shutdown has been signalled so the driver exits promptly.
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
