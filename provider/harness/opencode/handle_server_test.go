package opencode

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
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

// TestProvider_SpawnServer_AttachWiring drives the full Provider.Spawn Lane-B
// path (attach mode + injected client factory) end to end without a real
// binary, and confirms the per-spawn opencode.json was written with the
// provider lockout.
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
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyOpenAI, Model: "gpt-x", BaseURL: "http://compat/v1",
		},
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

	// The unique per-spawn config was written with the lockout.
	paths, err := filepath.Glob(filepath.Join(cwd, ".donmai-opencode", "spawn-*", "opencode.json"))
	if err != nil {
		t.Fatalf("glob injected config: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("injected config paths = %v, want exactly one", paths)
	}
	cfgPath := paths[0]
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read injected config: %v", err)
	}
	var cfg ocConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config JSON: %v", err)
	}
	if len(cfg.EnabledProviders) != 1 || cfg.EnabledProviders[0] != OCProviderID {
		t.Errorf("enabled_providers = %v, want [%s] (fallback lockout)", cfg.EnabledProviders, OCProviderID)
	}
	if cfg.Model != OCProviderID+"/gpt-x" {
		t.Errorf("config model = %q, want %s/gpt-x", cfg.Model, OCProviderID)
	}

	// Terminate cleanly through a terminal frame.
	fc.push(evt("t", evStepEnded, map[string]any{"sessionID": fc.sessionID, "finish": "stop"}))
	events := drainHandle(ctx, h)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Errorf("terminal contract: %v", err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("per-spawn config still exists after terminal teardown: err=%v", err)
	}
}
