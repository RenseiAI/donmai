package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"sync"
)

// CapabilityRealizationContractVersion is the versioned capability-realization contract surface.
const CapabilityRealizationContractVersion = "donmai.capability-realization/v1"

// CapabilitySurfaceKind is the versioned capability-realization contract surface.
type CapabilitySurfaceKind string

// Capability surface kinds keep delivery channels independent.
const (
	CapabilitySurfaceMCPServer  CapabilitySurfaceKind = "mcp_server"
	CapabilitySurfaceMCPTool    CapabilitySurfaceKind = "mcp_tool"
	CapabilitySurfaceNativeTool CapabilitySurfaceKind = "native_tool"
	CapabilitySurfacePartial    CapabilitySurfaceKind = "partial"
)

// CapabilitySurfaceIdentity is the versioned capability-realization contract surface.
type CapabilitySurfaceIdentity struct {
	Kind CapabilitySurfaceKind `json:"kind"`
	ID   string                `json:"id"`
}

// CapabilityRecipeEntry is the versioned capability-realization contract surface.
type CapabilityRecipeEntry struct {
	EntryID     string                      `json:"entryId"`
	Channel     ToolLifecycleChannel        `json:"channel"`
	Required    bool                        `json:"required"`
	InputDigest string                      `json:"inputDigest"`
	SurfaceRefs []CapabilitySurfaceIdentity `json:"surfaceRefs"`
}

// CapabilityRealizationRecipe is the versioned capability-realization contract surface.
type CapabilityRealizationRecipe struct {
	RecipeID              string                      `json:"recipeId"`
	Entries               []CapabilityRecipeEntry     `json:"entries"`
	DeclaredSurface       []CapabilitySurfaceIdentity `json:"declaredSurface"`
	RecipeDigest          string                      `json:"recipeDigest"`
	DeclaredSurfaceDigest string                      `json:"declaredSurfaceDigest"`
}

// CapabilityRealizationDeclaration is the versioned capability-realization contract surface.
type CapabilityRealizationDeclaration struct {
	ContractVersion string                      `json:"contractVersion"`
	CapabilityID    string                      `json:"capabilityId"`
	HarnessID       HarnessName                 `json:"harnessId"`
	AdapterVersion  string                      `json:"adapterVersion"`
	Mode            PromptSessionMode           `json:"mode"`
	Recipe          CapabilityRealizationRecipe `json:"recipe"`
}

// CapabilityRealizationInput is the versioned capability-realization contract surface.
type CapabilityRealizationInput struct {
	CapabilityID    string
	HarnessID       HarnessName
	AdapterVersion  string
	Mode            PromptSessionMode
	RecipeID        string
	Entries         []CapabilityRecipeEntry
	DeclaredSurface []CapabilitySurfaceIdentity
}

// CapabilityAppliedArtifact is the versioned capability-realization contract surface.
type CapabilityAppliedArtifact struct {
	EntryID     string               `json:"entryId"`
	Channel     ToolLifecycleChannel `json:"channel"`
	InputDigest string               `json:"inputDigest"`
}

// CapabilityFixtureObservation is the versioned capability-realization contract surface.
type CapabilityFixtureObservation struct {
	ContractVersion   string                      `json:"contractVersion"`
	CapabilityID      string                      `json:"capabilityId"`
	HarnessID         HarnessName                 `json:"harnessId"`
	AdapterVersion    string                      `json:"adapterVersion"`
	Mode              PromptSessionMode           `json:"mode"`
	RecipeDigest      string                      `json:"recipeDigest"`
	FixtureID         string                      `json:"fixtureId"`
	BinaryDigest      string                      `json:"binaryDigest"`
	AppliedArtifacts  []CapabilityAppliedArtifact `json:"appliedArtifacts"`
	ObservedSurface   []CapabilitySurfaceIdentity `json:"observedSurface"`
	ObservationDigest string                      `json:"observationDigest"`
}

// CapabilityFixtureObservationInput is the versioned capability-realization contract surface.
type CapabilityFixtureObservationInput struct {
	Declaration      CapabilityRealizationDeclaration
	FixtureID        string
	BinaryDigest     string
	AppliedArtifacts []CapabilityAppliedArtifact
	ObservedSurface  []CapabilitySurfaceIdentity
}

// CompiledCapabilityRealization is the versioned capability-realization contract surface.
type CompiledCapabilityRealization struct {
	Declaration CapabilityRealizationDeclaration `json:"declaration"`
	Observation CapabilityFixtureObservation     `json:"observation"`
}

// CapabilityRealizationBinding is the versioned capability-realization contract surface.
type CapabilityRealizationBinding struct {
	ContractVersion       string                  `json:"contractVersion"`
	CapabilityID          string                  `json:"capabilityId"`
	HarnessID             HarnessName             `json:"harnessId"`
	AdapterVersion        string                  `json:"adapterVersion"`
	Mode                  PromptSessionMode       `json:"mode"`
	RecipeID              string                  `json:"recipeId"`
	RecipeDigest          string                  `json:"recipeDigest"`
	DeclaredSurfaceDigest string                  `json:"declaredSurfaceDigest"`
	Entries               []CapabilityRecipeEntry `json:"entries"`
	ObservationDigest     string                  `json:"observationDigest"`
}

// CapabilityRealizationResult is the versioned capability-realization contract surface.
type CapabilityRealizationResult struct {
	CapabilityRealizationBinding
	Decision string `json:"decision"`
}

var (
	realizationRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,255}$`)
	realizationDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func domainDigest(domain string, value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("capability realization digest input: %w", err)
	}
	sum := sha256.Sum256(append([]byte(domain+"\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

// CapabilityRecipeInputDigest is the versioned capability-realization contract surface.
func CapabilityRecipeInputDigest(value any) (string, error) {
	return domainDigest("donmai.capability-realization.entry-input/v1", value)
}
func surfaceKey(v CapabilitySurfaceIdentity) string { return string(v.Kind) + "\x00" + v.ID }
func knownSurfaceKind(v CapabilitySurfaceKind) bool {
	switch v {
	case CapabilitySurfaceMCPServer, CapabilitySurfaceMCPTool, CapabilitySurfaceNativeTool, CapabilitySurfacePartial:
		return true
	}
	return false
}

func canonicalSurface(values []CapabilitySurfaceIdentity) ([]CapabilitySurfaceIdentity, error) {
	out := append([]CapabilitySurfaceIdentity(nil), values...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	for i, v := range out {
		if !knownSurfaceKind(v.Kind) || !realizationRef.MatchString(v.ID) || (i > 0 && surfaceKey(v) == surfaceKey(out[i-1])) {
			return nil, fmt.Errorf("capability realization surface is malformed")
		}
	}
	return out, nil
}

// NewCapabilityRealization is the versioned capability-realization contract surface.
func NewCapabilityRealization(input CapabilityRealizationInput) (CapabilityRealizationDeclaration, error) {
	if !realizationRef.MatchString(input.CapabilityID) || input.HarnessID == "" || !realizationRef.MatchString(input.AdapterVersion) || !realizationRef.MatchString(input.RecipeID) || input.Mode == "" {
		return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization identity is malformed")
	}
	surface, err := canonicalSurface(input.DeclaredSurface)
	if err != nil || len(surface) == 0 {
		return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization surface is incomplete")
	}
	declared := map[string]bool{}
	for _, v := range surface {
		declared[surfaceKey(v)] = true
	}
	entries := append([]CapabilityRecipeEntry(nil), input.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].EntryID < entries[j].EntryID })
	seen := map[string]bool{}
	covered := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		refs, refErr := canonicalSurface(e.SurfaceRefs)
		e.SurfaceRefs = refs
		if refErr != nil || !e.Required || !realizationRef.MatchString(e.EntryID) || !isKnownToolLifecycleChannel(e.Channel) || !realizationDigest.MatchString(e.InputDigest) || seen[e.EntryID] || len(refs) == 0 {
			return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization entries are malformed")
		}
		seen[e.EntryID] = true
		for _, ref := range refs {
			if !declared[surfaceKey(ref)] {
				return CapabilityRealizationDeclaration{}, fmt.Errorf("capability recipe references undeclared surface")
			}
			covered[surfaceKey(ref)] = true
		}
	}
	if len(entries) == 0 || len(covered) != len(declared) {
		return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization surface coverage is incomplete")
	}
	core := struct {
		RecipeID        string                      `json:"recipeId"`
		Entries         []CapabilityRecipeEntry     `json:"entries"`
		DeclaredSurface []CapabilitySurfaceIdentity `json:"declaredSurface"`
	}{input.RecipeID, entries, surface}
	recipeDigest, err := domainDigest("donmai.capability-realization.recipe/v1", core)
	if err != nil {
		return CapabilityRealizationDeclaration{}, err
	}
	surfaceDigest, err := domainDigest("donmai.capability-realization.surface/v1", surface)
	if err != nil {
		return CapabilityRealizationDeclaration{}, err
	}
	recipe := CapabilityRealizationRecipe{RecipeID: input.RecipeID, Entries: entries, DeclaredSurface: surface, RecipeDigest: recipeDigest, DeclaredSurfaceDigest: surfaceDigest}
	return CapabilityRealizationDeclaration{ContractVersion: CapabilityRealizationContractVersion, CapabilityID: input.CapabilityID, HarnessID: input.HarnessID, AdapterVersion: input.AdapterVersion, Mode: input.Mode, Recipe: recipe}, nil
}

// NewCapabilityFixtureObservation is the versioned capability-realization contract surface.
func NewCapabilityFixtureObservation(input CapabilityFixtureObservationInput) (CapabilityFixtureObservation, error) {
	d := input.Declaration
	if d.ContractVersion != CapabilityRealizationContractVersion || !realizationRef.MatchString(input.FixtureID) || !realizationDigest.MatchString(input.BinaryDigest) {
		return CapabilityFixtureObservation{}, fmt.Errorf("capability observation identity is malformed")
	}
	observed, err := canonicalSurface(input.ObservedSurface)
	if err != nil {
		return CapabilityFixtureObservation{}, err
	}
	artifacts := append([]CapabilityAppliedArtifact(nil), input.AppliedArtifacts...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].EntryID < artifacts[j].EntryID })
	o := CapabilityFixtureObservation{ContractVersion: CapabilityRealizationContractVersion, CapabilityID: d.CapabilityID, HarnessID: d.HarnessID, AdapterVersion: d.AdapterVersion, Mode: d.Mode, RecipeDigest: d.Recipe.RecipeDigest, FixtureID: input.FixtureID, BinaryDigest: input.BinaryDigest, AppliedArtifacts: artifacts, ObservedSurface: observed}
	digest, err := observationDigest(o)
	if err != nil {
		return CapabilityFixtureObservation{}, err
	}
	o.ObservationDigest = digest
	return o, nil
}

func observationDigest(o CapabilityFixtureObservation) (string, error) {
	o.ObservationDigest = ""
	return domainDigest("donmai.capability-realization.observation/v1", o)
}

func rebuildObservation(o CapabilityFixtureObservation) (CapabilityFixtureObservation, error) {
	if o.ContractVersion != CapabilityRealizationContractVersion || !realizationRef.MatchString(o.CapabilityID) || o.HarnessID == "" || !realizationRef.MatchString(o.AdapterVersion) || o.Mode == "" || !realizationDigest.MatchString(o.RecipeDigest) || !realizationRef.MatchString(o.FixtureID) || !realizationDigest.MatchString(o.BinaryDigest) || !realizationDigest.MatchString(o.ObservationDigest) {
		return CapabilityFixtureObservation{}, fmt.Errorf("capability observation identity is malformed")
	}
	surface, err := canonicalSurface(o.ObservedSurface)
	if err != nil || len(surface) == 0 {
		return CapabilityFixtureObservation{}, fmt.Errorf("capability observation surface is malformed")
	}
	artifacts := append([]CapabilityAppliedArtifact(nil), o.AppliedArtifacts...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].EntryID < artifacts[j].EntryID })
	for i, artifact := range artifacts {
		if !realizationRef.MatchString(artifact.EntryID) || !isKnownToolLifecycleChannel(artifact.Channel) || !realizationDigest.MatchString(artifact.InputDigest) || (i > 0 && artifact.EntryID == artifacts[i-1].EntryID) {
			return CapabilityFixtureObservation{}, fmt.Errorf("capability observation artifacts are malformed")
		}
	}
	canonical := o
	canonical.AppliedArtifacts = artifacts
	canonical.ObservedSurface = surface
	digest, err := observationDigest(canonical)
	if err != nil {
		return CapabilityFixtureObservation{}, err
	}
	canonical.ObservationDigest = digest
	return canonical, nil
}

// CompileCapabilityRealization is the versioned capability-realization contract surface.
func CompileCapabilityRealization(d CapabilityRealizationDeclaration, o CapabilityFixtureObservation) (CompiledCapabilityRealization, error) {
	rebuilt, err := NewCapabilityRealization(CapabilityRealizationInput{CapabilityID: d.CapabilityID, HarnessID: d.HarnessID, AdapterVersion: d.AdapterVersion, Mode: d.Mode, RecipeID: d.Recipe.RecipeID, Entries: d.Recipe.Entries, DeclaredSurface: d.Recipe.DeclaredSurface})
	if err != nil || !reflect.DeepEqual(rebuilt, d) {
		return CompiledCapabilityRealization{}, fmt.Errorf("capability declaration is not canonical")
	}
	canonicalObservation, err := rebuildObservation(o)
	if err != nil || canonicalObservation.ObservationDigest != o.ObservationDigest || o.CapabilityID != d.CapabilityID || o.HarnessID != d.HarnessID || o.AdapterVersion != d.AdapterVersion || o.Mode != d.Mode || o.RecipeDigest != d.Recipe.RecipeDigest {
		return CompiledCapabilityRealization{}, fmt.Errorf("capability observation binding is invalid")
	}
	artifacts := map[string]CapabilityAppliedArtifact{}
	for _, a := range o.AppliedArtifacts {
		artifacts[a.EntryID] = a
	}
	if len(artifacts) != len(d.Recipe.Entries) {
		return CompiledCapabilityRealization{}, fmt.Errorf("capability observation artifact coverage is incomplete")
	}
	for _, e := range d.Recipe.Entries {
		a, ok := artifacts[e.EntryID]
		if !ok || a.Channel != e.Channel || a.InputDigest != e.InputDigest {
			return CompiledCapabilityRealization{}, fmt.Errorf("capability observation artifact mismatch")
		}
	}
	observed := map[string]bool{}
	for _, v := range o.ObservedSurface {
		observed[surfaceKey(v)] = true
	}
	for _, v := range d.Recipe.DeclaredSurface {
		if !observed[surfaceKey(v)] {
			return CompiledCapabilityRealization{}, fmt.Errorf("capability observation surface is incomplete")
		}
	}
	return CompiledCapabilityRealization{Declaration: rebuilt, Observation: canonicalObservation}, nil
}

// CapabilityRealizationRegistry is the versioned capability-realization contract surface.
type CapabilityRealizationRegistry struct {
	mu           sync.RWMutex
	rows         map[string]CompiledCapabilityRealization
	capabilities map[string]bool
}

func cloneCompiled(in CompiledCapabilityRealization) CompiledCapabilityRealization {
	out := in
	out.Declaration.Recipe.Entries = append([]CapabilityRecipeEntry(nil), in.Declaration.Recipe.Entries...)
	for i := range out.Declaration.Recipe.Entries {
		out.Declaration.Recipe.Entries[i].SurfaceRefs = append([]CapabilitySurfaceIdentity(nil), in.Declaration.Recipe.Entries[i].SurfaceRefs...)
	}
	out.Declaration.Recipe.DeclaredSurface = append([]CapabilitySurfaceIdentity(nil), in.Declaration.Recipe.DeclaredSurface...)
	out.Observation.AppliedArtifacts = append([]CapabilityAppliedArtifact(nil), in.Observation.AppliedArtifacts...)
	out.Observation.ObservedSurface = append([]CapabilitySurfaceIdentity(nil), in.Observation.ObservedSurface...)
	return out
}

func realizationKey(c string, h HarnessName, a string, m PromptSessionMode) string {
	return c + "\x00" + string(h) + "\x00" + a + "\x00" + string(m)
}

// NewCapabilityRealizationRegistry is the versioned capability-realization contract surface.
func NewCapabilityRealizationRegistry(rows []CompiledCapabilityRealization) (*CapabilityRealizationRegistry, error) {
	r := &CapabilityRealizationRegistry{rows: map[string]CompiledCapabilityRealization{}, capabilities: map[string]bool{}}
	for _, row := range rows {
		compiled, err := CompileCapabilityRealization(row.Declaration, row.Observation)
		if err != nil {
			return nil, err
		}
		k := realizationKey(compiled.Declaration.CapabilityID, compiled.Declaration.HarnessID, compiled.Declaration.AdapterVersion, compiled.Declaration.Mode)
		if _, ok := r.rows[k]; ok {
			return nil, fmt.Errorf("duplicate capability realization")
		}
		r.rows[k] = cloneCompiled(compiled)
		r.capabilities[compiled.Declaration.CapabilityID] = true
	}
	return r, nil
}

// Knows is the versioned capability-realization contract surface.
func (r *CapabilityRealizationRegistry) Knows(c string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.capabilities[c]
}

// Resolve is the versioned capability-realization contract surface.
func (r *CapabilityRealizationRegistry) Resolve(c string, h HarnessName, a string, m PromptSessionMode) (CompiledCapabilityRealization, bool) {
	if r == nil {
		return CompiledCapabilityRealization{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.rows[realizationKey(c, h, a, m)]
	return cloneCompiled(v), ok
}

// BindCapabilityRealization is the versioned capability-realization contract surface.
func BindCapabilityRealization(c CompiledCapabilityRealization) CapabilityRealizationBinding {
	d := c.Declaration
	entries := append([]CapabilityRecipeEntry(nil), d.Recipe.Entries...)
	for i := range entries {
		entries[i].SurfaceRefs = append([]CapabilitySurfaceIdentity(nil), d.Recipe.Entries[i].SurfaceRefs...)
	}
	return CapabilityRealizationBinding{ContractVersion: CapabilityRealizationContractVersion, CapabilityID: d.CapabilityID, HarnessID: d.HarnessID, AdapterVersion: d.AdapterVersion, Mode: d.Mode, RecipeID: d.Recipe.RecipeID, RecipeDigest: d.Recipe.RecipeDigest, DeclaredSurfaceDigest: d.Recipe.DeclaredSurfaceDigest, Entries: entries, ObservationDigest: c.Observation.ObservationDigest}
}

// ResolveCapabilityRealizationResults is the versioned capability-realization contract surface.
func ResolveCapabilityRealizationResults(bindings []CapabilityRealizationBinding, entries []ToolLifecycleEntry) ([]CapabilityRealizationResult, error) {
	byID := map[string]ToolLifecycleEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	results := make([]CapabilityRealizationResult, 0, len(bindings))
	seen := map[string]bool{}
	for _, b := range bindings {
		if b.ContractVersion != CapabilityRealizationContractVersion || len(b.Entries) == 0 || seen[b.CapabilityID] || !realizationDigest.MatchString(b.ObservationDigest) {
			return nil, fmt.Errorf("capability realization binding is malformed")
		}
		seen[b.CapabilityID] = true
		r := CapabilityRealizationResult{CapabilityRealizationBinding: b, Decision: "artifact_bound"}
		for _, want := range b.Entries {
			got, ok := byID[want.EntryID]
			if !ok || got.Channel != want.Channel || got.InputDigest != want.InputDigest || got.Outcome != ToolOutcomeAdmitted {
				r.Decision = "denied"
				results = append(results, r)
				return results, fmt.Errorf("capability realization %q is partially applied", b.CapabilityID)
			}
		}
		results = append(results, r)
	}
	return results, nil
}
