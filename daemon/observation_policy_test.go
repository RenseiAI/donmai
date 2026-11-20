package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func policyMutation(id, items string) PendingMutation {
	return PendingMutation{ID: id, Op: observationsPolicyOp, Params: json.RawMessage(`{"version":1,"items":[` + items + `]}`)}
}

const (
	codexOn  = `{"kind":"subscription-login","slot":"default","harness":"codex","enabled":true,"guardPercent":20,"allowCredits":false,"allowOverage":true,"allowApiBilledFallback":false}`
	claudeOn = `{"kind":"subscription-login","slot":"default","harness":"claude","enabled":true,"guardPercent":0}`
)

var (
	codexSlot  = observationSlotKey{"subscription-login", "default", "codex"}
	claudeSlot = observationSlotKey{"subscription-login", "default", "claude"}
)

func TestDecodeObservationPolicy(t *testing.T) {
	tests := []struct {
		name, raw, wantErr string
	}{
		{"valid", `{"version":1,"items":[` + codexOn + `,` + claudeOn + `]}`, ""},
		{"unknown fields are dropped", `{"version":1,"extra":1,"items":[{"kind":"subscription-login","slot":"0123456789abcdef","harness":"muse","enabled":true,"future":true}]}`, ""},
		{"version 2", `{"version":2,"items":[]}`, "unsupported policy version"},
		{"raw path slot", `{"version":1,"items":[{"kind":"subscription-login","slot":"/home/u/.codex","harness":"codex"}]}`, "slot"},
		{"unknown harness", `{"version":1,"items":[{"kind":"subscription-login","slot":"default","harness":"other"}]}`, "unknown harness"},
		{"guard over 100", `{"version":1,"items":[{"kind":"subscription-login","slot":"default","harness":"codex","guardPercent":101}]}`, "guardPercent"},
		{"repeated item", `{"version":1,"items":[` + codexOn + `,` + codexOn + `]}`, "repeated"},
	}
	for _, tt := range tests {
		_, err := decodeObservationPolicy(json.RawMessage(tt.raw))
		if (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.wantErr)
		}
	}
	items, _ := decodeObservationPolicy(json.RawMessage(`{"version":1,"items":[` + codexOn + `]}`))
	if got := items[codexSlot]; !got.Enabled || got.GuardPercent != 20 || !got.AllowOverage || got.AllowCredits || got.AllowAPIBilledFallback {
		t.Errorf("decoded codex item = %+v", got)
	}
}

func TestObservationPolicyReplaceAndRegistrationIsolation(t *testing.T) {
	d := &Daemon{}
	if _, pushed := d.effectiveObservationPolicy(primaryObservationRegistration, codexSlot); pushed {
		t.Fatal("a policy is reported before any was pushed")
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		policyMutation("p1", codexOn+","+claudeOn), policyMutation("p2", codexOn),
	})
	if strings.Join(applied, ",") != "p1,p2" || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v", applied, failures)
	}
	if eff, pushed := d.effectiveObservationPolicy(primaryObservationRegistration, claudeSlot); !pushed || eff.Enabled {
		t.Errorf("claude after the replacing push = %+v pushed %v, want disabled", eff, pushed)
	}
	other := d.ApplyObservationMutations("org-b")
	if applied, _ := other(context.Background(), []PendingMutation{policyMutation("p3", claudeOn)}); len(applied) != 1 {
		t.Fatalf("org-b push not applied: %v", applied)
	}
	if eff, _ := d.effectiveObservationPolicy(primaryObservationRegistration, codexSlot); !eff.Enabled {
		t.Error("org-b's push changed the primary registration's policy")
	}
	if eff, _ := d.effectiveObservationPolicy("org-b", codexSlot); eff.Enabled {
		t.Error("org-b inherited the primary registration's codex item")
	}
	if _, pushed := d.effectiveObservationPolicy("org-c", codexSlot); pushed {
		t.Error("a registration with no push reports a policy")
	}
}

func TestObservationPolicyLocalNarrowOnly(t *testing.T) {
	guard := func(v int) *int { return &v }
	tests := []struct {
		name        string
		local       LocalObservationItem
		push        string
		wantEnabled bool
		wantGuard   int
	}{
		{"local disables", LocalObservationItem{Disabled: true}, codexOn, false, 20},
		{"local raises the guard", LocalObservationItem{GuardPercent: guard(30)}, codexOn, true, 30},
		{"local cannot lower the guard", LocalObservationItem{GuardPercent: guard(5)}, codexOn, true, 20},
		{"local cannot enable", LocalObservationItem{}, strings.Replace(codexOn, `"enabled":true`, `"enabled":false`, 1), false, 20},
	}
	for _, tt := range tests {
		local := tt.local
		local.Kind, local.Slot, local.Harness = "subscription-login", "default", "codex"
		d := &Daemon{config: &Config{Observations: &ObservationsConfig{Items: []LocalObservationItem{local}}}}
		if _, failures := d.applyPendingMutations(context.Background(), []PendingMutation{policyMutation("p", tt.push)}); len(failures) != 0 {
			t.Fatalf("%s: push failed: %v", tt.name, failures)
		}
		eff, _ := d.effectiveObservationPolicy(primaryObservationRegistration, codexSlot)
		if eff.Enabled != tt.wantEnabled || eff.GuardPercent != tt.wantGuard {
			t.Errorf("%s: effective = enabled %v guard %d, want %v %d", tt.name, eff.Enabled, eff.GuardPercent, tt.wantEnabled, tt.wantGuard)
		}
	}
}

func TestObservationMarksShowLocalDisable(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	d.config = &Config{Observations: &ObservationsConfig{Items: []LocalObservationItem{
		{Kind: "subscription-login", Slot: "default", Harness: "codex", Disabled: true},
	}}}
	result, err := d.ScanObservations(context.Background(), nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(result.Marks) != 1 || result.Marks[0].Enabled {
		t.Fatalf("marks = %+v, want the codex item locally disabled", result.Marks)
	}
}
