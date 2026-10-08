package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// shimSeatRunner builds a Runner with the standalone outbox enabled for
// attempt 1, posting to srv. The caller owns the worktree manager so the
// test can reopen the authority the way a restarted daemon does.
func shimSeatRunner(t *testing.T, manager *worktree.Manager, poster *result.Poster) *Runner {
	t.Helper()
	registry := NewRegistry()
	provider, _ := stub.New()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{
		Registry: registry, WorktreeManager: manager, Poster: poster,
		SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
		ShimSeat: &ShimSeatConfig{Attempt: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestShimSeatPersistsTerminalBodyBeforeFirstSend drives the production Run
// entry point with the standalone outbox enabled and the status endpoint
// refusing every send. The run must still persist the exact terminal bytes
// before that first refused send, so a later daemon replay delivers them
// byte-identically.
func TestShimSeatPersistsTerminalBodyBeforeFirstSend(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	var statusAttempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/lock-refresh") {
			_, _ = w.Write([]byte(`{"refreshed":true}`))
			return
		}
		if strings.HasSuffix(req.URL.Path, "/status") {
			statusAttempts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", HTTPClient: srv.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	r := shimSeatRunner(t, manager, poster)
	queued := QueuedWork{QueuedWork: queuedWorkBase("SHIM-OUTBOX-PERSIST"), WorkerID: "worker", PlatformURL: srv.URL, ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub}}
	queued.SessionID = runnerLeaseSessionID
	queued.Repository = bareRepo
	res, _ := r.Run(context.Background(), queued)
	if res == nil || res.Status == "" {
		t.Fatalf("result=%+v", res)
	}
	if statusAttempts.Load() == 0 {
		t.Fatal("status endpoint was never attempted")
	}
	// The send failed, so the run reports the post failure — but the exact
	// bytes must already be durable in the standalone outbox.
	loaded, err := manager.LoadStandaloneTerminalOutbox(context.Background(), queued.SessionID, 1)
	if err != nil {
		t.Fatalf("standalone outbox missing after refused send: %v", err)
	}
	retained, err := loaded.Record.Body()
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(retained, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["status"] != res.Status {
		t.Fatalf("persisted status=%v, want %q", envelope["status"], res.Status)
	}
	if loaded.Record.DeliveryState != workarea.TerminalStatusPending {
		t.Fatalf("delivery=%q, want pending", loaded.Record.DeliveryState)
	}
}

// TestShimSeatKilledAfterPersistReplaysExactBytesOnce is the Done-when
// replay: the first send never happens (the runner is "killed" after
// persist by refusing the status endpoint), then a reopened manager — the
// restarted daemon — replays the exact persisted bytes through a live
// receiver exactly once, and the runner's own record wins over a fallback.
func TestShimSeatKilledAfterPersistReplaysExactBytesOnce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/lock-refresh") {
			_, _ = w.Write([]byte(`{"refreshed":true}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(dead.Close)
	poster, err := result.NewPoster(result.Options{PlatformURL: dead.URL, WorkerID: "worker", HTTPClient: dead.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	r := shimSeatRunner(t, manager, poster)
	queued := QueuedWork{QueuedWork: queuedWorkBase("SHIM-OUTBOX-REPLAY"), WorkerID: "worker", PlatformURL: dead.URL, ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub}}
	queued.SessionID = runnerLeaseSessionID
	queued.Repository = bareRepo
	if _, err := r.Run(context.Background(), queued); err != nil {
		t.Logf("run error (post refused, persist must still hold): %v", err)
	}
	persisted, err := manager.LoadStandaloneTerminalOutbox(context.Background(), queued.SessionID, 1)
	if err != nil {
		t.Fatalf("no persisted body to replay: %v", err)
	}
	persistedBytes, _ := persisted.Record.Body()

	// The restarted daemon reopens the same authority and drains the
	// leaseless record through a live receiver.
	recovered, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	var accepted [][]byte
	var acceptedCount atomic.Int64
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/lock-refresh") {
			_, _ = w.Write([]byte(`{"refreshed":true}`))
			return
		}
		body, _ := ioReadAll(req)
		accepted = append(accepted, body)
		acceptedCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(live.Close)
	livePoster, err := result.NewPoster(result.Options{PlatformURL: live.URL, WorkerID: "worker", HTTPClient: live.Client(), BaseDelay: 0, ReceiverKey: poster.ReceiverKey()})
	if err != nil {
		t.Fatal(err)
	}
	considered, err := recovered.ReplayStandaloneTerminalOutbox(context.Background(), 8, 5*time.Second, livePoster.TerminalStatusSender(queued.SessionID))
	if err != nil || considered != 1 {
		t.Fatalf("considered=%d err=%v", considered, err)
	}
	if acceptedCount.Load() != 1 || len(accepted) != 1 || !bytes.Equal(accepted[0], persistedBytes) {
		t.Fatalf("replayed bytes differ: got %d sends", acceptedCount.Load())
	}
	// A forced second replay sends nothing: the record is delivered.
	considered, err = recovered.ReplayStandaloneTerminalOutbox(context.Background(), 8, 5*time.Second, livePoster.TerminalStatusSender(queued.SessionID))
	if err != nil || considered != 0 {
		t.Fatalf("second replay considered=%d err=%v", considered, err)
	}
	if acceptedCount.Load() != 1 {
		t.Fatalf("second replay resent: sends=%d", acceptedCount.Load())
	}
	// The fallback can never coexist with the runner record on this key.
	fallbackBody, err := poster.PrepareWorkerExitedWithoutResultBody(queued.SessionID, "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := stableTerminalResultID(queued.SessionID, agent.Result{Status: "failed", FailureMode: result.WorkerExitedWithoutResultFailureMode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.SaveStandaloneTerminalOutbox(context.Background(), workarea.StandaloneOutboxSaveSpec{
		SessionID: queued.SessionID, Attempt: 1, TerminalResultID: digest,
		ReceiverKey: poster.ReceiverKey(), Body: fallbackBody,
		DeadlineAt: persisted.Record.DeadlineAt, Kind: workarea.StandaloneOutboxWorkerExited, GroupReaped: true,
	}); err == nil {
		t.Fatal("fallback coexists with the runner record")
	}
}

// TestShimSeatPersistFailureKeepsSeatOutcome pins the best-effort policy: a
// shim-owned seat whose outbox persist is refused (here a non-UUID fixture
// session the store cannot key) still reports what the work did. The send
// stays authoritative; a successful send needs no replay.
func TestShimSeatPersistFailureKeepsSeatOutcome(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/lock-refresh") {
			_, _ = w.Write([]byte(`{"refreshed":true}`))
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", HTTPClient: srv.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	r := shimSeatRunner(t, manager, poster)
	queued := QueuedWork{QueuedWork: queuedWorkBase("SHIM-OUTBOX-NONUUID"), WorkerID: "worker", PlatformURL: srv.URL, ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub}}
	queued.SessionID = "owner-session"
	queued.Repository = bareRepo
	res, err := r.Run(context.Background(), queued)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status=%q failureMode=%q err=%q, want the seat's real completed outcome", res.Status, res.FailureMode, res.Error)
	}
	if res.FailureMode == FailureTerminalWorkareaLease {
		t.Fatal("refused outbox persist flipped the seat to a lease failure")
	}
}

// TestShimSeatPersistsFailedTerminalStatus covers the non-completed half of
// the slice: a failed seat persists its exact failed body too, lease or not.
func TestShimSeatPersistsFailedTerminalStatus(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/lock-refresh") {
			_, _ = w.Write([]byte(`{"refreshed":true}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", HTTPClient: srv.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	r := shimSeatRunner(t, manager, poster)
	queued := QueuedWork{QueuedWork: queuedWorkBase("SHIM-OUTBOX-FAILED"), WorkerID: "worker", PlatformURL: srv.URL, ResolvedProfile: ResolvedProfile{
		Provider:       agent.ProviderStub,
		ProviderConfig: map[string]any{"stub.behavior": string(stub.BehaviorSilentFail)},
	}}
	queued.SessionID = runnerLeaseSessionID
	queued.Repository = bareRepo
	res, _ := r.Run(context.Background(), queued)
	if res == nil || res.Status != "failed" {
		t.Fatalf("result=%+v, want failed", res)
	}
	loaded, err := manager.LoadStandaloneTerminalOutbox(context.Background(), queued.SessionID, 1)
	if err != nil {
		t.Fatalf("failed body not persisted: %v", err)
	}
	retained, _ := loaded.Record.Body()
	var envelope map[string]any
	if err := json.Unmarshal(retained, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["status"] != "failed" {
		t.Fatalf("persisted status=%v, want failed", envelope["status"])
	}
}
