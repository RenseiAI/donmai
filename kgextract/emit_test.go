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
