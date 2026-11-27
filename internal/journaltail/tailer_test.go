package journaltail

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
