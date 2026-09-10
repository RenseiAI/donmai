package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// CapabilityRealizationContractVersion identifies the opt-in realization binding.
const CapabilityRealizationContractVersion = "donmai.capability-realization/v1"

// CapabilityRealizationEvidenceTier names immutable fixture provenance strength.
type CapabilityRealizationEvidenceTier string

// Evidence tiers derive production eligibility; callers cannot set a boolean.
const (
	RealizationEvidenceFixture    CapabilityRealizationEvidenceTier = "fixture_verified"
	RealizationEvidenceRealBinary CapabilityRealizationEvidenceTier = "real_binary_verified"
	RealizationEvidenceLive       CapabilityRealizationEvidenceTier = "live_verified"
)

// CapabilityDeclaredSurface names the server and tool identities a recipe supports.
type CapabilityDeclaredSurface struct {
	MCPServerNames []string `json:"mcpServerNames"`
	MCPToolNames   []string `json:"mcpToolNames"`
}

// CapabilityRecipeEntry binds a recipe to an existing lifecycle entry.
type CapabilityRecipeEntry struct {
	EntryID  string               `json:"entryId"`
	Channel  ToolLifecycleChannel `json:"channel"`
	Required bool                 `json:"required"`
}

// CapabilityRealizationRecipe is a content-addressed delivery recipe.
type CapabilityRealizationRecipe struct {
	RecipeID              string                    `json:"recipeId"`
	Entries               []CapabilityRecipeEntry   `json:"entries"`
	DeclaredSurface       CapabilityDeclaredSurface `json:"declaredSurface"`
	RecipeDigest          string                    `json:"recipeDigest"`
	DeclaredSurfaceDigest string                    `json:"declaredSurfaceDigest"`
}

// CapabilityRealizationEvidence binds a recipe to a passing fixture artifact.
type CapabilityRealizationEvidence struct {
	FixtureID     string                            `json:"fixtureId"`
	FixtureDigest string                            `json:"fixtureDigest"`
	Tier          CapabilityRealizationEvidenceTier `json:"tier"`
}

// CapabilityRealizationDeclaration registers one exact capability/adapter/mode recipe.
type CapabilityRealizationDeclaration struct {
	ContractVersion string                        `json:"contractVersion"`
	CapabilityID    string                        `json:"capabilityId"`
	HarnessID       HarnessName                   `json:"harnessId"`
	AdapterVersion  string                        `json:"adapterVersion"`
	Mode            PromptSessionMode             `json:"mode"`
	Recipe          CapabilityRealizationRecipe   `json:"recipe"`
	Evidence        CapabilityRealizationEvidence `json:"evidence"`
}

// CapabilityRealizationBinding is the secret-free plan projection of a declaration.
type CapabilityRealizationBinding struct {
	ContractVersion       string                            `json:"contractVersion"`
	CapabilityID          string                            `json:"capabilityId"`
	HarnessID             HarnessName                       `json:"harnessId"`
	AdapterVersion        string                            `json:"adapterVersion"`
	Mode                  PromptSessionMode                 `json:"mode"`
	RecipeID              string                            `json:"recipeId"`
	RecipeDigest          string                            `json:"recipeDigest"`
	DeclaredSurfaceDigest string                            `json:"declaredSurfaceDigest"`
	RequiredEntryIDs      []string                          `json:"requiredEntryIds"`
	EvidenceFixtureDigest string                            `json:"evidenceFixtureDigest"`
	EvidenceTier          CapabilityRealizationEvidenceTier `json:"evidenceTier"`
}

// CapabilityRealizationResult records complete or denied recipe application.
type CapabilityRealizationResult struct {
	CapabilityRealizationBinding
	Decision string `json:"decision"`
}

// CapabilityRealizationInput contains trusted downstream registration source.
type CapabilityRealizationInput struct {
	CapabilityID    string
	HarnessID       HarnessName
	AdapterVersion  string
	Mode            PromptSessionMode
	RecipeID        string
	Entries         []CapabilityRecipeEntry
	DeclaredSurface CapabilityDeclaredSurface
	Evidence        CapabilityRealizationEvidence
}

var (
	realizationRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,255}$`)
	realizationDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func domainDigest(domain string, value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(append([]byte(domain+"\x00"), raw...))
	return hex.EncodeToString(sum[:])
}

func sortedUnique(values []string) ([]string, bool) {
	out := append([]string(nil), values...)
	sort.Strings(out)
	for i, value := range out {
		if !realizationRef.MatchString(value) || (i > 0 && value == out[i-1]) {
			return nil, false
		}
	}
	return out, true
}

// NewCapabilityRealization validates, canonicalizes, and domain-digests a registration.
func NewCapabilityRealization(input CapabilityRealizationInput) (CapabilityRealizationDeclaration, error) {
	if !realizationRef.MatchString(input.CapabilityID) || input.HarnessID == "" || !realizationRef.MatchString(input.AdapterVersion) || !realizationRef.MatchString(input.RecipeID) || input.Mode == "" {
		return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization identity is malformed")
	}
	if !realizationRef.MatchString(input.Evidence.FixtureID) || !realizationDigest.MatchString(input.Evidence.FixtureDigest) || (input.Evidence.Tier != RealizationEvidenceFixture && input.Evidence.Tier != RealizationEvidenceRealBinary && input.Evidence.Tier != RealizationEvidenceLive) {
		return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization evidence is malformed")
	}
	servers, ok := sortedUnique(input.DeclaredSurface.MCPServerNames)
	if !ok {
		return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization server surface is malformed")
	}
	tools, ok := sortedUnique(input.DeclaredSurface.MCPToolNames)
	if !ok {
		return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization tool surface is malformed")
	}
	entries := append([]CapabilityRecipeEntry(nil), input.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].EntryID < entries[j].EntryID })
	seen := map[string]bool{}
	required := 0
	for _, entry := range entries {
		if !realizationRef.MatchString(entry.EntryID) || !isKnownToolLifecycleChannel(entry.Channel) || seen[entry.EntryID] {
			return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization entries are malformed")
		}
		seen[entry.EntryID] = true
		if entry.Required {
			required++
		}
	}
	if len(entries) == 0 || required == 0 || len(servers) == 0 || len(tools) == 0 {
		return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization surface is incomplete")
	}
	surface := CapabilityDeclaredSurface{MCPServerNames: servers, MCPToolNames: tools}
	recipeCore := struct {
		RecipeID        string                    `json:"recipeId"`
		Entries         []CapabilityRecipeEntry   `json:"entries"`
		DeclaredSurface CapabilityDeclaredSurface `json:"declaredSurface"`
	}{input.RecipeID, entries, surface}
	recipe := CapabilityRealizationRecipe{RecipeID: input.RecipeID, Entries: entries, DeclaredSurface: surface, RecipeDigest: domainDigest("donmai.capability-realization.recipe/v1", recipeCore), DeclaredSurfaceDigest: domainDigest("donmai.capability-realization.surface/v1", surface)}
	return CapabilityRealizationDeclaration{ContractVersion: CapabilityRealizationContractVersion, CapabilityID: input.CapabilityID, HarnessID: input.HarnessID, AdapterVersion: input.AdapterVersion, Mode: input.Mode, Recipe: recipe, Evidence: input.Evidence}, nil
}

// CapabilityRealizationSupportsSpec verifies the declared server surface is mounted.
func CapabilityRealizationSupportsSpec(d CapabilityRealizationDeclaration, spec Spec) bool {
	servers := map[string]bool{}
	for _, server := range spec.MCPServers {
		servers[server.Name] = true
	}
	for _, required := range d.Recipe.DeclaredSurface.MCPServerNames {
		if !servers[required] {
			return false
		}
	}
	return true
}

// ProductionEligible derives eligibility solely from verified evidence tier.
func (d CapabilityRealizationDeclaration) ProductionEligible() bool {
	return d.Evidence.Tier == RealizationEvidenceRealBinary || d.Evidence.Tier == RealizationEvidenceLive
}

// CapabilityRealizationRegistry is an immutable trusted declaration snapshot.
type CapabilityRealizationRegistry struct {
	mu           sync.RWMutex
	rows         map[string]CapabilityRealizationDeclaration
	capabilities map[string]bool
}

func realizationKey(capability string, harness HarnessName, adapter string, mode PromptSessionMode) string {
	return capability + "\x00" + string(harness) + "\x00" + adapter + "\x00" + string(mode)
}

// NewCapabilityRealizationRegistry validates and copies canonical declarations.
func NewCapabilityRealizationRegistry(rows []CapabilityRealizationDeclaration) (*CapabilityRealizationRegistry, error) {
	r := &CapabilityRealizationRegistry{rows: map[string]CapabilityRealizationDeclaration{}, capabilities: map[string]bool{}}
	for _, row := range rows {
		rebuilt, err := NewCapabilityRealization(CapabilityRealizationInput{CapabilityID: row.CapabilityID, HarnessID: row.HarnessID, AdapterVersion: row.AdapterVersion, Mode: row.Mode, RecipeID: row.Recipe.RecipeID, Entries: row.Recipe.Entries, DeclaredSurface: row.Recipe.DeclaredSurface, Evidence: row.Evidence})
		if err != nil || row.ContractVersion != CapabilityRealizationContractVersion || rebuilt.Recipe.RecipeDigest != row.Recipe.RecipeDigest || rebuilt.Recipe.DeclaredSurfaceDigest != row.Recipe.DeclaredSurfaceDigest {
			return nil, fmt.Errorf("capability realization declaration is not canonical")
		}
		key := realizationKey(row.CapabilityID, row.HarnessID, row.AdapterVersion, row.Mode)
		if _, ok := r.rows[key]; ok {
			return nil, fmt.Errorf("duplicate capability realization")
		}
		r.rows[key] = rebuilt
		r.capabilities[row.CapabilityID] = true
	}
	return r, nil
}

// Knows reports whether the snapshot owns any declaration for a capability.
func (r *CapabilityRealizationRegistry) Knows(capability string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.capabilities[capability]
}

// Resolve returns a defensive copy for one exact capability/adapter/mode key.
func (r *CapabilityRealizationRegistry) Resolve(capability string, harness HarnessName, adapter string, mode PromptSessionMode) (CapabilityRealizationDeclaration, bool) {
	if r == nil {
		return CapabilityRealizationDeclaration{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.rows[realizationKey(capability, harness, adapter, mode)]
	if !ok {
		return CapabilityRealizationDeclaration{}, false
	}
	d.Recipe.Entries = append([]CapabilityRecipeEntry(nil), d.Recipe.Entries...)
	d.Recipe.DeclaredSurface.MCPServerNames = append([]string(nil), d.Recipe.DeclaredSurface.MCPServerNames...)
	d.Recipe.DeclaredSurface.MCPToolNames = append([]string(nil), d.Recipe.DeclaredSurface.MCPToolNames...)
	return d, true
}

// BindCapabilityRealization projects a trusted declaration into plan authority.
func BindCapabilityRealization(d CapabilityRealizationDeclaration) CapabilityRealizationBinding {
	required := []string{}
	for _, e := range d.Recipe.Entries {
		if e.Required {
			required = append(required, e.EntryID)
		}
	}
	return CapabilityRealizationBinding{ContractVersion: CapabilityRealizationContractVersion, CapabilityID: d.CapabilityID, HarnessID: d.HarnessID, AdapterVersion: d.AdapterVersion, Mode: d.Mode, RecipeID: d.Recipe.RecipeID, RecipeDigest: d.Recipe.RecipeDigest, DeclaredSurfaceDigest: d.Recipe.DeclaredSurfaceDigest, RequiredEntryIDs: required, EvidenceFixtureDigest: d.Evidence.FixtureDigest, EvidenceTier: d.Evidence.Tier}
}

// ResolveCapabilityRealizationResults refuses any partially applied required recipe.
func ResolveCapabilityRealizationResults(bindings []CapabilityRealizationBinding, entries []ToolLifecycleEntry) ([]CapabilityRealizationResult, error) {
	byID := map[string]ToolLifecycleEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	results := make([]CapabilityRealizationResult, 0, len(bindings))
	seen := map[string]bool{}
	for _, b := range bindings {
		if b.ContractVersion != CapabilityRealizationContractVersion || !realizationRef.MatchString(b.CapabilityID) || !realizationRef.MatchString(string(b.HarnessID)) || !realizationRef.MatchString(b.AdapterVersion) || b.Mode == "" || len(b.RequiredEntryIDs) == 0 || !realizationDigest.MatchString(b.RecipeDigest) || !realizationDigest.MatchString(b.DeclaredSurfaceDigest) || !realizationDigest.MatchString(b.EvidenceFixtureDigest) || seen[b.CapabilityID] {
			return nil, fmt.Errorf("capability realization binding is malformed")
		}
		seen[b.CapabilityID] = true
		result := CapabilityRealizationResult{CapabilityRealizationBinding: b, Decision: "ready"}
		for _, id := range b.RequiredEntryIDs {
			e, ok := byID[id]
			if !ok || e.Outcome == ToolOutcomeDenied || e.Outcome == ToolOutcomePendingRuntime || e.Outcome == ToolOutcomePendingCleanup {
				result.Decision = "denied"
				results = append(results, result)
				return results, fmt.Errorf("capability realization %q is partially applied", b.CapabilityID)
			}
		}
		results = append(results, result)
	}
	return results, nil
}
