package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/prompt"
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
