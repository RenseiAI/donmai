package codex

import (
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestProvider_EffortWireKey drives Spawn and Resume against the fake
// app-server and reads the turn/start params as they arrived on its stdin.
// A configured effort must reach the app-server as TurnStartParams.effort on
// every turn/start; reasoningEffort is a response-only field, and the
// app-server drops it without an error when it appears on a request.
func TestProvider_EffortWireKey(t *testing.T) {
	t.Parallel()
	p, fs := newSkillProvider(t, nil)
	spec := agent.Spec{Prompt: "go", Cwd: t.TempDir(), Model: "gpt-5.6-sol", Effort: agent.EffortXHigh}

	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drainEvents(t, h.Events(), 5*time.Second)
	r, err := p.Resume(t.Context(), "thread-S", spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	_ = drainEvents(t, r.Events(), 5*time.Second)

	turns := recordedParams(fs, "turn/start")
	if len(turns) != 2 {
		t.Fatalf("turn/start count = %d, want 2 (spawn + resume)", len(turns))
	}
	for i, params := range turns {
		if got := params["effort"]; got != "xhigh" {
			t.Errorf("turn/start #%d effort = %v, want \"xhigh\": %v", i+1, got, params)
		}
		if legacy, ok := params["reasoningEffort"]; ok {
			t.Errorf("turn/start #%d carries reasoningEffort=%v, which the app-server ignores", i+1, legacy)
		}
	}
	for _, method := range []string{"thread/start", "thread/resume"} {
		for _, params := range recordedParams(fs, method) {
			for _, key := range []string{"effort", "reasoningEffort"} {
				if v, ok := params[key]; ok {
					t.Errorf("%s carries %s=%v; effort rides turn/start", method, key, v)
				}
			}
		}
	}
}
