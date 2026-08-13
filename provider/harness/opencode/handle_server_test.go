package opencode

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// fakeClient is a programmable serverClient for Lane-B handle tests. Frames
// pushed onto evCh flow through the handle's forwarder; pending permissions are
// served until replied.
type fakeClient struct {
	mu        sync.Mutex
	evCh      chan serverEvent
	pending   []permissionRequest
	replies   map[string]permissionResponse
	prompts   []promptReq
	aborted   bool
	sessionID string
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		evCh:      make(chan serverEvent, 32),
		replies:   map[string]permissionResponse{},
		sessionID: "ses_it",
	}
}

func (f *fakeClient) Health(context.Context) error { return nil }
func (f *fakeClient) CreateSession(context.Context, createSessionReq) (string, error) {
	return f.sessionID, nil
}

func (f *fakeClient) Prompt(_ context.Context, _ string, req promptReq) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, req)
	return nil
}

func (f *fakeClient) Abort(context.Context, string) error {
	f.mu.Lock()
	f.aborted = true
	f.mu.Unlock()
	return nil
}

func (f *fakeClient) Events(context.Context) (<-chan serverEvent, func() error, error) {
	return f.evCh, func() error { return nil }, nil
}

func (f *fakeClient) PendingPermissions(_ context.Context, _ string) ([]permissionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []permissionRequest
	for _, p := range f.pending {
		if _, done := f.replies[p.ID]; !done {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeClient) RespondPermission(_ context.Context, _, permissionID string, resp permissionResponse) error {
	f.mu.Lock()
	f.replies[permissionID] = resp
	f.mu.Unlock()
	return nil
}

func (f *fakeClient) Messages(context.Context, string, string) ([]serverMessage, error) {
	return nil, nil
}

func (f *fakeClient) push(ev serverEvent) { f.evCh <- ev }

func (f *fakeClient) replyOf(id string) (permissionResponse, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.replies[id]
	return r, ok
}

// drainHandle collects events until the first terminal event (ResultEvent or
// ErrorEvent) or the channel closes, whichever comes first (bounded by ctx).
// It mirrors the production consumer, runner.consumeEvents, which returns as
// soon as it observes a ResultEvent rather than waiting for the channel to
// also close — that distinction matters here because an ORDINARY
// completed/failed turn no longer closes the events channel on its own
// (handle_server.go's forward/finishWithCleanup keep the pump, owned child,
// and owned config alive so a later Handle.Inject still has somewhere to
// land); only a fatal terminal or an explicit Stop does. A
// fatal-terminal session still closes the channel right after its
// ErrorEvent, so this helper returns the same way it always did for those
// cases.
func drainHandle(ctx context.Context, h agent.Handle) []agent.Event {
	var got []agent.Event
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
			switch ev.(type) {
			case agent.ResultEvent, agent.ErrorEvent:
				return got
			}
		case <-ctx.Done():
			return got
		}
	}
}

func TestServerHandle_HappyPath_Conforms(t *testing.T) {
	t.Parallel()
	fc := newFakeClient()
	h := newServerHandle(nil, fc, fc.sessionID, agent.Spec{}, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	fc.push(evt("e1", evSessionCreated, map[string]any{"sessionID": fc.sessionID}))
	fc.push(evt("e2", evTextEnded, map[string]any{"sessionID": fc.sessionID, "text": "done."}))
	fc.push(evt("e3", evStepEnded, map[string]any{"sessionID": fc.sessionID, "finish": "stop"}))

	events := drainHandle(ctx, h)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Errorf("terminal contract: %v\nevents=%s", err, kindsOf(events))
	}
	var init, text, result int
	for _, ev := range events {
		switch ev.(type) {
		case agent.InitEvent:
			init++
		case agent.AssistantTextEvent:
			text++
		case agent.ResultEvent:
			result++
		}
	}
	if init != 1 || text < 1 || result != 1 {
		t.Errorf("counts init=%d text=%d result=%d; want 1,>=1,1", init, text, result)
	}
	if h.SessionID() != fc.sessionID {
		t.Errorf("SessionID = %q, want %q", h.SessionID(), fc.sessionID)
	}
}

func TestServerHandle_PermissionRoundTrip(t *testing.T) {
	t.Parallel()
	fc := newFakeClient()
	fc.pending = []permissionRequest{
		{ID: "p-deny", SessionID: "ses_it", Action: "bash", Resources: []string{"rm -rf /"}},
		{ID: "p-allow", SessionID: "ses_it", Action: "bash", Resources: []string{"ls"}},
	}
	h := newServerHandle(nil, fc, fc.sessionID, agent.Spec{}, slog.Default())
	h.permInterval = 15 * time.Millisecond // per-instance; no shared-var race
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for the pump to adjudicate both requests.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, denyDone := fc.replyOf("p-deny")
		_, allowDone := fc.replyOf("p-allow")
		if denyDone && allowDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("permissions not adjudicated within deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r, _ := fc.replyOf("p-deny"); r.Reply != replyReject {
		t.Errorf("p-deny reply = %q, want reject (safety deny)", r.Reply)
	}
	if r, _ := fc.replyOf("p-allow"); r.Reply != replyOnce {
		t.Errorf("p-allow reply = %q, want once", r.Reply)
	}

	// End the session and confirm the pump surfaced observability SystemEvents.
	fc.push(evt("t", evStepEnded, map[string]any{"sessionID": fc.sessionID, "finish": "stop"}))
	events := drainHandle(ctx, h)
	var decisions int
	for _, ev := range events {
		if se, ok := ev.(agent.SystemEvent); ok && se.Subtype == "permission_decision" {
			decisions++
		}
	}
	if decisions < 2 {
		t.Errorf("permission_decision SystemEvents = %d, want >=2", decisions)
	}
}

func TestServerHandle_Inject(t *testing.T) {
	t.Parallel()
	fc := newFakeClient()
	h := newServerHandle(nil, fc, fc.sessionID, agent.Spec{}, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	if err := h.Inject(ctx, "follow up"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.prompts) != 1 || fc.prompts[0].Prompt.Text != "follow up" {
		t.Errorf("prompts = %+v, want one 'follow up'", fc.prompts)
	}
}

// --- Post-terminal Inject: an ordinary completed turn keeps the
// pump alive so a later Inject lands; a fatal terminal (the SSE stream
// itself ending) still fails closed. Mirrors the analogous pi harness fix's
// post-terminal Inject pair for this harness's own terminal contract — Lane
// B has no in-flight/settled routing to prove (every Inject is a "steer"
// prompt regardless of state), so the pair here proves the thing that was
// actually broken: whether the call lands at all.

// TestServerHandle_Inject_AfterOrdinaryTerminal_Lands proves that injecting
// after an ordinary completed turn (evStepEnded/finish=stop) still succeeds
// and posts to the still-live session, rather than failing closed with
// "session end" the instant the turn's own ResultEvent was mapped.
func TestServerHandle_Inject_AfterOrdinaryTerminal_Lands(t *testing.T) {
	t.Parallel()
	fc := newFakeClient()
	h := newServerHandle(nil, fc, fc.sessionID, agent.Spec{}, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	fc.push(evt("t", evStepEnded, map[string]any{"sessionID": fc.sessionID, "finish": "stop"}))
	first := drainHandle(ctx, h)
	if err := conformance.CheckTerminalContract(first); err != nil {
		t.Fatalf("terminal contract: %v", err)
	}
	if _, ok := first[len(first)-1].(agent.ResultEvent); !ok {
		t.Fatalf("expected the turn to end in a ResultEvent, got %s", kindsOf(first))
	}

	if err := h.Inject(ctx, "keep going"); err != nil {
		t.Fatalf("Inject after an ordinary completed turn = %v, want nil (the session must stay open for a post-settle inject)", err)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.prompts) != 1 || fc.prompts[0].Prompt.Text != "keep going" {
		t.Errorf("prompts = %+v, want one 'keep going'", fc.prompts)
	}
}

// TestServerHandle_Inject_AfterFatalTerminal_FailsClosed is the negative half
// of the pair: unlike an ordinary completed turn, a FATAL terminal — the SSE
// stream itself ending, whether because the owned child crashed or because
// an attached/still-alive server's feed just dropped — must still refuse
// Inject. There is no live session left to post a follow-up prompt to.
func TestServerHandle_Inject_AfterFatalTerminal_FailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		child *serveChild
	}{
		{
			name:  "server crashed (owned child exited)",
			child: exitedServeChild(),
		},
		{
			name:  "dropped feed (no owned child / attach mode)",
			child: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fc := newFakeClient()
			h := newServerHandle(tc.child, fc, fc.sessionID, agent.Spec{}, slog.Default())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.start(ctx); err != nil {
				t.Fatalf("start: %v", err)
			}
			close(fc.evCh) // fatal: the SSE stream itself ends.

			events := drainHandle(ctx, h)
			if err := conformance.CheckTerminalContract(events); err != nil {
				t.Fatalf("terminal contract: %v", err)
			}
			if _, ok := events[len(events)-1].(agent.ErrorEvent); !ok {
				t.Fatalf("expected a fatal ErrorEvent, got %s", kindsOf(events))
			}
			if err := h.Inject(ctx, "should not land"); err == nil {
				t.Error("Inject after a fatal terminal returned nil error; want a closed-session error")
			}
		})
	}
}

// exitedServeChild builds a serveChild whose exited() already reports true,
// without spawning a real process — done is the only field exited() reads.
func exitedServeChild() *serveChild {
	c := &serveChild{done: make(chan struct{})}
	close(c.done)
	return c
}

func TestServerHandle_StopIdempotentAndAborts(t *testing.T) {
	t.Parallel()
	fc := newFakeClient()
	h := newServerHandle(nil, fc, fc.sessionID, agent.Spec{}, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Second Stop is a no-op.
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop #2: %v", err)
	}
	fc.mu.Lock()
	aborted := fc.aborted
	fc.mu.Unlock()
	if !aborted {
		t.Error("Stop did not issue Abort")
	}
	// Events channel must be closed after Stop; drain any buffered events.
	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return // closed → pass
			}
		case <-timeout:
			t.Fatal("events channel not closed after Stop")
		}
	}
}

func newOwnedConfigServerHandle(t *testing.T, spec agent.Spec) (*Provider, *openCodeConfigBoundary, *serverHandle, *fakeClient) {
	t.Helper()
	boundary, err := newOpenCodeConfigBoundary(t.TempDir(), spec)
	if err != nil {
		t.Fatalf("new config boundary: %v", err)
	}
	resource := &openCodeServerResource{config: boundary}
	p := &Provider{}
	if err := p.registerResource(resource); err != nil {
		t.Fatalf("register resource: %v", err)
	}
	fc := newFakeClient()
	h := newServerHandle(nil, fc, fc.sessionID, spec, slog.Default())
	h.releaseOwned = func() error { return p.releaseResource(resource) }
	return p, boundary, h, fc
}

func TestServerHandle_StopAndConcurrentShutdownRemoveOwnedConfig(t *testing.T) {
	p, boundary, h, _ := newOwnedConfigServerHandle(t, agent.Spec{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- h.Stop(context.Background())
	}()
	go func() {
		defer wg.Done()
		errs <- p.Shutdown(context.Background())
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent teardown: %v", err)
		}
	}
	if _, err := os.Stat(boundary.home); !os.IsNotExist(err) {
		t.Fatalf("owned config survived Stop/Shutdown: %v", err)
	}
}

func TestProvider_ShutdownRemovesOrphanedOwnedConfig(t *testing.T) {
	boundary, err := newOpenCodeConfigBoundary(t.TempDir(), agent.Spec{})
	if err != nil {
		t.Fatalf("new config boundary: %v", err)
	}
	resource := &openCodeServerResource{config: boundary}
	p := &Provider{}
	if err := p.registerResource(resource); err != nil {
		t.Fatalf("register resource: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := os.Stat(boundary.home); !os.IsNotExist(err) {
		t.Fatalf("owned config survived Shutdown: %v", err)
	}
	if _, err := p.Spawn(t.Context(), agent.Spec{}); !errors.Is(err, errOpenCodeShutdown) {
		t.Fatalf("Spawn after Shutdown error = %v, want provider shutdown denial", err)
	}
}

// TestServerHandle_OrdinaryTerminalDefersConfigCleanupUntilStop is
// regression coverage for a structural sibling of a bug already fixed in
// this repo's pi harness: Lane B used to tear down its owned child and
// config the instant a turn's terminal event was mapped, in the same
// goroutine that produced it — so a caller that tried to Inject a follow-up
// immediately afterward found the session already gone, even though opencode
// itself was still alive and able to continue. An ordinary completed turn
// (evStepEnded/finish=stop) must leave the owned child/config alone; cleanup
// now happens lazily, in Stop, once a caller actually decides the session is
// over. (Formerly TestServerHandle_TerminalRemovesOwnedConfigBeforeResult,
// which asserted the opposite — the bug this fixes.)
func TestServerHandle_OrdinaryTerminalDefersConfigCleanupUntilStop(t *testing.T) {
	p, boundary, h, fc := newOwnedConfigServerHandle(t, agent.Spec{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	fc.push(evt("terminal", evStepEnded, map[string]any{"sessionID": fc.sessionID, "finish": "stop"}))
	events := drainHandle(ctx, h)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Fatalf("terminal contract: %v", err)
	}
	if _, err := os.Stat(boundary.home); err != nil {
		t.Fatalf("owned config removed before Stop — a post-settle Inject would have had nothing to land on: %v", err)
	}

	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(boundary.home); !os.IsNotExist(err) {
		t.Fatalf("owned config survived Stop after an ordinary terminal: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after terminal+Stop: %v", err)
	}
}

func TestServerHandle_StreamCrashRemovesOwnedConfig(t *testing.T) {
	_, boundary, h, fc := newOwnedConfigServerHandle(t, agent.Spec{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	close(fc.evCh)
	events := drainHandle(ctx, h)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Fatalf("crash terminal contract: %v", err)
	}
	if _, err := os.Stat(boundary.home); !os.IsNotExist(err) {
		t.Fatalf("owned config survived stream crash: %v", err)
	}
}

// TestServerHandle_TerminalCleanupFailureIsObservableAndSecretSafe adapts to
// the new deferred-cleanup contract: an ordinary terminal's own
// ResultEvent is no longer gated on cleanup succeeding (cleanup does not even
// run until Stop — see TestServerHandle_OrdinaryTerminalDefersConfigCleanupUntilStop),
// so a doomed config no longer suppresses it. The secret-safety net — never
// let a caller believe secrets were scrubbed when they were not — now lives
// at Stop time instead: Stop's return error still carries the bounded
// cleanup sentinel, and the config_cleanup_failed event it emits is still
// observable and still leaks neither the secret nor the config path.
func TestServerHandle_TerminalCleanupFailureIsObservableAndSecretSafe(t *testing.T) {
	const secretSentinel = "opencode-terminal-cleanup-secret-must-not-surface"
	spec := agent.Spec{MCPServers: []agent.MCPServerConfig{{
		Name: "platform", Type: "http", URL: "https://example.invalid/mcp",
		Headers: map[string]string{"Authorization": "Bearer " + secretSentinel},
	}}}
	p, boundary, h, fc := newOwnedConfigServerHandle(t, spec)
	otherInfo, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatalf("stat replacement parent identity: %v", err)
	}
	boundary.parentInfo = otherInfo
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	fc.push(evt("terminal", evStepEnded, map[string]any{"sessionID": fc.sessionID, "finish": "stop"}))

	// The ordinary terminal's own ResultEvent is unaffected by the
	// not-yet-attempted, doomed cleanup.
	events := drainHandle(ctx, h)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Fatalf("terminal contract: %v; events=%s", err, kindsOf(events))
	}
	if len(events) == 0 {
		t.Fatal("ordinary terminal produced no events")
	}
	if _, ok := events[len(events)-1].(agent.ResultEvent); !ok {
		t.Fatalf("ordinary terminal should end in its own ResultEvent, not a cleanup verdict it never attempted: %s", kindsOf(events))
	}

	// Stop is where the deferred cleanup actually runs (and fails).
	if err := h.Stop(context.Background()); !errors.Is(err, errOpenCodeConfigCleanup) {
		t.Fatalf("Stop error = %v, want bounded cleanup sentinel", err)
	}

	// By the time Stop returns, doStop has already emitted the
	// config_cleanup_failed event and closed the channel — drain whatever is
	// buffered and confirm it is observable and secret-safe.
	var cleanup *agent.ErrorEvent
	for ev := range h.Events() {
		if ee, ok := ev.(agent.ErrorEvent); ok && ee.Code == "config_cleanup_failed" {
			e := ee
			cleanup = &e
		}
	}
	if cleanup == nil {
		t.Fatal("Stop's cleanup failure produced no observable config_cleanup_failed event")
	}
	if strings.Contains(cleanup.Message, secretSentinel) || strings.Contains(cleanup.Message, boundary.configPath) {
		t.Fatalf("cleanup event leaked secret or path: %q", cleanup.Message)
	}
	if err := p.Shutdown(context.Background()); !errors.Is(err, errOpenCodeConfigCleanup) {
		t.Fatalf("Shutdown error = %v, want persistent cleanup sentinel", err)
	}
}

// TestProvider_SpawnServer_AttachWiring drives the full Provider.Spawn Lane-B
// path (attach mode + injected client factory) end to end without a real
// binary. Attach mode must not write or claim activation of a local project
// config: the external server owns its own configuration.
func TestProvider_SpawnServer_AttachWiring(t *testing.T) {
	t.Parallel()
	fc := newFakeClient()
	cwd := t.TempDir()
	p := &Provider{
		endpoint:      "http://attached.invalid",
		clientFactory: func(_, _ string) serverClient { return fc },
	}
	spec := agent.Spec{
		Prompt: "do it",
		Cwd:    cwd,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn(server): %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	// The initial prompt was admitted.
	fc.mu.Lock()
	nPrompts := len(fc.prompts)
	fc.mu.Unlock()
	if nPrompts != 1 {
		t.Errorf("initial prompts = %d, want 1", nPrompts)
	}

	// No local project config is written: an attached external server would
	// not read it, so writing it would overstate the adapter's authority.
	cfgPath := filepath.Join(cwd, ".donmai-opencode", "opencode.json")
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("attach mode wrote an inactive project config: %v", err)
	}

	// Terminate cleanly through a terminal frame.
	fc.push(evt("t", evStepEnded, map[string]any{"sessionID": fc.sessionID, "finish": "stop"}))
	events := drainHandle(ctx, h)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Errorf("terminal contract: %v", err)
	}
}
