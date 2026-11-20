package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// fixtureGatewayDeclarations pairs the production direct issuer with a
// synthetic multi-provider gateway: one login, two model providers, four
// ordered billing modes over the evidenced gateway cell.
func fixtureGatewayDeclarations() []subscriptionIssuerDeclaration {
	oauth := subscriptionSignIn{Endpoint: agent.CompanyOpenAI, Host: agent.HostOAuthCLI, AuthMode: agent.AuthHostSession}
	gateway := subscriptionSignIn{Endpoint: agent.CompanyOpenAI, Host: agent.HostGateway, AuthMode: agent.AuthBYOK}
	oauthModel := []SubscriptionReachableModel{{ModelProvider: agent.CompanyOpenAI, ModelID: "gpt-6-sol"}}
	gwModels := []SubscriptionReachableModel{
		{ModelProvider: agent.CompanyOpenAI, ModelID: "gpt-5-codex"},
		{ModelProvider: agent.CompanyAnthropic, ModelID: "claude-sonnet-4-5"},
	}
	allowanceOnly, allModes := []string{"allowance"}, []string{"allowance", "credits", "overage", "api-metered"}
	return []subscriptionIssuerDeclaration{
		{
			ID: "openai", Kind: SubscriptionIssuerDirect,
			Logins: []subscriptionLoginDeclaration{{LoginKind: "chatgpt", SignIn: oauth}},
			Plans: []subscriptionPlanDeclaration{{
				Slug: "plus", DisplayName: "Plus", AllowanceShape: "rolling",
				Defaults:        SubscriptionPlanDefaults{GuardPercent: 0, WarnAtPercent: []int{80}},
				ReachableModels: oauthModel, BillingModes: allowanceOnly,
			}},
		},
		{
			ID: "synthetic-gateway", Kind: SubscriptionIssuerGateway,
			Logins: []subscriptionLoginDeclaration{{LoginKind: "gatewaytoken", SignIn: gateway}},
			Plans: []subscriptionPlanDeclaration{{
				Slug: "standard", DisplayName: "Standard", AllowanceShape: "rolling",
				Defaults:        SubscriptionPlanDefaults{GuardPercent: 0, WarnAtPercent: []int{80}},
				ReachableModels: gwModels, BillingModes: allModes,
			}},
		},
	}
}

// subscriptionRows decodes the subscription section from both documents,
// failing when the release projection drops the section.
func subscriptionRows(t *testing.T, arts *Artifacts) []SubscriptionProviderRow {
	t.Helper()
	rows := map[string][]SubscriptionProviderRow{}
	for _, name := range []string{FileCapabilityMatrix, FileMatrix} {
		var doc struct {
			SubscriptionProviders []SubscriptionProviderRow `json:"subscriptionProviders"`
		}
		if err := json.Unmarshal(arts.Files[name], &doc); err != nil {
			t.Fatalf("%s: unmarshal subscriptionProviders: %v", name, err)
		}
		rows[name] = doc.SubscriptionProviders
	}
	if !reflect.DeepEqual(rows[FileCapabilityMatrix], rows[FileMatrix]) {
		t.Fatalf("release projection diverges from the full document")
	}
	return rows[FileCapabilityMatrix]
}

// rawSubscriptionIssuers decodes one artifact's subscription section into
// raw maps for exact wire-key assertions.
func rawSubscriptionIssuers(t *testing.T, arts *Artifacts, name string) []map[string]any {
	t.Helper()
	var doc struct {
		SubscriptionProviders []map[string]any `json:"subscriptionProviders"`
	}
	if err := json.Unmarshal(arts.Files[name], &doc); err != nil {
		t.Fatalf("%s: unmarshal: %v", name, err)
	}
	return doc.SubscriptionProviders
}

// TestSubscriptions_ProductionCatalog is AC-1 + AC-3: both documents carry
// the declared issuer and plans (exact login kinds, slugs, names, allowance
// shape, reachable pair, derived ceiling) and regeneration is byte-identical
// to the committed goldens. Defaults are present with values on the raw JSON.
func TestSubscriptions_ProductionCatalog(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	arts, err := built.Render()
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	full := subscriptionRows(t, arts)
	if len(full) != 1 {
		t.Fatalf("issuers = %d; want exactly the direct issuer", len(full))
	}
	issuer := full[0]
	if issuer.ID != "openai" || issuer.Kind != SubscriptionIssuerDirect {
		t.Errorf("issuer = %q/%q; want openai/direct", issuer.ID, issuer.Kind)
	}
	if !reflect.DeepEqual(issuer.LoginKinds, []string{"chatgpt"}) {
		t.Errorf("loginKinds = %q; want [chatgpt]", issuer.LoginKinds)
	}
	if len(issuer.Plans) != 2 { // declaration order: team, plus
		t.Fatalf("plans = %d; want team and plus", len(issuer.Plans))
	}
	team, plus := issuer.Plans[0], issuer.Plans[1]
	if team.Slug != "team" || team.DisplayName != "Team" || team.AllowanceShape != "rolling" || !team.LowCap {
		t.Errorf("team plan = %+v; want team/Team/rolling/lowCap", team)
	}
	if plus.Slug != "plus" || plus.DisplayName != "Plus" || plus.AllowanceShape != "rolling" || plus.LowCap {
		t.Errorf("plus plan = %+v; want plus/Plus/rolling/non-lowCap", plus)
	}
	for _, plan := range []SubscriptionPlanRow{team, plus} {
		if want := []SubscriptionReachableModel{{ModelProvider: agent.CompanyOpenAI, ModelID: "gpt-6-sol"}}; !reflect.DeepEqual(plan.ReachableModels, want) {
			t.Errorf("plan %q reachable = %+v; want %+v", plan.Slug, plan.ReachableModels, want)
		}
		if !reflect.DeepEqual(plan.CompatibleHarnesses, []agent.HarnessName{agent.HarnessCodex}) {
			t.Errorf("plan %q ceiling = %q; want the exact sign-in harness only", plan.Slug, plan.CompatibleHarnesses)
		}
		if !reflect.DeepEqual(plan.BillingModes, []string{"allowance"}) {
			t.Errorf("plan %q billingModes = %q; want [allowance]", plan.Slug, plan.BillingModes)
		}
	}
	assertRawDefaults(t, arts, team, plus)
	again, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	againArts, err := again.Render()
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	for _, name := range []string{FileCapabilityMatrix, FileMatrix} {
		if !bytes.Equal(arts.Files[name], againArts.Files[name]) {
			t.Errorf("%s: two fresh regenerations differ", name)
		}
		if committed, err := os.ReadFile(name); err != nil {
			t.Fatalf("read committed %s: %v", name, err)
		} else if !bytes.Equal(againArts.Files[name], committed) {
			t.Errorf("%s is stale: committed bytes != fresh regenerate. Run `make generate`.", name)
		}
	}
}

// assertRawDefaults checks defaults for value (typed rows) and presence
// (exact wire keys on the raw JSON of both documents).
func assertRawDefaults(t *testing.T, arts *Artifacts, team, plus SubscriptionPlanRow) {
	t.Helper()
	if team.Defaults.GuardPercent != 50 || plus.Defaults.GuardPercent != 0 {
		t.Errorf("guardPercent team=%d plus=%d; want 50/0", team.Defaults.GuardPercent, plus.Defaults.GuardPercent)
	}
	for _, plan := range []SubscriptionPlanRow{team, plus} {
		if !reflect.DeepEqual(plan.Defaults.WarnAtPercent, []int{80}) {
			t.Errorf("plan %q warnAtPercent = %v; want [80]", plan.Slug, plan.Defaults.WarnAtPercent)
		}
		if plan.Defaults.AllowCredits || plan.Defaults.AllowOverage || plan.Defaults.AllowAPIBilledFallback {
			t.Errorf("plan %q permissions = %+v; want explicit false", plan.Slug, plan.Defaults)
		}
	}
	for _, name := range []string{FileCapabilityMatrix, FileMatrix} {
		for _, issuer := range rawSubscriptionIssuers(t, arts, name) {
			for _, planRaw := range issuer["plans"].([]any) {
				plan := planRaw.(map[string]any)
				assertKeys(t, name, "defaults", plan["defaults"].(map[string]any),
					[]string{"guardPercent", "warnAtPercent", "allowCredits", "allowOverage", "allowApiBilledFallback"})
			}
		}
	}
}

// TestSubscriptions_GatewayContract is AC-2 + AC-4: the synthetic gateway
// renders with its kind, two model-provider pairs, and sign-in ceiling in
// both documents; billing modes keep order, wire keys are exact, and no
// program membership rides along.
func TestSubscriptions_GatewayContract(t *testing.T) {
	built, err := buildWithSubscriptionDeclarations(context.Background(), nil, nil, fixtureGatewayDeclarations())
	if err != nil {
		t.Fatalf("buildWithSubscriptionDeclarations(): %v", err)
	}
	arts, err := built.Render()
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	full := subscriptionRows(t, arts)
	if len(full) != 2 {
		t.Fatalf("issuers = %d; want direct + gateway", len(full))
	}
	if full[0].Kind != SubscriptionIssuerDirect || full[0].ID != "openai" {
		t.Errorf("first issuer = %q/%q; want the direct issuer intact", full[0].ID, full[0].Kind)
	}
	gateway := full[1]
	if gateway.ID != "synthetic-gateway" || gateway.Kind != SubscriptionIssuerGateway {
		t.Fatalf("gateway issuer = %q/%q; want synthetic-gateway/gateway", gateway.ID, gateway.Kind)
	}
	plan := gateway.Plans[0]
	if len(plan.ReachableModels) != 2 || plan.ReachableModels[0].ModelProvider == plan.ReachableModels[1].ModelProvider {
		t.Fatalf("reachable = %+v; want two distinct model-provider pairs", plan.ReachableModels)
	}
	for _, reachable := range plan.ReachableModels {
		if reachable.ModelProvider == agent.Company(gateway.ID) || reachable.ModelID == "" {
			t.Errorf("reachable pair %+v substitutes the issuer id for the model provider", reachable)
		}
	}
	if !reflect.DeepEqual(plan.CompatibleHarnesses, []agent.HarnessName{agent.HarnessOpenCode}) {
		t.Errorf("gateway ceiling = %q; want the declared sign-in harness only", plan.CompatibleHarnesses)
	}
	if want := []string{"allowance", "credits", "overage", "api-metered"}; !reflect.DeepEqual(plan.BillingModes, want) {
		t.Errorf("billingModes = %q; want exact order %q", plan.BillingModes, want)
	}
	for _, name := range []string{FileCapabilityMatrix, FileMatrix} {
		for _, issuer := range rawSubscriptionIssuers(t, arts, name) {
			assertKeys(t, name, "issuer", issuer, []string{"id", "kind", "loginKinds", "plans"})
			for _, planRaw := range issuer["plans"].([]any) {
				p := planRaw.(map[string]any)
				assertKeys(t, name, "plan", p, []string{"slug", "displayName", "allowanceShape", "lowCap", "defaults", "reachableModels", "compatibleHarnesses", "billingModes"})
				assertKeys(t, name, "defaults", p["defaults"].(map[string]any),
					[]string{"guardPercent", "warnAtPercent", "allowCredits", "allowOverage", "allowApiBilledFallback"})
				for _, modelRaw := range p["reachableModels"].([]any) {
					assertKeys(t, name, "reachableModel", modelRaw.(map[string]any), []string{"modelProvider", "modelId"})
				}
			}
		}
		section := strings.ToLower(string(arts.Files[name]))
		idx := strings.Index(section, `"subscriptionproviders"`)
		if idx < 0 {
			t.Fatalf("%s: subscription section missing from artifact", name)
		}
		if strings.Contains(section[idx:], "program") {
			t.Errorf("%s: subscription section carries account program membership", name)
		}
	}
}

func assertKeys(t *testing.T, doc, label string, got map[string]any, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %s has %d keys; want exactly %v", doc, label, len(got), want)
		return
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("%s: %s omits key %q", doc, label, key)
		}
	}
}
