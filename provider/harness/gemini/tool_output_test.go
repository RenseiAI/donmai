package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestExecuteToolCalls_BoundsModelCopyOnly drives the in-box tool loop end
// to end: an oversized tool result reaches the model (the functionResponse
// folded back into the conversation) truncated around the shared marker,
// while the ToolResultEvent handed to the runner keeps the full result so
// the runner applies its own limit and records its oversize warning.
func TestExecuteToolCalls_BoundsModelCopyOnly(t *testing.T) {
	const limit = 128
	dir := t.TempDir()
	big := strings.Repeat("x", 5000)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	h := &Handle{
		executor:           newToolExecutor(dir, nil, nil),
		events:             make(chan agent.Event, 4),
		shutdown:           make(chan struct{}),
		maxToolOutputBytes: limit,
	}
	ok := h.executeToolCalls(context.Background(), []candidateFuncCall{
		{ID: "c1", Name: "Read", Args: map[string]any{"path": "big.txt"}},
	})
	if !ok {
		t.Fatal("executeToolCalls returned false")
	}

	// Runner copy: the full result.
	select {
	case ev := <-h.events:
		tr, isResult := ev.(agent.ToolResultEvent)
		if !isResult {
			t.Fatalf("event = %T, want ToolResultEvent", ev)
		}
		if tr.Content != big {
			t.Fatalf("event content len = %d, want the full %d-byte result", len(tr.Content), len(big))
		}
	default:
		t.Fatal("no event emitted")
	}

	// Model copy: the functionResponse folded back into history.
	if len(h.contents) != 1 || len(h.contents[0].Parts) != 1 || h.contents[0].Parts[0].FunctionResponse == nil {
		t.Fatalf("contents = %+v, want one tool-result turn with one functionResponse", h.contents)
	}
	out, isString := h.contents[0].Parts[0].FunctionResponse.Response["content"].(string)
	if !isString {
		t.Fatalf("model payload = %T, want string", h.contents[0].Parts[0].FunctionResponse.Response["content"])
	}
	if len(out) > limit {
		t.Fatalf("model payload len = %d, want <= %d", len(out), limit)
	}
	if !strings.Contains(out, agent.ToolOutputTruncatedInfix) {
		t.Fatalf("model payload missing marker %q", agent.ToolOutputTruncatedInfix)
	}
}

// TestBoundToolResponse_NonStringPassThrough pins that only string values
// are bounded; structured fields such as exitCode pass through untouched.
func TestBoundToolResponse_NonStringPassThrough(t *testing.T) {
	bounded := boundToolResponse(map[string]any{"output": strings.Repeat("y", 5000), "exitCode": 0}, 128)
	if out, _ := bounded["output"].(string); len(out) > 128 {
		t.Fatalf("output len = %d, want <= 128", len(out))
	}
	if bounded["exitCode"] != 0 {
		t.Fatalf("exitCode = %v, want 0", bounded["exitCode"])
	}
}
