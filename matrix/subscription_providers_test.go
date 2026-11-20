package matrix

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

type spRows = []SubscriptionProvider

// TestSubscriptionProviders_Rules mutates one valid row per rule and requires
// generation to refuse it.
func TestSubscriptionProviders_Rules(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	harnessByName := map[agent.HarnessName]HarnessRow{}
	for _, h := range built.Harnesses {
		harnessByName[h.Name] = h
	}
	base := func() SubscriptionProvider { return cloneSubscriptionProvider(subscriptionProviders[0]) }
	cases := []struct {
		name   string
		mutate func(spRows) spRows
		want   string
	}{
		{"valid", func(p spRows) spRows { return p }, ""},
		{"harness not in matrix", func(p spRows) spRows { p[0].Harnesses = append(p[0].Harnesses, "nope"); return p }, `harnesses value "nope" not in`},
		{"harness listed twice", func(p spRows) spRows { p[0].Harnesses = append(p[0].Harnesses, p[0].Harnesses[0]); return p }, "listed twice"},
		{"harness without host-session cell", func(p spRows) spRows { p[0].Harnesses = append(p[0].Harnesses, agent.HarnessPi); return p }, `harness "pi" has no host-session cell`},
		{"endpoint not in matrix", func(p spRows) spRows { p[0].Models[0].Endpoint = "nope"; return p }, `models endpoint value "nope" not in`},
		{"unknown model id", func(p spRows) spRows { p[0].Models[0].ModelIDs = []string{"nope"}; return p }, `model id "nope" is not declared`},
		{"lowCap without guardPercent", func(p spRows) spRows { p[0].Plans[0].Defaults = map[string]any{}; return p }, "lowCap plan must set a guardPercent default"},
		{"enabled default", func(p spRows) spRows { p[0].Plans[0].Defaults["enabled"] = true; return p }, "enabled is never a default"},
		{"unknown default key", func(p spRows) spRows { p[0].Plans[0].Defaults["burst"] = 1; return p }, `default "burst" is not a subscription-login policy field`},
		{"guardPercent out of range", func(p spRows) spRows { p[0].Plans[0].Defaults["guardPercent"] = 101; return p }, "guardPercent must be an integer 0-100"},
		{"warnAtPercent out of range", func(p spRows) spRows { p[0].Plans[0].Defaults["warnAtPercent"] = []int{80, 120}; return p }, "entry 120 outside 0-100"},
		{"non-boolean allow flag", func(p spRows) spRows { p[0].Plans[0].Defaults["allowCredits"] = "yes"; return p }, "allowCredits must be a boolean"},
		{"duplicate plan slug", func(p spRows) spRows { p[0].Plans = append(p[0].Plans, p[0].Plans[0]); return p }, "listed twice"},
		{"uppercase plan slug", func(p spRows) spRows { p[0].Plans[0].Slug = "Plus"; return p }, `plan slug "Plus" must match`},
		{"duplicate provider id", func(p spRows) spRows { return append(p, base()) }, "duplicate id"},
		{"bad provider id", func(p spRows) spRows { p[0].ID = "-openai"; return p }, "id must match"},
		{"unknown kind", func(p spRows) spRows { p[0].Kind = "reseller"; return p }, `kind "reseller"`},
		{"billing mode outside vocab", func(p spRows) spRows { p[0].BillingModes = append(p[0].BillingModes, "prepaid"); return p }, `billingModes value "prepaid"`},
		{"billing modes not allowance first", func(p spRows) spRows { p[0].BillingModes = []string{"credits", "allowance"}; return p }, `must start with "allowance"`},
		{"unknown login kind", func(p spRows) spRows { p[0].LoginKinds = []string{"forge-identity"}; return p }, `loginKinds value "forge-identity"`},
		{"unknown allowance unit", func(p spRows) spRows { p[0].Plans[0].AllowanceUnits = []string{"credits"}; return p }, `allowanceUnits value "credits"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSubscriptionProviders(tc.mutate(spRows{base()}), harnessByName, built.Matrix.Cells, built.Endpoints)
			if tc.want == "" && err != nil {
				t.Fatalf("valid row refused: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v; want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestSubscriptionProviders_StandaloneMatchesMatrixSection pins the vendored file to the matrix section.
func TestSubscriptionProviders_StandaloneMatchesMatrixSection(t *testing.T) {
	standalone, err1 := os.ReadFile(FileSubscriptionProviders)
	raw, err2 := os.ReadFile(FileCapabilityMatrix)
	if err := errors.Join(err1, err2); err != nil {
		t.Fatalf("read artifacts: %v", err)
	}
	var doc struct {
		SubscriptionProviders json.RawMessage `json:"subscriptionProviders"`
	}
	var a, b bytes.Buffer
	if err := json.Unmarshal(raw, &doc); err != nil || json.Compact(&a, standalone) != nil || json.Compact(&b, doc.SubscriptionProviders) != nil {
		t.Fatalf("decode artifacts: %v", err)
	}
	if a.String() != b.String() {
		t.Fatalf("%s differs from the capability-matrix subscriptionProviders section", FileSubscriptionProviders)
	}
	if bytes.Contains(standalone, []byte("null")) {
		t.Errorf("%s renders a null field; absent collections must be [] or {}", FileSubscriptionProviders)
	}
	var catalog SubscriptionProviderCatalog
	if err := json.Unmarshal(standalone, &catalog); err != nil || catalog.ContractABI != SubscriptionProvidersContractABI || len(catalog.Providers) == 0 {
		t.Fatalf("catalog = %+v, err %v; want contract %q with rows", catalog, err, SubscriptionProvidersContractABI)
	}
	if !slices.IsSortedFunc(catalog.Providers, func(a, b SubscriptionProvider) int { return strings.Compare(a.ID, b.ID) }) {
		t.Errorf("providers are not sorted by id")
	}
}
