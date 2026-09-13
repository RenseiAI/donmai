package matrix

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestRenderRefusesCallerAuthoredCapabilityWithoutEvidence(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	built.Capabilities = append(built.Capabilities, CapabilityEligibilityRow{
		CapabilityID: "forged.capability/v1", HarnessID: agent.HarnessPi,
		AdapterVersion: "pi/interactive/tool-lifecycle-v4", Mode: agent.PromptModeHumanControlled,
		EvidenceTier: agent.CapabilityEvidenceSmoked, EvidenceDigest: strings.Repeat("a", 64), ProductionEligible: true,
	})
	if _, err := built.Render(); err == nil {
		t.Fatal("Render accepted caller-authored production eligibility without evidence")
	}
}

func TestRenderDerivesIdenticalCapabilityArtifactsFromValidatedEvidence(t *testing.T) {
	built := evidencedBuild(t)
	artifacts, err := built.Render()
	if err != nil {
		t.Fatal(err)
	}
	var standalone struct {
		Realizations []CapabilityRealizationEvidenceRow `json:"realizations"`
		Capabilities []CapabilityEligibilityRow         `json:"capabilities"`
	}
	if err := json.Unmarshal(artifacts.Files[FileCapabilityRealizations], &standalone); err != nil {
		t.Fatal(err)
	}
	var matrixDocument CapabilityMatrix
	if err := json.Unmarshal(artifacts.Files[FileCapabilityMatrix], &matrixDocument); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(standalone.Realizations, matrixDocument.Realizations) ||
		!reflect.DeepEqual(standalone.Capabilities, matrixDocument.Capabilities) {
		t.Fatalf("capability artifacts disagree: standalone=%+v matrix=%+v", standalone, matrixDocument)
	}
	wantCapabilities := capabilityEligibilityRows(standalone.Realizations)
	if !reflect.DeepEqual(standalone.Capabilities, wantCapabilities) {
		t.Fatalf("serialized capabilities=%+v want derived=%+v", standalone.Capabilities, wantCapabilities)
	}
}

func TestRenderRefusesContradictoryOrInvalidCapabilityMirrors(t *testing.T) {
	tests := map[string]func(*Built){
		"standalone capability removed": func(b *Built) {
			b.Capabilities = []CapabilityEligibilityRow{}
		},
		"matrix capability removed": func(b *Built) {
			b.Matrix.Capabilities = []CapabilityEligibilityRow{}
		},
		"standalone realization removed": func(b *Built) {
			b.Realizations = []CapabilityRealizationEvidenceRow{}
		},
		"matrix realization removed": func(b *Built) {
			b.Matrix.Realizations = []CapabilityRealizationEvidenceRow{}
		},
		"derived eligibility changed": func(b *Built) {
			b.Capabilities[0].ProductionEligible = false
		},
		"capability tuple changed": func(b *Built) {
			b.Capabilities[0].AdapterVersion = "other/v1"
		},
		"matrix capability digest changed": func(b *Built) {
			b.Matrix.Capabilities = append([]CapabilityEligibilityRow(nil), b.Matrix.Capabilities...)
			b.Matrix.Capabilities[0].EvidenceDigest = strings.Repeat("d", 64)
		},
		"realization evidence changed": func(b *Built) {
			b.Realizations[0].Evidence.EvidenceDigest = strings.Repeat("e", 64)
		},
		"realization tuple changed": func(b *Built) {
			b.Realizations[0].Evidence.AdapterVersion = "other/v1"
		},
		"evidence absent": func(b *Built) {
			b.Realizations[0] = CapabilityRealizationEvidenceRow{}
		},
		"all mirrors replaced with other valid evidence": func(b *Built) {
			other := evidencedBuildFor(t, "other.workflow/v1")
			b.Realizations = other.Realizations
			b.Capabilities = other.Capabilities
			b.Matrix.Realizations = other.Matrix.Realizations
			b.Matrix.Capabilities = other.Matrix.Capabilities
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			built := evidencedBuild(t)
			mutate(built)
			if _, err := built.Render(); err == nil {
				t.Fatal("Render accepted contradictory or invalid capability state")
			}
		})
	}
}

func evidencedBuild(t *testing.T) *Built {
	return evidencedBuildFor(t, "example.workflow/v1")
}

func evidencedBuildFor(t *testing.T, capability string) *Built {
	t.Helper()
	source := realizationEvidenceFixture(t, capability, agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	source.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		return sourceExecution(t, declaration), nil
	})
	built, err := BuildWithCapabilityRealizations(context.Background(), []CapabilityRealizationEvidenceSource{source})
	if err != nil {
		t.Fatal(err)
	}
	return built
}
