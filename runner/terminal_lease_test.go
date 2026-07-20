package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

const (
	runnerLeaseSessionID    = "11111111-1111-4111-8111-111111111111"
	runnerLeaseInvocationID = "22222222-2222-4222-8222-222222222222"
	runnerLeaseClaimID      = "33333333-3333-4333-8333-333333333333"
)

func terminalLeaseRequest() *workarea.TerminalLeaseRequest {
	request := workarea.DefaultTerminalLeaseRequest()
	return &request
}

func TestStableTerminalResultIDIsDeterministicAndContentBound(t *testing.T) {
	t.Parallel()
	terminal := agent.Result{Status: "completed", Summary: "done", CommitSHA: "abc"}
	first, err := stableTerminalResultID(runnerLeaseSessionID, terminal)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := stableTerminalResultID(runnerLeaseSessionID, terminal)
	if first != second || !strings.HasPrefix(first, "tr_") {
		t.Fatalf("ids=%q/%q", first, second)
	}
	terminal.Summary = "different"
	changed, _ := stableTerminalResultID(runnerLeaseSessionID, terminal)
	if changed == first {
		t.Fatal("content change retained terminal result identity")
	}
}

func TestRunPersistsExactProjectionAndReleasesAfterAcknowledgement(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git unavailable")
	}
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	var terminalBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/status") {
			body, readErr := ioReadAll(req)
			if readErr != nil {
				t.Error(readErr)
			}
			var envelope map[string]any
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Error(err)
			}
			if envelope["status"] == "completed" {
				terminalBody = append([]byte(nil), body...)
				projection, ok := envelope["terminalWorkareaLease"].(map[string]any)
				if !ok || len(projection) != 4 || projection["leaseId"] == nil || projection["workareaId"] == nil || projection["terminalResultId"] == nil || projection["expiresAt"] == nil {
					t.Errorf("projection=%#v", envelope["terminalWorkareaLease"])
				}
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
	r := newLeaseRunner(t, manager, poster, false)
	queued := QueuedWork{QueuedWork: queuedWorkBase("LEASE-LIFECYCLE"), WorkerID: "worker", PlatformURL: srv.URL, TerminalWorkareaLease: terminalLeaseRequest(), ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub}}
	queued.SessionID = runnerLeaseSessionID
	queued.Repository = bareRepo
	res, runErr := r.Run(context.Background(), queued)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if res.TerminalWorkareaLease == nil || len(terminalBody) == 0 {
		t.Fatalf("result=%+v body=%q", res.TerminalWorkareaLease, terminalBody)
	}
	lease, err := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.TerminalStatus == nil || lease.TerminalStatus.DeliveryState != workarea.TerminalStatusDelivered {
		t.Fatalf("outbox=%+v", lease.TerminalStatus)
	}
	retainedBody, err := lease.TerminalStatus.Body()
	if err != nil || !bytes.Equal(retainedBody, terminalBody) {
		t.Fatalf("retained body differs err=%v", err)
	}
	claim, err := manager.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: runnerLeaseInvocationID, ClaimID: runnerLeaseClaimID,
	})
	if err != nil || claim.ClaimNowMS != claim.Claim.ClaimedAt.UnixMilli() {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	outcome, err := manager.AcknowledgeTerminalResult(context.Background(), workarea.TerminalResultAcknowledgement{
		SchemaVersion: workarea.TerminalLeaseAcknowledgementSchemaV1, Acknowledged: true,
		InvocationID: runnerLeaseInvocationID, ClaimID: runnerLeaseClaimID, LeaseID: lease.LeaseID,
		SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID, WorkareaID: lease.WorkareaID,
	})
	if err != nil || outcome.Outcome != workarea.AcknowledgementApplied {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if _, err := manager.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lease.WorkareaPath); !os.IsNotExist(err) {
		t.Fatalf("released leaf still exists: %v", err)
	}
}

func TestRunOutboxReplaysByteIdenticallyAfterManagerRestart(t *testing.T) {
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	var accepted []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/lock-refresh") {
			_, _ = w.Write([]byte(`{"refreshed":true}`))
			return
		}
		if strings.HasSuffix(req.URL.Path, "/status") {
			body, _ := ioReadAll(req)
			var envelope map[string]any
			_ = json.Unmarshal(body, &envelope)
			if envelope["status"] == "completed" && fail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if envelope["status"] == "completed" {
				accepted = append([]byte(nil), body...)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", HTTPClient: srv.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	r := newLeaseRunner(t, manager, poster, false)
	queued := QueuedWork{QueuedWork: queuedWorkBase("LEASE-REPLAY"), WorkerID: "worker", PlatformURL: srv.URL, TerminalWorkareaLease: terminalLeaseRequest(), ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub}}
	queued.SessionID = runnerLeaseSessionID
	queued.Repository = bareRepo
	res, _ := r.Run(context.Background(), queued)
	if res.TerminalWorkareaLease == nil {
		t.Fatal("lease projection missing")
	}
	pending, err := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.TerminalStatus.DeliveryState != workarea.TerminalStatusPending {
		t.Fatalf("delivery state=%s", pending.TerminalStatus.DeliveryState)
	}
	retained, _ := pending.TerminalStatus.Body()
	fail.Store(false)
	recovered, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	considered, err := recovered.ReplayTerminalResults(context.Background(), 1, 5*time.Second, poster.TerminalStatusSender(runnerLeaseSessionID))
	if err != nil || considered != 1 {
		t.Fatalf("considered=%d err=%v", considered, err)
	}
	if !bytes.Equal(retained, accepted) {
		t.Fatalf("replayed body changed\nretained=%s\naccepted=%s", retained, accepted)
	}
}

func TestRequestedLeaseSurvivesPreserveAlwaysAndArchivesOnRelease(t *testing.T) {
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
	poster, _ := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", HTTPClient: srv.Client(), BaseDelay: 0})
	r := newLeaseRunner(t, manager, poster, true)
	queued := QueuedWork{QueuedWork: queuedWorkBase("LEASE-PRESERVE"), WorkerID: "worker", PlatformURL: srv.URL, TerminalWorkareaLease: terminalLeaseRequest(), ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub}}
	queued.SessionID = runnerLeaseSessionID
	queued.Repository = bareRepo
	res, err := r.Run(context.Background(), queued)
	if err != nil || res.TerminalWorkareaLease == nil {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	lease, _ := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
	if lease.ReleaseDisposition != "archive" {
		t.Fatalf("disposition=%q", lease.ReleaseDisposition)
	}
	claim, err := manager.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: runnerLeaseInvocationID, ClaimID: runnerLeaseClaimID,
	})
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	_, _ = manager.AcknowledgeTerminalResult(context.Background(), workarea.TerminalResultAcknowledgement{
		SchemaVersion: workarea.TerminalLeaseAcknowledgementSchemaV1, Acknowledged: true,
		InvocationID: runnerLeaseInvocationID, ClaimID: runnerLeaseClaimID, LeaseID: lease.LeaseID,
		SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID, WorkareaID: lease.WorkareaID,
	})
	if _, err := manager.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lease.WorkareaPath); err != nil {
		t.Fatalf("archived preserved leaf missing: %v", err)
	}
}

func newLeaseRunner(t *testing.T, manager *worktree.Manager, poster *result.Poster, preserveAlways bool) *Runner {
	t.Helper()
	registry := NewRegistry()
	provider, _ := stub.New()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{
		Registry: registry, WorktreeManager: manager, Poster: poster, PreserveWorktreeAlways: preserveAlways,
		SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ioReadAll(req *http.Request) ([]byte, error) {
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(req.Body); err != nil {
		_ = req.Body.Close()
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if err := req.Body.Close(); err != nil {
		return nil, fmt.Errorf("close request body: %w", err)
	}
	return buffer.Bytes(), nil
}
