package observation

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func loginItem(t *testing.T, h Header, a SubscriptionLogin) Item {
	t.Helper()
	item, err := Encode(h, a)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return item
}

func sampleLogin() (Header, SubscriptionLogin) {
	dur := 300
	a := SubscriptionLogin{
		Harness: "codex", Provider: ProviderOpenAIChatGPT, Plan: "pro",
		Allowances: []Allowance{{ID: "codex", Unit: UnitPercent, Windows: []Window{
			{ID: "primary", Kind: "session", Used: 42.5, ResetsAt: "2026-11-19T15:00:00Z", WindowDurationMins: &dur},
		}}},
		Billing: Billing{Mode: BillingAllowance},
	}
	return sampleHeader(), a
}

func TestReportGolden(t *testing.T) {
	h, a := sampleLogin()
	muse := SubscriptionLogin{Harness: "muse", Provider: ProviderMuse, Billing: Billing{Mode: BillingUnknown}}
	museHeader := Header{
		ItemKey: HostScopedItemKey(KindSubscriptionLogin, "muse", DefaultSlot), Slot: DefaultSlot,
		Identity: IdentityHostScoped, Readiness: Readiness{State: ReadinessUnavailable, Reasons: []string{"login_refused"}, CheckedAt: checkedAt},
	}
	scanned := time.Date(2026, 11, 19, 12, 0, 5, 0, time.UTC)
	report, err := NewReport(scanned, true, []string{KindSubscriptionLogin}, []Item{loginItem(t, h, a), loginItem(t, museHeader, muse)})
	if err != nil {
		t.Fatalf("NewReport: %v", err)
	}
	got, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want, err := os.ReadFile("testdata/subscription_login_report.golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, bytes.TrimSpace(want)) {
		t.Fatalf("report bytes drifted from the golden fixture\n got: %s\nwant: %s", got, want)
	}
}

func TestSubscriptionLoginRefusesBadAttributes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SubscriptionLogin)
	}{
		{"bad harness", func(a *SubscriptionLogin) { a.Harness = "other" }},
		{"bad provider", func(a *SubscriptionLogin) { a.Provider = "other" }},
		{"bad billing mode", func(a *SubscriptionLogin) { a.Billing.Mode = "free" }},
		{"bad unit", func(a *SubscriptionLogin) { a.Allowances[0].Unit = "dollars" }},
		{"bad window kind", func(a *SubscriptionLogin) { a.Allowances[0].Windows[0].Kind = "daily" }},
		{"percent over 100", func(a *SubscriptionLogin) { a.Allowances[0].Windows[0].Used = 101 }},
		{"jwt plan", func(a *SubscriptionLogin) { a.Plan = "eyJhbGciOiJSUzI1NiJ9.e30.c2ln" }},
		{"path window id", func(a *SubscriptionLogin) { a.Allowances[0].Windows[0].ID = "/home/u/auth.json" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, a := sampleLogin()
			tt.mutate(&a)
			item, err := Encode(h, a)
			if err == nil {
				t.Fatal("Encode accepted a bad attribute")
			}
			if _, err := json.Marshal(item); err == nil {
				t.Fatal("a refused item still marshals")
			}
		})
	}
}

func TestSubscriptionLoginEncodesClaudeWindowIDs(t *testing.T) {
	h, a := sampleLogin()
	h.Definition = "anthropic-claude:max_20x"
	a.Harness, a.Provider, a.Plan = "claude", ProviderAnthropicClaude, "max_20x"
	a.Allowances = []Allowance{{ID: "claude", Unit: UnitPercent, Windows: []Window{
		{ID: "five_hour", Kind: "session", Used: 54},
		{ID: "seven_day", Kind: "weekly", Used: 18.4},
		{ID: "seven_day_opus", Kind: "weekly", Used: 73},
	}}}
	out, err := json.Marshal(loginItem(t, h, a))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, id := range []string{`"five_hour"`, `"seven_day"`, `"seven_day_opus"`, `"anthropic-claude:max_20x"`} {
		if !bytes.Contains(out, []byte(id)) {
			t.Errorf("encoded claude item lacks %s: %s", id, out)
		}
	}
}
