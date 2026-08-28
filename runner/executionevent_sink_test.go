package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/executionevent"
)

type recordingActivitySink struct{ count int }

func (s *recordingActivitySink) Send(context.Context, agent.Event) { s.count++ }

func TestExecutionEventSinkPreservesActivityAndPostsNormalizedEvent(t *testing.T) {
	posted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posted <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	uploader, err := executionevent.New(executionevent.Config{SessionID: "session_1", BaseURL: server.URL, JournalDir: t.TempDir(), AuthToken: "test", MaxRetries: 1, InitialBackoff: time.Nanosecond, Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = uploader.Journal().Close() }()
	primary := &recordingActivitySink{}
	sink := newExecutionEventSink(primary, uploader, nil)
	t.Cleanup(func() { sink.Close(&Result{Result: agent.Result{Status: "completed"}}) })
	sink.Send(context.Background(), agent.ToolUseEvent{ToolName: "Read"})
	select {
	case <-posted:
	case <-time.After(time.Second):
		t.Fatal("normalized event was not posted")
	}
	if primary.count != 1 {
		t.Fatalf("activity events = %d, want 1", primary.count)
	}
	deadline := time.Now().Add(time.Second)
	for len(uploader.Journal().Pending()) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pending := uploader.Journal().Pending(); len(pending) != 0 {
		t.Fatalf("pending normalized events = %d", len(pending))
	}
}

func TestExecutionEventSinkReturnsAfterJournalAppendWhenRemoteIsSlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	uploader, err := executionevent.New(executionevent.Config{SessionID: "session_2", BaseURL: server.URL, JournalDir: t.TempDir(), MaxRetries: 10})
	if err != nil {
		t.Fatal(err)
	}
	sink := newExecutionEventSink(nil, uploader, nil)
	started := time.Now()
	sink.Send(context.Background(), agent.ToolUseEvent{ToolName: "Read"})
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Send waited on remote delivery for %s", elapsed)
	}
	sink.Close(&Result{Result: agent.Result{Status: "completed"}})
}

func TestExecutionEventOutcomePreservesFailureTruth(t *testing.T) {
	cases := []struct {
		status, failure, outcome, evidence string
	}{
		{"completed", "", "succeeded", "graceful"},
		{"stopped", "", "terminated", "inferred"},
		{"stopped", FailureOperatorCancelled, "cancelled", "graceful"},
		{"failed", FailureOperatorCancelled, "cancelled", "graceful"},
		{"failed", FailureAgentBlocked, "interrupted", "inferred"},
		{"failed", FailureTimeout, "expired", "forced"},
		{"failed", FailureLostOwnership, "lost", "inferred"},
		{"failed", FailureSilentExit, "lost", "inferred"},
		{"failed", FailureProviderError, "failed", "inferred"},
	}
	for _, tc := range cases {
		got, evidence := executionEventOutcome(&Result{Result: agent.Result{Status: tc.status, FailureMode: tc.failure}})
		if got != tc.outcome || evidence != tc.evidence {
			t.Errorf("outcome(%q,%q) = %q/%q, want %q/%q", tc.status, tc.failure, got, evidence, tc.outcome, tc.evidence)
		}
		if _, err := executionevent.NewSessionEndedRecordWithEvidence("session_outcome", 1, time.Now(), got, evidence, executionevent.DigestResult(tc.status, "summary", tc.failure)); err != nil {
			t.Errorf("outcome(%q,%q) cannot construct platform record: %v", tc.status, tc.failure, err)
		}
	}
}

func TestExecutionEventCapabilityOffDoesNotConstructOrSend(t *testing.T) {
	called := false
	qw := QueuedWork{}
	sink, err := newExecutionEventSinkForWork(qw, nil, nil, func() (*executionevent.Uploader, error) {
		called = true
		return nil, nil
	})
	if err != nil || sink != nil || called {
		t.Fatalf("capability-off factory: sink=%v err=%v called=%v", sink, err, called)
	}
}

func TestExecutionEventSinkAppendsBlockedAndCompletePullRequestBeforeSoleTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	uploader, err := executionevent.New(executionevent.Config{
		SessionID: "session_terminal", BaseURL: server.URL, JournalDir: t.TempDir(), AuthToken: "test",
		MaxRetries: 1, InitialBackoff: time.Nanosecond, Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = uploader.Journal().Close() })
	stubGhOnPath(t, 0, `{"number":88,"url":"https://github.com/RenseiAI/donmai/pull/88","baseRefName":"main","headRefName":"agent/test-fact","repository":{"nameWithOwner":"RenseiAI/donmai"}}`)
	sink := newExecutionEventSink(nil, uploader, nil)
	sink.Close(&Result{Result: agent.Result{
		Status: "failed", FailureMode: FailureAgentBlocked,
		WorktreePath: t.TempDir(), PullRequestURL: "https://github.com/RenseiAI/donmai/pull/88",
	}})
	records := uploader.Journal().Records
	if len(records) != 3 {
		t.Fatalf("journal records = %d, want 3: %+v", len(records), records)
	}
	for index, wantType := range []string{"session.blocked", "pr.opened", "session.ended"} {
		if records[index].StructuredSeq != uint64(index+1) || records[index].EventType != wantType {
			t.Fatalf("record[%d] = seq %d %q, want seq %d %q", index, records[index].StructuredSeq, records[index].EventType, index+1, wantType)
		}
	}
	if records[0].Payload["reason"] != "agent declined to proceed" {
		t.Fatalf("blocked payload = %#v", records[0].Payload)
	}
	for key, want := range map[string]any{
		"provider": "github", "number": 88, "repository": "RenseiAI/donmai",
		"url": "https://github.com/RenseiAI/donmai/pull/88", "baseBranch": "main", "headBranch": "agent/test-fact",
	} {
		if got := records[1].Payload[key]; got != want {
			t.Fatalf("pr payload[%q] = %#v, want %#v", key, got, want)
		}
	}
	if records[2].Payload["outcome"] != "interrupted" {
		t.Fatalf("terminal payload = %#v", records[2].Payload)
	}
}

func TestExecutionEventSinkRejectsAgentPRURLWhenGitHubReadbackFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	uploader, err := executionevent.New(executionevent.Config{SessionID: "session_incomplete_pr", BaseURL: server.URL, JournalDir: t.TempDir(), AuthToken: "test", MaxRetries: 1, InitialBackoff: time.Nanosecond, Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = uploader.Journal().Close() })
	stubGhOnPath(t, 1, `{"number":2,"url":"https://github.com/unrelated/private/pull/2","baseRefName":"main","headRefName":"fabricated","repository":{"nameWithOwner":"unrelated/private"}}`)
	sink := newExecutionEventSink(nil, uploader, nil)
	sink.Close(&Result{Result: agent.Result{
		Status: "completed", WorktreePath: t.TempDir(),
		PullRequestURL: "https://github.com/unrelated/private/pull/2",
	}})
	records := uploader.Journal().Records
	if len(records) != 1 || records[0].EventType != "session.ended" {
		t.Fatalf("unverified PR URL emitted records = %+v, want sole session.ended", records)
	}
}
