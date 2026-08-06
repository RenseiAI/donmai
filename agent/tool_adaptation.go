package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// ToolLifecycleContractVersion is the closed tool, MCP, policy, lifecycle,
// replay, and cleanup adaptation contract. Child topology is deliberately not
// part of this contract; it belongs to the session graph.
const ToolLifecycleContractVersion = "donmai.tool-lifecycle/v1alpha1"

// ToolLifecycleChannel identifies independently enforced operational inputs.
type ToolLifecycleChannel string

// Tool lifecycle channels are the independently enforced input and evidence axes.
const (
	// ToolChannelToolPlugin carries a requested harness tool-plugin surface.
	ToolChannelToolPlugin       ToolLifecycleChannel = "tool_plugin"
	ToolChannelMCPServer        ToolLifecycleChannel = "mcp_server"
	ToolChannelAllowedTools     ToolLifecycleChannel = "allowed_tools"
	ToolChannelDisallowedTools  ToolLifecycleChannel = "disallowed_tools"
	ToolChannelPermissionConfig ToolLifecycleChannel = "permission_config"
	ToolChannelMCPToolNames     ToolLifecycleChannel = "mcp_tool_names"
	ToolChannelToolHook         ToolLifecycleChannel = "tool_hook"
	ToolChannelLifecycle        ToolLifecycleChannel = "lifecycle_event"
	ToolChannelReplay           ToolLifecycleChannel = "replay"
	ToolChannelCleanup          ToolLifecycleChannel = "cleanup"
)

// ToolDeliveryKind names the exact native or injected boundary evidenced by a
// harness profile. Unsupported is a capability value, never permission to
// strip a requested field.
type ToolDeliveryKind string

// Tool delivery kinds identify exact native, injected, or unsupported boundaries.
const (
	// ToolDeliveryUnsupported denies requirements on an unavailable boundary.
	ToolDeliveryUnsupported              ToolDeliveryKind = "unsupported"
	ToolDeliveryStubOracle               ToolDeliveryKind = "stub_oracle"
	ToolDeliveryClaudeCLIAllowDeny       ToolDeliveryKind = "claude_cli_allow_deny"
	ToolDeliveryClaudeMCPConfig          ToolDeliveryKind = "claude_cli_mcp_config"
	ToolDeliveryCodexApprovalBridge      ToolDeliveryKind = "codex_approval_bridge"
	ToolDeliveryCodexAppServerMCP        ToolDeliveryKind = "codex_app_server_mcp"
	ToolDeliveryCodexCLIMCPConfig        ToolDeliveryKind = "codex_cli_mcp_config"
	ToolDeliveryGeminiNativeBoundary     ToolDeliveryKind = "gemini_in_box_native_boundary"
	ToolDeliveryGeminiMCPBridge          ToolDeliveryKind = "gemini_in_box_mcp_bridge"
	ToolDeliveryAmpMCPConfig             ToolDeliveryKind = "amp_cli_mcp_config"
	ToolDeliveryOpenCodePermissionMap    ToolDeliveryKind = "opencode_permission_map"
	ToolDeliveryOpenCodeProjectMCP       ToolDeliveryKind = "opencode_project_mcp_config"
	ToolDeliveryPiInjectedBoundary       ToolDeliveryKind = "pi_handshake_policy_extension"
	ToolDeliveryStructuredProviderEvents ToolDeliveryKind = "structured_provider_events"
	ToolDeliveryCoarsePTYEvents          ToolDeliveryKind = "coarse_pty_events"
	ToolDeliveryStructuredEventReplay    ToolDeliveryKind = "structured_event_replay"
	ToolDeliveryTerminalCastReplay       ToolDeliveryKind = "terminal_cast_replay"
	ToolDeliveryHandleCleanup            ToolDeliveryKind = "handle_stop_and_resource_cleanup"
)

// EvidenceFidelity makes headless structured events and PTY byte/coarse
// evidence distinct. A coarse profile never inherits a structured claim.
type EvidenceFidelity string

// Evidence fidelity values distinguish unavailable, coarse PTY, and structured evidence.
const (
	// EvidenceUnsupported declares that the mode supplies no such evidence.
	EvidenceUnsupported EvidenceFidelity = "unsupported"
	EvidenceCoarse      EvidenceFidelity = "coarse"
	EvidenceStructured  EvidenceFidelity = "structured"
)

// ToolLifecycleProfile is the exact harness/version/mode declaration. Each
// independent field prevents MCP support from being inferred from a plugin
// boolean, or an approval bridge from being inferred from an autonomous flag.
type ToolLifecycleProfile struct {
	ID                       string             `json:"id"`
	Mode                     PromptSessionMode  `json:"mode"`
	ToolPluginDelivery       ToolDeliveryKind   `json:"toolPluginDelivery"`
	MCPDelivery              ToolDeliveryKind   `json:"mcpDelivery"`
	NativeToolPolicyDelivery ToolDeliveryKind   `json:"nativeToolPolicyDelivery"`
	PermissionConfigDelivery ToolDeliveryKind   `json:"permissionConfigDelivery"`
	MCPToolPolicyDelivery    ToolDeliveryKind   `json:"mcpToolPolicyDelivery"`
	ToolHookDelivery         ToolDeliveryKind   `json:"toolHookDelivery"`
	LifecycleDelivery        ToolDeliveryKind   `json:"lifecycleDelivery"`
	LifecycleFidelity        EvidenceFidelity   `json:"lifecycleFidelity"`
	LifecycleEvents          []EventKind        `json:"lifecycleEvents"`
	ReplayDelivery           ToolDeliveryKind   `json:"replayDelivery"`
	ReplayFidelity           EvidenceFidelity   `json:"replayFidelity"`
	ReplayEvents             []EventKind        `json:"replayEvents"`
	CleanupDelivery          ToolDeliveryKind   `json:"cleanupDelivery"`
	FallbackDeliveries       []ToolDeliveryKind `json:"fallbackDeliveries,omitempty"`
	EvidenceTier             string             `json:"evidenceTier"`
	ProductionEligible       bool               `json:"productionEligible"`
}

// ToolHookRequirement names a hook whose delivery must be proved before
// spawn. Kind remains open to the composing layer while the enclosing schema
// is closed; empty identities/kinds are malformed.
type ToolHookRequirement struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
}

// LifecycleRequirement names one normalized event and its minimum fidelity.
type LifecycleRequirement struct {
	ID              string           `json:"id"`
	Event           EventKind        `json:"event"`
	Required        bool             `json:"required"`
	MinimumFidelity EvidenceFidelity `json:"minimumFidelity"`
}

// ToolLifecycleFallback is a caller-authorized alternate. The selected
// delivery must also be declared by the exact profile.
type ToolLifecycleFallback struct {
	ID      string               `json:"id"`
	Channel ToolLifecycleChannel `json:"channel"`
	To      ToolDeliveryKind     `json:"to"`
}

// ToolLifecyclePlan carries requirements not already present as legacy Spec
// fields. Legacy non-empty fields are projected as required entries and cannot
// be made optional through this plan.
type ToolLifecyclePlan struct {
	ContractVersion     string                  `json:"contractVersion"`
	RequireToolPlugins  bool                    `json:"requireToolPlugins,omitempty"`
	ToolHooks           []ToolHookRequirement   `json:"toolHooks,omitempty"`
	Lifecycle           []LifecycleRequirement  `json:"lifecycle,omitempty"`
	Replay              *LifecycleRequirement   `json:"replay,omitempty"`
	RequireCleanup      bool                    `json:"requireCleanup,omitempty"`
	AuthorizedFallbacks []ToolLifecycleFallback `json:"authorizedFallbacks,omitempty"`
}

// ToolAdaptationOutcome is the immutable pre-spawn result for one entry.
type ToolAdaptationOutcome string

// Tool adaptation outcomes distinguish admission, runtime proof, downgrade,
// and denial. Pending values remain readable for pre-promotion receipts but
// the v1alpha1 compiler does not emit them without durable promotion support.
const (
	// ToolOutcomeAdmitted records that an exact delivery boundary was selected.
	ToolOutcomeAdmitted       ToolAdaptationOutcome = "admitted"
	ToolOutcomePendingRuntime ToolAdaptationOutcome = "pending_runtime"
	ToolOutcomePendingCleanup ToolAdaptationOutcome = "pending_cleanup"
	ToolOutcomeDenied         ToolAdaptationOutcome = "denied"
	ToolOutcomeDowngraded     ToolAdaptationOutcome = "downgraded"
)

// ToolAdaptationDenialCode is the typed failure taxonomy.
type ToolAdaptationDenialCode string

// Tool adaptation denial codes form the closed pre-spawn failure taxonomy.
const (
	// ToolDenialUnsupportedContract rejects an unknown contract version.
	ToolDenialUnsupportedContract   ToolAdaptationDenialCode = "unsupported_contract_version"
	ToolDenialMalformedPlan         ToolAdaptationDenialCode = "malformed_tool_lifecycle_plan"
	ToolDenialDeliveryUnsupported   ToolAdaptationDenialCode = "delivery_unsupported"
	ToolDenialDowngradeUnauthorized ToolAdaptationDenialCode = "downgrade_not_authorized"
	ToolDenialApplicationFailed     ToolAdaptationDenialCode = "application_failed"
)

// ToolLifecycleEntry records provenance without copying policies, commands,
// server environment, or any other potentially secret-bearing input.
type ToolLifecycleEntry struct {
	ID             string                   `json:"id"`
	Channel        ToolLifecycleChannel     `json:"channel"`
	Required       bool                     `json:"required"`
	Outcome        ToolAdaptationOutcome    `json:"outcome"`
	Delivery       ToolDeliveryKind         `json:"delivery,omitempty"`
	InputDigest    string                   `json:"inputDigest,omitempty"`
	FallbackAuthID string                   `json:"fallbackAuthorizationId,omitempty"`
	DenialCode     ToolAdaptationDenialCode `json:"denialCode,omitempty"`
}

// ToolLifecycleReceipt is persisted before provider side effects. Evidence
// tier and production eligibility are separate from declared capability.
type ToolLifecycleReceipt struct {
	ContractVersion    string               `json:"contractVersion"`
	ProfileID          string               `json:"profileId"`
	Decision           string               `json:"decision"`
	EvidenceTier       string               `json:"evidenceTier"`
	ProductionEligible bool                 `json:"productionEligible"`
	Entries            []ToolLifecycleEntry `json:"entries"`
}

// ToolAdaptationError is returned before any provider process starts.
type ToolAdaptationError struct {
	Code    ToolAdaptationDenialCode
	Channel ToolLifecycleChannel
	Detail  string
}

func (e *ToolAdaptationError) Error() string {
	return fmt.Sprintf("tool/lifecycle adaptation denied (%s, channel=%s): %s", e.Code, e.Channel, e.Detail)
}

// ToolLifecycleProfile returns the exact mode profile or false.
func (m HarnessManifest) ToolLifecycleProfile(mode PromptSessionMode) (ToolLifecycleProfile, bool) {
	for _, profile := range m.ToolLifecycle {
		if profile.Mode == mode {
			return profile, true
		}
	}
	return ToolLifecycleProfile{}, false
}

// EnsureToolLifecyclePlan projects an absent plan to the current contract.
func EnsureToolLifecyclePlan(spec Spec) ToolLifecyclePlan {
	if spec.ToolLifecyclePlan != nil {
		return *spec.ToolLifecyclePlan
	}
	return ToolLifecyclePlan{ContractVersion: ToolLifecycleContractVersion}
}

// PrepareHarness applies prompt adaptation first, then the independent
// tool/MCP/lifecycle contract. Both complete before provider side effects.
func PrepareHarness(spec Spec, manifest HarnessManifest) (Spec, error) {
	adapted, err := PreparePrompt(spec, manifest)
	if err != nil {
		return spec, err
	}
	return PrepareToolLifecycle(adapted, manifest)
}

// PrepareToolLifecycle compiles and persists the exact mode receipt.
func PrepareToolLifecycle(spec Spec, manifest HarnessManifest) (Spec, error) {
	profile, ok := manifest.ToolLifecycleProfile(PromptModeForSpec(spec))
	if !ok {
		receipt := ToolLifecycleReceipt{ContractVersion: ToolLifecycleContractVersion, Decision: "denied"}
		err := &ToolAdaptationError{Code: ToolDenialDeliveryUnsupported, Detail: "manifest has no tool/lifecycle profile for requested session mode"}
		if receiptErr := emitToolLifecycleReceipt(spec, receipt); receiptErr != nil {
			return spec, receiptPersistenceError(receiptErr)
		}
		return spec, err
	}
	adapted, receipt, err := AdaptToolLifecycle(spec, profile)
	if err != nil {
		if receiptErr := emitToolLifecycleReceipt(spec, receipt); receiptErr != nil {
			return spec, receiptPersistenceError(receiptErr)
		}
		return spec, err
	}
	if receiptErr := emitToolLifecycleReceipt(spec, receipt); receiptErr != nil {
		return spec, receiptPersistenceError(receiptErr)
	}
	adapted.ToolLifecycleReceipt = &receipt
	return adapted, nil
}

func receiptPersistenceError(err error) *ToolAdaptationError {
	return &ToolAdaptationError{Code: ToolDenialApplicationFailed, Detail: "persist tool/lifecycle receipt: " + err.Error()}
}

func emitToolLifecycleReceipt(spec Spec, receipt ToolLifecycleReceipt) error {
	if spec.OnToolLifecycleAdapted != nil {
		return spec.OnToolLifecycleAdapted(receipt)
	}
	return nil
}

type toolRequirement struct {
	id             string
	channel        ToolLifecycleChannel
	required       bool
	delivery       ToolDeliveryKind
	digest         string
	fallbackDenied bool
}

// AdaptToolLifecycle is the pure pre-spawn compiler.
func AdaptToolLifecycle(spec Spec, profile ToolLifecycleProfile) (Spec, ToolLifecycleReceipt, error) {
	plan := EnsureToolLifecyclePlan(spec)
	receipt := ToolLifecycleReceipt{
		ContractVersion:    ToolLifecycleContractVersion,
		ProfileID:          profile.ID,
		Decision:           "denied",
		EvidenceTier:       profile.EvidenceTier,
		ProductionEligible: profile.ProductionEligible,
	}
	deny := func(code ToolAdaptationDenialCode, channel ToolLifecycleChannel, id, digest, detail string) (Spec, ToolLifecycleReceipt, error) {
		receipt.Entries = append(receipt.Entries, ToolLifecycleEntry{ID: id, Channel: channel, Required: true, Outcome: ToolOutcomeDenied, InputDigest: digest, DenialCode: code})
		return spec, receipt, &ToolAdaptationError{Code: code, Channel: channel, Detail: detail}
	}

	if plan.ContractVersion != ToolLifecycleContractVersion {
		return deny(ToolDenialUnsupportedContract, "", "tool-lifecycle-plan", "", fmt.Sprintf("got %q", plan.ContractVersion))
	}
	if detail := validateToolLifecycleProfile(profile); detail != "" {
		return deny(ToolDenialMalformedPlan, "", "tool-lifecycle-profile", "", detail)
	}
	if detail := validateToolLifecyclePlan(plan); detail != "" {
		return deny(ToolDenialMalformedPlan, "", "tool-lifecycle-plan", "", detail)
	}
	if channel, detail := validateLegacyToolInputs(spec); detail != "" {
		return deny(ToolDenialMalformedPlan, channel, "legacy-tool-input", "", detail)
	}

	requirements := legacyToolRequirements(spec, profile)
	if plan.RequireToolPlugins {
		requirements = append(requirements, toolRequirement{id: "tool-plugins", channel: ToolChannelToolPlugin, required: true, delivery: profile.ToolPluginDelivery, digest: digestToolInput(true)})
	}
	for _, hook := range plan.ToolHooks {
		requirements = append(requirements, toolRequirement{id: hook.ID, channel: ToolChannelToolHook, required: hook.Required, delivery: profile.ToolHookDelivery, digest: digestToolInput(hook)})
	}
	for _, lifecycle := range plan.Lifecycle {
		// The runner does not yet promote admission receipts from actual runtime
		// events. A declaration alone is not evidence, so demand is denied until
		// a durable post-runtime verification path exists.
		requirements = append(requirements, toolRequirement{id: lifecycle.ID, channel: ToolChannelLifecycle, required: lifecycle.Required, delivery: ToolDeliveryUnsupported, digest: digestToolInput(lifecycle), fallbackDenied: true})
	}
	if plan.Replay != nil {
		requirements = append(requirements, toolRequirement{id: plan.Replay.ID, channel: ToolChannelReplay, required: plan.Replay.Required, delivery: ToolDeliveryUnsupported, digest: digestToolInput(plan.Replay), fallbackDenied: true})
	}
	if plan.RequireCleanup {
		requirements = append(requirements, toolRequirement{id: "cleanup", channel: ToolChannelCleanup, required: true, delivery: ToolDeliveryUnsupported, digest: digestToolInput(true), fallbackDenied: true})
	}

	for _, requirement := range requirements {
		entry := ToolLifecycleEntry{ID: requirement.id, Channel: requirement.channel, Required: requirement.required, InputDigest: requirement.digest}
		if requirement.delivery != ToolDeliveryUnsupported {
			entry.Outcome = ToolOutcomeAdmitted
			entry.Delivery = requirement.delivery
			receipt.Entries = append(receipt.Entries, entry)
			continue
		}
		if fallback, ok := authorizedToolFallback(plan, profile, requirement.channel); ok && !requirement.fallbackDenied {
			entry.Outcome = ToolOutcomeDowngraded
			entry.Delivery = fallback.To
			entry.FallbackAuthID = fallback.ID
			receipt.Entries = append(receipt.Entries, entry)
			continue
		}
		entry.Outcome = ToolOutcomeDenied
		entry.DenialCode = ToolDenialDeliveryUnsupported
		receipt.Entries = append(receipt.Entries, entry)
		if requirement.required {
			return spec, receipt, &ToolAdaptationError{Code: ToolDenialDeliveryUnsupported, Channel: requirement.channel, Detail: fmt.Sprintf("exact harness profile %q cannot apply required entry %q", profile.ID, requirement.id)}
		}
	}

	receipt.Decision = "ready"
	return spec, receipt, nil
}

func validateLegacyToolInputs(spec Spec) (ToolLifecycleChannel, string) {
	for _, named := range []struct {
		channel ToolLifecycleChannel
		values  []string
	}{
		{ToolChannelAllowedTools, spec.AllowedTools},
		{ToolChannelDisallowedTools, spec.DisallowedTools},
		{ToolChannelMCPToolNames, spec.MCPToolNames},
	} {
		for _, value := range named.values {
			if strings.TrimSpace(value) == "" {
				return named.channel, "tool policy entries must not be blank"
			}
		}
	}
	if spec.PermissionConfig != nil {
		patterns := append(append([]string(nil), spec.PermissionConfig.AllowPatterns...), spec.PermissionConfig.DisallowPatterns...)
		for _, pattern := range patterns {
			if strings.TrimSpace(pattern) == "" {
				return ToolChannelPermissionConfig, "permission patterns must not be blank"
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return ToolChannelPermissionConfig, "permission patterns must be valid regular expressions"
			}
		}
		switch strings.ToLower(strings.TrimSpace(spec.PermissionConfig.DefaultDecision)) {
		case "", "allow", "deny", "prompt", "ask":
		default:
			return ToolChannelPermissionConfig, "permission default decision must be allow, deny, or prompt"
		}
	}

	seenServers := map[string]bool{}
	for _, server := range spec.MCPServers {
		name := strings.TrimSpace(server.Name)
		if name == "" {
			return ToolChannelMCPServer, "MCP server names must not be blank"
		}
		if seenServers[name] {
			return ToolChannelMCPServer, "MCP server names must be unique"
		}
		seenServers[name] = true
		switch strings.ToLower(strings.TrimSpace(server.Type)) {
		case "", "stdio":
			if strings.TrimSpace(server.Command) == "" {
				return ToolChannelMCPServer, "stdio MCP servers require a command"
			}
		case "http":
			u, err := url.Parse(server.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return ToolChannelMCPServer, "HTTP MCP servers require an absolute http or https URL"
			}
			for header := range server.Headers {
				if strings.TrimSpace(header) == "" {
					return ToolChannelMCPServer, "HTTP MCP header names must not be blank"
				}
			}
		default:
			return ToolChannelMCPServer, "MCP server type must be stdio or http"
		}
	}
	return "", ""
}

func legacyToolRequirements(spec Spec, profile ToolLifecycleProfile) []toolRequirement {
	var out []toolRequirement
	if len(spec.AllowedTools) > 0 {
		out = append(out, toolRequirement{id: "allowed-tools", channel: ToolChannelAllowedTools, required: true, delivery: profile.NativeToolPolicyDelivery, digest: digestToolInput(spec.AllowedTools)})
	}
	if len(spec.DisallowedTools) > 0 {
		out = append(out, toolRequirement{id: "disallowed-tools", channel: ToolChannelDisallowedTools, required: true, delivery: profile.NativeToolPolicyDelivery, digest: digestToolInput(spec.DisallowedTools)})
	}
	if spec.PermissionConfig != nil {
		out = append(out, toolRequirement{id: "permission-config", channel: ToolChannelPermissionConfig, required: true, delivery: profile.PermissionConfigDelivery, digest: digestToolInput(spec.PermissionConfig)})
	}
	if len(spec.MCPServers) > 0 {
		out = append(out, toolRequirement{id: "mcp-servers", channel: ToolChannelMCPServer, required: true, delivery: profile.MCPDelivery, digest: digestToolInput(spec.MCPServers)})
	}
	if len(spec.MCPToolNames) > 0 {
		out = append(out, toolRequirement{id: "mcp-tool-names", channel: ToolChannelMCPToolNames, required: true, delivery: profile.MCPToolPolicyDelivery, digest: digestToolInput(spec.MCPToolNames)})
	}
	return out
}

func validateToolLifecycleProfile(profile ToolLifecycleProfile) string {
	if profile.ID == "" || profile.Mode == "" || profile.EvidenceTier == "" {
		return "profile requires id, mode, and evidence tier"
	}
	values := []ToolDeliveryKind{profile.ToolPluginDelivery, profile.MCPDelivery, profile.NativeToolPolicyDelivery, profile.PermissionConfigDelivery, profile.MCPToolPolicyDelivery, profile.ToolHookDelivery, profile.LifecycleDelivery, profile.ReplayDelivery, profile.CleanupDelivery}
	for _, value := range values {
		if !isKnownToolDelivery(value) {
			return "profile contains an unknown delivery declaration"
		}
	}
	seenFallbackDeliveries := map[ToolDeliveryKind]bool{}
	for _, value := range profile.FallbackDeliveries {
		if !isKnownToolDelivery(value) || value == ToolDeliveryUnsupported || seenFallbackDeliveries[value] {
			return "profile contains an unknown, unsupported, or duplicate fallback delivery"
		}
		seenFallbackDeliveries[value] = true
	}
	if fidelityRank(profile.LifecycleFidelity) < 0 || fidelityRank(profile.ReplayFidelity) < 0 {
		return "profile contains an unknown evidence fidelity"
	}
	if profile.LifecycleDelivery != ToolDeliveryUnsupported && len(profile.LifecycleEvents) == 0 {
		return "supported lifecycle delivery requires exact event declarations"
	}
	if profile.ReplayDelivery != ToolDeliveryUnsupported && len(profile.ReplayEvents) == 0 {
		return "supported replay delivery requires exact event declarations"
	}
	if !validEventSet(profile.LifecycleEvents) || !validEventSet(profile.ReplayEvents) {
		return "profile contains an unknown or duplicate lifecycle event"
	}
	return ""
}

func validEventSet(events []EventKind) bool {
	seen := map[EventKind]bool{}
	for _, event := range events {
		if seen[event] || !isKnownEvent(event) {
			return false
		}
		seen[event] = true
	}
	return true
}

func isKnownEvent(event EventKind) bool {
	switch event {
	case EventInit, EventSystem, EventAssistantText, EventLlmCall, EventToolUse, EventToolResult, EventToolProgress, EventResult, EventError:
		return true
	default:
		return false
	}
}

func validateToolLifecyclePlan(plan ToolLifecyclePlan) string {
	seen := map[string]bool{}
	for _, hook := range plan.ToolHooks {
		if hook.ID == "" || hook.Kind == "" || seen[hook.ID] {
			return "tool hooks require unique non-empty ids and kinds"
		}
		seen[hook.ID] = true
	}
	for _, lifecycle := range plan.Lifecycle {
		if lifecycle.ID == "" || !isKnownEvent(lifecycle.Event) || seen[lifecycle.ID] || fidelityRank(lifecycle.MinimumFidelity) < 0 {
			return "lifecycle requirements require unique ids, events, and known fidelity"
		}
		seen[lifecycle.ID] = true
	}
	if replay := plan.Replay; replay != nil {
		if replay.ID == "" || !isKnownEvent(replay.Event) || seen[replay.ID] || fidelityRank(replay.MinimumFidelity) < 0 {
			return "replay requires a unique id and known fidelity"
		}
	}
	for _, fallback := range plan.AuthorizedFallbacks {
		if fallback.ID == "" || !isKnownToolLifecycleChannel(fallback.Channel) || !isKnownToolDelivery(fallback.To) || fallback.To == ToolDeliveryUnsupported {
			return "fallbacks require id, a known channel, and a known non-unsupported delivery"
		}
	}
	return ""
}

func isKnownToolLifecycleChannel(channel ToolLifecycleChannel) bool {
	switch channel {
	case ToolChannelToolPlugin, ToolChannelMCPServer, ToolChannelAllowedTools,
		ToolChannelDisallowedTools, ToolChannelPermissionConfig, ToolChannelMCPToolNames,
		ToolChannelToolHook, ToolChannelLifecycle, ToolChannelReplay, ToolChannelCleanup:
		return true
	default:
		return false
	}
}

func isKnownToolDelivery(delivery ToolDeliveryKind) bool {
	switch delivery {
	case ToolDeliveryUnsupported, ToolDeliveryStubOracle, ToolDeliveryClaudeCLIAllowDeny,
		ToolDeliveryClaudeMCPConfig, ToolDeliveryCodexApprovalBridge, ToolDeliveryCodexAppServerMCP,
		ToolDeliveryCodexCLIMCPConfig,
		ToolDeliveryGeminiNativeBoundary, ToolDeliveryGeminiMCPBridge, ToolDeliveryAmpMCPConfig,
		ToolDeliveryOpenCodePermissionMap, ToolDeliveryOpenCodeProjectMCP, ToolDeliveryPiInjectedBoundary,
		ToolDeliveryStructuredProviderEvents, ToolDeliveryCoarsePTYEvents, ToolDeliveryStructuredEventReplay,
		ToolDeliveryTerminalCastReplay, ToolDeliveryHandleCleanup:
		return true
	default:
		return false
	}
}

func authorizedToolFallback(plan ToolLifecyclePlan, profile ToolLifecycleProfile, channel ToolLifecycleChannel) (ToolLifecycleFallback, bool) {
	declared := append([]ToolDeliveryKind(nil), profile.FallbackDeliveries...)
	sort.Slice(declared, func(i, j int) bool { return declared[i] < declared[j] })
	for _, fallback := range plan.AuthorizedFallbacks {
		if fallback.Channel != channel {
			continue
		}
		i := sort.Search(len(declared), func(i int) bool { return declared[i] >= fallback.To })
		if i < len(declared) && declared[i] == fallback.To {
			return fallback, true
		}
	}
	return ToolLifecycleFallback{}, false
}

func fidelityRank(fidelity EvidenceFidelity) int {
	switch fidelity {
	case EvidenceUnsupported:
		return 0
	case EvidenceCoarse:
		return 1
	case EvidenceStructured:
		return 2
	default:
		return -1
	}
}

func digestToolInput(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		body = []byte(fmt.Sprintf("%T", value))
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
