//go:build integration

package claude

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestIntegration_ProbeUsageAnswersAgainstHostLogin runs the
// production usage probe against the real claude CLI with the host's
// own login. It asserts the contract the heartbeat consumer relies
// on: a signed-in host answers with windows and ok:true, never with
// a refusal. The probe sends no user message and performs no model
// turn, so it costs no quota.
//
// Run via:
//
//	go test -tags integration -run TestIntegration_ProbeUsageAnswersAgainstHostLogin ./provider/harness/claude/
func TestIntegration_ProbeUsageAnswersAgainstHostLogin(t *testing.T) {
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude binary not on PATH; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	accountID, probed, _ := ProbeUsage(ctx, binary)
	if probed.Unavailable != nil {
		if probed.Unavailable.Reason == agent.UsageUnavailableUnsupported {
			t.Fatalf("signed-in host probe unsupported: %+v (the get_usage answer never arrived)", probed.Unavailable)
		}
		if probed.Unavailable.Answered {
			t.Fatalf("signed-in host probe refused: %+v (ok:false on a signed-in host)", probed.Unavailable)
		}
		t.Skipf("probe unreachable, no verdict: %+v", probed.Unavailable)
	}
	if len(probed.Windows) == 0 {
		t.Fatalf("probed = %+v, want windows", probed)
	}
	if accountID == "" {
		t.Error("account id is empty; the probe must name the account it read")
	}
	check := agent.UsageAuthCheckAfterProbe(agent.UsageHarnessClaude, &probed, time.Now())
	if check == nil || !check.OK {
		t.Errorf("authCheck = %+v, want ok:true beside answered windows", check)
	}
}
