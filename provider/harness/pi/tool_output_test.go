package pi

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestHandle_BoundsToolOutputBeforeModel(t *testing.T) {
	h := newHandle(nil, nil, agent.Spec{}, "")
	h.maxToolOutputBytes = 128
	// Drain the buffered channel via emit (non-blocking drop on full is
	// okay; buffer is 256 so one event always lands).
	big := strings.Repeat("x", 5000)
	h.emit(agent.ToolResultEvent{ToolName: "bash", ToolUseID: "c1", Content: big})
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
	h := newHandle(nil, nil, agent.Spec{
		Env:            env,
		ProviderConfig: map[string]any{agent.ToolOutputLimitConfigKey: 777},
	}, "")
	if got := h.toolOutputLimit(); got != 777 {
		t.Fatalf("handle limit = %d, want 777", got)
	}
}
