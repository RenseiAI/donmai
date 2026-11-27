package observation

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var checkedAt = "2026-11-19T12:00:00Z"

// testKind is registered only in tests: the envelope needs no real kind.
type testKind struct {
	Name string `json:"name" disclosure:"public"`
}

const testKindID = "test-kind"

func init() { registry[testKindID] = testKind{} }

func (testKind) kindID() string { return testKindID }

func (k testKind) normalize() (Attributes, error) {
	if !slugPattern.MatchString(k.Name) {
		return nil, errors.New("name is not a lowercase slug")
	}
	return k, nil
}

func sampleHeader() Header {
	return Header{
		ItemKey: SubscriptionLoginItemKey("openai-chatgpt", "acct-1", ""), Slot: DefaultSlot, Identity: IdentityAccount,
		Definition: "openai-chatgpt:pro", Readiness: Readiness{State: ReadinessReady, CheckedAt: checkedAt},
	}
}

func TestKeyHelpersGolden(t *testing.T) {
	tests := []struct{ name, got, want string }{
		{"account", SubscriptionLoginItemKey("openai-chatgpt", "acct-1", ""), "b60967ee74bf2f1943f978c146bb61df27f37130321366b20f79b812935790e0"},
		{"account per seat", SubscriptionLoginItemKey("openai-chatgpt", "acct-1", "user-1"), "ab8c7c183ce8ea7d11ca10eaee0b909c30cb68905207ebf3231364ea5d8f7bc6"},
		{"host scoped", HostScopedItemKey("subscription-login", "muse", DefaultSlot), "da828cbf662364a38691c5cf62b79618d970c899915639916f996d11502af8ca"},
		{"default slot", SlotFor("codex", ""), "default"},
		{"extra slot", SlotFor("codex", "/home/u/.codex-work"), "9bacf73279972940"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, tt.got, tt.want)
		}
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
	h := sampleHeader()
	h.ItemKey = SubscriptionLoginItemKey("openai-chatgpt", secrets["account"], secrets["email"])
	h.Slot = SlotFor("codex", secrets["auth path"])
	item, err := Encode(h, testKind{Name: "codex"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for name, secret := range secrets {
		if strings.Contains(string(out), secret) {
			t.Errorf("encoded item carries the %s: %s", name, out)
		}
	}
}

type bogusKind struct{ testKind }

func (bogusKind) kindID() string { return "bogus" }

func TestEncodeRefusesBadShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Header)
		attrs  Attributes
		ok     bool
	}{
		{name: "underscore ids", attrs: testKind{Name: "seven_day_opus"}, ok: true},
		{name: "underscore definition", mutate: func(h *Header) { h.Definition = "anthropic-claude:max_20x" }, ok: true},
		{name: "uppercase itemKey", mutate: func(h *Header) { h.ItemKey = strings.ToUpper(h.ItemKey) }},
		{name: "short itemKey", mutate: func(h *Header) { h.ItemKey = "abc" }},
		{name: "raw path slot", mutate: func(h *Header) { h.Slot = "/home/u/.codex" }},
		{name: "bad identity", mutate: func(h *Header) { h.Identity = "user" }},
		{name: "bad readiness", mutate: func(h *Header) { h.Readiness.State = "fine" }},
		{name: "jwt definition", mutate: func(h *Header) { h.Definition = "openai-chatgpt:eyJhbGciOiJSUzI1NiJ9" }},
		{name: "unknown kind", attrs: bogusKind{testKind{Name: "codex"}}},
		{name: "attributes refuse", attrs: testKind{Name: "/home/u/auth.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := sampleHeader()
			if tt.mutate != nil {
				tt.mutate(&h)
			}
			attrs := tt.attrs
			if attrs == nil {
				attrs = testKind{Name: "codex"}
			}
			item, err := Encode(h, attrs)
			if tt.ok != (err == nil) {
				t.Fatalf("Encode error = %v, want ok = %v", err, tt.ok)
			}
			if tt.ok {
				return
			}
			if _, err := json.Marshal(item); err == nil {
				t.Fatal("a refused item still marshals")
			}
		})
	}
}

func TestNewReportRefusesUncoveredItems(t *testing.T) {
	item, err := Encode(sampleHeader(), testKind{Name: "codex"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	scanned := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for name, args := range map[string]struct {
		kinds []string
		items []Item
	}{
		"unknown kind": {[]string{"bogus"}, nil},
		"zero item":    {[]string{testKindID}, []Item{{}}},
		"no kinds":     {nil, []Item{item}},
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
