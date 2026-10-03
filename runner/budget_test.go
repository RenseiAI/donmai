package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	spanruntime "github.com/RenseiAI/donmai/runtime/span"
)

// TestBudgetEnforcer_DisabledWhenBudgetNil asserts that a nil
// StageBudget produces a no-op enforcer (legacy work path) — every
// Track* method returns nil, no caps, .Enabled()=false. This is the
// "cardinal rule 1: legacy paths stay working" check.
func TestBudgetEnforcer_DisabledWhenBudgetNil(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(nil, time.Now())
	if enf.Enabled() {
		t.Fatalf("expected disabled enforcer for nil budget")
	}
	// Construct a fake Task tool-use; should not trip anything.
	if err := enf.ObserveEvent(agent.ToolUseEvent{ToolName: "Task"}); err != nil {
		t.Fatalf("disabled enforcer should not return error on Task: %v", err)
	}
	if err := enf.ObserveEvent(agent.ResultEvent{Cost: &agent.CostData{InputTokens: 999_999_999}}); err != nil {
		t.Fatalf("disabled enforcer should not return error on huge cost: %v", err)
	}
	if err := enf.CheckDuration(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatalf("disabled enforcer should not return error on long duration: %v", err)
	}
	rep := enf.Report(time.Now())
	if rep == nil {
		t.Fatalf("expected non-nil report")
	}
	if rep.Enforced {
		t.Fatalf("expected Enforced=false on disabled enforcer")
	}
}

// TestBudgetEnforcer_SubAgentCap asserts that the (N+1)th Task tool
// invocation trips the max-sub-agents cap and returns a
// *BudgetExceededError.
func TestBudgetEnforcer_SubAgentCap(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxSubAgents: 2}, time.Now())
	for i := 0; i < 2; i++ {
		if err := enf.ObserveEvent(agent.ToolUseEvent{ToolName: "Task"}); err != nil {
			t.Fatalf("Task #%d should be within budget: %v", i+1, err)
		}
	}
	err := enf.ObserveEvent(agent.ToolUseEvent{ToolName: "Task"})
	if err == nil {
		t.Fatalf("expected BudgetExceededError on 3rd Task with cap=2")
	}
	if err.Cap != CapSubAgents {
		t.Fatalf("expected CapSubAgents, got %s", err.Cap)
	}
	rep := enf.Report(time.Now())
	if rep.CapBreached != CapSubAgents {
		t.Fatalf("expected report.CapBreached=CapSubAgents, got %s", rep.CapBreached)
	}
	if rep.ObservedSubAgents != 3 {
		t.Fatalf("expected ObservedSubAgents=3, got %d", rep.ObservedSubAgents)
	}
}

// TestBudgetEnforcer_TokensCap asserts that the cumulative token
// count crossing MaxTokens trips a CapTokens breach.
func TestBudgetEnforcer_TokensCap(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxTokens: 1000}, time.Now())

	// Two intermediate cost events sum to 700 — within budget.
	if err := enf.ObserveEvent(agent.ResultEvent{Cost: &agent.CostData{InputTokens: 300, OutputTokens: 100}}); err != nil {
		t.Fatalf("first ResultEvent should be within budget: %v", err)
	}
	if err := enf.ObserveEvent(agent.ResultEvent{Cost: &agent.CostData{InputTokens: 200, OutputTokens: 100}}); err != nil {
		t.Fatalf("second ResultEvent should be within budget: %v", err)
	}
	// Third event pushes total to 700 + 500 = 1200 → breach.
	err := enf.ObserveEvent(agent.ResultEvent{Cost: &agent.CostData{InputTokens: 300, OutputTokens: 200}})
	if err == nil {
		t.Fatalf("expected breach when total tokens > 1000")
	}
	if err.Cap != CapTokens {
		t.Fatalf("expected CapTokens, got %s", err.Cap)
	}
	rep := enf.Report(time.Now())
	if rep.ObservedTokens != 1200 {
		t.Fatalf("expected ObservedTokens=1200, got %d", rep.ObservedTokens)
	}
}

// callUsage is one harness-reported LLM call's usage.
func callUsage(in, out int64) agent.LlmCallEvent {
	return agent.LlmCallEvent{InputTokens: in, OutputTokens: out, UsageSource: agent.LlmUsageProvider}
}

// resultCost is a successful ResultEvent carrying the given cost.
func resultCost(in, out int64) agent.ResultEvent {
	return agent.ResultEvent{Success: true, Cost: &agent.CostData{InputTokens: in, OutputTokens: out}}
}

// TestBudgetEnforcer_RunningTotalResultCost pins that each token is counted
// once whatever a harness's ResultEvent cost means. The first two cases are
// recorded pi sessions: pi's ResultEvent carries the handle's running total,
// so a steered session's second ResultEvent repeats the first — summing both
// measured 2,782,193 tokens against a 1,500,000 cap for a session that spent
// 1,457,724 (the sum of its per-call usage). A single-ResultEvent session
// over the cap must still breach. The remaining cases pin that every other
// shape keeps its previous accounting. Each case runs on the raw harness
// events and again through the span processor, which is how consumeEvents
// feeds the enforcer.
func TestBudgetEnforcer_RunningTotalResultCost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		limit      int64
		events     []agent.Event
		wantTokens int64
		wantBreach bool
	}{
		{
			name:  "recorded pi steered session counts the running total once",
			limit: 1_500_000,
			events: []agent.Event{
				callUsage(600_000, 80_000), callUsage(565_980, 78_489),
				resultCost(1_165_980, 158_489),
				callUsage(50_000, 20_000), callUsage(37_002, 26_253),
				resultCost(1_252_982, 204_742),
			},
			wantTokens: 1_457_724,
		},
		{
			name:  "recorded pi single-result session over the cap still breaches",
			limit: 1_500_000,
			events: []agent.Event{
				callUsage(1_000_000, 30_000), callUsage(974_017, 38_390),
				resultCost(1_974_017, 68_390),
			},
			wantTokens: 2_042_407,
			wantBreach: true,
		},
		{
			name:  "per-invocation costs still sum",
			limit: 10_000,
			events: []agent.Event{
				callUsage(300, 100), resultCost(300, 100),
				callUsage(200, 100), resultCost(200, 100),
			},
			wantTokens: 700,
		},
		{
			// Equal totals with no per-call usage in between prove nothing
			// about a running total: both results count.
			name:       "equal results with no per-call usage both count",
			limit:      10_000,
			events:     []agent.Event{resultCost(100, 0), resultCost(100, 0)},
			wantTokens: 200,
		},
		{
			name:       "no per-call usage keeps summing results",
			limit:      10_000,
			events:     []agent.Event{resultCost(300, 100), resultCost(200, 100)},
			wantTokens: 700,
		},
		{
			name:  "aggregate usage derived from the result is not evidence",
			limit: 10_000,
			events: []agent.Event{
				agent.LlmCallEvent{InputTokens: 400, UsageSource: agent.LlmUsageAggregate, Synthetic: true},
				resultCost(400, 0),
				agent.LlmCallEvent{InputTokens: 700, UsageSource: agent.LlmUsageAggregate, Synthetic: true},
				resultCost(700, 0),
			},
			wantTokens: 1_100,
		},
		{
			name:  "a restarted running total counts whole",
			limit: 10_000,
			events: []agent.Event{
				callUsage(1_000, 0), resultCost(1_000, 0),
				callUsage(300, 0), resultCost(300, 0),
			},
			wantTokens: 1_300,
		},
		{
			name:  "a restarted total larger than the old one counts whole",
			limit: 10_000,
			events: []agent.Event{
				callUsage(300, 0), resultCost(300, 0),
				callUsage(500, 0), resultCost(500, 0),
			},
			wantTokens: 800,
		},
		{
			name:  "a cost-less result keeps the per-call evidence",
			limit: 10_000,
			events: []agent.Event{
				callUsage(100, 0), resultCost(100, 0),
				callUsage(50, 0),
				agent.ResultEvent{Success: true},
				callUsage(25, 0), resultCost(175, 0),
			},
			wantTokens: 175,
		},
		{
			name:  "per-call usage that does not reconcile counts the whole result",
			limit: 10_000,
			events: []agent.Event{
				callUsage(100, 0), resultCost(100, 0),
				callUsage(60, 0), resultCost(150, 0),
			},
			wantTokens: 250,
		},
	}
	for _, tc := range cases {
		for _, viaSpan := range []bool{false, true} {
			name := tc.name
			if viaSpan {
				name += " (through the span processor)"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				events := tc.events
				if viaSpan {
					processor, err := spanruntime.NewProcessor(spanruntime.ProcessorConfig{
						SessionID:   "budget-test",
						OrgID:       "org-test",
						WorkspaceID: "workspace-test",
						Sender:      spanruntime.SendFunc(func(agent.Span) bool { return true }),
					})
					if err != nil {
						t.Fatalf("NewProcessor: %v", err)
					}
					events = nil
					for _, ev := range tc.events {
						events = append(events, processor.Process(ev)...)
					}
				}
				enf := NewBudgetEnforcer(&prompt.StageBudget{MaxTokens: tc.limit}, time.Now())
				breached := false
				for _, ev := range events {
					if err := enf.ObserveEvent(ev); err != nil {
						breached = true
					}
				}
				rep := enf.Report(time.Now())
				if rep.ObservedTokens != tc.wantTokens {
					t.Errorf("ObservedTokens = %d, want %d", rep.ObservedTokens, tc.wantTokens)
				}
				// The reported cost is the same meter.
				if cost := enf.cost(); cost == nil || cost.InputTokens+cost.OutputTokens != tc.wantTokens {
					t.Errorf("cost = %+v, want %d tokens", cost, tc.wantTokens)
				}
				if breached != tc.wantBreach || (rep.CapBreached == CapTokens) != tc.wantBreach {
					t.Errorf("breach = %v (report cap %q), want %v", breached, rep.CapBreached, tc.wantBreach)
				}
			})
		}
	}
}

// TestBudgetEnforcer_TokenCapTripsMidTurn pins the live meter: the model
// call whose usage crosses the cap trips it, before the turn's ResultEvent;
// the turn's ResultEvent then reconciles the running total without counting
// it twice, and the breach keeps the crossing.
func TestBudgetEnforcer_TokenCapTripsMidTurn(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxTokens: 1_000}, time.Now())
	for _, ev := range []agent.Event{callUsage(500, 100), agent.ToolUseEvent{ToolName: "bash"}, callUsage(300, 50)} {
		if err := enf.ObserveEvent(ev); err != nil {
			t.Fatalf("within the cap at %T: %v", ev, err)
		}
	}
	err := enf.ObserveEvent(callUsage(100, 0))
	if err == nil || err.Cap != CapTokens || err.Detail != "max-tokens exceeded: observed=1050 limit=1000" {
		t.Fatalf("crossing call = %v; want the max-tokens breach at observed=1050", err)
	}
	if rep := enf.Report(time.Now()); rep.ObservedTokens != 1_050 || rep.CapBreached != CapTokens {
		t.Fatalf("Report = %+v; want 1050 observed and the max-tokens breach", rep)
	}
	if err := enf.ObserveEvent(resultCost(900, 150)); err == nil {
		t.Fatal("the turn's ResultEvent is still over the cap; want the breach again")
	}
	if rep := enf.Report(time.Now()); rep.ObservedTokens != 1_050 || rep.BreachDetail != "max-tokens exceeded: observed=1050 limit=1000" {
		t.Fatalf("Report = %+v; want the running total counted once and the crossing kept", rep)
	}
}

// TestBudgetEnforcer_WrapUpPoint pins the wrap-up point: due once the meter
// reaches four fifths of the token cap, whether a model call or a turn's
// result moves it there, until the request is sent; never without a token cap.
func TestBudgetEnforcer_WrapUpPoint(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxTokens: 1_000}, time.Now())
	_ = enf.ObserveEvent(callUsage(799, 0))
	if enf.wrapUpDue() {
		t.Fatal("wrap-up due at 799 of 1000 tokens")
	}
	_ = enf.ObserveEvent(callUsage(1, 0))
	if !enf.wrapUpDue() {
		t.Fatal("wrap-up not due at 800 of 1000 tokens")
	}
	enf.wrapUpSent()
	_ = enf.ObserveEvent(callUsage(50, 0))
	if enf.wrapUpDue() {
		t.Fatal("wrap-up due again after it was sent")
	}

	byResult := NewBudgetEnforcer(&prompt.StageBudget{MaxTokens: 1_000}, time.Now())
	_ = byResult.ObserveEvent(resultCost(850, 0))
	if !byResult.wrapUpDue() {
		t.Fatal("wrap-up not due after a result of 850 of 1000 tokens")
	}

	for _, b := range []*prompt.StageBudget{nil, {MaxDurationSeconds: 60}} {
		enf := NewBudgetEnforcer(b, time.Now())
		_ = enf.ObserveEvent(callUsage(1_000_000, 0))
		if enf.wrapUpDue() {
			t.Errorf("wrap-up due with budget %+v; there is no token cap", b)
		}
	}
}

// TestBudgetEnforcer_CarriesEveryTokenClass pins that cache-write and
// reasoning tokens ride the meter into the reported cost: per-call usage
// accumulates them, a running-total ResultEvent reconciles them by
// difference, and addCost sums them — while the token-cap meter still
// counts input+output only.
func TestBudgetEnforcer_CarriesEveryTokenClass(t *testing.T) {
	t.Parallel()
	turn := func(in, out, cached, written, reasoning int64) agent.LlmCallEvent {
		return agent.LlmCallEvent{
			InputTokens: in, OutputTokens: out, CachedInputTokens: cached,
			CacheWriteTokens: written, ReasoningTokens: reasoning,
			UsageSource: agent.LlmUsageProvider,
		}
	}
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxTokens: 1_000_000}, time.Now())
	// First turn: per-call usage accumulates every class into pending.
	if err := enf.ObserveEvent(turn(5, 200, 16526, 1200, 40)); err != nil {
		t.Fatalf("ObserveEvent(call): %v", err)
	}
	got := enf.cost()
	want := agent.CostData{
		InputTokens: 5, OutputTokens: 200, CachedInputTokens: 16526,
		CacheWriteTokens: 1200, ReasoningTokens: 40, NumTurns: 1,
	}
	if got == nil || *got != want {
		t.Fatalf("per-call cost = %+v; want %+v", got, want)
	}
	// The turn's ResultEvent reports the running total; then a second
	// turn's running total reconciles against it by difference per class.
	first := &agent.CostData{
		InputTokens: 5, OutputTokens: 200, CachedInputTokens: 16526,
		CacheWriteTokens: 1200, ReasoningTokens: 40, NumTurns: 1,
	}
	if err := enf.ObserveEvent(agent.ResultEvent{Success: true, Cost: first}); err != nil {
		t.Fatalf("ObserveEvent(first result): %v", err)
	}
	if err := enf.ObserveEvent(turn(5, 200, 0, 1200, 40)); err != nil {
		t.Fatalf("ObserveEvent(call): %v", err)
	}
	second := &agent.CostData{
		InputTokens: 10, OutputTokens: 400, CachedInputTokens: 16526,
		CacheWriteTokens: 2400, ReasoningTokens: 80, NumTurns: 2,
	}
	if err := enf.ObserveEvent(agent.ResultEvent{Success: true, Cost: second}); err != nil {
		t.Fatalf("ObserveEvent(second result): %v", err)
	}
	got = enf.cost()
	want = agent.CostData{
		InputTokens: 10, OutputTokens: 400, CachedInputTokens: 16526,
		CacheWriteTokens: 2400, ReasoningTokens: 80, NumTurns: 2,
	}
	if got == nil || *got != want {
		t.Fatalf("reconciled cost = %+v; want %+v", got, want)
	}
	if rep := enf.Report(time.Now()); rep.ObservedTokens != 410 {
		t.Fatalf("ObservedTokens = %d; want 410 (input+output only)", rep.ObservedTokens)
	}
}

// TestBudgetEnforcer_MetersWithoutABudget pins that the usage meter runs for
// every session — the reported cost comes from it — while a session with no
// budget is never breached.
func TestBudgetEnforcer_MetersWithoutABudget(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(nil, time.Now())
	for _, ev := range []agent.Event{
		agent.ResultEvent{Success: true, Cost: &agent.CostData{InputTokens: 300, OutputTokens: 100, CachedInputTokens: 20, TotalCostUsd: 0.5, NumTurns: 3}},
		callUsage(40, 10),
	} {
		if err := enf.ObserveEvent(ev); err != nil {
			t.Fatalf("no budget, no breach: %v", err)
		}
	}
	want := agent.CostData{InputTokens: 340, OutputTokens: 110, CachedInputTokens: 20, TotalCostUsd: 0.5, NumTurns: 4}
	if got := enf.cost(); got == nil || *got != want {
		t.Fatalf("cost = %+v; want %+v", got, want)
	}
	if rep := enf.Report(time.Now()); rep.ObservedTokens != 450 || rep.Enforced || rep.CapBreached != "" {
		t.Fatalf("Report = %+v; want 450 observed, not enforced, no breach", rep)
	}
	if NewBudgetEnforcer(nil, time.Now()).cost() != nil {
		t.Fatal("cost of a session that metered nothing; want nil")
	}
}

// TestBudgetEnforcer_DurationCap asserts that CheckDuration trips
// when wall-clock exceeds the cap.
func TestBudgetEnforcer_DurationCap(t *testing.T) {
	t.Parallel()
	startedAt := time.Now()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxDurationSeconds: 60}, startedAt)
	// Within budget at 30s.
	if err := enf.CheckDuration(startedAt.Add(30 * time.Second)); err != nil {
		t.Fatalf("expected no breach at 30s of 60s budget: %v", err)
	}
	// Breach at 90s.
	err := enf.CheckDuration(startedAt.Add(90 * time.Second))
	if err == nil {
		t.Fatalf("expected breach at 90s of 60s budget")
	}
	if err.Cap != CapDuration {
		t.Fatalf("expected CapDuration, got %s", err.Cap)
	}
}

// TestBudgetEnforcer_WithDurationCap_DerivedContext asserts that the
// derived context fires its Done channel after the cap elapses (in
// real time — using a tiny cap so the test runs quickly).
func TestBudgetEnforcer_WithDurationCap_DerivedContext(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxDurationSeconds: 1}, time.Now())
	parent := context.Background()
	ctx, cancel := enf.WithDurationCap(parent)
	defer cancel()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", ctx.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("derived ctx never fired Done within 3s for 1s budget")
	}
}

// TestBudgetEnforcer_WithDurationCap_NoCapPassesThroughCancel asserts
// that without a duration cap the derived context still propagates
// the parent cancellation (i.e. callers can defer cancel safely).
func TestBudgetEnforcer_WithDurationCap_NoCapPassesThroughCancel(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxSubAgents: 5}, time.Now())
	parent, parentCancel := context.WithCancel(context.Background())
	ctx, cancel := enf.WithDurationCap(parent)
	defer cancel()

	parentCancel()
	select {
	case <-ctx.Done():
		// expected
	case <-time.After(time.Second):
		t.Fatalf("derived ctx did not propagate parent cancel")
	}
}

// TestBudgetEnforcer_NamespacedTaskToolCounts asserts that MCP-namespaced
// Task tool names (e.g. "mcp__af__Task") count toward the sub-agent cap.
func TestBudgetEnforcer_NamespacedTaskToolCounts(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxSubAgents: 1}, time.Now())
	if err := enf.ObserveEvent(agent.ToolUseEvent{ToolName: "Task"}); err != nil {
		t.Fatalf("first Task should pass: %v", err)
	}
	err := enf.ObserveEvent(agent.ToolUseEvent{ToolName: "mcp__af__Task"})
	if err == nil {
		t.Fatalf("expected breach on namespaced Task")
	}
}

// TestIsBudgetExceeded sanity-checks the helper.
func TestIsBudgetExceeded(t *testing.T) {
	t.Parallel()
	if IsBudgetExceeded(nil) {
		t.Fatalf("nil error should be false")
	}
	if IsBudgetExceeded(errors.New("plain")) {
		t.Fatalf("plain error should be false")
	}
	if !IsBudgetExceeded(&BudgetExceededError{Cap: CapTokens, Detail: "x"}) {
		t.Fatalf("BudgetExceededError should be detected")
	}
}
