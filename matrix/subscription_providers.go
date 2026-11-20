package matrix

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"

	"github.com/RenseiAI/donmai/agent"
)

// SubscriptionProvidersContractABI pins the catalog contract a deployment
// vendors (subscription-providers.json) and parses exactly.
const SubscriptionProvidersContractABI = "subscription-providers/v1"

// SubscriptionProviderCatalog lists the companies or gateways that issue and
// bill a login. It describes and supplies defaults; it never grants. A
// deployment may add rows, set defaults and narrow harnesses, never widen them.
type SubscriptionProviderCatalog struct {
	ContractABI string                 `json:"contractAbi"`
	Providers   []SubscriptionProvider `json:"providers"`
}

// SubscriptionProvider is one catalog row.
type SubscriptionProvider struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	DisplayName string   `json:"displayName"`
	LoginKinds  []string `json:"loginKinds"`
	// Harnesses is the compatibility CEILING (matrix harness ids).
	Harnesses []agent.HarnessName `json:"harnesses"`
	// BillingModes is what happens past the included allowance, in order.
	BillingModes []string                    `json:"billingModes"`
	Models       []SubscriptionProviderModel `json:"models"`
	Plans        []SubscriptionPlan          `json:"plans"`
}

// SubscriptionProviderModel is a matrix endpoint company the provider reaches.
type SubscriptionProviderModel struct {
	Endpoint agent.Company `json:"endpoint"`
	ModelIDs []string      `json:"modelIds,omitempty"`
}

// SubscriptionPlan is matched by the slug the scanner reports; an unmatched
// plan is uncatalogued and takes the kind's built-in defaults.
type SubscriptionPlan struct {
	Slug           string         `json:"slug"`
	DisplayName    string         `json:"displayName"`
	LowCap         bool           `json:"lowCap"`
	AllowanceUnits []string       `json:"allowanceUnits"`
	Defaults       map[string]any `json:"defaults"`
}

// guard50 is the low-cap default (keep half of each window unused).
func guard50() map[string]any { return map[string]any{"guardPercent": 50} }

// subscriptionProviders lists providers a matrix harness signs in with (a
// host-session cell); gateways join once the matrix realizes them.
var subscriptionProviders = []SubscriptionProvider{
	{
		ID: "openai-chatgpt", Kind: "direct", DisplayName: "ChatGPT",
		LoginKinds:   []string{"subscription-login"},
		Harnesses:    []agent.HarnessName{agent.HarnessCodex},
		BillingModes: []string{"allowance", "credits"},
		Models:       []SubscriptionProviderModel{{Endpoint: agent.CompanyOpenAI}},
		// Slugs are the planType values the codex rate-limit read reports.
		Plans: []SubscriptionPlan{
			{Slug: "free", DisplayName: "Free", LowCap: true, AllowanceUnits: []string{"percent"}, Defaults: guard50()},
			{Slug: "go", DisplayName: "Go", LowCap: true, AllowanceUnits: []string{"percent"}, Defaults: guard50()},
			{Slug: "plus", DisplayName: "Plus", LowCap: true, AllowanceUnits: []string{"percent"}, Defaults: guard50()},
			{Slug: "pro", DisplayName: "Pro", AllowanceUnits: []string{"percent"}},
			{Slug: "team", DisplayName: "Team", LowCap: true, AllowanceUnits: []string{"percent"}, Defaults: guard50()},
			{Slug: "business", DisplayName: "Business", LowCap: true, AllowanceUnits: []string{"percent"}, Defaults: guard50()},
			{Slug: "enterprise", DisplayName: "Enterprise", AllowanceUnits: []string{"percent"}},
			{Slug: "edu", DisplayName: "Edu", AllowanceUnits: []string{"percent"}},
		},
	},
	{
		ID: "anthropic-claude", Kind: "direct", DisplayName: "Claude",
		LoginKinds:   []string{"subscription-login"},
		Harnesses:    []agent.HarnessName{agent.HarnessClaudeCode},
		BillingModes: []string{"allowance", "overage", "api-metered"},
		Models:       []SubscriptionProviderModel{{Endpoint: agent.CompanyAnthropic}},
		// No scanner reports a Claude plan yet. Matching is by reported slug,
		// so these rows apply once one does; any other slug is uncatalogued.
		Plans: []SubscriptionPlan{
			{Slug: "pro", DisplayName: "Pro", LowCap: true, AllowanceUnits: []string{"percent"}, Defaults: guard50()},
			{Slug: "max", DisplayName: "Max", AllowanceUnits: []string{"percent"}},
			{Slug: "team", DisplayName: "Team", AllowanceUnits: []string{"percent"}},
			{Slug: "enterprise", DisplayName: "Enterprise", AllowanceUnits: []string{"percent"}},
		},
	},
}

var (
	catalogSlugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	providerKinds       = []string{"direct", "gateway"}
	providerLoginKinds  = []string{"subscription-login"}
	providerBillingMode = []string{"allowance", "credits", "overage", "api-metered"}
	planAllowanceUnits  = []string{"percent", "tokens"}
)

// buildSubscriptionProviders validates the declared catalog against the
// matrix and returns it sorted by provider id, plans in declared order.
func buildSubscriptionProviders(declared []SubscriptionProvider, harnessByName map[agent.HarnessName]HarnessRow, cells []HarnessEndpointCell, endpoints []EndpointRow) (SubscriptionProviderCatalog, error) {
	endpointByCompany := map[agent.Company]EndpointRow{}
	for _, e := range endpoints {
		endpointByCompany[e.Company] = e
	}
	seen := map[string]bool{}
	out := make([]SubscriptionProvider, 0, len(declared))
	for _, p := range declared {
		if seen[p.ID] {
			return SubscriptionProviderCatalog{}, fmt.Errorf("subscription provider %q: duplicate id", p.ID)
		}
		seen[p.ID] = true
		if err := validateSubscriptionProvider(p, harnessByName, cells, endpointByCompany); err != nil {
			return SubscriptionProviderCatalog{}, fmt.Errorf("subscription provider %q: %w", p.ID, err)
		}
		out = append(out, cloneSubscriptionProvider(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return SubscriptionProviderCatalog{ContractABI: SubscriptionProvidersContractABI, Providers: out}, nil
}

func validateSubscriptionProvider(p SubscriptionProvider, harnessByName map[agent.HarnessName]HarnessRow, cells []HarnessEndpointCell, endpointByCompany map[agent.Company]EndpointRow) error {
	if !catalogSlugPattern.MatchString(p.ID) {
		return fmt.Errorf("id must match %s", catalogSlugPattern)
	}
	if !slices.Contains(providerKinds, p.Kind) {
		return fmt.Errorf("kind %q not in %v", p.Kind, providerKinds)
	}
	if p.DisplayName == "" {
		return fmt.Errorf("displayName is empty")
	}
	if err := checkVocab("loginKinds", p.LoginKinds, providerLoginKinds); err != nil {
		return err
	}
	if err := checkVocab("billingModes", p.BillingModes, providerBillingMode); err != nil {
		return err
	}
	if p.BillingModes[0] != "allowance" {
		return fmt.Errorf("billingModes must start with %q", "allowance")
	}
	var reached []agent.Company
	for _, m := range p.Models {
		reached = append(reached, m.Endpoint)
	}
	if err := checkVocab("models endpoint", reached, slices.Sorted(maps.Keys(endpointByCompany))); err != nil {
		return fmt.Errorf("%w: endpoints must be matrix endpoint companies", err)
	}
	for _, m := range p.Models {
		for _, id := range m.ModelIDs {
			if !slices.ContainsFunc(endpointByCompany[m.Endpoint].Models, func(d agent.ModelDesc) bool { return d.ID == id }) {
				return fmt.Errorf("model id %q is not declared by endpoint %q", id, m.Endpoint)
			}
		}
	}
	if err := checkVocab("harnesses", p.Harnesses, slices.Sorted(maps.Keys(harnessByName))); err != nil {
		return fmt.Errorf("%w: harnesses must be matrix harness ids", err)
	}
	for _, h := range p.Harnesses { // two-axis validity: a host-session cell on a reached endpoint
		if !slices.ContainsFunc(cells, func(c HarnessEndpointCell) bool {
			return c.Harness == h && slices.Contains(reached, c.Endpoint) && slices.Contains(c.AuthModes, agent.AuthHostSession)
		}) {
			return fmt.Errorf("harness %q has no host-session cell on the provider's endpoints", h)
		}
	}
	slugs := map[string]bool{}
	for _, plan := range p.Plans {
		if !catalogSlugPattern.MatchString(plan.Slug) {
			return fmt.Errorf("plan slug %q must match %s", plan.Slug, catalogSlugPattern)
		}
		if slugs[plan.Slug] {
			return fmt.Errorf("plan slug %q listed twice", plan.Slug)
		}
		slugs[plan.Slug] = true
		if err := validateSubscriptionPlan(plan); err != nil {
			return fmt.Errorf("plan %q: %w", plan.Slug, err)
		}
	}
	return nil
}

func validateSubscriptionPlan(plan SubscriptionPlan) error {
	if plan.DisplayName == "" {
		return fmt.Errorf("displayName is empty")
	}
	if err := checkVocab("allowanceUnits", plan.AllowanceUnits, planAllowanceUnits); err != nil {
		return err
	}
	for key, value := range plan.Defaults {
		switch key {
		case "guardPercent":
			if v, ok := value.(int); !ok || v < 0 || v > 100 {
				return fmt.Errorf("default guardPercent must be an integer 0-100, got %v", value)
			}
		case "allowCredits", "allowOverage", "allowApiBilledFallback":
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("default %s must be a boolean, got %v", key, value)
			}
		case "warnAtPercent":
			list, ok := value.([]int)
			if !ok {
				return fmt.Errorf("default warnAtPercent must be a list of integers, got %v", value)
			}
			for _, v := range list {
				if v < 0 || v > 100 {
					return fmt.Errorf("default warnAtPercent entry %d outside 0-100", v)
				}
			}
		case "enabled":
			return fmt.Errorf("enabled is never a default: enablement is set explicitly per login")
		default:
			return fmt.Errorf("default %q is not a subscription-login policy field", key)
		}
	}
	if _, ok := plan.Defaults["guardPercent"]; plan.LowCap && !ok {
		return fmt.Errorf("lowCap plan must set a guardPercent default")
	}
	return nil
}

// checkVocab requires a non-empty, duplicate-free list drawn from vocab.
func checkVocab[T ~string](field string, values, vocab []T) error {
	if len(values) == 0 {
		return fmt.Errorf("%s is empty", field)
	}
	for i, v := range values {
		if !slices.Contains(vocab, v) {
			return fmt.Errorf("%s value %q not in %v", field, v, vocab)
		}
		if slices.Contains(values[:i], v) {
			return fmt.Errorf("%s value %q listed twice", field, v)
		}
	}
	return nil
}

// cloneSubscriptionProvider copies a row so the built catalog cannot mutate
// the declared data, rendering absent defaults as {} rather than null.
func cloneSubscriptionProvider(p SubscriptionProvider) SubscriptionProvider {
	p.LoginKinds, p.Harnesses, p.BillingModes = slices.Clone(p.LoginKinds), slices.Clone(p.Harnesses), slices.Clone(p.BillingModes)
	p.Models, p.Plans = slices.Clone(p.Models), append([]SubscriptionPlan{}, p.Plans...)
	for i := range p.Models {
		p.Models[i].ModelIDs = slices.Clone(p.Models[i].ModelIDs)
	}
	for i := range p.Plans {
		p.Plans[i].AllowanceUnits = slices.Clone(p.Plans[i].AllowanceUnits)
		if p.Plans[i].Defaults = maps.Clone(p.Plans[i].Defaults); p.Plans[i].Defaults == nil {
			p.Plans[i].Defaults = map[string]any{}
		}
	}
	return p
}
