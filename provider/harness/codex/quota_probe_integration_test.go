//go:build codex_integration

package codex

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestIntegration_ProbeQuotaAnswersAgainstHostLogin runs the
// production quota probe (construct, read, shut down) against the real
// codex CLI with the host's own login. It asserts the contract the
// heartbeat consumer relies on: a signed-in host answers with windows
// and ok:true, never with a refusal. It needs no model turn — the
// read is the documented rate-limits call — so it costs no quota.
//
// Run via:
//
//	go test -tags codex_integration -run TestIntegration_ProbeQuotaAnswersAgainstHostLogin ./provider/harness/codex/
func TestIntegration_ProbeQuotaAnswersAgainstHostLogin(t *testing.T) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex binary not on PATH; skipping integration test")
	}
	p, err := New(Options{
		CodexBin:         binary,
		HostSessionAuth:  true,
		HandshakeTimeout: 30 * 1000000000,
		RPCTimeout:       RateLimitsProbeTimeout,
	})
	if err != nil {
		t.Skipf("codex probe construction unavailable (no host login?): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	accountID, plan, probed := p.ProbeQuota(ctx)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = p.Shutdown(shutdownCtx)
	if probed.Unavailable != nil {
		if probed.Unavailable.Answered {
			t.Fatalf("signed-in host probe refused: %+v (ok:false on a signed-in host)", probed.Unavailable)
		}
		t.Skipf("codex probe unreachable, no verdict: %+v", probed.Unavailable)
	}
	if len(probed.Windows) == 0 {
		t.Fatalf("probed = %+v, want windows", probed)
	}
	if accountID == "" {
		t.Error("account id is empty; the probe must name the account it read")
	}
	if plan == "" {
		t.Error("plan is empty; the probe must name the subscription plan")
	}
	check := agent.UsageAuthCheckAfterProbe(agent.UsageHarnessCodex, &probed, time.Now())
	if check == nil || !check.OK {
		t.Errorf("authCheck = %+v, want ok:true beside answered windows", check)
	}
}
