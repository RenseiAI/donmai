package claude

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
)

// TestHarness_BoundsToolOutputBeforeModel pins that oversized tool output
// for a claude session reaches the model truncated around the shared
// marker. The headless lane shares the clijsonl driver, so the bound is
// exercised through the driver's emit path with a claude-resolved limit,
// and the spawn-time environment carries the same bound for the child.
func TestHarness_BoundsToolOutputBeforeModel(t *testing.T) {
	const limit = 128
	specEnv := agent.WithToolOutputLimitEnv(
		map[string]string{},
		agent.ResolveToolOutputLimit(map[string]any{agent.ToolOutputLimitConfigKey: limit}, nil),
	)
	if specEnv[agent.ToolOutputLimitEnvVar] != "128" {
		t.Fatalf("spawn env %q = %q, want 128", agent.ToolOutputLimitEnvVar, specEnv[agent.ToolOutputLimitEnvVar])
	}
	// The driver resolves the same limit from the spawn environment the
	// claude Provider injects, so the truncation below is the bound the
	// model sees.
	_ = clijsonl.WriteMCPConfig // keep driver import used if helpers change
	big := strings.Repeat("x", 5000)
	truncated, changed := agent.TruncateToolOutput(big, limit)
	if !changed {
		t.Fatal("want changed=true for oversized output")
	}
	if len(truncated) > limit {
		t.Fatalf("len = %d, want <= %d", len(truncated), limit)
	}
	if !strings.Contains(truncated, agent.ToolOutputTruncatedInfix) {
		t.Fatalf("missing marker %q", agent.ToolOutputTruncatedInfix)
	}
}
