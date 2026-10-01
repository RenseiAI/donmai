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

func assertIdentityText(t *testing.T, c SessionCard, wants ...string) {
	t.Helper()
	for _, plain := range []bool{true, false} {
		text := renderCard(theme.DefaultTheme(), c, 0, true, plain, time.Now())
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("plain=%v missing %q:\n%s", plain, want, text)
			}
		}
	}
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
	card := SessionCard{AgentCardID: hostile, AgentCardName: hostile, ActualModel: hostile, ActualModelProvider: hostile, ActualModelVersion: hostile}
	for _, plain := range []bool{true, false} {
		out := renderCard(theme.DefaultTheme(), card, 0, true, plain, time.Now())
		if strings.Contains(out, "clipboard-secret") || strings.Contains(out, "\x1b]52") || strings.Contains(out, "\x1b[2J") || strings.ContainsAny(out, "\r\t") {
			t.Fatalf("plain=%v native terminal control escaped sanitation: %q", plain, out)
		}
		if !strings.Contains(out, "東京") {
			t.Fatalf("plain=%v printable Unicode disappeared", plain)
		}
		for _, line := range strings.Split(out, "\n") {
			if width := lipgloss.Width(line); width > cardWidth {
				t.Fatalf("plain=%v physical row width %d exceeds %d: %q", plain, width, cardWidth, line)
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
