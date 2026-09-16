package runner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/codeintelbridge"
	"github.com/RenseiAI/donmai/prompt"
)

type codeIntelDeliveryRoute uint8

const (
	codeIntelDeliveryLegacy codeIntelDeliveryRoute = iota
	codeIntelDeliveryMCP
	codeIntelDeliveryNative
)

type codeIntelDeliverySelection struct {
	Route codeIntelDeliveryRoute
	Tools []string
}

func resolveCodeIntelDeliveryRoute(codeIntel *prompt.CodeIntelWork, resolved []ResolvedCapabilityParameterBinding) (codeIntelDeliverySelection, error) {
	route := codeIntelDeliveryLegacy
	found := false
	var selectedTools []string
	for _, item := range resolved {
		binding := item.Realization
		if binding.ParameterContract == nil || binding.ParameterContract.ID != codeintelbridge.ParameterContractID {
			continue
		}
		if codeIntel == nil || binding.ParameterBinding == nil {
			return codeIntelDeliverySelection{}, fmt.Errorf("runner: code-intelligence realization has no admitted parameters")
		}
		candidate := codeIntelDeliveryRoute(0)
		for _, entry := range binding.Entries {
			switch {
			case entry.Channel == agent.ToolChannelMCPServer:
				if candidate != 0 && candidate != codeIntelDeliveryMCP {
					return codeIntelDeliverySelection{}, fmt.Errorf("runner: code-intelligence realization mixes delivery channels")
				}
				candidate = codeIntelDeliveryMCP
			case entry.Channel == agent.ToolChannelToolPlugin && strings.HasPrefix(entry.EntryID, agent.AdditionalExtensionCapabilityEntryPrefix):
				if candidate != 0 && candidate != codeIntelDeliveryNative {
					return codeIntelDeliverySelection{}, fmt.Errorf("runner: code-intelligence realization mixes delivery channels")
				}
				candidate = codeIntelDeliveryNative
			}
		}
		if candidate == 0 {
			return codeIntelDeliverySelection{}, fmt.Errorf("runner: code-intelligence realization has no delivery channel")
		}
		if found && route != candidate {
			return codeIntelDeliverySelection{}, fmt.Errorf("runner: code-intelligence realizations conflict on delivery")
		}
		config, _, err := codeintelbridge.DecodeRuntimeConfig(item.Materialization.Config)
		if err != nil {
			return codeIntelDeliverySelection{}, err
		}
		selected := make([]string, 0)
		for _, identity := range binding.ParameterBinding.SelectedSurface {
			if identity.Kind == agent.CapabilitySurfaceMCPTool || identity.Kind == agent.CapabilitySurfaceNativeTool {
				selected = append(selected, identity.ID)
			}
		}
		sort.Strings(selected)
		if strings.Join(selected, "\x00") != strings.Join(config.Tools, "\x00") {
			return codeIntelDeliverySelection{}, fmt.Errorf("runner: code-intelligence selected surface and runtime config differ")
		}
		if found && strings.Join(selectedTools, "\x00") != strings.Join(config.Tools, "\x00") {
			return codeIntelDeliverySelection{}, fmt.Errorf("runner: code-intelligence realizations conflict on selected surface")
		}
		selectedTools = append([]string(nil), config.Tools...)
		route, found = candidate, true
	}
	return codeIntelDeliverySelection{Route: route, Tools: selectedTools}, nil
}

func codeIntelRouteFromSpec(codeIntel *prompt.CodeIntelWork, spec agent.Spec) (codeIntelDeliverySelection, error) {
	resolved := make([]ResolvedCapabilityParameterBinding, 0)
	if spec.ToolLifecyclePlan == nil {
		return codeIntelDeliverySelection{Route: codeIntelDeliveryLegacy}, nil
	}
	byKey := map[string]agent.CapabilityRuntimeMaterializationV1{}
	for _, materialization := range spec.CapabilityRuntimeMaterializations {
		byKey[materialization.CapabilityID+"\x00"+materialization.BindingDigest] = materialization
	}
	for _, binding := range spec.ToolLifecyclePlan.CapabilityRealizations {
		if binding.ParameterBinding == nil {
			continue
		}
		key := binding.CapabilityID + "\x00" + binding.ParameterBinding.BindingDigest
		resolved = append(resolved, ResolvedCapabilityParameterBinding{Realization: binding, Materialization: byKey[key]})
	}
	return resolveCodeIntelDeliveryRoute(codeIntel, resolved)
}
