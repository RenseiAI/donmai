package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/codeintelbridge"
	"github.com/RenseiAI/donmai/internal/codeintelcontract"
)

var windowsPathPrefix = regexp.MustCompile(`^[A-Za-z]:`)

type codeIntelParameterBinder struct{ sourceDigest string }

// NewCodeIntelParameterBinder constructs the strict process-owned binder.
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
	// encoding/json silently replaces invalid UTF-8 with U+FFFD, so validate
	// the raw member bytes before any decode can make the input lossy.
	if !utf8.Valid(raw) {
		return rawCodeIntelParameters{}, fmt.Errorf("code-intelligence parameters are not valid UTF-8")
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
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, target) != nil {
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
	// The executioncell digest refuses duplicate member names at any depth,
	// which Go's map decode below would otherwise resolve last-wins.
	if payloadDigest, err := executioncell.DigestOperationalPayload(context.OperationalPayload); err != nil || payloadDigest != context.Requirement.OperationalPayloadDigest {
		return CapabilityParameterBindResult{}, fmt.Errorf("code-intelligence operational payload digest mismatch")
	}
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

// nativeCodeIntelRecipeID names the inert native declaration shape only.
const nativeCodeIntelRecipeID = "donmai/code-intelligence-native/v1"

// NewNativeCodeIntelRealizationInput builds the authority-bound native recipe
// input for one capability, harness, adapter, and mode from the six native
// descriptors and the subset contract; it fixes no downstream requirement.
func NewNativeCodeIntelRealizationInput(capabilityID string, harness agent.HarnessName, adapterVersion string, mode agent.PromptSessionMode, binderSourceDigest string) (agent.CapabilityRealizationInput, error) {
	if !agent.ValidCapabilityParameterContractID(capabilityID) || harness == "" || adapterVersion == "" || mode == "" || !validLowerHexDigest(binderSourceDigest) {
		return agent.CapabilityRealizationInput{}, fmt.Errorf("code-intelligence native recipe identity is malformed")
	}
	delivery := codeintelbridge.Delivery()
	entryID, err := agent.AdditionalExtensionCapabilityEntryID(delivery.ID)
	if err != nil {
		return agent.CapabilityRealizationInput{}, err
	}
	descriptors := codeintelcontract.Descriptors()
	surface := make([]agent.CapabilitySurfaceIdentity, 0, len(descriptors))
	for _, descriptor := range descriptors {
		surface = append(surface, agent.CapabilitySurfaceIdentity{Kind: agent.CapabilitySurfaceNativeTool, ID: descriptor.Name})
	}
	inputDigest := agent.CapabilityExtensionInputDigest([]agent.ExtensionDelivery{delivery})
	contract := &agent.CapabilityParameterContractV1{ContractVersion: agent.CapabilityParameterContractVersionV1, ID: codeintelbridge.ParameterContractID, BinderSourceDigest: binderSourceDigest, SurfaceProjection: "subset", RuntimeObservation: "none"}
	return agent.CapabilityRealizationInput{CapabilityID: capabilityID, HarnessID: harness, AdapterVersion: adapterVersion, Mode: mode, RecipeID: nativeCodeIntelRecipeID, Entries: []agent.CapabilityRecipeEntry{{EntryID: entryID, Channel: agent.ToolChannelToolPlugin, Required: true, InputDigest: inputDigest, SurfaceRefs: surface}}, DeclaredSurface: surface, ParameterContract: contract}, nil
}

// CompileNativeCodeIntelFacts binds the runner-owned absolute root to the
// validated binding's canonical subtree and ordered tools, verifying contract,
// binding/materialization identity, and surface/config agreement.
func CompileNativeCodeIntelFacts(worktreeRoot string, resolved ResolvedCapabilityParameterBinding) (codeintelbridge.NativeFactsV1, string, error) {
	if !filepath.IsAbs(worktreeRoot) || filepath.Clean(worktreeRoot) != worktreeRoot {
		return codeintelbridge.NativeFactsV1{}, "", fmt.Errorf("code-intelligence native root is not a clean absolute path")
	}
	if resolved.Realization.ParameterContract == nil || resolved.Realization.ParameterContract.ID != codeintelbridge.ParameterContractID {
		return codeintelbridge.NativeFactsV1{}, "", fmt.Errorf("code-intelligence native contract is foreign")
	}
	binding := resolved.Realization.ParameterBinding
	if binding == nil {
		return codeintelbridge.NativeFactsV1{}, "", fmt.Errorf("code-intelligence native binding is missing")
	}
	materialization := resolved.Materialization
	if materialization.ContractVersion != agent.CapabilityRuntimeMaterializationContractVersionV1 || materialization.CapabilityID != binding.CapabilityID || materialization.ParameterContractID != binding.ParameterContractID || materialization.BindingDigest != binding.BindingDigest || materialization.ConfigDigest != binding.RuntimeConfigDigest || binding.CapabilityID != resolved.Realization.CapabilityID {
		return codeintelbridge.NativeFactsV1{}, "", fmt.Errorf("code-intelligence native binding identity differs")
	}
	config, configDigest, err := codeintelbridge.DecodeRuntimeConfig(materialization.Config)
	if err != nil || configDigest != binding.RuntimeConfigDigest || configDigest != materialization.ConfigDigest {
		return codeintelbridge.NativeFactsV1{}, "", fmt.Errorf("code-intelligence native config differs")
	}
	selectedDigest, err := agent.CapabilitySelectedSurfaceDigest(binding.SelectedSurface)
	if err != nil || selectedDigest != binding.SelectedSurfaceDigest {
		return codeintelbridge.NativeFactsV1{}, "", fmt.Errorf("code-intelligence native surface differs")
	}
	selected := make([]string, 0, len(binding.SelectedSurface))
	for _, identity := range binding.SelectedSurface {
		if identity.Kind == agent.CapabilitySurfaceMCPTool || identity.Kind == agent.CapabilitySurfaceNativeTool {
			selected = append(selected, identity.ID)
		}
	}
	sort.Strings(selected)
	if strings.Join(selected, "\x00") != strings.Join(config.Tools, "\x00") {
		return codeintelbridge.NativeFactsV1{}, "", fmt.Errorf("code-intelligence native surface and config differ")
	}
	for _, tool := range config.Tools {
		match, matchErr := codeintelcontract.NormalizePolicyIdentity(tool)
		if matchErr != nil || !match.Related || match.Canonical != tool {
			return codeintelbridge.NativeFactsV1{}, "", fmt.Errorf("code-intelligence native tool is not admitted")
		}
	}
	facts := codeintelbridge.NativeFactsV1{ContractVersion: codeintelbridge.NativeFactsVersion, Root: worktreeRoot, RepoPath: config.RepoPath, Tools: append([]string(nil), config.Tools...)}
	_, digest, err := codeintelbridge.CanonicalNativeFacts(facts)
	if err != nil {
		return codeintelbridge.NativeFactsV1{}, "", err
	}
	return facts, digest, nil
}
