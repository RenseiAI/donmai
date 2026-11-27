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

type spRows = []SubscriptionIssuer

// TestSubscriptionProviders_Rules mutates one valid built row per rule and
// requires validation to refuse it.
func TestSubscriptionProviders_Rules(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	harnessByName := map[agent.HarnessName]HarnessRow{}
	endpointByCompany := map[agent.Company]EndpointRow{}
	for _, h := range built.Harnesses {
		harnessByName[h.Name] = h
	}
	for _, e := range built.Endpoints {
		endpointByCompany[e.Company] = e
	}
	fresh := func() spRows {
		rows, err := buildSubscriptionProviders(subscriptionIssuers, harnessByName, built.Matrix.Cells, built.Endpoints)
		if err != nil {
			t.Fatalf("declared catalog refused: %v", err)
		}
		return rows
	}
	plan := func(p *spRows) *SubscriptionPlan { return &(*p)[1].Plans[0] } // openai-chatgpt "free" (low-cap)
	openai := func(p *spRows) *SubscriptionIssuer { return &(*p)[1] }
	cases := []struct {
		name   string
		mutate func(*spRows)
		want   string
	}{
		{"valid", func(*spRows) {}, ""},
		{"harness not in matrix", func(p *spRows) { plan(p).CompatibleHarnesses = append(plan(p).CompatibleHarnesses, "nope") }, `compatibleHarnesses value "nope" not in`},
		{"harness without host-session cell", func(p *spRows) { plan(p).CompatibleHarnesses = append(plan(p).CompatibleHarnesses, agent.HarnessPi) }, `harness "pi" has no host-session cell`},
		{"model id not on endpoint", func(p *spRows) { plan(p).ReachableModels[0].ModelID = "nope" }, `model "nope" is not declared by matrix endpoint "openai"`},
		{"model provider not in matrix", func(p *spRows) { plan(p).ReachableModels[0].ModelProvider = "nope" }, `model "gpt-6-sol" is not declared by matrix endpoint "nope"`},
		{"duplicate reachable model", func(p *spRows) { plan(p).ReachableModels = append(plan(p).ReachableModels, plan(p).ReachableModels[0]) }, "listed twice"},
		{"lowCap with guard 0", func(p *spRows) { plan(p).Defaults["guardPercent"] = 0 }, "lowCap plan needs a guardPercent above 0"},
		{"enabled default", func(p *spRows) { plan(p).Defaults["enabled"] = true }, "enabled is never a default"},
		{"missing default field", func(p *spRows) { delete(plan(p).Defaults, "allowOverage") }, "defaults must set exactly"},
		{"unknown default key", func(p *spRows) { plan(p).Defaults["burst"] = 1 }, "defaults must set exactly"},
		{"guardPercent out of range", func(p *spRows) { plan(p).Defaults["guardPercent"] = 101 }, "must be integers 0-100"},
		{"warnAtPercent out of range", func(p *spRows) { plan(p).Defaults["warnAtPercent"] = []int{80, 120} }, "must be integers 0-100"},
		{"non-boolean allow flag", func(p *spRows) { plan(p).Defaults["allowCredits"] = "no" }, "allowCredits must be a boolean"},
		{"duplicate plan slug", func(p *spRows) { openai(p).Plans = append(openai(p).Plans, openai(p).Plans[0]) }, `plan slug "free" must be unique`},
		{"duplicate issuer id", func(p *spRows) { *p = append(*p, (*p)[0]) }, "duplicate id"},
		{"uppercase plan slug", func(p *spRows) { plan(p).Slug = "Free" }, `plan slug "Free" must be unique and match`},
		{"bad issuer id", func(p *spRows) { openai(p).ID = "-openai" }, "id must match"},
		{"unknown kind", func(p *spRows) { openai(p).Kind = "reseller" }, `kind value "reseller"`},
		{"no plans", func(p *spRows) { openai(p).Plans = []SubscriptionPlan{} }, "at least one plan"},
		{"blank allowance shape", func(p *spRows) { plan(p).AllowanceShape = "" }, "allowanceShape"},
		{"billing mode outside vocab", func(p *spRows) { plan(p).BillingModes = append(plan(p).BillingModes, "prepaid") }, `billingModes value "prepaid"`},
		{"billing modes not allowance first", func(p *spRows) { plan(p).BillingModes = []string{"credits", "allowance"} }, `must start with "allowance"`},
		{"unknown login kind", func(p *spRows) { openai(p).LoginKinds = []string{"forge-identity"} }, `loginKinds value "forge-identity"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := fresh()
			tc.mutate(&rows)
			err := validateSubscriptionProviders(rows, harnessByName, built.Matrix.Cells, endpointByCompany)
			if tc.want == "" && err != nil {
				t.Fatalf("valid rows refused: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v; want it to contain %q", err, tc.want)
			}
		})
	}
	t.Run("endpoint with no model ids", func(t *testing.T) {
		empty := map[agent.Company]EndpointRow{agent.CompanyOpenAI: {Company: agent.CompanyOpenAI}}
		if _, err := deriveReachableModels([]agent.Company{agent.CompanyOpenAI}, []agent.HarnessName{agent.HarnessCodex}, built.Matrix.Cells, empty); err == nil {
			t.Fatal("derivation invented model ids for an endpoint that declares none")
		}
	})
}

// TestSubscriptionProviders_MatrixJSONMatchesFullDocument pins the vendored
// matrix.json array to the capability-matrix.json section and to the platform
// sync check: a non-empty array where every issuer has plans, with no nulls.
func TestSubscriptionProviders_MatrixJSONMatchesFullDocument(t *testing.T) {
	var arrays [2]bytes.Buffer
	for i, name := range []string{FileMatrix, FileCapabilityMatrix} {
		raw, err := os.ReadFile(name)
		var doc struct {
			SubscriptionProviders json.RawMessage `json:"subscriptionProviders"`
		}
		if err := errors.Join(err, json.Unmarshal(raw, &doc), json.Compact(&arrays[i], doc.SubscriptionProviders)); err != nil {
			t.Fatalf("decode %s subscriptionProviders: %v", name, err)
		}
	}
	if arrays[0].String() != arrays[1].String() {
		t.Fatalf("%s subscriptionProviders differs from %s", FileMatrix, FileCapabilityMatrix)
	}
	var issuers []SubscriptionIssuer
	err := json.Unmarshal(arrays[0].Bytes(), &issuers)
	if err != nil || len(issuers) == 0 || bytes.Contains(arrays[0].Bytes(), []byte("null")) || slices.ContainsFunc(issuers, func(p SubscriptionIssuer) bool { return len(p.Plans) == 0 }) {
		t.Fatalf("subscriptionProviders = %s (err %v); want a non-empty array, every issuer with plans, no nulls", arrays[0].String(), err)
	}
}
