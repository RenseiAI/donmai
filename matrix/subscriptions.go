package matrix

import (
	"fmt"
	"sort"

	"github.com/RenseiAI/donmai/agent"
)

// Subscription catalog (release metadata only): one row per issuer with
// its id, direct/gateway kind, login kinds, and plans. The harness ceiling
// is DERIVED from cells matching the issuer sign-in path and subscription
// auth (endpoint + host + auth, never the company alone); a consumer may
// narrow it. Reachable models are explicit and independent of the issuer
// id. Program membership stays in per-account observations.

// SubscriptionIssuerKind names how an issuer bills.
type SubscriptionIssuerKind string

// Subscription issuer kinds billed through the catalog.
const (
	SubscriptionIssuerDirect  SubscriptionIssuerKind = "direct"
	SubscriptionIssuerGateway SubscriptionIssuerKind = "gateway"
)

// SubscriptionPlanDefaults holds a plan's policy defaults. Always
// serialized (no omitempty): false and 0 are values, not absence.
type SubscriptionPlanDefaults struct {
	GuardPercent           int   `json:"guardPercent"`
	WarnAtPercent          []int `json:"warnAtPercent"`
	AllowCredits           bool  `json:"allowCredits"`
	AllowOverage           bool  `json:"allowOverage"`
	AllowAPIBilledFallback bool  `json:"allowApiBilledFallback"`
}

// SubscriptionReachableModel is one (model provider, model ID) pair a plan
// reaches. ModelProvider is the serving company, never the issuer id.
type SubscriptionReachableModel struct {
	ModelProvider agent.Company `json:"modelProvider"`
	ModelID       string        `json:"modelId"`
}

// SubscriptionPlanRow is one plan in the generated catalog.
type SubscriptionPlanRow struct {
	Slug                string                       `json:"slug"`
	DisplayName         string                       `json:"displayName"`
	AllowanceShape      string                       `json:"allowanceShape"`
	LowCap              bool                         `json:"lowCap"`
	Defaults            SubscriptionPlanDefaults     `json:"defaults"`
	ReachableModels     []SubscriptionReachableModel `json:"reachableModels"`
	CompatibleHarnesses []agent.HarnessName          `json:"compatibleHarnesses"`
	BillingModes        []string                     `json:"billingModes"`
}

// SubscriptionProviderRow is one issuer in the generated catalog.
type SubscriptionProviderRow struct {
	ID         string                 `json:"id"`
	Kind       SubscriptionIssuerKind `json:"kind"`
	LoginKinds []string               `json:"loginKinds"`
	Plans      []SubscriptionPlanRow  `json:"plans"`
}

// subscriptionSignIn is one login kind's sign-in path: the exact
// (endpoint, host, auth) slice whose harnesses form the plan ceiling.
type subscriptionSignIn struct {
	Endpoint agent.Company
	Host     agent.ServingHost
	AuthMode agent.AuthMode
}

type subscriptionLoginDeclaration struct {
	LoginKind string
	SignIn    subscriptionSignIn
}

type subscriptionPlanDeclaration struct {
	Slug, DisplayName, AllowanceShape string
	LowCap                            bool
	Defaults                          SubscriptionPlanDefaults
	ReachableModels                   []SubscriptionReachableModel
	BillingModes                      []string
}

type subscriptionIssuerDeclaration struct {
	ID     string
	Kind   SubscriptionIssuerKind
	Logins []subscriptionLoginDeclaration
	Plans  []subscriptionPlanDeclaration
}

// subscriptionIssuerDeclarations is the single declaration projected into
// both JSON artifacts: one direct issuer, team low-cap and plus ordinary.
func subscriptionIssuerDeclarations() []subscriptionIssuerDeclaration {
	oauthSignIn := subscriptionSignIn{Endpoint: agent.CompanyOpenAI, Host: agent.HostOAuthCLI, AuthMode: agent.AuthHostSession}
	oauthModel := []SubscriptionReachableModel{{ModelProvider: agent.CompanyOpenAI, ModelID: "gpt-6-sol"}}
	team := subscriptionPlanDeclaration{
		Slug: "team", DisplayName: "Team", AllowanceShape: "rolling", LowCap: true,
		Defaults: SubscriptionPlanDefaults{GuardPercent: 50, WarnAtPercent: []int{80}}, ReachableModels: oauthModel, BillingModes: []string{"allowance"},
	}
	plus := team
	plus.Slug, plus.DisplayName, plus.LowCap, plus.Defaults.GuardPercent = "plus", "Plus", false, 0
	return []subscriptionIssuerDeclaration{{
		ID: "openai", Kind: SubscriptionIssuerDirect,
		Logins: []subscriptionLoginDeclaration{{LoginKind: "chatgpt", SignIn: oauthSignIn}},
		Plans:  []subscriptionPlanDeclaration{team, plus},
	}}
}

// buildSubscriptionProviders projects declarations into catalog rows.
// Billing order and reachable pairs keep declaration order; the ceiling
// is sorted unique.
func buildSubscriptionProviders(cells []HarnessEndpointCell, decls []subscriptionIssuerDeclaration) ([]SubscriptionProviderRow, error) {
	seenIssuer := map[string]bool{}
	rows := make([]SubscriptionProviderRow, 0, len(decls))
	for _, decl := range decls {
		if decl.ID == "" || seenIssuer[decl.ID] {
			return nil, fmt.Errorf("subscription provider: bad or duplicate issuer id %q", decl.ID)
		}
		seenIssuer[decl.ID] = true
		if decl.Kind != SubscriptionIssuerDirect && decl.Kind != SubscriptionIssuerGateway {
			return nil, fmt.Errorf("subscription provider %q: unknown kind %q", decl.ID, decl.Kind)
		}
		if len(decl.Logins) == 0 || len(decl.Plans) == 0 {
			return nil, fmt.Errorf("subscription provider %q: needs login kinds and plans", decl.ID)
		}
		seenLogin := map[string]bool{}
		loginKinds := make([]string, 0, len(decl.Logins))
		for _, login := range decl.Logins {
			if login.LoginKind == "" || seenLogin[login.LoginKind] {
				return nil, fmt.Errorf("subscription provider %q: bad login kind %q", decl.ID, login.LoginKind)
			}
			seenLogin[login.LoginKind] = true
			if login.SignIn.Endpoint == "" || login.SignIn.Host == "" || !isCanonicalAuthMode(login.SignIn.AuthMode) {
				return nil, fmt.Errorf("subscription provider %q: bad sign-in path for login %q", decl.ID, login.LoginKind)
			}
			loginKinds = append(loginKinds, login.LoginKind)
		}
		seenSlug := map[string]bool{}
		plans := make([]SubscriptionPlanRow, 0, len(decl.Plans))
		ceiling := subscriptionHarnessCeiling(cells, decl.Logins)
		if len(ceiling) == 0 {
			return nil, fmt.Errorf("subscription provider %q: no harness matches the declared sign-in path", decl.ID)
		}
		for _, plan := range decl.Plans {
			bad := plan.Slug == "" || seenSlug[plan.Slug] || plan.DisplayName == "" || plan.AllowanceShape == "" ||
				plan.Defaults.WarnAtPercent == nil || len(plan.ReachableModels) == 0 || len(plan.BillingModes) == 0
			seenMode := map[string]bool{}
			for _, reachable := range plan.ReachableModels {
				bad = bad || reachable.ModelProvider == "" || reachable.ModelID == ""
			}
			for _, mode := range plan.BillingModes {
				bad = bad || mode == "" || seenMode[mode]
				seenMode[mode] = true
			}
			if bad {
				return nil, fmt.Errorf("subscription provider %q: bad plan declaration %q", decl.ID, plan.Slug)
			}
			seenSlug[plan.Slug] = true
			plans = append(plans, SubscriptionPlanRow{
				Slug: plan.Slug, DisplayName: plan.DisplayName, AllowanceShape: plan.AllowanceShape, LowCap: plan.LowCap,
				Defaults: SubscriptionPlanDefaults{
					GuardPercent: plan.Defaults.GuardPercent, WarnAtPercent: append([]int(nil), plan.Defaults.WarnAtPercent...),
					AllowCredits: plan.Defaults.AllowCredits, AllowOverage: plan.Defaults.AllowOverage,
					AllowAPIBilledFallback: plan.Defaults.AllowAPIBilledFallback,
				},
				ReachableModels:     append([]SubscriptionReachableModel(nil), plan.ReachableModels...),
				CompatibleHarnesses: ceiling, BillingModes: append([]string(nil), plan.BillingModes...),
			})
		}
		rows = append(rows, SubscriptionProviderRow{ID: decl.ID, Kind: decl.Kind, LoginKinds: loginKinds, Plans: plans})
	}
	return rows, nil
}

// subscriptionHarnessCeiling returns the sorted unique harnesses from
// cells matching an issuer sign-in path. Host and auth must match exactly.
func subscriptionHarnessCeiling(cells []HarnessEndpointCell, logins []subscriptionLoginDeclaration) []agent.HarnessName {
	seen := map[agent.HarnessName]bool{}
	var ceiling []agent.HarnessName
	for _, c := range cells {
		for _, login := range logins {
			if c.Endpoint == login.SignIn.Endpoint && c.Host == login.SignIn.Host && containsAuth(c.AuthModes, login.SignIn.AuthMode) {
				if !seen[c.Harness] {
					seen[c.Harness] = true
					ceiling = append(ceiling, c.Harness)
				}
				break
			}
		}
	}
	sort.Slice(ceiling, func(i, j int) bool { return ceiling[i] < ceiling[j] })
	if ceiling == nil {
		ceiling = []agent.HarnessName{}
	}
	return ceiling
}

func cloneSubscriptionProviders(in []SubscriptionProviderRow) []SubscriptionProviderRow {
	if in == nil {
		return []SubscriptionProviderRow{}
	}
	out := make([]SubscriptionProviderRow, len(in))
	for i, row := range in {
		out[i] = SubscriptionProviderRow{
			ID: row.ID, Kind: row.Kind,
			LoginKinds: append([]string(nil), row.LoginKinds...), Plans: append([]SubscriptionPlanRow(nil), row.Plans...),
		}
		for j, plan := range row.Plans {
			out[i].Plans[j].Defaults.WarnAtPercent = append([]int(nil), plan.Defaults.WarnAtPercent...)
			out[i].Plans[j].ReachableModels = append([]SubscriptionReachableModel(nil), plan.ReachableModels...)
			out[i].Plans[j].CompatibleHarnesses = append([]agent.HarnessName(nil), plan.CompatibleHarnesses...)
			out[i].Plans[j].BillingModes = append([]string(nil), plan.BillingModes...)
		}
	}
	return out
}
