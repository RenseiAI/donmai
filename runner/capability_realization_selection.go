package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

// ProtectedRuntimeMCPHistoricalAbsenceMode is the closed process-composition
// choice for targeted retained work that predates explicit selection.
type ProtectedRuntimeMCPHistoricalAbsenceMode string

const (
	// ProtectedRuntimeMCPHistoricalAbsenceRefuse is the default. Targeted work
	// without an explicit selection refuses.
	ProtectedRuntimeMCPHistoricalAbsenceRefuse ProtectedRuntimeMCPHistoricalAbsenceMode = ""
	// ProtectedRuntimeMCPHistoricalAbsenceV1 selects only the configured V1
	// row. The operator's history-closure evidence remains an activation gate;
	// this enum is not a runtime certificate.
	ProtectedRuntimeMCPHistoricalAbsenceV1 ProtectedRuntimeMCPHistoricalAbsenceMode = "historical_v1"
)

// ProtectedRuntimeMCPDualSelectionPolicy is immutable process configuration
// for the two exact realizations one process may consume concurrently.
type ProtectedRuntimeMCPDualSelectionPolicy struct {
	V1                ProtectedRuntimeMCPSelector
	V2                ProtectedRuntimeMCPV2Selector
	HistoricalAbsence ProtectedRuntimeMCPHistoricalAbsenceMode
}

func (p ProtectedRuntimeMCPDualSelectionPolicy) configured() bool {
	return p.V1.configured() || p.V2.configured()
}

func validateProtectedRuntimeMCPDualSelectionPolicy(p ProtectedRuntimeMCPDualSelectionPolicy, realizations *agent.CapabilityRealizationRegistry) error {
	if !p.configured() {
		if p.HistoricalAbsence != ProtectedRuntimeMCPHistoricalAbsenceRefuse {
			return errors.New("runner: historical protected runtime MCP absence mode requires a dual policy")
		}
		return nil
	}
	if !p.V1.configured() || !p.V2.configured() {
		return errors.New("runner: protected runtime MCP dual policy requires V1 and V2 selectors")
	}
	if p.HistoricalAbsence != ProtectedRuntimeMCPHistoricalAbsenceRefuse && p.HistoricalAbsence != ProtectedRuntimeMCPHistoricalAbsenceV1 {
		return fmt.Errorf("runner: unsupported protected runtime MCP historical absence mode %q", p.HistoricalAbsence)
	}
	if err := validateProtectedRuntimeMCPSelector(p.V1, realizations); err != nil {
		return err
	}
	if err := validateProtectedRuntimeMCPV2Selector(p.V2, realizations); err != nil {
		return err
	}
	if p.V1.CapabilityID != p.V2.CapabilityID || p.V1.HarnessID != p.V2.HarnessID || p.V1.Mode != p.V2.Mode || p.V1.AdapterProfileID == p.V2.AdapterProfileID {
		return errors.New("runner: protected runtime MCP dual policy selectors are incompatible")
	}
	return nil
}

type protectedRuntimeMCPSelectionPolicy struct {
	v1         ProtectedRuntimeMCPSelector
	v2         ProtectedRuntimeMCPV2Selector
	historical ProtectedRuntimeMCPHistoricalAbsenceMode
	dual       bool
}

func newProtectedRuntimeMCPSelectionPolicy(v1 ProtectedRuntimeMCPSelector, v2 ProtectedRuntimeMCPV2Selector, dual ProtectedRuntimeMCPDualSelectionPolicy, realizations *agent.CapabilityRealizationRegistry) (protectedRuntimeMCPSelectionPolicy, error) {
	if dual.configured() || dual.HistoricalAbsence != ProtectedRuntimeMCPHistoricalAbsenceRefuse {
		if v1.configured() || v2.configured() {
			return protectedRuntimeMCPSelectionPolicy{}, errors.New("runner: protected runtime MCP dual policy cannot mix with legacy selectors")
		}
		if err := validateProtectedRuntimeMCPDualSelectionPolicy(dual, realizations); err != nil {
			return protectedRuntimeMCPSelectionPolicy{}, err
		}
		return protectedRuntimeMCPSelectionPolicy{v1: dual.V1, v2: dual.V2, historical: dual.HistoricalAbsence, dual: true}, nil
	}
	if v1.configured() && v2.configured() {
		return protectedRuntimeMCPSelectionPolicy{}, errors.New("runner: protected runtime MCP selector version is ambiguous")
	}
	if err := validateProtectedRuntimeMCPSelector(v1, realizations); err != nil {
		return protectedRuntimeMCPSelectionPolicy{}, err
	}
	if err := validateProtectedRuntimeMCPV2Selector(v2, realizations); err != nil {
		return protectedRuntimeMCPSelectionPolicy{}, err
	}
	return protectedRuntimeMCPSelectionPolicy{v1: v1, v2: v2}, nil
}

func (p protectedRuntimeMCPSelectionPolicy) target(selection executioncell.CapabilityRealizationSelectionV1) bool {
	return (p.v1.configured() && selection.CapabilityID == p.v1.CapabilityID && selection.HarnessID == string(p.v1.HarnessID) && selection.Mode == string(p.v1.Mode)) ||
		(p.v2.configured() && selection.CapabilityID == p.v2.CapabilityID && selection.HarnessID == string(p.v2.HarnessID) && selection.Mode == string(p.v2.Mode))
}

func (p protectedRuntimeMCPSelectionPolicy) adapterAllowed(selection executioncell.CapabilityRealizationSelectionV1) bool {
	return (p.v1.configured() && selection.CapabilityID == p.v1.CapabilityID && selection.HarnessID == string(p.v1.HarnessID) && selection.Mode == string(p.v1.Mode) && selection.AdapterVersion == p.v1.AdapterProfileID) ||
		(p.v2.configured() && selection.CapabilityID == p.v2.CapabilityID && selection.HarnessID == string(p.v2.HarnessID) && selection.Mode == string(p.v2.Mode) && selection.AdapterVersion == p.v2.AdapterProfileID)
}

func selectionMode(cell executioncell.ResolvedExecutionCell) agent.PromptSessionMode {
	if cell.SessionMode == executioncell.SessionHumanControlled {
		return agent.PromptModeHumanControlled
	}
	return agent.PromptModeAutonomous
}

func capabilityGrantCount(cell executioncell.ResolvedExecutionCell, capability string) int {
	count := 0
	for _, grant := range cell.GrantedCapabilities {
		if grant.Name == capability {
			count++
		}
	}
	return count
}

func requireSelectedToolLifecycleProfile(qw QueuedWork, profileID string, requireHostAdaptation bool) error {
	if !requireHostAdaptation {
		return nil
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(qw.HostAdaptationReceipt)
	if err != nil {
		return err
	}
	var tool agent.ToolLifecycleReceipt
	if err := json.Unmarshal(host.ToolLifecycleReceipt, &tool); err != nil {
		return errors.New("runner: selected capability realization host tool receipt is malformed")
	}
	if tool.ProfileID != profileID {
		return errors.New("runner: selected capability realization differs from host tool profile")
	}
	return nil
}

// bindCapabilityRealizationSelection independently establishes receipt, digest,
// claim, runtime-binding and effective-cell authority before it reads the raw
// selection. It returns a copied work item with only the private profile id
// changed.
func bindCapabilityRealizationSelection(qw QueuedWork, realizations *agent.CapabilityRealizationRegistry, policy protectedRuntimeMCPSelectionPolicy, requireHostAdaptation bool) (QueuedWork, error) {
	if len(bytes.TrimSpace(qw.OperationalPayload)) == 0 && !policy.v1.configured() && !policy.v2.configured() {
		return qw, nil
	}
	selection, err := executioncell.ExtractCapabilityRealizationSelectionV1(qw.OperationalPayload)
	if err != nil {
		return QueuedWork{}, err
	}
	if selection == nil && !policy.v1.configured() && !policy.v2.configured() {
		return qw, nil
	}
	if len(qw.AdmissionReceipt) == 0 {
		if selection != nil {
			return QueuedWork{}, errors.New("runner: explicit capability realization selection requires an admission receipt")
		}
		return qw, nil
	}
	admission, err := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
	if err != nil {
		return QueuedWork{}, err
	}
	receipt := admission.Value()
	if receipt.Decision != executioncell.AdmissionAdmitted || receipt.RequestID != qw.SessionID {
		return QueuedWork{}, errors.New("runner: capability realization selection requires matching admitted receipt")
	}
	digest, err := DigestOperationalPayload(qw)
	if err != nil || digest != receipt.OperationalPayloadDigest {
		return QueuedWork{}, errors.New("runner: capability realization selection operational payload digest mismatch")
	}
	cell, _, _, err := resolveReceiptEffectiveCell(qw, admission, requireHostAdaptation)
	if err != nil {
		return QueuedWork{}, err
	}
	mode := selectionMode(cell)
	if selection == nil {
		if !policy.dual {
			if !policy.v1.configured() && policy.v2.configured() {
				return BindProtectedRuntimeMCPV2ProfileIntent(qw, policy.v2), nil
			}
			return qw, nil
		}
		if !policy.v1.configured() && policy.v2.configured() {
			return BindProtectedRuntimeMCPV2ProfileIntent(qw, policy.v2), nil
		}
		if !policy.v1.configured() {
			return qw, nil
		}
		grants := capabilityGrantCount(cell, policy.v1.CapabilityID)
		if grants == 0 || cell.Harness.ID != string(policy.v1.HarnessID) || mode != policy.v1.Mode {
			return qw, nil
		}
		if grants != 1 {
			return QueuedWork{}, errors.New("runner: protected target capability must be granted exactly once")
		}
		if policy.historical != ProtectedRuntimeMCPHistoricalAbsenceV1 {
			return QueuedWork{}, errors.New("runner: targeted capability realization selection is required")
		}
		qw.toolLifecycleProfileID = policy.v1.AdapterProfileID
		if err := requireSelectedToolLifecycleProfile(qw, qw.toolLifecycleProfileID, requireHostAdaptation); err != nil {
			return QueuedWork{}, err
		}
		return qw, nil
	}
	if capabilityGrantCount(cell, selection.CapabilityID) != 1 || cell.Harness.ID != selection.HarnessID || string(mode) != selection.Mode {
		return QueuedWork{}, errors.New("runner: capability realization selection does not match effective cell")
	}
	compiled, ok := realizations.Resolve(selection.CapabilityID, agent.HarnessName(selection.HarnessID), selection.AdapterVersion, mode)
	if !ok || compiled.Declaration.Recipe.RecipeDigest != selection.RecipeDigest || compiled.Declaration.Recipe.DeclaredSurfaceDigest != selection.DeclaredSurfaceDigest || compiled.Observation.ObservationDigest != selection.ObservationDigest {
		return QueuedWork{}, errors.New("runner: capability realization selection does not match exact local registry evidence")
	}
	if policy.target(*selection) {
		if !policy.adapterAllowed(*selection) {
			return QueuedWork{}, errors.New("runner: capability realization selection adapter is not permitted by this consumer")
		}
	}
	qw.toolLifecycleProfileID = selection.AdapterVersion
	if err := requireSelectedToolLifecycleProfile(qw, qw.toolLifecycleProfileID, requireHostAdaptation); err != nil {
		return QueuedWork{}, err
	}
	return qw, nil
}

// BindProtectedRuntimeMCPSelection independently derives the private runtime
// profile from retained admitted work before a child preflight can bind
// credentials or invoke a provider. It accepts only process configuration.
func BindProtectedRuntimeMCPSelection(qw QueuedWork, realizations *agent.CapabilityRealizationRegistry, v1 ProtectedRuntimeMCPSelector, v2 ProtectedRuntimeMCPV2Selector, dual ProtectedRuntimeMCPDualSelectionPolicy) (QueuedWork, error) {
	policy, err := newProtectedRuntimeMCPSelectionPolicy(v1, v2, dual, realizations)
	if err != nil {
		return QueuedWork{}, err
	}
	return bindCapabilityRealizationSelection(qw, realizations, policy, true)
}

// PreflightHarnessWithProtectedRuntimeMCPSelection binds retained raw
// selection authority before the existing explicit-harness admission path.
// It returns the bound work so callers cannot discard the private profile.
func (r *Registry) PreflightHarnessWithProtectedRuntimeMCPSelection(qw QueuedWork, realizations *agent.CapabilityRealizationRegistry, v1 ProtectedRuntimeMCPSelector, v2 ProtectedRuntimeMCPV2Selector, dual ProtectedRuntimeMCPDualSelectionPolicy) (QueuedWork, *HarnessAdmission, error) {
	bound, err := BindProtectedRuntimeMCPSelection(qw, realizations, v1, v2, dual)
	if err != nil {
		return QueuedWork{}, nil, err
	}
	admission, err := r.PreflightHarness(bound, realizations)
	if err != nil {
		return bound, admission, err
	}
	return bound, admission, nil
}
