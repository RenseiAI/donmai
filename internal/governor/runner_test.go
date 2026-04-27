package governor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/internal/linear"
)

// ─── mock Linear ──────────────────────────────────────────────────────────────

type mockLinear struct {
	mu      sync.Mutex
	results map[string][]linear.Issue // project → issues
	err     map[string]error          // project → error
	// blockCh, if non-nil, is closed to unblock a slow call
	blockCh <-chan struct{}
}

func (m *mockLinear) ListIssuesByProject(ctx context.Context, project string, _ []string) ([]linear.Issue, error) {
	if m.blockCh != nil {
		select {
		case <-m.blockCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.err[project]; ok && err != nil {
		return nil, err
	}
	return m.results[project], nil
}

func (m *mockLinear) GetIssue(_ context.Context, _ string) (*linear.Issue, error) {
	return nil, nil
}

func (m *mockLinear) ListSubIssues(_ context.Context, _ string) ([]linear.Issue, error) {
	return nil, nil
}

// ─── mock Queue ───────────────────────────────────────────────────────────────

type mockQueue struct {
	mu       sync.Mutex
	enqueued [][]byte
	enqErr   error
	counters map[string]int64
}

func newMockQueue() *mockQueue {
	return &mockQueue{counters: make(map[string]int64)}
}

func (q *mockQueue) Ping(_ context.Context) error { return nil }

func (q *mockQueue) Enqueue(_ context.Context, payload []byte) error {
	if q.enqErr != nil {
		return q.enqErr
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueued = append(q.enqueued, payload)
	return nil
}

func (q *mockQueue) Peek(_ context.Context) ([]byte, error) { return nil, nil }

func (q *mockQueue) IncrDispatchCounter(_ context.Context, key string) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.counters[key]++
	return q.counters[key], nil
}

func (q *mockQueue) GetDispatchCounter(_ context.Context, key string) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.counters[key], nil
}

func (q *mockQueue) Close() error { return nil }

func (q *mockQueue) EnqueuedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.enqueued)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func makeIssue(id, state, project string) linear.Issue {
	return linear.Issue{
		ID:         id,
		Identifier: id,
		State: struct {
			Name string `json:"name"`
		}{Name: state},
		Project: struct {
			Name string `json:"name"`
		}{Name: project},
	}
}

func baseConfig() Config {
	return Config{
		Projects:            []string{"TestProject"},
		ScanInterval:        10 * time.Millisecond,
		MaxDispatches:       100,
		Once:                true,
		Mode:                ModePollOnly,
		AutoResearch:        true,
		AutoBacklogCreation: true,
		AutoDevelopment:     true,
		AutoQA:              true,
		AutoAcceptance:      true,
	}
}

// ─── tests ────────────────────────────────────────────────────────────────────

func TestNewRunner_InvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{"empty projects", Config{MaxDispatches: 1, ScanInterval: time.Second, Mode: ModePollOnly}},
		{"zero max dispatches", Config{Projects: []string{"p"}, ScanInterval: time.Second, Mode: ModePollOnly}},
		{"zero scan interval", Config{Projects: []string{"p"}, MaxDispatches: 1, Mode: ModePollOnly}},
		{"invalid mode", Config{Projects: []string{"p"}, MaxDispatches: 1, ScanInterval: time.Second, Mode: "bad"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRunner(tc.cfg, &mockLinear{}, newMockQueue(), nil)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestNewRunner_NilLoggerUsesDefault(t *testing.T) {
	t.Parallel()
	r, err := NewRunner(baseConfig(), &mockLinear{results: map[string][]linear.Issue{}}, newMockQueue(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.logger == nil {
		t.Fatal("logger should not be nil")
	}
}

func TestRun_Once_ThreeIssues(t *testing.T) {
	t.Parallel()

	issues := []linear.Issue{
		makeIssue("A-1", "Backlog", "TestProject"),
		makeIssue("A-2", "Backlog", "TestProject"),
		makeIssue("A-3", "Backlog", "TestProject"),
	}
	lin := &mockLinear{results: map[string][]linear.Issue{"TestProject": issues}}
	q := newMockQueue()

	cfg := baseConfig()
	r, err := NewRunner(cfg, lin, q, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := q.EnqueuedCount(); got != 3 {
		t.Errorf("enqueued = %d, want 3", got)
	}
}

func TestRun_MaxDispatchesCap(t *testing.T) {
	t.Parallel()

	issues := make([]linear.Issue, 5)
	for i := range issues {
		issues[i] = makeIssue("B-"+string(rune('1'+i)), "Backlog", "TestProject")
	}
	lin := &mockLinear{results: map[string][]linear.Issue{"TestProject": issues}}
	q := newMockQueue()

	cfg := baseConfig()
	cfg.MaxDispatches = 2

	r, err := NewRunner(cfg, lin, q, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := q.EnqueuedCount(); got != 2 {
		t.Errorf("enqueued = %d, want 2 (capped by MaxDispatches)", got)
	}
}

func TestRun_CtxCancellation(t *testing.T) {
	t.Parallel()

	// blockCh is used to block the Linear call until the ctx is cancelled.
	ctx, cancel := context.WithCancel(context.Background())

	blockCh := make(chan struct{})
	lin := &mockLinear{
		results: map[string][]linear.Issue{},
		blockCh: blockCh,
	}
	q := newMockQueue()

	cfg := baseConfig()
	cfg.Once = false
	cfg.ScanInterval = 5 * time.Millisecond

	r, err := NewRunner(cfg, lin, q, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	runDone := make(chan error, 1)
	go func() {
		// Wait for first tick then cancel before Linear unblocks.
		time.Sleep(15 * time.Millisecond)
		cancel()
		runDone <- r.Run(ctx)
	}()

	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

func TestRun_LinearErrorPerProject(t *testing.T) {
	t.Parallel()

	goodIssues := []linear.Issue{makeIssue("C-1", "Backlog", "Good")}
	lin := &mockLinear{
		results: map[string][]linear.Issue{"Good": goodIssues},
		err:     map[string]error{"Bad": errors.New("API error")},
	}
	q := newMockQueue()

	cfg := baseConfig()
	cfg.Projects = []string{"Bad", "Good"}

	r, err := NewRunner(cfg, lin, q, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	if err := r.Run(context.Background()); err != nil {
		t.Errorf("Run returned unexpected error: %v", err)
	}

	// Good project still dispatched.
	if got := q.EnqueuedCount(); got != 1 {
		t.Errorf("enqueued = %d, want 1 (only from Good project)", got)
	}
}

func TestRun_FeatureToggleGating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		state      string
		toggleOff  func(*Config)
		wantQueued int
	}{
		{
			name:  "AutoResearch off skips Triage",
			state: "Triage",
			toggleOff: func(c *Config) {
				c.AutoResearch = false
			},
			wantQueued: 0,
		},
		{
			name:  "AutoDevelopment off skips Backlog-with-project",
			state: "Backlog",
			toggleOff: func(c *Config) {
				c.AutoDevelopment = false
			},
			wantQueued: 0,
		},
		{
			name:  "AutoQA off skips Started",
			state: "Started",
			toggleOff: func(c *Config) {
				c.AutoQA = false
			},
			wantQueued: 0,
		},
		{
			name:  "AutoAcceptance off skips In Review",
			state: "In Review",
			toggleOff: func(c *Config) {
				c.AutoAcceptance = false
			},
			wantQueued: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			issues := []linear.Issue{makeIssue("D-1", tc.state, "TestProject")}
			lin := &mockLinear{results: map[string][]linear.Issue{"TestProject": issues}}
			q := newMockQueue()

			cfg := baseConfig()
			tc.toggleOff(&cfg)

			r, err := NewRunner(cfg, lin, q, nil)
			if err != nil {
				t.Fatalf("NewRunner: %v", err)
			}

			if err := r.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if got := q.EnqueuedCount(); got != tc.wantQueued {
				t.Errorf("enqueued = %d, want %d", got, tc.wantQueued)
			}
		})
	}
}

func TestConfig_ModeValidation(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Mode = "nonsense"

	_, err := NewRunner(cfg, &mockLinear{}, newMockQueue(), nil)
	if err == nil {
		t.Fatal("expected error for invalid mode, got nil")
	}
}

func TestRun_BothModes_Once(t *testing.T) {
	t.Parallel()

	modes := []Mode{ModePollOnly, ModeEventDriven}

	for _, mode := range modes {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			issues := []linear.Issue{makeIssue("E-1", "Backlog", "TestProject")}
			lin := &mockLinear{results: map[string][]linear.Issue{"TestProject": issues}}
			q := newMockQueue()

			cfg := baseConfig()
			cfg.Mode = mode

			r, err := NewRunner(cfg, lin, q, nil)
			if err != nil {
				t.Fatalf("NewRunner: %v", err)
			}

			if err := r.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if got := q.EnqueuedCount(); got != 1 {
				t.Errorf("mode %s: enqueued = %d, want 1", mode, got)
			}
		})
	}
}

// TestRun_TickerLoop verifies that the runner performs multiple scans when
// Once=false and ctx is cancelled after a few ticks.
func TestRun_TickerLoop(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int64

	lin := &mockLinear{}
	// Override ListIssuesByProject via a custom mock.
	customLin := &countingLinear{counter: &callCount, issues: []linear.Issue{}}
	q := newMockQueue()

	_ = lin // unused — use countingLinear instead

	cfg := baseConfig()
	cfg.Once = false
	cfg.ScanInterval = 10 * time.Millisecond
	cfg.Projects = []string{"TestProject"}

	r, err := NewRunner(cfg, customLin, q, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Millisecond)
	defer cancel()

	if err := r.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run returned %v, want DeadlineExceeded", err)
	}

	// With 10ms ticker and 55ms timeout we expect at least 3 scans.
	if n := callCount.Load(); n < 3 {
		t.Errorf("ListIssuesByProject called %d times, want ≥3", n)
	}
}

// countingLinear counts ListIssuesByProject calls.
type countingLinear struct {
	counter *atomic.Int64
	issues  []linear.Issue
}

func (c *countingLinear) ListIssuesByProject(ctx context.Context, _ string, _ []string) ([]linear.Issue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.counter.Add(1)
	return c.issues, nil
}

func (c *countingLinear) GetIssue(_ context.Context, _ string) (*linear.Issue, error) {
	return nil, nil
}

func (c *countingLinear) ListSubIssues(_ context.Context, _ string) ([]linear.Issue, error) {
	return nil, nil
}
