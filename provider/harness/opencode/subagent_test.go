package opencode

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// runHandleOverLines drives the production Lane-A entry point —
// openCodeHandle.readStdout — over the given NDJSON lines through an
// in-memory pipe (no subprocess, so no per-test script file and no
// ETXTBSY window), and returns every event the reader forwarded. The
// handle carries a real `sleep` child that outlives the read so
// readStdout's cmd.Wait blocks until Stop kills the process group,
// exactly like a live `opencode run` child.
func runHandleOverLines(t *testing.T, lines []string) []agent.Event {
	t.Helper()
	reader, writer := io.Pipe()
	cmd := exec.Command("sleep", "30")
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start `sleep` child: %v", err)
	}
	h := &openCodeHandle{
		cmd:        cmd,
		events:     make(chan agent.Event, eventBufferSize),
		logger:     slog.Default(),
		stdoutPipe: reader,
		stderrPipe: reader,
		stderrBuf:  &boundedBuffer{limit: stderrBufferSize},
		shutdown:   make(chan struct{}),
		done:       make(chan struct{}),
	}
	go h.readStdout()
	t.Cleanup(func() {
		_ = writer.Close()
		_ = h.Stop(context.Background())
	})
	for _, line := range lines {
		if _, err := io.WriteString(writer, line+"\n"); err != nil {
			t.Fatalf("write NDJSON line: %v", err)
		}
	}
	_ = writer.Close()
	var events []agent.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return events
			}
			events = append(events, ev)
			if _, ok := ev.(agent.ResultEvent); ok {
				return events
			}
		case <-timeout:
			t.Fatalf("timed out waiting for terminal event; got %d events", len(events))
			return events
		}
	}
}

// delegationNDJSON is a Lane-A fixture: a native `task` delegation whose
// tool_use and tool_result share one CallID, followed by a terminal
// step_finish. The stateful line mapper must pair the two tool lines into
// the typed sub-agent lifecycle (started, then completed) carrying the
// delegating CallID.
const delegationNDJSON = `{"type":"step_start","sessionID":"ses_delegation_001","part":{"type":"step-start"}}
{"type":"tool_use","sessionID":"ses_delegation_001","part":{"type":"tool","tool":"task","callID":"call_delegate_1","state":{"status":"completed","input":{"description":"delegate"},"output":"done"}}}
{"type":"step_finish","sessionID":"ses_delegation_001","part":{"type":"step-finish","reason":"stop","tokens":{"total":50,"input":40,"output":10},"cost":0}}
`

// failedDelegationNDJSON mirrors delegationNDJSON with a terminal error
// state so the lifecycle must end in `failed` rather than `completed`.
const failedDelegationNDJSON = `{"type":"step_start","sessionID":"ses_delegation_002","part":{"type":"step-start"}}
{"type":"tool_use","sessionID":"ses_delegation_002","part":{"type":"tool","tool":"task","callID":"call_delegate_9","state":{"status":"error","input":{"description":"delegate"},"output":"boom"}}}
{"type":"step_finish","sessionID":"ses_delegation_002","part":{"type":"step-finish","reason":"stop","tokens":{"total":50,"input":40,"output":10},"cost":0}}
`

// splitDelegationLines covers the two-line shape: a pending tool_use line
// (started) followed by a separate completed tool_use line for the same
// CallID (terminal). The pending line must emit exactly one `started`; the
// completed line must emit the terminal without a second `started`.
func splitDelegationLines() (pending, completed []byte) {
	pending = []byte(`{"type":"tool_use","sessionID":"ses_split","part":{"type":"tool","tool":"task","callID":"call_split_1","state":{"status":"pending","input":{"description":"delegate"}}}}`)
	completed = []byte(`{"type":"tool_use","sessionID":"ses_split","part":{"type":"tool","tool":"task","callID":"call_split_1","state":{"status":"completed","input":{"description":"delegate"},"output":"done"}}}`)
	return pending, completed
}

func subagentPhases(events []agent.Event) ([]agent.SubagentPhase, []string) {
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
	return phases, ids
}

// TestLineMapper_DelegationLifecycle drives the production Lane-A entry
// point (the handle's stdout reader, fed through an in-memory pipe instead
// of a subprocess) and asserts the typed lifecycle: `started` then
// `completed`, both carrying the delegating CallID.
func TestLineMapper_DelegationLifecycle(t *testing.T) {
	t.Parallel()

	events := runHandleOverLines(t, strings.Split(strings.TrimSuffix(delegationNDJSON, "\n"), "\n"))
	phases, ids := subagentPhases(events)
	if len(phases) != 2 || phases[0] != agent.SubagentStarted || phases[1] != agent.SubagentCompleted {
		t.Fatalf("delegation lifecycle phases = %v, want [started completed]", phases)
	}
	if len(ids) != 2 || ids[0] != "call_delegate_1" || ids[1] != "call_delegate_1" {
		t.Fatalf("delegation lifecycle toolUseIds = %v, want [call_delegate_1 call_delegate_1]", ids)
	}
}

// TestLineMapper_DelegationFailed is the error half: a terminal error state
// must surface `failed` rather than `completed`.
func TestLineMapper_DelegationFailed(t *testing.T) {
	t.Parallel()

	events := runHandleOverLines(t, strings.Split(strings.TrimSuffix(failedDelegationNDJSON, "\n"), "\n"))
	phases, ids := subagentPhases(events)
	if len(phases) != 2 || phases[0] != agent.SubagentStarted || phases[1] != agent.SubagentFailed {
		t.Fatalf("failed delegation lifecycle phases = %v, want [started failed]", phases)
	}
	if len(ids) != 2 || ids[0] != "call_delegate_9" || ids[1] != "call_delegate_9" {
		t.Fatalf("failed delegation lifecycle toolUseIds = %v, want [call_delegate_9 call_delegate_9]", ids)
	}
}

// TestLineMapper_SplitDelegationLines pins the two-line shape at the mapper
// level: the pending line emits exactly one `started`, and the later
// completed line for the same CallID emits the terminal without a duplicate
// `started`.
func TestLineMapper_SplitDelegationLines(t *testing.T) {
	t.Parallel()

	pending, completed := splitDelegationLines()
	var m openCodeLineMapper
	first := m.mapLine(pending)
	phases, _ := subagentPhases(first)
	if len(phases) != 1 || phases[0] != agent.SubagentStarted {
		t.Fatalf("pending line subagent phases = %v, want [started]", phases)
	}
	second := m.mapLine(completed)
	phases, _ = subagentPhases(second)
	if len(phases) != 1 || phases[0] != agent.SubagentCompleted {
		t.Fatalf("completed line subagent phases = %v, want [completed] (no duplicate started)", phases)
	}
}

// TestLineMapper_NonDelegationEmitsNoLifecycle guards the matcher: a
// non-delegation tool must not emit any lifecycle event.
func TestLineMapper_NonDelegationEmitsNoLifecycle(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"tool_use","sessionID":"ses_x","part":{"type":"tool","tool":"read","callID":"call_read_1","state":{"status":"completed","input":{"filePath":"/tmp"},"output":"contents"}}}`)
	var m openCodeLineMapper
	phases, _ := subagentPhases(m.mapLine(line))
	if len(phases) != 0 {
		t.Fatalf("non-delegation tool emitted subagent phases = %v, want none", phases)
	}
}

// testBudgetEnforcer is a minimal local stand-in for the runner's budget
// gate: it counts typed `started` lifecycle events and refuses past the
// limit with the sub-agent cap. It mirrors the production counting rule
// (BudgetEnforcer.ObserveEvent counts SubagentStarted against
// MaxSubAgents) without importing the runner package, which would create
// an import cycle from this package's tests.
type testBudgetEnforcer struct {
	limit int
	seen  int
}

func (e *testBudgetEnforcer) observe(ev agent.Event) (breached bool, capName string) {
	sub, ok := ev.(agent.SubagentEvent)
	if !ok || sub.Phase != agent.SubagentStarted {
		return false, ""
	}
	e.seen++
	if e.seen > e.limit {
		return true, "max-sub-agents"
	}
	return false, ""
}

// TestLineMapper_CapRefusesPastLimit shows the cap counting the emitted
// lifecycle: with a one-delegation budget, the first `started` passes and
// the second breaches with the sub-agent cap. The counting rule mirrors
// the production budget entry point (BudgetEnforcer.ObserveEvent counts
// typed starts against MaxSubAgents); the full production wiring is
// pinned by the runner's own budget tests over the same event shape.
func TestLineMapper_CapRefusesPastLimit(t *testing.T) {
	t.Parallel()

	var m openCodeLineMapper
	var starts []agent.SubagentEvent
	for _, callID := range []string{"call_cap_1", "call_cap_2"} {
		line := []byte(`{"type":"tool_use","sessionID":"ses_cap","part":{"type":"tool","tool":"task","callID":"` + callID + `","state":{"status":"pending","input":{}}}}`)
		for _, ev := range m.mapLine(line) {
			if sub, ok := ev.(agent.SubagentEvent); ok && sub.Phase == agent.SubagentStarted {
				starts = append(starts, sub)
			}
		}
	}
	if len(starts) != 2 {
		t.Fatalf("emitted %d sub-agent starts, want 2", len(starts))
	}
	one := testBudgetEnforcer{limit: 1}
	_ = prompt.StageBudget{}
	if breached, _ := one.observe(starts[0]); breached {
		t.Fatal("first delegation should be within budget, got breach")
	}
	if breached, capName := one.observe(starts[1]); !breached {
		t.Fatal("second delegation past a one-delegation cap should breach, got nil")
	} else if capName != "max-sub-agents" {
		t.Fatalf("breach cap = %s, want %s", capName, "max-sub-agents")
	}
}

// TestSSEMapper_DelegationLifecycle pins the Lane-B equivalent: a `task`
// tool.called frame emits `started`, and the matching tool.failed frame
// emits `failed`, both carrying the delegating CallID.
func TestSSEMapper_DelegationLifecycle(t *testing.T) {
	t.Parallel()

	const sid = "ses_sse_delegation"
	m := newSSEMapper(sid)
	events := mapAll(
		m,
		evt("e1", evSessionCreated, map[string]any{"sessionID": sid}),
		evt("e2", evToolCalled, map[string]any{"sessionID": sid, "callID": "c_delegate_1", "tool": "task", "input": map[string]any{}}),
		evt("e3", evToolFailed, map[string]any{"sessionID": sid, "callID": "c_delegate_1", "tool": "task", "error": "boom"}),
	)
	phases, ids := subagentPhases(events)
	if len(phases) != 2 || phases[0] != agent.SubagentStarted || phases[1] != agent.SubagentFailed {
		t.Fatalf("SSE delegation lifecycle phases = %v, want [started failed]", phases)
	}
	if len(ids) != 2 || ids[0] != "c_delegate_1" || ids[1] != "c_delegate_1" {
		t.Fatalf("SSE delegation lifecycle toolUseIds = %v, want [c_delegate_1 c_delegate_1]", ids)
	}
}
