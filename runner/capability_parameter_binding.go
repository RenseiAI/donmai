package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

// CapabilityParameterBindingContext contains only already-verified requirement
// facts, exact operational bytes, and immutable catalog authority.
type CapabilityParameterBindingContext struct {
	Requirement        agent.CapabilityParameterRequirementFacts
	OperationalPayload json.RawMessage
	Realization        agent.CompiledCapabilityRealization
}

// CapabilityParameterBinder is a process-owned implementation selected by the
// realization's trusted parameter contract, never by session wire data.
type CapabilityParameterBinder interface {
	ContractID() string
	SourceDigest() string
	Bind(CapabilityParameterBindingContext) (CapabilityParameterBindResult, error)
}

type CapabilityParameterBindResult struct {
	Binding              agent.CapabilityParameterBindingV1
	Materialization      agent.CapabilityRuntimeMaterializationV1
	AdditionalExtensions []agent.ExtensionDelivery
}

type ResolvedCapabilityParameterBinding struct {
	Realization          agent.CapabilityRealizationBinding
	Materialization      agent.CapabilityRuntimeMaterializationV1
	AdditionalExtensions []agent.ExtensionDelivery
}

type capabilityParameterBinderDescriptor struct {
	contractID   string
	sourceDigest string
	binder       CapabilityParameterBinder
}

// CapabilityParameterBinderRegistry is immutable after construction.
type CapabilityParameterBinderRegistry struct {
	mu      sync.RWMutex
	binders map[string]capabilityParameterBinderDescriptor
}

func validLowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NewCapabilityParameterBinderRegistry freezes binder descriptors and refuses
// empty, malformed, or duplicate contract identities.
func NewCapabilityParameterBinderRegistry(binders ...CapabilityParameterBinder) (*CapabilityParameterBinderRegistry, error) {
	registry := &CapabilityParameterBinderRegistry{binders: make(map[string]capabilityParameterBinderDescriptor, len(binders))}
	for _, binder := range binders {
		if binder == nil {
			return nil, fmt.Errorf("capability parameter binder is required")
		}
		contractID, sourceDigest := binder.ContractID(), binder.SourceDigest()
		if !agent.ValidCapabilityParameterContractID(contractID) || !validLowerHexDigest(sourceDigest) {
			return nil, fmt.Errorf("capability parameter binder descriptor is malformed")
		}
		if _, duplicate := registry.binders[contractID]; duplicate {
			return nil, fmt.Errorf("duplicate capability parameter binder")
		}
		registry.binders[contractID] = capabilityParameterBinderDescriptor{contractID: contractID, sourceDigest: sourceDigest, binder: binder}
	}
	return registry, nil
}

// ResolveAndBind is the sole runtime constructor that attaches a session
// parameter binding. Caller-authored decoded bindings never enter this path.
func (r *CapabilityParameterBinderRegistry) ResolveAndBind(
	realizations *agent.CapabilityRealizationRegistry,
	requirement agent.CapabilityParameterRequirementFacts,
	operationalPayload json.RawMessage,
	harness agent.HarnessName,
	adapterVersion string,
	mode agent.PromptSessionMode,
) (ResolvedCapabilityParameterBinding, error) {
	if realizations == nil {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("capability realization registry is required")
	}
	compiled, ok := realizations.Resolve(requirement.CapabilityID, harness, adapterVersion, mode)
	if !ok {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("capability has no exact realization")
	}
	if compiled.Declaration.ContractVersion == agent.CapabilityRealizationContractVersionV1 {
		return ResolvedCapabilityParameterBinding{Realization: agent.BindCapabilityRealization(compiled)}, nil
	}
	if compiled.Declaration.ContractVersion != agent.CapabilityRealizationContractVersionV2 || compiled.Declaration.ParameterContract == nil || r == nil {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("parameterized capability realization has no binder registry")
	}
	if requirement.CapabilityID == "" || !validLowerHexDigest(requirement.ParametersDigest) || !validLowerHexDigest(requirement.OperationalPayloadDigest) || len(bytes.TrimSpace(operationalPayload)) == 0 {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("capability parameter requirement facts are malformed")
	}
	payloadDigest, err := executioncell.DigestOperationalPayload(operationalPayload)
	if err != nil || payloadDigest != requirement.OperationalPayloadDigest {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("capability parameter operational payload digest mismatch")
	}
	r.mu.RLock()
	descriptor, found := r.binders[compiled.Declaration.ParameterContract.ID]
	r.mu.RUnlock()
	if !found || descriptor.sourceDigest != compiled.Declaration.ParameterContract.BinderSourceDigest ||
		descriptor.binder.ContractID() != descriptor.contractID || descriptor.binder.SourceDigest() != descriptor.sourceDigest {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("capability parameter binder does not match trusted catalog authority")
	}
	context := CapabilityParameterBindingContext{
		Requirement:        requirement,
		OperationalPayload: bytes.Clone(operationalPayload),
		Realization:        compiled,
	}
	bound, err := descriptor.binder.Bind(context)
	if err != nil {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("bind capability parameters: %w", err)
	}
	if err := agent.ValidateCapabilityParameterBinding(bound.Binding, compiled.Declaration, requirement); err != nil {
		return ResolvedCapabilityParameterBinding{}, err
	}
	canonicalConfig, configDigest, err := agent.CanonicalCapabilityRuntimeConfig(bound.Materialization.Config)
	if err != nil || string(canonicalConfig) != string(bound.Materialization.Config) || configDigest != bound.Materialization.ConfigDigest ||
		bound.Materialization.ContractVersion != agent.CapabilityRuntimeMaterializationContractVersionV1 ||
		bound.Materialization.CapabilityID != bound.Binding.CapabilityID || bound.Materialization.ParameterContractID != bound.Binding.ParameterContractID ||
		bound.Materialization.BindingDigest != bound.Binding.BindingDigest || bound.Materialization.ConfigDigest != bound.Binding.RuntimeConfigDigest {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("capability runtime materialization is invalid")
	}
	namedEntries := map[string]agent.CapabilityRecipeEntry{}
	for _, entry := range compiled.Declaration.Recipe.Entries {
		if entry.Channel == agent.ToolChannelToolPlugin && strings.HasPrefix(entry.EntryID, agent.AdditionalExtensionCapabilityEntryPrefix) {
			namedEntries[entry.EntryID] = entry
		}
	}
	seenDeliveries := map[string]bool{}
	for _, delivery := range bound.AdditionalExtensions {
		if err := agent.ValidateExtensionDelivery(delivery); err != nil || seenDeliveries[delivery.ID] {
			return ResolvedCapabilityParameterBinding{}, fmt.Errorf("capability runtime extension delivery is invalid")
		}
		seenDeliveries[delivery.ID] = true
		entryID, err := agent.AdditionalExtensionCapabilityEntryID(delivery.ID)
		entry, ok := namedEntries[entryID]
		if err != nil || !ok || entry.Required != delivery.Required || entry.InputDigest != agent.CapabilityExtensionInputDigest([]agent.ExtensionDelivery{delivery}) {
			return ResolvedCapabilityParameterBinding{}, fmt.Errorf("capability runtime extension delivery does not match recipe")
		}
		delete(namedEntries, entryID)
	}
	if len(namedEntries) != 0 {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("capability runtime extension delivery coverage is incomplete")
	}
	parameterBinding := bound.Binding
	parameterBinding.SelectedSurface = append([]agent.CapabilitySurfaceIdentity(nil), parameterBinding.SelectedSurface...)
	parameterBinding.Entries = append([]agent.CapabilityBoundEntryV1(nil), parameterBinding.Entries...)
	binding := agent.BindCapabilityRealization(compiled)
	binding.ParameterBinding = &parameterBinding
	result := ResolvedCapabilityParameterBinding{Realization: binding, Materialization: bound.Materialization}
	result.Materialization.Config = append(json.RawMessage(nil), canonicalConfig...)
	result.AdditionalExtensions = cloneExtensionDeliveries(bound.AdditionalExtensions)
	return result, nil
}

func cloneExtensionDeliveries(in []agent.ExtensionDelivery) []agent.ExtensionDelivery {
	out := append([]agent.ExtensionDelivery(nil), in...)
	for i := range out {
		out[i].Source = append([]byte(nil), in[i].Source...)
	}
	return out
}

type capabilityRealizationResolver interface {
	Knows(string) bool
	Resolve(string, agent.HarnessName, string, agent.PromptSessionMode) (agent.CompiledCapabilityRealization, bool)
}

type parameterBoundCapabilityResolver struct {
	realizations *agent.CapabilityRealizationRegistry
	binders      *CapabilityParameterBinderRegistry
}

func (r *parameterBoundCapabilityResolver) Knows(capability string) bool {
	return r != nil && r.realizations != nil && r.realizations.Knows(capability)
}

func (r *parameterBoundCapabilityResolver) Resolve(capability string, harness agent.HarnessName, adapter string, mode agent.PromptSessionMode) (agent.CompiledCapabilityRealization, bool) {
	if r == nil || r.realizations == nil {
		return agent.CompiledCapabilityRealization{}, false
	}
	return r.realizations.Resolve(capability, harness, adapter, mode)
}

func (r *parameterBoundCapabilityResolver) resolveAndBind(requirement agent.CapabilityParameterRequirementFacts, payload json.RawMessage, harness agent.HarnessName, adapter string, mode agent.PromptSessionMode) (ResolvedCapabilityParameterBinding, error) {
	if r == nil || r.binders == nil {
		return ResolvedCapabilityParameterBinding{}, fmt.Errorf("parameterized capability realization has no binder registry")
	}
	return r.binders.ResolveAndBind(r.realizations, requirement, payload, harness, adapter, mode)
}

func newPreparedCapabilityResolver(realizations *agent.CapabilityRealizationRegistry, binders *CapabilityParameterBinderRegistry) (capabilityRealizationResolver, error) {
	if binders == nil {
		return realizations, nil
	}
	if realizations == nil {
		return nil, fmt.Errorf("capability parameter binders require a realization registry")
	}
	return &parameterBoundCapabilityResolver{realizations: realizations, binders: binders}, nil
}
