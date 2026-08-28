package executionevent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func testRecord(t *testing.T, session string, seq uint64) Record {
	t.Helper()
	r, err := NewRecord(session, seq, time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), "tool.called", map[string]any{"toolName": "Read"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestJournalAckResumeAndStrictFiles(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, "session_1")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append("session_1", testRecord(t, "session_1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := j.Append("session_1", testRecord(t, "session_1", 2)); err != nil {
		t.Fatal(err)
	}
	if err := j.Ack(1); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = OpenJournal(dir, "session_1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	if got := j.Pending(); len(got) != 1 || got[0].StructuredSeq != 2 {
		t.Fatalf("pending after resume = %+v", got)
	}
	for _, name := range []string{journalFileName, ackFileName} {
		mode, statErr := os.Stat(filepath.Join(dir, name))
		if statErr != nil {
			t.Fatal(statErr)
		}
		if mode.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, mode.Mode().Perm())
		}
	}
}

func TestJournalUsesCrashReleasingExclusiveLock(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenJournal(dir, "session_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(dir, "session_1"); err == nil {
		t.Fatal("expected second journal open to fail while first is live")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenJournal(dir, "session_1")
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUploaderRetriesNetworkAndQuarantinesExactPermanentStatuses(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer fresh" {
			t.Errorf("request metadata: method=%s content-type=%q auth=%q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	u, err := New(Config{SessionID: "session_1", BaseURL: server.URL, JournalDir: t.TempDir(), AuthToken: "old", MaxRetries: 2, InitialBackoff: time.Nanosecond, Sleep: func(time.Duration) {}, CredentialProvider: func(context.Context) (RuntimeCredentials, error) { return RuntimeCredentials{AuthToken: "fresh"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.journal.Close() }()
	if err := u.SendRecord(testRecord(t, "session_1", 1)); err != nil {
		t.Fatal(err)
	}
	result, err := u.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Delivered != 1 || attempts.Load() != 2 {
		t.Fatalf("result=%+v attempts=%d", result, attempts.Load())
	}

	permanent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) }))
	defer permanent.Close()
	u2, err := New(Config{SessionID: "session_2", BaseURL: permanent.URL, JournalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := u2.SendRecord(testRecord(t, "session_2", 1)); err != nil {
		t.Fatal(err)
	}
	result, err = u2.Flush(context.Background())
	if err != nil || result.Quarantined != 1 || len(u2.journal.Pending()) != 0 {
		t.Fatalf("permanent result=%+v err=%v pending=%v", result, err, u2.journal.Pending())
	}
	_ = u2.journal.Close()
}

func TestUploaderLeavesForbiddenScopePending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	u, err := New(Config{SessionID: "session_1", BaseURL: server.URL, JournalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.journal.Close() }()
	if err := u.SendRecord(testRecord(t, "session_1", 1)); err != nil {
		t.Fatal(err)
	}
	_, err = u.Flush(context.Background())
	if err == nil || len(u.journal.Pending()) != 1 {
		t.Fatalf("403 err=%v pending=%d", err, len(u.journal.Pending()))
	}
}

func TestUploaderStopsWithinContextAndResumes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()
	u, err := New(Config{SessionID: "session_1", BaseURL: server.URL, JournalDir: t.TempDir(), MaxRetries: 10, StopDrainTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.SendRecord(testRecord(t, "session_1", 1)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = u.Stop()
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("Stop err=%v elapsed=%s", err, time.Since(started))
	}
}

func TestNormalizeEventNeverForwardsRawInputOrOutput(t *testing.T) {
	r, emitted, err := NormalizeEvent("session_1", 1, time.Now(), agent.ToolUseEvent{ToolName: "Run", Input: map[string]any{"token": "secret"}, Raw: map[string]any{"secret": "no"}})
	if err != nil || !emitted {
		t.Fatalf("normalize emitted=%v err=%v", emitted, err)
	}
	b, _ := MarshalCompact(r)
	if string(b) == "" || bytes.Contains(b, []byte("secret")) || bytes.Contains(b, []byte(`"no"`)) {
		t.Fatalf("raw data leaked: %s", b)
	}
	if _, emitted, err := NormalizeEvent("session_1", 2, time.Now(), agent.AssistantTextEvent{Text: "prompt secret"}); err != nil || emitted {
		t.Fatalf("assistant text emitted=%v err=%v", emitted, err)
	}
}

func TestSendEventSequencesOnlyEmittedTopics(t *testing.T) {
	u, err := New(Config{SessionID: "session_1", BaseURL: "http://127.0.0.1:1", JournalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.journal.Close() }()
	if emitted, err := u.SendEvent(agent.AssistantTextEvent{Text: "not an active topic"}); err != nil || emitted {
		t.Fatalf("unsupported event emitted=%v err=%v", emitted, err)
	}
	if emitted, err := u.SendEvent(agent.ToolUseEvent{ToolName: "Read"}); err != nil || !emitted {
		t.Fatalf("tool event emitted=%v err=%v", emitted, err)
	}
	if pending := u.journal.Pending(); len(pending) != 1 || pending[0].StructuredSeq != 1 {
		t.Fatalf("pending=%+v", pending)
	}
}

func TestBatchAllowsResumeAtArbitrarySequence(t *testing.T) {
	r := testRecord(t, "session_1", 42)
	if err := ValidateBatch(Batch{Version: BatchVersion, SessionID: "session_1", Records: []Record{r}}); err != nil {
		t.Fatal(err)
	}
	var unknown map[string]any
	b, _ := MarshalCompact(Batch{Version: BatchVersion, SessionID: "session_1", Records: []Record{r}})
	if err := json.Unmarshal(b, &unknown); err != nil || unknown["version"] != BatchVersion {
		t.Fatal("compact batch did not round-trip")
	}
}
