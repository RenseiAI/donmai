package observation

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

var checkedAt = "2026-11-19T12:00:00Z"

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
	h := Header{
		ItemKey: SubscriptionLoginItemKey(ProviderOpenAIChatGPT, "acct-1", ""), Slot: DefaultSlot,
		Identity: IdentityAccount, Definition: "openai-chatgpt:pro",
		Readiness: Readiness{State: ReadinessReady, CheckedAt: checkedAt},
	}
	a := SubscriptionLogin{
		Harness: "codex", Provider: ProviderOpenAIChatGPT, Plan: "pro",
		Allowances: []Allowance{{ID: "codex", Unit: UnitPercent, Windows: []Window{
			{ID: "primary", Kind: "session", Used: 42.5, ResetsAt: "2026-11-19T15:00:00Z", WindowDurationMins: &dur},
		}}},
		Billing: Billing{Mode: BillingAllowance},
	}
	return h, a
}

func TestKeyHelpersGolden(t *testing.T) {
	tests := []struct{ name, got, want string }{
		{"account", SubscriptionLoginItemKey("openai-chatgpt", "acct-1", ""), "b60967ee74bf2f1943f978c146bb61df27f37130321366b20f79b812935790e0"},
		{"account per seat", SubscriptionLoginItemKey("openai-chatgpt", "acct-1", "user-1"), "ab8c7c183ce8ea7d11ca10eaee0b909c30cb68905207ebf3231364ea5d8f7bc6"},
		{"host scoped", HostScopedItemKey(KindSubscriptionLogin, "muse", DefaultSlot), "da828cbf662364a38691c5cf62b79618d970c899915639916f996d11502af8ca"},
		{"default slot", SlotFor("codex", ""), "default"},
		{"extra slot", SlotFor("codex", "/home/u/.codex-work"), "9bacf73279972940"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, tt.got, tt.want)
		}
	}
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

func TestEncodedItemIsSecretFree(t *testing.T) {
	secrets := map[string]string{
		"token":     "sk-proj-AbC123dEf456GhI789jKl012MnO345pQr678",
		"jwt":       "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.c2lnbmF0dXJl",
		"email":     "someone@example.com",
		"auth path": "/home/someone/.codex/auth.json",
		"account":   "4f9c2b1e-8a7d-4e3f-9b6a-1c2d3e4f5a6b",
	}
	h, a := sampleLogin()
	h.ItemKey = SubscriptionLoginItemKey(ProviderOpenAIChatGPT, secrets["account"], secrets["email"])
	h.Slot = SlotFor("codex", secrets["auth path"])
	out, err := json.Marshal(loginItem(t, h, a))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for name, secret := range secrets {
		if strings.Contains(string(out), secret) {
			t.Errorf("encoded item carries the %s: %s", name, out)
		}
	}
}

type bogusKind struct{ SubscriptionLogin }

func (bogusKind) kindID() string { return "bogus" }

func TestEncodeRefusesBadShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Header, *SubscriptionLogin)
		attrs  func(SubscriptionLogin) Attributes
	}{
		{name: "uppercase itemKey", mutate: func(h *Header, _ *SubscriptionLogin) { h.ItemKey = strings.ToUpper(h.ItemKey) }},
		{name: "short itemKey", mutate: func(h *Header, _ *SubscriptionLogin) { h.ItemKey = "abc" }},
		{name: "raw path slot", mutate: func(h *Header, _ *SubscriptionLogin) { h.Slot = "/home/u/.codex" }},
		{name: "bad identity", mutate: func(h *Header, _ *SubscriptionLogin) { h.Identity = "user" }},
		{name: "bad readiness", mutate: func(h *Header, _ *SubscriptionLogin) { h.Readiness.State = "fine" }},
		{name: "bad harness", mutate: func(_ *Header, a *SubscriptionLogin) { a.Harness = "other" }},
		{name: "bad billing mode", mutate: func(_ *Header, a *SubscriptionLogin) { a.Billing.Mode = "free" }},
		{name: "bad window kind", mutate: func(_ *Header, a *SubscriptionLogin) { a.Allowances[0].Windows[0].Kind = "daily" }},
		{name: "jwt plan", mutate: func(_ *Header, a *SubscriptionLogin) { a.Plan = "eyJhbGciOiJSUzI1NiJ9.e30.c2ln" }},
		{name: "path window id", mutate: func(_ *Header, a *SubscriptionLogin) { a.Allowances[0].Windows[0].ID = "/home/u/auth.json" }},
		{name: "unknown kind", attrs: func(a SubscriptionLogin) Attributes { return bogusKind{a} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, a := sampleLogin()
			if tt.mutate != nil {
				tt.mutate(&h, &a)
			}
			var attrs Attributes = a
			if tt.attrs != nil {
				attrs = tt.attrs(a)
			}
			item, err := Encode(h, attrs)
			if err == nil {
				t.Fatal("Encode accepted a bad shape")
			}
			if _, err := json.Marshal(item); err == nil {
				t.Fatal("a refused item still marshals")
			}
		})
	}
}

func TestNewReportRefusesUncoveredItems(t *testing.T) {
	h, a := sampleLogin()
	scanned := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for name, args := range map[string]struct {
		kinds []string
		items []Item
	}{
		"unknown kind": {[]string{"bogus"}, nil},
		"zero item":    {[]string{KindSubscriptionLogin}, []Item{{}}},
		"no kinds":     {nil, []Item{loginItem(t, h, a)}},
	} {
		if _, err := NewReport(scanned, true, args.kinds, args.items); err == nil {
			t.Errorf("%s: NewReport accepted it", name)
		}
	}
}

func TestEveryAttributeDeclaresDisclosure(t *testing.T) {
	valid := []Disclosure{DisclosurePublic, DisclosureIdentity, DisclosureHashed, DisclosureBoolean, DisclosureEnum}
	for kind, attrs := range registry {
		typ := reflect.TypeOf(attrs)
		for i := 0; i < typ.NumField(); i++ {
			if f := typ.Field(i); !oneOf(Disclosure(f.Tag.Get("disclosure")), valid...) {
				t.Errorf("%s.%s has no valid disclosure class", kind, f.Name)
			}
		}
	}
}
