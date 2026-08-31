package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

// HarnessAdmissionError is the typed pre-spawn denial returned when an
// explicit harness selector cannot be honored exactly. Code uses the canonical
// execution-cell denial vocabulary. Receipt is populated by preflight or
// runLoop after the canonical denied AdmissionReceipt is built and validated.
type HarnessAdmissionError struct {
	Code      executioncell.AdmissionDenialCode
	Harness   string
	Detail    string
	Decisions []executioncell.ResolverDecision
	Receipt   executioncell.ImmutableAdmissionReceipt
	cause     error
}

func (e *HarnessAdmissionError) Error() string {
	return fmt.Sprintf("harness admission denied (%s, harness=%q): %s", e.Code, e.Harness, e.Detail)
}

func (e *HarnessAdmissionError) Unwrap() error { return e.cause }

// harnessSelection is the single admitted runtime selection carried through a
// run. Provider is the concrete implementation; Harness is the independent
// loop-driver identity. Decisions use the canonical execution-cell resolver
// vocabulary so they can be attached to the full upstream receipt when the
// dispatch contract is threaded end-to-end.
type harnessSelection struct {
	Provider      agent.Provider
	Harness       executioncell.HarnessRef
	Decisions     []executioncell.ResolverDecision
	Explicit      bool
	receipt       executioncell.ImmutableAdmissionReceipt
	claimReceipt  executioncell.ImmutableClaimReceipt
	effectiveCell executioncell.ResolvedExecutionCell
	effectiveJSON []byte
}

type harnessSelectorIntent struct {
	Harness  string
	Provider agent.ProviderName
	Runner   string
}

func selectorIntent(profile ResolvedProfile) harnessSelectorIntent {
	return harnessSelectorIntent{
		Harness: profile.Harness, Provider: profile.Provider, Runner: profile.Runner,
	}
}

// HarnessAdmission is an opaque, side-effect-free explicit-harness preflight
// result. Its private fields bind it to the registry and selector intent that
// produced it, so RunAdmitted can consume the exact admitted provider once
// even if later pre-run setup adds unrelated profile fields such as Endpoint.
type HarnessAdmission struct {
	registry            *Registry
	requestID           string
	intent              harnessSelectorIntent
	selection           harnessSelection
	receipt             executioncell.ImmutableAdmissionReceipt
	denial              error
	denialPayloadDigest string
	consumed            atomic.Bool
}

// CanonicalHarnessRef returns a value copy of the canonical harness identity
// from a successful explicit admission. Reading the projection does not
// consume the one-shot admission and never exposes its mutable Provider. Nil,
// legacy/absent, and denied admissions return false.
func (a *HarnessAdmission) CanonicalHarnessRef() (executioncell.HarnessRef, bool) {
	if a == nil || a.denial != nil || a.selection.Provider == nil || a.selection.Harness.ID == "" {
		return executioncell.HarnessRef{}, false
	}
	return a.selection.Harness, true
}

// PreflightHarness admits explicit harness intent using only the in-memory
// registry. It performs no posterior request, worktree operation, credential
// delivery, gateway binding, provider Spawn, or result/status post. A nil
// admission with nil error means the request omitted Harness and must follow
// the named legacy adapter inside Run. Explicit denials carry a canonical
// immutable denied receipt on HarnessAdmissionError.
func (r *Registry) PreflightHarness(qw QueuedWork) (*HarnessAdmission, error) {
	if len(qw.AdmissionReceipt) > 0 {
		return r.preflightAdmissionReceipt(qw, true)
	}
	if qw.ResolvedProfile.Harness == "" {
		return nil, nil
	}
	selection, err := r.selectExplicitHarness(qw.ResolvedProfile)
	if err != nil {
		denial := attachDeniedHarnessReceipt(qw, err, time.Now())
		payloadDigest, _ := DigestOperationalPayload(qw)
		return &HarnessAdmission{
			registry: r, requestID: qw.SessionID,
			intent: selectorIntent(qw.ResolvedProfile), denial: denial,
			denialPayloadDigest: payloadDigest,
		}, denial
	}
	return &HarnessAdmission{
		registry: r, requestID: qw.SessionID,
		intent: selectorIntent(qw.ResolvedProfile), selection: selection,
	}, nil
}

func (r *Registry) preflightAdmissionReceipt(qw QueuedWork, requireHostAdaptation bool) (*HarnessAdmission, error) {
	immutable, err := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
	if err != nil {
		return deniedHarnessAdmissionToken(r, qw, executioncell.ImmutableAdmissionReceipt{}, err), err
	}
	receipt := immutable.Value()
	if receipt.RequestID != qw.SessionID {
		denial := receiptHarnessDenial(receipt, executioncell.DenialFallbackNotAllowed,
			"admission receipt request does not match queued session")
		attached := attachDeniedHarnessReceipt(qw, denial, time.Now())
		return deniedHarnessAdmissionToken(r, qw, executioncell.ImmutableAdmissionReceipt{}, attached), attached
	}
	payloadDigest, digestErr := DigestOperationalPayload(qw)
	if digestErr != nil {
		return deniedHarnessAdmissionToken(r, qw, executioncell.ImmutableAdmissionReceipt{}, digestErr), digestErr
	}
	if payloadDigest != receipt.OperationalPayloadDigest {
		denial := receiptHarnessDenial(receipt, executioncell.DenialFallbackNotAllowed,
			"queued operational payload does not match admitted digest")
		attached := attachDeniedHarnessReceipt(qw, denial, time.Now())
		return deniedHarnessAdmissionToken(r, qw, executioncell.ImmutableAdmissionReceipt{}, attached), attached
	}
	if receipt.Decision == executioncell.AdmissionDenied {
		denial := &HarnessAdmissionError{
			Code:      receipt.DenialCode,
			Detail:    receipt.DenialDetail,
			Decisions: append([]executioncell.ResolverDecision(nil), receipt.ResolverDecisions...),
			Receipt:   immutable,
		}
		return deniedHarnessAdmissionToken(r, qw, immutable, denial), denial
	}

	effectiveCell, effectiveJSON, claimReceipt, cellErr := resolveReceiptEffectiveCell(qw, immutable, requireHostAdaptation)
	if cellErr != nil {
		cellErr = attachDeniedHarnessReceipt(qw, cellErr, time.Now())
		return deniedHarnessAdmissionToken(r, qw, executioncell.ImmutableAdmissionReceipt{}, cellErr), cellErr
	}
	selection, selectionErr := r.selectReceiptHarness(qw, receipt, effectiveCell)
	if selectionErr != nil {
		selectionErr = attachDeniedHarnessReceipt(qw, selectionErr, time.Now())
		return deniedHarnessAdmissionToken(r, qw, executioncell.ImmutableAdmissionReceipt{}, selectionErr), selectionErr
	}
	selection.receipt = immutable
	selection.claimReceipt = claimReceipt
	selection.effectiveCell = effectiveCell
	selection.effectiveJSON = effectiveJSON
	return &HarnessAdmission{
		registry: r, requestID: qw.SessionID,
		intent: selectorIntent(qw.ResolvedProfile), selection: selection, receipt: immutable,
	}, nil
}

func deniedHarnessAdmissionToken(r *Registry, qw QueuedWork, receipt executioncell.ImmutableAdmissionReceipt, denial error) *HarnessAdmission {
	payloadDigest, _ := DigestOperationalPayload(qw)
	return &HarnessAdmission{
		registry: r, requestID: qw.SessionID,
		intent: selectorIntent(qw.ResolvedProfile), receipt: receipt, denial: denial,
		denialPayloadDigest: payloadDigest,
	}
}

// resolveReceiptEffectiveCell closes the admission-to-runtime placement gap.
// An exact admission is already effective and must not carry claim evidence. A
// claim-bound pool must carry one claimed, narrow-only receipt. In both cases
// the worker independently supplies the secret-free EffectiveCell bytes it is
// about to execute, and those bytes must canonically equal the expected cell.
func resolveReceiptEffectiveCell(qw QueuedWork, admission executioncell.ImmutableAdmissionReceipt, requireHostAdaptation bool) (executioncell.ResolvedExecutionCell, []byte, executioncell.ImmutableClaimReceipt, error) {
	receipt := admission.Value()
	if receipt.Cell == nil {
		return executioncell.ResolvedExecutionCell{}, nil, executioncell.ImmutableClaimReceipt{},
			receiptHarnessDenial(receipt, executioncell.DenialHarnessUnavailable, "admitted receipt does not contain a resolved execution cell")
	}

	expected := *receipt.Cell
	binding, bindingErr := executioncell.DecodeRuntimeBinding(qw.ExecutionRuntimeBinding)
	if bindingErr != nil {
		return executioncell.ResolvedExecutionCell{}, nil, executioncell.ImmutableClaimReceipt{},
			receiptHarnessDenial(receipt, executioncell.DenialPlacementUnsatisfied, "receipt-bearing work requires a valid daemon runtime binding")
	}
	if binding.RequestID != qw.SessionID || binding.WorkerID != qw.WorkerID {
		return executioncell.ResolvedExecutionCell{}, nil, executioncell.ImmutableClaimReceipt{},
			receiptHarnessDenial(receipt, executioncell.DenialPlacementUnsatisfied, "runtime binding is not owned by this request and worker")
	}
	var claim executioncell.ImmutableClaimReceipt
	claimBound := expected.Placement.Kind == executioncell.PlacementPool && expected.Placement.Resolution == executioncell.PlacementClaimBound
	switch {
	case claimBound:
		if len(qw.ClaimReceipt) == 0 {
			return executioncell.ResolvedExecutionCell{}, nil, claim,
				receiptHarnessDenial(receipt, executioncell.DenialPlacementUnsatisfied, "claim-bound admission requires a claim receipt")
		}
		decoded, err := executioncell.DecodeClaimReceipt(qw.ClaimReceipt)
		if err != nil {
			return executioncell.ResolvedExecutionCell{}, nil, claim, &HarnessAdmissionError{
				Code: executioncell.DenialPlacementUnsatisfied, Harness: receiptCellHarnessID(receipt),
				Detail: "claim receipt is not a valid closed execution-cell contract", cause: err,
			}
		}
		if err := executioncell.AssertNarrowClaim(admission, decoded); err != nil {
			return executioncell.ResolvedExecutionCell{}, nil, claim, &HarnessAdmissionError{
				Code: executioncell.DenialPlacementUnsatisfied, Harness: receiptCellHarnessID(receipt),
				Detail: "claim receipt does not narrow the admitted cell", cause: err,
			}
		}
		claimValue := decoded.Value()
		if claimValue.Decision != executioncell.ClaimClaimed || claimValue.EffectiveCell == nil {
			return executioncell.ResolvedExecutionCell{}, nil, claim,
				receiptHarnessDenial(receipt, executioncell.DenialPlacementUnsatisfied, "claim receipt did not produce an effective cell")
		}
		claim = decoded
		expected = *claimValue.EffectiveCell
		if binding.ClaimID != claimValue.ClaimID {
			return executioncell.ResolvedExecutionCell{}, nil, claim,
				receiptHarnessDenial(receipt, executioncell.DenialPlacementUnsatisfied, "claim receipt is not the active claim for this worker")
		}
	case len(qw.ClaimReceipt) != 0:
		return executioncell.ResolvedExecutionCell{}, nil, claim,
			receiptHarnessDenial(receipt, executioncell.DenialUnknownPlacement, "exact admission must not carry a claim receipt")
	case binding.ClaimID != "":
		return executioncell.ResolvedExecutionCell{}, nil, claim,
			receiptHarnessDenial(receipt, executioncell.DenialUnknownPlacement, "exact admission must not carry an active claim id")
	}
	if binding.PlacementID != expected.Placement.ID {
		return executioncell.ResolvedExecutionCell{}, nil, claim,
			receiptHarnessDenial(receipt, executioncell.DenialPlacementUnsatisfied, "effective placement is not the target assigned to this worker")
	}

	if len(qw.EffectiveCell) == 0 {
		return executioncell.ResolvedExecutionCell{}, nil, claim,
			receiptHarnessDenial(receipt, executioncell.DenialPlacementUnsatisfied, "receipt-bearing work requires an explicit effective cell")
	}
	effective, err := executioncell.DecodeResolvedExecutionCell(qw.EffectiveCell)
	if err != nil {
		return executioncell.ResolvedExecutionCell{}, nil, claim, &HarnessAdmissionError{
			Code: executioncell.DenialPlacementUnsatisfied, Harness: receiptCellHarnessID(receipt),
			Detail: "effective cell is not a valid closed execution-cell contract", cause: err,
		}
	}
	effectiveJSON, err := executioncell.CanonicalJSON(effective)
	if err != nil {
		return executioncell.ResolvedExecutionCell{}, nil, claim, err
	}
	expectedJSON, err := executioncell.CanonicalJSON(expected)
	if err != nil {
		return executioncell.ResolvedExecutionCell{}, nil, claim, err
	}
	if !bytes.Equal(effectiveJSON, expectedJSON) {
		return executioncell.ResolvedExecutionCell{}, nil, claim,
			receiptHarnessDenial(receipt, executioncell.DenialFallbackNotAllowed, "runtime effective cell does not equal the admitted or claimed cell")
	}
	if requireHostAdaptation {
		if err := validateHostAdaptationReceipt(qw, receipt, claim, binding); err != nil {
			return executioncell.ResolvedExecutionCell{}, nil, claim,
				receiptHarnessDenial(receipt, executioncell.DenialPlacementUnsatisfied, err.Error())
		}
	}
	return effective, effectiveJSON, claim, nil
}

func validateHostAdaptationReceipt(qw QueuedWork, admission executioncell.AdmissionReceipt, claim executioncell.ImmutableClaimReceipt, binding executioncell.RuntimeBinding) error {
	host, err := executioncell.DecodeHostAdaptationReceipt(qw.HostAdaptationReceipt)
	if err != nil {
		return fmt.Errorf("daemon adaptation-ready receipt is invalid: %w", err)
	}
	if host.RequestID != qw.SessionID || host.WorkerID != qw.WorkerID || host.PlacementID != binding.PlacementID || host.ClaimID != binding.ClaimID || host.Decision != "ready" {
		return errors.New("daemon adaptation-ready receipt does not match the active runtime binding")
	}
	var prepared agent.PreparedHarness
	if err := json.Unmarshal(host.Plan, &prepared); err != nil {
		return errors.New("daemon harness adaptation plan is malformed")
	}
	if agent.DigestPreparedHarness(&prepared) != host.PlanDigest {
		return errors.New("daemon harness adaptation plan digest mismatch")
	}
	if err := agent.ValidatePreparedHarness(&prepared, admission.OperationalPayloadDigest); err != nil {
		return err
	}
	wantMode := agent.PromptModeAutonomous
	if admission.Cell != nil && admission.Cell.SessionMode == executioncell.SessionHumanControlled {
		wantMode = agent.PromptModeHumanControlled
	}
	if prepared.Mode != wantMode || (admission.Cell != nil && prepared.Harness != admission.Cell.Harness.ID) {
		return errors.New("daemon harness adaptation plan does not match admitted harness and mode")
	}
	var promptReceipt agent.PromptDeliveryReceipt
	if err := json.Unmarshal(host.PromptReceipt, &promptReceipt); err != nil || promptReceipt.Decision != "ready" {
		return errors.New("daemon prompt adaptation receipt is not ready")
	}
	var toolReceipt agent.ToolLifecycleReceipt
	if err := json.Unmarshal(host.ToolLifecycleReceipt, &toolReceipt); err != nil || toolReceipt.Decision != "ready" {
		return errors.New("daemon tool/lifecycle adaptation receipt is not ready")
	}
	if toolReceipt.AdmissionReceiptID != admission.ReceiptID || toolReceipt.OperationalPayloadDigest != admission.OperationalPayloadDigest {
		return errors.New("daemon tool/lifecycle receipt is not linked to this admission and operational payload")
	}
	wantClaimID := ""
	if len(claim.Bytes()) > 0 {
		wantClaimID = claim.Value().ClaimReceiptID
	}
	if toolReceipt.ClaimReceiptID != wantClaimID {
		return errors.New("daemon tool/lifecycle receipt is not linked to this claim")
	}
	if !bytes.Equal(host.PromptReceipt, mustJSON(prepared.PromptReceipt)) || !bytes.Equal(host.ToolLifecycleReceipt, mustJSON(prepared.ToolLifecycleReceipt)) {
		return errors.New("daemon receipt projections differ from sole prepared harness authority")
	}
	return nil
}

func mustJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func preparedHarnessFromWork(qw QueuedWork) (*agent.PreparedHarness, error) {
	host, err := executioncell.DecodeHostAdaptationReceipt(qw.HostAdaptationReceipt)
	if err != nil {
		return nil, err
	}
	var prepared agent.PreparedHarness
	if err := json.Unmarshal(host.Plan, &prepared); err != nil {
		return nil, err
	}
	return &prepared, nil
}

func (r *Registry) selectReceiptHarness(qw QueuedWork, receipt executioncell.AdmissionReceipt, cell executioncell.ResolvedExecutionCell) (harnessSelection, error) {
	if receipt.Cell == nil {
		return harnessSelection{}, receiptHarnessDenial(receipt, executioncell.DenialHarnessUnavailable,
			"admitted receipt does not contain a resolved execution cell")
	}

	profile := qw.ResolvedProfile
	if profile.Harness == "" {
		profile.Harness = cell.Harness.ID
	}
	selection, err := r.selectExplicitHarness(profile)
	if err != nil {
		return harnessSelection{}, err
	}
	if selection.Harness.ID != cell.Harness.ID {
		return harnessSelection{}, receiptHarnessDenial(receipt, executioncell.DenialUnknownHarness,
			fmt.Sprintf("queued harness resolves to %q but admission receipt pins %q", selection.Harness.ID, cell.Harness.ID))
	}
	if selection.Harness.Version != cell.Harness.Version {
		return harnessSelection{}, receiptHarnessDenial(receipt, executioncell.DenialUnsupportedHarnessVersion,
			fmt.Sprintf("registered harness %q has version %q but admission receipt pins %q", selection.Harness.ID, selection.Harness.Version, cell.Harness.Version))
	}
	if err := validateReceiptCell(qw, receipt, selection, cell); err != nil {
		return harnessSelection{}, err
	}
	selection.Decisions = append([]executioncell.ResolverDecision(nil), receipt.ResolverDecisions...)
	selection.Explicit = true
	return selection, nil
}

func validateReceiptCell(qw QueuedWork, receipt executioncell.AdmissionReceipt, selection harnessSelection, cell executioncell.ResolvedExecutionCell) error {
	if strings.TrimSpace(qw.ResolvedProfile.Model) != cell.Model.ID {
		return receiptHarnessDenial(receipt, executioncell.DenialUnknownModel,
			fmt.Sprintf("queued model %q does not match admitted model %q", strings.TrimSpace(qw.ResolvedProfile.Model), cell.Model.ID))
	}
	wantMode := executioncell.SessionAutonomous
	if qw.isInteractive() || qw.isInterview() {
		wantMode = executioncell.SessionHumanControlled
	}
	if cell.SessionMode != wantMode {
		return receiptHarnessDenial(receipt, executioncell.DenialUnsupportedSessionMode,
			fmt.Sprintf("queued mode %q resolves to %q but admission receipt pins %q", qw.Mode, wantMode, cell.SessionMode))
	}
	if selection.Harness.ID != cell.Harness.ID || selection.Harness.Version != cell.Harness.Version {
		return receiptHarnessDenial(receipt, executioncell.DenialUnsupportedHarnessVersion, "selected harness identity changed after admission")
	}
	endpoint := qw.ResolvedProfile.Endpoint
	if endpoint == nil {
		return receiptHarnessDenial(receipt, executioncell.DenialUnknownEndpoint, "receipt-bearing work requires an explicit endpoint binding")
	}
	// The endpoint carries two distinct identities and they must not be
	// conflated. Company is the SPEAK-axis endpoint identity — the vendor
	// surface and wire dialect the request is spoken to
	// (anthropic/openai/google/local/stub). ModelAuthor is who authored the
	// model that runs. They coincide only for first-party direct cells; a
	// gateway or aggregator cell speaks openai-chat to a model authored by
	// someone else, and a local cell speaks to no vendor at all. The admitted
	// execution cell has no company axis (ServingEndpointRef is id, protocol,
	// operator, revision), so ModelAuthor is the only field that may be
	// compared against cell.Model.Author. Company stays pinned by the
	// operational-payload digest, which binds it as part of the stable
	// endpoint identity.
	if endpoint.EndpointID != cell.Endpoint.ID || endpoint.EndpointOperator != cell.Endpoint.Operator ||
		endpoint.EndpointRevision != cell.Endpoint.Revision || string(endpoint.Protocol) != cell.Endpoint.Protocol ||
		endpoint.Model != cell.Model.ID || endpoint.ModelAuthor != cell.Model.Author {
		return receiptHarnessDenial(receipt, executioncell.DenialUnknownEndpoint, "resolved endpoint/model identity does not match the effective cell")
	}
	if endpoint.AuthBindingID != cell.AuthBinding.ID || endpoint.Mechanism != cell.AuthBinding.Mechanism ||
		endpoint.AuthCommercialMode != string(cell.AuthBinding.CommercialMode) || endpoint.AuthAuthority != cell.AuthBinding.Authority ||
		endpoint.AuthBindingScope != string(cell.AuthBinding.BindingScope) || endpoint.AuthPortability != string(cell.AuthBinding.Portability) ||
		endpoint.AuthDelivery != string(cell.AuthBinding.Delivery) {
		return receiptHarnessDenial(receipt, executioncell.DenialUnknownAuthBinding, "resolved authentication binding does not match the effective cell")
	}
	manifestProvider, ok := selection.Provider.(agent.HarnessProvider)
	if !ok {
		return receiptHarnessDenial(receipt, executioncell.DenialHarnessUnavailable, "selected runtime does not expose an exact harness manifest")
	}
	manifest := manifestProvider.Manifest()
	if !containsWireProtocol(manifest.Caps.Drives, endpoint.Protocol) || !containsServingHost(manifest.Caps.DrivesHosts, endpoint.Host) {
		return receiptHarnessDenial(receipt, executioncell.DenialUnknownEndpoint, "selected harness does not drive the admitted endpoint protocol and host")
	}
	seenCapabilities := make(map[string]struct{}, len(cell.GrantedCapabilities))
	for _, capability := range cell.GrantedCapabilities {
		if _, duplicate := seenCapabilities[capability.Name]; duplicate {
			return receiptHarnessDenial(receipt, executioncell.DenialCapabilityUnsupported,
				fmt.Sprintf("admitted capability %q is duplicated", capability.Name))
		}
		seenCapabilities[capability.Name] = struct{}{}
		switch capability.Name {
		case "watch", "replay", "cancel":
			// These capabilities are projected onto the existing exact
			// tool/lifecycle plan immediately before provider spawn.
		default:
			return receiptHarnessDenial(receipt, executioncell.DenialCapabilityUnsupported,
				fmt.Sprintf("admitted capability %q has no current exact pre-spawn adapter", capability.Name))
		}
	}
	return nil
}

func containsWireProtocol(values []agent.WireProtocol, want agent.WireProtocol) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsServingHost(values []agent.ServingHost, want agent.ServingHost) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func receiptHarnessDenial(receipt executioncell.AdmissionReceipt, code executioncell.AdmissionDenialCode, detail string) *HarnessAdmissionError {
	return &HarnessAdmissionError{
		Code: code, Harness: receiptCellHarnessID(receipt), Detail: detail,
		Decisions: append([]executioncell.ResolverDecision(nil), receipt.ResolverDecisions...),
	}
}

func receiptCellHarnessID(receipt executioncell.AdmissionReceipt) string {
	if receipt.Cell == nil {
		return ""
	}
	return receipt.Cell.Harness.ID
}

const (
	legacyNativeHarness = agent.HarnessName("native")
	legacyRawHarness    = agent.HarnessName("raw")
)

// recognizedHarnessToken recognizes canonical execution-cell harness ids plus
// the documented legacy Platform wire aliases. Recognition is deliberately
// separate from raw/native provider-pair validation: both compatibility tokens
// are known even when their provider pair cannot be admitted.
func recognizedHarnessToken(token string) (agent.HarnessName, bool) {
	if token == "" || token != strings.TrimSpace(token) {
		return "", false
	}
	switch token {
	case string(agent.HarnessClaudeCode), "claude":
		return agent.HarnessClaudeCode, true
	case string(agent.HarnessCodex):
		return agent.HarnessCodex, true
	case string(agent.HarnessOpenCode):
		return agent.HarnessOpenCode, true
	case string(agent.HarnessAntigravity), "agy":
		return agent.HarnessAntigravity, true
	case string(agent.HarnessAmp):
		return agent.HarnessAmp, true
	case string(agent.HarnessGeminiDirect):
		return agent.HarnessGeminiDirect, true
	case string(agent.HarnessOllama):
		return agent.HarnessOllama, true
	case string(legacyRawHarness):
		return legacyRawHarness, true
	case "native":
		return legacyNativeHarness, true
	case string(agent.HarnessStub):
		return agent.HarnessStub, true
	case string(agent.HarnessPi):
		return agent.HarnessPi, true
	case string(agent.HarnessShell):
		return agent.HarnessShell, true
	default:
		return "", false
	}
}

// canonicalInBoxHarness validates the provider-paired raw/native compatibility
// aliases present on the live Platform wire. Claude and Codex have dedicated
// CLI harness tokens; accepting raw/native for them would conflate direct/API
// and CLI execution.
func canonicalInBoxHarness(provider agent.ProviderName) (agent.HarnessName, bool) {
	switch provider {
	case agent.ProviderGemini:
		return agent.HarnessGeminiDirect, true
	case agent.ProviderOllama:
		return agent.HarnessOllama, true
	default:
		return "", false
	}
}

func harnessRef(provider agent.HarnessProvider) executioncell.HarnessRef {
	manifest := provider.Manifest()
	return executioncell.HarnessRef{ID: string(manifest.Name), Version: manifest.ContractABI}
}

// legacyHarnessNameForProvider is compatibility projection confined to the
// named legacy adapter. It lets older Provider implementations that predate
// Manifest() retain absent-selector behavior; explicit harness admission never
// uses this map and always requires a live registered manifest match.
func legacyHarnessNameForProvider(name agent.ProviderName) agent.HarnessName {
	switch name {
	case agent.ProviderClaude:
		return agent.HarnessClaudeCode
	case agent.ProviderCodex:
		return agent.HarnessCodex
	case agent.ProviderAmp:
		return agent.HarnessAmp
	case agent.ProviderAGYCLI:
		return agent.HarnessAntigravity
	case agent.ProviderOpenCode:
		return agent.HarnessOpenCode
	case agent.ProviderGemini:
		return agent.HarnessGeminiDirect
	case agent.ProviderOllama:
		return agent.HarnessOllama
	case agent.ProviderStub:
		return agent.HarnessStub
	case agent.ProviderPi:
		return agent.HarnessPi
	case agent.ProviderShell:
		return agent.HarnessShell
	default:
		return agent.HarnessName(name)
	}
}

func legacyHarnessRef(provider agent.Provider) executioncell.HarnessRef {
	if manifestProvider, ok := provider.(agent.HarnessProvider); ok {
		return harnessRef(manifestProvider)
	}
	return executioncell.HarnessRef{
		ID: string(legacyHarnessNameForProvider(provider.Name())), Version: "harness/v2",
	}
}

func explicitHarnessSourceRef(token string) string {
	switch token {
	case "claude", "agy", "native", string(legacyRawHarness):
		return "legacy-harness:" + token
	default:
		// codex, amp, and opencode are both live Platform wire values and
		// canonical manifest ids; no alias translation is necessary.
		return "canonical-harness:" + token
	}
}

func explicitHarnessDecision(token string, ref executioncell.HarnessRef) executioncell.ResolverDecision {
	return executioncell.ResolverDecision{
		Kind:        executioncell.DecisionExplicit,
		Field:       "harness",
		SelectedRef: "harness:" + ref.ID + "@" + ref.Version,
		SourceRef:   explicitHarnessSourceRef(token),
		Reason:      "ResolvedProfile carried an explicit harness selector; the registered manifest supplied its canonical identity and version.",
	}
}

// selectExplicitHarness resolves only the requested harness. It never consults
// Provider, Runner, a default, or posterior routing as a fallback. Provider is
// consulted only by the receipted raw/native compatibility adapter; a canonical
// harness id is authoritative even when Provider contradicts it.
func (r *Registry) selectExplicitHarness(profile ResolvedProfile) (harnessSelection, error) {
	canonical, known := recognizedHarnessToken(profile.Harness)
	if !known {
		return harnessSelection{}, &HarnessAdmissionError{
			Code:    executioncell.DenialUnknownHarness,
			Harness: profile.Harness,
			Detail:  "selector is not a known canonical harness or documented legacy harness alias",
		}
	}
	if canonical == legacyNativeHarness || canonical == legacyRawHarness {
		var compatible bool
		canonical, compatible = canonicalInBoxHarness(profile.Provider)
		if !compatible {
			decision := executioncell.ResolverDecision{
				Kind:        executioncell.DecisionExplicit,
				Field:       "harness",
				SelectedRef: "harness:" + profile.Harness,
				SourceRef:   explicitHarnessSourceRef(profile.Harness),
				Reason:      "Legacy ResolvedProfile explicitly selected a known raw/native compatibility alias, but its provider was not one of the documented gemini or ollama pairings.",
			}
			return harnessSelection{}, &HarnessAdmissionError{
				Code:      executioncell.DenialHarnessUnavailable,
				Harness:   profile.Harness,
				Detail:    "known raw/native compatibility alias requires provider gemini or ollama",
				Decisions: []executioncell.ResolverDecision{decision},
			}
		}
	}

	var matches []agent.HarnessProvider
	for _, name := range r.Names() {
		provider, err := r.Resolve(name)
		if err != nil {
			continue
		}
		harness, ok := provider.(agent.HarnessProvider)
		if ok && harness.Manifest().Name == canonical {
			matches = append(matches, harness)
		}
	}

	if len(matches) != 1 {
		detail := "known harness has no registered runtime implementation"
		if len(matches) > 1 {
			detail = "known canonical harness maps to multiple registered runtime implementations"
		}
		decision := executioncell.ResolverDecision{
			Kind:        executioncell.DecisionExplicit,
			Field:       "harness",
			SelectedRef: "harness:" + string(canonical),
			SourceRef:   explicitHarnessSourceRef(profile.Harness),
			Reason:      "ResolvedProfile explicitly selected a known harness, but the live registry could not admit exactly one matching implementation.",
		}
		return harnessSelection{}, &HarnessAdmissionError{
			Code:      executioncell.DenialHarnessUnavailable,
			Harness:   string(canonical),
			Detail:    detail,
			Decisions: []executioncell.ResolverDecision{decision},
		}
	}

	selected := matches[0]
	ref := harnessRef(selected)
	decision := explicitHarnessDecision(profile.Harness, ref)
	return harnessSelection{
		Provider: selected, Harness: ref,
		Decisions: []executioncell.ResolverDecision{decision}, Explicit: true,
	}, nil
}

type legacyHarnessSource struct {
	provider agent.ProviderName
	kind     executioncell.ResolverDecisionKind
	source   string
	reason   string
}

// legacyHarnessSelectionSource is the only place where absent harness intent
// may use the historical Provider -> Runner -> Claude chain.
func legacyHarnessSelectionSource(profile ResolvedProfile) legacyHarnessSource {
	if profile.Provider != "" {
		return legacyHarnessSource{
			provider: profile.Provider,
			kind:     executioncell.DecisionLegacyInference,
			source:   "legacy-provider:" + string(profile.Provider),
			reason:   "Legacy ResolvedProfile omitted harness; the documented provider mapping supplied it.",
		}
	}
	if profile.Runner != "" {
		return legacyHarnessSource{
			provider: agent.ProviderName(profile.Runner),
			kind:     executioncell.DecisionLegacyInference,
			source:   "legacy-runner:" + profile.Runner,
			reason:   "Legacy ResolvedProfile omitted harness and provider; the documented runner mapping supplied it.",
		}
	}
	return legacyHarnessSource{
		provider: agent.ProviderClaude,
		kind:     executioncell.DecisionDefault,
		reason:   "Legacy ResolvedProfile omitted harness, provider, and runner; existing dispatch semantics default to Claude Code.",
	}
}

// legacyHarnessSelectionAdapter preserves absent-selector behavior while
// making every inference/default visible. posteriorProvider is optional; when
// set, it is the final legacy routing choice and resolution still happens once.
func (r *Registry) legacyHarnessSelectionAdapter(profile ResolvedProfile, posteriorProvider agent.ProviderName) (harnessSelection, error) {
	source := legacyHarnessSelectionSource(profile)
	if posteriorProvider != "" {
		source = legacyHarnessSource{
			provider: posteriorProvider,
			kind:     executioncell.DecisionLegacyInference,
			source:   "legacy-posterior:" + string(posteriorProvider),
			reason:   "Legacy request omitted an explicit harness; posterior routing selected a registered provider before harness admission.",
		}
	}
	provider, err := r.Resolve(source.provider)
	if err != nil {
		providerErr := &ProviderNotRegisteredError{
			RequestedID: string(source.provider),
			Registered:  r.registeredNames(),
		}
		return harnessSelection{}, &HarnessAdmissionError{
			Code:      executioncell.DenialHarnessUnavailable,
			Harness:   string(source.provider),
			Detail:    providerErr.Error(),
			Decisions: []executioncell.ResolverDecision{legacyUnresolvedDecision(source)},
			cause:     providerErr,
		}
	}
	ref := legacyHarnessRef(provider)
	decision := executioncell.ResolverDecision{
		Kind: source.kind, Field: "harness",
		SelectedRef: "harness:" + ref.ID + "@" + ref.Version,
		SourceRef:   source.source, Reason: source.reason,
	}
	return harnessSelection{
		Provider: provider, Harness: ref,
		Decisions: []executioncell.ResolverDecision{decision},
	}, nil
}

func legacyUnresolvedDecision(source legacyHarnessSource) executioncell.ResolverDecision {
	return executioncell.ResolverDecision{
		Kind: source.kind, Field: "harness",
		SelectedRef: "harness:" + string(source.provider),
		SourceRef:   source.source, Reason: source.reason,
	}
}

// resolveHarnessSelection performs exactly one registry resolution. Explicit
// intent is admitted first and never reaches posterior routing. Legacy intent
// may retain posterior behavior, but the final posterior/static provider is
// handed once to the explicit legacy adapter.
func (r *Runner) resolveHarnessSelection(ctx context.Context, qw QueuedWork) (harnessSelection, error) {
	if len(qw.AdmissionReceipt) > 0 {
		admission, err := r.registry.PreflightHarness(qw)
		if err != nil {
			return harnessSelection{}, err
		}
		return r.admittedHarnessSelection(ctx, qw, admission)
	}
	if qw.ResolvedProfile.Harness != "" {
		return r.registry.selectExplicitHarness(qw.ResolvedProfile)
	}
	var posterior agent.ProviderName
	if name, ok := r.selectProviderByPosterior(ctx, qw); ok {
		posterior = agent.ProviderName(name)
	}
	return r.registry.legacyHarnessSelectionAdapter(qw.ResolvedProfile, posterior)
}

func (r *Runner) admittedHarnessSelection(ctx context.Context, qw QueuedWork, admission *HarnessAdmission) (harnessSelection, error) {
	if admission == nil {
		return r.resolveHarnessSelection(ctx, qw)
	}
	if admission.registry != r.registry {
		return harnessSelection{}, errors.New("runner: harness admission belongs to a different registry")
	}
	if admission.requestID != qw.SessionID {
		return harnessSelection{}, errors.New("runner: harness admission belongs to a different request")
	}
	if admission.intent != selectorIntent(qw.ResolvedProfile) {
		return harnessSelection{}, errors.New("runner: harness selector intent changed after preflight admission")
	}
	if len(admission.receipt.Bytes()) > 0 {
		payloadDigest, err := DigestOperationalPayload(qw)
		if err != nil {
			return harnessSelection{}, fmt.Errorf("runner: digest operational payload after preflight: %w", err)
		}
		if payloadDigest != admission.receipt.Value().OperationalPayloadDigest {
			return harnessSelection{}, errors.New("runner: operational payload changed after preflight admission")
		}
		currentReceipt, err := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
		if err != nil {
			return harnessSelection{}, fmt.Errorf("runner: decode admission receipt after preflight: %w", err)
		}
		if !bytes.Equal(currentReceipt.Bytes(), admission.receipt.Bytes()) {
			return harnessSelection{}, errors.New("runner: admission receipt changed after preflight")
		}
		if admission.denial == nil {
			effectiveCell, effectiveJSON, claimReceipt, err := resolveReceiptEffectiveCell(qw, currentReceipt, true)
			if err != nil {
				return harnessSelection{}, err
			}
			if !bytes.Equal(effectiveJSON, admission.selection.effectiveJSON) ||
				!bytes.Equal(claimReceipt.Bytes(), admission.selection.claimReceipt.Bytes()) {
				return harnessSelection{}, errors.New("runner: claim receipt or effective cell changed after preflight")
			}
			if err := validateReceiptCell(qw, currentReceipt.Value(), admission.selection, effectiveCell); err != nil {
				return harnessSelection{}, err
			}
		}
	}
	if admission.denial != nil {
		payloadDigest, err := DigestOperationalPayload(qw)
		if err != nil {
			return harnessSelection{}, fmt.Errorf("runner: digest denied harness payload: %w", err)
		}
		if payloadDigest != admission.denialPayloadDigest {
			return harnessSelection{}, errors.New("runner: denied harness payload changed after preflight admission")
		}
	}
	if !admission.consumed.CompareAndSwap(false, true) {
		return harnessSelection{}, errors.New("runner: harness admission was already consumed")
	}
	if admission.denial != nil {
		return harnessSelection{}, admission.denial
	}
	return admission.selection, nil
}

func attachDeniedHarnessReceipt(qw QueuedWork, err error, recordedAt time.Time) error {
	var admissionErr *HarnessAdmissionError
	if !errors.As(err, &admissionErr) {
		return err
	}
	if len(admissionErr.Receipt.Bytes()) != 0 {
		return err
	}
	receipt, receiptErr := deniedHarnessAdmissionReceipt(qw, admissionErr, recordedAt)
	if receiptErr != nil {
		return errors.Join(err, fmt.Errorf("persist canonical harness denial evidence: %w", receiptErr))
	}
	admissionErr.Receipt = receipt
	return err
}

func deniedHarnessAdmissionReceipt(qw QueuedWork, denial *HarnessAdmissionError, recordedAt time.Time) (executioncell.ImmutableAdmissionReceipt, error) {
	decisions := append([]executioncell.ResolverDecision{}, denial.Decisions...)
	intentProjection := struct {
		ContractVersion string          `json:"contractVersion"`
		RequestID       string          `json:"requestId"`
		ResolvedProfile ResolvedProfile `json:"resolvedProfile"`
	}{executioncell.ContractVersion, qw.SessionID, qw.ResolvedProfile}
	intentDigest, err := executioncell.DigestContractValue(intentProjection)
	if err != nil {
		return executioncell.ImmutableAdmissionReceipt{}, err
	}
	payloadDigest, err := DigestOperationalPayload(qw)
	if err != nil {
		return executioncell.ImmutableAdmissionReceipt{}, err
	}
	receiptID := "admission_harness_" + intentDigest[:24]
	receipt := executioncell.AdmissionReceipt{
		ContractVersion:          executioncell.ContractVersion,
		ReceiptID:                receiptID,
		RequestID:                qw.SessionID,
		Decision:                 executioncell.AdmissionDenied,
		IntentDigest:             intentDigest,
		OperationalPayloadDigest: payloadDigest,
		DenialCode:               denial.Code,
		DenialDetail:             denial.Detail,
		ResolverDecisions:        decisions,
		RecordedAt:               recordedAt.UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return executioncell.ImmutableAdmissionReceipt{}, fmt.Errorf("marshal denied harness admission receipt: %w", err)
	}
	immutable, err := executioncell.DecodeAdmissionReceipt(raw)
	if err != nil {
		return executioncell.ImmutableAdmissionReceipt{}, fmt.Errorf("validate denied harness admission receipt: %w", err)
	}
	return immutable, nil
}
