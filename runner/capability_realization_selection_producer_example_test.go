package runner_test

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runner"
)

func mustSelectionProducerExample[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func ExampleComposeCapabilityRealizationSelectionOperationalPayload() {
	surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceMCPServer, ID: "local-authoring"}}
	inputDigest := strings.Repeat("a", 64)
	declaration := mustSelectionProducerExample(agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
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
	}))
	observation := mustSelectionProducerExample(agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
		Declaration: declaration, FixtureID: "local-constructor-fixture", BinaryDigest: strings.Repeat("b", 64),
		AppliedArtifacts: []agent.CapabilityAppliedArtifact{{
			EntryID: "mcp-servers", Channel: agent.ToolChannelMCPServer, InputDigest: inputDigest,
		}},
		ObservedSurface: surface,
	}))
	compiled := mustSelectionProducerExample(agent.CompileCapabilityRealization(declaration, observation))
	registry := mustSelectionProducerExample(agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled}))
	target := runner.CapabilityRealizationSelectionTarget{
		CapabilityID: compiled.Declaration.CapabilityID, HarnessID: compiled.Declaration.HarnessID,
		AdapterVersion: compiled.Declaration.AdapterVersion, Mode: compiled.Declaration.Mode,
	}

	produced := mustSelectionProducerExample(runner.ComposeCapabilityRealizationSelectionOperationalPayload(
		[]byte(`{"sessionId":"request_local","mode":"interactive","repository":"example/repository"}`),
		registry,
		target,
	))
	selection := mustSelectionProducerExample(executioncell.ExtractCapabilityRealizationSelectionV1(produced))

	adapted := mustSelectionProducerExample(executioncell.AdaptQueuedWorkJSON(produced, executioncell.LegacyResolvedProfile{
		Harness: "codex", Provider: "local", Model: "local-model", ServingHost: "local-endpoint", AuthMode: "local-none",
	}, executioncell.LegacyAdapterContext{
		HarnessRefsByLegacyID:  map[string]executioncell.HarnessRef{"codex": {ID: string(agent.HarnessCodex), Version: target.AdapterVersion}},
		ModelAuthorsByProvider: map[string]string{"local": "local-author"},
		EndpointsByServingHost: map[string]executioncell.ServingEndpointRef{"local-endpoint": {
			ID: "local-endpoint", Protocol: "local", Operator: "local-operator", Revision: "1",
		}},
		AuthBindingsByMode: map[string]executioncell.AuthBindingRef{"local-none": {
			ID: "local-none", Mechanism: executioncell.AuthNone, CommercialMode: executioncell.CommercialFree,
			Authority: "local-operator", BindingScope: executioncell.ScopeProcess,
			Portability: executioncell.Portable, Delivery: executioncell.DeliveryNone,
		}},
		Placement:            executioncell.PlacementRef{ID: "local-host", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		RequiredCapabilities: []executioncell.CapabilityRequest{{Name: target.CapabilityID}},
	}))
	intentDigest := mustSelectionProducerExample(executioncell.DigestContractValue(adapted.Intent))
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion:        executioncell.ContractVersion,
		Harness:                *adapted.Intent.Harness,
		Model:                  adapted.Intent.Model,
		Endpoint:               *adapted.Intent.Endpoint,
		AuthBinding:            *adapted.Intent.AuthBinding,
		Placement:              *adapted.Intent.Placement,
		SessionMode:            adapted.Intent.SessionMode,
		GrantedCapabilities:    []executioncell.CapabilityRequirement{{Name: target.CapabilityID}},
		EvidenceTier:           executioncell.EvidenceUnitVerified,
		CompatibilityDigest:    strings.Repeat("c", 64),
		RuntimeInventoryDigest: strings.Repeat("d", 64),
	}
	cellBytes := mustSelectionProducerExample(executioncell.CanonicalJSON(cell))
	_, cellErr := executioncell.DecodeResolvedExecutionCell(cellBytes)

	// Trusted local composition chooses this registry, target, and admission
	// decision. The producer and decoders validate immutable data; they do not
	// grant authority by themselves.
	receiptBytes := mustSelectionProducerExample(json.Marshal(executioncell.AdmissionReceipt{
		ContractVersion:          executioncell.ContractVersion,
		ReceiptID:                "admission_local",
		RequestID:                adapted.Intent.RequestID,
		Decision:                 executioncell.AdmissionAdmitted,
		IntentDigest:             intentDigest,
		OperationalPayloadDigest: adapted.OperationalPayloadDigest,
		Cell:                     &cell,
		ResolverDecisions:        adapted.ResolverDecisions,
		RecordedAt:               "2026-01-01T00:00:00Z",
	}))
	immutableReceipt, receiptErr := executioncell.DecodeAdmissionReceipt(receiptBytes)
	bindingBytes := mustSelectionProducerExample(json.Marshal(executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion,
		RequestID:       adapted.Intent.RequestID,
		WorkerID:        "local-worker",
		PlacementID:     cell.Placement.ID,
	}))
	_, bindingErr := executioncell.DecodeRuntimeBinding(bindingBytes)
	payloadDigest := mustSelectionProducerExample(executioncell.DigestOperationalPayload(produced))

	fmt.Println(selection.AdapterVersion)
	fmt.Println(cellErr == nil, receiptErr == nil && len(immutableReceipt.Bytes()) > 0, bindingErr == nil && payloadDigest == adapted.OperationalPayloadDigest)
	// Output:
	// codex/local/authoring-v1
	// true true true
}
