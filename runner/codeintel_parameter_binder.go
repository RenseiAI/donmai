package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/codeintelbridge"
)

var windowsPathPrefix = regexp.MustCompile(`^[A-Za-z]:`)

type codeIntelParameterBinder struct{ sourceDigest string }

func NewCodeIntelParameterBinder(sourceDigest string) (CapabilityParameterBinder, error) {
	if !validLowerHexDigest(sourceDigest) {
		return nil, fmt.Errorf("code-intelligence binder source digest is malformed")
	}
	return &codeIntelParameterBinder{sourceDigest: sourceDigest}, nil
}

func (b *codeIntelParameterBinder) ContractID() string   { return codeintelbridge.ParameterContractID }
func (b *codeIntelParameterBinder) SourceDigest() string { return b.sourceDigest }

type rawCodeIntelParameters struct {
	Repo     string
	Ref      string
	RepoPath string
	Tools    []string
	toolsSet bool
}

func decodeCodeIntelParameters(payload json.RawMessage) (rawCodeIntelParameters, error) {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(payload, &outer); err != nil {
		return rawCodeIntelParameters{}, fmt.Errorf("decode operational payload")
	}
	raw, ok := outer["codeIntel"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return rawCodeIntelParameters{}, fmt.Errorf("code-intelligence parameters are missing")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return rawCodeIntelParameters{}, fmt.Errorf("decode code-intelligence parameters")
	}
	for name := range members {
		switch name {
		case "repo", "ref", "repoPath", "tools":
		default:
			return rawCodeIntelParameters{}, fmt.Errorf("code-intelligence parameters contain an unknown field")
		}
	}
	var out rawCodeIntelParameters
	for name, target := range map[string]*string{"repo": &out.Repo, "ref": &out.Ref, "repoPath": &out.RepoPath} {
		if value, present := members[name]; present {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, target) != nil || !utf8.ValidString(*target) {
				return rawCodeIntelParameters{}, fmt.Errorf("code-intelligence parameter %q is malformed", name)
			}
		}
	}
	if rawTools, present := members["tools"]; present {
		out.toolsSet = true
		if bytes.Equal(bytes.TrimSpace(rawTools), []byte("null")) || json.Unmarshal(rawTools, &out.Tools) != nil {
			return rawCodeIntelParameters{}, fmt.Errorf("code-intelligence tools are malformed")
		}
	}
	return out, nil
}

func normalizeCodeIntelRepoPath(value string) (string, error) {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || strings.Contains(value, `\`) || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || windowsPathPrefix.MatchString(value) {
		return "", fmt.Errorf("code-intelligence repo path is malformed")
	}
	parts := strings.Split(value, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("code-intelligence repo path escapes root")
		default:
			out = append(out, part)
		}
	}
	return strings.Join(out, "/"), nil
}

func selectedCodeIntelSurface(declaration agent.CapabilityRealizationDeclaration, requested rawCodeIntelParameters) ([]agent.CapabilitySurfaceIdentity, []string, error) {
	declaredTools := map[string]bool{}
	for _, identity := range declaration.Recipe.DeclaredSurface {
		if identity.Kind == agent.CapabilitySurfaceMCPTool || identity.Kind == agent.CapabilitySurfaceNativeTool {
			declaredTools[identity.ID] = true
		}
	}
	if len(declaredTools) == 0 {
		return nil, nil, fmt.Errorf("code-intelligence realization declares no tool surface")
	}
	selectedNames := make([]string, 0, len(declaredTools))
	if !requested.toolsSet || len(requested.Tools) == 0 {
		for name := range declaredTools {
			selectedNames = append(selectedNames, name)
		}
	} else {
		seen := map[string]bool{}
		for _, name := range requested.Tools {
			if name == "" || strings.TrimSpace(name) != name || seen[name] || !declaredTools[name] {
				return nil, nil, fmt.Errorf("code-intelligence tool selection is malformed")
			}
			seen[name] = true
			selectedNames = append(selectedNames, name)
		}
	}
	sort.Strings(selectedNames)
	selectedSet := map[string]bool{}
	for _, name := range selectedNames {
		selectedSet[name] = true
	}
	surface := make([]agent.CapabilitySurfaceIdentity, 0, len(declaration.Recipe.DeclaredSurface))
	for _, identity := range declaration.Recipe.DeclaredSurface {
		if identity.Kind != agent.CapabilitySurfaceMCPTool && identity.Kind != agent.CapabilitySurfaceNativeTool || selectedSet[identity.ID] {
			surface = append(surface, identity)
		}
	}
	return surface, selectedNames, nil
}

func (b *codeIntelParameterBinder) Bind(context CapabilityParameterBindingContext) (CapabilityParameterBindResult, error) {
	params, err := decodeCodeIntelParameters(context.OperationalPayload)
	if err != nil {
		return CapabilityParameterBindResult{}, err
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(context.OperationalPayload, &outer); err != nil {
		return CapabilityParameterBindResult{}, fmt.Errorf("decode operational payload")
	}
	parameterDigest, err := executioncell.DigestCapabilityParameters(outer["codeIntel"])
	if err != nil || parameterDigest != context.Requirement.ParametersDigest {
		return CapabilityParameterBindResult{}, fmt.Errorf("code-intelligence parameter digest mismatch")
	}
	repoPath, err := normalizeCodeIntelRepoPath(params.RepoPath)
	if err != nil {
		return CapabilityParameterBindResult{}, err
	}
	selected, tools, err := selectedCodeIntelSurface(context.Realization.Declaration, params)
	if err != nil {
		return CapabilityParameterBindResult{}, err
	}
	config, configDigest, err := codeintelbridge.CanonicalRuntimeConfig(codeintelbridge.RuntimeConfigV1{ContractVersion: codeintelbridge.RuntimeConfigVersion, RepoPath: repoPath, Tools: tools})
	if err != nil {
		return CapabilityParameterBindResult{}, err
	}
	selectedDigest, err := agent.CapabilitySelectedSurfaceDigest(selected)
	if err != nil {
		return CapabilityParameterBindResult{}, err
	}
	entries := make([]agent.CapabilityBoundEntryV1, len(context.Realization.Declaration.Recipe.Entries))
	for i, recipeEntry := range context.Realization.Declaration.Recipe.Entries {
		digest, err := agent.CapabilityEntryParametersDigest(recipeEntry.EntryID, recipeEntry.InputDigest, context.Requirement.ParametersDigest, selectedDigest, context.Requirement.OperationalPayloadDigest)
		if err != nil {
			return CapabilityParameterBindResult{}, err
		}
		entries[i] = agent.CapabilityBoundEntryV1{EntryID: recipeEntry.EntryID, StaticInputDigest: recipeEntry.InputDigest, EntryParametersDigest: digest}
	}
	binding := agent.CapabilityParameterBindingV1{ContractVersion: agent.CapabilityParameterBindingVersionV1, CapabilityID: context.Requirement.CapabilityID, ParameterContractID: codeintelbridge.ParameterContractID, ParametersDigest: context.Requirement.ParametersDigest, OperationalPayloadDigest: context.Requirement.OperationalPayloadDigest, StaticRecipeDigest: context.Realization.Declaration.Recipe.RecipeDigest, SelectedSurface: selected, SelectedSurfaceDigest: selectedDigest, RuntimeConfigDigest: configDigest, Entries: entries}
	binding.BindingDigest, err = agent.CapabilityParameterBindingDigest(binding)
	if err != nil {
		return CapabilityParameterBindResult{}, err
	}
	materialization := agent.CapabilityRuntimeMaterializationV1{ContractVersion: agent.CapabilityRuntimeMaterializationContractVersionV1, CapabilityID: binding.CapabilityID, ParameterContractID: binding.ParameterContractID, BindingDigest: binding.BindingDigest, ConfigDigest: configDigest, Config: config}
	result := CapabilityParameterBindResult{Binding: binding, Materialization: materialization}
	for _, entry := range context.Realization.Declaration.Recipe.Entries {
		if entry.Channel == agent.ToolChannelToolPlugin && strings.HasPrefix(entry.EntryID, agent.AdditionalExtensionCapabilityEntryPrefix) {
			result.AdditionalExtensions = append(result.AdditionalExtensions, codeintelbridge.Delivery())
		}
	}
	return result, nil
}
