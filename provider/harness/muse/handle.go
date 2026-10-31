package muse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// stopGracePeriod is the deadline between SIGTERM and SIGKILL on Stop.
const stopGracePeriod = 5 * time.Second

// stderrRetentionBytes bounds how much of a serve child's stderr is
// retained for post-mortem diagnostics. Borrowed from clijsonl's
// stderr tail (same bounded drop-oldest shape).
const stderrRetentionBytes = 8 * 1024

// streamProgressReservedSlots reserves buffer slots for terminal-adjacent
// events (assistant completion, turn usage, result) so a burst of
// best-effort streaming output can never fill the events channel
// and silently drop the verdict. Streaming output is best-effort: it is
// dropped once the buffer holds cap-reserved events, leaving the reserved
// tail for the authoritative completion and terminal result.
const streamProgressReservedSlots = 8

// Handle is one live MSP session: exactly one `muse serve` child, one
// session id, many turns. It implements agent.Handle.
//
// Lifecycle:
//  1. Provider.spawnHandle builds the Handle, starts the child, runs
//     the initialize/initialized handshake, then session/start (or
//     session/resume) and the first turn/start.
//  2. The forwarder goroutine consumes inbound notifications, answers
//     server-requests, translates events via mapNotification, and
//     publishes to events.
//  3. On terminal ResultEvent / ErrorEvent / ctx cancellation / host
//     crash, the forwarder closes events and stops the child.
type Handle struct {
	provider *Provider // nil for unit tests; non-nil in real use
	client   *Client
	bridge   *ApprovalBridge
	spec     agent.Spec
	plan     SpawnPlan

	// Per-call request timeout for initialize, session/start, etc.
	rpcTimeout time.Duration

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	// stderrBuf is the bounded drop-oldest tail of the child's
	// stderr, drained in the background so the child never blocks
	// on a full pipe.
	stderrBuf *boundedBuffer

	idMu      sync.RWMutex
	sessionID string
	// turnMu guards currentTurnID: the foreground turn id, used by
	// Stop (turn/interrupt) and Inject (turn/steer expectedTurnId).
	turnMu         sync.RWMutex
	currentTurnID  string
	workspaceRoots []string

	events   chan agent.Event
	notifyCh chan notification

	// decidedApprovals dedupes approval decisions by approvalId: the
	// host may present the same approval as both an
	// approval/requested notification and an approval/request
	// server-request. Only the forward goroutine touches this map.
	decidedApprovals map[string]struct{}

	closeOnce    sync.Once
	closedOnce   sync.Once
	closed       chan struct{}
	eventsMu     sync.RWMutex
	eventsClosed atomic.Bool
	// childStopping guards shutdownChild. It is a CAS flag, not a
	// sync.Once: shutdownChild stops the client, whose close hook
	// (failNow → closeEvents) re-enters shutdownChild on the same
	// goroutine, and a re-entrant Once.Do deadlocks.
	childStopping atomic.Bool
	// exited closes once watchExit has reaped the serve child. Nil
	// when no real process was started.
	exited chan struct{}

	// confinement is the plan a confined spawn runs under. The
	// rendered profile must stay on disk until the confined child has
	// exited, so the release runs when the session stops, not at
	// spawn. Nil for an unconfined spawn. sessionDir is the
	// caller-owned per-session scratch directory holding the tmp and
	// cache subdirectories; removed when the session stops.
	confinement     confinementPlan
	sessionDir      string
	confinementOnce sync.Once

	state *mapperState
}

// HandleOptions tweaks handle behavior. Used by tests; production code
// uses the defaults via Provider.Spawn.
type HandleOptions struct {
	// RPCTimeout caps how long a single JSON-RPC request waits.
	// Defaults to 30s.
	RPCTimeout time.Duration

	// EventBuffer sets the events channel buffer. Defaults to 256.
	EventBuffer int
}

// confinementPlan is the narrow slice of *confinement.Plan this handle
// needs (release only). It keeps the TAL narrow so unit-test handles
// built without a provider never touch the OS boundary.
type confinementPlan interface {
	Release() error
}

var _ confinementPlan = (*confinement.Plan)(nil)

func newHandle(p *Provider, spec agent.Spec, plan SpawnPlan, opts HandleOptions) *Handle {
	if opts.RPCTimeout == 0 {
		opts.RPCTimeout = DefaultRPCTimeout
	}
	if opts.EventBuffer == 0 {
		opts.EventBuffer = 256
	}
	return &Handle{
		provider:         p,
		bridge:           NewApprovalBridge(spec.PermissionConfig),
		spec:             spec,
		plan:             plan,
		rpcTimeout:       opts.RPCTimeout,
		events:           make(chan agent.Event, opts.EventBuffer),
		notifyCh:         make(chan notification, 256),
		closed:           make(chan struct{}),
		state:            newMapperState(resolveModel(spec)),
		decidedApprovals: make(map[string]struct{}),
	}
}

// setConfinement attaches the plan a confined spawn runs under. The
// rendered profile must stay on disk until the confined child has
// exited, so the release runs when the session stops, not at spawn. A
// nil plan (an unconfined spawn) leaves the interface nil — it is NOT
// assigned, because a typed-nil *Plan in the interface would read
// non-nil and panic on Release. Set before the pump starts.
func (h *Handle) setConfinement(plan *confinement.Plan, sessionDir string) {
	if plan != nil {
		h.confinement = plan
	}
	h.sessionDir = sessionDir
}

// releaseConfinement releases the rendered profile and removes the
// per-session scratch directory. It runs once, when the session stops
// or the pump drains — after the confined child has exited (or at
// least read the profile); a process already running under the profile
// stays confined. Releasing early makes the start fail, never run
// unconfined.
func (h *Handle) releaseConfinement() {
	h.confinementOnce.Do(func() {
		if h.confinement != nil {
			_ = h.confinement.Release()
		}
		if h.sessionDir != "" {
			_ = os.RemoveAll(h.sessionDir)
		}
	})
}

// SessionID returns the MSP session id once session/start resolves.
func (h *Handle) SessionID() string {
	h.idMu.RLock()
	defer h.idMu.RUnlock()
	return h.sessionID
}

// Events returns the read-only event channel. Closes after the
// terminal event.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject steers the running turn via turn/steer (message injection
// into the live session). Fails when no turn is running.
func (h *Handle) Inject(ctx context.Context, text string) error {
	h.turnMu.RLock()
	turnID := h.currentTurnID
	h.turnMu.RUnlock()
	if turnID == "" {
		return fmt.Errorf("muse inject: no turn running: %w", agent.ErrUnsupported)
	}
	steer := joinNonEmpty(h.spec.BaseInstructions, text)
	input := []TurnInputPart{{Type: string(TurnInputText), Text: &steer}}
	params := map[string]any{
		"sessionId":      h.SessionID(),
		"commandId":      newUUIDv7(),
		"expectedTurnId": turnID,
		"input":          turnInputToParams(input),
	}
	if h.client == nil {
		return errors.New("muse inject: no client")
	}
	if _, err := h.client.Request(ctx, "turn/steer", params, h.rpcTimeout); err != nil {
		return fmt.Errorf("muse inject: turn/steer: %w", err)
	}
	return nil
}

// Stop interrupts the active turn, stops the host child, and closes
// the events channel. Idempotent.
func (h *Handle) Stop(ctx context.Context) error {
	h.closeOnce.Do(func() {
		if h.client != nil && h.SessionID() != "" {
			h.turnMu.RLock()
			turnID := h.currentTurnID
			h.turnMu.RUnlock()
			interrupt := map[string]any{
				"sessionId": h.SessionID(),
				"commandId": newUUIDv7(),
			}
			if turnID != "" {
				interrupt["turnId"] = turnID
			}
			_, _ = h.client.Request(ctx, "turn/interrupt", interrupt, h.rpcTimeout)
		}
	})
	_ = h.shutdownChild("stop")
	h.signalClosed()
	h.closeEvents()
	return nil
}

// failNow marks the handle terminal with an ErrorEvent and closes the
// events channel. Used when the serve child crashes. Safe to call
// concurrently with Stop / forward. A client close caused by our own
// shutdownChild is not a crash: it closes the stream without the
// serve_crashed error.
func (h *Handle) failNow(err error) {
	if h.childStopping.Load() {
		h.signalClosed()
		h.closeEvents()
		return
	}
	msg := fmt.Sprintf("muse provider failure: %s", err.Error())
	if excerpt := h.stderrExcerpt(); excerpt != "" {
		msg += " (serve stderr: " + excerpt + ")"
	}
	h.emit(agent.ErrorEvent{Message: msg, Code: "serve_crashed"})
	h.signalClosed()
	h.closeEvents()
}

// confinementPlan returns the session's OS confinement as the narrow
// *confinement.Plan confineMuseArgv/confineMuseEnv take. h.confinement is
// the interface-typed field (so unit-test handles never touch the OS
// boundary); the assertion only succeeds for a real prepared plan.
func (h *Handle) confinementPlan() *confinement.Plan {
	if p, ok := h.confinement.(*confinement.Plan); ok {
		return p
	}
	return nil
}

// spawnChild starts the `muse serve` subprocess and wires the JSON-RPC
// client to its stdio. A non-nil confinement (set by the provider before
// this call) wraps the argv in the session's OS boundary and binds the
// session tmp and caches over their variables; the child then inherits
// exactly three descriptors — the stdin/stdout/stderr pipes created below
// — and never a descriptor open on an out-of-set file (ExtraFiles is
// never set).
func (h *Handle) spawnChild(workspaceRoot string, spec agent.Spec) error {
	if h.provider != nil && h.provider.opts.skipProcess {
		// Test path: caller wired stdin/stdout via overrides.
		h.stdin = h.provider.opts.stdinOverride
		h.stdout = h.provider.opts.stdoutOverride
		h.stderr = h.provider.opts.stderrOverride
		h.client = NewClient(h.stdin, h.stdout)
		h.client.SetOnClose(func(err error) { h.failNow(joinErr("serve stream closed", err)) })
		return nil
	}
	bin := ""
	args := []string{"serve"}
	cwd := ""
	envExtra := map[string]string(nil)
	trust := false
	disableSandbox := false
	if h.provider != nil {
		bin = h.provider.museBin
		args = append([]string(nil), h.provider.opts.Args...)
		cwd = h.provider.opts.Cwd
		envExtra = h.provider.opts.Env
		trust = h.provider.opts.TrustWorkspace
		disableSandbox = h.provider.opts.DisableSandbox
	}
	if bin == "" {
		var err error
		bin, err = resolveMuseBinary("")
		if err != nil {
			return err
		}
	}
	if len(args) == 0 || args[0] != "serve" {
		args = append([]string{"serve"}, args...)
	}
	// Sandbox posture is fixed at host construction: append the
	// session's lane flags.
	for _, flag := range serveArgs(spec, trust, disableSandbox) {
		if flag == "serve" {
			continue
		}
		args = append(args, flag)
	}
	// Wrap the whole serve argv in the session's confinement AFTER the
	// lane flags are appended, so the OS boundary sits outside whatever
	// the host enforces itself (--disable-shell --disable-write,
	// --disable-sandbox stay on the inner argv). A nil confinement (an
	// unconfined session) leaves argv unchanged.
	argv, cerr := confineMuseArgv(h.confinementPlan(), append([]string{bin}, args...))
	if cerr != nil {
		return fmt.Errorf("muse confine command: %w", cerr)
	}
	// nolint:gosec // G204: bin is the resolved-from-PATH host binary.
	cmd := exec.Command(argv[0], argv[1:]...)
	configureProcessGroup(cmd)
	cmd.Dir = cwd
	if cmd.Dir == "" {
		cmd.Dir = workspaceRoot
	}
	cmd.Env = confineMuseEnv(serveEnv(os.Environ(), envExtra, spec.Env), h.confinementPlan())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("muse stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("muse stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("muse stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return fmt.Errorf("muse spawn: %w", err)
	}
	h.cmd = cmd
	h.stdin = stdin
	h.stdout = stdout
	h.stderr = stderr
	h.stderrBuf = newBoundedBuffer(stderrRetentionBytes)
	h.exited = make(chan struct{})
	go func() {
		defer func() { _ = stderr.Close() }()
		_, _ = io.Copy(h.stderrBuf, stderr)
	}()
	h.client = NewClient(stdin, stdout)
	h.client.SetOnClose(func(err error) { h.failNow(joinErr("serve stream closed", err)) })
	go h.watchExit()
	if spec.OnProcessSpawned != nil && cmd.Process != nil {
		spec.OnProcessSpawned(cmd.Process.Pid)
	}
	return nil
}

// handshake runs initialize then the initialized notification, and
// validates the schema version + granted capabilities. Anything sent
// before the handshake completes is dropped by the host, so session
// start waits for this.
func (h *Handle) handshake(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, h.rpcTimeout)
	defer cancel()
	raw, err := h.client.Request(hctx, "initialize", handshakeParams(), h.rpcTimeout)
	if err != nil {
		return fmt.Errorf("muse initialize handshake: %w", err)
	}
	if _, err := checkHandshakeResultForPlan(raw, len(h.plan.MCPServers) > 0); err != nil {
		return err
	}
	if err := h.client.Notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("muse initialized notification: %w", err)
	}
	return nil
}

// startSession opens a fresh session and subscribes to its events.
func (h *Handle) startSession(ctx context.Context) error {
	sessionID := newUUIDv7()
	commandID := newUUIDv7()
	mcpServers := h.plan.MCPServers
	params := sessionStartParams(h.spec, sessionID, commandID, h.plan.WorkspaceRoot, mcpServers)
	raw, err := h.client.Request(ctx, "session/start", params, h.rpcTimeout)
	if err != nil {
		return fmt.Errorf("session/start: %w", err)
	}
	var res SessionStartResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("session/start: decode response: %w", err)
	}
	if res.Session.SessionID == "" {
		return errors.New("session/start: empty session id in response")
	}
	h.setSessionID(res.Session.SessionID)
	h.client.Subscribe(res.Session.SessionID, func(n notification) { h.deliver(n) })
	if h.spec.SessionName != "" {
		_, _ = h.client.Request(ctx, "session/rename", map[string]any{
			"sessionId": res.Session.SessionID,
			"commandId": newUUIDv7(),
			"name":      h.spec.SessionName,
		}, h.rpcTimeout)
	}
	return nil
}

// resumeSession re-attaches to an existing session and subscribes to
// its events. excludeItems keeps the resume light.
func (h *Handle) resumeSession(ctx context.Context, sessionID string) error {
	h.setSessionID(sessionID)
	h.client.Subscribe(sessionID, func(n notification) { h.deliver(n) })
	params := sessionResumeParams(sessionID, newUUIDv7(), h.plan.MCPServers)
	raw, err := h.client.Request(ctx, "session/resume", params, h.rpcTimeout)
	if err != nil {
		h.client.Unsubscribe(sessionID)
		return fmt.Errorf("session/resume: %w", err)
	}
	var res SessionResumeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		h.client.Unsubscribe(sessionID)
		return fmt.Errorf("session/resume: decode response: %w", err)
	}
	if res.Session.SessionID != "" && res.Session.SessionID != sessionID {
		h.client.Unsubscribe(sessionID)
		return fmt.Errorf("session/resume: host returned a different session id %q", res.Session.SessionID)
	}
	return nil
}

// startTurn starts one turn on the session and records the foreground
// turn id from the ack (the returned turnId equals the commandId).
func (h *Handle) startTurn(ctx context.Context, input []TurnInputPart) error {
	sessionID := h.SessionID()
	commandID := newUUIDv7()
	roots := h.workspaceRoots
	if len(roots) == 0 && h.plan.WorkspaceRoot != "" {
		roots = []string{h.plan.WorkspaceRoot}
	}
	params := turnStartParams(sessionID, commandID, h.spec, input, roots)
	raw, err := h.client.Request(ctx, "turn/start", params, h.rpcTimeout)
	if err != nil {
		return fmt.Errorf("turn/start: %w", err)
	}
	var res TurnStartResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("turn/start: decode response: %w", err)
	}
	if res.TurnID == "" {
		return errors.New("turn/start: empty turn id in response")
	}
	h.setCurrentTurnID(res.TurnID)
	return nil
}

// deliver routes one inbound notification to the forwarder. It blocks
// (applying backpressure to the read loop) rather than dropping: a
// dropped turn/completed or server-request would stall the session
// forever. It only gives up once the handle is closed.
func (h *Handle) deliver(n notification) {
	select {
	case h.notifyCh <- n:
	case <-h.closed:
	}
}

// forward is the per-Handle goroutine that consumes inbound
// notifications, translates them to agent.Events, and publishes.
func (h *Handle) forward(ctx context.Context) {
	defer func() {
		if sid := h.SessionID(); sid != "" && h.client != nil {
			h.client.Unsubscribe(sid)
		}
		h.signalClosed()
		h.closeEvents()
	}()

	for {
		select {
		case <-ctx.Done():
			// Emit before Stop: Stop closes the events channel.
			h.emit(agent.ErrorEvent{Message: "session cancelled: " + ctx.Err().Error(), Code: "context_cancelled"})
			ctxStop, cancel := context.WithTimeout(context.Background(), h.rpcTimeout)
			_ = h.Stop(ctxStop)
			cancel()
			return
		case <-h.closed:
			return
		case n := <-h.notifyCh:
			done := h.handleNotification(ctx, n)
			if done {
				return
			}
		}
	}
}

// handleNotification dispatches one inbound notification. Returns true
// when the session has reached its terminal event.
func (h *Handle) handleNotification(ctx context.Context, n notification) bool {
	// Server-requests (approval/request, userInput/request) arrive on
	// the same channel; answer the presentation receipt immediately.
	if len(n.ServerRequestID) > 0 {
		h.handleServerRequest(n)
		return false
	}

	// Route foreign-session traffic to the global log, not this
	// handle's stream.
	if sid := extractSessionID(n.Params); sid != "" && sid != h.SessionID() {
		return false
	}

	var rawObj map[string]any
	if len(n.Params) > 0 {
		_ = json.Unmarshal(n.Params, &rawObj)
	}

	// The host presents an approval twice: once as an
	// approval/requested notification (view fold) and once as an
	// approval/request server-request (presentation). The decision
	// travels as approval/decide exactly once, driven here from the
	// notification; the later server-request only gets its `{}`
	// receipt.
	if n.Method == "approval/requested" {
		h.decideFromNotification(n.Params, rawObj)
	}

	events := mapNotification(n.Method, n.Params, h.state, rawObj)
	for _, ev := range events {
		switch e := ev.(type) {
		case agent.ResultEvent:
			h.clearCurrentTurnID()
			h.emit(e)
			return true
		case agent.ErrorEvent:
			if e.Code == "view_gap" {
				// Fatal for the stream, recoverable for the
				// session: re-anchor via session/resume, then
				// keep forwarding.
				h.emit(e)
				h.recoverFromGap(ctx)
				return false
			}
			h.emit(e)
			return true
		}
		h.emit(ev)
	}
	// A turn that completed without a terminal ResultEvent (unknown
	// terminal vocabulary) still ends foreground tracking.
	if n.Method == "turn/completed" {
		h.clearCurrentTurnID()
	}
	return false
}

// recoverFromGap re-anchors the session after a view/gap: session/resume
// with the last known cursor replays the dropped range, then live
// delivery continues. The gap flag clears so item events flow again.
func (h *Handle) recoverFromGap(ctx context.Context) {
	if h.state.gap == nil {
		return
	}
	after := h.state.gap.After
	h.state.gap = nil
	// Best-effort: resume from the last delivered cursor. A resume
	// failure is terminal — the stream cannot be trusted.
	rctx, cancel := context.WithTimeout(ctx, h.rpcTimeout)
	defer cancel()
	params := map[string]any{
		"sessionId":    h.SessionID(),
		"commandId":    newUUIDv7(),
		"excludeItems": true,
		"cursor":       after,
	}
	if _, err := h.client.Request(rctx, "session/resume", params, h.rpcTimeout); err != nil {
		h.emit(agent.ErrorEvent{
			Message: fmt.Sprintf("muse gap recovery failed: %s", err.Error()),
			Code:    "view_gap_recovery_failed",
		})
		h.signalClosed()
		h.closeEvents()
		return
	}
	h.emit(agent.SystemEvent{Subtype: "view_gap_recovered", Message: "Session re-anchored after view gap", Raw: nil})
}

// handleServerRequest answers the host's must-answer requests with the
// `{}` presentation receipt, and drives the approval decision for
// approval/request via a separate approval/decide command.
func (h *Handle) handleServerRequest(n notification) {
	// The presentation receipt always goes first: the decision/answer
	// travels as a separate command.
	_ = h.client.RespondToServerRequest(n.ServerRequestID, map[string]any{})

	var rawObj map[string]any
	_ = json.Unmarshal(n.Params, &rawObj)

	switch n.Method {
	case "approval/request":
		// Receipt already sent above; the decision was driven from
		// the approval/requested notification (exactly-once per
		// approvalId via decidedApprovals). Nothing more to do.
	case "userInput/request":
		var req UserInputRequestParams
		_ = json.Unmarshal(n.Params, &req)
		h.emit(agent.SystemEvent{
			Subtype: "user_input_requested",
			Message: fmt.Sprintf("Host requested user input for %s; autonomous mode has no user to prompt", req.ToolName),
			Raw:     rawObj,
		})
	default:
		h.emit(agent.SystemEvent{
			Subtype: "unhandled_server_request",
			Message: "Declined unhandled muse server request: " + n.Method,
			Raw:     rawObj,
		})
	}
}

// decideFromNotification drives the approval decision from an
// approval/requested notification (the view fold, which carries the
// full subject + choices). The matching approval/request
// server-request only needs its `{}` receipt afterward.
func (h *Handle) decideFromNotification(params json.RawMessage, rawObj map[string]any) {
	var req ApprovalRequestParams
	_ = json.Unmarshal(params, &req)
	h.decideApproval(req, rawObj)
}

// decideApproval evaluates one approval request via the bridge and
// answers with approval/decide. A decline maps to the denial/abort
// choice. Unknown subjects (no matching choice) are declined when a
// denial choice exists, else left for the host timeout. Exactly-once
// per approvalId: the host may present the same approval as both an
// approval/requested notification and an approval/request
// server-request.
func (h *Handle) decideApproval(req ApprovalRequestParams, rawObj map[string]any) {
	if req.ApprovalID != "" {
		if _, done := h.decidedApprovals[req.ApprovalID]; done {
			return
		}
		h.decidedApprovals[req.ApprovalID] = struct{}{}
	}
	parsed := parseApprovalRequest(req, h.spec.Cwd)
	decision := h.bridge.Evaluate(parsed)
	allow := decision.Action == ActionAllow

	toolUseID := "approval-" + req.ApprovalID
	h.emit(agent.ToolUseEvent{
		ToolName:  approvalToolName(parsed),
		ToolUseID: toolUseID,
		Input:     approvalInput(parsed),
		Raw:       rawObj,
	})

	choice, ok := approvalChoiceFor(req, allow)
	if !ok {
		h.emit(agent.ToolResultEvent{
			ToolName:  approvalToolName(parsed),
			ToolUseID: toolUseID,
			Content:   "no matching approval choice; leaving for host timeout",
			IsError:   true,
			Raw:       rawObj,
		})
		return
	}
	params := map[string]any{
		"sessionId":     req.SessionID,
		"commandId":     newUUIDv7(),
		"approvalId":    req.ApprovalID,
		"requirementId": map[string]any{"approvalId": req.CurrentRequirementID.ApprovalID, "sourceIndex": req.CurrentRequirementID.SourceIndex},
		"choiceId":      choice.ChoiceID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.rpcTimeout)
	defer cancel()
	decideErr := error(nil)
	if _, err := h.client.Request(ctx, "approval/decide", params, h.rpcTimeout); err != nil {
		decideErr = err
	}

	content := string(decision.Action)
	if decision.Reason != "" {
		content += ": " + decision.Reason
	}
	if decideErr != nil {
		content += fmt.Sprintf(" (approval/decide failed: %s)", decideErr.Error())
	}
	h.emit(agent.ToolResultEvent{
		ToolName:  approvalToolName(parsed),
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   !allow || decideErr != nil,
		Raw:       rawObj,
	})
	if !allow {
		h.emit(agent.SystemEvent{
			Subtype: "approval_denied",
			Message: "Blocked: " + decision.Reason,
			Raw:     rawObj,
		})
	}
}

// watchExit observes the serve child exit and stops the client so the
// handle fails via onClose.
func (h *Handle) watchExit() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	err := h.cmd.Wait()
	close(h.exited)
	cause := err
	if cause == nil {
		cause = errors.New("muse serve exited")
	}
	slog.Log(context.Background(), slog.LevelWarn, "muse: serve process exited", "error", err)
	if h.client != nil {
		h.client.Stop(cause)
	}
}

// shutdownChild stops the serve child's whole process group (SIGTERM,
// then SIGKILL after the grace period) and closes its pipes. Idempotent
// and re-entrant: only the first caller does the work.
func (h *Handle) shutdownChild(reason string) error {
	if !h.childStopping.CompareAndSwap(false, true) {
		return nil
	}
	if h.client != nil {
		h.client.Stop(errors.New("muse handle shutdown: " + reason))
	}
	if h.cmd != nil && h.cmd.Process != nil && h.exited != nil {
		signalProcessGroup(h.cmd, sigTERM())
		select {
		case <-h.exited:
		case <-time.After(stopGracePeriod):
		}
		// Sweep the group even when the leader exited on SIGTERM:
		// helpers it forked (MCP servers, tool shells) share the
		// group and may outlive it.
		signalProcessGroup(h.cmd, sigKILL())
	}
	for _, pipe := range []io.Closer{h.stdin, h.stdout, h.stderr} {
		if pipe != nil {
			_ = pipe.Close()
		}
	}
	return nil
}

func (h *Handle) stderrExcerpt() string {
	if h.stderrBuf == nil {
		return ""
	}
	return h.stderrBuf.excerpt()
}

func (h *Handle) setSessionID(id string) {
	h.idMu.Lock()
	h.sessionID = id
	h.idMu.Unlock()
}

func (h *Handle) setCurrentTurnID(id string) {
	h.turnMu.Lock()
	h.currentTurnID = id
	h.turnMu.Unlock()
}

func (h *Handle) clearCurrentTurnID() {
	h.turnMu.Lock()
	h.currentTurnID = ""
	h.turnMu.Unlock()
}

// signalClosed closes h.closed exactly once.
func (h *Handle) signalClosed() {
	h.closedOnce.Do(func() { close(h.closed) })
}

// closeEvents closes h.events exactly once. The flag is set FIRST
// under the write lock so concurrent emit() callers cannot race past
// the check and send to the closed channel.
func (h *Handle) closeEvents() {
	h.eventsMu.Lock()
	if h.eventsClosed.Load() {
		h.eventsMu.Unlock()
		return
	}
	h.eventsClosed.Store(true)
	close(h.events)
	h.eventsMu.Unlock()

	if h.provider != nil {
		h.provider.unregisterHandle(h)
	}
	_ = h.shutdownChild("events closed")
	h.releaseConfinement()
}

// emit publishes an event on the events channel without blocking.
// Best-effort streaming output never takes a reserved slot: once the
// buffer holds cap-reserved events it is dropped early so the
// authoritative assistant completion and terminal result still fit.
// When a non-best-effort event arrives at a full buffer, emit evicts
// queued best-effort output to make room; a fixed reservation alone only
// moves the failure threshold once ordinary traffic (tool calls) fills
// the reserved tail. When no best-effort output remains, a high-priority
// terminal-adjacent event (assistant completion, usage, result/error)
// evicts the oldest queued event rather than dropping the verdict.
func (h *Handle) emit(ev agent.Event) {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	if h.eventsClosed.Load() {
		return
	}
	if isBestEffortStreamEvent(ev) {
		if n := cap(h.events); n > streamProgressReservedSlots && len(h.events) >= n-streamProgressReservedSlots {
			return
		}
		select {
		case h.events <- ev:
		case <-h.closed:
		default:
		}
		return
	}
	select {
	case h.events <- ev:
		return
	case <-h.closed:
		return
	default:
	}
	if h.evictForTerminal(ev) {
		select {
		case h.events <- ev:
		case <-h.closed:
		default:
		}
	}
}

// evictForTerminal makes room for ev on a full events channel by
// dropping queued best-effort output first. It must be called with
// eventsMu held and only when the non-blocking send above failed.
// Reports whether room was made. Drained normal events are re-queued in
// order; only best-effort output — or, for high-priority terminal-
// adjacent events with no best-effort left, the single oldest event —
// is lost.
func (h *Handle) evictForTerminal(ev agent.Event) bool {
	n := len(h.events)
	if n == 0 {
		return true
	}
	kept := make([]agent.Event, 0, n)
	evicted := 0
	for {
		select {
		case e := <-h.events:
			if isBestEffortStreamEvent(e) {
				evicted++
				continue
			}
			kept = append(kept, e)
		default:
			goto drained
		}
	}
drained:
	if evicted == 0 {
		if !isTerminalAdjacentEvent(ev) {
			for _, e := range kept {
				select {
				case h.events <- e:
				case <-h.closed:
					return false
				default:
					return false
				}
			}
			return false
		}
		if len(kept) == 0 {
			return true
		}
		kept = kept[1:]
	}
	for _, e := range kept {
		select {
		case h.events <- e:
		case <-h.closed:
			return false
		default:
			return false
		}
	}
	return true
}

// isTerminalAdjacentEvent reports whether ev is terminal-adjacent and
// must not be dropped in favor of older queued output: the final
// assistant message the runner verdict-scans, the aggregate per-turn
// usage, and the terminal result/error itself.
func isTerminalAdjacentEvent(ev agent.Event) bool {
	switch ev.(type) {
	case agent.AssistantTextEvent,
		agent.LlmCallEvent,
		agent.UsageEvent,
		agent.ContextUsageEvent,
		agent.ResultEvent,
		agent.ErrorEvent:
		return true
	default:
		return false
	}
}

// isBestEffortStreamEvent reports whether ev is best-effort streaming
// output that may be dropped first when the buffer fills: content-free
// stream-progress liveness, and reasoning fragments. Reasoning is never
// verdict-scanned and replays only as intermediate thought, while the
// authoritative assistant completion and terminal result must still fit;
// under a slow consumer a reasoning burst would otherwise fill the channel
// and silently drop the verdict.
func isBestEffortStreamEvent(ev agent.Event) bool {
	sys, ok := ev.(agent.SystemEvent)
	if !ok {
		return false
	}
	return sys.Subtype == agent.SystemSubtypeStreamProgress || sys.Subtype == "reasoning"
}

func approvalToolName(req ApprovalRequest) string {
	switch req.Kind {
	case ApprovalKindCommand:
		return "shell"
	case ApprovalKindFileAccess:
		return "file_change"
	case ApprovalKindNetwork:
		return "network"
	case ApprovalKindTool:
		if req.Tool != "" {
			return req.Tool
		}
		return "tool"
	default:
		return "approval_unknown"
	}
}

func approvalInput(req ApprovalRequest) map[string]any {
	switch req.Kind {
	case ApprovalKindCommand:
		return map[string]any{"command": req.Command}
	case ApprovalKindFileAccess:
		return map[string]any{"path": req.Path}
	default:
		out := map[string]any{}
		if req.Tool != "" {
			out["tool"] = req.Tool
		}
		if req.Target != "" {
			out["target"] = req.Target
		}
		return out
	}
}

func joinErr(prefix string, err error) error {
	if err == nil {
		return errors.New(prefix)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

func turnInputToParams(input []TurnInputPart) []map[string]any {
	out := make([]map[string]any, 0, len(input))
	for _, p := range input {
		part := map[string]any{"type": p.Type}
		if p.Text != nil {
			part["text"] = *p.Text
		}
		out = append(out, part)
	}
	return out
}

// joinNonEmpty joins the instruction prefix with the steer text,
// skipping empties.
func joinNonEmpty(prefix, text string) string {
	if prefix == "" {
		return text
	}
	if text == "" {
		return prefix
	}
	return prefix + "\n\n" + text
}

// boundedBuffer is the drop-oldest stderr tail, borrowed from
// clijsonl's shape (each harness keeps its own copy per repo
// convention).
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit, buf: make([]byte, 0, limit)}
}

// Write implements io.Writer. Always returns len(p), nil.
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

func (b *boundedBuffer) excerpt() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.buf...))
}
