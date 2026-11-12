package runner

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestTruncateToolOutput_Marker(t *testing.T) {
	big := strings.Repeat("x", DefaultMaxToolOutputBytes+1000)
	got, changed := truncateToolOutput(big, DefaultMaxToolOutputBytes)
	if !changed {
		t.Fatal("want changed=true for oversized output")
	}
	if len(got) > DefaultMaxToolOutputBytes {
		t.Fatalf("truncated len = %d, want <= %d", len(got), DefaultMaxToolOutputBytes)
	}
	if !strings.Contains(got, agent.ToolOutputTruncatedInfix) {
		t.Fatalf("truncated output missing marker %q", agent.ToolOutputTruncatedInfix)
	}
}

func TestRunnerToolOutputLimit_Default(t *testing.T) {
	var r *Runner
	if got := r.toolOutputLimit(); got != DefaultMaxToolOutputBytes {
		t.Fatalf("nil runner limit = %d, want default %d", got, DefaultMaxToolOutputBytes)
	}
	if DefaultMaxToolOutputBytes != agent.DefaultMaxToolOutputBytes {
		t.Fatalf("runner default %d != agent default %d; the limit must be shared",
			DefaultMaxToolOutputBytes, agent.DefaultMaxToolOutputBytes)
	}
}

func TestRunnerToolOutputLimit_Custom(t *testing.T) {
	r := &Runner{maxToolOutputBytes: 100}
	if got := r.toolOutputLimit(); got != 100 {
		t.Fatalf("custom limit = %d, want 100", got)
	}
	big := strings.Repeat("y", 1000)
	got, changed := truncateToolOutput(big, r.toolOutputLimit())
	if !changed {
		t.Fatal("want changed=true")
	}
	if len(got) > 100 {
		t.Fatalf("len = %d, want <= 100", len(got))
	}
	if !strings.Contains(got, agent.ToolOutputTruncatedInfix) {
		t.Fatalf("missing marker %q", agent.ToolOutputTruncatedInfix)
	}
}
