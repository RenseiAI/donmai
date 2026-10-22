package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	spanruntime "github.com/RenseiAI/donmai/runtime/span"
	"github.com/RenseiAI/donmai/runtime/worktree"
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
	// A sub-agent lifecycle event on a disabled enforcer trips nothing.
	if err := enf.ObserveEvent(agent.SubagentEvent{ToolName: "Task", ToolUseID: "t1", Phase: agent.SubagentStarted}); err != nil {
		t.Fatalf("disabled enforcer should not return error on subagent start: %v", err)
	}
	// A plain tool-use never counts as a sub-agent.
	if err := enf.ObserveEvent(agent.ToolUseEvent{ToolName: "Task", ToolUseID: "t1"}); err != nil {
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

// TestBudgetEnforcer_SubAgentCap asserts that the (N+1)th sub-agent
// lifecycle start trips the max-sub-agents cap and returns a
// *BudgetExceededError.
func TestBudgetEnforcer_SubAgentCap(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxSubAgents: subAgentCap(2)}, time.Now())
	for i := 0; i < 2; i++ {
		if err := enf.ObserveEvent(agent.SubagentEvent{ToolName: "Task", ToolUseID: "t" + string(rune('0'+i)), Phase: agent.SubagentStarted}); err != nil {
			t.Fatalf("subagent #%d should be within budget: %v", i+1, err)
		}
	}
	err := enf.ObserveEvent(agent.SubagentEvent{ToolName: "Task", ToolUseID: "t2", Phase: agent.SubagentStarted})
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
// difference, and addCost sums them — while the token-cap meter counts
// input + output plus cache reads at a tenth, never cache writes or
// reasoning.
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
	// 10 input + 400 output + 16,526 cache reads / 10 = 2,062; the 2,400
	// cache writes and 80 reasoning tokens do not move the meter.
	if rep := enf.Report(time.Now()); rep.ObservedTokens != 2_062 {
		t.Fatalf("ObservedTokens = %d; want 2062 (input + output + cache reads / 10)", rep.ObservedTokens)
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
	// 340 input + 110 output + 20 cache reads / 10.
	if rep := enf.Report(time.Now()); rep.ObservedTokens != 452 || rep.Enforced || rep.CapBreached != "" {
		t.Fatalf("Report = %+v; want 452 observed, not enforced, no breach", rep)
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
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxSubAgents: subAgentCap(5)}, time.Now())
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

// TestBudgetEnforcer_SubAgentTerminalPhasesDoNotCount asserts that only
// the `started` phase counts toward the sub-agent cap: terminal phases
// (completed/failed) and plain tool-use calls are observations, not new
// delegations.
func TestBudgetEnforcer_SubAgentTerminalPhasesDoNotCount(t *testing.T) {
	t.Parallel()
	enf := NewBudgetEnforcer(&prompt.StageBudget{MaxSubAgents: subAgentCap(1)}, time.Now())
	if err := enf.ObserveEvent(agent.SubagentEvent{ToolName: "Task", ToolUseID: "t1", Phase: agent.SubagentStarted}); err != nil {
		t.Fatalf("first start should pass: %v", err)
	}
	if err := enf.ObserveEvent(agent.SubagentEvent{ToolName: "Task", ToolUseID: "t1", Phase: agent.SubagentCompleted}); err != nil {
		t.Fatalf("completed phase should not breach: %v", err)
	}
	if err := enf.ObserveEvent(agent.SubagentEvent{ToolName: "Task", ToolUseID: "t1", Phase: agent.SubagentFailed}); err != nil {
		t.Fatalf("failed phase should not breach: %v", err)
	}
	if err := enf.ObserveEvent(agent.ToolUseEvent{ToolName: "Task", ToolUseID: "t1"}); err != nil {
		t.Fatalf("plain tool-use should not breach: %v", err)
	}
	rep := enf.Report(time.Now())
	if rep.ObservedSubAgents != 1 {
		t.Fatalf("expected ObservedSubAgents=1, got %d", rep.ObservedSubAgents)
	}
}

// TestBudgetEnforcer_SubAgentCapTable is the table-driven sub-agent
// budget test over the typed lifecycle event: caps of 0 (explicit
// "none"), 2 (third start breaches), and unset (nil, not enforced).
// Only `started` phases count; the adapter names its own delegation
// tools, so tool names are opaque labels here.
func TestBudgetEnforcer_SubAgentCapTable(t *testing.T) {
	t.Parallel()
	zero := 0
	two := 2
	cases := []struct {
		name      string
		cap       *int
		phases    []agent.SubagentPhase
		calls     int
		wantErrAt int // 1-based call index that must breach; 0 = no breach
	}{
		{name: "cap 0 first start", cap: &zero, phases: []agent.SubagentPhase{agent.SubagentStarted}, calls: 1, wantErrAt: 1},
		{name: "cap 0 terminal first", cap: &zero, phases: []agent.SubagentPhase{agent.SubagentCompleted}, calls: 1, wantErrAt: 0},
		{name: "cap 2 third start", cap: &two, phases: []agent.SubagentPhase{agent.SubagentStarted}, calls: 3, wantErrAt: 3},
		{name: "cap 2 mixed phases", cap: &two, phases: []agent.SubagentPhase{agent.SubagentStarted, agent.SubagentCompleted, agent.SubagentStarted, agent.SubagentFailed, agent.SubagentStarted}, calls: 5, wantErrAt: 5},
		{name: "unset starts unbounded", cap: nil, phases: []agent.SubagentPhase{agent.SubagentStarted}, calls: 3, wantErrAt: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enf := NewBudgetEnforcer(&prompt.StageBudget{MaxSubAgents: tc.cap}, time.Now())
			for i := 0; i < tc.calls; i++ {
				phase := tc.phases[i%len(tc.phases)]
				err := enf.ObserveEvent(agent.SubagentEvent{ToolName: "Task", ToolUseID: "t" + string(rune('0'+i)), Phase: phase})
				want := i+1 == tc.wantErrAt
				// Once breached the enforcer keeps reporting the
				// breach on later calls; any call at or past the
				// breach point must error.
				if tc.wantErrAt > 0 && i+1 >= tc.wantErrAt {
					want = true
				}
				if want && err == nil {
					t.Fatalf("call #%d (%s) should breach: cap=%v", i+1, phase, intCapForTest(tc.cap))
				}
				if !want && err != nil {
					t.Fatalf("call #%d (%s) should be within budget: cap=%v err=%v", i+1, phase, intCapForTest(tc.cap), err)
				}
				if err != nil && err.Cap != CapSubAgents {
					t.Fatalf("expected CapSubAgents, got %s", err.Cap)
				}
			}
			rep := enf.Report(time.Now())
			wantObserved := 0
			for i := 0; i < tc.calls; i++ {
				if tc.phases[i%len(tc.phases)] == agent.SubagentStarted {
					wantObserved++
				}
			}
			if rep.ObservedSubAgents != wantObserved {
				t.Fatalf("expected ObservedSubAgents=%d, got %d", wantObserved, rep.ObservedSubAgents)
			}
			if tc.wantErrAt > 0 && rep.CapBreached != CapSubAgents {
				t.Fatalf("expected report.CapBreached=CapSubAgents, got %s", rep.CapBreached)
			}
			if tc.wantErrAt == 0 && rep.CapBreached != "" {
				t.Fatalf("expected no breach with unset cap, got %s", rep.CapBreached)
			}
		})
	}
}

// intCapForTest renders a nil cap as "unset" for failure messages.
func intCapForTest(limit *int) string {
	if limit == nil {
		return "unset"
	}
	return strings.TrimSpace(strings.Join([]string{fmt.Sprint(*limit)}, " "))
}

// TestBudgetEnforcer_SubAgentCapDecoding pins the wire distinction the
// enforcer depends on: a stageBudget that omits maxSubAgents decodes to a
// nil cap (not enforced), while an explicit `"maxSubAgents": 0`
// decodes to a non-nil zero (no sub-agents allowed).
func TestBudgetEnforcer_SubAgentCapDecoding(t *testing.T) {
	t.Parallel()
	var omitted prompt.StageBudget
	if err := json.Unmarshal([]byte(`{}`), &omitted); err != nil {
		t.Fatalf("unmarshal omitted cap: %v", err)
	}
	if omitted.MaxSubAgents != nil {
		t.Fatalf("omitted maxSubAgents = %v; want nil (not enforced)", *omitted.MaxSubAgents)
	}
	var explicit prompt.StageBudget
	if err := json.Unmarshal([]byte(`{"maxSubAgents": 0}`), &explicit); err != nil {
		t.Fatalf("unmarshal explicit zero cap: %v", err)
	}
	if explicit.MaxSubAgents == nil || *explicit.MaxSubAgents != 0 {
		t.Fatalf("explicit maxSubAgents:0 = %+v; want non-nil zero (none allowed)", explicit.MaxSubAgents)
	}
	// The explicit zero breaches on the first sub-agent start; the
	// omitted cap never breaches.
	if err := NewBudgetEnforcer(&explicit, time.Now()).ObserveEvent(agent.SubagentEvent{ToolName: "Agent", ToolUseID: "t1", Phase: agent.SubagentStarted}); err == nil || err.Cap != CapSubAgents {
		t.Fatalf("explicit zero cap with subagent start = %v; want the max-sub-agents breach", err)
	}
	if err := NewBudgetEnforcer(&prompt.StageBudget{}, time.Now()).ObserveEvent(agent.SubagentEvent{ToolName: "Agent", ToolUseID: "t1", Phase: agent.SubagentStarted}); err != nil {
		t.Fatalf("omitted cap with subagent start = %v; want no breach", err)
	}
}

// subAgentCap returns a pointer to n for StageBudget.MaxSubAgents
// literals in tests.
func subAgentCap(n int) *int { return &n }

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

// TestBudgetEnforcer_CacheReadsMeterAtATenth pins the token cap's weight for
// cache reads. A harness that reports its cached prefix as cache reads, not
// as input (pi, once its cache buckets are mapped), would otherwise read ten
// times the context before the cap noticed; each cache-read token therefore
// counts a tenth toward the cap, on the live per-call meter and on the
// reconciled ResultEvent meter alike. Cache writes never move it.
func TestBudgetEnforcer_CacheReadsMeterAtATenth(t *testing.T) {
	t.Parallel()
	call := func(in, out, cached, written int64) agent.LlmCallEvent {
		return agent.LlmCallEvent{
			InputTokens: in, OutputTokens: out, CachedInputTokens: cached, CacheWriteTokens: written,
			UsageSource: agent.LlmUsageProvider,
		}
	}
	cases := []struct {
		name       string
		events     []agent.Event
		wantTokens int64
		wantBreach string
	}{
		{
			name:       "cache reads at a tenth stay within the cap",
			events:     []agent.Event{call(100, 0, 9_000, 0)},
			wantTokens: 1_000,
		},
		{
			name:       "cache reads alone can cross the cap",
			events:     []agent.Event{call(100, 0, 9_000, 0), call(0, 0, 20, 0)},
			wantTokens: 1_002,
			wantBreach: "max-tokens exceeded: observed=1002 limit=1000",
		},
		{
			name:       "cache writes never move the meter",
			events:     []agent.Event{call(100, 50, 0, 50_000)},
			wantTokens: 150,
		},
		{
			name: "a running-total result meters its cache reads once",
			events: []agent.Event{
				call(100, 10, 4_000, 0),
				agent.ResultEvent{Success: true, Cost: &agent.CostData{InputTokens: 100, OutputTokens: 10, CachedInputTokens: 4_000, NumTurns: 1}},
				call(100, 10, 4_000, 0),
				agent.ResultEvent{Success: true, Cost: &agent.CostData{InputTokens: 200, OutputTokens: 20, CachedInputTokens: 8_000, NumTurns: 2}},
			},
			wantTokens: 1_020,
			wantBreach: "max-tokens exceeded: observed=1020 limit=1000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enf := NewBudgetEnforcer(&prompt.StageBudget{MaxTokens: 1_000}, time.Now())
			var breach *BudgetExceededError
			for _, ev := range tc.events {
				if err := enf.ObserveEvent(ev); err != nil && breach == nil {
					breach = err
				}
			}
			rep := enf.Report(time.Now())
			if rep.ObservedTokens != tc.wantTokens {
				t.Errorf("ObservedTokens = %d; want %d", rep.ObservedTokens, tc.wantTokens)
			}
			switch {
			case tc.wantBreach == "" && breach != nil:
				t.Errorf("breach = %v; want none", breach)
			case tc.wantBreach != "" && (breach == nil || breach.Detail != tc.wantBreach):
				t.Errorf("breach = %v; want %q", breach, tc.wantBreach)
			}
		})
	}
}

// TestBudgetDuration_Boundary pins the shared seconds-to-duration clamp
// every budget path converts through: non-positive values yield no cap,
// values up to the ceiling pass through, and values at or past the
// ceiling — including ones that would overflow time.Duration — clamp to
// the ceiling instead of wrapping negative (unbounded).
func TestBudgetDuration_Boundary(t *testing.T) {
	t.Parallel()
	ceilingSeconds := int(MaxSessionDurationCeiling / time.Second)
	cases := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{name: "zero is no cap", seconds: 0, want: 0},
		{name: "negative is no cap", seconds: -1, want: 0},
		{name: "one second passes through", seconds: 1, want: time.Second},
		{name: "ceiling minus one passes through", seconds: ceilingSeconds - 1, want: time.Duration(ceilingSeconds-1) * time.Second},
		{name: "ceiling clamps", seconds: ceilingSeconds, want: MaxSessionDurationCeiling},
		{name: "ceiling plus one clamps", seconds: ceilingSeconds + 1, want: MaxSessionDurationCeiling},
		{name: "max int clamps", seconds: math.MaxInt, want: MaxSessionDurationCeiling},
		{name: "max int32 clamps", seconds: math.MaxInt32, want: MaxSessionDurationCeiling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BudgetDuration(tc.seconds); got != tc.want {
				t.Fatalf("BudgetDuration(%d) = %s, want %s", tc.seconds, got, tc.want)
			}
			if got := BudgetDuration(tc.seconds); got < 0 {
				t.Fatalf("BudgetDuration(%d) = %s; must never wrap negative", tc.seconds, got)
			}
		})
	}
}

// TestBudgetEnforcer_OverflowBoundaryStaysBounded pins that both
// duration paths — the derived stream context and the wall-clock
// classification — agree on a bounded session at the overflow boundary.
// Before the shared clamp, the enforcer multiplied the raw seconds and
// wrapped: a value near the boundary died immediately as
// budget-exceeded, and one past it dropped the cap while the run context
// kept its ceiling.
func TestBudgetEnforcer_OverflowBoundaryStaysBounded(t *testing.T) {
	t.Parallel()
	ceilingSeconds := int(MaxSessionDurationCeiling / time.Second)
	for _, seconds := range []int{ceilingSeconds - 1, ceilingSeconds, ceilingSeconds + 1, math.MaxInt32, math.MaxInt} {
		startedAt := time.Now()
		enf := NewBudgetEnforcer(&prompt.StageBudget{MaxDurationSeconds: seconds}, startedAt)
		// The stream cap fires at the bounded deadline, never in the past
		// and never unbounded: within the session at the ceiling.
		ctx, cancel := enf.WithDurationCap(context.Background())
		deadline, ok := ctx.Deadline()
		cancel()
		if !ok {
			t.Fatalf("seconds=%d: derived context carries no deadline; want the bounded cap", seconds)
		}
		if want := startedAt.Add(MaxSessionDurationCeiling); deadline.After(want.Add(time.Second)) || deadline.Before(startedAt) {
			t.Fatalf("seconds=%d: derived deadline %s, want within [%s, %s]", seconds, deadline, startedAt, want)
		}
		// Classification agrees: not breached at half the ceiling,
		// breached as the duration cap past it.
		if err := enf.CheckDuration(startedAt.Add(MaxSessionDurationCeiling / 2)); err != nil {
			t.Fatalf("seconds=%d: breached at half the ceiling: %v", seconds, err)
		}
		err := enf.CheckDuration(startedAt.Add(MaxSessionDurationCeiling).Add(time.Second))
		if err == nil || err.Cap != CapDuration {
			t.Fatalf("seconds=%d: CheckDuration past the ceiling = %v; want the duration-cap breach", seconds, err)
		}
		// The breach detail reports the clamped limit, never the raw
		// dispatched seconds: a 30 h budget breaching at 24 h must not
		// read observed=86400s limit=108000s.
		wantLimit := ceilingSeconds
		if seconds < ceilingSeconds {
			wantLimit = seconds
		}
		if !strings.Contains(err.Detail, fmt.Sprintf("limit=%ds", wantLimit)) {
			t.Fatalf("seconds=%d: breach detail %q; want limit=%ds", seconds, err.Detail, wantLimit)
		}
		if seconds > ceilingSeconds && !strings.Contains(err.Detail, "clamped") {
			t.Fatalf("seconds=%d: breach detail %q; want the clamp noted", seconds, err.Detail)
		}
	}
}

// delayProvisionRunner builds a Runner whose worktree provision blocks in
// the manager's pre-provision delay seam, so a run context whose deadline
// fires first fails provisioning with context.DeadlineExceeded — the same
// shape production produces when the budget-derived run deadline lapses
// mid-clone (the worker runs with the dispatched budget as its run-context
// deadline). An already-expired ctx returns from the seam immediately, so
// the test stays fast and deterministic.
func delayProvisionRunner(t *testing.T, platformURL string, client *http.Client, p agent.Provider) *Runner {
	t.Helper()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), PreProvisionDelay: 30 * time.Second})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	return newFollowUpRunner(t, platformURL, client, p, func(o *Options) {
		o.WorktreeManager = wtm
	})
}

// TestRun_PreStreamBudgetExpiryReportsBudgetExceeded pins that a budget
// already elapsed before the agent stream starts maps to the
// budget-exceeded failure, not a provisioning failure — but only when the
// provision actually died on the deadline. The production worker runs with
// the dispatched budget as its run-context deadline, so a budget that
// lapses mid-provision surfaces as a context-deadline error; the delay
// seam above reproduces that shape deterministically. A provision failure
// with time still on the budget, a non-deadline failure (bad remote) with
// an elapsed clock, and an interactive session (which runs with no
// duration ceiling) all keep the provisioning classification.
func TestRun_PreStreamBudgetExpiryReportsBudgetExceeded(t *testing.T) {
	// A budget that lapses mid-provision reports budget-exceeded: the
	// provision died on the run-context deadline while the runner clock
	// reads past the one-second budget.
	t.Run("expired budget on a deadline-cut provision", func(t *testing.T) {
		start := time.Now()
		platform := newRecordingPlatformServer(t)
		base, err := stub.New()
		if err != nil {
			t.Fatalf("stub.New: %v", err)
		}
		r := delayProvisionRunner(t, platform.URL, platform.Client(), base)
		// Advance the clock past the one-second budget for every read
		// after the run's own start stamp: the enforcer starts at run
		// start and the provision branch reads the advanced clock.
		calls := 0
		r.now = func() time.Time {
			calls++
			if calls == 1 {
				return start
			}
			return start.Add(2 * time.Second)
		}
		qw := QueuedWork{
			QueuedWork:      queuedWorkBase("PRESTREAM-EXPIRED"),
			WorkerID:        "worker-1",
			AuthToken:       "tok",
			PlatformURL:     platform.URL,
			ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
		}
		qw.StageBudget = &prompt.StageBudget{MaxDurationSeconds: 1}
		// Already expired: the delay seam returns context.DeadlineExceeded
		// without materializing anything.
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		res, _ := r.Run(ctx, qw)
		if res.Status != "failed" || res.FailureMode != FailureBudgetExceeded {
			t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureBudgetExceeded)
		}
		if res.BudgetBreach == nil || res.BudgetBreach.Cap != string(CapDuration) {
			t.Fatalf("BudgetBreach = %+v; want the duration-cap breach", res.BudgetBreach)
		}
		if res.BudgetReport == nil || res.BudgetReport.CapBreached != CapDuration {
			t.Fatalf("BudgetReport = %+v; want CapBreached %s", res.BudgetReport, CapDuration)
		}
		if res.WorktreePath != "" {
			t.Errorf("WorktreePath = %q; the workarea never materialized", res.WorktreePath)
		}
	})

	// The control: the same deadline-cut provision with time left on the
	// budget keeps the provisioning classification.
	t.Run("live budget on a deadline-cut provision", func(t *testing.T) {
		platform := newRecordingPlatformServer(t)
		base, err := stub.New()
		if err != nil {
			t.Fatalf("stub.New: %v", err)
		}
		r := delayProvisionRunner(t, platform.URL, platform.Client(), base)
		qw := QueuedWork{
			QueuedWork:      queuedWorkBase("PRESTREAM-LIVE"),
			WorkerID:        "worker-1",
			AuthToken:       "tok",
			PlatformURL:     platform.URL,
			ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
		}
		qw.StageBudget = &prompt.StageBudget{MaxDurationSeconds: 3600}
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		res, _ := r.Run(ctx, qw)
		if res.Status != "failed" || res.FailureMode != FailureWorktreeProvision {
			t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureWorktreeProvision)
		}
		if res.BudgetBreach != nil {
			t.Fatalf("BudgetBreach = %+v; a live budget must not record a breach", res.BudgetBreach)
		}
	})

	// A genuine misconfiguration that surfaces after the clock passes the
	// budget keeps its real cause: the clone error is not a deadline, so
	// the budget classification must not swallow it.
	t.Run("expired budget on a non-deadline provision failure", func(t *testing.T) {
		start := time.Now()
		platform := newRecordingPlatformServer(t)
		base, err := stub.New()
		if err != nil {
			t.Fatalf("stub.New: %v", err)
		}
		r := newFollowUpRunner(t, platform.URL, platform.Client(), base, nil)
		calls := 0
		r.now = func() time.Time {
			calls++
			if calls == 1 {
				return start
			}
			return start.Add(2 * time.Second)
		}
		qw := QueuedWork{
			QueuedWork:      queuedWorkBase("PRESTREAM-MISCONFIG"),
			WorkerID:        "worker-1",
			AuthToken:       "tok",
			PlatformURL:     platform.URL,
			ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
		}
		qw.StageBudget = &prompt.StageBudget{MaxDurationSeconds: 1}
		// Unclonable: fails fast without network, with a live context so
		// the failure is a clone error rather than a deadline.
		qw.Repository = filepath.Join(t.TempDir(), "does-not-exist.git")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, _ := r.Run(ctx, qw)
		if res.Status != "failed" || res.FailureMode != FailureWorktreeProvision {
			t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureWorktreeProvision)
		}
		if res.BudgetBreach != nil {
			t.Fatalf("BudgetBreach = %+v; a non-deadline failure must not record a breach", res.BudgetBreach)
		}
		if !strings.Contains(res.Error, "does-not-exist") {
			t.Fatalf("Error = %q; want the clone cause preserved, not a budget message", res.Error)
		}
	})

	// Interactive sessions run with no duration ceiling: even a
	// deadline-cut provision under an elapsed legacy budget keeps the
	// provisioning classification and records no breach.
	t.Run("interactive session with expired budget on a deadline-cut provision", func(t *testing.T) {
		start := time.Now()
		platform := newRecordingPlatformServer(t)
		base, err := stub.New()
		if err != nil {
			t.Fatalf("stub.New: %v", err)
		}
		r := delayProvisionRunner(t, platform.URL, platform.Client(), base)
		calls := 0
		r.now = func() time.Time {
			calls++
			if calls == 1 {
				return start
			}
			return start.Add(2 * time.Second)
		}
		qw := QueuedWork{
			QueuedWork:      queuedWorkBase("PRESTREAM-INTERACTIVE"),
			WorkerID:        "worker-1",
			AuthToken:       "tok",
			PlatformURL:     platform.URL,
			ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
		}
		qw.Mode = prompt.InteractiveRunMode
		qw.StageBudget = &prompt.StageBudget{MaxDurationSeconds: 1}
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		res, _ := r.Run(ctx, qw)
		if res.Status != "failed" || res.FailureMode != FailureWorktreeProvision {
			t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureWorktreeProvision)
		}
		if res.BudgetBreach != nil {
			t.Fatalf("BudgetBreach = %+v; an interactive session must not record a budget breach", res.BudgetBreach)
		}
	})
}
