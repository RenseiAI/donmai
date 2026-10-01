package hostwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestObservedUsageIsIndependentAndTerminalReplaces(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{}, "")
	m.cards = []SessionCard{{SessionID: "s", StartedAtUnixMs: 1}}
	zero := 0.0
	first := agent.LlmCallEvent{SpanID: "call-1", UsageSource: agent.LlmUsageProvider, TurnCompleted: true, ObservedCostUsd: &zero}
	m.foldMetrics(TailEvent{SessionID: "s", Event: first, Replay: true})
	m.foldMetrics(TailEvent{SessionID: "s", Event: first, Replay: true}) // same correlated call
	card := m.cards[0]
	if !card.CostReported || !card.TurnsReported || card.CostUsd != 0 || card.NumTurns != 1 || !card.LastWorkAt.IsZero() {
		t.Fatalf("reported zero, dedup or replay freshness lost: %+v", card)
	}
	one := 1.25
	m.foldMetrics(TailEvent{SessionID: "s", Event: agent.LlmCallEvent{SpanID: "call-2", UsageSource: agent.LlmUsageProvider, TurnCompleted: true, ObservedCostUsd: &one}})
	if m.cards[0].CostUsd != 1.25 || m.cards[0].NumTurns != 2 {
		t.Fatalf("live observed values did not advance: %+v", m.cards[0])
	}
	total := 1.10 // authoritative correction, not another increment
	turns := 2
	offset := int64(84)
	result := TailEvent{SessionID: "s", Offset: &offset, Event: agent.ResultEvent{Success: true, ObservedCostUsd: &total, ObservedTurns: &turns}}
	m.foldMetrics(result)
	m.foldMetrics(result)
	if m.cards[0].CostUsd != total || m.cards[0].NumTurns != turns {
		t.Fatalf("terminal aggregate double-counted: %+v", m.cards[0])
	}
	next := 0.25
	m.foldMetrics(TailEvent{SessionID: "s", Event: agent.LlmCallEvent{SpanID: "call-3", UsageSource: agent.LlmUsageProvider, TurnCompleted: true, ObservedCostUsd: &next}})
	m.foldMetrics(result) // replay of same journal offset cannot roll back a continuation
	if m.cards[0].CostUsd != 1.35 || m.cards[0].NumTurns != 3 {
		t.Fatalf("continuation or terminal dedup failed: %+v", m.cards[0])
	}
	m.foldMetrics(TailEvent{SessionID: "s", Event: agent.ResultEvent{Success: true}}) // EOF without aggregate
	if m.cards[0].CostUsd != 1.35 || m.cards[0].NumTurns != 3 {
		t.Fatalf("missing terminal observation erased known values: %+v", m.cards[0])
	}
}

func TestObservedUsageDoesNotInferUnavailableAxis(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{}, "")
	m.cards = []SessionCard{{SessionID: "s"}}
	m.foldMetrics(TailEvent{SessionID: "s", Event: agent.LlmCallEvent{
		SpanID: "turn", UsageSource: agent.LlmUsageProvider, TurnCompleted: true,
	}})
	if !m.cards[0].TurnsReported || m.cards[0].CostReported || m.cards[0].NumTurns != 1 {
		t.Fatalf("missing cost was inferred: %+v", m.cards[0])
	}
	cost := 0.5
	m.foldMetrics(TailEvent{SessionID: "s", Event: agent.LlmCallEvent{
		SpanID: "synthetic", UsageSource: agent.LlmUsageAggregate, Synthetic: true,
		TurnCompleted: true, ObservedCostUsd: &cost,
	}})
	if m.cards[0].CostReported || m.cards[0].NumTurns != 1 {
		t.Fatalf("synthetic fallback counted: %+v", m.cards[0])
	}
}

func TestObservedUsageSurvivesRefreshOnlyForSameRun(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{}, "")
	zero := int64(0)
	m.cards = []SessionCard{{SessionID: "s", StartedAtUnixMs: 10, WorktreePath: "/run/a", EventLogStartOffset: &zero, CostUsd: 1.25, NumTurns: 2, CostReported: true, TurnsReported: true}}
	same := SessionCard{SessionID: "s", StartedAtUnixMs: 10, WorktreePath: "/run/a", EventLogStartOffset: &zero}
	m.applySnapshot(Snapshot{Cards: []SessionCard{same}})
	m.applySnapshot(Snapshot{Cards: []SessionCard{same}})
	if !m.cards[0].CostReported || !m.cards[0].TurnsReported || m.cards[0].CostUsd != 1.25 || m.cards[0].NumTurns != 2 {
		t.Fatalf("two same-run index refreshes lost metrics: %+v", m.cards[0])
	}
	m.applySnapshot(Snapshot{Cards: []SessionCard{{SessionID: "s", StartedAtUnixMs: 11, WorktreePath: "/run/a"}}})
	if m.cards[0].CostReported || m.cards[0].TurnsReported || m.cards[0].CostUsd != 0 || m.cards[0].NumTurns != 0 {
		t.Fatalf("replacement run inherited old metrics: %+v", m.cards[0])
	}
	m.cards[0].CostReported = true
	m.applySnapshot(Snapshot{Cards: []SessionCard{{SessionID: "s", WorktreePath: "/run/a"}}})
	if m.cards[0].CostReported {
		t.Fatal("missing run start inherited old metrics")
	}
}

func TestMixedVersionRefreshRetainsLiveActivityWithoutCostFence(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{}, "")
	m.cards = []SessionCard{{
		SessionID: "s", WorktreePath: "", ToolCalls: 2, LastActivity: "tool ran", Observed: true,
		CostUsd: 1.25, NumTurns: 2, CostReported: true, TurnsReported: true,
	}}
	m.applySnapshot(Snapshot{Cards: []SessionCard{{SessionID: "s"}}})
	m.applySnapshot(Snapshot{Cards: []SessionCard{{SessionID: "s"}}})
	c := m.cards[0]
	if c.ToolCalls != 2 || c.LastActivity != "tool ran" || !c.Observed {
		t.Fatalf("legacy live observations lost on mixed-version refresh: %+v", c)
	}
	if c.CostReported || c.TurnsReported || c.CostUsd != 0 || c.NumTurns != 0 {
		t.Fatalf("unfenced cost/turns inherited from prior card: %+v", c)
	}
}

func TestMetricTailerRecoveryFenceAndBoundedReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	for range 300 {
		writeEvents(t, path, agent.SystemEvent{Subtype: "fixture"})
	}
	zero := int64(0)
	tailer := NewMetricTailer("s", path, &zero, time.Now)
	first, err := tailer.Poll()
	if err != nil || len(first) != 256 {
		t.Fatalf("bounded first replay: events=%d err=%v", len(first), err)
	}
	second, err := tailer.Poll()
	if err != nil || len(second) != 44 || !first[0].Replay || !second[43].Replay {
		t.Fatalf("bounded second replay: events=%d err=%v", len(second), err)
	}
	writeEvents(t, path, agent.ResultEvent{Success: true}, agent.SystemEvent{Subtype: "later"})
	live, err := tailer.Poll()
	if err != nil || len(live) != 2 || live[0].Replay || live[1].Replay || tailer.Done() {
		t.Fatalf("live continuation after result: events=%d err=%v done=%v", len(live), err, tailer.Done())
	}
	for _, invalid := range []*int64{nil, func() *int64 { v := int64(-1); return &v }(), func() *int64 { v := int64(1 << 30); return &v }()} {
		tailer := NewMetricTailer("s", path, invalid, time.Now)
		got, err := tailer.Poll()
		if err != nil || len(got) != 0 {
			t.Fatalf("invalid/unknown offset replayed old bytes: events=%d err=%v", len(got), err)
		}
	}
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	writeEvents(t, path, agent.SystemEvent{Subtype: "replacement"})
	stale, err := tailer.Poll()
	if err != nil || len(stale) != 0 {
		t.Fatalf("old run reader accepted replacement journal: events=%d err=%v", len(stale), err)
	}
	writeEvents(t, path, agent.SystemEvent{Subtype: "replacement-later"})
	stale, err = tailer.Poll()
	if err != nil || len(stale) != 0 {
		t.Fatalf("old run reader resumed before new run fence: events=%d err=%v", len(stale), err)
	}
}

func TestMetricTailerChecksOpenedFileAtStatOpenBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeEvents(t, path, agent.SystemEvent{Subtype: "old-run"})
	zero := int64(0)
	tailer := NewMetricTailer("s", path, &zero, time.Now)
	tailer.openFile = func(name string) (*os.File, error) {
		if err := os.Rename(name, filepath.Join(dir, "previous.jsonl")); err != nil {
			t.Fatal(err)
		}
		writeEvents(t, name, agent.SystemEvent{Subtype: "replacement-run"})
		return os.Open(name)
	}
	got, err := tailer.Poll()
	if err != nil || len(got) != 0 || !tailer.awaitRunFence {
		t.Fatalf("replacement bytes crossed old run fence: events=%d err=%v held=%v", len(got), err, tailer.awaitRunFence)
	}
	writeEvents(t, path, agent.SystemEvent{Subtype: "replacement-later"})
	got, err = tailer.Poll()
	if err != nil || len(got) != 0 {
		t.Fatalf("old tailer resumed before new run fence: events=%d err=%v", len(got), err)
	}
	next := NewMetricTailer("s", path, &zero, time.Now)
	got, err = next.Poll()
	if err != nil || len(got) != 2 || !got[0].Replay {
		t.Fatalf("new run could not recover its own journal: events=%d err=%v", len(got), err)
	}
}

func TestMetricTailerRejectsOpenedSizeChangeBeforeSeek(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeEvents(t, path, agent.SystemEvent{Subtype: "old-run"})
	zero := int64(0)
	tailer := NewMetricTailer("s", path, &zero, time.Now)
	tailer.openFile = func(name string) (*os.File, error) {
		file, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		if err := os.Truncate(name, 0); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
	got, err := tailer.Poll()
	if err != nil || len(got) != 0 || !tailer.awaitRunFence {
		t.Fatalf("changed opened size was trusted: events=%d err=%v held=%v", len(got), err, tailer.awaitRunFence)
	}
}

func TestMetricTailerAcceptsSameInodeAppendAtOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeEvents(t, path, agent.SystemEvent{Subtype: "before-attach"})
	zero := int64(0)
	tailer := NewMetricTailer("s", path, &zero, time.Now)
	tailer.openFile = func(name string) (*os.File, error) {
		file, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		// This append happens deterministically after Poll's path Stat and
		// before its opened-descriptor Stat, without replacing the inode.
		writeEvents(t, name, agent.SystemEvent{Subtype: "append-at-open"})
		return file, nil
	}
	got, err := tailer.Poll()
	if err != nil || len(got) != 2 || tailer.awaitRunFence {
		t.Fatalf("normal same-inode append stalled tailer: events=%d err=%v held=%v", len(got), err, tailer.awaitRunFence)
	}
	if !got[0].Replay || got[1].Replay {
		t.Fatalf("attach boundary changed across append: old=%v new=%v", got[0].Replay, got[1].Replay)
	}
	tailer.openFile = nil
	writeEvents(t, path, agent.SystemEvent{Subtype: "later-append"})
	later, err := tailer.Poll()
	if err != nil || len(later) != 1 || later[0].Replay {
		t.Fatalf("tailer failed to follow later append: events=%d err=%v", len(later), err)
	}
}

func TestObservedUsageWithoutUploadSpanUsesJournalPosition(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{}, "")
	m.cards = []SessionCard{{SessionID: "local", StartedAtUnixMs: 1}}
	amount := 0.5
	offset := int64(24)
	row := TailEvent{SessionID: "local", Replay: true, Offset: &offset, Event: agent.LlmCallEvent{UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &amount, TurnCompleted: true}}
	m.foldMetrics(row)
	m.foldMetrics(row)
	if !m.cards[0].CostReported || !m.cards[0].TurnsReported || m.cards[0].CostUsd != amount || m.cards[0].NumTurns != 1 {
		t.Fatalf("local usage without optional upload correlation lost or duplicated: %+v", m.cards[0])
	}
	if !m.cards[0].LastWorkAt.IsZero() {
		t.Fatal("replay fabricated freshness")
	}
}
