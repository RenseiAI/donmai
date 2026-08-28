package executionevent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/statehome"
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

func TestUploaderRefreshesCredentialsAfterUnauthorized(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed" {
			t.Errorf("authorization after refresh = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	u, err := New(Config{SessionID: "session_auth", BaseURL: server.URL, JournalDir: t.TempDir(), AuthToken: "expired", MaxRetries: 1, InitialBackoff: time.Nanosecond, Sleep: func(time.Duration) {}, CredentialProvider: func(context.Context) (RuntimeCredentials, error) {
		return RuntimeCredentials{AuthToken: "refreshed"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Journal().Close() }()
	if err := u.SendRecord(testRecord(t, "session_auth", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Flush(context.Background()); err != nil || attempts.Load() != 2 || len(u.Journal().Pending()) != 0 {
		t.Fatalf("refresh result err=%v attempts=%d pending=%d", err, attempts.Load(), len(u.Journal().Pending()))
	}
}

func TestUploaderLeavesPostCommitServerFailureForReplay(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	u, err := New(Config{SessionID: "session_replay", BaseURL: server.URL, JournalDir: t.TempDir(), MaxRetries: 1, InitialBackoff: time.Nanosecond, Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Journal().Close() }()
	if err := u.SendRecord(testRecord(t, "session_replay", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Flush(context.Background()); err == nil || len(u.Journal().Pending()) != 1 {
		t.Fatalf("first failure err=%v pending=%d", err, len(u.Journal().Pending()))
	}
	if _, err := u.Flush(context.Background()); err != nil || len(u.Journal().Pending()) != 0 {
		t.Fatalf("replay err=%v pending=%d", err, len(u.Journal().Pending()))
	}
}

func TestUploader413QuarantineSurvivesReopen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusRequestEntityTooLarge) }))
	defer server.Close()
	dir := t.TempDir()
	u, err := New(Config{SessionID: "session_413", BaseURL: server.URL, JournalDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.SendRecord(testRecord(t, "session_413", 1)); err != nil {
		t.Fatal(err)
	}
	result, err := u.Flush(context.Background())
	if err != nil || result.Quarantined != 1 {
		t.Fatalf("413 result=%+v err=%v", result, err)
	}
	if err := u.Journal().Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(dir, "session_413")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if len(reopened.Pending()) != 0 {
		t.Fatalf("quarantined record replayed: %v", reopened.Pending())
	}
	if info, err := os.Stat(filepath.Join(dir, quarantineFileName)); err != nil || info.Size() == 0 {
		t.Fatalf("quarantine evidence missing: info=%v err=%v", info, err)
	}
}

func TestQuarantineEvidenceWinsWhenAckWriteWasInterrupted(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, "session_crash")
	if err != nil {
		t.Fatal(err)
	}
	first := testRecord(t, "session_crash", 1)
	if err := j.Append("session_crash", first); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	entry, err := MarshalCompact(quarantineRecord{Record: first, Status: 413, Reason: "platform returned HTTP 413"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, quarantineFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(entry, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(dir, "session_crash")
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Pending()) != 0 {
		t.Fatalf("quarantine evidence did not advance effective ack: %v", reopened.Pending())
	}
	if err := reopened.Append("session_crash", testRecord(t, "session_crash", 2)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	pending := reopened.Pending()
	if len(pending) != 1 || pending[0].StructuredSeq != 2 {
		t.Fatalf("later pending records = %+v", pending)
	}
}

func TestDefaultJournalSurvivesWorktreeDeletion(t *testing.T) {
	statehome.ResetForTest()
	defer statehome.ResetForTest()
	base := t.TempDir()
	statehome.SetBaseHome(base)
	worktree := filepath.Join(base, "disposable-worktree")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	u, err := New(Config{SessionID: "session_home", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.SendRecord(testRecord(t, "session_home", 1)); err != nil {
		t.Fatal(err)
	}
	dir := u.Journal().Directory()
	if err := u.Journal().Close(); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(dir, worktree+string(os.PathSeparator)) {
		t.Fatalf("default journal nested under worktree: %s", dir)
	}
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Config{SessionID: "session_home", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Journal().Close() }()
	if len(reopened.Journal().Pending()) != 1 {
		t.Fatalf("pending after worktree deletion = %d", len(reopened.Journal().Pending()))
	}
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

func TestTerminalRecordMatchesPlatformSessionEndedShape(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	record, err := NewSessionEndedRecordWithEvidence("session_shape", 1, time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), "terminated", "inferred", digest)
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCompact(Batch{Version: BatchVersion, SessionID: "session_shape", Records: []Record{record}})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 3 || wire["version"] != BatchVersion || wire["sessionId"] != "session_shape" {
		t.Fatalf("batch shape = %s", body)
	}
	records, ok := wire["records"].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("records shape = %s", body)
	}
	item, ok := records[0].(map[string]any)
	if !ok || len(item) != 8 || item["eventType"] != "session.ended" || item["persistencePolicy"] != "durable" {
		t.Fatalf("record shape = %s", body)
	}
	payload, ok := item["payload"].(map[string]any)
	if !ok || payload["outcome"] != "terminated" || payload["terminalEvidence"] != "inferred" || payload["resultDigest"] != digest {
		t.Fatalf("terminal payload shape = %s", body)
	}
}
