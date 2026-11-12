package codex

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestHandle_BoundsToolOutputBeforeModel(t *testing.T) {
	h := newHandle(nil, nil, agent.Spec{}, HandleOptions{EventBuffer: 4})
	h.maxToolOutputBytes = 128
	big := strings.Repeat("x", 5000)
	h.emit(agent.ToolResultEvent{ToolName: "exec", ToolUseID: "c1", Content: big})
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
	specEnv := agent.WithToolOutputLimitEnv(nil, agent.ResolveToolOutputLimit(map[string]any{agent.ToolOutputLimitConfigKey: 777}, nil))
	if specEnv[agent.ToolOutputLimitEnvVar] != "777" {
		t.Fatalf("env = %q, want 777", specEnv[agent.ToolOutputLimitEnvVar])
	}
	h := newHandle(nil, nil, agent.Spec{
		Env:            specEnv,
		ProviderConfig: map[string]any{agent.ToolOutputLimitConfigKey: 777},
	}, HandleOptions{EventBuffer: 4})
	if got := h.toolOutputLimit(); got != 777 {
		t.Fatalf("handle limit = %d, want 777", got)
	}
}
