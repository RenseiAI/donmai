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
				if breached != tc.wantBreach || (rep.CapBreached == CapTokens) != tc.wantBreach {
					t.Errorf("breach = %v (report cap %q), want %v", breached, rep.CapBreached, tc.wantBreach)
				}
			})
		}
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
