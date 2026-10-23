package hostwatch

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
)

// TestInteractiveDispatchedStreamParity drives two journals through the
// production render path (applySnapshot, then drainTails): an
// interactive-shaped sequence (system lifecycle markers around the same
// assistant text, tool call, result and model call) and the equivalent
// dispatched sequence (init plus the same shared events). The shared
// events must render byte-identical lines across both journals, and none
// of the newly typed kinds may render as a bare kind token with an empty
// summary. (init keeps its pre-existing kind-only line by design.)
func TestInteractiveDispatchedStreamParity(t *testing.T) {
	cost := 0.0123
	shared := []agent.Event{
		agent.AssistantTextEvent{Text: "Reviewing the diff"},
		agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "go test ./..."}},
		agent.ToolResultEvent{ToolName: "Bash", Content: "ok"},
		agent.LlmCallEvent{
			Model: "requested-model", ResponseModel: "served-model",
			InputTokens: 100, OutputTokens: 50, CachedInputTokens: 10,
			ObservedCostUsd: &cost, TurnCompleted: true,
			UsageSource: agent.LlmUsageProvider,
		},
	}

	interactive := append([]agent.Event{
		agent.SystemEvent{Subtype: "user_message", Message: "run the tests"},
	}, append(append([]agent.Event{}, shared...), agent.SystemEvent{Subtype: "turn_completed"})...)

	dispatched := append([]agent.Event{
		agent.InitEvent{SessionID: "prov-1"},
	}, shared...)

	linesInteractive := renderJournal(t, "sess-interactive", interactive)
	linesDispatched := renderJournal(t, "sess-dispatched", dispatched)

	// The shared events render byte-identical lines however the session
	// was launched: same fixed clock, same issue label, same event bytes.
	for _, kind := range []string{"thought", "tool_use", "tool_result", "llm_call"} {
		a := lineForKind(t, linesInteractive, kind, "interactive")
		b := lineForKind(t, linesDispatched, kind, "dispatched")
		if a != b {
			t.Errorf("kind %q differs:\n interactive: %q\n dispatched:  %q", kind, a, b)
		}
	}

	// No newly typed kind renders bare: every llm_call, context_usage,
	// usage and subagent row carries a summary. (init is the one
	// pre-existing kind-only row.)
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"interactive", linesInteractive},
		{"dispatched", linesDispatched},
	} {
		for _, line := range tc.lines {
			for _, bare := range []string{"] llm_call", "] context_usage", "] usage", "] subagent"} {
				if strings.HasSuffix(line, bare) {
					t.Errorf("%s journal has bare summary-less row: %q", tc.name, line)
				}
			}
		}
	}
}

// renderJournal writes state.json (with a fixed issue identifier so both
// journals share the stream label) and the given events.jsonl, then
// renders through the production path: applySnapshot, then drainTails. It
// returns the retained plain stream lines.
func renderJournal(t *testing.T, sessionID string, evs []agent.Event) []string {
	t.Helper()
	dir := t.TempDir()
	wt := filepath.Join(dir, sessionID)
	writeState(t, wt, state.State{IssueIdentifier: "ENG-77"})
	evPath := filepath.Join(wt, state.AgentDirName, "events.jsonl")

	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: sessionID, State: "running", Repository: "o/a", WorktreePath: wt,
		}},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	writeEvents(t, evPath, evs...)
	drainTails(t, m)
	return append([]string(nil), m.stream.lines...)
}

// lineForKind returns the single stream line carrying the given kind
// token, failing when the kind renders zero or several rows.
func lineForKind(t *testing.T, lines []string, kind, journal string) string {
	t.Helper()
	var got []string
	for _, line := range lines {
		if strings.Contains(line, "] "+kind+" ") || strings.HasSuffix(line, "] "+kind) {
			got = append(got, line)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%s journal: want 1 %q row, got %d: %q", journal, kind, len(got), lines)
	}
	return got[0]
}
