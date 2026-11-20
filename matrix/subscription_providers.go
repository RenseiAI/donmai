package matrix

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// SubscriptionIssuer is one subscriptionProviders row: a company or gateway
// that issues and bills a login. It describes and supplies defaults, never
// grants; a deployment may add rows and narrow harnesses, never widen them.
type SubscriptionIssuer struct {
	ID          string             `json:"id"`
	Kind        string             `json:"kind"`
	DisplayName string             `json:"displayName"`
	LoginKinds  []string           `json:"loginKinds"`
	Plans       []SubscriptionPlan `json:"plans"`
}

// SubscriptionPlan is matched by the slug the scanner reports; an unmatched
// plan is uncatalogued and takes the kind's built-in defaults.
type SubscriptionPlan struct {
	Slug            string           `json:"slug"`
	DisplayName     string           `json:"displayName"`
	AllowanceShape  string           `json:"allowanceShape"`
	LowCap          bool             `json:"lowCap"`
	ReachableModels []ReachableModel `json:"reachableModels"`
	// CompatibleHarnesses is the compatibility CEILING (matrix harness ids).
	CompatibleHarnesses []agent.HarnessName `json:"compatibleHarnesses"`
	BillingModes        []string            `json:"billingModes"`
	Defaults            map[string]any      `json:"defaults"`
}

// ReachableModel is a (matrix endpoint company, model id) pair a plan reaches.
type ReachableModel struct {
	ModelProvider agent.Company `json:"modelProvider"`
	ModelID       string        `json:"modelId"`
}

// issuerSpec is the hand-authored input; plans share its harnesses, billing
// modes and endpoints. Model ids come from the matrix; monthly marks plans the
// codex adapter reads as one monthly window.
type issuerSpec struct {
	id, kind, name         string
	harnesses              []agent.HarnessName
	billingModes           []string
	endpoints              []agent.Company
	plans, lowCap, monthly []string
}

// subscriptionIssuers lists issuers a matrix harness signs in with (a
// host-session cell); gateways join once the matrix realizes them.
var subscriptionIssuers = []issuerSpec{
	{
		id: "openai-chatgpt", kind: "direct", name: "ChatGPT",
		harnesses: []agent.HarnessName{agent.HarnessCodex}, billingModes: []string{"allowance", "credits"}, endpoints: []agent.Company{agent.CompanyOpenAI},
		// Slugs are the planType values the codex rate-limit read reports.
		plans:  []string{"free", "go", "plus", "pro", "team", "business", "enterprise", "edu"},
		lowCap: []string{"free", "go", "plus", "team", "business"}, monthly: []string{"free", "go"},
	},
	{
		id: "anthropic-claude", kind: "direct", name: "Claude",
		harnesses: []agent.HarnessName{agent.HarnessClaudeCode}, billingModes: []string{"allowance", "overage", "api-metered"}, endpoints: []agent.Company{agent.CompanyAnthropic},
		// No scanner reports a Claude plan yet. Matching is by reported slug,
		// so these rows apply once one does; any other slug is uncatalogued.
		plans: []string{"pro", "max", "team", "enterprise"}, lowCap: []string{"pro"},
	},
}

var (
	catalogSlugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	providerKinds       = []string{"direct", "gateway"}
	providerLoginKinds  = []string{"subscription-login"}
	providerBillingMode = []string{"allowance", "credits", "overage", "api-metered"}
	planDefaultFields   = []string{"allowApiBilledFallback", "allowCredits", "allowOverage", "guardPercent", "warnAtPercent"}
)

// buildSubscriptionProviders expands the specs into per-plan rows, derives
// model ids from the matrix, validates, and sorts by issuer id.
func buildSubscriptionProviders(specs []issuerSpec, harnessByName map[agent.HarnessName]HarnessRow, cells []HarnessEndpointCell, endpoints []EndpointRow) ([]SubscriptionIssuer, error) {
	endpointByCompany := map[agent.Company]EndpointRow{}
	for _, e := range endpoints {
		endpointByCompany[e.Company] = e
	}
	out := make([]SubscriptionIssuer, 0, len(specs))
	for _, s := range specs {
		models, err := deriveReachableModels(s.endpoints, s.harnesses, cells, endpointByCompany)
		if err != nil {
			return nil, fmt.Errorf("subscription provider %q: %w", s.id, err)
		}
		issuer := SubscriptionIssuer{ID: s.id, Kind: s.kind, DisplayName: s.name, LoginKinds: []string{"subscription-login"}, Plans: []SubscriptionPlan{}}
		for _, slug := range s.plans {
			lowCap := slices.Contains(s.lowCap, slug)
			shape := map[bool]string{false: "percent windows", true: "monthly percent"}[slices.Contains(s.monthly, slug)]
			guard := map[bool]int{true: 50}[lowCap] // low-cap: keep half of each window unused
			issuer.Plans = append(issuer.Plans, SubscriptionPlan{
				Slug: slug, DisplayName: strings.ToUpper(slug[:1]) + slug[1:], AllowanceShape: shape, LowCap: lowCap,
				ReachableModels: slices.Clone(models), CompatibleHarnesses: slices.Clone(s.harnesses), BillingModes: slices.Clone(s.billingModes),
				Defaults: map[string]any{"guardPercent": guard, "warnAtPercent": []int{80}, "allowCredits": false, "allowOverage": false, "allowApiBilledFallback": false},
			})
		}
		out = append(out, issuer)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, validateSubscriptionProviders(out, harnessByName, cells, endpointByCompany)
}

// deriveReachableModels takes each endpoint's models served on the harnesses'
// host-session hosts, else all its declared ids; it never invents ids.
func deriveReachableModels(endpoints []agent.Company, harnesses []agent.HarnessName, cells []HarnessEndpointCell, endpointByCompany map[agent.Company]EndpointRow) ([]ReachableModel, error) {
	var out []ReachableModel
	for _, e := range endpoints {
		var hosts []agent.ServingHost
		for _, c := range cells {
			if c.Endpoint == e && slices.Contains(harnesses, c.Harness) && slices.Contains(c.AuthModes, agent.AuthHostSession) {
				hosts = append(hosts, c.Host)
			}
		}
		var served, all []ReachableModel
		for _, m := range endpointByCompany[e].Models {
			all = append(all, ReachableModel{ModelProvider: e, ModelID: m.ID})
			if slices.ContainsFunc(m.Hosts, func(h agent.ServingHost) bool { return slices.Contains(hosts, h) }) {
				served = append(served, ReachableModel{ModelProvider: e, ModelID: m.ID})
			}
		}
		if len(served) == 0 {
			served = all
		}
		if len(served) == 0 {
			return nil, fmt.Errorf("endpoint %q declares no model ids to reach", e)
		}
		out = append(out, served...)
	}
	return out, nil
}

func validateSubscriptionProviders(issuers []SubscriptionIssuer, harnessByName map[agent.HarnessName]HarnessRow, cells []HarnessEndpointCell, endpointByCompany map[agent.Company]EndpointRow) error {
	for i, p := range issuers {
		err := validateSubscriptionIssuer(p, harnessByName, cells, endpointByCompany)
		if slices.ContainsFunc(issuers[:i], func(o SubscriptionIssuer) bool { return o.ID == p.ID }) {
			err = fmt.Errorf("duplicate id")
		}
		if err != nil {
			return fmt.Errorf("subscription provider %q: %w", p.ID, err)
		}
	}
	return nil
}

func validateSubscriptionIssuer(p SubscriptionIssuer, harnessByName map[agent.HarnessName]HarnessRow, cells []HarnessEndpointCell, endpointByCompany map[agent.Company]EndpointRow) error {
	if !catalogSlugPattern.MatchString(p.ID) {
		return fmt.Errorf("id must match %s", catalogSlugPattern)
	}
	if err := checkVocab("kind", []string{p.Kind}, providerKinds); err != nil {
		return err
	}
	if p.DisplayName == "" || len(p.Plans) == 0 {
		return fmt.Errorf("displayName and at least one plan are required")
	}
	if err := checkVocab("loginKinds", p.LoginKinds, providerLoginKinds); err != nil {
		return err
	}
	for i, plan := range p.Plans {
		if !catalogSlugPattern.MatchString(plan.Slug) || slices.ContainsFunc(p.Plans[:i], func(o SubscriptionPlan) bool { return o.Slug == plan.Slug }) {
			return fmt.Errorf("plan slug %q must be unique and match %s", plan.Slug, catalogSlugPattern)
		}
		if err := validateSubscriptionPlan(plan, harnessByName, cells, endpointByCompany); err != nil {
			return fmt.Errorf("plan %q: %w", plan.Slug, err)
		}
	}
	return nil
}

func validateSubscriptionPlan(plan SubscriptionPlan, harnessByName map[agent.HarnessName]HarnessRow, cells []HarnessEndpointCell, endpointByCompany map[agent.Company]EndpointRow) error {
	if plan.DisplayName == "" || plan.AllowanceShape == "" || len(plan.ReachableModels) == 0 {
		return fmt.Errorf("displayName, allowanceShape and reachableModels are required")
	}
	if err := checkVocab("billingModes", plan.BillingModes, providerBillingMode); err != nil {
		return err
	}
	if plan.BillingModes[0] != "allowance" {
		return fmt.Errorf("billingModes must start with %q", "allowance")
	}
	for i, m := range plan.ReachableModels {
		if !slices.ContainsFunc(endpointByCompany[m.ModelProvider].Models, func(d agent.ModelDesc) bool { return d.ID == m.ModelID }) {
			return fmt.Errorf("model %q is not declared by matrix endpoint %q", m.ModelID, m.ModelProvider)
		}
		if slices.Contains(plan.ReachableModels[:i], m) {
			return fmt.Errorf("reachable model %v listed twice", m)
		}
	}
	if err := checkVocab("compatibleHarnesses", plan.CompatibleHarnesses, slices.Sorted(maps.Keys(harnessByName))); err != nil {
		return fmt.Errorf("%w: harnesses must be matrix harness ids", err)
	}
	for _, h := range plan.CompatibleHarnesses { // two-axis validity: a host-session cell on a reached endpoint
		if !slices.ContainsFunc(cells, func(c HarnessEndpointCell) bool {
			return c.Harness == h && slices.Contains(c.AuthModes, agent.AuthHostSession) &&
				slices.ContainsFunc(plan.ReachableModels, func(m ReachableModel) bool { return m.ModelProvider == c.Endpoint })
		}) {
			return fmt.Errorf("harness %q has no host-session cell on the plan's endpoints", h)
		}
	}
	return validatePlanDefaults(plan)
}

// validatePlanDefaults requires exactly the five subscription-login policy
// fields, typed and in range; enablement is never a default.
func validatePlanDefaults(plan SubscriptionPlan) error {
	if _, ok := plan.Defaults["enabled"]; ok {
		return fmt.Errorf("enabled is never a default: enablement is set explicitly per login")
	}
	if keys := slices.Sorted(maps.Keys(plan.Defaults)); !slices.Equal(keys, planDefaultFields) {
		return fmt.Errorf("defaults must set exactly %v, got %v", planDefaultFields, keys)
	}
	guard, okGuard := plan.Defaults["guardPercent"].(int)
	warn, okWarn := plan.Defaults["warnAtPercent"].([]int)
	if outOfRange := func(v int) bool { return v < 0 || v > 100 }; !okGuard || !okWarn || outOfRange(guard) || slices.ContainsFunc(warn, outOfRange) {
		return fmt.Errorf("guardPercent and warnAtPercent entries must be integers 0-100")
	}
	for _, key := range planDefaultFields[:3] {
		if _, ok := plan.Defaults[key].(bool); !ok {
			return fmt.Errorf("default %s must be a boolean", key)
		}
	}
	if plan.LowCap && guard == 0 {
		return fmt.Errorf("lowCap plan needs a guardPercent above 0")
	}
	return nil
}

// checkVocab requires a non-empty, duplicate-free list drawn from vocab.
func checkVocab[T ~string](field string, values, vocab []T) error {
	if len(values) == 0 {
		return fmt.Errorf("%s is empty", field)
	}
	for i, v := range values {
		if v == "" || !slices.Contains(vocab, v) {
			return fmt.Errorf("%s value %q not in %v", field, v, vocab)
		}
		if slices.Contains(values[:i], v) {
			return fmt.Errorf("%s value %q listed twice", field, v)
		}
	}
	return nil
}
