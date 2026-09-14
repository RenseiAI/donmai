package afcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	providerstub "github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// deadlineProbeProvider wraps a provider and records the deadline of the
// context the runner hands to Spawn.
//
// Spawn is reached at runner/loop.go step 8, BEFORE the budget enforcer
// derives its own duration cap at step 10, so the deadline observed here is
// the RUN context's — the outer bound every other deadline in the session is
// derived from, and therefore the one this test is about. The embedded
// interface only promotes agent.Provider's method set, so Manifest is
// delegated explicitly to keep the wrapper an agent.HarnessProvider like the
// provider it wraps.
type deadlineProbeProvider struct {
	agent.Provider

	mu       sync.Mutex
	deadline time.Time
	seen     bool
}

func (p *deadlineProbeProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	deadline, ok := ctx.Deadline()
	p.mu.Lock()
	p.deadline, p.seen = deadline, ok
	p.mu.Unlock()
	return p.Provider.Spawn(ctx, spec)
}

func (p *deadlineProbeProvider) Manifest() agent.HarnessManifest {
	return p.Provider.(agent.HarnessProvider).Manifest()
}

// observed returns the recorded run-context deadline, and whether Spawn saw
// one at all (a runner with no cap hands over a deadline-free context).
func (p *deadlineProbeProvider) observed() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deadline, p.seen
}

// TestAgentRunDispatchedBudgetBoundsTheRunContext drives the whole production
// chain a daemon-spawned worker walks — daemon session-detail JSON →
// fetchSessionDetail → agentRunMaxSessionDuration → runner.Options →
// runner.Run's run context — and pins the run deadline to the DISPATCHED stage
// budget rather than runner.DefaultMaxSessionDuration.
//
// The budget is deliberately three hours: longer than the two-hour default, so
// a run context still cut at the default is unambiguously visible. Before the
// change this test is the red one — the derivation returned zero, the runner
// fell back to its two-hour default, and the budget enforcer could not widen
// it because context.WithDeadline only ever moves a deadline earlier.
func TestAgentRunDispatchedBudgetBoundsTheRunContext(t *testing.T) {
	const (
		budgetSeconds = 3 * 60 * 60
		budget        = time.Duration(budgetSeconds) * time.Second
		// Wall-clock slack for the clone + spawn the run performs between the
		// deadline being set and Spawn observing it.
		slack = 2 * time.Minute
	)
	if budget <= runner.DefaultMaxSessionDuration {
		t.Fatalf("fixture budget %s must exceed the runner default %s to be discriminating",
			budget, runner.DefaultMaxSessionDuration)
	}

	repository := makeSpecDecoratorBareRepo(t)
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(platform.Close)

	dispatched := &daemon.SessionDetail{
		SessionID:       "session-stage-budget",
		IssueID:         "issue-stage-budget",
		IssueIdentifier: "BUDGET-1",
		WorkType:        "development",
		ProjectName:     "TestProject",
		OrganizationID:  "org_test",
		Title:           "Stage budget bounds the run context",
		Body:            "Dispatched with a stage budget longer than the runner default.",
		Repository:      repository,
		WorkerID:        "worker-1",
		AuthToken:       "token", //nolint:gosec // G101: fixed test-only credential.
		PlatformURL:     platform.URL,
		ResolvedProfile: &daemon.SessionResolvedProfile{Provider: string(agent.ProviderStub)},
		StageID:         "development",
		StageBudget:     &daemon.PollStageBudget{MaxDurationSeconds: budgetSeconds},
	}

	// Fetch through the real daemon-control client so the budget has to
	// survive the wire shape (`stageBudget.maxDurationSeconds`), not just the
	// in-process struct.
	detailServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(dispatched) //nolint:gosec // fixed test-only credentials
	}))
	t.Cleanup(detailServer.Close)
	detail, err := fetchSessionDetail(t.Context(), detailServer.Client(), detailServer.URL, dispatched.SessionID, "")
	if err != nil {
		t.Fatalf("fetchSessionDetail: %v", err)
	}
	if detail.StageBudget == nil || detail.StageBudget.MaxDurationSeconds != budgetSeconds {
		t.Fatalf("session detail stage budget = %+v; want maxDurationSeconds=%d — "+
			"the budget is not visible at derivation time", detail.StageBudget, budgetSeconds)
	}

	queued, err := detailToQueuedWork(detail)
	if err != nil {
		t.Fatalf("detailToQueuedWork: %v", err)
	}
	if queued.StageBudget == nil || queued.StageBudget.MaxDurationSeconds != budgetSeconds {
		t.Fatalf("queued work stage budget = %+v; want maxDurationSeconds=%d",
			queued.StageBudget, budgetSeconds)
	}

	stub, err := providerstub.New()
	if err != nil {
		t.Fatalf("stub provider: %v", err)
	}
	probe := &deadlineProbeProvider{Provider: stub}
	registry := runner.NewRegistry()
	if err := registry.Register(probe); err != nil {
		t.Fatalf("register probe provider: %v", err)
	}
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: platform.URL, WorkerID: "worker-1", AuthToken: "token",
		HTTPClient: platform.Client(), BaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	run, err := runner.New(runner.Options{
		Registry: registry, WorktreeManager: manager, Poster: poster,
		HTTPClient: platform.Client(), Logger: quietLogger(),
		// The line under test: the worker derives the runner's session cap
		// from the session detail it was dispatched.
		MaxSessionDuration: agentRunMaxSessionDuration(detail),
		SkipBackstop:       true, SkipSteering: true, SkipPostSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	// The parent context deliberately carries NO deadline: a bounded parent
	// would clamp the run context and mask the value under test (context.
	// WithTimeout keeps the earlier of the two). The go test binary's own
	// -timeout is the backstop for a wedged run.
	ctx := t.Context()
	dispatchedAt := time.Now()
	res, err := run.Run(ctx, queued)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	deadline, seen := probe.observed()
	if !seen {
		t.Fatal("run context carried no deadline; want one bounded by the dispatched budget")
	}
	if got := deadline.Sub(dispatchedAt); got < budget-slack || got > budget+slack {
		t.Fatalf("run context deadline is %s after dispatch; want the dispatched budget %s (±%s). "+
			"A deadline of ~%s means the runner default truncated the budget",
			got.Round(time.Second), budget, slack, runner.DefaultMaxSessionDuration)
	}

	// The budget enforcer derives its duration cap from this same run context
	// (runner/loop.go step 10 → BudgetEnforcer.WithDurationCap), starting from
	// the run's own StartedAt. Assert its deadline lands at or inside the run
	// deadline: when it does, the enforcer's cap is the one that fires and the
	// breach is classified as a budget breach rather than a bare timeout.
	enforcerDeadline := time.UnixMilli(res.StartedAt).Add(budget)
	if enforcerDeadline.After(deadline) {
		t.Errorf("budget enforcer deadline %s is %s past the run deadline %s; "+
			"context.WithDeadline cannot widen a parent, so the cap would be truncated",
			enforcerDeadline, enforcerDeadline.Sub(deadline), deadline)
	}
}

// TestAgentRunWithoutBudgetKeepsTheRunnerDefault is the other half of the
// pair: work dispatched with no stage budget must still be bounded by
// runner.DefaultMaxSessionDuration, not left to run unbounded.
func TestAgentRunWithoutBudgetKeepsTheRunnerDefault(t *testing.T) {
	const slack = 2 * time.Minute

	repository := makeSpecDecoratorBareRepo(t)
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(platform.Close)

	detail := &daemon.SessionDetail{
		SessionID:       "session-no-budget",
		IssueID:         "issue-no-budget",
		IssueIdentifier: "BUDGET-2",
		WorkType:        "development",
		ProjectName:     "TestProject",
		OrganizationID:  "org_test",
		Title:           "No stage budget",
		Body:            "Dispatched without a stage budget.",
		Repository:      repository,
		WorkerID:        "worker-1",
		AuthToken:       "token", //nolint:gosec // G101: fixed test-only credential.
		PlatformURL:     platform.URL,
		ResolvedProfile: &daemon.SessionResolvedProfile{Provider: string(agent.ProviderStub)},
	}
	queued, err := detailToQueuedWork(detail)
	if err != nil {
		t.Fatalf("detailToQueuedWork: %v", err)
	}

	stub, err := providerstub.New()
	if err != nil {
		t.Fatalf("stub provider: %v", err)
	}
	probe := &deadlineProbeProvider{Provider: stub}
	registry := runner.NewRegistry()
	if err := registry.Register(probe); err != nil {
		t.Fatalf("register probe provider: %v", err)
	}
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: platform.URL, WorkerID: "worker-1", AuthToken: "token",
		HTTPClient: platform.Client(), BaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	run, err := runner.New(runner.Options{
		Registry: registry, WorktreeManager: manager, Poster: poster,
		HTTPClient: platform.Client(), Logger: quietLogger(),
		MaxSessionDuration: agentRunMaxSessionDuration(detail),
		SkipBackstop:       true, SkipSteering: true, SkipPostSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	// The parent context deliberately carries NO deadline: a bounded parent
	// would clamp the run context and mask the value under test (context.
	// WithTimeout keeps the earlier of the two). The go test binary's own
	// -timeout is the backstop for a wedged run.
	ctx := t.Context()
	dispatchedAt := time.Now()
	if _, err := run.Run(ctx, queued); err != nil {
		t.Fatalf("Run: %v", err)
	}

	deadline, seen := probe.observed()
	if !seen {
		t.Fatal("run context carried no deadline; want runner.DefaultMaxSessionDuration")
	}
	want := runner.DefaultMaxSessionDuration
	if got := deadline.Sub(dispatchedAt); got < want-slack || got > want+slack {
		t.Fatalf("run context deadline is %s after dispatch; want the runner default %s (±%s)",
			got.Round(time.Second), want, slack)
	}
}
