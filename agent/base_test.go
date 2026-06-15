package agent

import (
	"context"
	"errors"
	"testing"
)

// Unit tests for the SDK base provider contract (base.go), grounded in
// donmai-architecture/002-provider-base-contract.md.

func TestKnownProviderFamilies_NineRoster(t *testing.T) {
	fams := KnownProviderFamilies()
	if len(fams) != 9 {
		t.Fatalf("KnownProviderFamilies len=%d, want 9 (002 §enum)", len(fams))
	}
	// The two freezing axes must be present and carry their documented
	// discriminant strings.
	if !IsKnownProviderFamily(FamilyHarness) {
		t.Errorf("harness family %q not in roster", FamilyHarness)
	}
	if FamilyHarness != "agent-runtime" {
		t.Errorf("FamilyHarness=%q, want byte-identical to 002's \"agent-runtime\"", FamilyHarness)
	}
	if !IsKnownProviderFamily(FamilyModelEndpoint) {
		t.Errorf("model-endpoint family %q not in roster", FamilyModelEndpoint)
	}
	// No duplicates in the roster.
	seen := map[ProviderFamily]bool{}
	for _, f := range fams {
		if seen[f] {
			t.Errorf("duplicate family %q in roster", f)
		}
		seen[f] = true
	}
	if IsKnownProviderFamily("not-a-family") {
		t.Errorf("IsKnownProviderFamily accepted an off-roster family")
	}
}

func TestScopeSpecificity_Ordering(t *testing.T) {
	// Most-specific wins: project > org > tenant > global (002 §"Scope
	// resolution" rule 1).
	cases := []struct {
		level string
		want  int
	}{
		{ScopeProject, 3},
		{ScopeOrg, 2},
		{ScopeTenant, 1},
		{ScopeGlobal, 0},
		{"bogus", -1},
	}
	for _, c := range cases {
		if got := ScopeSpecificity(c.level); got != c.want {
			t.Errorf("ScopeSpecificity(%q)=%d, want %d", c.level, got, c.want)
		}
	}
	if !(ScopeSpecificity(ScopeProject) > ScopeSpecificity(ScopeOrg) &&
		ScopeSpecificity(ScopeOrg) > ScopeSpecificity(ScopeTenant) &&
		ScopeSpecificity(ScopeTenant) > ScopeSpecificity(ScopeGlobal)) {
		t.Errorf("specificity ordering is not strictly project>org>tenant>global")
	}
}

func TestValidateScope(t *testing.T) {
	cases := []struct {
		name    string
		scope   ProviderScope
		wantErr bool
	}{
		{"global no selector ok", GlobalScope(), false},
		{"unknown level", ProviderScope{Level: "galaxy"}, true},
		{
			"non-global empty selector invalid (rule 4)",
			ProviderScope{Level: ScopeProject},
			true,
		},
		{
			"project with selector ok",
			ProviderScope{Level: ScopeProject, Selector: &ScopeSelector{Project: []string{"p1"}}},
			false,
		},
		{
			"org with path selector ok",
			ProviderScope{Level: ScopeOrg, Selector: &ScopeSelector{Paths: []string{"apps/**"}}},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateScope(c.scope)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ValidateScope(%+v)=nil, want error", c.scope)
				}
				if !errors.Is(err, ErrInvalidScope) {
					t.Errorf("ValidateScope error = %v, want ErrInvalidScope", err)
				}
			} else if err != nil {
				t.Errorf("ValidateScope(%+v)=%v, want nil", c.scope, err)
			}
		})
	}
}

func TestScopeSelector_IsEmpty(t *testing.T) {
	if !(*ScopeSelector)(nil).IsEmpty() {
		t.Errorf("nil selector should report empty")
	}
	if !(&ScopeSelector{}).IsEmpty() {
		t.Errorf("zero selector should report empty")
	}
	if (&ScopeSelector{Org: []string{"o"}}).IsEmpty() {
		t.Errorf("selector with an org matcher should not report empty")
	}
}

// myCaps is a stand-in family-typed capabilities struct, exercising the
// generic ProviderManifest[F] the way a new SDK-authored family would.
type myCaps struct {
	Frobs bool `json:"frobs"`
}

func TestProviderManifest_Generic_SatisfiesBaseManifest(t *testing.T) {
	mf := ProviderManifest[myCaps]{
		ProviderBase: ProviderBase{
			APIVersion: ProviderAPIVersion,
			Family:     FamilySandbox,
			ID:         "acme.sandbox",
			Name:       "Acme Sandbox",
			Version:    "1.0.0",
		},
		CapabilitiesDeclared: myCaps{Frobs: true},
	}
	var bm BaseManifest = mf
	if got := ManifestFamily(bm); got != FamilySandbox {
		t.Errorf("ManifestFamily=%q, want %q", got, FamilySandbox)
	}
	// A zero scope reads as global.
	if got := ManifestScope(bm); got.Level != ScopeGlobal {
		t.Errorf("ManifestScope default level=%q, want %q", got.Level, ScopeGlobal)
	}
	// Unset stability reads as stable.
	if got := ManifestStability(bm); got != StabilityStable {
		t.Errorf("ManifestStability default=%q, want %q", got, StabilityStable)
	}
	if !mf.CapabilitiesDeclared.Frobs {
		t.Errorf("declared capability lost through the generic manifest")
	}
}

func TestNoopLifecycle_Idempotent(t *testing.T) {
	var lc NoopLifecycle
	ctx := context.Background()
	if err := lc.Activate(ctx); err != nil {
		t.Errorf("Activate: %v", err)
	}
	if err := lc.Deactivate(ctx); err != nil {
		t.Errorf("Deactivate: %v", err)
	}
	if err := lc.Deactivate(ctx); err != nil {
		t.Errorf("second Deactivate must not error: %v", err)
	}
	h, err := lc.Health(ctx)
	if err != nil {
		t.Errorf("Health: %v", err)
	}
	if h.Status != HealthReady {
		t.Errorf("Health=%q, want %q", h.Status, HealthReady)
	}
}

func TestManifestStability_KnownTiers(t *testing.T) {
	for _, tier := range []string{StabilityStable, StabilityBeta, StabilityUnstable, StabilityRegistrationOnly} {
		mf := HarnessManifest{Name: HarnessStub, Family: FamilyHarness, ContractABI: "harness/v2"}
		base := mf.Base()
		base.Stability = tier
		// Re-wrap via a tiny BaseManifest stand-in to exercise ManifestStability.
		bm := fixedBase{base}
		if got := ManifestStability(bm); got != tier {
			t.Errorf("ManifestStability(%q)=%q", tier, got)
		}
	}
}

type fixedBase struct{ b ProviderBase }

func (f fixedBase) Base() ProviderBase { return f.b }
