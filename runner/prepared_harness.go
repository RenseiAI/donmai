package runner

import (
	"errors"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/prompt"
)

func buildPreparedSourceSpec(qw QueuedWork, selection harnessSelection) (agent.Spec, []string, error) {
	provider := selection.Provider
	if provider == nil {
		return agent.Spec{}, nil, errors.New("runner: prepared source requires exact provider")
	}
	working := qw
	if working.isInterview() {
		working.SystemPromptOverride = buildInterviewSystemPrompt(working.SystemPromptOverride, interview.InterviewCompleteSentinel)
	}
	builder := prompt.NewBuilder()
	inlineAppend, inlineDisallow, _ := foldInlineSkills("", working.Skills)
	builder.SkillAppend = inlineAppend
	composition, err := builder.BuildComposition(working.QueuedWork)
	if err != nil {
		return agent.Spec{}, nil, err
	}
	composition.HarnessProtocol = injectCodeIntelPartial(composition.HarnessProtocol, provider.Capabilities(), working.CodeIntel)
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
	mode := sessionPromptMode(working, selection.effectiveCell)
	authorityWork := working
	authorityWork.PlatformURL = "https://runtime.invalid"
	authorityWork.AuthToken = "runtime-materialized"
	defaults := defaultMCPServersForHarness(authorityWork, "/runtime/worktree", provider, mode)
	runtimeNames := make([]string, 0, len(defaults))
	for _, server := range defaults {
		runtimeNames = append(runtimeNames, server.Name)
	}
	mcpServers := mergeMCPServers(defaults, working.McpServers)
	spec := translateSpec(working, provider.Capabilities(), SpecInputs{
		Prompt: userPrompt, SystemPromptAppend: composition.SystemPrompt(), PromptPlan: promptPlan,
		InitialContext: composition.InitialContext, MCPServers: mcpServers,
		Autonomous: mode == agent.PromptModeAutonomous, ProviderName: string(provider.Name()),
	})
	spec.PromptMode = mode
	if working.isInteractive() {
		spec.Interactive = &agent.InteractiveSpec{}
	}
	if len(inlineDisallow) > 0 {
		spec.DisallowedTools = append(spec.DisallowedTools, inlineDisallow...)
		if spec.PermissionConfig != nil {
			spec.PermissionConfig.DisallowPatterns = append(spec.PermissionConfig.DisallowPatterns, inlineDisallow...)
		}
	}
	if working.isInterview() {
		spec.DisallowedTools = append(spec.DisallowedTools, "AskUserQuestion", "Write", "Edit", "Task", "Bash")
	}
	spec, err = bindAdmissionToolLifecyclePlan(spec, selection.receipt, selection.claimReceipt)
	if err != nil {
		return agent.Spec{}, nil, err
	}
	return spec, runtimeNames, nil
}

func compilePreparedHarness(qw QueuedWork, selection harnessSelection) (*agent.PreparedHarness, agent.Spec, error) {
	spec, runtimeNames, err := buildPreparedSourceSpec(qw, selection)
	if err != nil {
		return nil, agent.Spec{}, err
	}
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
