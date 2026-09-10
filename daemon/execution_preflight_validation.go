package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

// validateExecutionPreflightReceiptAuthority is the v2-only host/controller
// join. The shared codec proves the complete prepared plan and nested receipt
// bytes; this daemon check binds those facts to the actual admission, claim,
// effective cell and operational payload that arrived on the trusted poll.
func validateExecutionPreflightReceiptAuthority(detail *SessionDetail, binding executioncell.RuntimeBinding, receipt json.RawMessage, operationalDigest string) error {
	if err := executioncell.ValidateHostAdaptationForRegistration(binding, receipt, operationalDigest); err != nil {
		return err
	}
	admitted, err := executioncell.DecodeAdmissionReceipt(detail.AdmissionReceipt)
	if err != nil {
		return fmt.Errorf("decode registered execution admission: %w", err)
	}
	admission := admitted.Value()
	if admission.Decision != executioncell.AdmissionAdmitted || admission.Cell == nil || admission.RequestID != binding.RequestID || admission.OperationalPayloadDigest != operationalDigest {
		return errors.New("registered host adaptation does not match the admitted operational request")
	}
	effective, err := executioncell.DecodeResolvedExecutionCell(detail.EffectiveCell)
	if err != nil {
		return fmt.Errorf("decode registered execution effective cell: %w", err)
	}
	wantClaimReceiptID := ""
	if len(detail.ClaimReceipt) == 0 {
		if !reflect.DeepEqual(*admission.Cell, effective) {
			return errors.New("registered host adaptation effective cell changed the exact admission")
		}
	} else {
		claim, claimErr := executioncell.DecodeClaimReceipt(detail.ClaimReceipt)
		if claimErr != nil {
			return fmt.Errorf("decode registered execution claim: %w", claimErr)
		}
		if claimErr = executioncell.AssertNarrowClaim(admitted, claim); claimErr != nil {
			return fmt.Errorf("registered execution claim does not narrow admission: %w", claimErr)
		}
		claimValue := claim.Value()
		if claimValue.Decision != executioncell.ClaimClaimed || claimValue.EffectiveCell == nil || !reflect.DeepEqual(*claimValue.EffectiveCell, effective) {
			return errors.New("registered host adaptation effective cell does not match the active claim")
		}
		wantClaimReceiptID = claimValue.ClaimReceiptID
	}
	host, _ := executioncell.DecodeHostAdaptationReceipt(receipt)
	var plan agent.PreparedHarness
	var tools agent.ToolLifecycleReceipt
	if err := json.Unmarshal(host.Plan, &plan); err != nil {
		return err
	}
	if err := json.Unmarshal(host.ToolLifecycleReceipt, &tools); err != nil {
		return err
	}
	wantMode := agent.PromptModeAutonomous
	if admission.Cell.SessionMode == executioncell.SessionHumanControlled {
		wantMode = agent.PromptModeHumanControlled
	}
	if plan.Harness != admission.Cell.Harness.ID || plan.Mode != wantMode {
		return errors.New("registered prepared harness does not match admitted harness and mode")
	}
	if tools.AdmissionReceiptID != admission.ReceiptID || tools.ClaimReceiptID != wantClaimReceiptID || tools.OperationalPayloadDigest != admission.OperationalPayloadDigest {
		return errors.New("registered tool lifecycle receipt does not match admission and claim")
	}
	return nil
}
