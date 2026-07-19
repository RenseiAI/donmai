package daemon

import (
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestPollItemToSessionDetailForwardsTerminalWorkareaLease(t *testing.T) {
	t.Parallel()

	request := &workarea.TerminalLeaseRequest{
		SchemaVersion:      workarea.TerminalLeaseRequestSchemaV1,
		SettlementBudgetMS: (17 * time.Minute).Milliseconds(),
		SafetyMarginMS:     time.Minute.Milliseconds(),
		LeaseDurationMS:    (30 * time.Minute).Milliseconds(),
		MaxLeaseDurationMS: (2 * time.Hour).Milliseconds(),
	}
	detail := PollItemToSessionDetail(PollWorkItem{
		SessionID:             "session-1",
		TerminalWorkareaLease: request,
	}, nil, "https://example.test", "token", "worker")
	if detail.TerminalWorkareaLease != request {
		t.Fatalf("terminal lease request = %+v", detail.TerminalWorkareaLease)
	}

	legacy := PollItemToSessionDetail(PollWorkItem{SessionID: "legacy"}, nil, "", "", "")
	if legacy.TerminalWorkareaLease != nil {
		t.Fatalf("legacy request = %+v, want nil", legacy.TerminalWorkareaLease)
	}
}
