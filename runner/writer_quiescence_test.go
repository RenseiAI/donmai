package runner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// quiescenceStopProvider wraps the stub harness manifest and plays one
// successful turn, but its handle's Stop returns the configured error so
// the test drives the production terminal-stop path: a nil stopErr proves
// the owned writers stopped and reaped, while a non-nil stopErr (for
// example context.DeadlineExceeded) proves they may still be live.
type quiescenceStopProvider struct {
	agent.HarnessProvider
	stopErr   error
	stopCalls *atomic.Int32
}

func (p *quiescenceStopProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	h := &quiescenceStopHandle{
		events:    make(chan agent.Event, 32),
		stopErr:   p.stopErr,
		stopCalls: p.stopCalls,
	}
	h.events <- agent.InitEvent{SessionID: "quiescence-session"}
	h.events <- agent.AssistantTextEvent{Text: "quiescence probe turn"}
	h.events <- agent.ResultEvent{Success: true, Message: "quiescence probe complete"}
	return h, nil
}

func (p *quiescenceStopProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

type quiescenceStopHandle struct {
	events    chan agent.Event
	stopErr   error
	stopCalls *atomic.Int32
	closed    atomic.Bool
}

func (h *quiescenceStopHandle) SessionID() string          { return "quiescence-session" }
func (h *quiescenceStopHandle) Events() <-chan agent.Event { return h.events }
func (h *quiescenceStopHandle) Inject(context.Context, string) error {
	return agent.ErrUnsupported
}

func (h *quiescenceStopHandle) Stop(context.Context) error {
	h.stopCalls.Add(1)
	if h.stopErr != nil {
		return h.stopErr
	}
	if h.closed.CompareAndSwap(false, true) {
		close(h.events)
	}
	return nil
}

// runQuiescenceSession drives one full Runner.Run against the
// controllable-stop provider and returns the manager that owns the
// session, the dispatched session ID, and the Stop call count. The
// caller then acquires a terminal lease on the same manager to read
// the durable writerQuiesced snapshot — the same handoff a daemon
// reaper or a post-restart manager uses.
func runQuiescenceSession(t *testing.T, sessionID string, stopErr error) (*worktree.Manager, string, string, *atomic.Int32) {
	t.Helper()
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	platform := newRecordingPlatformServer(t)
	poster, err := result.NewPoster(result.Options{
		PlatformURL: platform.URL,
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		HTTPClient:  platform.Client(),
		BaseDelay:   0,
	})
	if err != nil {
		t.Fatal(err)
	}
	base, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	harness, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	var stopCalls atomic.Int32
	provider := &quiescenceStopProvider{HarnessProvider: harness, stopErr: stopErr, stopCalls: &stopCalls}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{
		Registry:               reg,
		WorktreeManager:        manager,
		Poster:                 poster,
		HTTPClient:             platform.Client(),
		SkipBackstop:           true,
		SkipSteering:           true,
		SkipPostSession:        true,
		PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	qwBase := queuedWorkBase("QUIESCENCE-STOP")
	qwBase.SessionID = sessionID
	qw := QueuedWork{
		QueuedWork:      qwBase,
		WorkerID:        "worker-1",
		AuthToken:       "tok",
		PlatformURL:     platform.URL,
		ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
	}
	qw.Repository = makeBareRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.Run(ctx, qw); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stopCalls.Load(); got == 0 {
		t.Fatal("production terminal Stop was never invoked; quiescence was not proved")
	}
	return manager, wtParent, qw.SessionID, &stopCalls
}

func acquireQuiescenceLease(t *testing.T, manager *worktree.Manager, sessionID string) *workarea.TerminalLease {
	t.Helper()
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: "tr_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

// TestRunRecordsWriterQuiescenceOnCleanStop drives the production entry
// point (Runner.Run) with a handle whose Stop succeeds, then reads the
// durable lease snapshot: only the terminal Stop inside runLoop may
// record quiesced evidence, and the lease must carry it across a manager
// restart. Deleting the deferred recording in runLoop (the B2 mutant)
// leaves the lease without the snapshot and this test goes RED.
func TestRunRecordsWriterQuiescenceOnCleanStop(t *testing.T) {
	manager, wtParent, sessionID, _ := runQuiescenceSession(t, "55555555-5555-4555-8555-555555555555", nil)
	lease := acquireQuiescenceLease(t, manager, sessionID)
	if lease.ReleaseMetadata["writerQuiesced"] != "true" {
		t.Fatalf("durable lease writerQuiesced = %q, want %q (clean Stop must prove quiesced)", lease.ReleaseMetadata["writerQuiesced"], "true")
	}
	restarted, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := restarted.TerminalLease(lease.LeaseID)
	if err != nil {
		t.Fatalf("restart did not adopt the settled lease: %v", err)
	}
	if adopted.ReleaseMetadata["writerQuiesced"] != "true" {
		t.Fatalf("adopted lease writerQuiesced = %q, want %q", adopted.ReleaseMetadata["writerQuiesced"], "true")
	}
}

// TestRunLeavesUnquiescedOnFailedStop drives the production entry point
// with a handle whose Stop times out: a failed Stop must not become
// durable quiesced evidence. Recording true unconditionally (the B1
// mutant) leaves writerQuiesced=true on the lease and this test goes RED.
func TestRunLeavesUnquiescedOnFailedStop(t *testing.T) {
	manager, _, sessionID, _ := runQuiescenceSession(t, "66666666-6666-4666-8666-666666666666", context.DeadlineExceeded)
	lease := acquireQuiescenceLease(t, manager, sessionID)
	if lease.ReleaseMetadata["writerQuiesced"] == "true" {
		t.Fatalf("durable lease writerQuiesced = %q, want absent on timed-out Stop (Stop must gate the evidence)", lease.ReleaseMetadata["writerQuiesced"])
	}
}

// TestRecordWriterQuiescenceSafeNoOps pins that the recorder never panics
// or refuses: unit-test runners without a manager stay unquiesced
// evidence, never a refusal.
func TestRecordWriterQuiescenceSafeNoOps(t *testing.T) {
	parent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "55555555-5555-4555-8555-555555555555"
	if _, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: sessionID, Strategy: worktree.StrategyEmpty,
	}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{wt: manager}
	r.recordWriterQuiescence(sessionID, true)
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: "tr_55555555555555555555555555555555",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.ReleaseMetadata["writerQuiesced"] != "true" {
		t.Fatalf("durable lease writerQuiesced = %q, want %q", lease.ReleaseMetadata["writerQuiesced"], "true")
	}
	(&Runner{}).recordWriterQuiescence(sessionID, true)
	r.recordWriterQuiescence("", true)
	(&Runner{}).recordWriterQuiescence(sessionID, false)
	r.recordWriterQuiescence("", false)
}
