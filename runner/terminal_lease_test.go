package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func terminalLeaseRequest() *workarea.TerminalLeaseRequest {
	return &workarea.TerminalLeaseRequest{
		SchemaVersion:      workarea.TerminalLeaseRequestSchemaV1,
		SettlementBudgetMS: (17 * time.Minute).Milliseconds(),
		SafetyMarginMS:     time.Minute.Milliseconds(),
		LeaseDurationMS:    (30 * time.Minute).Milliseconds(),
		MaxLeaseDurationMS: (2 * time.Hour).Milliseconds(),
	}
}

func TestStableTerminalResultIDIsDeterministicAndContentBound(t *testing.T) {
	t.Parallel()

	terminal := agent.Result{Status: "completed", Summary: "done", CommitSHA: "abc"}
	first, err := stableTerminalResultID("session-1", terminal)
	if err != nil {
		t.Fatal(err)
	}
	second, err := stableTerminalResultID("session-1", terminal)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "tr_") {
		t.Fatalf("ids = %q / %q", first, second)
	}
	terminal.Summary = "different"
	changed, err := stableTerminalResultID("session-1", terminal)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatalf("content change kept id %q", changed)
	}
}

func TestRunTerminalLeaseDefersRealTeardownBeforeStatusResponse(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git unavailable")
	}

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}

	serverErr := make(chan error, 1)
	statusSeen := make(chan workarea.TerminalLeaseDescriptor, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/status"):
			var body struct {
				Status                string                            `json:"status"`
				TerminalWorkareaLease *workarea.TerminalLeaseDescriptor `json:"terminalWorkareaLease"`
			}
			if decodeErr := json.NewDecoder(req.Body).Decode(&body); decodeErr != nil {
				serverErr <- decodeErr
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body.Status == "running" {
				w.WriteHeader(http.StatusOK)
				return
			}
			if body.Status != "completed" || body.TerminalWorkareaLease == nil {
				serverErr <- fmt.Errorf("status body = %+v", body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			lease, getErr := manager.TerminalLease(body.TerminalWorkareaLease.LeaseID)
			if getErr != nil {
				serverErr <- getErr
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if !lease.ReleaseRequested || lease.State != workarea.LeaseActive {
				serverErr <- fmt.Errorf("lease was not durably deferred before status: %+v", lease)
				w.WriteHeader(http.StatusConflict)
				return
			}
			if _, statErr := os.Stat(lease.WorkareaPath); statErr != nil {
				serverErr <- fmt.Errorf("leased leaf missing while status in flight: %w", statErr)
				w.WriteHeader(http.StatusConflict)
				return
			}
			statusSeen <- *body.TerminalWorkareaLease
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(req.URL.Path, "/lock-refresh"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"refreshed":true}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", HTTPClient: srv.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	provider, _ := stub.New()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	runner, err := New(Options{
		Registry:        registry,
		WorktreeManager: manager,
		Poster:          poster,
		HTTPClient:      srv.Client(),
		SkipBackstop:    true,
		SkipSteering:    true,
		SkipPostSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	queued := QueuedWork{
		QueuedWork:            queuedWorkBase("LEASE-LIFECYCLE-1"),
		WorkerID:              "worker",
		PlatformURL:           srv.URL,
		TerminalWorkareaLease: terminalLeaseRequest(),
		ResolvedProfile:       ResolvedProfile{Provider: agent.ProviderStub},
	}
	queued.Repository = bareRepo

	res, runErr := runner.Run(context.Background(), queued)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	select {
	case err := <-serverErr:
		t.Fatal(err)
	default:
	}
	desc := <-statusSeen
	if res.TerminalWorkareaLease == nil || res.TerminalWorkareaLease.LeaseID != desc.LeaseID {
		t.Fatalf("result descriptor = %+v, status descriptor = %+v", res.TerminalWorkareaLease, desc)
	}
	lease, err := manager.TerminalLease(desc.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.TerminalResultPost == nil || lease.TerminalResultPost.State != workarea.TerminalResultPostObserved {
		t.Fatalf("successful status did not settle outbox: %+v", lease.TerminalResultPost)
	}
	if _, err := os.Stat(lease.WorkareaPath); err != nil {
		t.Fatalf("leaf released without acknowledgement: %v", err)
	}
	if _, err := manager.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID:          desc.LeaseID,
		SessionID:        desc.SessionID,
		TerminalResultID: desc.TerminalResultID,
		WorkareaID:       desc.WorkareaID,
		InvocationID:     "invocation-1",
		ClaimID:          "claim-1",
	}); err != nil {
		t.Fatalf("ClaimTerminalLeaseExecution: %v", err)
	}
	_, err = manager.AcknowledgeTerminalResult(context.Background(), workarea.TerminalResultAcknowledgement{
		SchemaVersion:    workarea.TerminalLeaseAcknowledgementSchemaV1,
		Acknowledged:     true,
		InvocationID:     "invocation-1",
		ClaimID:          "claim-1",
		LeaseID:          desc.LeaseID,
		SessionID:        desc.SessionID,
		TerminalResultID: desc.TerminalResultID,
		WorkareaID:       desc.WorkareaID,
	})
	if err != nil {
		t.Fatalf("AcknowledgeTerminalResult: %v", err)
	}
	if _, err := os.Stat(lease.WorkareaPath); !os.IsNotExist(err) {
		t.Fatalf("acknowledged leaf still exists: %v", err)
	}
}

func TestRunTerminalStatusOutboxReplaysAfterManagerRestart(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git unavailable")
	}

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	var failTerminal atomic.Bool
	failTerminal.Store(true)
	var terminalPosts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/lock-refresh") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"refreshed":true}`))
			return
		}
		if strings.HasSuffix(req.URL.Path, "/status") {
			var body struct {
				Status                string                            `json:"status"`
				TerminalWorkareaLease *workarea.TerminalLeaseDescriptor `json:"terminalWorkareaLease"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body.Status == "completed" && body.TerminalWorkareaLease != nil {
				terminalPosts.Add(1)
				if failTerminal.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", HTTPClient: srv.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	provider, _ := stub.New()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	runner, err := New(Options{
		Registry:        registry,
		WorktreeManager: manager,
		Poster:          poster,
		HTTPClient:      srv.Client(),
		SkipBackstop:    true,
		SkipSteering:    true,
		SkipPostSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	queued := QueuedWork{
		QueuedWork:            queuedWorkBase("LEASE-OUTBOX-1"),
		WorkerID:              "worker",
		PlatformURL:           srv.URL,
		TerminalWorkareaLease: terminalLeaseRequest(),
		ResolvedProfile:       ResolvedProfile{Provider: agent.ProviderStub},
	}
	queued.Repository = bareRepo

	res, runErr := runner.Run(context.Background(), queued)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if res.TerminalWorkareaLease == nil {
		t.Fatal("terminal lease missing after exhausted status post")
	}
	pending, err := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.TerminalResultPost == nil || pending.TerminalResultPost.State != workarea.TerminalResultPostPending {
		t.Fatalf("outbox after failed status = %+v", pending.TerminalResultPost)
	}

	failTerminal.Store(false)
	recovered, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	considered, err := recovered.ReplayTerminalResults(context.Background(), 1, 5*time.Second, poster.ReplayTerminalResult)
	if err != nil || considered != 1 {
		t.Fatalf("ReplayTerminalResults considered=%d err=%v", considered, err)
	}
	observed, err := recovered.TerminalLease(res.TerminalWorkareaLease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.TerminalResultPost.State != workarea.TerminalResultPostObserved || observed.TerminalResultPost.ObservedAt == nil {
		t.Fatalf("replayed outbox = %+v", observed.TerminalResultPost)
	}
	if terminalPosts.Load() != int32(result.DefaultMaxAttempts+1) {
		t.Fatalf("terminal status posts = %d, want %d", terminalPosts.Load(), result.DefaultMaxAttempts+1)
	}
	if _, err := os.Stat(observed.WorkareaPath); err != nil {
		t.Fatalf("replay released workarea before acknowledgement: %v", err)
	}
}

type controlledTerminalProvider struct {
	spawned chan struct{}
	release chan struct{}
}

func (p *controlledTerminalProvider) Name() agent.ProviderName { return "controlled-terminal" }
func (p *controlledTerminalProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}

func (p *controlledTerminalProvider) Spawn(ctx context.Context, _ agent.Spec) (agent.Handle, error) {
	events := make(chan agent.Event, 2)
	close(p.spawned)
	go func() {
		defer close(events)
		select {
		case events <- agent.InitEvent{SessionID: "controlled-1"}:
		case <-ctx.Done():
			return
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		select {
		case events <- agent.ResultEvent{Success: true, Message: "done <!-- WORK_RESULT:passed -->"}:
		case <-ctx.Done():
		}
	}()
	return &fakeHandle{events: events}, nil
}

func (p *controlledTerminalProvider) Resume(ctx context.Context, _ string, spec agent.Spec) (agent.Handle, error) {
	return p.Spawn(ctx, spec)
}
func (p *controlledTerminalProvider) Shutdown(context.Context) error { return nil }

func TestRunTerminalLeaseAcquisitionFailureNeverPostsSuccess(t *testing.T) {
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}

	statusBodies := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/status") {
			var body map[string]any
			_ = json.NewDecoder(req.Body).Decode(&body)
			if body["status"] != "running" {
				statusBodies <- body
			}
		}
		if strings.HasSuffix(req.URL.Path, "/lock-refresh") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"refreshed":true}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", HTTPClient: srv.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	provider := &controlledTerminalProvider{spawned: make(chan struct{}), release: make(chan struct{})}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	runner, err := New(Options{
		Registry:        registry,
		WorktreeManager: manager,
		Poster:          poster,
		HTTPClient:      srv.Client(),
		SkipBackstop:    true,
		SkipSteering:    true,
		SkipPostSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	queued := QueuedWork{
		QueuedWork:            queuedWorkBase("LEASE-FAIL-1"),
		WorkerID:              "worker",
		PlatformURL:           srv.URL,
		TerminalWorkareaLease: terminalLeaseRequest(),
		ResolvedProfile:       ResolvedProfile{Provider: provider.Name()},
	}
	queued.Repository = bareRepo

	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, runErr := runner.Run(context.Background(), queued)
		done <- outcome{res: res, err: runErr}
	}()
	<-provider.spawned
	if _, err := manager.Path(queued.SessionID); err != nil {
		t.Fatalf("worktree path before terminal: %v", err)
	}
	records := filepath.Join(wtParent, ".terminal-leases", "records")
	if err := os.RemoveAll(records); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(records, []byte("not-a-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(provider.release)

	got := <-done
	if got.err == nil || got.res == nil {
		t.Fatalf("outcome = %+v", got)
	}
	if got.res.Status != "failed" || got.res.FailureMode != FailureTerminalWorkareaLease {
		t.Fatalf("result = %+v", got.res)
	}
	body := <-statusBodies
	if body["status"] == "completed" || body["terminalWorkareaLease"] != nil {
		t.Fatalf("eligible terminal status posted after acquisition failure: %#v", body)
	}
	if !errors.Is(got.err, os.ErrNotExist) && !strings.Contains(got.err.Error(), "terminal workarea lease") {
		t.Fatalf("unexpected error: %v", got.err)
	}
}
