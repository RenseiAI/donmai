package kgextract

import (
	"context"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// fakeProvider is a minimal agent.Provider that returns a fakeHandle replaying a
// fixed event sequence. It exercises the REAL providerEmitter (accumulate
// AssistantTextEvent, observe terminal Error/Result) without a live CLI.
type fakeProvider struct {
	events   []agent.Event
	spawnErr error
	lastSpec agent.Spec
	spawnedN int
}

func (p *fakeProvider) Name() agent.ProviderName         { return agent.ProviderStub }
func (p *fakeProvider) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (p *fakeProvider) Shutdown(context.Context) error   { return nil }
func (p *fakeProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

// Manifest makes fakeProvider a HarnessProvider so it can drive the shared
// one-shot lane (agent.SpawnComplete) the providerEmitter now delegates to.
func (p *fakeProvider) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{Name: agent.HarnessStub}
}

func (p *fakeProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.spawnedN++
	p.lastSpec = spec
	if p.spawnErr != nil {
		return nil, p.spawnErr
	}
	ch := make(chan agent.Event, len(p.events))
	for _, ev := range p.events {
		ch <- ev
	}
	close(ch)
	return &fakeHandle{events: ch}, nil
}

type fakeHandle struct{ events chan agent.Event }

func (h *fakeHandle) SessionID() string                    { return "fake-session" }
func (h *fakeHandle) Events() <-chan agent.Event           { return h.events }
func (h *fakeHandle) Inject(context.Context, string) error { return agent.ErrUnsupported }
func (h *fakeHandle) Stop(context.Context) error           { return nil }

func TestProviderEmitter_AccumulatesAssistantText(t *testing.T) {
	prov := &fakeProvider{events: []agent.Event{
		agent.InitEvent{SessionID: "fake-session"},
		agent.AssistantTextEvent{Text: `{"nodes":[`},
		agent.AssistantTextEvent{Text: `],"edges":[]}`},
		agent.ResultEvent{Success: true, Message: "done"},
	}}
	em, err := NewProviderEmitter(ProviderEmitterConfig{Provider: prov, Model: "claude-x"})
	if err != nil {
		t.Fatalf("NewProviderEmitter: %v", err)
	}

	out, err := em.Emit(context.Background(), "sys prompt", "observation content")
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if out != `{"nodes":[],"edges":[]}` {
		t.Errorf("accumulated text = %q, want concatenation of assistant chunks", out)
	}
	// The constrained-emit Spec must carry the system prompt + user content +
	// single-turn cap (the documented provider-emit shape).
	if prov.lastSpec.SystemPromptAppend != "sys prompt" {
		t.Errorf("SystemPromptAppend = %q, want sys prompt", prov.lastSpec.SystemPromptAppend)
	}
	if prov.lastSpec.Prompt != "observation content" {
		t.Errorf("Prompt = %q, want observation content", prov.lastSpec.Prompt)
	}
	if prov.lastSpec.MaxTurns == nil || *prov.lastSpec.MaxTurns != 1 {
		t.Errorf("MaxTurns = %v, want 1 (single constrained turn)", prov.lastSpec.MaxTurns)
	}
	if !prov.lastSpec.Autonomous {
		t.Error("Spec.Autonomous = false, want true (non-interactive emit)")
	}
	if prov.lastSpec.Model != "claude-x" {
		t.Errorf("Model = %q, want claude-x", prov.lastSpec.Model)
	}
}

func TestProviderEmitter_ErrorEvent(t *testing.T) {
	prov := &fakeProvider{events: []agent.Event{
		agent.InitEvent{SessionID: "fake-session"},
		agent.ErrorEvent{Message: "rate limited", Code: "rate_limited"},
	}}
	em, _ := NewProviderEmitter(ProviderEmitterConfig{Provider: prov})
	if _, err := em.Emit(context.Background(), "s", "c"); err == nil {
		t.Fatal("expected error on provider ErrorEvent")
	}
}

func TestProviderEmitter_FailedResult(t *testing.T) {
	prov := &fakeProvider{events: []agent.Event{
		agent.InitEvent{SessionID: "fake-session"},
		agent.AssistantTextEvent{Text: "partial"},
		agent.ResultEvent{Success: false, Errors: []string{"max turns"}},
	}}
	em, _ := NewProviderEmitter(ProviderEmitterConfig{Provider: prov})
	if _, err := em.Emit(context.Background(), "s", "c"); err == nil {
		t.Fatal("expected error on unsuccessful ResultEvent")
	}
}

func TestProviderEmitter_NoAssistantText(t *testing.T) {
	prov := &fakeProvider{events: []agent.Event{
		agent.InitEvent{SessionID: "fake-session"},
		agent.ResultEvent{Success: true},
	}}
	em, _ := NewProviderEmitter(ProviderEmitterConfig{Provider: prov})
	if _, err := em.Emit(context.Background(), "s", "c"); err == nil {
		t.Fatal("expected error when emit produced no assistant text")
	}
}

func TestProviderEmitter_SpawnError(t *testing.T) {
	prov := &fakeProvider{spawnErr: errors.New("no PATH")}
	em, _ := NewProviderEmitter(ProviderEmitterConfig{Provider: prov})
	if _, err := em.Emit(context.Background(), "s", "c"); err == nil {
		t.Fatal("expected error when Spawn fails")
	}
}

func TestNewProviderEmitter_NilProvider(t *testing.T) {
	if _, err := NewProviderEmitter(ProviderEmitterConfig{}); err == nil {
		t.Fatal("expected error for nil provider")
	}
}

// ── non-agentic routing ──────────────────────────────────────────────────────

// fakeOneShotProvider is a fakeProvider that ALSO implements
// agent.OneShotProvider — i.e. a harness with its own non-agentic completion
// mode, which is what the real claude harness became.
type fakeOneShotProvider struct {
	fakeProvider
	completeN   int
	lastReq     agent.OneShotRequest
	text        string
	completeErr error
}

func (p *fakeOneShotProvider) Complete(_ context.Context, req agent.OneShotRequest) (agent.OneShotResult, error) {
	p.completeN++
	p.lastReq = req
	if p.completeErr != nil {
		return agent.OneShotResult{}, p.completeErr
	}
	return agent.OneShotResult{Text: p.text}, nil
}

// TestProviderEmitter_RoutesToNonAgenticOneShotLane is the kgextract-side red
// for defect #4.
//
// The emitter used to call agent.SpawnComplete DIRECTLY, which pins every emit
// to the agent-harness projection: for the claude harness that means booting the
// full Claude Code agent (tools, MCP servers, project memory) to produce one JSON
// object. MaxTurns=1 caps the loop without avoiding the cost of standing it up,
// and in the fleet that cold start blew the 120s per-observation deadline on
// EVERY observation — zero graph nodes written.
//
// Going through agent.Complete instead lets a harness that HAS a non-agentic
// completion mode serve the emit from it. This test proves the routing: with a
// provider that implements OneShotProvider, Complete must be called and Spawn
// must NOT be — reverting to SpawnComplete inverts both counters.
func TestProviderEmitter_RoutesToNonAgenticOneShotLane(t *testing.T) {
	prov := &fakeOneShotProvider{text: `{"nodes":[],"edges":[]}`}
	em, err := NewProviderEmitter(ProviderEmitterConfig{Provider: prov, Model: "claude-x"})
	if err != nil {
		t.Fatalf("NewProviderEmitter: %v", err)
	}

	out, err := em.Emit(context.Background(), "sys prompt", "observation content")
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if out != `{"nodes":[],"edges":[]}` {
		t.Errorf("emit text = %q, want the one-shot completion", out)
	}
	if prov.completeN != 1 {
		t.Errorf("OneShotProvider.Complete calls = %d, want 1 — the emit must take the non-agentic lane", prov.completeN)
	}
	if prov.spawnedN != 0 {
		t.Errorf("Spawn calls = %d, want 0 — a constrained triple extraction must not boot an agent session", prov.spawnedN)
	}
	// The request must still carry the platform's extraction prompt as SYSTEM and
	// the observation as the single user turn.
	if prov.lastReq.System != "sys prompt" {
		t.Errorf("System = %q, want sys prompt", prov.lastReq.System)
	}
	if len(prov.lastReq.Messages) != 1 || prov.lastReq.Messages[0].Content != "observation content" {
		t.Errorf("Messages = %+v, want a single user turn with the observation", prov.lastReq.Messages)
	}
	if prov.lastReq.Model != "claude-x" {
		t.Errorf("Model = %q, want claude-x", prov.lastReq.Model)
	}
	// kgextract owns the {nodes,edges} shape and validates it in parse.go, so it
	// deliberately passes no ResponseSchema (the parse-or-drop posture).
	if len(prov.lastReq.ResponseSchema) != 0 {
		t.Errorf("ResponseSchema = %q, want none (kgextract validates in parse.go)", prov.lastReq.ResponseSchema)
	}
}

// TestProviderEmitter_OneShotLaneErrorSurfaces proves a failure on the
// non-agentic lane becomes a per-observation emit error rather than empty text
// the executor would parse as "the model found no triples".
func TestProviderEmitter_OneShotLaneErrorSurfaces(t *testing.T) {
	prov := &fakeOneShotProvider{completeErr: errors.New("one-shot failed (api_error status=404)")}
	em, _ := NewProviderEmitter(ProviderEmitterConfig{Provider: prov})
	_, err := em.Emit(context.Background(), "s", "c")
	if err == nil {
		t.Fatal("expected an error when the one-shot lane fails")
	}
	if prov.spawnedN != 0 {
		t.Errorf("Spawn calls = %d, want 0 — a one-shot failure must not fall back to an agent session", prov.spawnedN)
	}
}

// TestProviderEmitter_HarnessWithoutOneShotStillWorks proves the fallback is
// intact: a harness with no non-agentic mode (the native gemini/ollama shape,
// which already delivers structured output inside Spawn) keeps riding
// agent.SpawnComplete. The fix must not strand those providers.
func TestProviderEmitter_HarnessWithoutOneShotStillWorks(t *testing.T) {
	prov := &fakeProvider{events: []agent.Event{
		agent.InitEvent{SessionID: "fake-session"},
		agent.AssistantTextEvent{Text: `{"nodes":[],"edges":[]}`},
		agent.ResultEvent{Success: true},
	}}
	em, _ := NewProviderEmitter(ProviderEmitterConfig{Provider: prov})
	out, err := em.Emit(context.Background(), "s", "c")
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if out != `{"nodes":[],"edges":[]}` {
		t.Errorf("emit text = %q, want the accumulated assistant text", out)
	}
	if prov.spawnedN != 1 {
		t.Errorf("Spawn calls = %d, want 1 — a harness without a one-shot lane still rides SpawnComplete", prov.spawnedN)
	}
}
