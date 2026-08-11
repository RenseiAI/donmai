package runner

import (
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

// watchLifecycleEvents projects one admitted `watch` capability onto normalized
// lifecycle events. The session boundary — the init event and the terminal
// result — is the floor every declared harness profile carries, so it stays
// required: a mode that cannot evidence even that is not watchable and denies.
// The richer per-turn and per-tool evidence is requested but optional, because
// harness profiles carry genuinely different event vocabularies and a mode "may
// not inherit headless evidence"
// (ADR-2026-08-06-harness-adaptation-plan-and-receipt.md D6). Requiring the full
// structured vocabulary of every mode would deny admission to interactive PTY
// and completion-only profiles for evidence they never claimed, instead of
// recording the gap where the contract puts it: an optional denial on the
// receipt, which stays truthful and visible (D4).
var watchLifecycleEvents = []struct {
	kind     agent.EventKind
	required bool
}{
	{agent.EventInit, true},
	{agent.EventResult, true},
	{agent.EventAssistantText, false},
	{agent.EventToolUse, false},
	{agent.EventToolResult, false},
	{agent.EventError, false},
}

// bindAdmissionToolLifecyclePlan links the upstream execution-cell admission
// to the existing exact-harness tool/lifecycle compiler. Receipt-bearing work
// is additive: legacy work without a receipt keeps the pre-existing plan.
func bindAdmissionToolLifecyclePlan(spec agent.Spec, receipt executioncell.ImmutableAdmissionReceipt, claim executioncell.ImmutableClaimReceipt) (agent.Spec, error) {
	if len(receipt.Bytes()) == 0 {
		return spec, nil
	}
	value := receipt.Value()
	if value.Decision != executioncell.AdmissionAdmitted || value.Cell == nil {
		return spec, &agent.ToolAdaptationError{
			Code:   agent.ToolDenialMalformedPlan,
			Detail: "tool/lifecycle adaptation requires an admitted execution-cell receipt",
		}
	}

	plan := agent.EnsureToolLifecyclePlan(spec)
	if plan.AdmissionReceiptID != "" && plan.AdmissionReceiptID != value.ReceiptID {
		return spec, &agent.ToolAdaptationError{
			Code:   agent.ToolDenialMalformedPlan,
			Detail: "tool/lifecycle plan is already bound to a different admission receipt",
		}
	}
	if plan.OperationalPayloadDigest != "" && plan.OperationalPayloadDigest != value.OperationalPayloadDigest {
		return spec, &agent.ToolAdaptationError{
			Code:   agent.ToolDenialMalformedPlan,
			Detail: "tool/lifecycle plan is already bound to a different operational payload",
		}
	}
	plan.AdmissionReceiptID = value.ReceiptID
	plan.OperationalPayloadDigest = value.OperationalPayloadDigest
	if len(claim.Bytes()) != 0 {
		claimValue := claim.Value()
		if claimValue.AdmissionReceiptID != value.ReceiptID || claimValue.Decision != executioncell.ClaimClaimed {
			return spec, &agent.ToolAdaptationError{
				Code:   agent.ToolDenialMalformedPlan,
				Detail: "tool/lifecycle adaptation requires claim evidence bound to the admission receipt",
			}
		}
		if plan.ClaimReceiptID != "" && plan.ClaimReceiptID != claimValue.ClaimReceiptID {
			return spec, &agent.ToolAdaptationError{
				Code:   agent.ToolDenialMalformedPlan,
				Detail: "tool/lifecycle plan is already bound to a different claim receipt",
			}
		}
		plan.ClaimReceiptID = claimValue.ClaimReceiptID
	}

	minimumFidelity := agent.EvidenceStructured
	if value.Cell.SessionMode == executioncell.SessionHumanControlled {
		minimumFidelity = agent.EvidenceCoarse
	}
	for _, capability := range value.Cell.GrantedCapabilities {
		switch capability.Name {
		case "watch":
			for _, event := range watchLifecycleEvents {
				plan.Lifecycle = append(plan.Lifecycle, agent.LifecycleRequirement{
					ID:               fmt.Sprintf("execution-cell-watch-%s", event.kind),
					Event:            event.kind,
					Required:         event.required,
					MinimumFidelity:  minimumFidelity,
					ParametersDigest: capability.ParametersDigest,
				})
			}
		case "replay":
			plan.Replay = &agent.LifecycleRequirement{
				ID:               "execution-cell-replay",
				Event:            agent.EventResult,
				Required:         true,
				MinimumFidelity:  minimumFidelity,
				ParametersDigest: capability.ParametersDigest,
			}
		case "cancel":
			plan.RequireCleanup = true
			plan.CleanupParametersDigest = capability.ParametersDigest
		default:
			// Preflight rejects unknown capability names. Keep a defensive
			// denial here so no alternate caller can silently strip one.
			return spec, &agent.ToolAdaptationError{
				Code:    agent.ToolDenialDeliveryUnsupported,
				Channel: agent.ToolChannelLifecycle,
				Detail:  fmt.Sprintf("admitted capability %q has no exact lifecycle projection", capability.Name),
			}
		}
	}
	spec.ToolLifecyclePlan = &plan
	return spec, nil
}
