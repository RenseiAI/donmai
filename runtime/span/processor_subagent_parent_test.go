package span

import (
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestProcessor_SubagentNestsUnderDelegatingToolSpan pins the nesting rule:
// a `started` lifecycle whose ToolUseID matches a pending tool call parents
// the subagent span under that tool span instead of the session root.
func TestProcessor_SubagentNestsUnderDelegatingToolSpan(t *testing.T) {
	t.Parallel()
	recorder := &spanRecorder{}
	now := time.Unix(1_700_000_000, 0)
	p := newTestProcessor(t, recorder, func() time.Time { return now })

	toolUse := p.Process(agent.ToolUseEvent{ToolName: "task", ToolUseID: "call_nest_1"})[0].(agent.ToolUseEvent)
	started := p.Process(agent.SubagentEvent{ToolName: "task", ToolUseID: "call_nest_1", Phase: agent.SubagentStarted})[0].(agent.SubagentEvent)
	if started.ParentSpanID != toolUse.SpanID {
		t.Fatalf("subagent parent = %q, want delegating tool span %q", started.ParentSpanID, toolUse.SpanID)
	}
	_ = p.Process(agent.ToolResultEvent{ToolName: "task", ToolUseID: "call_nest_1", Content: "done"})
	completed := p.Process(agent.SubagentEvent{ToolName: "task", ToolUseID: "call_nest_1", Phase: agent.SubagentCompleted})[0].(agent.SubagentEvent)
	if completed.ParentSpanID != toolUse.SpanID {
		t.Fatalf("completed subagent parent = %q, want delegating tool span %q", completed.ParentSpanID, toolUse.SpanID)
	}
	p.Finish("completed", "")
	for _, s := range recorder.snapshot() {
		sub, ok := s.(agent.SubagentSpan)
		if !ok {
			continue
		}
		if sub.ParentSpanID != toolUse.SpanID {
			t.Fatalf("subagent span parent = %q, want delegating tool span %q", sub.ParentSpanID, toolUse.SpanID)
		}
		return
	}
	t.Fatal("no subagent span emitted")
}
