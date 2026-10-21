package afcli

import (
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runner"
)

// TestDetailToQueuedWork_TopLevelDurationFallback pins that a legacy top-level
// poll duration takes effect as the stage budget duration when the nested
// stage budget carries no positive duration.
func TestDetailToQueuedWork_TopLevelDurationFallback(t *testing.T) {
	detail := &daemon.SessionDetail{
		SessionID:          "sess-top-level",
		MaxDurationSeconds: 3600,
	}
	qw, err := detailToQueuedWork(detail)
	if err != nil {
		t.Fatalf("detailToQueuedWork: %v", err)
	}
	if qw.StageBudget == nil || qw.StageBudget.MaxDurationSeconds != 3600 {
		t.Fatalf("queued StageBudget = %+v, want duration 3600", qw.StageBudget)
	}
}

// TestDetailToQueuedWork_NestedDurationWins pins precedence: a positive nested
// stage budget duration wins over the top-level fallback.
func TestDetailToQueuedWork_NestedDurationWins(t *testing.T) {
	detail := &daemon.SessionDetail{
		SessionID:          "sess-both",
		MaxDurationSeconds: 900,
		StageBudget:        &daemon.PollStageBudget{MaxDurationSeconds: 1800, MaxTokens: 5000},
	}
	qw, err := detailToQueuedWork(detail)
	if err != nil {
		t.Fatalf("detailToQueuedWork: %v", err)
	}
	if qw.StageBudget == nil || qw.StageBudget.MaxDurationSeconds != 1800 {
		t.Fatalf("queued StageBudget = %+v, want nested duration 1800", qw.StageBudget)
	}
	if qw.StageBudget.MaxTokens != 5000 {
		t.Fatalf("queued StageBudget tokens = %d, want 5000 (nested caps preserved)", qw.StageBudget.MaxTokens)
	}
}

// TestDetailToQueuedWork_TopLevelPreservesNestedCaps pins that the fallback
// fills only the missing duration, preserving nested sub-agent/token caps.
func TestDetailToQueuedWork_TopLevelPreservesNestedCaps(t *testing.T) {
	subAgentCap := 2
	detail := &daemon.SessionDetail{
		SessionID:          "sess-caps",
		MaxDurationSeconds: 3600,
		StageBudget:        &daemon.PollStageBudget{MaxSubAgents: &subAgentCap, MaxTokens: 8000},
	}
	qw, err := detailToQueuedWork(detail)
	if err != nil {
		t.Fatalf("detailToQueuedWork: %v", err)
	}
	if qw.StageBudget == nil || qw.StageBudget.MaxDurationSeconds != 3600 {
		t.Fatalf("queued StageBudget = %+v, want fallback duration 3600", qw.StageBudget)
	}
	if qw.StageBudget.MaxTokens != 8000 {
		t.Fatalf("queued StageBudget tokens = %d, want 8000", qw.StageBudget.MaxTokens)
	}
	if qw.StageBudget.MaxSubAgents == nil || *qw.StageBudget.MaxSubAgents != 2 {
		t.Fatalf("queued StageBudget sub-agents = %+v, want 2", qw.StageBudget.MaxSubAgents)
	}
}

// TestAgentRunMaxSessionDuration_TopLevelFallback pins that the runner outer
// timeout honors the legacy top-level duration when no nested duration exists.
func TestAgentRunMaxSessionDuration_TopLevelFallback(t *testing.T) {
	detail := &daemon.SessionDetail{
		SessionID:          "sess-top-level",
		MaxDurationSeconds: 3600,
	}
	if got := agentRunMaxSessionDuration(detail); got != 3600*time.Second {
		t.Fatalf("agentRunMaxSessionDuration = %s, want %s", got, 3600*time.Second)
	}
}

// TestAgentRunMaxSessionDuration_NestedWins pins precedence for the outer
// timeout: nested wins when positive.
func TestAgentRunMaxSessionDuration_NestedWins(t *testing.T) {
	detail := &daemon.SessionDetail{
		SessionID:          "sess-both",
		MaxDurationSeconds: 900,
		StageBudget:        &daemon.PollStageBudget{MaxDurationSeconds: 1800},
	}
	if got := agentRunMaxSessionDuration(detail); got != 1800*time.Second {
		t.Fatalf("agentRunMaxSessionDuration = %s, want %s", got, 1800*time.Second)
	}
}

// TestAgentRunMaxSessionDuration_TopLevelClamped pins that the fallback is
// clamped to the same ceiling as the nested budget.
func TestAgentRunMaxSessionDuration_TopLevelClamped(t *testing.T) {
	detail := &daemon.SessionDetail{
		SessionID:          "sess-huge",
		MaxDurationSeconds: int(runner.MaxSessionDurationCeiling/time.Second) + 3600,
	}
	if got := agentRunMaxSessionDuration(detail); got != runner.MaxSessionDurationCeiling {
		t.Fatalf("agentRunMaxSessionDuration = %s, want ceiling %s", got, runner.MaxSessionDurationCeiling)
	}
}

// TestEffectiveStageDurationSeconds_NoCap pins the zero case: no duration
// anywhere means no cap (zero), preserving the legacy unbounded path.
func TestEffectiveStageDurationSeconds_NoCap(t *testing.T) {
	if got := effectiveStageDurationSeconds(&daemon.SessionDetail{SessionID: "sess-none"}); got != 0 {
		t.Fatalf("effectiveStageDurationSeconds = %d, want 0", got)
	}
	if got := effectiveStageDurationSeconds(nil); got != 0 {
		t.Fatalf("effectiveStageDurationSeconds(nil) = %d, want 0", got)
	}
}
