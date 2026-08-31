package afcli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

func TestDetailToQueuedWorkReconcilesSiblingStageBudget(t *testing.T) {
	tests := []struct {
		name   string
		budget daemon.PollStageBudget
	}{
		{
			name: "all limits",
			budget: daemon.PollStageBudget{
				MaxDurationSeconds: 1800,
				MaxSubAgents:       3,
				MaxTokens:          24_000,
			},
		},
		{name: "explicit zero budget"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work, err := detailToQueuedWork(&daemon.SessionDetail{StageBudget: &tt.budget})
			if err != nil {
				t.Fatalf("detailToQueuedWork: %v", err)
			}
			if work.StageBudget == nil {
				t.Fatal("detailToQueuedWork omitted the sibling stage budget")
			}
			if work.StageBudget.MaxDurationSeconds != tt.budget.MaxDurationSeconds ||
				work.StageBudget.MaxSubAgents != tt.budget.MaxSubAgents ||
				work.StageBudget.MaxTokens != tt.budget.MaxTokens {
				t.Fatalf("StageBudget = %+v, want %+v", *work.StageBudget, tt.budget)
			}
		})
	}
}

func TestDetailToQueuedWorkRejectsSiblingStageBudgetAbsentFromOperationalPayload(t *testing.T) {
	_, err := detailToQueuedWork(&daemon.SessionDetail{
		SessionID:          "receipt-stage-budget-mismatch",
		OperationalPayload: json.RawMessage(`{"sessionId":"receipt-stage-budget-mismatch"}`),
		StageBudget:        &daemon.PollStageBudget{MaxTokens: 24_000},
	})
	if err == nil || !strings.Contains(err.Error(), "stage budget compatibility mirror differs from operational payload") {
		t.Fatalf("detailToQueuedWork error = %v, want operational-payload mismatch refusal", err)
	}
}
