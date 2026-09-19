package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// runtimeMaterializedCredential is the placeholder the prepared-source lane
// substitutes for every runtime credential. The lane computes the shape of the
// session's MCP set without holding real credentials, so the placeholder only
// has to be non-empty — its value never reaches a request.
const runtimeMaterializedCredential = "runtime-materialized"

// materializeRuntimeAuthority returns a copy of qw with placeholder runtime
// credentials, so the prepared-source lane derives the same implicit MCP server
// set the spawn lane will.
//
// BOTH bearers are materialized. The gateway's bearer is mcpGatewayBearer(qw),
// which prefers the session-scoped token, so materializing only the worker one
// would let the two lanes disagree the moment the platform starts stamping a
// session-scoped bearer. Only the server NAMES are carried out of this lane
// today, but keeping the two inputs symmetric costs one line and removes a
// future divergence.
func materializeRuntimeAuthority(qw QueuedWork) QueuedWork {
	qw.PlatformURL = "https://runtime.invalid"
	qw.AuthToken = runtimeMaterializedCredential
	qw.McpAuthToken = runtimeMaterializedCredential
	return qw
}

// decorate is the embedder's additional-extension decorator
// (afcli.Config.AgentSpecExtensionDecorator), applied via
// ReconcileAdditionalExtensions before this function returns spec — see that
// function's doc comment for why every caller that persists or compares a
// ToolLifecycleReceipt against this Spec must supply the SAME decorate value
// the real spawn's decorated Provider will apply. nil is a legitimate value:
// a session with no registered decorator never had this mutation to
// reconcile.
func buildPreparedSourceSpec(qw QueuedWork, selection harnessSelection, decorate agent.ExtensionDecorator, registries ...capabilityRealizationResolver) (agent.Spec, []string, error) {
	return buildPreparedSourceSpecWithPlatformMCPServerName(qw, selection, decorate, platformMCPServerName(), registries...)
}

func buildPreparedSourceSpecWithPlatformMCPServerName(qw QueuedWork, selection harnessSelection, decorate agent.ExtensionDecorator, platformMCPServerName string, registries ...capabilityRealizationResolver) (agent.Spec, []string, error) {
	provider := selection.Provider
	if provider == nil {
		return agent.Spec{}, nil, errors.New("runner: prepared source requires exact provider")
	}
	working := qw
	if working.isInterview() {
		working.SystemPromptOverride = buildInterviewSystemPrompt(working.SystemPromptOverride, interview.InterviewCompleteSentinel)
	}
	mode := sessionPromptMode(working, selection.effectiveCell)
	harness, ok := provider.(agent.HarnessProvider)
	if !ok {
		return agent.Spec{}, nil, errors.New("runner: selected provider has no exact harness manifest")
	}
	manifest := harness.Manifest()
	profile, ok := toolLifecycleProfileForWork(working, manifest, mode)
	if !ok {
		return agent.Spec{}, nil, errors.New("runner: selected provider has no exact tool lifecycle profile")
	}
	var realizations capabilityRealizationResolver
	if len(registries) > 0 {
		realizations = registries[0]
	}
	resolvedCapabilities, err := resolvePreparedCapabilities(working, selection, realizations, manifest.Name, profile.ID, mode)
	if err != nil {
		return agent.Spec{}, nil, err
	}
	codeIntelDelivery, err := resolveCodeIntelDeliveryRoute(working.CodeIntel, resolvedCapabilities)
	if err != nil {
		return agent.Spec{}, nil, err
	}
	builder := prompt.NewBuilder()
	inlineAppend, inlineDisallow, _ := foldInlineSkills("", working.Skills)
	builder.SkillAppend = inlineAppend
	composition, err := builder.BuildComposition(working.QueuedWork)
	if err != nil {
		return agent.Spec{}, nil, err
	}
	composition.HarnessProtocol = injectCodeIntelPartialForDelivery(composition.HarnessProtocol, provider.Capabilities(), working.CodeIntel, codeIntelDelivery)
	composition.HarnessProtocol = injectWorkareaProtocolPartial(composition.HarnessProtocol, working.RepositoryDeclaration != nil)
	userPrompt := composition.UserPrompt
	if working.isInteractive() {
		userPrompt = working.InitialPrompt
	}
	promptPlan := &agent.PromptPlan{
		ContractVersion:  agent.PromptContractVersion,
		BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
		UserPrompt:       agent.PromptContent{ID: "runner-user-task", Text: userPrompt, Required: userPrompt != ""},
	}
	if provider.Name() != agent.ProviderShell {
		promptPlan.HarnessProtocol = &agent.PromptContent{ID: "runner-harness-protocol", Text: composition.HarnessProtocol, Required: true}
		promptPlan.AuthorizedDowngrades = []agent.PromptDowngradeAuthorization{
			{ID: "runner-authorizes-protocol-to-user", Channel: agent.PromptChannelHarnessProtocol, To: agent.PromptChannelUserPrompt},
			{ID: "runner-authorizes-role-to-user", Channel: agent.PromptChannelRoleIntent, To: agent.PromptChannelUserPrompt},
			{ID: "runner-authorizes-context-to-user", Channel: agent.PromptChannelInitialContext, To: agent.PromptChannelUserPrompt},
		}
		if composition.RoleIntent != "" {
			promptPlan.RoleIntent = &agent.PromptContent{ID: "agent-card-role-intent", Text: composition.RoleIntent, Required: true}
		}
		if composition.InitialContext != "" {
			promptPlan.InitialContext = []agent.PromptContent{{ID: "agent-memory-context", Text: composition.InitialContext, Required: true}}
		}
	}
	defaults := defaultMCPServersForHarnessWithPlatformMCPServerName(materializeRuntimeAuthority(working), "/runtime/worktree", provider, mode, platformMCPServerName, codeIntelDelivery.Route)
	runtimeNames := make([]string, 0, len(defaults))
	for _, server := range defaults {
		runtimeNames = append(runtimeNames, server.Name)
	}
	mcpServers := mergeMCPServers(defaults, working.McpServers)
	spec, err := translateSpecForCodeIntelDelivery(working, provider.Capabilities(), SpecInputs{
		Prompt: userPrompt, SystemPromptAppend: composition.SystemPrompt(), PromptPlan: promptPlan,
		InitialContext: composition.InitialContext, MCPServers: mcpServers, Env: maps.Clone(working.Env),
		Autonomous: mode == agent.PromptModeAutonomous, ProviderName: string(provider.Name()),
	}, codeIntelDelivery, inlineDisallow)
	if err != nil {
		return agent.Spec{}, nil, err
	}
	spec.PromptMode = mode
	if working.toolLifecycleProfileID != "" {
		spec = agent.WithToolLifecycleProfile(spec, working.toolLifecycleProfileID)
	}
	if working.isInteractive() {
		spec.Interactive = &agent.InteractiveSpec{}
	}
	var admissionRegistry *agent.CapabilityRealizationRegistry
	switch registry := realizations.(type) {
	case *agent.CapabilityRealizationRegistry:
		admissionRegistry = registry
	case *parameterBoundCapabilityResolver:
		admissionRegistry = registry.realizations
	}
	spec, err = bindAdmissionToolLifecyclePlan(spec, selection.receipt, selection.claimReceipt, admissionRegistry)
	if err != nil {
		return agent.Spec{}, nil, err
	}
	for _, resolved := range resolvedCapabilities {
		spec.ToolLifecyclePlan.CapabilityRealizations = append(spec.ToolLifecyclePlan.CapabilityRealizations, resolved.Realization)
		if resolved.Realization.ContractVersion == agent.CapabilityRealizationContractVersionV2 {
			spec.CapabilityRuntimeMaterializations = append(spec.CapabilityRuntimeMaterializations, cloneCapabilityRuntimeMaterialization(resolved.Materialization))
			spec.AdditionalExtensions = append(spec.AdditionalExtensions, cloneExtensionDeliveries(resolved.AdditionalExtensions)...)
		}
	}
	sort.Slice(spec.AdditionalExtensions, func(i, j int) bool { return spec.AdditionalExtensions[i].ID < spec.AdditionalExtensions[j].ID })
	spec = ReconcileAdditionalExtensions(spec, decorate)
	if err := validateCapabilityRuntimeMaterializations(spec); err != nil {
		return agent.Spec{}, nil, err
	}
	return spec, runtimeNames, nil
}

func resolvePreparedCapabilities(working QueuedWork, selection harnessSelection, realizations capabilityRealizationResolver, harness agent.HarnessName, profileID string, mode agent.PromptSessionMode) ([]ResolvedCapabilityParameterBinding, error) {
	if realizations == nil || len(selection.receipt.Bytes()) == 0 || selection.receipt.Value().Cell == nil {
		return nil, nil
	}
	operationalPayload, err := CanonicalOperationalPayload(working)
	if err != nil {
		return nil, err
	}
	out := make([]ResolvedCapabilityParameterBinding, 0)
	for _, capability := range selection.receipt.Value().Cell.GrantedCapabilities {
		if !realizations.Knows(capability.Name) {
			continue
		}
		compiled, found := realizations.Resolve(capability.Name, harness, profileID, mode)
		if !found {
			return nil, fmt.Errorf("runner: capability %q has no production-eligible exact realization", capability.Name)
		}
		if compiled.Declaration.ContractVersion == agent.CapabilityRealizationContractVersionV1 {
			out = append(out, ResolvedCapabilityParameterBinding{Realization: agent.BindCapabilityRealization(compiled)})
			continue
		}
		parameterResolver, ok := realizations.(*parameterBoundCapabilityResolver)
		if !ok {
			return nil, fmt.Errorf("runner: parameterized capability %q has no process-owned binder", capability.Name)
		}
		facts := agent.CapabilityParameterRequirementFacts{CapabilityID: capability.Name, ParametersDigest: capability.ParametersDigest, OperationalPayloadDigest: selection.receipt.Value().OperationalPayloadDigest}
		resolved, err := parameterResolver.resolveAndBind(facts, operationalPayload, harness, profileID, mode)
		if err != nil {
			return nil, fmt.Errorf("runner: bind parameterized capability %q: %w", capability.Name, err)
		}
		out = append(out, resolved)
	}
	return out, nil
}

func cloneCapabilityRuntimeMaterialization(in agent.CapabilityRuntimeMaterializationV1) agent.CapabilityRuntimeMaterializationV1 {
	out := in
	out.Config = append(json.RawMessage(nil), in.Config...)
	return out
}

func validateCapabilityRuntimeMaterializations(spec agent.Spec) error {
	bindings := map[string]agent.CapabilityRealizationBinding{}
	if spec.ToolLifecyclePlan != nil {
		for _, binding := range spec.ToolLifecyclePlan.CapabilityRealizations {
			if binding.ContractVersion == agent.CapabilityRealizationContractVersionV2 && binding.ParameterBinding != nil {
				bindings[binding.CapabilityID+"\x00"+binding.ParameterBinding.BindingDigest] = binding
			}
		}
	}
	seenCapabilities := map[string]bool{}
	seenBindings := map[string]bool{}
	for _, materialization := range spec.CapabilityRuntimeMaterializations {
		key := materialization.CapabilityID + "\x00" + materialization.BindingDigest
		binding, ok := bindings[key]
		if !ok || seenCapabilities[materialization.CapabilityID] || seenBindings[materialization.BindingDigest] || binding.ParameterBinding == nil ||
			materialization.ContractVersion != agent.CapabilityRuntimeMaterializationContractVersionV1 || materialization.ParameterContractID != binding.ParameterBinding.ParameterContractID || materialization.ConfigDigest != binding.ParameterBinding.RuntimeConfigDigest {
			return fmt.Errorf("runner: capability runtime materialization does not match exact binding")
		}
		canonical, digest, err := agent.CanonicalCapabilityRuntimeConfig(materialization.Config)
		if err != nil || string(canonical) != string(materialization.Config) || digest != materialization.ConfigDigest {
			return fmt.Errorf("runner: capability runtime materialization config is invalid")
		}
		seenCapabilities[materialization.CapabilityID] = true
		seenBindings[materialization.BindingDigest] = true
		delete(bindings, key)
	}
	if len(bindings) != 0 {
		return fmt.Errorf("runner: capability runtime materialization coverage is incomplete")
	}
	return nil
}

// compilePreparedHarness compiles the receipt-bearing host authority for qw.
// repositoryDeclaration is the SAME normalized declaration the spawn lane
// resolves (runner.resolveRepositoryWorkarea) — the daemon preflight
// compiler resolves it independently (ProviderView.PreflightExecution) and
// forwards it here so ReconcileRepositorySandbox applies identically on both
// sides; see that function's doc comment for why a receipt-bearing session
// with a declared repository must never leave this reconciliation to only
// one of the two compile sites. Nil is a legitimate value: a repository-free
// or non-multi-repo-protocol session never had this mutation to reconcile.
// decorate is forwarded to buildPreparedSourceSpec — see its doc comment and
// ReconcileAdditionalExtensions for why the daemon's preflight compiler
// (ProviderView.PreflightExecution, the sole caller of this function) must
// supply the SAME embedder decorator the real spawn's Provider will apply.
func compilePreparedHarness(qw QueuedWork, selection harnessSelection, repositoryDeclaration *workarea.NormalizedDeclaration, decorate agent.ExtensionDecorator, registries ...capabilityRealizationResolver) (*agent.PreparedHarness, agent.Spec, error) {
	return compilePreparedHarnessWithPlatformMCPServerName(qw, selection, repositoryDeclaration, decorate, platformMCPServerName(), registries...)
}

func compilePreparedHarnessWithPlatformMCPServerName(qw QueuedWork, selection harnessSelection, repositoryDeclaration *workarea.NormalizedDeclaration, decorate agent.ExtensionDecorator, platformMCPServerName string, registries ...capabilityRealizationResolver) (*agent.PreparedHarness, agent.Spec, error) {
	spec, runtimeNames, err := buildPreparedSourceSpecWithPlatformMCPServerName(qw, selection, decorate, platformMCPServerName, registries...)
	if err != nil {
		return nil, agent.Spec{}, err
	}
	spec = ReconcileRepositorySandbox(spec, repositoryDeclaration)
	harness, ok := selection.Provider.(agent.HarnessProvider)
	if !ok {
		return nil, agent.Spec{}, errors.New("runner: selected provider has no exact harness manifest")
	}
	digest, err := DigestOperationalPayload(qw)
	if err != nil {
		return nil, agent.Spec{}, err
	}
	channels := []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"}
	materializations := make([]agent.HarnessMaterialization, 0, len(channels))
	for _, channel := range channels {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: digest, Required: true})
	}
	plan, compileErr := agent.CompilePreparedHarness(spec, harness.Manifest(), digest, runtimeNames, materializations)
	return plan, spec, compileErr
}

func applyPreparedSourceAuthority(target, source agent.Spec, plan *agent.PreparedHarness) agent.Spec {
	target.SessionName = source.SessionName
	target.Prompt = source.Prompt
	target.Autonomous = source.Autonomous
	target.SandboxEnabled, target.SandboxLevel = source.SandboxEnabled, source.SandboxLevel
	target.AllowedTools = append([]string(nil), source.AllowedTools...)
	target.DisallowedTools = append([]string(nil), source.DisallowedTools...)
	target.MCPToolNames = append([]string(nil), source.MCPToolNames...)
	target.Model, target.Effort = source.Model, source.Effort
	target.BaseInstructions, target.SystemPromptAppend, target.InitialContext = source.BaseInstructions, source.SystemPromptAppend, source.InitialContext
	target.PermissionConfig = source.PermissionConfig
	target.ProviderConfig = source.ProviderConfig
	target.PromptPlan = source.PromptPlan
	target.ToolLifecyclePlan = source.ToolLifecyclePlan
	target.AdditionalExtensions = boundRuntimeExtensionDeliveries(source)
	target.CapabilityRuntimeMaterializations = make([]agent.CapabilityRuntimeMaterializationV1, len(source.CapabilityRuntimeMaterializations))
	for i := range source.CapabilityRuntimeMaterializations {
		target.CapabilityRuntimeMaterializations[i] = cloneCapabilityRuntimeMaterialization(source.CapabilityRuntimeMaterializations[i])
	}
	target.PromptMode = source.PromptMode
	target.PreparedHarness = plan
	if source.Interactive != nil {
		if target.Interactive == nil {
			target.Interactive = &agent.InteractiveSpec{}
		}
	} else {
		target.Interactive = nil
	}
	return target
}

func boundRuntimeExtensionDeliveries(source agent.Spec) []agent.ExtensionDelivery {
	named := map[string]bool{}
	if source.ToolLifecyclePlan != nil {
		for _, binding := range source.ToolLifecyclePlan.CapabilityRealizations {
			if binding.ContractVersion != agent.CapabilityRealizationContractVersionV2 {
				continue
			}
			for _, entry := range binding.Entries {
				if entry.Channel == agent.ToolChannelToolPlugin && strings.HasPrefix(entry.EntryID, agent.AdditionalExtensionCapabilityEntryPrefix) {
					named[entry.EntryID] = true
				}
			}
		}
	}
	out := make([]agent.ExtensionDelivery, 0, len(named))
	for _, delivery := range source.AdditionalExtensions {
		entryID, err := agent.AdditionalExtensionCapabilityEntryID(delivery.ID)
		if err == nil && named[entryID] {
			out = append(out, delivery)
		}
	}
	return cloneExtensionDeliveries(out)
}
