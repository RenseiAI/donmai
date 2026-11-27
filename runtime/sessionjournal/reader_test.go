package sessionjournal_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/sessionjournal"
)

func TestReaderReusedWorktree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	const previous = "{\"kind\":\"system\",\"subtype\":\"previous\"}\n"
	const current = "{\"kind\":\"result\",\"success\":true}\n"
	const partial = "{\"kind\":\"assistant_text\",\"text\":"
	const live = "{\"kind\":\"system\",\"subtype\":\"live\"}\n"
	const malformed = "invalid\n"
	if err := os.WriteFile(path, []byte(previous+current+malformed+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	start := int64(len(previous))
	r := sessionjournal.NewReader("current", path, &start)
	history, err := r.Poll()
	if err != nil || len(history) != 2 {
		t.Fatalf("want two complete current-run entries, got %d, err=%v", len(history), err)
	}
	if event, ok := history[0].Event.(agent.ResultEvent); !ok || !event.Success || history[0].Offset != start || !history[0].Replay || history[0].Err != nil || history[0].At.IsZero() || history[0].SessionID != "current" {
		t.Fatalf("wrong run or metadata: %+v", history[0])
	}
	diagnostic := history[1]
	if diagnostic.Err == nil || diagnostic.Event != nil || diagnostic.Offset != start+int64(len(current)) || !diagnostic.Replay {
		t.Fatalf("missing positioned parse diagnostic: %+v", diagnostic)
	}

	const finish = "\"tail\"}\n"
	appendJournal(t, path, finish+live)
	entries, err := r.Poll()
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected completed partial and live event, got %d, err=%v", len(entries), err)
	}
	pendingOffset := int64(len(previous + current + malformed))
	if text, ok := entries[0].Event.(agent.AssistantTextEvent); !ok || text.Text != "tail" || !entries[0].Replay || entries[0].Offset != pendingOffset {
		t.Fatalf("partial append lost its position or replay status: %+v", entries[0])
	}
	if e, ok := entries[1].Event.(agent.SystemEvent); !ok || e.Subtype != "live" || entries[1].Replay || entries[1].Offset != pendingOffset+int64(len(partial+finish)) {
		t.Fatalf("wrong live append: %+v", entries[1])
	}
}

func TestReaderTruncationBeforeReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	const old = "{\"kind\":\"system\",\"subtype\":\"previous\"}\n"
	const replacement = "{\"kind\":\"system\",\"subtype\":\"next\"}\n"
	if err := os.WriteFile(path, []byte(strings.Repeat(old, 3)), 0o600); err != nil {
		t.Fatal(err)
	}
	start := int64(0)
	r := sessionjournal.NewReader("current", path, &start)
	if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if stale, err := r.Poll(); err != nil || len(stale) != 0 {
			t.Fatalf("previous run read replacement before receiving a new fence: %d entries, err=%v", len(stale), err)
		}
		appendJournal(t, path, replacement)
	}
	next := sessionjournal.NewReader("next", path, &start)
	if entries, err := next.Poll(); err != nil || len(entries) != 3 || !entries[0].Replay {
		t.Fatalf("new reader could not replay replacement run: %d entries, err=%v", len(entries), err)
	}
}

func appendJournal(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}
