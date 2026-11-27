package runner

import (
	"fmt"
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

// TestRun_HarnessTruncatedToolOutputStillWarns drives a truncating harness:
// it bounds an oversized tool result with the shared marker before emitting
// it, so the copy the runner receives is already within the runner's own
// limit. The runner must still record the oversize as the post-session
// warning, naming the bound the harness applied and the original size.
func TestRun_HarnessTruncatedToolOutputStillWarns(t *testing.T) {
	const harnessLimit = 256
	original := "command echo\n" + strings.Repeat("x", 5000) + "\nhttps://github.com/example/repo/pull/7\n"
	bounded, changed := agent.TruncateToolOutput(original, harnessLimit)
	if !changed {
		t.Fatal("fixture: harness truncation did not fire")
	}
	res, _ := runScriptedSession(t, scriptedSession{
		workType:        "development",
		skipSteering:    true,
		repository:      "https://github.com/example/repo",
		continueNumber:  12,
		toolOutputLimit: 4096, // above the harness bound: the runner never cuts this copy itself
		turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			events: []agent.Event{
				agent.ToolUseEvent{ToolName: "bash", ToolUseID: "call-1"},
				agent.ToolResultEvent{ToolName: "bash", ToolUseID: "call-1", Content: bounded},
			},
		}},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); oversize alone must not fail the turn", res.Status, res.FailureMode, res.Error)
	}
	if !sawToolOutputTruncationWarning(res) {
		t.Fatalf("PostSessionWarnings = %q; want the truncation warning for a harness-truncated result", res.PostSessionWarnings)
	}
	want := fmt.Sprintf("exceeded the %d-byte limit", harnessLimit)
	wantSize := fmt.Sprintf("largest result %d bytes", len(original))
	for _, w := range res.PostSessionWarnings {
		if strings.HasPrefix(w, toolOutputTruncationWarningPrefix) {
			if !strings.Contains(w, want) || !strings.Contains(w, wantSize) {
				t.Fatalf("warning = %q; want it to name %q and %q", w, want, wantSize)
			}
		}
	}
}
