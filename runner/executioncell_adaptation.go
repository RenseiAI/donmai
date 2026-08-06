package runner

import (
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

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
			for _, event := range []agent.EventKind{
				agent.EventInit,
				agent.EventAssistantText,
				agent.EventToolUse,
				agent.EventToolResult,
				agent.EventResult,
				agent.EventError,
			} {
				plan.Lifecycle = append(plan.Lifecycle, agent.LifecycleRequirement{
					ID:               fmt.Sprintf("execution-cell-watch-%s", event),
					Event:            event,
					Required:         true,
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
