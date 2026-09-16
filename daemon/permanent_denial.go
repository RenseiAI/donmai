package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

const (
	preSpawnPermanentDenialContractVersion = "pre-spawn-permanent-denial/v1"
	preSpawnPermanentDenialReason          = "Required pre-spawn tool delivery is unsupported."
)

type preSpawnPermanentDenialReport struct {
	ContractVersion               string                         `json:"contractVersion"`
	Code                          agent.ToolAdaptationDenialCode `json:"code"`
	Channel                       agent.ToolLifecycleChannel     `json:"channel"`
	AdmissionReceiptID            string                         `json:"admissionReceiptId"`
	ClaimReceiptID                string                         `json:"claimReceiptId,omitempty"`
	OperationalPayloadDigest      string                         `json:"operationalPayloadDigest"`
	RuntimeBindingCanonicalSHA256 string                         `json:"runtimeBindingCanonicalSha256"`
	HostReceiptSHA256             string                         `json:"hostReceiptSha256"`
}

// preSpawnPermanentDenialError keeps the original typed cause reachable to
// errors.As without copying its potentially value-bearing detail into Error.
// report is wire-authoritative only after durable is set following an exact
// receipt persistence or retained-byte reaffirmation.
type preSpawnPermanentDenialError struct {
	cause   error
	report  *preSpawnPermanentDenialReport
	durable bool
}

type preSpawnPermanentDenialBoundaryError struct {
	phase  string
	causes []error
}

func (e *preSpawnPermanentDenialBoundaryError) Error() string {
	return "permanent pre-spawn denial " + e.phase + " failed"
}

func (e *preSpawnPermanentDenialBoundaryError) Unwrap() []error {
	return append([]error(nil), e.causes...)
}

func protectPermanentDenialBoundaryFailure(phase string, failure, preflightErr error) error {
	if permanentDenialWrapper(preflightErr) == nil {
		return failure
	}
	return &preSpawnPermanentDenialBoundaryError{
		phase: phase, causes: []error{failure, preflightErr},
	}
}

func (e *preSpawnPermanentDenialError) Error() string { return preSpawnPermanentDenialReason }
func (e *preSpawnPermanentDenialError) Unwrap() error { return e.cause }

func (e *preSpawnPermanentDenialError) markDurable() {
	if e != nil && e.report != nil {
		e.durable = true
	}
}

func durablePermanentDenialReport(err error) *preSpawnPermanentDenialReport {
	var denial *preSpawnPermanentDenialError
	if !errors.As(err, &denial) || denial == nil || !denial.durable || denial.report == nil {
		return nil
	}
	report := *denial.report
	return &report
}

func permanentDenialWrapper(err error) *preSpawnPermanentDenialError {
	var denial *preSpawnPermanentDenialError
	if errors.As(err, &denial) {
		return denial
	}
	return nil
}

func freshPermanentDenialCandidate(
	detail *SessionDetail,
	binding executioncell.RuntimeBinding,
	receipt json.RawMessage,
	operationalDigest string,
	preflightErr error,
) *preSpawnPermanentDenialError {
	var typed *agent.ToolAdaptationError
	if !errors.As(preflightErr, &typed) || typed.Code != agent.ToolDenialDeliveryUnsupported {
		return nil
	}
	// Even a typed denial that fails evidence correlation gets a value-free
	// wrapper so outer Error()/errors.Join logging cannot reveal Detail. It gains
	// no report and therefore no terminal authority.
	wrapped := &preSpawnPermanentDenialError{cause: preflightErr}
	if !knownPermanentDenialChannel(typed.Channel) {
		return wrapped
	}
	report, ok := projectPermanentDenialReport(
		detail, binding, receipt, operationalDigest, typed.Channel,
	)
	if ok {
		wrapped.report = &report
	}
	return wrapped
}

func retainedPermanentDenial(
	detail *SessionDetail,
	binding executioncell.RuntimeBinding,
	receipt json.RawMessage,
	operationalDigest string,
) *preSpawnPermanentDenialError {
	report, ok := projectPermanentDenialReport(
		detail, binding, receipt, operationalDigest, "",
	)
	if !ok {
		return nil
	}
	denial := &preSpawnPermanentDenialError{
		cause:   errors.New("retained permanent pre-spawn denial"),
		report:  &report,
		durable: true,
	}
	return denial
}

func projectPermanentDenialReport(
	detail *SessionDetail,
	binding executioncell.RuntimeBinding,
	receipt json.RawMessage,
	operationalDigest string,
	wantChannel agent.ToolLifecycleChannel,
) (preSpawnPermanentDenialReport, bool) {
	var zero preSpawnPermanentDenialReport
	if detail == nil {
		return zero, false
	}
	computedOperationalDigest, err := executioncell.DigestOperationalPayload(detail.OperationalPayload)
	if err != nil {
		return zero, false
	}
	if operationalDigest == "" {
		operationalDigest = computedOperationalDigest
	} else if operationalDigest != computedOperationalDigest {
		return zero, false
	}
	decodedBinding, err := executioncell.DecodeRuntimeBinding(detail.ExecutionRuntimeBinding)
	if err != nil || !reflect.DeepEqual(decodedBinding, binding) ||
		detail.SessionID != binding.RequestID || detail.WorkerID != binding.WorkerID {
		return zero, false
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil || host.Decision != "denied" ||
		host.RequestID != binding.RequestID || host.WorkerID != binding.WorkerID ||
		host.PlacementID != binding.PlacementID || host.ClaimID != binding.ClaimID {
		return zero, false
	}
	if len(host.Plan) == 0 || len(host.ToolLifecycleReceipt) == 0 || host.PlanDigest == "" {
		return zero, false
	}
	planSum := sha256.Sum256(host.Plan)
	if hex.EncodeToString(planSum[:]) != host.PlanDigest {
		return zero, false
	}

	admitted, err := executioncell.DecodeAdmissionReceipt(detail.AdmissionReceipt)
	if err != nil {
		return zero, false
	}
	admission := admitted.Value()
	if admission.Decision != executioncell.AdmissionAdmitted || admission.Cell == nil ||
		admission.RequestID != binding.RequestID ||
		admission.OperationalPayloadDigest != operationalDigest {
		return zero, false
	}
	effective, err := executioncell.DecodeResolvedExecutionCell(detail.EffectiveCell)
	if err != nil || effective.Placement.ID != binding.PlacementID {
		return zero, false
	}
	wantClaimReceiptID := ""
	if len(detail.ClaimReceipt) == 0 {
		if binding.ClaimID != "" || !reflect.DeepEqual(*admission.Cell, effective) {
			return zero, false
		}
	} else {
		claim, claimErr := executioncell.DecodeClaimReceipt(detail.ClaimReceipt)
		if claimErr != nil || executioncell.AssertNarrowClaim(admitted, claim) != nil {
			return zero, false
		}
		claimValue := claim.Value()
		if claimValue.Decision != executioncell.ClaimClaimed ||
			claimValue.ClaimID != binding.ClaimID || claimValue.EffectiveCell == nil ||
			!reflect.DeepEqual(*claimValue.EffectiveCell, effective) {
			return zero, false
		}
		wantClaimReceiptID = claimValue.ClaimReceiptID
	}

	var plan agent.PreparedHarness
	if err := decodeClosedPermanentDenialJSON(host.Plan, &plan); err != nil {
		return zero, false
	}
	if plan.ContractVersion != agent.HarnessAdaptationContractVersion ||
		plan.Harness != admission.Cell.Harness.ID ||
		plan.OperationalPayloadDigest != operationalDigest ||
		!lowerHex64(plan.AuthorityDigest) {
		return zero, false
	}
	wantMode := agent.PromptModeAutonomous
	if admission.Cell.SessionMode == executioncell.SessionHumanControlled {
		wantMode = agent.PromptModeHumanControlled
	}
	if plan.Mode != wantMode {
		return zero, false
	}
	var planMembers map[string]json.RawMessage
	if err := json.Unmarshal(host.Plan, &planMembers); err != nil {
		return zero, false
	}
	planToolRaw := planMembers["toolLifecycleReceipt"]
	planPromptRaw := planMembers["promptReceipt"]
	if len(planToolRaw) == 0 || len(planPromptRaw) == 0 || len(host.PromptReceipt) == 0 {
		return zero, false
	}
	planPromptCanonical, err := executioncell.CanonicalJSON(json.RawMessage(planPromptRaw))
	if err != nil {
		return zero, false
	}
	outerPromptCanonical, err := executioncell.CanonicalJSON(json.RawMessage(host.PromptReceipt))
	if err != nil || !bytes.Equal(planPromptCanonical, outerPromptCanonical) {
		return zero, false
	}
	var prompt agent.PromptDeliveryReceipt
	if err := decodeClosedPermanentDenialJSON(host.PromptReceipt, &prompt); err != nil ||
		prompt.ContractVersion != agent.PromptContractVersion ||
		strings.TrimSpace(prompt.ProfileID) == "" || prompt.Decision != "ready" {
		return zero, false
	}
	planToolCanonical, err := executioncell.CanonicalJSON(json.RawMessage(planToolRaw))
	if err != nil {
		return zero, false
	}
	outerToolCanonical, err := executioncell.CanonicalJSON(json.RawMessage(host.ToolLifecycleReceipt))
	if err != nil || !bytes.Equal(planToolCanonical, outerToolCanonical) {
		return zero, false
	}
	var tools agent.ToolLifecycleReceipt
	if err := decodeClosedPermanentDenialJSON(host.ToolLifecycleReceipt, &tools); err != nil {
		return zero, false
	}
	if tools.ContractVersion != agent.ToolLifecycleContractVersion ||
		strings.TrimSpace(tools.ProfileID) == "" || tools.Decision != "denied" ||
		tools.AdmissionReceiptID != admission.ReceiptID ||
		tools.ClaimReceiptID != wantClaimReceiptID ||
		tools.OperationalPayloadDigest != operationalDigest {
		return zero, false
	}

	matching := 0
	matchedChannel := agent.ToolLifecycleChannel("")
	seenEntries := make(map[string]struct{}, len(tools.Entries))
	for _, entry := range tools.Entries {
		if strings.TrimSpace(entry.ID) == "" || !knownPermanentDenialChannel(entry.Channel) ||
			!knownToolOutcome(entry.Outcome) {
			return zero, false
		}
		if _, duplicate := seenEntries[entry.ID]; duplicate {
			return zero, false
		}
		seenEntries[entry.ID] = struct{}{}
		if entry.Required && entry.Outcome == agent.ToolOutcomeDenied &&
			entry.DenialCode == agent.ToolDenialDeliveryUnsupported &&
			lowerHex64(entry.InputDigest) {
			matching++
			matchedChannel = entry.Channel
		}
	}
	if matching != 1 || (wantChannel != "" && matchedChannel != wantChannel) {
		return zero, false
	}

	bindingCanonical, err := executioncell.CanonicalJSON(json.RawMessage(detail.ExecutionRuntimeBinding))
	if err != nil {
		return zero, false
	}
	bindingSum := sha256.Sum256(bindingCanonical)
	hostSum := sha256.Sum256(receipt)
	return preSpawnPermanentDenialReport{
		ContractVersion:               preSpawnPermanentDenialContractVersion,
		Code:                          agent.ToolDenialDeliveryUnsupported,
		Channel:                       matchedChannel,
		AdmissionReceiptID:            admission.ReceiptID,
		ClaimReceiptID:                wantClaimReceiptID,
		OperationalPayloadDigest:      operationalDigest,
		RuntimeBindingCanonicalSHA256: hex.EncodeToString(bindingSum[:]),
		HostReceiptSHA256:             hex.EncodeToString(hostSum[:]),
	}, true
}

func knownToolOutcome(outcome agent.ToolAdaptationOutcome) bool {
	switch outcome {
	case agent.ToolOutcomeAdmitted,
		agent.ToolOutcomePendingRuntime,
		agent.ToolOutcomePendingCleanup,
		agent.ToolOutcomeDenied,
		agent.ToolOutcomeDowngraded:
		return true
	default:
		return false
	}
}

func lowerHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func knownPermanentDenialChannel(channel agent.ToolLifecycleChannel) bool {
	switch channel {
	case agent.ToolChannelToolPlugin,
		agent.ToolChannelMCPServer,
		agent.ToolChannelAllowedTools,
		agent.ToolChannelDisallowedTools,
		agent.ToolChannelPermissionConfig,
		agent.ToolChannelMCPToolNames,
		agent.ToolChannelToolHook,
		agent.ToolChannelLifecycle,
		agent.ToolChannelReplay,
		agent.ToolChannelCleanup:
		return true
	default:
		return false
	}
}

func decodeClosedPermanentDenialJSON(raw json.RawMessage, target any) error {
	if err := rejectPermanentDenialDuplicateJSON(raw); err != nil {
		return err
	}
	if err := validatePermanentDenialExactJSONNames(raw, reflect.TypeOf(target)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("permanent denial evidence has trailing JSON")
	}
	return nil
}

var permanentDenialRawMessageType = reflect.TypeOf(json.RawMessage{})

func validatePermanentDenialExactJSONNames(raw json.RawMessage, targetType reflect.Type) error {
	for targetType.Kind() == reflect.Pointer {
		targetType = targetType.Elem()
	}
	if targetType == permanentDenialRawMessageType || targetType.Kind() == reflect.Interface {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	switch targetType.Kind() {
	case reflect.Struct:
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil || members == nil {
			return errors.New("permanent denial evidence must be an object")
		}
		fields := make(map[string]reflect.Type, targetType.NumField())
		collectPermanentDenialStructJSONFields(targetType, fields, map[reflect.Type]bool{})
		for name, memberRaw := range members {
			fieldType, ok := fields[name]
			if !ok {
				return errors.New("permanent denial evidence has unknown or aliased member")
			}
			if err := validatePermanentDenialExactJSONNames(memberRaw, fieldType); err != nil {
				return err
			}
		}
	case reflect.Map:
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil || members == nil {
			return errors.New("permanent denial evidence has invalid dynamic map")
		}
		for _, memberRaw := range members {
			if err := validatePermanentDenialExactJSONNames(memberRaw, targetType.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return errors.New("permanent denial evidence has invalid array")
		}
		for _, elementRaw := range elements {
			if err := validatePermanentDenialExactJSONNames(elementRaw, targetType.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func collectPermanentDenialStructJSONFields(
	structType reflect.Type,
	fields map[string]reflect.Type,
	visiting map[reflect.Type]bool,
) {
	for structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	if structType.Kind() != reflect.Struct || visiting[structType] {
		return
	}
	visiting[structType] = true
	defer delete(visiting, structType)
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		fieldType := field.Type
		unwrapped := fieldType
		for unwrapped.Kind() == reflect.Pointer {
			unwrapped = unwrapped.Elem()
		}
		if field.Anonymous && name == "" && unwrapped.Kind() == reflect.Struct {
			collectPermanentDenialStructJSONFields(unwrapped, fields, visiting)
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = fieldType
	}
}

func rejectPermanentDenialDuplicateJSON(raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanPermanentDenialJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid permanent denial evidence JSON")
	}
	return nil
}

func scanPermanentDenialJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("permanent denial evidence has invalid object key")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("permanent denial evidence has duplicate member")
			}
			seen[key] = struct{}{}
			if err := scanPermanentDenialJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("permanent denial evidence has invalid object")
		}
	case '[':
		for decoder.More() {
			if err := scanPermanentDenialJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("permanent denial evidence has invalid array")
		}
	default:
		return fmt.Errorf("permanent denial evidence has invalid delimiter %q", delimiter)
	}
	return nil
}
