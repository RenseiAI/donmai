package hostwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// writeEvents marshals events and appends them as JSONL lines to path.
func writeEvents(t *testing.T, path string, evs ...agent.Event) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, ev := range evs {
		body, err := agent.MarshalEvent(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(append(body, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func fixedClock() func() time.Time {
	base := time.Date(2026, 6, 13, 14, 0, 0, 0, time.UTC)
	return func() time.Time { return base }
}

func TestTailer_MissingFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	tl := NewTailer("s1", filepath.Join(dir, "events.jsonl"), false, fixedClock())
	got, err := tl.Poll()
	if err != nil {
		t.Fatalf("Poll on missing file: unexpected err %v", err)
	}
	if got != nil {
		t.Fatalf("Poll on missing file: want nil, got %v", got)
	}
}

func TestTailer_AppendFromTop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeEvents(t, path,
		agent.InitEvent{SessionID: "prov-1"},
		agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "go test"}},
	)
	tl := NewTailer("s1", path, false /*startAtEnd=false → read history*/, fixedClock())

	got, err := tl.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	if _, ok := got[0].Event.(agent.InitEvent); !ok {
		t.Errorf("event 0: want InitEvent, got %T", got[0].Event)
	}
	if tu, ok := got[1].Event.(agent.ToolUseEvent); !ok || tu.ToolName != "Bash" {
		t.Errorf("event 1: want Bash ToolUseEvent, got %#v", got[1].Event)
	}
	if got[0].SessionID != "s1" {
		t.Errorf("attribution: want s1, got %q", got[0].SessionID)
	}

	// A second poll with no new bytes returns nothing.
	again, err := tl.Poll()
	if err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second Poll: want 0, got %d", len(again))
	}
}

func TestTailer_StartAtEndSkipsHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeEvents(t, path, agent.InitEvent{SessionID: "prov-1"})

	tl := NewTailer("s1", path, true /*startAtEnd*/, fixedClock())

	// First poll resolves the seek-to-end; no historical events emitted.
	got, err := tl.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("startAtEnd first poll: want 0, got %d", len(got))
	}

	// Now append; the next poll should see only the new event.
	writeEvents(t, path, agent.AssistantTextEvent{Text: "hello"})
	got, err = tl.Poll()
	if err != nil {
		t.Fatalf("Poll after append: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 new event, got %d", len(got))
	}
	if at, ok := got[0].Event.(agent.AssistantTextEvent); !ok || at.Text != "hello" {
		t.Errorf("want AssistantTextEvent hello, got %#v", got[0].Event)
	}
}

func TestTailer_IncrementalAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeEvents(t, path, agent.InitEvent{SessionID: "p"})
	tl := NewTailer("s1", path, false, fixedClock())

	if got, _ := tl.Poll(); len(got) != 1 {
		t.Fatalf("first poll: want 1, got %d", len(got))
	}
	writeEvents(t, path, agent.ToolUseEvent{ToolName: "Read", Input: map[string]any{"file_path": "x.go"}})
	writeEvents(t, path, agent.ToolResultEvent{ToolName: "Read", Content: "ok"})
	got, err := tl.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("incremental poll: want 2, got %d", len(got))
	}
}

func TestTailer_TruncationReseeks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeEvents(t, path,
		agent.InitEvent{SessionID: "p"},
		agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "a"}},
	)
	tl := NewTailer("s1", path, false, fixedClock())
	if got, _ := tl.Poll(); len(got) != 2 {
		t.Fatalf("pre-truncate poll: want 2, got %d", len(got))
	}

	// Simulate copy-truncate logrotate / a fresh run reusing the path: the
	// file shrinks below the consumed offset.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	writeEvents(t, path, agent.SystemEvent{Subtype: "rotated"})

	got, err := tl.Poll()
	if err != nil {
		t.Fatalf("post-truncate poll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("post-truncate poll: want 1 (re-read from top), got %d", len(got))
	}
	if se, ok := got[0].Event.(agent.SystemEvent); !ok || se.Subtype != "rotated" {
		t.Errorf("want SystemEvent rotated, got %#v", got[0].Event)
	}
}

func TestTailer_TerminalEventMarksDone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeEvents(t, path,
		agent.InitEvent{SessionID: "p"},
		agent.ResultEvent{Success: true, Cost: &agent.CostData{TotalCostUsd: 1.5, NumTurns: 4}},
		// A line after the terminal event must be ignored once Done.
		agent.AssistantTextEvent{Text: "late"},
	)
	tl := NewTailer("s1", path, false, fixedClock())
	got, err := tl.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Should stop at the ResultEvent (init + result = 2), not consume "late".
	if len(got) != 2 {
		t.Fatalf("want 2 events up to terminal, got %d", len(got))
	}
	if !tl.Done() {
		t.Fatal("tailer should be Done after terminal ResultEvent")
	}
	// Subsequent polls are no-ops.
	again, err := tl.Poll()
	if err != nil || len(again) != 0 {
		t.Fatalf("post-done poll: want (0,nil), got (%d,%v)", len(again), err)
	}
}

func TestTailer_PartialLineCarried(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	// Write a complete event, then a half-line (writer mid-append, no
	// trailing newline yet).
	writeEvents(t, path, agent.InitEvent{SessionID: "p"})
	half := []byte(`{"kind":"assistant_text","text":"partial`)
	if err := os.WriteFile(path, appendBytes(readFile(t, path), half), 0o600); err != nil {
		t.Fatalf("write half: %v", err)
	}
	tl := NewTailer("s1", path, false, fixedClock())
	got, err := tl.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Only the complete InitEvent decodes; the half-line is carried.
	if len(got) != 1 {
		t.Fatalf("want 1 (complete) event, got %d", len(got))
	}
	// Now complete the half-line.
	rest := []byte(`"}` + "\n")
	if err := os.WriteFile(path, appendBytes(readFile(t, path), rest), 0o600); err != nil {
		t.Fatalf("complete line: %v", err)
	}
	got, err = tl.Poll()
	if err != nil {
		t.Fatalf("poll after completion: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 completed event, got %d", len(got))
	}
	if at, ok := got[0].Event.(agent.AssistantTextEvent); !ok || at.Text != "partial" {
		t.Errorf("want AssistantTextEvent partial, got %#v", got[0].Event)
	}
}

func TestTailer_MalformedLineSurfacesError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	tl := NewTailer("s1", path, false, fixedClock())
	got, err := tl.Poll()
	if err != nil {
		t.Fatalf("Poll should not fail on a bad line: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 surfaced event, got %d", len(got))
	}
	if got[0].Err == nil {
		t.Fatal("malformed line should set TailEvent.Err")
	}
	if got[0].Event != nil {
		t.Errorf("malformed line should have nil Event, got %#v", got[0].Event)
	}
}

func TestTailer_BlankLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte("\n\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	tl := NewTailer("s1", path, false, fixedClock())
	got, err := tl.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("blank lines should yield 0 events, got %d", len(got))
	}
}

// ── small file helpers (no extra deps) ──────────────────────────────────────

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func appendBytes(a, b []byte) []byte {
	out := make([]byte, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}
