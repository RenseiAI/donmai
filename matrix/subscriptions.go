package matrix

import (
	"github.com/RenseiAI/donmai/agent"
)

// Subscription catalog (additive p1.5 section): the deterministic,
// generator-owned definition of each direct subscription provider and its
// reported plan slugs. Consumers match provider id plus plan slug against
// host-observed account identity; membership, usage and remaining quota
// stay host-observed and are never declared here. Listing a billing mode
// describes compatibility only and never authorizes spending.
//
// The issuer id (for example openai-chatgpt) is distinct from the model
// provider id (for example openai): the former names who issued the
// subscription, the latter names who authored the model.

// SubscriptionProviderKind names how the subscription is served: direct
// (first-party subscription login) or gateway (translated upstream).
type SubscriptionProviderKind string

// Subscription provider kinds. Only direct ships a row today; gateway is
// reserved as the other kind so a future translated provider joins without
// a kind-rename.
const (
	SubscriptionProviderKindDirect  SubscriptionProviderKind = "direct"
	SubscriptionProviderKindGateway SubscriptionProviderKind = "gateway"
)

// SubscriptionAllowance is the percentage-window allowance shape for a plan.
// Type is always "percentage-window" in this seed: quota is observed as
// rolling percentage windows on the host.
type SubscriptionAllowance struct {
	Type string `json:"type"`
}

// SubscriptionModelRef is one compatible catalog model: the model provider
// (endpoint company) plus the catalog model id reachable on the verified
// subscription cell.
type SubscriptionModelRef struct {
	ModelProvider string `json:"modelProvider"`
	ModelID       string `json:"modelId"`
}

// SubscriptionPlan is one reported plan slug and its definition defaults.
// GuardPercent, WarnAtPercent and the three allow-* permissions are explicit
// definition defaults (zero/false are emitted, never omitted) so a consumer
// can distinguish an explicit zero or false from an absent key.
type SubscriptionPlan struct {
	Slug                   string                 `json:"slug"`
	DisplayName            string                 `json:"displayName"`
	Allowance              SubscriptionAllowance  `json:"allowance"`
	Models                 []SubscriptionModelRef `json:"models"`
	Harnesses              []agent.HarnessName    `json:"harnesses"`
	BillingModes           []string               `json:"billingModes"`
	LowCap                 bool                   `json:"lowCap"`
	GuardPercent           int                    `json:"guardPercent"`
	WarnAtPercent          []int                  `json:"warnAtPercent"`
	AllowCredits           bool                   `json:"allowCredits"`
	AllowOverage           bool                   `json:"allowOverage"`
	AllowAPIBilledFallback bool                   `json:"allowApiBilledFallback"`
}

// SubscriptionProvider is one subscription issuer: its stable id, serving
// kind, issued login kinds, and nested plans in stable slug order.
type SubscriptionProvider struct {
	ID         string                   `json:"id"`
	Kind       SubscriptionProviderKind `json:"kind"`
	LoginKinds []agent.AuthMode         `json:"loginKinds"`
	Plans      []SubscriptionPlan       `json:"plans"`
}

// subscriptionProviders is the single hand-authored subscription catalog.
// Stable order: one provider today, plans plus then team.
var subscriptionProviders = []SubscriptionProvider{
	{
		ID:         "openai-chatgpt",
		Kind:       SubscriptionProviderKindDirect,
		LoginKinds: []agent.AuthMode{agent.AuthHostSession},
		Plans: []SubscriptionPlan{
			{
				Slug:        "plus",
				DisplayName: "Plus",
				Allowance:   SubscriptionAllowance{Type: "percentage-window"},
				Models: []SubscriptionModelRef{
					{ModelProvider: string(agent.CompanyOpenAI), ModelID: "gpt-6-sol"},
				},
				Harnesses:              []agent.HarnessName{agent.HarnessCodex},
				BillingModes:           []string{"allowance", "credits", "overage", "api-metered"},
				LowCap:                 false,
				GuardPercent:           0,
				WarnAtPercent:          []int{80},
				AllowCredits:           false,
				AllowOverage:           false,
				AllowAPIBilledFallback: false,
			},
			{
				Slug:        "team",
				DisplayName: "Team",
				Allowance:   SubscriptionAllowance{Type: "percentage-window"},
				Models: []SubscriptionModelRef{
					{ModelProvider: string(agent.CompanyOpenAI), ModelID: "gpt-6-sol"},
				},
				Harnesses:              []agent.HarnessName{agent.HarnessCodex},
				BillingModes:           []string{"allowance", "credits", "overage", "api-metered"},
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

// cloneSubscriptionProviders returns a deep copy of the hand-authored
// catalog so Build and Render share identical rows without aliasing the
// package-level literals.
func cloneSubscriptionProviders() []SubscriptionProvider {
	out := make([]SubscriptionProvider, 0, len(subscriptionProviders))
	for _, p := range subscriptionProviders {
		cp := SubscriptionProvider{
			ID:         p.ID,
			Kind:       p.Kind,
			LoginKinds: append([]agent.AuthMode(nil), p.LoginKinds...),
			Plans:      make([]SubscriptionPlan, 0, len(p.Plans)),
		}
		for _, plan := range p.Plans {
			pp := SubscriptionPlan{
				Slug:                   plan.Slug,
				DisplayName:            plan.DisplayName,
				Allowance:              plan.Allowance,
				Models:                 append([]SubscriptionModelRef(nil), plan.Models...),
				Harnesses:              append([]agent.HarnessName(nil), plan.Harnesses...),
				BillingModes:           append([]string(nil), plan.BillingModes...),
				LowCap:                 plan.LowCap,
				GuardPercent:           plan.GuardPercent,
				WarnAtPercent:          append([]int(nil), plan.WarnAtPercent...),
				AllowCredits:           plan.AllowCredits,
				AllowOverage:           plan.AllowOverage,
				AllowAPIBilledFallback: plan.AllowAPIBilledFallback,
			}
			cp.Plans = append(cp.Plans, pp)
		}
		out = append(out, cp)
	}
	return out
}
