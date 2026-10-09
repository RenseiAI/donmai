package hostwatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/charmbracelet/x/ansi"
)

func TestIdentityObservedResponseAndCardSurviveRefresh(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, state.State{SessionID: "one", StartedAt: 1000, AgentCardID: "stale", AgentCardName: "Stale"})
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "one", WorktreePath: dir, AcceptedAt: "first", AgentCardID: "card-review", AgentCardName: "Reviewer", Harness: "pi", Model: "requested-alias", ModelProvider: "configured-vendor"}, {SessionID: "two"}}}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	// Requested/configured identity and synthetic aggregate events prove no
	// actual response, even when they carry usage or plausible aliases.
	m.applyTailBatch([]TailEvent{{SessionID: "one", Event: agent.LlmCallEvent{System: "configured-vendor", Model: "requested-alias", InputTokens: 1}}, {SessionID: "one", Event: agent.LlmCallEvent{Synthetic: true, ResponseModel: "invented", ResponseModelProvider: "invented", ModelSnapshotID: "invented"}}})
	assertIdentityText(t, m.cards[0], "Agent card Reviewer", "Card ID card-review", "Model identity unknown", "Actual provider unknown", "Model version unknown", "model requested-alias", "Endpoint surface configured-vendor")
	m.applyTailBatch([]TailEvent{{SessionID: "one", Replay: true, Event: agent.LlmCallEvent{ResponseModel: "served-id", ResponseModelProvider: "response-vendor", ModelSnapshotID: "snapshot-v2"}}})
	if !m.cards[0].LastWorkAt.IsZero() || !m.cards[0].LastOutputAt.IsZero() {
		t.Fatal("historical identity changed freshness")
	}
	for i := 0; i < 2; i++ {
		fd.sessions[0], fd.sessions[1] = fd.sessions[1], fd.sessions[0]
		m.applySnapshot(m.src.Snapshot())
	}
	assertIdentityText(t, m.cards[0], "Model identity served-id", "Actual provider response-vendor", "Model version snapshot-v2")
	// A later actual response with unreported provider/version does not inherit
	// fields from another model, nor from the configured request.
	m.applyTailBatch([]TailEvent{{SessionID: "one", Event: agent.LlmCallEvent{ResponseModel: "fallback-id", System: "configured-vendor", Model: "requested-alias"}}})
	assertIdentityText(t, m.cards[0], "Model identity fallback-id", "Actual provider unknown", "Model version unknown")
}

// assertIdentityText checks "<Label> <value>" pairs against the session
// detail view: the structured field must hold exactly that value, and the
// rendered detail (plain and styled) must show it on the label's row.
func assertIdentityText(t *testing.T, c SessionCard, wants ...string) {
	t.Helper()
	now := time.Now()
	fields := detailFields(c, now)
	for _, want := range wants {
		label := ""
		for _, f := range fields {
			if strings.HasPrefix(strings.ToLower(want), strings.ToLower(f.label)+" ") && len(f.label) > len(label) {
				label = f.label
			}
		}
		if label == "" {
			t.Errorf("no detail field for %q", want)
			continue
		}
		value := want[len(label)+1:]
		if got := detailValue(fields, label); got != value {
			t.Errorf("detail %s = %q, want %q", label, got, value)
		}
		for _, plain := range []bool{true, false} {
			rows := renderDetail(theme.DefaultTheme(), c, now, 80, 0, plain) // one field per row
			found := false
			for _, row := range rows {
				row = ansi.Strip(row)
				if strings.HasPrefix(row, label+" ") && strings.TrimSpace(row[len(label):]) == value {
					found = true
				}
			}
			if !found {
				t.Errorf("plain=%v rendered detail lacks %s %q:\n%s", plain, label, value, strings.Join(rows, "\n"))
			}
		}
	}
}

func detailValue(fields []detailField, label string) string {
	for _, f := range fields {
		if strings.EqualFold(f.label, label) {
			return f.value
		}
	}
	return ""
}

func TestIdentityRunReplacementRejectsStaleTailBatch(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, state.State{SessionID: "one", StartedAt: 1000})
	path := filepath.Join(dir, state.AgentDirName, "events.jsonl")
	writeEvents(t, path)
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "one", WorktreePath: dir, AcceptedAt: "first"}}}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	old := m.tailers["one"]
	m.applyTailBatch([]TailEvent{{SessionID: "one", source: old, Event: agent.LlmCallEvent{ResponseModel: "old-model"}}, {SessionID: "one", source: old, Event: agent.ToolUseEvent{ToolName: "Read"}}})
	// Controller re-adoption changes admission observation but does not change
	// the surviving runner's persisted start or reader identity.
	fd.sessions[0].AcceptedAt = "adopted"
	m.applySnapshot(m.src.Snapshot())
	if m.tailers["one"] != old || m.cards[0].ActualModel != "old-model" || m.cards[0].ToolCalls != 1 {
		t.Fatal("re-adoption discarded same-run identity/metrics")
	}
	writeEvents(t, path, agent.LlmCallEvent{ResponseModel: "stale-model"})
	stale := m.pollTails()().(tailBatchMsg)
	if len(stale.events) != 1 {
		t.Fatalf("literal tail command read %d events, want one", len(stale.events))
	}
	writeState(t, dir, state.State{SessionID: "one", StartedAt: 2000})
	m.applySnapshot(m.src.Snapshot())
	if m.tailers["one"] == old || m.cards[0].ActualModel != "" || m.cards[0].ToolCalls != 0 {
		t.Fatal("new observed run retained old identity/metrics")
	}
	// Read through the actual asynchronous poll command before replacing the
	// run. Deleting production source stamping must make this fixture fail.
	m.applyTailBatch(stale.events)
	if m.cards[0].ActualModel != "" {
		t.Fatal("stale asynchronous batch reached replacement run")
	}
	current := m.tailers["one"]
	m.applyTailBatch([]TailEvent{{SessionID: "one", source: current, Event: agent.LlmCallEvent{ResponseModel: "new-model"}}})
	if m.cards[0].ActualModel != "new-model" {
		t.Fatal("current reader did not update replacement run")
	}
	fd.sessions = nil
	m.applySnapshot(m.src.Snapshot())
	fd.sessions = []afclient.DaemonSessionHandle{{SessionID: "one", WorktreePath: dir}}
	m.applySnapshot(m.src.Snapshot())
	m.applyTailBatch([]TailEvent{{SessionID: "one", source: current, Event: agent.LlmCallEvent{ResponseModel: "removed-model"}}})
	if m.cards[0].ActualModel != "" {
		t.Fatal("removed reader reached reused session ID")
	}
}

func TestIdentityOldDaemonStateAndUnknown(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, state.State{AgentCardID: "local-card", AgentCardName: "Local"})
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "old", WorktreePath: dir}}}
	snap := NewSource(fd, state.NewStore(), "").Snapshot()
	assertIdentityText(t, snap.Cards[0], "Agent card Local", "Card ID local-card", "Model identity unknown", "Actual provider unknown", "Model version unknown")
	assertIdentityText(t, SessionCard{}, "Agent card unknown", "Card ID unknown", "Model identity unknown", "Actual provider unknown", "Model version unknown")
}

func TestIdentityDoesNotBackfillAnotherSessionState(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, state.State{SessionID: "previous-session", StartedAt: 1000, AgentCardID: "previous-card", AgentCardName: "Previous"})
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "current-session", WorktreePath: dir}}}
	snap := NewSource(fd, state.NewStore(), "").Snapshot()
	if snap.Cards[0].AgentCardID != "" || snap.Cards[0].AgentCardName != "" || snap.Cards[0].StartedAtUnixMs != 0 {
		t.Fatal("another session's state backfilled current identity")
	}
}

func TestIdentityMetadataStatusEventCodecAndTerminalUsage(t *testing.T) {
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "one"}}}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	ev, err := agent.UnmarshalEvent([]byte(`{"kind":"system","subtype":"model_identity","observedModel":{"model":"served-id","provider":"observed","version":"snapshot-v2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	cost := 1.25
	turns := 3
	m.applyTailBatch([]TailEvent{{SessionID: "one", Replay: true, Event: ev}, {SessionID: "one", Event: agent.LlmCallEvent{Synthetic: true, Model: "alias", System: "configured", InputTokens: 120, UsageSource: agent.LlmUsageAggregate}}, {SessionID: "one", Event: agent.ResultEvent{Success: true, Cost: &agent.CostData{TotalCostUsd: cost, NumTurns: turns}, ObservedCostUsd: &cost, ObservedTurns: &turns}}})
	assertIdentityText(t, m.cards[0], "Model identity served-id", "Actual provider observed", "Model version snapshot-v2", "cost $1.25", "turns 3")
}

func TestIdentityRenderStripsNativeTerminalControlsAndBudgetsRows(t *testing.T) {
	hostile := "東京" + "\x1b]52;c;clipboard-secret\x07" + "\x1b[2J" + "\n\r\t" + strings.Repeat("界", 100)
	card := SessionCard{AgentCardID: hostile, AgentCardName: hostile, ActualModel: hostile, ActualModelProvider: hostile, ActualModelVersion: hostile, IssueTitle: hostile, LastActivity: hostile}
	const width = 44
	for _, plain := range []bool{true, false} {
		rendered := map[string]string{
			"card":   renderCard(theme.DefaultTheme(), card, 0, true, plain, time.Now(), width),
			"detail": strings.Join(renderDetail(theme.DefaultTheme(), card, time.Now(), width, 0, plain), "\n"),
		}
		for name, out := range rendered {
			if strings.Contains(out, "clipboard-secret") || strings.Contains(out, "\x1b]52") || strings.Contains(out, "\x1b[2J") || strings.ContainsAny(out, "\r\t") {
				t.Fatalf("plain=%v %s: native terminal control escaped sanitation: %q", plain, name, out)
			}
			if !strings.Contains(out, "東京") {
				t.Fatalf("plain=%v %s: printable Unicode disappeared", plain, name)
			}
			for _, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Fatalf("plain=%v %s: physical row width %d exceeds %d: %q", plain, name, w, width, line)
				}
			}
		}
	}
}

func TestIdentityReplacementRunDoesNotReplayOldLog(t *testing.T) {
	dir := t.TempDir()
	zero := int64(0)
	writeState(t, dir, state.State{SessionID: "one", StartedAt: 1000, EventLogStartOffset: &zero})
	path := filepath.Join(dir, state.AgentDirName, "events.jsonl")
	writeEvents(t, path, agent.SystemEvent{Subtype: agent.SystemSubtypeModelIdentity, ObservedModel: &agent.ObservedModelIdentity{Model: "old-model"}})
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "one", WorktreePath: dir}}}
	m := newTestModel(t, fd, "")
	m.opts.Replay = true
	m.applySnapshot(m.src.Snapshot())
	m.applyTailBatch(m.pollTails()().(tailBatchMsg).events)
	if m.cards[0].ActualModel != "old-model" {
		t.Fatal("ordinary initial replay lost observed identity")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	boundary := info.Size()
	writeState(t, dir, state.State{SessionID: "one", StartedAt: 2000, EventLogStartOffset: &boundary})
	m.applySnapshot(m.src.Snapshot())
	m.applyTailBatch(m.pollTails()().(tailBatchMsg).events)
	if m.cards[0].ActualModel != "" {
		t.Fatal("replacement run replayed old run identity")
	}
	writeEvents(t, path, agent.SystemEvent{Subtype: agent.SystemSubtypeModelIdentity, ObservedModel: &agent.ObservedModelIdentity{Model: "new-model"}})
	m.applyTailBatch(m.pollTails()().(tailBatchMsg).events)
	if m.cards[0].ActualModel != "new-model" {
		t.Fatal("replacement reader lost new live identity")
	}
}

func TestIdentityUnknownRunBoundaryDoesNotAttributeReplay(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, state.State{SessionID: "one", StartedAt: 1000})
	path := filepath.Join(dir, state.AgentDirName, "events.jsonl")
	writeEvents(t, path, agent.SystemEvent{Subtype: agent.SystemSubtypeModelIdentity, ObservedModel: &agent.ObservedModelIdentity{Model: "unproven-model"}})
	m := newTestModel(t, &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "one", WorktreePath: dir}}}, "")
	m.opts.Replay = true
	m.applySnapshot(m.src.Snapshot())
	m.applyTailBatch(m.pollTails()().(tailBatchMsg).events)
	if m.cards[0].ActualModel != "" {
		t.Fatal("unknown run boundary attributed historical identity")
	}
}
