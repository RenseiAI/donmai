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

// Capability realization contract versions distinguish immutable static rows
// from parameter-bound templates. The compatibility alias remains v1.
const (
	CapabilityRealizationContractVersionV1 = "donmai.capability-realization/v1"
	CapabilityRealizationContractVersionV2 = "donmai.capability-realization/v2"
	CapabilityRealizationContractVersion   = CapabilityRealizationContractVersionV1
	CapabilityParameterContractVersionV1   = "donmai.capability-parameter-contract/v1"
	CapabilityParameterBindingVersionV1    = "donmai.capability-parameter-binding/v1"
)

// CapabilityFixtureEvidenceContractVersion identifies executed-fixture evidence.
const CapabilityFixtureEvidenceContractVersion = "donmai.capability-fixture-evidence/v1"

// CapabilityEvidenceTier names what an executed fixture actually measured.
type CapabilityEvidenceTier string

const (
	// CapabilityEvidenceSmoked means the exact integration was exercised against
	// its pinned real harness binary. It does not attest a hosted deployment,
	// session grant, or live consumer acceptance.
	CapabilityEvidenceSmoked CapabilityEvidenceTier = "smoked"
)

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

// CapabilityParameterContractV1 is immutable catalog authority for narrowing a
// parameterized realization. Wire/session input cannot select or replace it.
type CapabilityParameterContractV1 struct {
	ContractVersion    string `json:"contractVersion"`
	ID                 string `json:"id"`
	BinderSourceDigest string `json:"binderSourceDigest"`
	SurfaceProjection  string `json:"surfaceProjection"`
	RuntimeObservation string `json:"runtimeObservation"`
}

// CapabilityParameterRequirementFacts is the package-neutral projection of an
// already verified execution-cell capability requirement.
type CapabilityParameterRequirementFacts struct {
	CapabilityID             string `json:"capabilityId"`
	ParametersDigest         string `json:"parametersDigest"`
	OperationalPayloadDigest string `json:"operationalPayloadDigest"`
}

// CapabilityBoundEntryV1 keeps static artifact identity separate from the
// session parameter binding applied to that entry.
type CapabilityBoundEntryV1 struct {
	EntryID               string `json:"entryId"`
	StaticInputDigest     string `json:"staticInputDigest"`
	EntryParametersDigest string `json:"entryParametersDigest"`
}

// CapabilityParameterBindingV1 is a closed, digest-only session projection.
// It is not authority by itself; runner construction owns the binder call.
type CapabilityParameterBindingV1 struct {
	ContractVersion          string                      `json:"contractVersion"`
	CapabilityID             string                      `json:"capabilityId"`
	ParameterContractID      string                      `json:"parameterContractId"`
	ParametersDigest         string                      `json:"parametersDigest"`
	OperationalPayloadDigest string                      `json:"operationalPayloadDigest"`
	StaticRecipeDigest       string                      `json:"staticRecipeDigest"`
	SelectedSurface          []CapabilitySurfaceIdentity `json:"selectedSurface"`
	SelectedSurfaceDigest    string                      `json:"selectedSurfaceDigest"`
	Entries                  []CapabilityBoundEntryV1    `json:"entries"`
	BindingDigest            string                      `json:"bindingDigest"`
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
	ContractVersion         string                         `json:"contractVersion"`
	CapabilityID            string                         `json:"capabilityId"`
	HarnessID               HarnessName                    `json:"harnessId"`
	AdapterVersion          string                         `json:"adapterVersion"`
	Mode                    PromptSessionMode              `json:"mode"`
	Recipe                  CapabilityRealizationRecipe    `json:"recipe"`
	ParameterContract       *CapabilityParameterContractV1 `json:"parameterContract,omitempty"`
	ParameterContractDigest string                         `json:"parameterContractDigest,omitempty"`
	TemplateDigest          string                         `json:"templateDigest,omitempty"`
}

// CapabilityRealizationInput is the versioned capability-realization contract surface.
type CapabilityRealizationInput struct {
	CapabilityID      string
	HarnessID         HarnessName
	AdapterVersion    string
	Mode              PromptSessionMode
	RecipeID          string
	Entries           []CapabilityRecipeEntry
	DeclaredSurface   []CapabilitySurfaceIdentity
	ParameterContract *CapabilityParameterContractV1
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
	TemplateDigest    string                      `json:"templateDigest,omitempty"`
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

// CapabilityFixtureProducer identifies the executable fixture and mandatory
// release gate that produced an observation. The compiler hashes Source to bind
// the reviewed producer implementation; validation of that digest does not
// itself prove the fixture ran, so consumers must also trust the generated
// artifact's source and release provenance.
type CapabilityFixtureProducer struct {
	ID           string
	Source       []byte
	BinderSource []byte
	ReleaseGate  string
}

// CapabilityFixtureExecution is the direct output of one fixture execution.
// Success is represented by returning an observation; skipped and failed runs
// return an error through the matrix executor and cannot produce evidence.
type CapabilityFixtureExecution struct {
	Producer    CapabilityFixtureProducer
	Observation CapabilityFixtureObservation
}

// CapabilityRealizationEvidence binds a canonical compiled realization to the
// executed fixture producer that observed it.
type CapabilityRealizationEvidence struct {
	ContractVersion      string            `json:"contractVersion"`
	ProducerID           string            `json:"producerId"`
	ProducerSourceDigest string            `json:"producerSourceDigest"`
	ReleaseGate          string            `json:"releaseGate"`
	CapabilityID         string            `json:"capabilityId"`
	HarnessID            HarnessName       `json:"harnessId"`
	AdapterVersion       string            `json:"adapterVersion"`
	Mode                 PromptSessionMode `json:"mode"`
	RecipeDigest         string            `json:"recipeDigest"`
	TemplateDigest       string            `json:"templateDigest,omitempty"`
	FixtureID            string            `json:"fixtureId"`
	BinaryDigest         string            `json:"binaryDigest"`
	ObservationDigest    string            `json:"observationDigest"`
	EvidenceDigest       string            `json:"evidenceDigest"`
}

// CompiledCapabilityRealizationEvidence is the validated generated-row shape.
// EvidenceTier and ProductionEligible are derived outputs: callers never pass
// either value to the compiler.
type CompiledCapabilityRealizationEvidence struct {
	Compiled           CompiledCapabilityRealization `json:"compiled"`
	Evidence           CapabilityRealizationEvidence `json:"evidence"`
	EvidenceTier       CapabilityEvidenceTier        `json:"evidenceTier"`
	ProductionEligible bool                          `json:"productionEligible"`
}

// UnmarshalJSON preserves evidence template member presence relative to the
// nested realization version. The evidence codec has its own version string,
// so this enclosing row is the first boundary that can decide v1 versus v2.
func (r *CompiledCapabilityRealizationEvidence) UnmarshalJSON(data []byte) error {
	type wire CompiledCapabilityRealizationEvidence
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	members, err := rawMembers(data)
	if err != nil {
		return err
	}
	var evidenceMembers map[string]json.RawMessage
	if raw, ok := members["evidence"]; !ok || json.Unmarshal(raw, &evidenceMembers) != nil || evidenceMembers == nil {
		return fmt.Errorf("capability realization evidence wire is malformed")
	}
	if err := requireVersionedMembers(decoded.Compiled.Declaration.ContractVersion, evidenceMembers, "templateDigest"); err != nil {
		return err
	}
	*r = CompiledCapabilityRealizationEvidence(decoded)
	return nil
}

// CapabilityRealizationBinding is the versioned capability-realization contract surface.
type CapabilityRealizationBinding struct {
	ContractVersion         string                         `json:"contractVersion"`
	CapabilityID            string                         `json:"capabilityId"`
	HarnessID               HarnessName                    `json:"harnessId"`
	AdapterVersion          string                         `json:"adapterVersion"`
	Mode                    PromptSessionMode              `json:"mode"`
	RecipeID                string                         `json:"recipeId"`
	RecipeDigest            string                         `json:"recipeDigest"`
	DeclaredSurfaceDigest   string                         `json:"declaredSurfaceDigest"`
	Entries                 []CapabilityRecipeEntry        `json:"entries"`
	FixtureID               string                         `json:"fixtureId"`
	BinaryDigest            string                         `json:"binaryDigest"`
	AppliedArtifacts        []CapabilityAppliedArtifact    `json:"appliedArtifacts"`
	ObservedSurface         []CapabilitySurfaceIdentity    `json:"observedSurface"`
	ObservationDigest       string                         `json:"observationDigest"`
	ParameterContract       *CapabilityParameterContractV1 `json:"parameterContract,omitempty"`
	ParameterContractDigest string                         `json:"parameterContractDigest,omitempty"`
	TemplateDigest          string                         `json:"templateDigest,omitempty"`
	ParameterBinding        *CapabilityParameterBindingV1  `json:"parameterBinding,omitempty"`
}

// CapabilityRealizationResult is the versioned capability-realization contract surface.
type CapabilityRealizationResult struct {
	CapabilityRealizationBinding
	Decision string `json:"decision"`
}

// UnmarshalJSON preserves the flattened binding plus result decision even
// though the embedded binding has its own strict version-aware decoder.
func (r *CapabilityRealizationResult) UnmarshalJSON(data []byte) error {
	var binding CapabilityRealizationBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return err
	}
	var result struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	r.CapabilityRealizationBinding = binding
	r.Decision = result.Decision
	return nil
}

func rawMembers(data []byte) (map[string]json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		return nil, fmt.Errorf("capability realization wire is malformed")
	}
	return members, nil
}

func requireVersionedMembers(version string, members map[string]json.RawMessage, v2Names ...string) error {
	switch version {
	case CapabilityRealizationContractVersionV1:
		for _, name := range v2Names {
			if _, present := members[name]; present {
				return fmt.Errorf("v1 capability realization contains v2 member %q", name)
			}
		}
	case CapabilityRealizationContractVersionV2:
		for _, name := range v2Names {
			if raw, present := members[name]; !present || string(raw) == "null" {
				return fmt.Errorf("v2 capability realization omits member %q", name)
			}
		}
	default:
		return fmt.Errorf("capability realization contract version is unsupported")
	}
	return nil
}

// UnmarshalJSON rejects v2-only member presence on raw v1 wire, including
// explicit null/empty values that ordinary Go decoding would erase.
func (d *CapabilityRealizationDeclaration) UnmarshalJSON(data []byte) error {
	type wire CapabilityRealizationDeclaration
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	members, err := rawMembers(data)
	if err != nil {
		return err
	}
	if err := requireVersionedMembers(decoded.ContractVersion, members, "parameterContract", "parameterContractDigest", "templateDigest"); err != nil {
		return err
	}
	*d = CapabilityRealizationDeclaration(decoded)
	return nil
}

// UnmarshalJSON preserves the v1/v2 observation member boundary.
func (o *CapabilityFixtureObservation) UnmarshalJSON(data []byte) error {
	type wire CapabilityFixtureObservation
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	members, err := rawMembers(data)
	if err != nil {
		return err
	}
	if err := requireVersionedMembers(decoded.ContractVersion, members, "templateDigest"); err != nil {
		return err
	}
	*o = CapabilityFixtureObservation(decoded)
	return nil
}

// UnmarshalJSON preserves the v1/v2 evidence member boundary.
func (e *CapabilityRealizationEvidence) UnmarshalJSON(data []byte) error {
	type wire CapabilityRealizationEvidence
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	// Evidence carries its own v1 contract version. Its template member is
	// selected by the nested realization tuple rather than this evidence codec,
	// so raw v1 protection is enforced by ValidateCompiled... against the
	// declaration. Decode still retains the member exactly.
	*e = CapabilityRealizationEvidence(decoded)
	return nil
}

// UnmarshalJSON rejects raw v2-only binding members on v1 receipts.
func (b *CapabilityRealizationBinding) UnmarshalJSON(data []byte) error {
	type wire CapabilityRealizationBinding
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	members, err := rawMembers(data)
	if err != nil {
		return err
	}
	if err := requireVersionedMembers(decoded.ContractVersion, members, "parameterContract", "parameterContractDigest", "templateDigest", "parameterBinding"); err != nil {
		return err
	}
	*b = CapabilityRealizationBinding(decoded)
	return nil
}

var (
	realizationRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,255}$`)
	realizationDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidCapabilityParameterContractID reports whether id uses the canonical
// realization-reference grammar shared by catalog and binder descriptors.
func ValidCapabilityParameterContractID(id string) bool {
	return realizationRef.MatchString(id)
}

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

func capabilitySurfaceDigest(surface []CapabilitySurfaceIdentity) (string, error) {
	canonical, err := canonicalSurface(surface)
	if err != nil || len(canonical) == 0 {
		return "", fmt.Errorf("capability realization surface is incomplete")
	}
	return domainDigest("donmai.capability-realization.surface/v1", canonical)
}

func canonicalParameterContract(contract *CapabilityParameterContractV1) (*CapabilityParameterContractV1, string, error) {
	if contract == nil {
		return nil, "", nil
	}
	canonical := *contract
	if canonical.ContractVersion != CapabilityParameterContractVersionV1 ||
		!realizationRef.MatchString(canonical.ID) ||
		!realizationDigest.MatchString(canonical.BinderSourceDigest) ||
		canonical.SurfaceProjection != "subset" ||
		(canonical.RuntimeObservation != "none" && canonical.RuntimeObservation != "before_first_turn") {
		return nil, "", fmt.Errorf("capability parameter contract is malformed")
	}
	digest, err := domainDigest("donmai.capability-realization.parameter-contract/v1", canonical)
	if err != nil {
		return nil, "", err
	}
	return &canonical, digest, nil
}

func capabilityTemplateDigest(d CapabilityRealizationDeclaration) (string, error) {
	core := struct {
		CapabilityID            string            `json:"capabilityId"`
		HarnessID               HarnessName       `json:"harnessId"`
		AdapterVersion          string            `json:"adapterVersion"`
		Mode                    PromptSessionMode `json:"mode"`
		RecipeDigest            string            `json:"recipeDigest"`
		DeclaredSurfaceDigest   string            `json:"declaredSurfaceDigest"`
		ParameterContractDigest string            `json:"parameterContractDigest"`
	}{d.CapabilityID, d.HarnessID, d.AdapterVersion, d.Mode, d.Recipe.RecipeDigest, d.Recipe.DeclaredSurfaceDigest, d.ParameterContractDigest}
	return domainDigest("donmai.capability-realization.template/v2", core)
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
	contract, contractDigest, err := canonicalParameterContract(input.ParameterContract)
	if err != nil {
		return CapabilityRealizationDeclaration{}, err
	}
	version := CapabilityRealizationContractVersionV1
	if contract != nil {
		version = CapabilityRealizationContractVersionV2
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
	requiredEntries := 0
	for i := range entries {
		e := &entries[i]
		refs, refErr := canonicalSurface(e.SurfaceRefs)
		e.SurfaceRefs = refs
		if refErr != nil || (version == CapabilityRealizationContractVersionV1 && !e.Required) || !realizationRef.MatchString(e.EntryID) || !isKnownToolLifecycleChannel(e.Channel) || !realizationDigest.MatchString(e.InputDigest) || seen[e.EntryID] || len(refs) == 0 {
			return CapabilityRealizationDeclaration{}, fmt.Errorf("capability realization entries are malformed")
		}
		if version == CapabilityRealizationContractVersionV2 && e.Channel == ToolChannelToolPlugin {
			if _, namedErr := additionalExtensionDeliveryID(e.EntryID); namedErr != nil {
				return CapabilityRealizationDeclaration{}, fmt.Errorf("parameterized tool-plugin realization requires a named additional-extension entry")
			}
		}
		if e.Required {
			requiredEntries++
		}
		seen[e.EntryID] = true
		for _, ref := range refs {
			if !declared[surfaceKey(ref)] {
				return CapabilityRealizationDeclaration{}, fmt.Errorf("capability recipe references undeclared surface")
			}
			covered[surfaceKey(ref)] = true
		}
	}
	if len(entries) == 0 || (version == CapabilityRealizationContractVersionV1 && requiredEntries == 0) || len(covered) != len(declared) {
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
	surfaceDigest, err := capabilitySurfaceDigest(surface)
	if err != nil {
		return CapabilityRealizationDeclaration{}, err
	}
	recipe := CapabilityRealizationRecipe{RecipeID: input.RecipeID, Entries: entries, DeclaredSurface: surface, RecipeDigest: recipeDigest, DeclaredSurfaceDigest: surfaceDigest}
	declaration := CapabilityRealizationDeclaration{
		ContractVersion: version, CapabilityID: input.CapabilityID, HarnessID: input.HarnessID,
		AdapterVersion: input.AdapterVersion, Mode: input.Mode, Recipe: recipe,
		ParameterContract: contract, ParameterContractDigest: contractDigest,
	}
	if version == CapabilityRealizationContractVersionV2 {
		declaration.TemplateDigest, err = capabilityTemplateDigest(declaration)
		if err != nil {
			return CapabilityRealizationDeclaration{}, err
		}
	}
	return declaration, nil
}

// NewCapabilityFixtureObservation is the versioned capability-realization contract surface.
func NewCapabilityFixtureObservation(input CapabilityFixtureObservationInput) (CapabilityFixtureObservation, error) {
	d := input.Declaration
	if (d.ContractVersion != CapabilityRealizationContractVersionV1 && d.ContractVersion != CapabilityRealizationContractVersionV2) || !realizationRef.MatchString(input.FixtureID) || !realizationDigest.MatchString(input.BinaryDigest) {
		return CapabilityFixtureObservation{}, fmt.Errorf("capability observation identity is malformed")
	}
	observed, err := canonicalSurface(input.ObservedSurface)
	if err != nil {
		return CapabilityFixtureObservation{}, err
	}
	artifacts := append([]CapabilityAppliedArtifact(nil), input.AppliedArtifacts...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].EntryID < artifacts[j].EntryID })
	o := CapabilityFixtureObservation{ContractVersion: d.ContractVersion, CapabilityID: d.CapabilityID, HarnessID: d.HarnessID, AdapterVersion: d.AdapterVersion, Mode: d.Mode, RecipeDigest: d.Recipe.RecipeDigest, TemplateDigest: d.TemplateDigest, FixtureID: input.FixtureID, BinaryDigest: input.BinaryDigest, AppliedArtifacts: artifacts, ObservedSurface: observed}
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
	if (o.ContractVersion != CapabilityRealizationContractVersionV1 && o.ContractVersion != CapabilityRealizationContractVersionV2) || !realizationRef.MatchString(o.CapabilityID) || o.HarnessID == "" || !realizationRef.MatchString(o.AdapterVersion) || o.Mode == "" || !realizationDigest.MatchString(o.RecipeDigest) || !realizationRef.MatchString(o.FixtureID) || !realizationDigest.MatchString(o.BinaryDigest) || !realizationDigest.MatchString(o.ObservationDigest) {
		return CapabilityFixtureObservation{}, fmt.Errorf("capability observation identity is malformed")
	}
	if (o.ContractVersion == CapabilityRealizationContractVersionV1 && o.TemplateDigest != "") ||
		(o.ContractVersion == CapabilityRealizationContractVersionV2 && !realizationDigest.MatchString(o.TemplateDigest)) {
		return CapabilityFixtureObservation{}, fmt.Errorf("capability observation template binding is malformed")
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
	rebuilt, err := NewCapabilityRealization(CapabilityRealizationInput{CapabilityID: d.CapabilityID, HarnessID: d.HarnessID, AdapterVersion: d.AdapterVersion, Mode: d.Mode, RecipeID: d.Recipe.RecipeID, Entries: d.Recipe.Entries, DeclaredSurface: d.Recipe.DeclaredSurface, ParameterContract: d.ParameterContract})
	if err != nil || !reflect.DeepEqual(rebuilt, d) {
		return CompiledCapabilityRealization{}, fmt.Errorf("capability declaration is not canonical")
	}
	canonicalObservation, err := rebuildObservation(o)
	if err != nil || canonicalObservation.ObservationDigest != o.ObservationDigest || o.CapabilityID != d.CapabilityID || o.HarnessID != d.HarnessID || o.AdapterVersion != d.AdapterVersion || o.Mode != d.Mode || o.RecipeDigest != d.Recipe.RecipeDigest || o.TemplateDigest != d.TemplateDigest {
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

// CompileCapabilityRealizationEvidence derives eligibility from one executed
// fixture observation. The trusted generator invokes the fixture; this codec
// validates and binds its output without accepting a pass or eligibility flag.
func CompileCapabilityRealizationEvidence(
	d CapabilityRealizationDeclaration,
	execution CapabilityFixtureExecution,
) (CompiledCapabilityRealizationEvidence, error) {
	producer := execution.Producer
	if !realizationRef.MatchString(producer.ID) || len(producer.Source) == 0 || !realizationRef.MatchString(producer.ReleaseGate) {
		return CompiledCapabilityRealizationEvidence{}, fmt.Errorf("capability fixture producer identity is malformed")
	}
	if d.ContractVersion == CapabilityRealizationContractVersionV2 {
		if d.ParameterContract == nil || len(producer.BinderSource) == 0 {
			return CompiledCapabilityRealizationEvidence{}, fmt.Errorf("capability fixture binder source is required")
		}
		binderSourceDigest := sha256.Sum256(producer.BinderSource)
		if hex.EncodeToString(binderSourceDigest[:]) != d.ParameterContract.BinderSourceDigest {
			return CompiledCapabilityRealizationEvidence{}, fmt.Errorf("capability fixture binder source does not match parameter contract")
		}
	}
	compiled, err := CompileCapabilityRealization(d, execution.Observation)
	if err != nil {
		return CompiledCapabilityRealizationEvidence{}, fmt.Errorf("compile capability fixture observation: %w", err)
	}
	producerSourceDigest := sha256.Sum256(producer.Source)
	evidence := CapabilityRealizationEvidence{
		ContractVersion: CapabilityFixtureEvidenceContractVersion,
		ProducerID:      producer.ID, ProducerSourceDigest: hex.EncodeToString(producerSourceDigest[:]), ReleaseGate: producer.ReleaseGate,
		CapabilityID: d.CapabilityID, HarnessID: d.HarnessID, AdapterVersion: d.AdapterVersion, Mode: d.Mode,
		RecipeDigest: d.Recipe.RecipeDigest, TemplateDigest: d.TemplateDigest, FixtureID: compiled.Observation.FixtureID,
		BinaryDigest: compiled.Observation.BinaryDigest, ObservationDigest: compiled.Observation.ObservationDigest,
	}
	evidence.EvidenceDigest, err = capabilityEvidenceDigest(evidence)
	if err != nil {
		return CompiledCapabilityRealizationEvidence{}, err
	}
	return CompiledCapabilityRealizationEvidence{
		Compiled: compiled, Evidence: evidence,
		EvidenceTier: CapabilityEvidenceSmoked, ProductionEligible: true,
	}, nil
}

func capabilityEvidenceDigest(evidence CapabilityRealizationEvidence) (string, error) {
	evidence.EvidenceDigest = ""
	return domainDigest("donmai.capability-fixture-evidence/v1", evidence)
}

// ValidateCompiledCapabilityRealizationEvidence validates generated artifact
// bytes without executing their fixture. A valid hash proves internal
// consistency only; the caller must trust the artifact source and the named
// non-skipped release gate before treating it as empirical evidence.
func ValidateCompiledCapabilityRealizationEvidence(row CompiledCapabilityRealizationEvidence) error {
	if row.EvidenceTier != CapabilityEvidenceSmoked || !row.ProductionEligible {
		return fmt.Errorf("capability eligibility is not compiler-derived")
	}
	compiled, err := CompileCapabilityRealization(row.Compiled.Declaration, row.Compiled.Observation)
	if err != nil || !reflect.DeepEqual(compiled, row.Compiled) || row.Evidence.ContractVersion != CapabilityFixtureEvidenceContractVersion ||
		!realizationRef.MatchString(row.Evidence.ProducerID) || !realizationDigest.MatchString(row.Evidence.ProducerSourceDigest) || !realizationRef.MatchString(row.Evidence.ReleaseGate) ||
		row.Evidence.CapabilityID != row.Compiled.Declaration.CapabilityID || row.Evidence.HarnessID != row.Compiled.Declaration.HarnessID ||
		row.Evidence.AdapterVersion != row.Compiled.Declaration.AdapterVersion || row.Evidence.Mode != row.Compiled.Declaration.Mode ||
		row.Evidence.RecipeDigest != row.Compiled.Declaration.Recipe.RecipeDigest || row.Evidence.TemplateDigest != row.Compiled.Declaration.TemplateDigest || row.Evidence.FixtureID != row.Compiled.Observation.FixtureID ||
		row.Evidence.BinaryDigest != row.Compiled.Observation.BinaryDigest || row.Evidence.ObservationDigest != row.Compiled.Observation.ObservationDigest {
		return fmt.Errorf("compiled capability realization evidence is invalid")
	}
	digest, err := capabilityEvidenceDigest(row.Evidence)
	if err != nil || digest != row.Evidence.EvidenceDigest {
		return fmt.Errorf("compiled capability realization evidence is invalid")
	}
	return nil
}

// NewCapabilityRealizationRegistryFromEvidence validates generated rows and
// constructs the existing runtime registry without executing fixtures. Trusted
// source and release provenance remain a caller responsibility.
func NewCapabilityRealizationRegistryFromEvidence(rows []CompiledCapabilityRealizationEvidence) (*CapabilityRealizationRegistry, error) {
	compiled := make([]CompiledCapabilityRealization, len(rows))
	for i, row := range rows {
		if err := ValidateCompiledCapabilityRealizationEvidence(row); err != nil {
			return nil, err
		}
		compiled[i] = row.Compiled
	}
	return NewCapabilityRealizationRegistry(compiled)
}

// CapabilityRealizationRegistry is the versioned capability-realization contract surface.
type CapabilityRealizationRegistry struct {
	mu           sync.RWMutex
	rows         map[string]CompiledCapabilityRealization
	capabilities map[string]bool
}

func cloneCompiled(in CompiledCapabilityRealization) CompiledCapabilityRealization {
	out := in
	if in.Declaration.ParameterContract != nil {
		contract := *in.Declaration.ParameterContract
		out.Declaration.ParameterContract = &contract
	}
	out.Declaration.Recipe.Entries = append([]CapabilityRecipeEntry(nil), in.Declaration.Recipe.Entries...)
	for i := range out.Declaration.Recipe.Entries {
		out.Declaration.Recipe.Entries[i].SurfaceRefs = append([]CapabilitySurfaceIdentity(nil), in.Declaration.Recipe.Entries[i].SurfaceRefs...)
	}
	out.Declaration.Recipe.DeclaredSurface = append([]CapabilitySurfaceIdentity(nil), in.Declaration.Recipe.DeclaredSurface...)
	out.Observation.AppliedArtifacts = append([]CapabilityAppliedArtifact(nil), in.Observation.AppliedArtifacts...)
	out.Observation.ObservedSurface = append([]CapabilitySurfaceIdentity(nil), in.Observation.ObservedSurface...)
	return out
}

// ComposeCapabilityRealizationRegistries creates one immutable union and
// refuses duplicate exact tuples, including byte-identical duplicates.
func ComposeCapabilityRealizationRegistries(registries ...*CapabilityRealizationRegistry) (*CapabilityRealizationRegistry, error) {
	rows := make([]CompiledCapabilityRealization, 0)
	for _, registry := range registries {
		if registry == nil {
			continue
		}
		registry.mu.RLock()
		keys := make([]string, 0, len(registry.rows))
		for key := range registry.rows {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			rows = append(rows, cloneCompiled(registry.rows[key]))
		}
		registry.mu.RUnlock()
	}
	return NewCapabilityRealizationRegistry(rows)
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
	o := c.Observation
	entries := append([]CapabilityRecipeEntry(nil), d.Recipe.Entries...)
	for i := range entries {
		entries[i].SurfaceRefs = append([]CapabilitySurfaceIdentity(nil), d.Recipe.Entries[i].SurfaceRefs...)
	}
	binding := CapabilityRealizationBinding{
		ContractVersion: d.ContractVersion,
		CapabilityID:    d.CapabilityID, HarnessID: d.HarnessID, AdapterVersion: d.AdapterVersion, Mode: d.Mode,
		RecipeID: d.Recipe.RecipeID, RecipeDigest: d.Recipe.RecipeDigest,
		DeclaredSurfaceDigest: d.Recipe.DeclaredSurfaceDigest, Entries: entries,
		FixtureID: o.FixtureID, BinaryDigest: o.BinaryDigest,
		AppliedArtifacts:        append([]CapabilityAppliedArtifact(nil), o.AppliedArtifacts...),
		ObservedSurface:         append([]CapabilitySurfaceIdentity(nil), o.ObservedSurface...),
		ObservationDigest:       o.ObservationDigest,
		ParameterContractDigest: d.ParameterContractDigest,
		TemplateDigest:          d.TemplateDigest,
	}
	if d.ParameterContract != nil {
		contract := *d.ParameterContract
		binding.ParameterContract = &contract
	}
	return binding
}

// CapabilitySelectedSurfaceDigest uses the same domain and canonical ordering
// as static declared surfaces.
func CapabilitySelectedSurfaceDigest(surface []CapabilitySurfaceIdentity) (string, error) {
	return capabilitySurfaceDigest(surface)
}

// CapabilityEntryParametersDigest binds one static artifact input to the exact
// admitted session parameters and selected surface.
func CapabilityEntryParametersDigest(entryID, staticInputDigest, parametersDigest, selectedSurfaceDigest, operationalPayloadDigest string) (string, error) {
	core := struct {
		EntryID                  string `json:"entryId"`
		StaticInputDigest        string `json:"staticInputDigest"`
		ParametersDigest         string `json:"parametersDigest"`
		SelectedSurfaceDigest    string `json:"selectedSurfaceDigest"`
		OperationalPayloadDigest string `json:"operationalPayloadDigest"`
	}{entryID, staticInputDigest, parametersDigest, selectedSurfaceDigest, operationalPayloadDigest}
	return domainDigest("donmai.capability-realization.entry-parameters/v1", core)
}

// CapabilityParameterBindingDigest returns the canonical binding digest. The
// value is a codec primitive and does not attest that a process-owned binder ran.
func CapabilityParameterBindingDigest(binding CapabilityParameterBindingV1) (string, error) {
	binding.BindingDigest = ""
	return domainDigest("donmai.capability-realization.parameter-binding/v1", binding)
}

// ValidateCapabilityParameterBinding validates an untrusted binder result from
// first principles against static catalog authority and verified requirement facts.
func ValidateCapabilityParameterBinding(binding CapabilityParameterBindingV1, declaration CapabilityRealizationDeclaration, requirement CapabilityParameterRequirementFacts) error {
	if declaration.ContractVersion != CapabilityRealizationContractVersionV2 || declaration.ParameterContract == nil ||
		binding.ContractVersion != CapabilityParameterBindingVersionV1 || binding.CapabilityID != declaration.CapabilityID ||
		binding.ParameterContractID != declaration.ParameterContract.ID || binding.ParametersDigest != requirement.ParametersDigest ||
		binding.OperationalPayloadDigest != requirement.OperationalPayloadDigest || binding.StaticRecipeDigest != declaration.Recipe.RecipeDigest ||
		requirement.CapabilityID != declaration.CapabilityID || !realizationDigest.MatchString(binding.BindingDigest) {
		return fmt.Errorf("capability parameter binding identity is invalid")
	}
	selected, err := canonicalSurface(binding.SelectedSurface)
	if err != nil || len(selected) == 0 || !reflect.DeepEqual(selected, binding.SelectedSurface) {
		return fmt.Errorf("capability parameter binding selected surface is invalid")
	}
	selectedDigest, err := capabilitySurfaceDigest(selected)
	if err != nil || selectedDigest != binding.SelectedSurfaceDigest {
		return fmt.Errorf("capability parameter binding selected surface is invalid")
	}
	declared := make(map[string]bool, len(declaration.Recipe.DeclaredSurface))
	for _, identity := range declaration.Recipe.DeclaredSurface {
		declared[surfaceKey(identity)] = true
	}
	for _, identity := range selected {
		if !declared[surfaceKey(identity)] {
			return fmt.Errorf("capability parameter binding widens declared surface")
		}
	}
	if len(binding.Entries) != len(declaration.Recipe.Entries) {
		return fmt.Errorf("capability parameter binding entry coverage is incomplete")
	}
	entriesByID := make(map[string]CapabilityRecipeEntry, len(declaration.Recipe.Entries))
	for _, entry := range declaration.Recipe.Entries {
		entriesByID[entry.EntryID] = entry
	}
	previous := ""
	for _, entry := range binding.Entries {
		static, ok := entriesByID[entry.EntryID]
		if !ok || entry.EntryID <= previous || entry.StaticInputDigest != static.InputDigest || !realizationDigest.MatchString(entry.EntryParametersDigest) {
			return fmt.Errorf("capability parameter binding entries are invalid")
		}
		expected, digestErr := CapabilityEntryParametersDigest(entry.EntryID, entry.StaticInputDigest, binding.ParametersDigest, binding.SelectedSurfaceDigest, binding.OperationalPayloadDigest)
		if digestErr != nil || expected != entry.EntryParametersDigest {
			return fmt.Errorf("capability parameter binding entries are invalid")
		}
		previous = entry.EntryID
	}
	digest, err := CapabilityParameterBindingDigest(binding)
	if err != nil || digest != binding.BindingDigest {
		return fmt.Errorf("capability parameter binding digest is invalid")
	}
	return nil
}

func validateCapabilityRealizationBinding(b CapabilityRealizationBinding) error {
	if (b.ContractVersion != CapabilityRealizationContractVersionV1 && b.ContractVersion != CapabilityRealizationContractVersionV2) || len(b.Entries) == 0 {
		return fmt.Errorf("capability realization binding is malformed")
	}
	if b.ContractVersion == CapabilityRealizationContractVersionV1 {
		if b.ParameterContract != nil || b.ParameterContractDigest != "" || b.TemplateDigest != "" || b.ParameterBinding != nil {
			return fmt.Errorf("capability realization binding is malformed")
		}
	} else if b.ParameterContract == nil || !realizationDigest.MatchString(b.ParameterContractDigest) || !realizationDigest.MatchString(b.TemplateDigest) || b.ParameterBinding == nil {
		return fmt.Errorf("capability realization binding is malformed")
	}
	declared := make([]CapabilitySurfaceIdentity, 0)
	declaredSeen := map[string]bool{}
	for _, entry := range b.Entries {
		for _, surface := range entry.SurfaceRefs {
			if key := surfaceKey(surface); !declaredSeen[key] {
				declaredSeen[key] = true
				declared = append(declared, surface)
			}
		}
	}
	canonicalDeclared, err := canonicalSurface(declared)
	if err != nil {
		return fmt.Errorf("capability realization binding is malformed")
	}
	declaration, err := NewCapabilityRealization(CapabilityRealizationInput{
		CapabilityID: b.CapabilityID, HarnessID: b.HarnessID, AdapterVersion: b.AdapterVersion,
		Mode: b.Mode, RecipeID: b.RecipeID, Entries: b.Entries, DeclaredSurface: canonicalDeclared,
		ParameterContract: b.ParameterContract,
	})
	if err != nil || declaration.Recipe.RecipeDigest != b.RecipeDigest || declaration.Recipe.DeclaredSurfaceDigest != b.DeclaredSurfaceDigest || declaration.ParameterContractDigest != b.ParameterContractDigest || declaration.TemplateDigest != b.TemplateDigest {
		return fmt.Errorf("capability realization binding recipe is invalid")
	}
	observation := CapabilityFixtureObservation{
		ContractVersion: b.ContractVersion,
		CapabilityID:    b.CapabilityID, HarnessID: b.HarnessID, AdapterVersion: b.AdapterVersion,
		Mode: b.Mode, RecipeDigest: b.RecipeDigest, TemplateDigest: b.TemplateDigest, FixtureID: b.FixtureID, BinaryDigest: b.BinaryDigest,
		AppliedArtifacts:  append([]CapabilityAppliedArtifact(nil), b.AppliedArtifacts...),
		ObservedSurface:   append([]CapabilitySurfaceIdentity(nil), b.ObservedSurface...),
		ObservationDigest: b.ObservationDigest,
	}
	compiled, err := CompileCapabilityRealization(declaration, observation)
	expected := BindCapabilityRealization(compiled)
	expected.ParameterBinding = b.ParameterBinding
	if err != nil || !reflect.DeepEqual(expected, b) {
		return fmt.Errorf("capability realization binding observation is invalid")
	}
	if b.ParameterBinding != nil {
		requirement := CapabilityParameterRequirementFacts{CapabilityID: b.CapabilityID, ParametersDigest: b.ParameterBinding.ParametersDigest, OperationalPayloadDigest: b.ParameterBinding.OperationalPayloadDigest}
		if err := ValidateCapabilityParameterBinding(*b.ParameterBinding, declaration, requirement); err != nil {
			return err
		}
	}
	return nil
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
		if seen[b.CapabilityID] || validateCapabilityRealizationBinding(b) != nil {
			return nil, fmt.Errorf("capability realization binding is malformed")
		}
		seen[b.CapabilityID] = true
		r := CapabilityRealizationResult{CapabilityRealizationBinding: b, Decision: "artifact_bound"}
		for _, want := range b.Entries {
			got, ok := byID[want.EntryID]
			expectedOutcome := ToolOutcomeAdmitted
			if b.ContractVersion == CapabilityRealizationContractVersionV2 && b.ParameterContract != nil && b.ParameterContract.RuntimeObservation == "before_first_turn" && want.Channel == ToolChannelToolPlugin {
				expectedOutcome = ToolOutcomePendingRuntime
			}
			if !ok || got.Channel != want.Channel || got.Required != want.Required || got.InputDigest != want.InputDigest || got.Outcome != expectedOutcome {
				r.Decision = "denied"
				results = append(results, r)
				return results, fmt.Errorf("capability realization %q is partially applied", b.CapabilityID)
			}
		}
		results = append(results, r)
	}
	return results, nil
}
