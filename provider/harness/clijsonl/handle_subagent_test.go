package clijsonl

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// The Handle's stdout and resume readers decode lines through the
// stateful LineMapper (h.mapper.MapLine) so native delegation tool
// calls emit the typed sub-agent lifecycle alongside the plain tool
// events. These tests drive the production entry points — SpawnBinary
// stdout for the parent path, Inject for the resume path — with raw
// stream-json lines, so reverting either call site to the stateless
// mapLine drops the SubagentEvents and both tests go RED.
func TestHandle_StdoutPath_EmitsSubagentLifecycle(t *testing.T) {
	t.Parallel()

	body := `{"type":"system","subtype":"init","session_id":"sess-sub-handle"}
{"type":"assistant","session_id":"sess-sub-handle","message":{"model":"claude-opus-4-7","id":"msg_1","role":"assistant","content":[{"type":"tool_use","id":"toolu_agent_1","name":"Agent","input":{"description":"delegate"}}]}}
{"type":"user","session_id":"sess-sub-handle","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_agent_1","content":"done","is_error":false}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"usage":{}}`
	cli := fakeCLI(t, body)

	h := spawnFake(t, cli, agent.Spec{Prompt: "delegate please"})
	defer func() { _ = h.Stop(t.Context()) }()

	events := collect(t, h)

	var phases []agent.SubagentPhase
	var ids []string
	for _, ev := range events {
		sub, ok := ev.(agent.SubagentEvent)
		if !ok {
			continue
		}
		phases = append(phases, sub.Phase)
		ids = append(ids, sub.ToolUseID)
	}
	if len(phases) != 2 || phases[0] != agent.SubagentStarted || phases[1] != agent.SubagentCompleted {
		t.Fatalf("stdout path subagent phases = %v, want [started completed]", phases)
	}
	if len(ids) != 2 || ids[0] != "toolu_agent_1" || ids[1] != "toolu_agent_1" {
		t.Fatalf("stdout path subagent toolUseIds = %v, want [toolu_agent_1 toolu_agent_1]", ids)
	}
}

// The Inject resume reader shares the same mapper wiring: a failing
// delegation on the resume turn must surface SubagentFailed through
// the production Inject path, not only through the mapper helper.
func TestHandle_ResumePath_EmitsSubagentFailed(t *testing.T) {
	t.Parallel()

	parentBody := `{"type":"system","subtype":"init","session_id":"sess-sub-resume"}
{"type":"assistant","session_id":"sess-sub-resume","message":{"role":"assistant","content":[{"type":"text","text":"Turn 1."}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"usage":{}}`
	resumeBody := `{"type":"system","subtype":"init","session_id":"sess-sub-resume"}
{"type":"assistant","session_id":"sess-sub-resume","message":{"model":"claude-opus-4-7","id":"msg_2","role":"assistant","content":[{"type":"tool_use","id":"toolu_agent_9","name":"Agent","input":{"description":"delegate"}}]}}
{"type":"user","session_id":"sess-sub-resume","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_agent_9","content":"boom","is_error":true}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"usage":{}}`
	cli := fakeMultiCLI(t, parentBody, resumeBody, t.TempDir()+"/argv-trace.log")

	h := spawnFake(t, cli, agent.Spec{Prompt: "first turn"})
	defer func() { _ = h.Stop(t.Context()) }()

	_ = drainUntilResult(t, h)
	if err := h.Inject(t.Context(), "follow up please"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	resumeEvents := drainUntilResult(t, h)

	var phases []agent.SubagentPhase
	var ids []string
	for _, ev := range resumeEvents {
		sub, ok := ev.(agent.SubagentEvent)
		if !ok {
			continue
		}
		phases = append(phases, sub.Phase)
		ids = append(ids, sub.ToolUseID)
	}
	if len(phases) != 2 || phases[0] != agent.SubagentStarted || phases[1] != agent.SubagentFailed {
		t.Fatalf("resume path subagent phases = %v, want [started failed]", phases)
	}
	if len(ids) != 2 || ids[0] != "toolu_agent_9" || ids[1] != "toolu_agent_9" {
		t.Fatalf("resume path subagent toolUseIds = %v, want [toolu_agent_9 toolu_agent_9]", ids)
	}
}
