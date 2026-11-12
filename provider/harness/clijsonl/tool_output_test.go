package clijsonl

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestHandle_BoundsToolOutputBeforeModel pins that oversized tool output
// handed through the shared JSONL driver reaches the model (and the runner)
// truncated around the shared marker, using the same documented limit the
// runner applies.
func TestHandle_BoundsToolOutputBeforeModel(t *testing.T) {
	const limit = 128
	h := &Handle{
		events:             make(chan agent.Event, 4),
		shutdown:           make(chan struct{}),
		maxToolOutputBytes: limit,
	}
	big := strings.Repeat("x", 5000)
	h.sendEvent(agent.ToolResultEvent{ToolName: "Bash", ToolUseID: "t1", Content: big})
	select {
	case ev := <-h.events:
		tr, ok := ev.(agent.ToolResultEvent)
		if !ok {
			t.Fatalf("event = %T, want ToolResultEvent", ev)
		}
		if len(tr.Content) > limit {
			t.Fatalf("truncated len = %d, want <= %d", len(tr.Content), limit)
		}
		if !strings.Contains(tr.Content, agent.ToolOutputTruncatedInfix) {
			t.Fatalf("truncated output missing marker %q: %q", agent.ToolOutputTruncatedInfix, tr.Content)
		}
	default:
		t.Fatal("no event emitted")
	}
}

// TestSpawn_ExportsToolOutputLimit pins that the spawn-time environment
// carries the resolved bound so the child-side enforcement applies the same
// limit the Go-side truncation uses.
func TestSpawn_ExportsToolOutputLimit(t *testing.T) {
	env := map[string]string{}
	limit := agent.ResolveToolOutputLimit(map[string]any{agent.ToolOutputLimitConfigKey: 777}, env)
	env = agent.WithToolOutputLimitEnv(env, limit)
	if env[agent.ToolOutputLimitEnvVar] != "777" {
		t.Fatalf("spawn env %q = %q, want 777", agent.ToolOutputLimitEnvVar, env[agent.ToolOutputLimitEnvVar])
	}
	if got := agent.ResolveToolOutputLimit(nil, env); got != 777 {
		t.Fatalf("resolved limit = %d, want 777", got)
	}
}
