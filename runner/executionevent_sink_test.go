package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/executionevent"
	"github.com/RenseiAI/donmai/runtime/workarea"
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
	root, worktree := writeDeclaredMutableWorkarea(t, "../retargeted/local-origin", []workarea.DeclaredRepositoryV1{{
		Source: workarea.RepositorySource{Repository: "https://github.com/RenseiAI/donmai.git", Ref: "main"},
		Role:   workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
	}})
	stubGhOnPath(t, 0, `{"number":88,"url":"https://github.com/RenseiAI/donmai/pull/88","baseRefName":"main","headRefName":"agent/test-fact"}`)
	sink := newExecutionEventSink(nil, uploader, nil)
	sink.Close(&Result{Result: agent.Result{
		Status: "failed", FailureMode: FailureAgentBlocked,
		WorktreePath: worktree, WorkareaRoot: root.String(), PullRequestURL: "https://github.com/RenseiAI/donmai/pull/88",
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

func TestExecutionEventSinkRejectsSuccessfulForeignReadbackForSelectedRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	uploader, err := executionevent.New(executionevent.Config{SessionID: "session_incomplete_pr", BaseURL: server.URL, JournalDir: t.TempDir(), AuthToken: "test", MaxRetries: 1, InitialBackoff: time.Nanosecond, Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = uploader.Journal().Close() })
	root, worktree := writeDeclaredMutableWorkarea(t, "../retargeted/local-origin", []workarea.DeclaredRepositoryV1{{
		Source: workarea.RepositorySource{Repository: "https://github.com/RenseiAI/donmai.git", Ref: "main"},
		Role:   workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
	}})
	stubGhOnPath(t, 0, `{"number":2,"url":"https://github.com/unrelated/private/pull/2","baseRefName":"main","headRefName":"fabricated"}`)
	sink := newExecutionEventSink(nil, uploader, nil)
	sink.Close(&Result{Result: agent.Result{
		Status: "completed", WorktreePath: worktree, WorkareaRoot: root.String(),
		PullRequestURL: "https://github.com/unrelated/private/pull/2",
	}})
	records := uploader.Journal().Records
	if len(records) != 1 || records[0].EventType != "session.ended" {
		t.Fatalf("unverified PR URL emitted records = %+v, want sole session.ended", records)
	}
}

func TestExecutionEventSinkAllowsSelectedRepositoryFactWithRetargetedOriginAndDeclarationAuthority(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	uploader, err := executionevent.New(executionevent.Config{SessionID: "session_retargeted_origin", BaseURL: server.URL, JournalDir: t.TempDir(), AuthToken: "test", MaxRetries: 1, InitialBackoff: time.Nanosecond, Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = uploader.Journal().Close() })
	root, worktree := writeDeclaredMutableWorkarea(t, "../retargeted/local-origin", []workarea.DeclaredRepositoryV1{{
		Source: workarea.RepositorySource{Repository: "https://github.com/RenseiAI/donmai.git", Ref: "main"},
		Role:   workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
	}})
	stubGhOnPath(t, 0, `{"number":88,"url":"https://github.com/RenseiAI/donmai/pull/88","baseRefName":"main","headRefName":"agent/test-fact"}`)
	sink := newExecutionEventSink(nil, uploader, nil)
	sink.Close(&Result{Result: agent.Result{
		Status: "completed", WorktreePath: worktree, WorkareaRoot: root.String(),
		PullRequestURL: "https://github.com/RenseiAI/donmai/pull/88",
	}})
	records := uploader.Journal().Records
	if len(records) != 2 || records[0].EventType != "pr.opened" || records[1].EventType != "session.ended" {
		t.Fatalf("retargeted origin emitted records = %+v, want pr.opened then session.ended", records)
	}
}

func TestExecutionEventSinkFailsClosedWithoutDeclarationAuthority(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	uploader, err := executionevent.New(executionevent.Config{SessionID: "session_no_declaration", BaseURL: server.URL, JournalDir: t.TempDir(), AuthToken: "test", MaxRetries: 1, InitialBackoff: time.Nanosecond, Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = uploader.Journal().Close() })
	worktree := t.TempDir()
	gitInitWithOrigin(t, worktree, "../retargeted/local-origin")
	stubGhOnPath(t, 0, `{"number":88,"url":"https://github.com/RenseiAI/donmai/pull/88","baseRefName":"main","headRefName":"agent/test-fact"}`)
	sink := newExecutionEventSink(nil, uploader, nil)
	sink.Close(&Result{Result: agent.Result{
		Status: "completed", WorktreePath: worktree,
		PullRequestURL: "https://github.com/RenseiAI/donmai/pull/88",
	}})
	records := uploader.Journal().Records
	if len(records) != 1 || records[0].EventType != "session.ended" {
		t.Fatalf("no declaration authority emitted records = %+v, want sole session.ended", records)
	}
}

func TestExecutionEventPullRequestFactsAllowDeclaredMutableRepositoryWithRetargetedOrigin(t *testing.T) {
	root := workarea.RootPath(t.TempDir())
	selectedPath := filepath.Join(root.String(), "primary")
	docsPath := filepath.Join(root.String(), "docs")
	gitInitWithOrigin(t, selectedPath, "https://github.com.invalid/RenseiAI/donmai.git")
	gitInitWithOrigin(t, docsPath, "../retargeted/docs-origin")
	declaration, err := (workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{
				Source: workarea.RepositorySource{Repository: "https://github.com/RenseiAI/donmai.git", Ref: "main"},
				Name:   "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryReadOnly,
			},
			{
				Source: workarea.RepositorySource{Repository: "https://github.com/RenseiAI/docs.git", Ref: "main"},
				Name:   "docs", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable,
			},
		},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if err := workarea.WriteDeclaration(context.Background(), root, workarea.NewDeclarationRecord("session_declared_pr", "wa_declared_pr", declaration, map[string]string{
		"primary": "abc123",
		"docs":    "def456",
	})); err != nil {
		t.Fatal(err)
	}
	stubGhOnPath(t, 0, `{"number":9,"url":"https://github.com/RenseiAI/docs/pull/9","baseRefName":"main","headRefName":"agent/docs-pr"}`)
	r := minimalRunner(t)
	res := &Result{Result: agent.Result{
		Status:       "completed",
		WorktreePath: selectedPath,
		WorkareaRoot: root.String(),
		Manifest: &agent.TurnManifest{Repositories: &[]agent.TurnManifestRepository{{
			Name: "docs", PullRequestURL: "https://github.com/RenseiAI/docs/pull/9",
		}}},
	}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-DOCS-9")}
	qw.WorkType = WorkTypeQAStr
	report := r.runDeclaredBackstops(context.Background(), qw, "feature/docs", res, declaration, map[string]string{
		"primary": selectedPath,
		"docs":    docsPath,
	})
	facts := executionEventPullRequestFacts(&Result{Result: agent.Result{
		Status: "completed", WorktreePath: selectedPath, WorkareaRoot: root.String(), BackstopReport: &report,
	}})
	if len(facts) != 1 || facts[0].Repository != "RenseiAI/docs" {
		t.Fatalf("executionEventPullRequestFacts = %+v, want docs fact", facts)
	}
}

func writeDeclaredMutableWorkarea(t *testing.T, origin string, repositories []workarea.DeclaredRepositoryV1) (workarea.RootPath, string) {
	t.Helper()
	root := workarea.RootPath(t.TempDir())
	declaration, err := (workarea.RepositoryDeclarationV1{
		Protocol:     workarea.ProtocolSessionRootV1,
		Repositories: repositories,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	selectedPath := filepath.Join(root.String(), declaration.Selected.Leaf)
	gitInitWithOrigin(t, selectedPath, origin)
	if err := workarea.WriteDeclaration(context.Background(), root, workarea.NewDeclarationRecord("session_declared_selected", "wa_declared_selected", declaration, map[string]string{
		declaration.Selected.Name: "abc123",
	})); err != nil {
		t.Fatal(err)
	}
	return root, selectedPath
}

func gitInitWithOrigin(t *testing.T, dir, origin string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitInit(t, dir)
	//nolint:gosec // G204: test fixture, origin comes from the test case.
	cmd := exec.Command("git", "remote", "add", "origin", origin)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}
}
