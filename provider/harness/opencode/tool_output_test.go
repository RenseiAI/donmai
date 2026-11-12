package opencode

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestServerHandle_BoundsToolOutputBeforeModel(t *testing.T) {
	h := newServerHandle(nil, nil, "sess", agent.Spec{}, nil)
	h.maxToolOutputBytes = 128
	big := strings.Repeat("x", 5000)
	h.emit(agent.ToolResultEvent{ToolName: "read", ToolUseID: "c1", Content: big})
	select {
	case ev := <-h.events:
		tr, ok := ev.(agent.ToolResultEvent)
		if !ok {
			t.Fatalf("event = %T, want ToolResultEvent", ev)
		}
		if len(tr.Content) > 128 {
			t.Fatalf("len = %d, want <= 128", len(tr.Content))
		}
		if !strings.Contains(tr.Content, agent.ToolOutputTruncatedInfix) {
			t.Fatalf("missing marker %q", agent.ToolOutputTruncatedInfix)
		}
	default:
		t.Fatal("no event emitted")
	}
}

func TestOpenCodeHandle_BoundsToolOutputBeforeModel(t *testing.T) {
	h := &openCodeHandle{
		events:             make(chan agent.Event, 4),
		shutdown:           make(chan struct{}),
		maxToolOutputBytes: 128,
	}
	big := strings.Repeat("y", 5000)
	h.sendEvent(agent.ToolResultEvent{ToolName: "bash", ToolUseID: "c2", Content: big})
	select {
	case ev := <-h.events:
		tr, ok := ev.(agent.ToolResultEvent)
		if !ok {
			t.Fatalf("event = %T, want ToolResultEvent", ev)
		}
		if len(tr.Content) > 128 {
			t.Fatalf("len = %d, want <= 128", len(tr.Content))
		}
		if !strings.Contains(tr.Content, agent.ToolOutputTruncatedInfix) {
			t.Fatalf("missing marker %q", agent.ToolOutputTruncatedInfix)
		}
	default:
		t.Fatal("no event emitted")
	}
}

func TestSpawn_ExportsToolOutputLimit(t *testing.T) {
	env := agent.WithToolOutputLimitEnv(nil, agent.ResolveToolOutputLimit(map[string]any{agent.ToolOutputLimitConfigKey: 777}, nil))
	if env[agent.ToolOutputLimitEnvVar] != "777" {
		t.Fatalf("env = %q, want 777", env[agent.ToolOutputLimitEnvVar])
	}
	if got := agent.ResolveToolOutputLimit(map[string]any{agent.ToolOutputLimitConfigKey: 777}, env); got != 777 {
		t.Fatalf("resolved = %d, want 777", got)
	}
}
