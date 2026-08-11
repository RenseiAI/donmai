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
	ID               string           `json:"id"`
	Event            EventKind        `json:"event"`
	Required         bool             `json:"required"`
	MinimumFidelity  EvidenceFidelity `json:"minimumFidelity"`
	ParametersDigest string           `json:"parametersDigest,omitempty"`
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
	ContractVersion          string                  `json:"contractVersion"`
	AdmissionReceiptID       string                  `json:"admissionReceiptId,omitempty"`
	ClaimReceiptID           string                  `json:"claimReceiptId,omitempty"`
	OperationalPayloadDigest string                  `json:"operationalPayloadDigest,omitempty"`
	RequireToolPlugins       bool                    `json:"requireToolPlugins,omitempty"`
	ToolHooks                []ToolHookRequirement   `json:"toolHooks,omitempty"`
	Lifecycle                []LifecycleRequirement  `json:"lifecycle,omitempty"`
	Replay                   *LifecycleRequirement   `json:"replay,omitempty"`
	RequireCleanup           bool                    `json:"requireCleanup,omitempty"`
	CleanupParametersDigest  string                  `json:"cleanupParametersDigest,omitempty"`
	AuthorizedFallbacks      []ToolLifecycleFallback `json:"authorizedFallbacks,omitempty"`
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
	ContractVersion          string               `json:"contractVersion"`
	AdmissionReceiptID       string               `json:"admissionReceiptId,omitempty"`
	ClaimReceiptID           string               `json:"claimReceiptId,omitempty"`
	OperationalPayloadDigest string               `json:"operationalPayloadDigest,omitempty"`
	ProfileID                string               `json:"profileId"`
	Decision                 string               `json:"decision"`
	EvidenceTier             string               `json:"evidenceTier"`
	ProductionEligible       bool                 `json:"productionEligible"`
	Entries                  []ToolLifecycleEntry `json:"entries"`
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
	if spec.PreparedHarness != nil {
		return ApplyPreparedHarness(spec, manifest)
	}
	if err := ValidateSpecCapabilities(spec, manifest); err != nil {
		return spec, err
	}
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
		plan := EnsureToolLifecyclePlan(spec)
		receipt := ToolLifecycleReceipt{
			ContractVersion:          ToolLifecycleContractVersion,
			AdmissionReceiptID:       plan.AdmissionReceiptID,
			ClaimReceiptID:           plan.ClaimReceiptID,
			OperationalPayloadDigest: plan.OperationalPayloadDigest,
			Decision:                 "denied",
		}
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
	id       string
	channel  ToolLifecycleChannel
	required bool
	delivery ToolDeliveryKind
	digest   string
	// fallbackDenied blocks the downgrade lane for a requirement no alternate
	// delivery could satisfy — an authorized downgrade selects a different
	// boundary, it never invents evidence the exact profile does not declare.
	fallbackDenied bool
	// pendingOutcome is the admitted outcome for a requirement whose delivery
	// boundary is proved pre-spawn but whose evidence only exists at runtime or
	// cleanup. Pre-spawn channels leave it empty and record `admitted`.
	pendingOutcome ToolAdaptationOutcome
}

// admittedOutcome names the receipt outcome for a requirement whose delivery
// boundary the exact profile declares. Per
// ADR-2026-08-06-harness-adaptation-plan-and-receipt.md D4, pre-spawn entries
// must already be terminal, while runtime and cleanup entries are explicitly
// pending and transition through append-only outcome records.
func (r toolRequirement) admittedOutcome() ToolAdaptationOutcome {
	if r.pendingOutcome != "" {
		return r.pendingOutcome
	}
	return ToolOutcomeAdmitted
}

// runtimeEvidenceRequirement answers one lifecycle or replay requirement
// against the exact profile's declared surface, per harness rather than per
// channel. The profile is the executor-attested inventory
// (ADR-2026-08-08-harness-authority-admission-plane-parked.md D3.2: the
// attestation derives from the same generated artifact the executor reads), so
// a harness that declares the event at the demanded fidelity delivers it and
// one that does not degrades through the contract's named lanes:
//
//   - an event the profile never emits cannot be rescued by an alternate
//     delivery, so the downgrade lane is closed and a required entry denies;
//   - a fidelity shortfall is exactly what an authorized downgrade names, so the
//     downgrade lane stays open — a coarse profile never inherits a structured
//     claim (ADR-2026-08-06 D6) but may still be accepted as coarse when the
//     caller authorized that alternate before admission.
func runtimeEvidenceRequirement(requirement LifecycleRequirement, channel ToolLifecycleChannel, delivery ToolDeliveryKind, fidelity EvidenceFidelity, events []EventKind) toolRequirement {
	out := toolRequirement{
		id:             requirement.ID,
		channel:        channel,
		required:       requirement.Required,
		delivery:       delivery,
		digest:         requirementInputDigest(requirement.ParametersDigest, requirement),
		pendingOutcome: ToolOutcomePendingRuntime,
	}
	if !declaresEvent(events, requirement.Event) {
		out.delivery = ToolDeliveryUnsupported
		out.fallbackDenied = true
		return out
	}
	if fidelityRank(fidelity) < fidelityRank(requirement.MinimumFidelity) {
		out.delivery = ToolDeliveryUnsupported
	}
	return out
}

func declaresEvent(events []EventKind, event EventKind) bool {
	for _, declared := range events {
		if declared == event {
			return true
		}
	}
	return false
}

// AdaptToolLifecycle is the pure pre-spawn compiler.
func AdaptToolLifecycle(spec Spec, profile ToolLifecycleProfile) (Spec, ToolLifecycleReceipt, error) {
	plan := EnsureToolLifecyclePlan(spec)
	receipt := ToolLifecycleReceipt{
		ContractVersion:          ToolLifecycleContractVersion,
		AdmissionReceiptID:       plan.AdmissionReceiptID,
		ClaimReceiptID:           plan.ClaimReceiptID,
		OperationalPayloadDigest: plan.OperationalPayloadDigest,
		ProfileID:                profile.ID,
		Decision:                 "denied",
		EvidenceTier:             profile.EvidenceTier,
		ProductionEligible:       profile.ProductionEligible,
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
	// Lifecycle, replay, and cleanup are runtime and cleanup phase channels:
	// the pre-spawn compiler proves the exact profile declares the delivery
	// boundary, and the evidence itself arrives later. Answering them per
	// harness from the profile — rather than denying every harness alike — is
	// what keeps the receipt truthful in both directions: a profile that
	// declares the boundary admits it as pending, and one that does not records
	// a denied entry naming the channel.
	for _, lifecycle := range plan.Lifecycle {
		requirements = append(requirements, runtimeEvidenceRequirement(lifecycle, ToolChannelLifecycle, profile.LifecycleDelivery, profile.LifecycleFidelity, profile.LifecycleEvents))
	}
	if plan.Replay != nil {
		requirements = append(requirements, runtimeEvidenceRequirement(*plan.Replay, ToolChannelReplay, profile.ReplayDelivery, profile.ReplayFidelity, profile.ReplayEvents))
	}
	if plan.RequireCleanup {
		// Cleanup carries no event or fidelity axis: the profile either declares
		// a teardown boundary for its handles and resources or it does not.
		requirements = append(requirements, toolRequirement{id: "cleanup", channel: ToolChannelCleanup, required: true, delivery: profile.CleanupDelivery, digest: requirementInputDigest(plan.CleanupParametersDigest, true), pendingOutcome: ToolOutcomePendingCleanup})
	}

	for _, requirement := range requirements {
		entry := ToolLifecycleEntry{ID: requirement.id, Channel: requirement.channel, Required: requirement.required, InputDigest: requirement.digest}
		if requirement.delivery != ToolDeliveryUnsupported {
			entry.Outcome = requirement.admittedOutcome()
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

// toolDesignatorRe matches the tool-designator permission grammar:
// a tool name (`Read`, `mcp__linear__list_issues`) optionally followed by a
// single parenthesized argument glob (`Bash(git *)`, `Bash(*)`). This is the
// grammar agent cards express AllowedTools/DisallowedTools in and the grammar
// the codex approval bridge's compilePatterns consumes ahead of its raw-regex
// fallback. Kept deliberately narrow: one leading identifier, one optional
// trailing "(...)" group spanning the rest of the string.
var toolDesignatorRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\(.*\))?$`)

func isToolDesignatorPattern(pattern string) bool {
	return toolDesignatorRe.MatchString(strings.TrimSpace(pattern))
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
			// Tool designators ("Read", "Bash(git *)") are a first-class
			// pattern grammar here, not just regexes: the codex approval
			// bridge's compilePatterns consumes designators natively
			// (file-change designators flip its file gate, Bash(...)
			// designators become command regexes), and the runner's
			// AllowedTools→PermissionConfig bridge forwards card
			// designators verbatim. Requiring regexp validity for them
			// fail-closed every headless codex spawn whose card allowed
			// e.g. "Bash(*)" ("*" after "(" is not a valid regex).
			if isToolDesignatorPattern(pattern) {
				continue
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return ToolChannelPermissionConfig, "permission patterns must be valid regular expressions or tool designators"
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
		// MCP tool names are a NARROWING hint over the tools of the mounted
		// servers — load-bearing only when the harness cannot deliver the
		// mount boundary itself. When the profile delivers mcp-servers (the
		// codex shape: it mounts exactly the configured servers but
		// auto-discovers their tools, so a name filter is undeliverable),
		// an unapplicable name policy is bounded by the mounts we control:
		// record a truthful denied entry on the receipt and proceed. When
		// the profile cannot deliver mounts either (external-attach
		// harnesses like opencode, where the platform controls nothing on
		// the attached server), an unapplicable name policy means zero MCP
		// control and MUST stay a fatal denial.
		out = append(out, toolRequirement{id: "mcp-tool-names", channel: ToolChannelMCPToolNames, required: profile.MCPDelivery == ToolDeliveryUnsupported, delivery: profile.MCPToolPolicyDelivery, digest: digestToolInput(spec.MCPToolNames)})
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
	if (plan.AdmissionReceiptID == "") != (plan.OperationalPayloadDigest == "") {
		return "admission receipt id and operational payload digest must be present together"
	}
	if plan.AdmissionReceiptID != "" && !contractReferencePattern.MatchString(plan.AdmissionReceiptID) {
		return "admission receipt id is malformed"
	}
	if plan.ClaimReceiptID != "" && plan.AdmissionReceiptID == "" {
		return "claim receipt id requires an admission receipt id"
	}
	if plan.ClaimReceiptID != "" && !contractReferencePattern.MatchString(plan.ClaimReceiptID) {
		return "claim receipt id is malformed"
	}
	if plan.OperationalPayloadDigest != "" && !sha256DigestPattern.MatchString(plan.OperationalPayloadDigest) {
		return "operational payload digest must be a lowercase SHA-256 digest"
	}
	if plan.CleanupParametersDigest != "" && !sha256DigestPattern.MatchString(plan.CleanupParametersDigest) {
		return "cleanup parameters digest must be a lowercase SHA-256 digest"
	}
	seen := map[string]bool{}
	for _, hook := range plan.ToolHooks {
		if hook.ID == "" || hook.Kind == "" || seen[hook.ID] {
			return "tool hooks require unique non-empty ids and kinds"
		}
		seen[hook.ID] = true
	}
	for _, lifecycle := range plan.Lifecycle {
		if lifecycle.ID == "" || !isKnownEvent(lifecycle.Event) || seen[lifecycle.ID] || fidelityRank(lifecycle.MinimumFidelity) < 0 || !validParametersDigest(lifecycle.ParametersDigest) {
			return "lifecycle requirements require unique ids, events, and known fidelity"
		}
		seen[lifecycle.ID] = true
	}
	if replay := plan.Replay; replay != nil {
		if replay.ID == "" || !isKnownEvent(replay.Event) || seen[replay.ID] || fidelityRank(replay.MinimumFidelity) < 0 || !validParametersDigest(replay.ParametersDigest) {
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

var (
	contractReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]*(?:[:/][A-Za-z0-9._@+-]+)*$`)
	sha256DigestPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validParametersDigest(digest string) bool {
	return digest == "" || sha256DigestPattern.MatchString(digest)
}

func requirementInputDigest(parametersDigest string, value any) string {
	if parametersDigest != "" {
		return parametersDigest
	}
	return digestToolInput(value)
}
