package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// DefaultBinary is the executable name probed on $PATH at construction.
// OpenCode can be run as a standalone CLI with `opencode run`.
const DefaultBinary = "opencode"

// DefaultEndpoint is the URL probed at construction when no explicit
// endpoint or env var is supplied. Mirrors the OpenCode CLI's default
// server bind address.
const DefaultEndpoint = "http://localhost:7700"

// EnvEndpoint overrides DefaultEndpoint when set.
const EnvEndpoint = "OPENCODE_ENDPOINT"

// EnvAPIKey is the optional bearer-token env var NAME. Forwarded for
// future hosted variants; not required for the default localhost
// server.
const EnvAPIKey = "OPENCODE_API_KEY" //nolint:gosec // G101: env-var name, not a credential

// DefaultProbeTimeout caps the probe HTTP GET at construction when
// not in CLI-binary mode.
const DefaultProbeTimeout = 2 * time.Second

// eventBufferSize matches provider/claude — sized to absorb a burst
// of fan-out events without backpressuring the stdout reader.
const eventBufferSize = 64

// stderrBufferSize caps how many bytes of stderr we retain for
// post-mortem diagnostics on unexpected exits.
const stderrBufferSize = 8 * 1024

// stopGracePeriod is the deadline between SIGTERM and SIGKILL.
const stopGracePeriod = 5 * time.Second

// Options configures Provider construction.
type Options struct {
	// Binary names the opencode CLI executable to invoke. When empty,
	// DefaultBinary is used. Tests inject a fake-CLI script path here.
	// When set (or when DefaultBinary is on PATH), the provider uses
	// CLI-spawn mode (opencode run --format json) rather than the HTTP
	// server mode.
	Binary string

	// Endpoint overrides the OpenCode server URL for HTTP-server mode.
	// Empty falls back to $OPENCODE_ENDPOINT then DefaultEndpoint.
	// Ignored when BinaryMode is true or a binary is found.
	Endpoint string

	// APIKey is an optional bearer token. Empty falls back to
	// $OPENCODE_API_KEY (which may also be empty).
	APIKey string

	// HTTPClient is used for the probe call in HTTP-server mode.
	// Defaults to a client with DefaultProbeTimeout.
	HTTPClient *http.Client

	// Getenv overrides the environment lookup. Defaults to os.Getenv.
	Getenv func(string) string

	// LookPath overrides the binary-resolution function. Defaults to
	// exec.LookPath.
	LookPath func(name string) (string, error)

	// SkipProbe disables the construction-time liveness check in
	// HTTP-server mode. Tests use this when the goal is to assert
	// capability / Spawn behavior without standing up a server.
	SkipProbe bool
}

// Provider is the agent.Provider implementation for OpenCode.
// It supports two execution modes:
//
//  1. CLI mode (preferred): Spawn execs `opencode run --format json`
//     which streams NDJSON events to stdout.
//  2. HTTP-server mode (legacy/fallback): operator runs
//     `opencode serve`; the provider posts tasks to the REST API
//     and streams WebSocket events. Not yet wired — Spawn returns
//     ErrSpawnFailed in this mode until the REST client lands.
type Provider struct {
	binary   string // resolved CLI path, or "" for HTTP-server mode
	endpoint string // HTTP server endpoint (HTTP-server mode only)
	apiKey   string
}

// New constructs a Provider.
//
// Construction order:
//  1. Probe for the `opencode` CLI binary on PATH. If found, use CLI
//     mode — no HTTP probe needed.
//  2. If the binary is not on PATH, fall back to HTTP-server mode:
//     probe the OpenCode HTTP server at the configured endpoint.
//     Connection refused or 5xx → wrapped agent.ErrProviderUnavailable.
func New(opts Options) (*Provider, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = defaultGetenv
	}
	lookup := opts.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}

	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = getenv(EnvAPIKey)
	}

	// Try CLI binary first.
	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	resolved, lookErr := lookup(binary)
	if lookErr == nil {
		// CLI binary is available — use CLI mode.
		return &Provider{binary: resolved, apiKey: apiKey}, nil
	}

	// Fall back to HTTP-server mode.
	endpoint := strings.TrimRight(opts.Endpoint, "/")
	if endpoint == "" {
		endpoint = strings.TrimRight(getenv(EnvEndpoint), "/")
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	if !opts.SkipProbe {
		client := opts.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: DefaultProbeTimeout}
		}
		if err := probeLive(client, endpoint, apiKey); err != nil {
			return nil, fmt.Errorf(
				"%w: opencode CLI %q not on PATH and HTTP server at %s unreachable "+
					"(install CLI: `npm i -g opencode-ai` or start server with `opencode serve`): %v",
				agent.ErrProviderUnavailable, binary, endpoint, err,
			)
		}
	}

	return &Provider{endpoint: endpoint, apiKey: apiKey}, nil
}

// probeLive issues a GET against the server's root and accepts any
// non-5xx response as a successful liveness check.
func probeLive(client *http.Client, endpoint, apiKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	if resp.StatusCode >= 500 {
		return errors.New("HTTP " + resp.Status)
	}
	return nil
}

// Name returns ProviderOpenCode. Stable for the lifetime of the Provider.
func (*Provider) Name() agent.ProviderName { return agent.ProviderOpenCode }

// Capabilities returns the v1.0.0 capability matrix for the OpenCode
// CLI runner.
func (*Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            false, // future: opencode run --session <id> --continue
		SupportsSessionResume:               false, // future: same mechanism
		SupportsToolPlugins:                 false, // opencode manages its own tools
		NeedsBaseInstructions:               false,
		NeedsPermissionConfig:               false,
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 false,
		SupportsReasoningEffort:             true, // --variant flag maps to effort levels
		ToolPermissionFormat:                "claude",
		AcceptsAllowedToolsList:             false,
		AcceptsMcpServerSpec:                false, // opencode uses its own plugin system
		HumanLabel:                          "OpenCode",
	}
}

// Spawn starts a new OpenCode session.
//
// In CLI mode (opencode binary on PATH):
//
//	opencode run --format json --dangerously-skip-permissions
//	             [--dir <cwd>]
//	             [--model <id>]
//	             [--variant <effort>]
//
// The prompt is delivered via stdin. OpenCode's --format json mode
// streams NDJSON events that are translated to agent.Event values by
// mapOpenCodeLine.
//
// In HTTP-server mode: returns ErrSpawnFailed (not yet wired).
//
// On any pre-spawn failure (exec start) the provider returns an error
// wrapping agent.ErrSpawnFailed.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if p.binary == "" {
		return nil, fmt.Errorf(
			"%w: opencode HTTP-server runner not yet implemented — "+
				"install the opencode CLI binary (`npm i -g opencode-ai`) for CLI mode",
			agent.ErrSpawnFailed,
		)
	}
	h, err := p.spawnCLI(ctx, spec)
	if err != nil {
		// Return a typed nil to avoid the interface-nil trap: a non-nil
		// agent.Handle wrapping a nil *openCodeHandle would panic on
		// method calls in callers that check handle != nil.
		return nil, err
	}
	return h, nil
}

// spawnCLI starts `opencode run --format json` as a subprocess and
// returns a Handle backed by the running process.
func (p *Provider) spawnCLI(ctx context.Context, spec agent.Spec) (*openCodeHandle, error) {
	argv := buildOpenCodeArgs(spec)

	// nolint:gosec // p.binary is resolved at provider construction
	// from exec.LookPath; argv values come from a typed agent.Spec
	// and a closed set of CLI flags.
	cmd := exec.CommandContext(ctx, p.binary, argv...)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = composeEnv(os.Environ(), spec.Env)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error {
		signalProcessGroup(cmd, syscall.SIGKILL)
		return nil
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: stdin pipe: %v", agent.ErrSpawnFailed, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("%w: stdout pipe: %v", agent.ErrSpawnFailed, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("%w: stderr pipe: %v", agent.ErrSpawnFailed, err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("%w: cmd start: %v", agent.ErrSpawnFailed, err)
	}

	if spec.OnProcessSpawned != nil && cmd.Process != nil {
		spec.OnProcessSpawned(cmd.Process.Pid)
	}

	stderrBuf := &boundedBuffer{limit: stderrBufferSize, buf: make([]byte, 0, stderrBufferSize)}

	h := &openCodeHandle{
		binary:     p.binary,
		cwd:        spec.Cwd,
		cmd:        cmd,
		events:     make(chan agent.Event, eventBufferSize),
		logger:     slog.With("provider", "opencode", "pid", cmd.Process.Pid),
		stdoutPipe: stdout,
		stderrPipe: stderr,
		stderrBuf:  stderrBuf,
		shutdown:   make(chan struct{}),
		done:       make(chan struct{}),
	}

	go writePromptStdin(stdin, spec.Prompt, h.logger)
	go drainStderr(stderr, stderrBuf, h.logger)
	go h.readStdout()
	go h.watchCtx(ctx)

	return h, nil
}

// buildOpenCodeArgs translates an agent.Spec into argv for
// `opencode run --format json`.
//
// OpenCode flag mapping:
//
//	opencode run          → headless run subcommand
//	--format json         → NDJSON output (required for event streaming)
//	--dangerously-skip-permissions → auto-approve permissions
//	--dir <cwd>           → working directory on the remote server
//	--model <id>          → model in provider/model format
//	--variant <effort>    → reasoning effort (high, low, etc.)
//
// Prompt is delivered via stdin.
func buildOpenCodeArgs(spec agent.Spec) []string {
	argv := []string{
		"run",
		"--format", "json",
	}

	if spec.Autonomous {
		argv = append(argv, "--dangerously-skip-permissions")
	}

	if spec.Cwd != "" {
		argv = append(argv, "--dir", spec.Cwd)
	}

	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}

	if spec.Effort != "" {
		argv = append(argv, "--variant", string(spec.Effort))
	}

	return argv
}

// Resume always fails with agent.ErrUnsupported.
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/opencode: Resume: %w (SupportsSessionResume=false)", agent.ErrUnsupported)
}

// Shutdown is a no-op. There are no long-lived resources.
func (*Provider) Shutdown(_ context.Context) error { return nil }

// openCodeHandle is the agent.Handle implementation backed by an
// `opencode run` subprocess.
type openCodeHandle struct {
	binary     string
	cwd        string
	cmd        *exec.Cmd
	events     chan agent.Event
	logger     *slog.Logger
	stdoutPipe io.ReadCloser
	stderrPipe io.ReadCloser
	stderrBuf  *boundedBuffer

	// sessionID captured from the first step_start event.
	sessionID atomic.Pointer[string]

	stopOnce sync.Once
	stopErr  error

	shutdown chan struct{}

	eventsClosed atomic.Bool
	eventsMu     sync.RWMutex

	done    chan struct{}
	waitErr atomic.Pointer[error]

	terminal atomic.Bool
}

// SessionID returns the provider-native session id captured from the
// first NDJSON event. Empty until the first event fires.
func (h *openCodeHandle) SessionID() string {
	if v := h.sessionID.Load(); v != nil {
		return *v
	}
	return ""
}

// Events returns the read-only event channel. Closed by Stop() after
// the subprocess terminates.
func (h *openCodeHandle) Events() <-chan agent.Event { return h.events }

// Inject always returns agent.ErrUnsupported because
// SupportsMessageInjection is false for the OpenCode provider.
func (h *openCodeHandle) Inject(_ context.Context, _ string) error {
	return fmt.Errorf("provider/opencode: Inject: %w", agent.ErrUnsupported)
}

// Stop aborts the session. Idempotent; safe to call after the events
// channel has closed.
func (h *openCodeHandle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		h.stopErr = h.doStop(ctx)
	})
	return h.stopErr
}

func (h *openCodeHandle) doStop(ctx context.Context) error {
	close(h.shutdown)
	defer h.closeEvents()

	select {
	case <-h.done:
		return nil
	default:
	}

	if h.cmd != nil && h.cmd.Process != nil {
		signalProcessGroup(h.cmd, syscall.SIGTERM)
	}

	timer := time.NewTimer(stopGracePeriod)
	defer timer.Stop()

	select {
	case <-h.done:
	case <-timer.C:
		if h.cmd != nil && h.cmd.Process != nil {
			signalProcessGroup(h.cmd, syscall.SIGKILL)
		}
		<-h.done
	case <-ctx.Done():
		if h.cmd != nil && h.cmd.Process != nil {
			signalProcessGroup(h.cmd, syscall.SIGKILL)
		}
		<-h.done
	}
	return nil
}

func (h *openCodeHandle) sendEvent(ev agent.Event) {
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

func (h *openCodeHandle) closeEvents() {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	if h.eventsClosed.Load() {
		return
	}
	h.eventsClosed.Store(true)
	close(h.events)
}

func (h *openCodeHandle) watchCtx(ctx context.Context) {
	select {
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), stopGracePeriod+2*time.Second)
		defer cancel()
		_ = h.Stop(stopCtx)
	case <-h.shutdown:
	}
}

// readStdout is the goroutine that drains opencode's NDJSON stdout,
// decodes each line via mapOpenCodeLine, and forwards events onto the
// channel via sendEvent.
func (h *openCodeHandle) readStdout() {
	defer close(h.done)
	defer func() {
		err := h.cmd.Wait()
		if err != nil {
			h.waitErr.Store(&err)
		}
	}()

	// Force-close the pipe when shutdown fires to unblock the scanner.
	pipeCloseDone := make(chan struct{})
	go func() {
		defer close(pipeCloseDone)
		select {
		case <-h.done:
		case <-h.shutdown:
			_ = h.stdoutPipe.Close()
		}
	}()
	defer func() { <-pipeCloseDone }()

	scanner := bufio.NewScanner(h.stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	terminal := false
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		line := append([]byte(nil), raw...)
		for _, ev := range mapOpenCodeLine(line) {
			if ev == nil {
				continue
			}
			// Capture session ID from the first event that carries it.
			type sessionCarrier interface{ getSessionID() string }
			switch typed := ev.(type) {
			case agent.InitEvent:
				if typed.SessionID != "" {
					id := typed.SessionID
					h.sessionID.Store(&id)
				}
			case agent.ResultEvent:
				terminal = true
				_ = typed
			}
			h.sendEvent(ev)
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		h.sendEvent(agent.ErrorEvent{
			Message: fmt.Sprintf("provider/opencode: stdout scan: %v", err),
			Code:    "stdout_scan",
		})
		return
	}
	if terminal {
		return
	}
	stderrTail := h.stderrBuf.String()
	msg := "opencode exited without terminal result"
	if stderrTail != "" {
		msg = fmt.Sprintf("%s: stderr=%s", msg, stderrTail)
	}
	h.sendEvent(agent.ErrorEvent{
		Message: msg,
		Code:    "spawn_no_result",
	})
}

// ─── OpenCode NDJSON event mapping ──────────────────────────────────────────
//
// OpenCode --format json emits one JSON object per line. The known types are:
//
//	{"type":"step_start","sessionID":"ses_…","part":{"type":"step-start",…}}
//	{"type":"text","sessionID":"ses_…","part":{"type":"text","text":"…",…}}
//	{"type":"tool_use","sessionID":"ses_…","part":{"type":"tool","tool":"…","callID":"…","state":{…}}}
//	{"type":"step_finish","sessionID":"ses_…","part":{"type":"step-finish","reason":"stop"|"tool-calls",…,"tokens":{…},"cost":…}}
//
// We synthesize a single InitEvent from the first event's sessionID, then
// map text → AssistantTextEvent, tool_use → ToolUseEvent + ToolResultEvent
// (when state.status = "completed"), and step_finish with reason="stop" and
// no pending tool calls → ResultEvent.

// rawOpenCodeEnvelope is the discriminator-only decode for routing.
type rawOpenCodeEnvelope struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
}

// rawOpenCodePart decodes the nested "part" object.
type rawOpenCodePart struct {
	Type    string  `json:"type"`
	Text    string  `json:"text,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	Tokens  *tokens `json:"tokens,omitempty"`
	Cost    float64 `json:"cost"`
	// Tool-use part fields.
	Tool   string               `json:"tool,omitempty"`
	CallID string               `json:"callID,omitempty"`
	State  *rawOpenCodeToolState `json:"state,omitempty"`
}

// rawOpenCodeToolState carries a tool call's final state.
type rawOpenCodeToolState struct {
	Status string          `json:"status"` // "pending" | "running" | "completed" | "error"
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
}

type tokens struct {
	Total  int64 `json:"total"`
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

// initSent tracks per-session whether we have emitted a synthetic
// InitEvent for this handle. Since mapOpenCodeLine is a package-level
// function (not a method), the openCodeHandle itself injects this via
// a closure in readStdout.
//
// Because mapOpenCodeLine is stateless (it doesn't know about the
// handle), we track the session-id-seen flag in the readStdout caller
// and emit InitEvent inline there instead. The function returns a
// sentinel initSentinel type that readStdout recognises.

// mapOpenCodeLine decodes one NDJSON line from `opencode run --format json`
// and returns the corresponding agent.Event slice.
//
// Mapping:
//
//	step_start                         → (sessionID capture only; no event emitted here —
//	                                      the caller emits InitEvent on the first line)
//	text                               → AssistantTextEvent
//	tool_use (state.status=completed)  → ToolUseEvent + ToolResultEvent
//	tool_use (state.status=pending/running) → ToolUseEvent (no result yet)
//	step_finish (reason=stop)          → ResultEvent(success=true)
//	step_finish (reason=tool-calls)    → (internal step; no terminal event)
//	unknown / decode error             → ErrorEvent
func mapOpenCodeLine(line []byte) []agent.Event {
	var env rawOpenCodeEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/opencode: decode NDJSON envelope: %v", err),
			Code:    "decode_envelope",
			Raw:     json.RawMessage(line),
		}}
	}

	switch env.Type {
	case "step_start":
		// The sessionID is carried in every event. We synthesize a
		// single InitEvent from the first event; readStdout handles
		// the once-only emission by checking h.sessionID.
		// Return a lightweight SystemEvent carrying the session id so
		// readStdout can pick it up without extra state.
		return []agent.Event{agent.InitEvent{
			SessionID: env.SessionID,
			Raw:       json.RawMessage(line),
		}}

	case "text":
		var part rawOpenCodePart
		if err := json.Unmarshal(env.Part, &part); err != nil {
			return []agent.Event{agent.ErrorEvent{
				Message: fmt.Sprintf("provider/opencode: decode text part: %v", err),
				Code:    "decode_text",
				Raw:     json.RawMessage(line),
			}}
		}
		if part.Text == "" {
			return nil
		}
		return []agent.Event{agent.AssistantTextEvent{
			Text: part.Text,
			Raw:  json.RawMessage(line),
		}}

	case "tool_use":
		var part rawOpenCodePart
		if err := json.Unmarshal(env.Part, &part); err != nil {
			return []agent.Event{agent.ErrorEvent{
				Message: fmt.Sprintf("provider/opencode: decode tool_use part: %v", err),
				Code:    "decode_tool_use",
				Raw:     json.RawMessage(line),
			}}
		}
		return mapOpenCodeToolUse(line, &part)

	case "step_finish":
		var part rawOpenCodePart
		if err := json.Unmarshal(env.Part, &part); err != nil {
			return []agent.Event{agent.ErrorEvent{
				Message: fmt.Sprintf("provider/opencode: decode step_finish part: %v", err),
				Code:    "decode_step_finish",
				Raw:     json.RawMessage(line),
			}}
		}
		return mapOpenCodeStepFinish(line, &part)

	case "":
		return []agent.Event{agent.ErrorEvent{
			Message: "provider/opencode: NDJSON line missing top-level type",
			Code:    "missing_type",
			Raw:     json.RawMessage(line),
		}}
	default:
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown",
			Message: fmt.Sprintf("Unhandled opencode event type: %s", env.Type),
			Raw:     json.RawMessage(line),
		}}
	}
}

func mapOpenCodeToolUse(line []byte, part *rawOpenCodePart) []agent.Event {
	var input map[string]any
	if len(part.State.Input) > 0 {
		_ = json.Unmarshal(part.State.Input, &input)
	}

	toolEvent := agent.ToolUseEvent{
		ToolName:  part.Tool,
		ToolUseID: part.CallID,
		Input:     input,
		Raw:       json.RawMessage(line),
	}

	if part.State == nil || part.State.Status != "completed" {
		// Tool still pending/running — emit tool_use only.
		return []agent.Event{toolEvent}
	}

	// Tool completed — emit tool_use + tool_result.
	resultEvent := agent.ToolResultEvent{
		ToolName:  part.Tool,
		ToolUseID: part.CallID,
		Content:   part.State.Output,
		IsError:   part.State.Status == "error",
		Raw:       json.RawMessage(line),
	}
	return []agent.Event{toolEvent, resultEvent}
}

func mapOpenCodeStepFinish(line []byte, part *rawOpenCodePart) []agent.Event {
	switch part.Reason {
	case "stop":
		// Terminal step — emit ResultEvent(success=true).
		var cost *agent.CostData
		if part.Tokens != nil {
			cost = &agent.CostData{
				InputTokens:  part.Tokens.Input,
				OutputTokens: part.Tokens.Output,
				TotalCostUsd: part.Cost,
			}
		}
		return []agent.Event{agent.ResultEvent{
			Success: true,
			Cost:    cost,
			Raw:     json.RawMessage(line),
		}}
	case "tool-calls":
		// Intermediate step — agent is about to execute tools; not terminal.
		return nil
	case "error":
		return []agent.Event{agent.ResultEvent{
			Success:      false,
			Errors:       []string{"opencode step finished with error"},
			ErrorSubtype: "error",
			Raw:          json.RawMessage(line),
		}}
	default:
		// Unknown reason — emit as a system event so runners observe it.
		return []agent.Event{agent.SystemEvent{
			Subtype: "step_finish_unknown",
			Message: fmt.Sprintf("step_finish reason=%s", part.Reason),
			Raw:     json.RawMessage(line),
		}}
	}
}

// ─── Helpers shared with spawnCLI ────────────────────────────────────────────

func writePromptStdin(stdin io.WriteCloser, prompt string, logger *slog.Logger) {
	defer func() { _ = stdin.Close() }()
	if prompt == "" {
		return
	}
	if _, err := io.WriteString(stdin, prompt); err != nil {
		logger.Debug("provider/opencode: write prompt to stdin", "err", err)
	}
}

func drainStderr(r io.ReadCloser, buf *boundedBuffer, logger *slog.Logger) {
	defer func() { _ = r.Close() }()
	if _, err := io.Copy(buf, r); err != nil && !errors.Is(err, io.EOF) {
		logger.Debug("provider/opencode: drain stderr", "err", err)
	}
}

func composeEnv(parentEnv []string, specEnv map[string]string) []string {
	out := make([]string, 0, len(parentEnv)+len(specEnv))
	out = append(out, parentEnv...)
	for k, v := range specEnv {
		out = append(out, k+"="+v)
	}
	return out
}

// boundedBuffer accumulates the last N bytes written, dropping the
// oldest data once the limit is reached. Goroutine-safe.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	overflow := (len(b.buf) + len(p)) - b.limit
	if overflow > 0 {
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:len(b.buf)-overflow]
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
