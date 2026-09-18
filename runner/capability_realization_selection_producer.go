package runner

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

// CapabilityRealizationSelectionTarget identifies one exact realization in a
// process-owned registry. The target is trusted local composition input; work
// payload JSON cannot select or replace it.
type CapabilityRealizationSelectionTarget struct {
	CapabilityID   string
	HarnessID      agent.HarnessName
	AdapterVersion string
	Mode           agent.PromptSessionMode
}

// ComposeCapabilityRealizationSelectionOperationalPayload adds the exact
// registry-derived realization selection to a pre-admission operational
// payload. It validates and canonicalizes data; it does not admit work or
// grant execution authority.
func ComposeCapabilityRealizationSelectionOperationalPayload(
	raw []byte,
	registry *agent.CapabilityRealizationRegistry,
	target CapabilityRealizationSelectionTarget,
) ([]byte, error) {
	canonical, err := executioncell.NormalizeOperationalPayload(raw)
	if err != nil {
		return nil, fmt.Errorf("runner: normalize capability realization selection payload: %w", err)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &document); err != nil {
		return nil, fmt.Errorf("runner: decode normalized capability realization selection payload: %w", err)
	}
	if _, exists := document["capabilityRealizationSelection"]; exists {
		return nil, errors.New("runner: operational payload already contains capabilityRealizationSelection")
	}

	if registry == nil {
		return nil, errors.New("runner: capability realization registry is required")
	}
	resolved, ok := registry.Resolve(target.CapabilityID, target.HarnessID, target.AdapterVersion, target.Mode)
	if !ok {
		return nil, errors.New("runner: capability realization selection target is not registered")
	}
	declaration := resolved.Declaration
	observation := resolved.Observation
	selection := executioncell.CapabilityRealizationSelectionV1{
		ContractVersion:       executioncell.CapabilityRealizationSelectionContractVersionV1,
		CapabilityID:          declaration.CapabilityID,
		HarnessID:             string(declaration.HarnessID),
		AdapterVersion:        declaration.AdapterVersion,
		Mode:                  string(declaration.Mode),
		RecipeDigest:          declaration.Recipe.RecipeDigest,
		DeclaredSurfaceDigest: declaration.Recipe.DeclaredSurfaceDigest,
		ObservationDigest:     observation.ObservationDigest,
	}
	selectionBytes, err := executioncell.CanonicalCapabilityRealizationSelectionV1(selection)
	if err != nil {
		return nil, fmt.Errorf("runner: validate registry-derived capability realization selection: %w", err)
	}
	document["capabilityRealizationSelection"] = json.RawMessage(selectionBytes)

	composed, err := executioncell.CanonicalJSON(document)
	if err != nil {
		return nil, fmt.Errorf("runner: canonicalize capability realization selection payload: %w", err)
	}
	return composed, nil
}
