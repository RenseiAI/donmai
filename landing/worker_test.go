package landing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/landing/strategies"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeStrategy is a programmable strategies.Strategy. Each phase returns a
// pre-set result and records that it was called.
type fakeStrategy struct {
	prepareResult strategies.PrepareResult
	prepareErr    error
	executeResult strategies.MergeResult
	executeErr    error
	finalizeErr   error

	mu             sync.Mutex
	prepareCalls   int
	executeCalled  bool
	finalizeCalled bool
}

func (f *fakeStrategy) Name() string { return "fake" }

func (f *fakeStrategy) Prepare(_ context.Context, _ strategies.Context) (strategies.PrepareResult, error) {
	f.mu.Lock()
	f.prepareCalls++
	f.mu.Unlock()
	return f.prepareResult, f.prepareErr
}

func (f *fakeStrategy) Execute(_ context.Context, _ strategies.Context) (strategies.MergeResult, error) {
	f.mu.Lock()
	f.executeCalled = true
	f.mu.Unlock()
	return f.executeResult, f.executeErr
}

func (f *fakeStrategy) Finalize(_ context.Context, _ strategies.Context) error {
	f.mu.Lock()
	f.finalizeCalled = true
	f.mu.Unlock()
	return f.finalizeErr
}

// retryThenSucceedStrategy returns retryable prepare failures for the first
// failUntil calls, then succeeds.
type retryThenSucceedStrategy struct {
	failUntil int
	mu        sync.Mutex
	calls     int
}

func (s *retryThenSucceedStrategy) Name() string { return "retry" }

func (s *retryThenSucceedStrategy) Prepare(_ context.Context, _ strategies.Context) (strategies.PrepareResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.failUntil {
		return strategies.PrepareResult{Success: false, Retryable: true, Error: "branch held"}, nil
	}
	return strategies.PrepareResult{Success: true, HeadSHA: "abc"}, nil
}

func (s *retryThenSucceedStrategy) Execute(_ context.Context, _ strategies.Context) (strategies.MergeResult, error) {
	return strategies.MergeResult{Status: strategies.StatusSuccess, MergedSHA: "abc"}, nil
}

func (s *retryThenSucceedStrategy) Finalize(_ context.Context, _ strategies.Context) error {
	return nil
}

// fakeResolver is a programmable conflictResolver.
type fakeResolver struct {
	result ResolutionResult
	err    error
	called bool
}

func (f *fakeResolver) Resolve(_ context.Context, _ ConflictContext) (ResolutionResult, error) {
	f.called = true
	return f.result, f.err
}

// fakeLockHandler is a programmable lockHandler.
type fakeLockHandler struct {
	should bool
	result RegenerationResult
	err    error
	called bool
}

func (f *fakeLockHandler) ShouldRegenerate(_ PackageManager, _ bool) bool { return f.should }

func (f *fakeLockHandler) Regenerate(_ context.Context, _ string, _ PackageManager) (RegenerationResult, error) {
	f.called = true
	return f.result, f.err
}

// fakeRedis is an in-memory RedisClient for worker/pool tests.
type fakeRedis struct {
	mu     sync.Mutex
	values map[string]string
	// setNXFail makes the next SetNX return (false, nil) — simulates a held lock.
	lockHeld map[string]bool
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{values: map[string]string{}, lockHeld: map[string]bool{}}
}

func (r *fakeRedis) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lockHeld[key] {
		return false, nil
	}
	if _, ok := r.values[key]; ok {
		return false, nil
	}
	r.values[key] = value
	return true, nil
}

func (r *fakeRedis) Del(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

func (r *fakeRedis) Get(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values[key], nil
}

func (r *fakeRedis) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}

func (r *fakeRedis) Expire(_ context.Context, _ string, _ time.Duration) error { return nil }

func (r *fakeRedis) setMarker(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = "1"
}

// fakeStorage is an in-memory Storage recording mark calls for assertions.
type fakeStorage struct {
	mu        sync.Mutex
	queue     []Entry
	completed []int
	failed    map[int]string
	blocked   map[int]string
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{failed: map[int]string{}, blocked: map[int]string{}}
}

func (s *fakeStorage) Enqueue(_ context.Context, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queue {
		if q.Proposal == e.Proposal {
			return nil
		}
	}
	s.queue = append(s.queue, e)
	return nil
}

func (s *fakeStorage) Dequeue(_ context.Context, _ Key) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil, nil
	}
	e := s.queue[0]
	s.queue = s.queue[1:]
	return &e, nil
}

func (s *fakeStorage) PeekAll(_ context.Context, _ Key) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.queue))
	copy(out, s.queue)
	return out, nil
}

func (s *fakeStorage) DequeueBatch(_ context.Context, _ Key, proposals []int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[int]bool{}
	for _, p := range proposals {
		want[p] = true
	}
	var out []Entry
	var remaining []Entry
	for _, e := range s.queue {
		if want[e.Proposal] {
			out = append(out, e)
		} else {
			remaining = append(remaining, e)
		}
	}
	s.queue = remaining
	return out, nil
}

func (s *fakeStorage) QueueDepth(_ context.Context, _ Key) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue), nil
}

func (s *fakeStorage) IsEnqueued(_ context.Context, _ Key, proposal int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.queue {
		if e.Proposal == proposal {
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeStorage) Position(_ context.Context, _ Key, proposal int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.queue {
		if e.Proposal == proposal {
			return i + 1, nil
		}
	}
	return 0, nil
}

func (s *fakeStorage) Remove(_ context.Context, _ Key, proposal int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var remaining []Entry
	for _, e := range s.queue {
		if e.Proposal != proposal {
			remaining = append(remaining, e)
		}
	}
	s.queue = remaining
	return nil
}

func (s *fakeStorage) MarkCompleted(_ context.Context, _ Key, proposal int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, proposal)
	return nil
}

func (s *fakeStorage) MarkFailed(_ context.Context, _ Key, proposal int, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[proposal] = reason
	return nil
}

func (s *fakeStorage) MarkBlocked(_ context.Context, _ Key, proposal int, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked[proposal] = reason
	return nil
}

func (s *fakeStorage) FailedReason(_ context.Context, _ Key, proposal int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed[proposal], nil
}

func (s *fakeStorage) BlockedReason(_ context.Context, _ Key, proposal int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked[proposal], nil
}

// compile-time assertions for fakes.
var (
	_ Storage          = (*fakeStorage)(nil)
	_ RedisClient      = (*fakeRedis)(nil)
	_ conflictResolver = (*fakeResolver)(nil)
	_ lockHandler      = (*fakeLockHandler)(nil)
)

// newTestWorker wires a Worker with injectable fakes and an instant sleep.
func newTestWorker(cfg WorkerConfig, st *fakeStorage, rd *fakeRedis, stgy strategies.Strategy, res conflictResolver, lh lockHandler, runner commandRunner) *Worker {
	w := NewWorker(cfg, WorkerDeps{Storage: st, Redis: rd})
	w.runner = runner
	w.newStrategy = func(string) (strategies.Strategy, error) { return stgy, nil }
	w.newResolver = func(ConflictResolverConfig) conflictResolver { return res }
	w.newLockHandler = func() lockHandler { return lh }
	w.sleep = func(context.Context, time.Duration) {}
	w.now = func() time.Time { return fixedTime }
	return w
}

// ---------------------------------------------------------------------------
// ProcessEntry tests
// ---------------------------------------------------------------------------

func TestWorkerProcessEntry(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	entry := Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 1, SourceBranch: "ABC-1", IssueID: "ABC-1"}

	tests := []struct {
		name       string
		cfg        WorkerConfig
		stgy       strategies.Strategy
		resolver   *fakeResolver
		lock       *fakeLockHandler
		runnerErr  string // if set, the test command (sh -c) errors with this message
		preMarker  bool
		wantStatus ProcessStatus
		assert     func(t *testing.T, fs *fakeStrategy, res *fakeResolver, lh *fakeLockHandler)
	}{
		{
			name: "clean landing",
			cfg:  WorkerConfig{Key: key, Strategy: "fake"},
			stgy: &fakeStrategy{
				prepareResult: strategies.PrepareResult{Success: true, HeadSHA: "h"},
				executeResult: strategies.MergeResult{Status: strategies.StatusSuccess, MergedSHA: "m"},
			},
			wantStatus: ProcessMerged,
			assert: func(t *testing.T, fs *fakeStrategy, _ *fakeResolver, _ *fakeLockHandler) {
				if !fs.finalizeCalled {
					t.Error("Finalize should be called on a clean landing")
				}
			},
		},
		{
			name:       "pre-flight local marker → noop",
			cfg:        WorkerConfig{Key: key, Strategy: "fake"},
			stgy:       &fakeStrategy{},
			preMarker:  true,
			wantStatus: ProcessNoop,
			assert: func(t *testing.T, fs *fakeStrategy, _ *fakeResolver, _ *fakeLockHandler) {
				if fs.prepareCalls != 0 {
					t.Error("marker present should short-circuit before Prepare")
				}
			},
		},
		{
			name: "prepare already-merged → noop",
			cfg:  WorkerConfig{Key: key, Strategy: "fake"},
			stgy: &fakeStrategy{
				prepareResult: strategies.PrepareResult{Success: false, AlreadyMerged: true},
			},
			wantStatus: ProcessNoop,
		},
		{
			name: "prepare hard failure → error",
			cfg:  WorkerConfig{Key: key, Strategy: "fake"},
			stgy: &fakeStrategy{
				prepareResult: strategies.PrepareResult{Success: false, Error: "fetch failed"},
			},
			wantStatus: ProcessError,
		},
		{
			name: "execute conflict unresolved → conflict",
			cfg:  WorkerConfig{Key: key, Strategy: "fake"},
			stgy: &fakeStrategy{
				prepareResult: strategies.PrepareResult{Success: true},
				executeResult: strategies.MergeResult{Status: strategies.StatusConflict, ConflictFiles: []string{"a.go"}},
			},
			resolver:   &fakeResolver{result: ResolutionResult{Status: ResolutionEscalated, Message: "needs human"}},
			wantStatus: ProcessConflict,
			assert: func(t *testing.T, _ *fakeStrategy, res *fakeResolver, _ *fakeLockHandler) {
				if !res.called {
					t.Error("resolver should be invoked on a conflict")
				}
			},
		},
		{
			name: "execute conflict resolved → continues to merge",
			cfg:  WorkerConfig{Key: key, Strategy: "fake"},
			stgy: &fakeStrategy{
				prepareResult: strategies.PrepareResult{Success: true},
				executeResult: strategies.MergeResult{Status: strategies.StatusConflict, ConflictFiles: []string{"a.go"}},
			},
			resolver:   &fakeResolver{result: ResolutionResult{Status: ResolutionResolved}},
			wantStatus: ProcessMerged,
		},
		{
			name: "execute error → error",
			cfg:  WorkerConfig{Key: key, Strategy: "fake"},
			stgy: &fakeStrategy{
				prepareResult: strategies.PrepareResult{Success: true},
				executeResult: strategies.MergeResult{Status: strategies.StatusError, Error: "boom"},
			},
			wantStatus: ProcessError,
		},
		{
			name: "lock regen failure → error",
			cfg:  WorkerConfig{Key: key, Strategy: "fake", LockFileRegenerate: true, PackageManager: PMPnpm},
			stgy: &fakeStrategy{
				prepareResult: strategies.PrepareResult{Success: true},
				executeResult: strategies.MergeResult{Status: strategies.StatusSuccess},
			},
			lock:       &fakeLockHandler{should: true, result: RegenerationResult{Success: false, Error: "install failed"}},
			wantStatus: ProcessError,
		},
		{
			name: "test failure → test-failure",
			cfg:  WorkerConfig{Key: key, Strategy: "fake", TestCommand: "go test ./..."},
			stgy: &fakeStrategy{
				prepareResult: strategies.PrepareResult{Success: true},
				executeResult: strategies.MergeResult{Status: strategies.StatusSuccess},
			},
			runnerErr:  "FAIL",
			wantStatus: ProcessTestFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStorage()
			rd := newFakeRedis()
			if tt.preMarker {
				rd.setMarker(key.completedKey(entry.Proposal))
			}
			res := tt.resolver
			if res == nil {
				res = &fakeResolver{result: ResolutionResult{Status: ResolutionResolved}}
			}
			lh := tt.lock
			if lh == nil {
				lh = &fakeLockHandler{should: false}
			}
			runner := &fakeRunner{}
			if tt.runnerErr != "" {
				runner.reply = errReply("go test", tt.runnerErr, "")
			}
			w := newTestWorker(tt.cfg, st, rd, tt.stgy, res, lh, runner)

			result, err := w.ProcessEntry(context.Background(), entry)
			if err != nil {
				t.Fatalf("ProcessEntry returned error: %v", err)
			}
			if result.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q (msg=%q)", result.Status, tt.wantStatus, result.Message)
			}
			if tt.assert != nil {
				fs, _ := tt.stgy.(*fakeStrategy)
				tt.assert(t, fs, res, lh)
			}
		})
	}
}

func TestWorkerProcessEntryRetryablePrepare(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	entry := Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 1, SourceBranch: "b"}
	stgy := &retryThenSucceedStrategy{failUntil: 2}
	cfg := WorkerConfig{Key: key, Strategy: "retry", RetryablePrepareBackoffs: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	w := newTestWorker(cfg, newFakeStorage(), newFakeRedis(), stgy, &fakeResolver{}, &fakeLockHandler{}, &fakeRunner{})

	result, err := w.ProcessEntry(context.Background(), entry)
	if err != nil {
		t.Fatalf("ProcessEntry: %v", err)
	}
	if result.Status != ProcessMerged {
		t.Errorf("status = %q, want merged after retries (msg=%q)", result.Status, result.Message)
	}
	if stgy.calls != 3 {
		t.Errorf("Prepare called %d times, want 3 (2 retryable + 1 success)", stgy.calls)
	}
}

func TestWorkerProcessEntryRetryExhausted(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	entry := Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 1, SourceBranch: "b"}
	stgy := &retryThenSucceedStrategy{failUntil: 99} // always retryable-fails
	cfg := WorkerConfig{Key: key, Strategy: "retry", RetryablePrepareBackoffs: []time.Duration{time.Millisecond}}
	w := newTestWorker(cfg, newFakeStorage(), newFakeRedis(), stgy, &fakeResolver{}, &fakeLockHandler{}, &fakeRunner{})

	result, _ := w.ProcessEntry(context.Background(), entry)
	if result.Status != ProcessError {
		t.Errorf("status = %q, want error after retries exhausted", result.Status)
	}
	// 1 initial + 1 retry = 2 prepare calls.
	if stgy.calls != 2 {
		t.Errorf("Prepare called %d times, want 2", stgy.calls)
	}
}

// ---------------------------------------------------------------------------
// handleResult tests
// ---------------------------------------------------------------------------

func TestWorkerHandleResult(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	entry := Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 5}

	tests := []struct {
		name   string
		cfg    WorkerConfig
		result ProcessResult
		check  func(t *testing.T, st *fakeStorage, rd *fakeRedis)
	}{
		{
			name:   "merged marks completed and writes marker",
			cfg:    WorkerConfig{Key: key},
			result: ProcessResult{Proposal: 5, Status: ProcessMerged},
			check: func(t *testing.T, st *fakeStorage, rd *fakeRedis) {
				if len(st.completed) != 1 || st.completed[0] != 5 {
					t.Errorf("completed = %v, want [5]", st.completed)
				}
				if v, _ := rd.Get(context.Background(), key.completedKey(5)); v == "" {
					t.Error("recently-landed marker should be written on merge")
				}
			},
		},
		{
			name:   "conflict marks blocked",
			cfg:    WorkerConfig{Key: key},
			result: ProcessResult{Proposal: 5, Status: ProcessConflict, Message: "conflict in x"},
			check: func(t *testing.T, st *fakeStorage, _ *fakeRedis) {
				if st.blocked[5] != "conflict in x" {
					t.Errorf("blocked[5] = %q, want %q", st.blocked[5], "conflict in x")
				}
			},
		},
		{
			name:   "test-failure park marks blocked",
			cfg:    WorkerConfig{Key: key, Escalation: EscalationConfig{OnTestFailure: "park"}},
			result: ProcessResult{Proposal: 5, Status: ProcessTestFailure, Message: "tests"},
			check: func(t *testing.T, st *fakeStorage, _ *fakeRedis) {
				if st.blocked[5] != "tests" {
					t.Errorf("park should mark blocked; blocked[5] = %q", st.blocked[5])
				}
			},
		},
		{
			name:   "test-failure notify marks failed",
			cfg:    WorkerConfig{Key: key, Escalation: EscalationConfig{OnTestFailure: "notify"}},
			result: ProcessResult{Proposal: 5, Status: ProcessTestFailure, Message: "tests"},
			check: func(t *testing.T, st *fakeStorage, _ *fakeRedis) {
				if st.failed[5] != "tests" {
					t.Errorf("notify should mark failed; failed[5] = %q", st.failed[5])
				}
			},
		},
		{
			name:   "error marks failed",
			cfg:    WorkerConfig{Key: key},
			result: ProcessResult{Proposal: 5, Status: ProcessError, Message: "boom"},
			check: func(t *testing.T, st *fakeStorage, _ *fakeRedis) {
				if st.failed[5] != "boom" {
					t.Errorf("failed[5] = %q, want %q", st.failed[5], "boom")
				}
			},
		},
		{
			name:   "noop marks completed quietly",
			cfg:    WorkerConfig{Key: key},
			result: ProcessResult{Proposal: 5, Status: ProcessNoop},
			check: func(t *testing.T, st *fakeStorage, rd *fakeRedis) {
				if len(st.completed) != 1 {
					t.Errorf("noop should mark completed; completed = %v", st.completed)
				}
				if v, _ := rd.Get(context.Background(), key.completedKey(5)); v != "" {
					t.Error("noop should NOT write a recently-landed marker")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStorage()
			rd := newFakeRedis()
			w := NewWorker(tt.cfg, WorkerDeps{Storage: st, Redis: rd})
			w.now = func() time.Time { return fixedTime }
			if err := w.handleResult(context.Background(), entry, tt.result); err != nil {
				t.Fatalf("handleResult: %v", err)
			}
			tt.check(t, st, rd)
		})
	}
}

// ---------------------------------------------------------------------------
// Start loop tests
// ---------------------------------------------------------------------------

func TestWorkerStartProcessesQueueThenStops(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	st := newFakeStorage()
	_ = st.Enqueue(context.Background(), Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 1, SourceBranch: "b1"})
	_ = st.Enqueue(context.Background(), Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 2, SourceBranch: "b2"})
	rd := newFakeRedis()

	stgy := &fakeStrategy{
		prepareResult: strategies.PrepareResult{Success: true},
		executeResult: strategies.MergeResult{Status: strategies.StatusSuccess},
	}
	cfg := WorkerConfig{Key: key, Strategy: "fake", PollInterval: time.Millisecond}
	w := newTestWorker(cfg, st, rd, stgy, &fakeResolver{}, &fakeLockHandler{}, &fakeRunner{})

	// Stop after the queue drains: override sleep to Stop on the first empty poll.
	w.sleep = func(context.Context, time.Duration) { w.Stop() }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(st.completed) != 2 {
		t.Errorf("completed = %v, want both proposals landed", st.completed)
	}
	// Lock released on shutdown.
	if v, _ := rd.Get(context.Background(), key.lockKey()); v != "" {
		t.Error("lock should be released after Start returns")
	}
}

func TestWorkerStartLockContention(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	rd := newFakeRedis()
	rd.lockHeld[key.lockKey()] = true // another worker holds the lock
	w := newTestWorker(WorkerConfig{Key: key, Strategy: "fake"}, newFakeStorage(), rd,
		&fakeStrategy{}, &fakeResolver{}, &fakeLockHandler{}, &fakeRunner{})

	err := w.Start(context.Background())
	if err == nil {
		t.Fatal("Start should error when the lock is held")
	}
}

func TestWorkerStartRejectsInvalidKey(t *testing.T) {
	w := NewWorker(WorkerConfig{Key: Key{RepoID: "owner/repo"}, Strategy: "fake"},
		WorkerDeps{Storage: newFakeStorage(), Redis: newFakeRedis()})
	if err := w.Start(context.Background()); err == nil {
		t.Error("Start with empty OrgID should error")
	}
}

func TestSleepCtxCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	sleepCtx(ctx, time.Hour) // should return immediately on cancelled ctx
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("sleepCtx ignored cancellation; slept %v", elapsed)
	}
}

// errReplyWorker is a sanity check that the shared errReply helper behaves.
func TestErrReplyHelper(t *testing.T) {
	r := errReply("git push", "boom", "ok")
	if _, err := r("git", []string{"push", "origin"}); err == nil || err.Error() != "boom" {
		t.Errorf("expected boom error, got %v", err)
	}
	if out, err := r("git", []string{"status"}); err != nil || out != "ok" {
		t.Errorf("expected (ok,nil), got (%q,%v)", out, err)
	}
	_ = errors.New("") // keep errors import used if helper changes
}
