package gemini

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestHandle_BoundsToolOutputBeforeModel pins that oversized tool output
// executed in-box reaches the model (the functionResponse folded back into
// the conversation) truncated around the shared marker, and that the event
// handed to the runner carries the same bounded copy.
func TestHandle_BoundsToolOutputBeforeModel(t *testing.T) {
	const limit = 128
	h := &Handle{
		events:             make(chan agent.Event, 4),
		shutdown:           make(chan struct{}),
		maxToolOutputBytes: limit,
	}
	big := strings.Repeat("x", 5000)
	// Event path.
	h.sendEvent(agent.ToolResultEvent{ToolName: "Bash", ToolUseID: "c1", Content: big})
	select {
	case ev := <-h.events:
		tr, ok := ev.(agent.ToolResultEvent)
		if !ok {
			t.Fatalf("event = %T, want ToolResultEvent", ev)
		}
		if len(tr.Content) > limit {
			t.Fatalf("event len = %d, want <= %d", len(tr.Content), limit)
		}
		if !strings.Contains(tr.Content, agent.ToolOutputTruncatedInfix) {
			t.Fatalf("event missing marker %q", agent.ToolOutputTruncatedInfix)
		}
	default:
		t.Fatal("no event emitted")
	}
	// Model path: the functionResponse payload folded back into history.
	resp := map[string]any{"output": big, "exitCode": 0}
	bounded := boundToolResponse(resp, limit)
	out, ok := bounded["output"].(string)
	if !ok {
		t.Fatalf("bounded output = %T, want string", bounded["output"])
	}
	if len(out) > limit {
		t.Fatalf("model payload len = %d, want <= %d", len(out), limit)
	}
	if !strings.Contains(out, agent.ToolOutputTruncatedInfix) {
		t.Fatalf("model payload missing marker %q", agent.ToolOutputTruncatedInfix)
	}
	// Non-string values pass through untouched.
	if bounded["exitCode"] != 0 {
		t.Fatalf("exitCode = %v, want 0", bounded["exitCode"])
	}
}

func TestSpawn_ExportsToolOutputLimit(t *testing.T) {
	env := agent.WithToolOutputLimitEnv(nil, agent.ResolveToolOutputLimit(map[string]any{agent.ToolOutputLimitConfigKey: 777}, nil))
	if env[agent.ToolOutputLimitEnvVar] != "777" {
		t.Fatalf("env = %q, want 777", env[agent.ToolOutputLimitEnvVar])
	}
	h := &Handle{maxToolOutputBytes: agent.ResolveToolOutputLimit(map[string]any{agent.ToolOutputLimitConfigKey: 777}, env)}
	if got := h.toolOutputLimit(); got != 777 {
		t.Fatalf("handle limit = %d, want 777", got)
	}
}
