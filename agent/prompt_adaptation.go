package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// PromptContractVersion is the closed prompt-only adaptation contract. It is
// deliberately narrower than the full harness-adaptation contract: tools,
// MCP, hooks, credentials, and lifecycle remain separate capabilities.
const PromptContractVersion = "donmai.prompt-delivery/v1alpha1"

// PromptSessionMode selects the exact harness mode whose native prompt surface
// is being used.
type PromptSessionMode string

const (
	// PromptModeAutonomous selects a headless, runner-controlled session.
	PromptModeAutonomous PromptSessionMode = "autonomous"
	// PromptModeHumanControlled selects an interactive session.
	PromptModeHumanControlled PromptSessionMode = "human_controlled"
)

// BaseInstructionStrategy says what happens to the replaceable base layer.
// The harness operating protocol is outside this strategy and is always
// preserved.
type BaseInstructionStrategy string

const (
	// BaseInstructionsPreserve leaves the harness baseline unchanged.
	BaseInstructionsPreserve BaseInstructionStrategy = "preserve"
	// BaseInstructionsAppend adds policy after the baseline.
	BaseInstructionsAppend BaseInstructionStrategy = "append"
	// BaseInstructionsReplace replaces only the replaceable base layer.
	BaseInstructionsReplace BaseInstructionStrategy = "replace"
)

// UserPromptPosition is the only authority an amendment receives.
type UserPromptPosition string

const (
	// UserPromptPrepend places an amendment before the caller's task.
	UserPromptPrepend UserPromptPosition = "prepend"
	// UserPromptAppend places an amendment after the caller's task.
	UserPromptAppend UserPromptPosition = "append"
)

// PromptChannel identifies one independently receipted semantic input.
type PromptChannel string

const (
	// PromptChannelHarnessProtocol is harness-owned operating policy.
	PromptChannelHarnessProtocol PromptChannel = "harness_protocol"
	// PromptChannelBaseInstructions is the replaceable base layer.
	PromptChannelBaseInstructions PromptChannel = "base_instruction"
	// PromptChannelRoleIntent is agent-card role context.
	PromptChannelRoleIntent PromptChannel = "role_intent"
	// PromptChannelInitialContext is session context delivered at start.
	PromptChannelInitialContext PromptChannel = "initial_context"
	// PromptChannelUserPrompt is the caller's task.
	PromptChannelUserPrompt PromptChannel = "user_prompt"
	// PromptChannelUserAmendment is an ordered harness/caller amendment.
	PromptChannelUserAmendment PromptChannel = "user_prompt_amendment"
)

// PromptDeliveryKind names the exact native wire/config surface observed by
// harness tests. Unsupported is a truthful capability value, never a request
// to silently omit content.
type PromptDeliveryKind string

const (
	// PromptDeliveryUnsupported declares that no native surface exists.
	PromptDeliveryUnsupported PromptDeliveryKind = "unsupported"
	// PromptDeliveryPreserveExisting records an unchanged native baseline.
	PromptDeliveryPreserveExisting PromptDeliveryKind = "preserve_existing"
	// PromptDeliveryClaudeSystemAppend uses Claude's append-system flag.
	PromptDeliveryClaudeSystemAppend PromptDeliveryKind = "claude_cli_append_system_prompt"
	// PromptDeliveryClaudeUserStdin uses Claude headless stdin.
	PromptDeliveryClaudeUserStdin PromptDeliveryKind = "claude_cli_stdin"
	// PromptDeliveryClaudePTYSeed uses Claude's interactive seed argument.
	PromptDeliveryClaudePTYSeed PromptDeliveryKind = "claude_cli_pty_seed"
	// PromptDeliveryCodexBaseInstructions uses app-server baseInstructions.
	PromptDeliveryCodexBaseInstructions PromptDeliveryKind = "codex_app_server_base_instructions"
	// PromptDeliveryCodexDeveloperInstructions uses app-server developerInstructions.
	PromptDeliveryCodexDeveloperInstructions PromptDeliveryKind = "codex_app_server_developer_instructions"
	// PromptDeliveryCodexTurnInput uses app-server turn input.
	PromptDeliveryCodexTurnInput PromptDeliveryKind = "codex_app_server_turn_input"
	// PromptDeliveryCodexCLIInstructions uses CLI developer_instructions config.
	PromptDeliveryCodexCLIInstructions PromptDeliveryKind = "codex_cli_developer_instructions"
	// PromptDeliveryCodexPTYSeed uses Codex's interactive seed argument.
	PromptDeliveryCodexPTYSeed PromptDeliveryKind = "codex_cli_pty_seed"
	// PromptDeliveryGeminiSystemInstruction uses Gemini systemInstruction.
	PromptDeliveryGeminiSystemInstruction PromptDeliveryKind = "gemini_system_instruction"
	// PromptDeliveryGeminiUserContent uses Gemini user content.
	PromptDeliveryGeminiUserContent PromptDeliveryKind = "gemini_user_content"
	// PromptDeliveryOllamaSystemMessage uses an Ollama system message.
	PromptDeliveryOllamaSystemMessage PromptDeliveryKind = "ollama_system_message"
	// PromptDeliveryOllamaUserMessage uses an Ollama user message.
	PromptDeliveryOllamaUserMessage PromptDeliveryKind = "ollama_user_message"
	// PromptDeliveryAmpStdin uses Amp execute-mode stdin.
	PromptDeliveryAmpStdin PromptDeliveryKind = "amp_stdin"
	// PromptDeliveryAgyPromptFlag uses agy's prompt flag.
	PromptDeliveryAgyPromptFlag PromptDeliveryKind = "agy_prompt_flag"
	// PromptDeliveryOpenCodePrompt uses OpenCode's prompt request.
	PromptDeliveryOpenCodePrompt PromptDeliveryKind = "opencode_prompt"
	// PromptDeliveryPiSystemAppend uses pi's append-system flag.
	PromptDeliveryPiSystemAppend PromptDeliveryKind = "pi_cli_append_system_prompt"
	// PromptDeliveryPiRPCPrompt uses pi's RPC prompt message.
	PromptDeliveryPiRPCPrompt PromptDeliveryKind = "pi_rpc_prompt"
	// PromptDeliveryShellPTYSeed uses the shell PTY seed.
	PromptDeliveryShellPTYSeed PromptDeliveryKind = "shell_pty_seed"
	// PromptDeliveryAuthorizedUserDowngrade records explicit user fallback.
	PromptDeliveryAuthorizedUserDowngrade PromptDeliveryKind = "authorized_user_prompt_downgrade"
)

// PromptContent is one source-addressed semantic contribution. Text is the
// runtime payload; receipts retain only its digest.
type PromptContent struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Required bool   `json:"required"`
}

// BaseInstructionPlan controls only the replaceable base-instruction layer.
// Replace requires a stable authorization reference.
type BaseInstructionPlan struct {
	Strategy                   BaseInstructionStrategy `json:"strategy"`
	Content                    *PromptContent          `json:"content,omitempty"`
	ReplacementAuthorizationID string                  `json:"replacementAuthorizationId,omitempty"`
}

// UserPromptAmendment is ordered deterministically by Order then ID.
type UserPromptAmendment struct {
	ID       string             `json:"id"`
	Position UserPromptPosition `json:"position"`
	Order    int                `json:"order"`
	Content  PromptContent      `json:"content"`
}

// PromptDowngradeAuthorization is an explicit caller-selected semantic
// downgrade. Today only system/context-to-user delivery is supported.
type PromptDowngradeAuthorization struct {
	ID      string        `json:"id"`
	Channel PromptChannel `json:"channel"`
	To      PromptChannel `json:"to"`
}

// PromptPlan keeps instruction authorities independent until the exact
// harness adapter compiles them onto native surfaces.
type PromptPlan struct {
	ContractVersion      string                         `json:"contractVersion"`
	HarnessProtocol      *PromptContent                 `json:"harnessProtocol,omitempty"`
	BaseInstructions     BaseInstructionPlan            `json:"baseInstructions"`
	RoleIntent           *PromptContent                 `json:"roleIntent,omitempty"`
	InitialContext       []PromptContent                `json:"initialContext,omitempty"`
	UserPrompt           PromptContent                  `json:"userPrompt"`
	UserAmendments       []UserPromptAmendment          `json:"userPromptAmendments,omitempty"`
	AuthorizedDowngrades []PromptDowngradeAuthorization `json:"authorizedDowngrades,omitempty"`
}

// PromptDeliveryProfile is the exact harness/version/mode prompt capability
// declaration harvested from a harness manifest.
type PromptDeliveryProfile struct {
	ID                  string             `json:"id"`
	Mode                PromptSessionMode  `json:"mode"`
	SystemDelivery      PromptDeliveryKind `json:"systemDelivery"`
	BaseAppendDelivery  PromptDeliveryKind `json:"baseAppendDelivery"`
	BaseReplaceDelivery PromptDeliveryKind `json:"baseReplaceDelivery"`
	ContextDelivery     PromptDeliveryKind `json:"contextDelivery"`
	UserDelivery        PromptDeliveryKind `json:"userDelivery"`
	AmendmentDelivery   PromptDeliveryKind `json:"amendmentDelivery"`
}

// PromptDeliveryOutcome is the immutable outcome for one semantic entry.
type PromptDeliveryOutcome string

const (
	// PromptOutcomePreserved records an intentionally unchanged baseline.
	PromptOutcomePreserved PromptDeliveryOutcome = "preserved"
	// PromptOutcomeDelivered records native delivery.
	PromptOutcomeDelivered PromptDeliveryOutcome = "delivered"
	// PromptOutcomeDowngraded records an authorized alternate delivery.
	PromptOutcomeDowngraded PromptDeliveryOutcome = "downgraded"
	// PromptOutcomeDenied records an unapplied semantic.
	PromptOutcomeDenied PromptDeliveryOutcome = "denied"
)

// PromptDeliveryEntry records provenance without copying prompt bodies.
type PromptDeliveryEntry struct {
	ID                         string                  `json:"id"`
	Channel                    PromptChannel           `json:"channel"`
	Required                   bool                    `json:"required"`
	Outcome                    PromptDeliveryOutcome   `json:"outcome"`
	Delivery                   PromptDeliveryKind      `json:"delivery,omitempty"`
	BaseInstructionStrategy    BaseInstructionStrategy `json:"baseInstructionStrategy,omitempty"`
	ReplacementAuthorizationID string                  `json:"replacementAuthorizationId,omitempty"`
	ContentDigest              string                  `json:"contentDigest,omitempty"`
	DowngradeAuthID            string                  `json:"downgradeAuthorizationId,omitempty"`
	DenialCode                 PromptDenialCode        `json:"denialCode,omitempty"`
}

// PromptDeliveryReceipt is created before spawn. It is safe to persist: only
// stable IDs and SHA-256 digests are retained.
type PromptDeliveryReceipt struct {
	ContractVersion string                `json:"contractVersion"`
	ProfileID       string                `json:"profileId"`
	Decision        string                `json:"decision"`
	Entries         []PromptDeliveryEntry `json:"entries"`
}

// PromptDenialCode is the typed pre-spawn failure taxonomy.
type PromptDenialCode string

const (
	// PromptDenialUnsupportedContract rejects an unknown contract version.
	PromptDenialUnsupportedContract PromptDenialCode = "unsupported_contract_version"
	// PromptDenialMalformedPlan rejects an invalid closed plan/profile.
	PromptDenialMalformedPlan PromptDenialCode = "malformed_prompt_plan"
	// PromptDenialUnsupportedStrategy rejects an unsupported base strategy.
	PromptDenialUnsupportedStrategy PromptDenialCode = "unsupported_instruction_strategy"
	// PromptDenialReplacementAuth rejects replacement without authority.
	PromptDenialReplacementAuth PromptDenialCode = "replacement_not_authorized"
	// PromptDenialDeliveryUnsupported rejects a required unsupported channel.
	PromptDenialDeliveryUnsupported PromptDenialCode = "delivery_unsupported"
	// PromptDenialDowngradeUnauthorized rejects an unapproved alternate.
	PromptDenialDowngradeUnauthorized PromptDenialCode = "downgrade_not_authorized"
	// PromptDenialApplicationFailed rejects failed receipt persistence.
	PromptDenialApplicationFailed PromptDenialCode = "application_failed"
)

// PromptAdaptationError is returned before any provider process is started.
type PromptAdaptationError struct {
	Code    PromptDenialCode
	Channel PromptChannel
	Detail  string
}

func (e *PromptAdaptationError) Error() string {
	return fmt.Sprintf("prompt adaptation denied (%s, channel=%s): %s", e.Code, e.Channel, e.Detail)
}

// PromptModeForSpec derives the mode without hard-coding harness identities.
func PromptModeForSpec(spec Spec) PromptSessionMode {
	if spec.Interactive != nil {
		return PromptModeHumanControlled
	}
	return PromptModeAutonomous
}

// PromptProfile returns the exact mode profile or false when the manifest did
// not declare one.
func (m HarnessManifest) PromptProfile(mode PromptSessionMode) (PromptDeliveryProfile, bool) {
	for _, profile := range m.PromptDelivery {
		if profile.Mode == mode {
			return profile, true
		}
	}
	return PromptDeliveryProfile{}, false
}

// EnsurePromptPlan projects legacy Spec fields losslessly into the typed plan.
// It is also used by harnesses that add their own user amendment before
// compilation. The legacy projection never invents downgrade authority.
func EnsurePromptPlan(spec Spec) PromptPlan {
	if spec.PromptPlan != nil {
		return *spec.PromptPlan
	}
	plan := PromptPlan{
		ContractVersion:  PromptContractVersion,
		BaseInstructions: BaseInstructionPlan{Strategy: BaseInstructionsPreserve},
		UserPrompt:       PromptContent{ID: "legacy-user-prompt", Text: spec.Prompt, Required: strings.TrimSpace(spec.Prompt) != ""},
	}
	if spec.SystemPromptAppend != "" {
		plan.HarnessProtocol = &PromptContent{ID: "legacy-system-prompt-append", Text: spec.SystemPromptAppend, Required: true}
	}
	if spec.BaseInstructions != "" {
		plan.BaseInstructions = BaseInstructionPlan{
			Strategy: BaseInstructionsAppend,
			Content:  &PromptContent{ID: "legacy-base-instructions", Text: spec.BaseInstructions, Required: true},
		}
	}
	if spec.InitialContext != "" {
		plan.InitialContext = []PromptContent{{ID: "legacy-initial-context", Text: spec.InitialContext, Required: true}}
	}
	return plan
}

// PreparePrompt validates and compiles the prompt plan onto one exact native
// harness profile. It invokes OnPromptAdapted with both ready and denied
// receipts so a caller can persist the pre-spawn evidence.
func PreparePrompt(spec Spec, manifest HarnessManifest) (Spec, error) {
	profile, ok := manifest.PromptProfile(PromptModeForSpec(spec))
	if !ok {
		receipt := PromptDeliveryReceipt{ContractVersion: PromptContractVersion, Decision: "denied"}
		err := &PromptAdaptationError{Code: PromptDenialDeliveryUnsupported, Detail: "manifest has no prompt profile for requested session mode"}
		_ = emitPromptReceipt(spec, receipt)
		return spec, err
	}
	adapted, receipt, err := AdaptPrompt(spec, profile)
	if err != nil {
		_ = emitPromptReceipt(spec, receipt)
		return spec, err
	}
	if receiptErr := emitPromptReceipt(spec, receipt); receiptErr != nil {
		return spec, &PromptAdaptationError{Code: PromptDenialApplicationFailed, Detail: "persist prompt receipt: " + receiptErr.Error()}
	}
	adapted.PromptReceipt = &receipt
	return adapted, nil
}

func emitPromptReceipt(spec Spec, receipt PromptDeliveryReceipt) error {
	if spec.OnPromptAdapted != nil {
		return spec.OnPromptAdapted(receipt)
	}
	return nil
}

// AdaptPrompt is the pure conformance compiler used by provider adapters and
// exact-wire tests.
func AdaptPrompt(spec Spec, profile PromptDeliveryProfile) (Spec, PromptDeliveryReceipt, error) {
	plan := EnsurePromptPlan(spec)
	receipt := PromptDeliveryReceipt{
		ContractVersion: PromptContractVersion,
		ProfileID:       profile.ID,
		Decision:        "denied",
	}
	deny := func(code PromptDenialCode, channel PromptChannel, id string, required bool, detail string) (Spec, PromptDeliveryReceipt, error) {
		receipt.Entries = append(receipt.Entries, PromptDeliveryEntry{ID: id, Channel: channel, Required: required, Outcome: PromptOutcomeDenied, DenialCode: code})
		return spec, receipt, &PromptAdaptationError{Code: code, Channel: channel, Detail: detail}
	}

	if plan.ContractVersion != PromptContractVersion {
		return deny(PromptDenialUnsupportedContract, "", "prompt-plan", true, fmt.Sprintf("got %q", plan.ContractVersion))
	}
	if !validPromptProfile(profile) {
		return deny(PromptDenialMalformedPlan, "", "prompt-profile", true, "profile is incomplete")
	}
	if detail := validatePromptPlan(plan); detail != "" {
		return deny(PromptDenialMalformedPlan, "", "prompt-plan", true, detail)
	}

	var systemParts, contextParts, downgradedPrepend []string
	var baseInstructions string
	if plan.HarnessProtocol != nil && strings.TrimSpace(plan.HarnessProtocol.Text) != "" {
		var err error
		systemParts, receipt, err = applyPromptContent(systemParts, &downgradedPrepend, receipt, *plan.HarnessProtocol, PromptChannelHarnessProtocol, profile.SystemDelivery, plan.AuthorizedDowngrades)
		if err != nil {
			return spec, receipt, err
		}
	}

	switch plan.BaseInstructions.Strategy {
	case BaseInstructionsPreserve:
		if plan.BaseInstructions.Content != nil || plan.BaseInstructions.ReplacementAuthorizationID != "" {
			return deny(PromptDenialMalformedPlan, PromptChannelBaseInstructions, "base-instructions", true, "preserve forbids content and replacement authorization")
		}
		receipt.Entries = append(receipt.Entries, PromptDeliveryEntry{ID: "base-instructions", Channel: PromptChannelBaseInstructions, Outcome: PromptOutcomePreserved, Delivery: PromptDeliveryPreserveExisting, BaseInstructionStrategy: BaseInstructionsPreserve})
	case BaseInstructionsAppend, BaseInstructionsReplace:
		content := plan.BaseInstructions.Content
		if content == nil || strings.TrimSpace(content.Text) == "" {
			return deny(PromptDenialMalformedPlan, PromptChannelBaseInstructions, "base-instructions", true, "append/replace requires content")
		}
		if plan.BaseInstructions.Strategy == BaseInstructionsReplace {
			if plan.BaseInstructions.ReplacementAuthorizationID == "" {
				return deny(PromptDenialReplacementAuth, PromptChannelBaseInstructions, content.ID, content.Required, "replace requires an explicit authorization reference")
			}
			if profile.BaseReplaceDelivery == PromptDeliveryUnsupported {
				return deny(PromptDenialUnsupportedStrategy, PromptChannelBaseInstructions, content.ID, content.Required, "exact harness profile does not support base replacement")
			}
		}
		delivery := profile.BaseAppendDelivery
		if plan.BaseInstructions.Strategy == BaseInstructionsReplace {
			delivery = profile.BaseReplaceDelivery
		}
		if delivery == PromptDeliveryCodexBaseInstructions {
			baseInstructions = content.Text
			receipt.Entries = append(receipt.Entries, deliveredEntry(*content, PromptChannelBaseInstructions, delivery))
		} else {
			var err error
			systemParts, receipt, err = applyPromptContent(systemParts, &downgradedPrepend, receipt, *content, PromptChannelBaseInstructions, delivery, plan.AuthorizedDowngrades)
			if err != nil {
				return spec, receipt, err
			}
		}
		entry := &receipt.Entries[len(receipt.Entries)-1]
		entry.BaseInstructionStrategy = plan.BaseInstructions.Strategy
		entry.ReplacementAuthorizationID = plan.BaseInstructions.ReplacementAuthorizationID
	default:
		return deny(PromptDenialUnsupportedStrategy, PromptChannelBaseInstructions, "base-instructions", true, fmt.Sprintf("unknown strategy %q", plan.BaseInstructions.Strategy))
	}

	if plan.RoleIntent != nil && strings.TrimSpace(plan.RoleIntent.Text) != "" {
		var err error
		systemParts, receipt, err = applyPromptContent(systemParts, &downgradedPrepend, receipt, *plan.RoleIntent, PromptChannelRoleIntent, profile.SystemDelivery, plan.AuthorizedDowngrades)
		if err != nil {
			return spec, receipt, err
		}
	}

	for _, content := range plan.InitialContext {
		if strings.TrimSpace(content.Text) == "" {
			continue
		}
		switch profile.ContextDelivery {
		case PromptDeliveryCodexTurnInput:
			contextParts = append(contextParts, content.Text)
			receipt.Entries = append(receipt.Entries, deliveredEntry(content, PromptChannelInitialContext, profile.ContextDelivery))
		case PromptDeliveryCodexPTYSeed:
			downgradedPrepend = append(downgradedPrepend, content.Text)
			receipt.Entries = append(receipt.Entries, deliveredEntry(content, PromptChannelInitialContext, profile.ContextDelivery))
		case PromptDeliveryUnsupported:
			auth, found := findDowngrade(plan.AuthorizedDowngrades, PromptChannelInitialContext)
			if !found {
				if content.Required {
					return deny(PromptDenialDeliveryUnsupported, PromptChannelInitialContext, content.ID, true, "required initial context has no native delivery or authorized downgrade")
				}
				receipt.Entries = append(receipt.Entries, PromptDeliveryEntry{ID: content.ID, Channel: PromptChannelInitialContext, Required: false, Outcome: PromptOutcomeDenied, DenialCode: PromptDenialDeliveryUnsupported, ContentDigest: digestText(content.Text)})
				continue
			}
			downgradedPrepend = append(downgradedPrepend, content.Text)
			receipt.Entries = append(receipt.Entries, downgradedEntry(content, PromptChannelInitialContext, auth.ID))
		default:
			systemParts = append(systemParts, content.Text)
			receipt.Entries = append(receipt.Entries, deliveredEntry(content, PromptChannelInitialContext, profile.ContextDelivery))
		}
	}

	if strings.TrimSpace(plan.UserPrompt.Text) == "" && plan.UserPrompt.Required {
		return deny(PromptDenialMalformedPlan, PromptChannelUserPrompt, plan.UserPrompt.ID, true, "required user prompt is empty")
	}
	if profile.UserDelivery == PromptDeliveryUnsupported && strings.TrimSpace(plan.UserPrompt.Text) != "" {
		return deny(PromptDenialDeliveryUnsupported, PromptChannelUserPrompt, plan.UserPrompt.ID, plan.UserPrompt.Required, "user prompt delivery is unsupported")
	}

	amendments := append([]UserPromptAmendment(nil), plan.UserAmendments...)
	sort.SliceStable(amendments, func(i, j int) bool {
		if amendments[i].Order != amendments[j].Order {
			return amendments[i].Order < amendments[j].Order
		}
		return amendments[i].ID < amendments[j].ID
	})
	var prepends, appends []string
	for _, amendment := range amendments {
		if amendment.ID == "" || amendment.Content.ID == "" || amendment.Position != UserPromptPrepend && amendment.Position != UserPromptAppend {
			return deny(PromptDenialMalformedPlan, PromptChannelUserAmendment, amendment.ID, amendment.Content.Required, "amendment id, content id, and position are required")
		}
		if profile.AmendmentDelivery == PromptDeliveryUnsupported {
			if amendment.Content.Required {
				return deny(PromptDenialDeliveryUnsupported, PromptChannelUserAmendment, amendment.ID, true, "required user amendment delivery is unsupported")
			}
			receipt.Entries = append(receipt.Entries, PromptDeliveryEntry{ID: amendment.ID, Channel: PromptChannelUserAmendment, Outcome: PromptOutcomeDenied, DenialCode: PromptDenialDeliveryUnsupported, ContentDigest: digestText(amendment.Content.Text)})
			continue
		}
		if amendment.Position == UserPromptPrepend {
			prepends = append(prepends, amendment.Content.Text)
		} else {
			appends = append(appends, amendment.Content.Text)
		}
		receipt.Entries = append(receipt.Entries, deliveredEntry(PromptContent{ID: amendment.ID, Text: amendment.Content.Text, Required: amendment.Content.Required}, PromptChannelUserAmendment, profile.AmendmentDelivery))
	}

	prepends = append(downgradedPrepend, prepends...)
	userParts := append([]string{}, prepends...)
	userParts = append(userParts, plan.UserPrompt.Text)
	userParts = append(userParts, appends...)
	userText := joinPromptParts(userParts)

	if len(contextParts) > 0 {
		spec.InitialContext = joinPromptParts(contextParts)
	} else {
		spec.InitialContext = ""
	}
	spec.SystemPromptAppend = joinPromptParts(systemParts)
	spec.BaseInstructions = baseInstructions
	spec.Prompt = userText
	spec.PromptPlan = &plan
	if strings.TrimSpace(plan.UserPrompt.Text) != "" {
		receipt.Entries = append(receipt.Entries, deliveredEntry(plan.UserPrompt, PromptChannelUserPrompt, profile.UserDelivery))
	}
	receipt.Decision = "ready"
	return spec, receipt, nil
}

func applyPromptContent(parts []string, downgraded *[]string, receipt PromptDeliveryReceipt, content PromptContent, channel PromptChannel, delivery PromptDeliveryKind, auths []PromptDowngradeAuthorization) ([]string, PromptDeliveryReceipt, error) {
	if delivery != PromptDeliveryUnsupported {
		parts = append(parts, content.Text)
		receipt.Entries = append(receipt.Entries, deliveredEntry(content, channel, delivery))
		return parts, receipt, nil
	}
	auth, found := findDowngrade(auths, channel)
	if found {
		*downgraded = append(*downgraded, content.Text)
		receipt.Entries = append(receipt.Entries, downgradedEntry(content, channel, auth.ID))
		return parts, receipt, nil
	}
	if !content.Required {
		receipt.Entries = append(receipt.Entries, PromptDeliveryEntry{ID: content.ID, Channel: channel, Outcome: PromptOutcomeDenied, DenialCode: PromptDenialDeliveryUnsupported, ContentDigest: digestText(content.Text)})
		return parts, receipt, nil
	}
	receipt.Entries = append(receipt.Entries, PromptDeliveryEntry{ID: content.ID, Channel: channel, Required: true, Outcome: PromptOutcomeDenied, DenialCode: PromptDenialDeliveryUnsupported, ContentDigest: digestText(content.Text)})
	return parts, receipt, &PromptAdaptationError{Code: PromptDenialDeliveryUnsupported, Channel: channel, Detail: fmt.Sprintf("required prompt content %q has no native delivery or authorized downgrade", content.ID)}
}

func findDowngrade(auths []PromptDowngradeAuthorization, channel PromptChannel) (PromptDowngradeAuthorization, bool) {
	for _, auth := range auths {
		if auth.ID != "" && auth.Channel == channel && auth.To == PromptChannelUserPrompt {
			return auth, true
		}
	}
	return PromptDowngradeAuthorization{}, false
}

func validPromptProfile(profile PromptDeliveryProfile) bool {
	if profile.ID == "" || profile.Mode != PromptModeAutonomous && profile.Mode != PromptModeHumanControlled {
		return false
	}
	for _, delivery := range []PromptDeliveryKind{
		profile.SystemDelivery,
		profile.BaseAppendDelivery,
		profile.BaseReplaceDelivery,
		profile.ContextDelivery,
		profile.UserDelivery,
		profile.AmendmentDelivery,
	} {
		if !knownPromptDelivery(delivery) {
			return false
		}
	}
	return true
}

func knownPromptDelivery(delivery PromptDeliveryKind) bool {
	switch delivery {
	case PromptDeliveryUnsupported,
		PromptDeliveryPreserveExisting,
		PromptDeliveryClaudeSystemAppend,
		PromptDeliveryClaudeUserStdin,
		PromptDeliveryClaudePTYSeed,
		PromptDeliveryCodexBaseInstructions,
		PromptDeliveryCodexDeveloperInstructions,
		PromptDeliveryCodexTurnInput,
		PromptDeliveryCodexCLIInstructions,
		PromptDeliveryCodexPTYSeed,
		PromptDeliveryGeminiSystemInstruction,
		PromptDeliveryGeminiUserContent,
		PromptDeliveryOllamaSystemMessage,
		PromptDeliveryOllamaUserMessage,
		PromptDeliveryAmpStdin,
		PromptDeliveryAgyPromptFlag,
		PromptDeliveryOpenCodePrompt,
		PromptDeliveryPiSystemAppend,
		PromptDeliveryPiRPCPrompt,
		PromptDeliveryShellPTYSeed,
		PromptDeliveryAuthorizedUserDowngrade:
		return true
	default:
		return false
	}
}

func validatePromptPlan(plan PromptPlan) string {
	validateContent := func(content *PromptContent, label string) string {
		if content == nil {
			return ""
		}
		if content.ID == "" {
			return label + " requires a stable id"
		}
		if strings.TrimSpace(content.Text) == "" && content.Required {
			return label + " is required but empty"
		}
		return ""
	}
	if detail := validateContent(plan.HarnessProtocol, "harness protocol"); detail != "" {
		return detail
	}
	if detail := validateContent(plan.BaseInstructions.Content, "base instructions"); detail != "" {
		return detail
	}
	if detail := validateContent(plan.RoleIntent, "role intent"); detail != "" {
		return detail
	}
	for i := range plan.InitialContext {
		if detail := validateContent(&plan.InitialContext[i], "initial context"); detail != "" {
			return detail
		}
	}
	if detail := validateContent(&plan.UserPrompt, "user prompt"); detail != "" {
		return detail
	}

	seenAmendments := make(map[string]bool, len(plan.UserAmendments))
	for i := range plan.UserAmendments {
		amendment := &plan.UserAmendments[i]
		if amendment.ID == "" || seenAmendments[amendment.ID] {
			return "user amendments require unique stable ids"
		}
		seenAmendments[amendment.ID] = true
		if amendment.Position != UserPromptPrepend && amendment.Position != UserPromptAppend {
			return "user amendment has an unknown position"
		}
		if detail := validateContent(&amendment.Content, "user amendment content"); detail != "" {
			return detail
		}
	}

	seenDowngrades := make(map[string]bool, len(plan.AuthorizedDowngrades))
	for _, auth := range plan.AuthorizedDowngrades {
		if auth.ID == "" || seenDowngrades[auth.ID] {
			return "downgrade authorizations require unique stable ids"
		}
		seenDowngrades[auth.ID] = true
		switch auth.Channel {
		case PromptChannelHarnessProtocol, PromptChannelBaseInstructions, PromptChannelRoleIntent, PromptChannelInitialContext:
		default:
			return "downgrade authorization has an unknown source channel"
		}
		if auth.To != PromptChannelUserPrompt {
			return "downgrade authorization target must be user_prompt"
		}
	}
	return ""
}

func deliveredEntry(content PromptContent, channel PromptChannel, delivery PromptDeliveryKind) PromptDeliveryEntry {
	return PromptDeliveryEntry{ID: content.ID, Channel: channel, Required: content.Required, Outcome: PromptOutcomeDelivered, Delivery: delivery, ContentDigest: digestText(content.Text)}
}

func downgradedEntry(content PromptContent, channel PromptChannel, authID string) PromptDeliveryEntry {
	return PromptDeliveryEntry{ID: content.ID, Channel: channel, Required: content.Required, Outcome: PromptOutcomeDowngraded, Delivery: PromptDeliveryAuthorizedUserDowngrade, ContentDigest: digestText(content.Text), DowngradeAuthID: authID}
}

func joinPromptParts(parts []string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			clean = append(clean, strings.TrimSpace(part))
		}
	}
	return strings.Join(clean, "\n\n")
}

func digestText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// IsPromptAdaptationError lets provider tests assert typed denial without
// coupling to wrapped error strings.
func IsPromptAdaptationError(err error, code PromptDenialCode) bool {
	var target *PromptAdaptationError
	return errors.As(err, &target) && target.Code == code
}
