package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingPreflightForRace blocks the first compilation inside
// PreflightExecution until release closes, so the test can run the
// runtime-token refresher and the session-detail readers while the accept
// is parked mid-flight. Channel ordering only; no sleeps and no
// filesystem fault injection.
type blockingPreflightForRace struct {
	inner   *varyingReadyExecutionPreflight
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingPreflightForRace) Names() []string { return []string{"stub"} }

func (p *blockingPreflightForRace) Capabilities(string) (map[string]any, bool) {
	return map[string]any{}, true
}

func (p *blockingPreflightForRace) PreflightExecution(raw json.RawMessage) (json.RawMessage, error) {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return p.inner.PreflightExecution(raw)
}

// Accepting a session must not share a mutating *SessionDetail with
// concurrent readers. Before the fix the store published the live pointer
// at reservation time and the accept kept reading and writing it without
// the store lock (claim narrowing, receipt stamping) while the
// runtime-token refresher rewrote WorkerID/AuthToken and the detail
// readers dereferenced the same struct. Under -race that is a data race.
// After the fix the reservation is invisible, the accept builds a private
// copy, Publish installs the completed copy atomically, and Get hands out
// copies — so this test passes under -race.
func TestAcceptConcurrentRuntimeRefreshHasNoDataRace(t *testing.T) {
	const sessionID = "session-accept-refresh-race"
	tmp := t.TempDir()
	if err := WriteConfig(filepath.Join(tmp, "daemon.yaml"), &Config{
		APIVersion: "donmai.dev/v1", Kind: "LocalDaemon", ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		ProjectAdmissionMode: ProjectAdmissionModeAllRouted, Machine: MachineConfig{ID: "race-test"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 8},
		Orchestrator: OrchestratorConfig{URL: "https://example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	inner := &varyingReadyExecutionPreflight{sessionID: sessionID, workerID: "worker-local", placement: "host-local"}
	provider := &blockingPreflightForRace{
		inner:   inner,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	store := NewFileExecutionPreflightStore(filepath.Join(tmp, "receipts"))
	d := New(Options{
		ConfigPath: filepath.Join(tmp, "daemon.yaml"), JWTPath: filepath.Join(tmp, "daemon.jwt"),
		SkipWizard: true, SkipRegistration: true, ProviderRegistry: provider, ExecutionPreflightStore: store,
		SpawnerOptions: SpawnerOptions{
			WorkerCommand:        []string{"/bin/sh", "-c", "exit 0"},
			ProjectAdmissionMode: ProjectAdmissionModeAllRouted,
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })

	spec := SessionSpec{SessionID: sessionID, ProjectID: "project-test"}
	type acceptResult struct {
		handle *SessionHandle
		err    error
	}
	acceptDone := make(chan acceptResult, 1)
	go func() {
		handle, err := d.AcceptWorkWithDetail(spec, readyRedeliveryDetail(t, sessionID))
		acceptDone <- acceptResult{handle: handle, err: err}
	}()

	select {
	case <-provider.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("accept did not reach preflight compilation")
	}

	// Readers run while the accept is parked in PreflightExecution (the
	// blocked window) and keep running while it finishes the post-release
	// writes (claim/receipt stamping, publish, spawn). The first iterations
	// are guaranteed to overlap the blocked window: the main goroutine does
	// not close release until the readers have completed enough iterations.
	var iters atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if detail, ok := d.SessionDetail(sessionID); ok && detail != nil {
					_ = detail.WorkerID
					_ = detail.AuthToken
					_ = string(detail.ClaimReceipt)
					_ = string(detail.EffectiveCell)
					_ = string(detail.HostAdaptationReceipt)
					_ = redactSessionDetail(detail)
				}
				if detail, ok := d.sessionDetails.Get(sessionID); ok && detail != nil {
					_ = detail.WorkerID
					_ = string(detail.ExecutionRuntimeBinding)
				}
				d.UpdateSessionRuntimeCredentials("worker-local", "worker-local", "fresh-token")
				iters.Add(1)
			}
		}()
	}
	for iters.Load() < 200 {
		runtime.Gosched()
	}
	close(provider.release)

	var res acceptResult
	select {
	case res = <-acceptDone:
	case <-time.After(10 * time.Second):
		t.Fatal("accept did not finish after release")
	}
	wg.Wait()

	if res.err != nil {
		t.Fatalf("accept with concurrent refresh failed: %v", res.err)
	}
	if res.handle == nil {
		t.Fatal("accept with concurrent refresh has no handle")
	}
	// The durable receipt is the post-accept assertion that survives the
	// immediately-exiting stub worker (the in-memory detail is reaped on
	// SessionEventEnded). The race coverage itself is the concurrent
	// Get/Update loop above running under -race while the accept mutated
	// its private copy pre-publish.
	storedReceipt, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("load receipt after concurrent accept: %v", err)
	}
	if len(storedReceipt) == 0 {
		t.Fatal("stored receipt is empty after concurrent accept")
	}
	if _, err := os.Lstat(filepath.Join(tmp, "receipts", executionPreflightAttemptPendingName(sessionID))); err != nil {
		t.Fatalf("attempt-pending marker missing after concurrent accept: %v", err)
	}
}
