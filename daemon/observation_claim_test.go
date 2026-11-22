package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestSubscriptionLoginViability(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ahead, later, past := "2031-01-01T05:00:00Z", "2031-01-07T00:00:00Z", "2030-12-31T23:00:00Z"
	on := effectiveObservationPolicy{Enabled: true, GuardPercent: 20}
	tests := []struct {
		name       string
		eff        effectiveObservationPolicy
		windows    []agent.UsageWindow
		checkedAt  time.Time
		wantReason string
		wantUntil  string
	}{
		{name: "under the guard", eff: on, windows: []agent.UsageWindow{quotaClaimWindow("s", 79.9, ahead)}},
		{name: "exactly at the guard", eff: on, windows: []agent.UsageWindow{quotaClaimWindow("s", 80, ahead)}, wantReason: "guard_reached", wantUntil: ahead},
		{name: "until is the latest blocking reset", eff: on, windows: []agent.UsageWindow{quotaClaimWindow("s", 81, ahead), quotaClaimWindow("w", 85, later)}, wantReason: "guard_reached", wantUntil: later},
		{name: "guard window already reset", eff: on, windows: []agent.UsageWindow{quotaClaimWindow("s", 95, past)}},
		{name: "zero guard burns to exhaustion", eff: effectiveObservationPolicy{Enabled: true}, windows: []agent.UsageWindow{quotaClaimWindow("s", 99.9, ahead)}},
		{name: "exhausted with another window open", eff: effectiveObservationPolicy{Enabled: true}, windows: []agent.UsageWindow{quotaClaimWindow("s", 100, ahead), quotaClaimWindow("w", 40, later)}, wantReason: "exhausted", wantUntil: ahead},
		{name: "all exhausted: unknown billing is past the boundary", eff: on, windows: []agent.UsageWindow{quotaClaimWindow("s", 100, ahead)}, wantReason: "billing_boundary", wantUntil: ahead},
		{name: "disabled", eff: effectiveObservationPolicy{}, wantReason: "disabled"},
		{name: "stale usage admits", eff: on, windows: []agent.UsageWindow{quotaClaimWindow("s", 100, ahead)}, checkedAt: now.Add(-time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checked := tt.checkedAt
			if checked.IsZero() {
				checked = now
			}
			account := &agent.UsageAccount{Provider: "codex", Limits: agent.MakeUsageLimits(agent.ISOTime(checked), tt.windows)}
			report := observationUnavailableReportForError(subscriptionLoginViability(tt.eff, account, now))
			gotReason, gotUntil := "", ""
			if report != nil {
				gotReason, gotUntil = report.Reason, report.Until
			}
			if gotReason != tt.wantReason || gotUntil != tt.wantUntil {
				t.Errorf("refusal = %q until %q, want %q until %q", gotReason, gotUntil, tt.wantReason, tt.wantUntil)
			}
		})
	}
}

func TestObservationClaimAdmission_PolicyScope(t *testing.T) {
	var spawns int
	d := quotaClaimDaemonWithOptions(t, Options{}, &spawns)
	seedQuotaWindows(d, []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "2031-02-03T04:05:06Z")})
	spec := SessionSpec{Harness: "codex", AuthMode: string(agent.AuthHostSession)}
	if err := d.ObservationClaimAdmission(primaryObservationRegistration, spec, time.Now()); err != nil {
		t.Fatalf("no pushed policy: err = %v, want pass-through", err)
	}
	other := d.ApplyObservationMutations("org-b")
	other(context.Background(), []PendingMutation{policyMutation("p", claudeOn)})
	if err := d.ObservationClaimAdmission(primaryObservationRegistration, spec, time.Now()); err != nil {
		t.Fatalf("org-b's policy gated the primary registration: %v", err)
	}
	if err := d.ObservationClaimAdmission("org-b", spec, time.Now()); observationUnavailableReportForError(err) == nil {
		t.Fatalf("org-b claim err = %v, want a typed refusal (codex absent from its policy)", err)
	}
	keyCarried := SessionSpec{Harness: "codex", AuthMode: "api-key"}
	if err := d.ObservationClaimAdmission("org-b", keyCarried, time.Now()); err != nil {
		t.Fatalf("key-carried work was gated: %v", err)
	}
}

// A real poll claim under a pushed policy whose guard is reached nacks
// once with the typed observationUnavailable member and never spawns.
func TestObservationClaimAdmission_NacksTypedDetail(t *testing.T) {
	reset := "2031-02-03T04:05:06Z"
	fixture := newClaimAttemptHTTPFixture(t, quotaClaimPollBody("obs-1", claimAttemptTokenOne))
	var spawns int
	d := quotaClaimDaemonWithOptions(t, Options{}, &spawns)
	seedQuotaWindows(d, []agent.UsageWindow{quotaClaimWindow("five_hour", 85, reset)})
	if _, failures := d.applyPendingMutations(context.Background(), []PendingMutation{policyMutation("p", codexOn)}); len(failures) != 0 {
		t.Fatalf("policy push failed: %v", failures)
	}
	var onWorkErr error
	NewPollService(PollOptions{
		WorkerID: "worker-obs", RuntimeJWT: "runtime-obs",
		OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
		OnWork: func(item PollWorkItem) error {
			onWorkErr = d.handlePollWorkItem(item, fixture.server.URL)
			return onWorkErr
		},
	}).pollOnce(context.Background())
	var refused *observationUnavailableError
	if !errors.As(onWorkErr, &refused) || spawns != 0 {
		t.Fatalf("handlePollWorkItem err = %v spawns = %d, want a typed refusal and no spawn", onWorkErr, spawns)
	}
	nacks := fixture.nackBodies()
	if len(nacks) != 1 {
		t.Fatalf("NACK count = %d, want 1", len(nacks))
	}
	var got map[string]string
	if err := json.Unmarshal(nacks[0]["observationUnavailable"], &got); err != nil {
		t.Fatalf("observationUnavailable member missing: %s", mustJSON(nacks[0]))
	}
	want := map[string]string{"code": "observation_unavailable", "kind": "subscription-login", "reason": "guard_reached", "until": reset}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("observationUnavailable[%s] = %q, want %q (member %v)", k, got[k], v, got)
		}
	}
}
