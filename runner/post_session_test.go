package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/result"
	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
)

// proxyCall captures a single POST to /api/issue-tracker-proxy so
// tests can assert the wire shape (method, args).
type proxyCall struct {
	Method string
	Args   []any
}

// stubProxyServer stands up an httptest.Server that records every
// /api/issue-tracker-proxy POST and responds 200 OK with the standard
// success envelope. Other endpoints respond with the no-op completion/
// status/lock-refresh envelope so the runner's result.Poster.Post call
// continues to work alongside the new UpdateIssueStatus flow.
//
// Returns the server, a mutex-guarded slice of captured proxy calls,
// and a teardown func.
func stubProxyServer(t *testing.T) (*httptest.Server, *[]proxyCall, *sync.Mutex) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []proxyCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture proxy calls for assertions.
		if strings.Contains(r.URL.Path, "/api/issue-tracker-proxy") {
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			var req struct {
				Method string `json:"method"`
				Args   []any  `json:"args"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("decode proxy request: %v (body=%q)", err, body)
			}
			mu.Lock()
			calls = append(calls, proxyCall{Method: req.Method, Args: req.Args})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
			return
		}
		// All other endpoints — completion, status, lock-refresh —
		// return the no-op envelope.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

// makePostSessionRunner returns a minimal Runner wired to the stub
// proxy server. Used by post-session tests; does NOT include a
// real worktree manager (the post-session block doesn't touch the
// worktree).
func makePostSessionRunner(t *testing.T, srv *httptest.Server) *Runner {
	t.Helper()
	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "wkr-post",
		AuthToken:   "tok-post",
		HTTPClient:  srv.Client(),
		BaseDelay:   1, // 1ns — effectively no sleep between retries
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	r, err := New(Options{
		Registry:        NewRegistry(),
		WorktreeManager: wtm,
		Poster:          poster,
		HTTPClient:      srv.Client(),
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// TestRunPostSession_PassedTransitionsToFinished is the integration
// test for the REN-1467 happy path: a development session emits
// WORK_RESULT:passed and the runner POSTs updateIssueStatus("Finished")
// to the platform's issue-tracker proxy.
func TestRunPostSession_PassedTransitionsToFinished(t *testing.T) {
	srv, calls, mu := stubProxyServer(t)
	r := makePostSessionRunner(t, srv)

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-PS-PASS"),
		WorkerID:    "wkr-post",
		AuthToken:   "tok-post",
		PlatformURL: srv.URL,
	}
	qw.WorkType = WorkTypeDevelopmentStr
	qw.IssueID = "issue-uuid-abc"

	res := &Result{}
	res.Status = "completed"
	res.WorkResult = "passed"

	r.runPostSession(context.Background(), qw, res)

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("expected 1 proxy call; got %d (%v)", len(*calls), *calls)
	}
	c := (*calls)[0]
	if c.Method != "updateIssueStatus" {
		t.Errorf("Method = %q; want updateIssueStatus", c.Method)
	}
	if len(c.Args) != 2 || c.Args[0] != "issue-uuid-abc" || c.Args[1] != "Finished" {
		t.Errorf("Args = %v; want [issue-uuid-abc Finished]", c.Args)
	}
	if res.LinearStatusTransition == nil {
		t.Fatal("LinearStatusTransition not set on result")
	}
	if !res.LinearStatusTransition.Succeeded {
		t.Errorf("Succeeded = false; want true (err=%q)", res.LinearStatusTransition.Error)
	}
	if res.LinearStatusTransition.TargetStatus != "Finished" {
		t.Errorf("TargetStatus = %q; want Finished", res.LinearStatusTransition.TargetStatus)
	}
	if len(res.PostSessionWarnings) != 0 {
		t.Errorf("PostSessionWarnings = %v; want none", res.PostSessionWarnings)
	}
}

// TestRunPostSession_FailedQATransitionsToRejected covers the QA fail
// branch — emits updateIssueStatus("Rejected").
func TestRunPostSession_FailedQATransitionsToRejected(t *testing.T) {
	srv, calls, mu := stubProxyServer(t)
	r := makePostSessionRunner(t, srv)

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-PS-FAIL"),
		WorkerID:    "wkr-post",
		AuthToken:   "tok-post",
		PlatformURL: srv.URL,
	}
	qw.WorkType = WorkTypeQAStr
	qw.IssueID = "issue-uuid-qa"

	res := &Result{}
	res.Status = "completed"
	res.WorkResult = "failed"

	r.runPostSession(context.Background(), qw, res)

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("expected 1 proxy call; got %d", len(*calls))
	}
	c := (*calls)[0]
	if c.Method != "updateIssueStatus" || c.Args[1] != "Rejected" {
		t.Errorf("got Method=%s Args=%v; want updateIssueStatus [issue Rejected]", c.Method, c.Args)
	}
}

// TestRunPostSession_UnknownPostsDiagnosticComment covers the
// regression-test acceptance criterion: a result-sensitive type that
// completes without a WORK_RESULT marker triggers the createComment
// proxy call (NOT updateIssueStatus).
func TestRunPostSession_UnknownPostsDiagnosticComment(t *testing.T) {
	srv, calls, mu := stubProxyServer(t)
	r := makePostSessionRunner(t, srv)

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-PS-UNKNOWN"),
		WorkerID:    "wkr-post",
		AuthToken:   "tok-post",
		PlatformURL: srv.URL,
	}
	qw.WorkType = WorkTypeDevelopmentStr
	qw.IssueID = "issue-uuid-unk"

	res := &Result{}
	res.Status = "completed"
	res.WorkResult = "" // no marker — unknown branch

	r.runPostSession(context.Background(), qw, res)

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("expected 1 proxy call; got %d (%v)", len(*calls), *calls)
	}
	c := (*calls)[0]
	if c.Method != "createComment" {
		t.Errorf("Method = %q; want createComment", c.Method)
	}
	if len(c.Args) != 2 || c.Args[0] != "issue-uuid-unk" {
		t.Errorf("Args = %v; want [issue-uuid-unk <body>]", c.Args)
	}
	body, _ := c.Args[1].(string)
	if !strings.Contains(body, "WORK_RESULT:passed") {
		t.Errorf("body missing WORK_RESULT:passed mention; got %q", body)
	}
	if !strings.Contains(body, "WORK_RESULT:failed") {
		t.Errorf("body missing WORK_RESULT:failed mention; got %q", body)
	}
	if res.LinearStatusTransition == nil {
		t.Fatal("LinearStatusTransition not set")
	}
	if !res.LinearStatusTransition.DiagnosticPosted {
		t.Errorf("DiagnosticPosted = false; want true")
	}
	if res.LinearStatusTransition.Reason != "unknown" {
		t.Errorf("Reason = %q; want unknown", res.LinearStatusTransition.Reason)
	}
}

// TestRunPostSession_AcceptancePassedNoMQTransitions covers the
// non-deferred acceptance path: with no merge-queue adapter, a passing
// acceptance immediately transitions Delivered → Accepted.
func TestRunPostSession_AcceptancePassedNoMQTransitions(t *testing.T) {
	srv, calls, mu := stubProxyServer(t)
	r := makePostSessionRunner(t, srv)

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-PS-AC"),
		WorkerID:    "wkr-post",
		AuthToken:   "tok-post",
		PlatformURL: srv.URL,
	}
	qw.WorkType = WorkTypeAcceptance
	qw.IssueID = "issue-uuid-acc"

	res := &Result{}
	res.Status = "completed"
	res.WorkResult = "passed"

	r.runPostSession(context.Background(), qw, res)

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("expected 1 proxy call; got %d", len(*calls))
	}
	c := (*calls)[0]
	if c.Args[1] != "Accepted" {
		t.Errorf("targetStatus = %v; want Accepted", c.Args[1])
	}
}

// TestRunPostSession_NonResultSensitive_PromotesOnComplete covers the
// research/refinement fast-path: completion alone triggers the
// configured status transition (when one exists).
func TestRunPostSession_NonResultSensitive_PromotesOnComplete(t *testing.T) {
	srv, calls, mu := stubProxyServer(t)
	r := makePostSessionRunner(t, srv)

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-PS-REF"),
		WorkerID:    "wkr-post",
		AuthToken:   "tok-post",
		PlatformURL: srv.URL,
	}
	qw.WorkType = WorkTypeRefinement // → Backlog on complete
	qw.IssueID = "issue-uuid-ref"

	res := &Result{}
	res.Status = "completed"
	// No WORK_RESULT — non-sensitive, should still transition.
	res.WorkResult = ""

	r.runPostSession(context.Background(), qw, res)

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("expected 1 proxy call; got %d", len(*calls))
	}
	c := (*calls)[0]
	if c.Args[1] != "Backlog" {
		t.Errorf("targetStatus = %v; want Backlog", c.Args[1])
	}
}

// TestRunPostSession_NoMappingNoCall covers the silent path — research
// completion has no mapping, so no proxy call should fire.
func TestRunPostSession_NoMappingNoCall(t *testing.T) {
	srv, calls, mu := stubProxyServer(t)
	r := makePostSessionRunner(t, srv)

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-PS-RES"),
		WorkerID:    "wkr-post",
		AuthToken:   "tok-post",
		PlatformURL: srv.URL,
	}
	qw.WorkType = WorkTypeResearch
	qw.IssueID = "issue-uuid-res"

	res := &Result{}
	res.Status = "completed"

	r.runPostSession(context.Background(), qw, res)

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 0 {
		t.Errorf("expected 0 proxy calls; got %d (%v)", len(*calls), *calls)
	}
	if res.LinearStatusTransition == nil {
		t.Fatal("LinearStatusTransition not set")
	}
	if res.LinearStatusTransition.Reason != "no-mapping" {
		t.Errorf("Reason = %q; want no-mapping", res.LinearStatusTransition.Reason)
	}
}

// TestRunPostSession_TransitionFailureSurfaceWarning covers the
// "transition failed" path: the proxy returns 500, the runner records
// a PostSessionWarnings entry, and the session's terminal Status
// remains unchanged (the failure is observability-only).
func TestRunPostSession_TransitionFailureSurfaceWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/issue-tracker-proxy") {
			http.Error(w, `{"success":false,"error":{"code":"INTERNAL","message":"boom","retryable":false}}`, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	r := makePostSessionRunner(t, srv)

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-PS-ERR"),
		WorkerID:    "wkr-post",
		AuthToken:   "tok-post",
		PlatformURL: srv.URL,
	}
	qw.WorkType = WorkTypeDevelopmentStr
	qw.IssueID = "issue-uuid-err"

	res := &Result{}
	res.Status = "completed"
	res.WorkResult = "passed"

	r.runPostSession(context.Background(), qw, res)

	if res.LinearStatusTransition == nil {
		t.Fatal("LinearStatusTransition not set")
	}
	if res.LinearStatusTransition.Succeeded {
		t.Errorf("Succeeded = true; want false")
	}
	if !res.LinearStatusTransition.Attempted {
		t.Errorf("Attempted = false; want true")
	}
	if len(res.PostSessionWarnings) == 0 {
		t.Errorf("expected at least 1 PostSessionWarnings entry; got 0")
	}
	// Critical invariant: the session's terminal Status is NOT changed
	// by a failed transition — the post-session block is best-effort.
	if res.Status != "completed" {
		t.Errorf("Status = %q; want completed (transition failure must NOT change session status)", res.Status)
	}
}

// TestRunPostSession_NoIssueIDSkips guards the early-exit at the
// runner.runLoop call site: when QueuedWork carries no IssueID (e.g.
// governor work types without a Linear-side row), the post-session
// block is not invoked. We assert by calling the function with
// IssueID empty and confirming no proxy traffic.
func TestRunPostSession_NoIssueIDSkips(t *testing.T) {
	srv, calls, mu := stubProxyServer(t)
	r := makePostSessionRunner(t, srv)

	// Note: the early-exit check lives in runLoop — runPostSession
	// itself doesn't gate on IssueID. We therefore exercise the gate
	// by setting up the conditions and calling runPostSession with an
	// empty IssueID; the proxy call should fail with the "issueID is
	// required" error rather than silently sending a bad request to
	// the platform. PostSessionWarnings captures the failure.
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-PS-NOID"),
		WorkerID:    "wkr-post",
		AuthToken:   "tok-post",
		PlatformURL: srv.URL,
	}
	qw.WorkType = WorkTypeDevelopmentStr
	qw.IssueID = ""

	res := &Result{}
	res.Status = "completed"
	res.WorkResult = "passed"

	r.runPostSession(context.Background(), qw, res)

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 0 {
		t.Errorf("expected 0 proxy calls (empty IssueID); got %d", len(*calls))
	}
	if len(res.PostSessionWarnings) == 0 {
		t.Errorf("expected validation warning; got 0")
	}
}
