package agycli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// tailPlannerLine is a PLANNER_RESPONSE transcript line carrying one tool
// call; tailResultLine is the matching tool-result step.
const (
	tailPlannerLine = `{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"viewing","tool_calls":[{"name":"view_file","args":{"AbsolutePath":"\"/tmp/notes\""}}]}`
	tailResultLine  = `{"step_index":3,"source":"MODEL","type":"VIEW_FILE","status":"DONE","content":"notes body"}`
)

// newPathTailer builds a tailer pinned to a transcript path (discovery
// skipped) that records emitted events into the returned slice pointer.
func newPathTailer(path string) (*transcriptTailer, *[]agent.Event) {
	var evs []agent.Event
	t := &transcriptTailer{path: path, emit: func(ev agent.Event) { evs = append(evs, ev) }}
	return t, &evs
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestTailer_IncrementalAppend verifies the core live-streaming behavior:
// lines appended between drains are emitted as they appear, and the
// use↔result FIFO pairing survives the incremental reads.
func TestTailer_IncrementalAppend(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	tailer, evs := newPathTailer(path)

	appendFile(t, path, tailPlannerLine+"\n")
	tailer.drain(false)
	if len(*evs) != 1 {
		t.Fatalf("after first drain: want 1 event, got %#v", *evs)
	}
	use, ok := (*evs)[0].(agent.ToolUseEvent)
	if !ok || use.ToolName != "view_file" {
		t.Fatalf("want ToolUseEvent(view_file), got %#v", (*evs)[0])
	}

	appendFile(t, path, tailResultLine+"\n")
	tailer.drain(false)
	if len(*evs) != 2 {
		t.Fatalf("after second drain: want 2 events, got %#v", *evs)
	}
	res, ok := (*evs)[1].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("want ToolResultEvent, got %#v", (*evs)[1])
	}
	if res.ToolUseID != use.ToolUseID {
		t.Errorf("pairing across incremental drains: result id %q != use id %q", res.ToolUseID, use.ToolUseID)
	}

	// Idle drain: nothing new, nothing re-emitted.
	tailer.drain(false)
	if len(*evs) != 2 {
		t.Errorf("idle drain re-emitted: %#v", *evs)
	}
}

// TestTailer_PartialLineCarry verifies a line written in two chunks (no
// newline yet) is held back until complete, never emitted twice.
func TestTailer_PartialLineCarry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	tailer, evs := newPathTailer(path)

	half := len(tailPlannerLine) / 2
	appendFile(t, path, tailPlannerLine[:half])
	tailer.drain(false)
	if len(*evs) != 0 {
		t.Fatalf("partial line must not emit: %#v", *evs)
	}
	appendFile(t, path, tailPlannerLine[half:]+"\n")
	tailer.drain(false)
	if len(*evs) != 1 {
		t.Fatalf("completed line should emit exactly once, got %#v", *evs)
	}
}

// TestTailer_FinalDrainFlushesUnterminatedLine matches the legacy
// whole-file scanner behavior: the last line may lack a trailing newline
// and must still be parsed on the post-exit catch-up drain.
func TestTailer_FinalDrainFlushesUnterminatedLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	tailer, evs := newPathTailer(path)

	appendFile(t, path, tailPlannerLine) // no trailing newline
	tailer.drain(false)
	if len(*evs) != 0 {
		t.Fatalf("unterminated line must carry on a non-final drain: %#v", *evs)
	}
	tailer.drain(true)
	if len(*evs) != 1 {
		t.Fatalf("final drain must flush the unterminated line, got %#v", *evs)
	}
}

// TestTailer_TruncateRecoverySkipsConsumedLines covers the file-rewrite
// edge: when the transcript shrinks, the tailer re-reads from the start and
// skips the lines it already consumed so nothing is double-emitted.
func TestTailer_TruncateRecoverySkipsConsumedLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	tailer, evs := newPathTailer(path)

	appendFile(t, path, tailPlannerLine+"\n"+tailResultLine+"\n")
	tailer.drain(false)
	if len(*evs) != 2 {
		t.Fatalf("setup: want 2 events, got %#v", *evs)
	}

	// Rewrite the file SHORTER than the consumed offset (stable first line,
	// new second line) — simulates agy replacing the transcript.
	short := tailPlannerLine + "\n" + `{"step_index":4,"source":"MODEL","type":"RUN_COMMAND","status":"DONE","content":"ok"}` + "\n"
	if int64(len(short)) >= tailer.offset {
		t.Fatalf("fixture must shrink the file: %d >= %d", len(short), tailer.offset)
	}
	if err := os.WriteFile(path, []byte(short), 0o600); err != nil { //nolint:gosec // test fixture path
		t.Fatal(err)
	}
	tailer.drain(false)
	if len(*evs) != 3 {
		t.Fatalf("truncate recovery: want exactly 1 new event (3 total), got %#v", *evs)
	}
	res, ok := (*evs)[2].(agent.ToolResultEvent)
	if !ok || res.ToolName != "RUN_COMMAND" {
		t.Errorf("want the post-rewrite RUN_COMMAND result, got %#v", (*evs)[2])
	}
}

// TestTailer_MissingFileIsSilent ensures a not-yet-written transcript is a
// no-op (best-effort enrichment never errors).
func TestTailer_MissingFileIsSilent(t *testing.T) {
	t.Parallel()
	tailer, evs := newPathTailer(filepath.Join(t.TempDir(), "nope.jsonl"))
	tailer.drain(false)
	tailer.drain(true)
	if len(*evs) != 0 {
		t.Errorf("missing file should emit nothing, got %#v", *evs)
	}
}
