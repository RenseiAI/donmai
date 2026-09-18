package runner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

func selectionProducerFixture(t *testing.T) (*agent.CapabilityRealizationRegistry, agent.CompiledCapabilityRealization, CapabilityRealizationSelectionTarget) {
	t.Helper()
	surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceMCPServer, ID: "local-authoring"}}
	inputDigest := strings.Repeat("a", 64)
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
		CapabilityID:   "example.local-authoring/v1",
		HarnessID:      agent.HarnessCodex,
		AdapterVersion: "codex/local/authoring-v1",
		Mode:           agent.PromptModeHumanControlled,
		RecipeID:       "example/local-authoring-recipe/v1",
		Entries: []agent.CapabilityRecipeEntry{{
			EntryID: "mcp-servers", Channel: agent.ToolChannelMCPServer, Required: true,
			InputDigest: inputDigest, SurfaceRefs: surface,
		}},
		DeclaredSurface: surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
		Declaration: declaration, FixtureID: "local-constructor-fixture", BinaryDigest: strings.Repeat("b", 64),
		AppliedArtifacts: []agent.CapabilityAppliedArtifact{{
			EntryID: "mcp-servers", Channel: agent.ToolChannelMCPServer, InputDigest: inputDigest,
		}},
		ObservedSurface: surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.CompileCapabilityRealization(declaration, observation)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	target := CapabilityRealizationSelectionTarget{
		CapabilityID: compiled.Declaration.CapabilityID, HarnessID: compiled.Declaration.HarnessID,
		AdapterVersion: compiled.Declaration.AdapterVersion, Mode: compiled.Declaration.Mode,
	}
	return registry, compiled, target
}

func TestComposeCapabilityRealizationSelectionOperationalPayloadExactRegistryProjection(t *testing.T) {
	t.Parallel()
	registry, compiled, target := selectionProducerFixture(t)
	raw := []byte(` { "sessionId" : "request_local" , "ratio" : 1.25 , "nested" : { "count" : 42 } } `)
	before := bytes.Clone(raw)

	produced, err := ComposeCapabilityRealizationSelectionOperationalPayload(raw, registry, target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, before) {
		t.Fatalf("caller bytes mutated\n got: %s\nwant: %s", raw, before)
	}
	const want = `{"capabilityRealizationSelection":{"adapterVersion":"codex/local/authoring-v1","capabilityId":"example.local-authoring/v1","contractVersion":"donmai.capability-realization-selection/v1","declaredSurfaceDigest":"5850d77510604baf5041c90f3374b52a438332d3b7a55165e3cab61e19fd379c","harnessId":"codex","mode":"human_controlled","observationDigest":"ab16673d3e389344fc1cc60545b0b085199575dc6c6656ab78836a572b36606e","recipeDigest":"55314db48e23e299d18e2c39346c9f2e0935a45f71aab05b912bc5d3df014a4e"},"nested":{"count":42},"ratio":1.25,"sessionId":"request_local"}`
	if string(produced) != want {
		t.Fatalf("produced bytes = %s", produced)
	}

	selection, err := executioncell.ExtractCapabilityRealizationSelectionV1(produced)
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil {
		t.Fatal("selection is absent")
	}
	declaration := compiled.Declaration
	observation := compiled.Observation
	if selection.CapabilityID != declaration.CapabilityID ||
		selection.HarnessID != string(declaration.HarnessID) ||
		selection.AdapterVersion != declaration.AdapterVersion ||
		selection.Mode != string(declaration.Mode) ||
		selection.RecipeDigest != declaration.Recipe.RecipeDigest ||
		selection.DeclaredSurfaceDigest != declaration.Recipe.DeclaredSurfaceDigest ||
		selection.ObservationDigest != observation.ObservationDigest {
		t.Fatalf("selection = %+v, registry row = %+v", selection, compiled)
	}

	pristine := bytes.Clone(produced)
	produced[0] = 'x'
	again, err := ComposeCapabilityRealizationSelectionOperationalPayload(raw, registry, target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, pristine) {
		t.Fatalf("returned payload aliases producer state\n got: %s\nwant: %s", again, pristine)
	}
}

func TestComposeCapabilityRealizationSelectionOperationalPayloadUsesImmutableRegistryRow(t *testing.T) {
	t.Parallel()
	registry, compiled, target := selectionProducerFixture(t)
	before, err := ComposeCapabilityRealizationSelectionOperationalPayload([]byte(`{"sessionId":"request_local"}`), registry, target)
	if err != nil {
		t.Fatal(err)
	}
	compiled.Declaration.CapabilityID = "example.mutated/v1"
	compiled.Declaration.Recipe.RecipeDigest = strings.Repeat("c", 64)
	compiled.Observation.ObservationDigest = strings.Repeat("d", 64)
	after, err := ComposeCapabilityRealizationSelectionOperationalPayload([]byte(`{"sessionId":"request_local"}`), registry, target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("registry row changed through caller mutation\n got: %s\nwant: %s", after, before)
	}
}

func TestComposeCapabilityRealizationSelectionOperationalPayloadRejectsNonExactTarget(t *testing.T) {
	t.Parallel()
	registry, _, exact := selectionProducerFixture(t)
	tests := map[string]struct {
		registry *agent.CapabilityRealizationRegistry
		target   CapabilityRealizationSelectionTarget
	}{
		"nil registry": {registry: nil, target: exact},
		"empty target": {registry: registry, target: CapabilityRealizationSelectionTarget{}},
		"unknown capability": {registry: registry, target: func() CapabilityRealizationSelectionTarget {
			value := exact
			value.CapabilityID = "example.unknown/v1"
			return value
		}()},
		"wrong case": {registry: registry, target: func() CapabilityRealizationSelectionTarget {
			value := exact
			value.HarnessID = agent.HarnessName(strings.ToUpper(string(value.HarnessID)))
			return value
		}()},
		"wrong adapter": {registry: registry, target: func() CapabilityRealizationSelectionTarget {
			value := exact
			value.AdapterVersion += "-other"
			return value
		}()},
		"wrong mode": {registry: registry, target: func() CapabilityRealizationSelectionTarget {
			value := exact
			value.Mode = agent.PromptModeAutonomous
			return value
		}()},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ComposeCapabilityRealizationSelectionOperationalPayload([]byte(`{"sessionId":"request_local"}`), test.registry, test.target); err == nil {
				t.Fatal("non-exact target produced a payload")
			}
		})
	}
}

func TestComposeCapabilityRealizationSelectionOperationalPayloadRejectsCallerSelection(t *testing.T) {
	t.Parallel()
	registry, compiled, target := selectionProducerFixture(t)
	validSelection, err := executioncell.CanonicalCapabilityRealizationSelectionV1(executioncell.CapabilityRealizationSelectionV1{
		ContractVersion: executioncell.CapabilityRealizationSelectionContractVersionV1,
		CapabilityID:    compiled.Declaration.CapabilityID, HarnessID: string(compiled.Declaration.HarnessID),
		AdapterVersion: compiled.Declaration.AdapterVersion, Mode: string(compiled.Declaration.Mode),
		RecipeDigest: compiled.Declaration.Recipe.RecipeDigest, DeclaredSurfaceDigest: compiled.Declaration.Recipe.DeclaredSurfaceDigest,
		ObservationDigest: compiled.Observation.ObservationDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"identical":         []byte(`{"capabilityRealizationSelection":` + string(validSelection) + `,"sessionId":"request_local"}`),
		"null":              []byte(`{"capabilityRealizationSelection":null,"sessionId":"request_local"}`),
		"malformed":         []byte(`{"capabilityRealizationSelection":{"unexpected":true},"sessionId":"request_local"}`),
		"escaped key":       []byte(`{"capabilityRealizationSelec\u0074ion":null,"sessionId":"request_local"}`),
		"escaped duplicate": []byte(`{"capabilityRealizationSelection":null,"capabilityRealizationSelec\u0074ion":null}`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ComposeCapabilityRealizationSelectionOperationalPayload(raw, registry, target); err == nil {
				t.Fatal("caller-authored selection was accepted")
			}
		})
	}
}

func TestComposeCapabilityRealizationSelectionOperationalPayloadRejectsInvalidOperationalPayload(t *testing.T) {
	t.Parallel()
	registry, _, target := selectionProducerFixture(t)
	tests := map[string][]byte{
		"not object":        []byte(`[]`),
		"duplicate member":  []byte(`{"sessionId":"one","sessionId":"two"}`),
		"admission sidecar": []byte(`{"sessionId":"one","admissionReceipt":null}`),
		"runtime sidecar":   []byte(`{"sessionId":"one","executionRuntimeBinding":null}`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ComposeCapabilityRealizationSelectionOperationalPayload(raw, registry, target); err == nil {
				t.Fatal("invalid operational payload was accepted")
			}
		})
	}
}

func TestCapabilityRealizationSelectionProducerRejectsCorruptRegistryRows(t *testing.T) {
	t.Parallel()
	_, compiled, _ := selectionProducerFixture(t)
	for name, mutate := range map[string]func(*agent.CompiledCapabilityRealization){
		"recipe digest": func(row *agent.CompiledCapabilityRealization) {
			row.Declaration.Recipe.RecipeDigest = strings.Repeat("c", 64)
		},
		"observation digest": func(row *agent.CompiledCapabilityRealization) {
			row.Observation.ObservationDigest = strings.Repeat("d", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			row := compiled
			mutate(&row)
			if _, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{row}); err == nil {
				t.Fatal("corrupt registry row was accepted")
			}
		})
	}
}

func TestComposeCapabilityRealizationSelectionOperationalPayloadProducesCanonicalDetachedBytes(t *testing.T) {
	t.Parallel()
	registry, _, target := selectionProducerFixture(t)
	produced, err := ComposeCapabilityRealizationSelectionOperationalPayload([]byte(`{"whole":7,"fraction":1.25}`), registry, target)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := executioncell.NormalizeOperationalPayload(produced)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(produced, canonical) {
		t.Fatalf("payload is not canonical\n got: %s\nwant: %s", produced, canonical)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(produced, &document); err != nil {
		t.Fatal(err)
	}
	if string(document["fraction"]) != "1.25" || string(document["whole"]) != "7" {
		t.Fatalf("ordinary numeric fields changed: %s", produced)
	}
}
