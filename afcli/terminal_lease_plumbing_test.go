package afcli

import (
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestDetailToQueuedWorkForwardsTerminalWorkareaLease(t *testing.T) {
	t.Parallel()

	request := &workarea.TerminalLeaseRequest{
		SchemaVersion:      workarea.TerminalLeaseRequestSchemaV1,
		SettlementBudgetMS: (17 * time.Minute).Milliseconds(),
		SafetyMarginMS:     time.Minute.Milliseconds(),
		LeaseDurationMS:    (30 * time.Minute).Milliseconds(),
		MaxLeaseDurationMS: (2 * time.Hour).Milliseconds(),
	}
	queued, err := detailToQueuedWork(&daemon.SessionDetail{
		SessionID:             "session-1",
		InitialPrompt:         "inspect the retained terminal",
		TerminalWorkareaLease: request,
	})
	if err != nil {
		t.Fatalf("detailToQueuedWork: %v", err)
	}
	if queued.InitialPrompt != "inspect the retained terminal" {
		t.Fatalf("initial prompt = %q", queued.InitialPrompt)
	}
	if queued.TerminalWorkareaLease != request {
		t.Fatalf("terminal lease request = %+v", queued.TerminalWorkareaLease)
	}
}
