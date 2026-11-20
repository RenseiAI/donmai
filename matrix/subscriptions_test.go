package matrix

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// expectedSubscriptionFixture is the independent wire expectation for AC-1:
// provider identity and kind, both plan slugs in stable order, login kind,
// exact model and harness lists, and ordered billing modes. It duplicates
// the declaration literally so a drift in either copy fails the test.
func expectedSubscriptionFixture() []SubscriptionProvider {
	billing := []string{"allowance", "credits", "overage", "api-metered"}
	models := []SubscriptionModelRef{{ModelProvider: "openai", ModelID: "gpt-6-sol"}}
	return []SubscriptionProvider{
		{
			ID:         "openai-chatgpt",
			Kind:       SubscriptionProviderKindDirect,
			LoginKinds: []agent.AuthMode{agent.AuthHostSession},
			Plans: []SubscriptionPlan{
				{
					Slug:                   "plus",
					DisplayName:            "Plus",
					Allowance:              SubscriptionAllowance{Type: "percentage-window"},
					Models:                 append([]SubscriptionModelRef(nil), models...),
					Harnesses:              []agent.HarnessName{agent.HarnessCodex},
					BillingModes:           append([]string(nil), billing...),
					LowCap:                 false,
					GuardPercent:           0,
					WarnAtPercent:          []int{80},
					AllowCredits:           false,
					AllowOverage:           false,
					AllowAPIBilledFallback: false,
				},
				{
					Slug:                   "team",
					DisplayName:            "Team",
					Allowance:              SubscriptionAllowance{Type: "percentage-window"},
					Models:                 append([]SubscriptionModelRef(nil), models...),
					Harnesses:              []agent.HarnessName{agent.HarnessCodex},
					BillingModes:           append([]string(nil), billing...),
					LowCap:                 true,
					GuardPercent:           50,
					WarnAtPercent:          []int{80},
					AllowCredits:           false,
					AllowOverage:           false,
					AllowAPIBilledFallback: false,
				},
			},
		},
	}
}

func decodeSubscriptionProviders(t *testing.T, raw []byte) []SubscriptionProvider {
	t.Helper()
	var doc struct {
		SubscriptionProviders []SubscriptionProvider `json:"subscriptionProviders"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal subscriptionProviders: %v", err)
	}
	return doc.SubscriptionProviders
}

// TestSubscriptionProviders_MatchDeclarationInBothDocuments is AC-1: both
// rendered catalog documents carry the complete subscriptionProviders
// section matching the independent wire fixture, including compatible
// harnesses and billing order. Omitting the projection field or
// alphabetizing billing modes turns this test red.
func TestSubscriptionProviders_MatchDeclarationInBothDocuments(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	arts, err := built.Render()
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	want := expectedSubscriptionFixture()

	for _, name := range []string{FileCapabilityMatrix, FileMatrix} {
		got := decodeSubscriptionProviders(t, arts.Files[name])
		if !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			t.Errorf("%s subscriptionProviders mismatch:\n got=%s\nwant=%s", name, gotJSON, wantJSON)
			continue
		}
		if len(got) != 1 {
			t.Fatalf("%s: want 1 subscription provider, got %d", name, len(got))
		}
		p := got[0]
		if p.ID != "openai-chatgpt" {
			t.Errorf("%s: provider id = %q, want openai-chatgpt", name, p.ID)
		}
		if p.Kind != SubscriptionProviderKindDirect {
			t.Errorf("%s: provider kind = %q, want direct", name, p.Kind)
		}
		if len(p.LoginKinds) != 1 || p.LoginKinds[0] != agent.AuthHostSession {
			t.Errorf("%s: loginKinds = %v, want [host-session]", name, p.LoginKinds)
		}
		if len(p.Plans) != 2 || p.Plans[0].Slug != "plus" || p.Plans[1].Slug != "team" {
			t.Errorf("%s: plan slugs = %v, want [plus team] in stable order", name, planSlugs(p.Plans))
		}
		for _, plan := range p.Plans {
			if !reflect.DeepEqual(plan.Models, []SubscriptionModelRef{{ModelProvider: "openai", ModelID: "gpt-6-sol"}}) {
				t.Errorf("%s plan %q models = %+v, want openai/gpt-6-sol", name, plan.Slug, plan.Models)
			}
			if !reflect.DeepEqual(plan.Harnesses, []agent.HarnessName{agent.HarnessCodex}) {
				t.Errorf("%s plan %q harnesses = %v, want [codex]", name, plan.Slug, plan.Harnesses)
			}
			if !reflect.DeepEqual(plan.BillingModes, []string{"allowance", "credits", "overage", "api-metered"}) {
				t.Errorf("%s plan %q billingModes = %v, want ordered [allowance credits overage api-metered]", name, plan.Slug, plan.BillingModes)
			}
		}
	}
}

func planSlugs(plans []SubscriptionPlan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Slug)
	}
	return out
}

// TestSubscriptionPlan_DefaultsAreExplicit is AC-2: team reserves 50% with
// lowCap, plus reserves 0% without lowCap; both warn at [80] and carry all
// three permissions explicitly false. Key-presence checks on the raw JSON
// prove an explicit zero/false rather than an absent key. Giving plus the
// team guard or omitting a false permission turns this test red.
func TestSubscriptionPlan_DefaultsAreExplicit(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	arts, err := built.Render()
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	for _, name := range []string{FileCapabilityMatrix, FileMatrix} {
		// Typed check: values by slug.
		typed := decodeSubscriptionProviders(t, arts.Files[name])
		if len(typed) != 1 {
			t.Fatalf("%s: want 1 provider, got %d", name, len(typed))
		}
		bySlug := map[string]SubscriptionPlan{}
		for _, p := range typed[0].Plans {
			bySlug[p.Slug] = p
		}
		plus, ok := bySlug["plus"]
		if !ok {
			t.Fatalf("%s: missing plus plan", name)
		}
		team, ok := bySlug["team"]
		if !ok {
			t.Fatalf("%s: missing team plan", name)
		}
		if team.LowCap != true || team.GuardPercent != 50 {
			t.Errorf("%s team: lowCap=%v guardPercent=%d, want true/50", name, team.LowCap, team.GuardPercent)
		}
		if plus.LowCap != false || plus.GuardPercent != 0 {
			t.Errorf("%s plus: lowCap=%v guardPercent=%d, want false/0", name, plus.LowCap, plus.GuardPercent)
		}
		for _, plan := range []SubscriptionPlan{plus, team} {
			if !reflect.DeepEqual(plan.WarnAtPercent, []int{80}) {
				t.Errorf("%s plan %q warnAtPercent=%v, want [80]", name, plan.Slug, plan.WarnAtPercent)
			}
			if plan.AllowCredits || plan.AllowOverage || plan.AllowAPIBilledFallback {
				t.Errorf("%s plan %q permissions = credits %v overage %v apiFallback %v, want all false",
					name, plan.Slug, plan.AllowCredits, plan.AllowOverage, plan.AllowAPIBilledFallback)
			}
		}

		// Raw-JSON check: explicit zero/false keys must be present.
		var rawDoc map[string]any
		if err := json.Unmarshal(arts.Files[name], &rawDoc); err != nil {
			t.Fatalf("%s: unmarshal raw: %v", name, err)
		}
		provs, ok := rawDoc["subscriptionProviders"].([]any)
		if !ok || len(provs) != 1 {
			t.Fatalf("%s: raw subscriptionProviders missing or not length 1", name)
		}
		prov, _ := provs[0].(map[string]any)
		plans, _ := prov["plans"].([]any)
		if len(plans) != 2 {
			t.Fatalf("%s: raw plans length = %d, want 2", name, len(plans))
		}
		for _, rp := range plans {
			m, _ := rp.(map[string]any)
			slug, _ := m["slug"].(string)
			for _, key := range []string{"lowCap", "guardPercent", "warnAtPercent", "allowCredits", "allowOverage", "allowApiBilledFallback"} {
				if _, present := m[key]; !present {
					t.Errorf("%s plan %q: raw key %q absent (must be explicit zero/false/[80])", name, slug, key)
				}
			}
			if slug == "team" {
				if v, _ := m["guardPercent"].(float64); v != 50 {
					t.Errorf("%s team raw guardPercent=%v, want 50", name, v)
				}
				if v, _ := m["lowCap"].(bool); v != true {
					t.Errorf("%s team raw lowCap=%v, want true", name, v)
				}
			}
			if slug == "plus" {
				if v, _ := m["guardPercent"].(float64); v != 0 {
					t.Errorf("%s plus raw guardPercent=%v, want explicit 0", name, v)
				}
				if v, _ := m["lowCap"].(bool); v != false {
					t.Errorf("%s plus raw lowCap=%v, want explicit false", name, v)
				}
			}
			if warn, _ := m["warnAtPercent"].([]any); len(warn) != 1 || warn[0] != float64(80) {
				t.Errorf("%s plan %q raw warnAtPercent=%v, want [80]", name, slug, m["warnAtPercent"])
			}
			for _, key := range []string{"allowCredits", "allowOverage", "allowApiBilledFallback"} {
				if v, _ := m[key].(bool); v != false {
					t.Errorf("%s plan %q raw %s=%v, want explicit false", name, slug, key, v)
				}
			}
		}
	}
}
