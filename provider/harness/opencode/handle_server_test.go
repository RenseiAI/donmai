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

// drainHandle collects all events until the channel closes or ctx fires.
func drainHandle(ctx context.Context, h agent.Handle) []agent.Event {
	var got []agent.Event
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
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

func TestServerHandle_TerminalRemovesOwnedConfigBeforeResult(t *testing.T) {
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
	if _, err := os.Stat(boundary.home); !os.IsNotExist(err) {
		t.Fatalf("owned config survived terminal result: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after terminal: %v", err)
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
	events := drainHandle(ctx, h)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Fatalf("cleanup terminal contract: %v; events=%s", err, kindsOf(events))
	}
	if len(events) == 0 {
		t.Fatal("cleanup emitted no terminal error")
	}
	cleanup, ok := events[len(events)-1].(agent.ErrorEvent)
	if !ok || cleanup.Code != "config_cleanup_failed" {
		t.Fatalf("cleanup terminal = %#v, want config_cleanup_failed", events[len(events)-1])
	}
	for _, event := range events {
		if _, ok := event.(agent.ResultEvent); ok {
			t.Fatalf("cleanup failure retained successful ResultEvent: %s", kindsOf(events))
		}
	}
	if strings.Contains(cleanup.Message, secretSentinel) || strings.Contains(cleanup.Message, boundary.configPath) {
		t.Fatalf("cleanup event leaked secret or path: %q", cleanup.Message)
	}
	if err := h.Stop(context.Background()); !errors.Is(err, errOpenCodeConfigCleanup) {
		t.Fatalf("Stop error = %v, want bounded cleanup sentinel", err)
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
