package executioncell

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// MaxPreflightRegistrationReceiptBytes bounds the exact host receipt carried
// to a controller. A larger receipt refuses instead of being truncated.
const MaxPreflightRegistrationReceiptBytes = 256 * 1024

// PreflightRegistrationRequest carries the exact fsynced host receipt to the
// authority that owns the admitted execution context. ReceiptBytesBase64 is an
// exact byte carrier; consumers must not parse and reserialize it for storage.
type PreflightRegistrationRequest struct {
	ContractVersion          string         `json:"contractVersion"`
	RuntimeBinding           RuntimeBinding `json:"runtimeBinding"`
	ReceiptBytesBase64       string         `json:"receiptBytesBase64"`
	ReceiptSHA256            string         `json:"receiptSha256"`
	OperationalPayloadDigest string         `json:"operationalPayloadDigest"`
}

// PreflightRegistrationRefusalCode is the closed controller refusal domain.
type PreflightRegistrationRefusalCode string

const (
	// PreflightBindingMismatch refuses changed execution identity or receipt bytes.
	PreflightBindingMismatch PreflightRegistrationRefusalCode = "binding_mismatch"
	// PreflightReceiptInvalid refuses malformed or inconsistent host evidence.
	PreflightReceiptInvalid PreflightRegistrationRefusalCode = "receipt_invalid"
	// PreflightReceiptDenied refuses a host compiler's denied receipt.
	PreflightReceiptDenied PreflightRegistrationRefusalCode = "receipt_denied"
	// PreflightClaimRetired refuses a claim that no longer owns the session.
	PreflightClaimRetired PreflightRegistrationRefusalCode = "claim_retired"
	// PreflightAuthorizationUnavailable refuses when controller evidence cannot be established.
	PreflightAuthorizationUnavailable PreflightRegistrationRefusalCode = "authorization_unavailable"
	// PreflightAlreadyStarted refuses reuse after execution began.
	PreflightAlreadyStarted PreflightRegistrationRefusalCode = "already_started"
)

// PreflightRegistrationResponse is a closed authorized-or-refused envelope.
// The transport authenticates the response; matching correlation alone never
// grants permission to spawn.
type PreflightRegistrationResponse struct {
	ContractVersion       string                           `json:"contractVersion"`
	Decision              string                           `json:"decision"`
	RegistrationID        string                           `json:"registrationId,omitempty"`
	ReceiptSHA256         string                           `json:"receiptSha256,omitempty"`
	RuntimeBinding        *RuntimeBinding                  `json:"runtimeBinding,omitempty"`
	AuthorizationRevision uint64                           `json:"authorizationRevision,omitempty"`
	Code                  PreflightRegistrationRefusalCode `json:"code,omitempty"`
}

func isHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func validateRuntimeBindingValue(value RuntimeBinding) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = DecodeRuntimeBinding(raw)
	return err
}

func decodeClosed(raw []byte, out any, label string) error {
	if err := rejectDuplicateFields(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("executioncell: decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("executioncell: %s has trailing JSON", label)
	}
	return nil
}

// NewPreflightRegistrationRequest binds exact receipt bytes and the operational
// payload digest to a previously decoded runtime binding v2.
func NewPreflightRegistrationRequest(binding RuntimeBinding, receipt []byte, operationalPayloadDigest string) (PreflightRegistrationRequest, error) {
	request := PreflightRegistrationRequest{
		ContractVersion:          PreflightRegistrationContractVersion,
		RuntimeBinding:           binding,
		ReceiptBytesBase64:       base64.StdEncoding.EncodeToString(receipt),
		OperationalPayloadDigest: operationalPayloadDigest,
	}
	digest := sha256.Sum256(receipt)
	request.ReceiptSHA256 = hex.EncodeToString(digest[:])
	if err := ValidatePreflightRegistrationRequest(request); err != nil {
		return PreflightRegistrationRequest{}, err
	}
	return request, nil
}

// ValidateHostAdaptationForRegistration applies the v2-only deep receipt
// floor. Historical host-adaptation/v1 decoding remains unchanged for v1
// callers; a registration request additionally requires the actual closed
// PreparedHarness and its two exact ready receipt projections.
func ValidateHostAdaptationForRegistration(binding RuntimeBinding, receipt []byte, operationalPayloadDigest string) error {
	if binding.ContractVersion != RuntimeBindingV2ContractVersion || !isHex64(operationalPayloadDigest) {
		return errors.New("executioncell: registered host adaptation requires runtime binding v2 and operational digest")
	}
	host, err := DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		return err
	}
	if host.RequestID != binding.RequestID || host.WorkerID != binding.WorkerID || host.PlacementID != binding.PlacementID || host.ClaimID != binding.ClaimID {
		return errors.New("executioncell: registered host adaptation does not match the active binding")
	}
	if host.Decision == "denied" {
		return nil
	}
	var plan agent.PreparedHarness
	if err := decodeClosed(host.Plan, &plan, "prepared harness"); err != nil {
		return err
	}
	if err := agent.ValidatePreparedHarness(&plan, operationalPayloadDigest); err != nil {
		return fmt.Errorf("executioncell: registered prepared harness: %w", err)
	}
	if agent.DigestPreparedHarness(&plan) != host.PlanDigest {
		return errors.New("executioncell: registered prepared harness digest mismatch")
	}
	var prompt agent.PromptDeliveryReceipt
	if err := decodeClosed(host.PromptReceipt, &prompt, "prompt delivery receipt"); err != nil {
		return err
	}
	if prompt.ContractVersion != agent.PromptContractVersion || strings.TrimSpace(prompt.ProfileID) == "" || prompt.Decision != "ready" {
		return errors.New("executioncell: registered prompt delivery receipt is not ready")
	}
	var tool agent.ToolLifecycleReceipt
	if err := decodeClosed(host.ToolLifecycleReceipt, &tool, "tool lifecycle receipt"); err != nil {
		return err
	}
	if tool.ContractVersion != agent.ToolLifecycleContractVersion || strings.TrimSpace(tool.ProfileID) == "" || tool.Decision != "ready" || tool.OperationalPayloadDigest != operationalPayloadDigest || strings.TrimSpace(tool.AdmissionReceiptID) == "" {
		return errors.New("executioncell: registered tool lifecycle receipt is not ready or bound")
	}
	if (binding.ClaimID == "" && tool.ClaimReceiptID != "") || (binding.ClaimID != "" && strings.TrimSpace(tool.ClaimReceiptID) == "") {
		return errors.New("executioncell: registered tool lifecycle claim binding mismatch")
	}
	promptRaw, promptErr := json.Marshal(plan.PromptReceipt)
	toolRaw, toolErr := json.Marshal(plan.ToolLifecycleReceipt)
	if promptErr != nil || toolErr != nil || !bytes.Equal(promptRaw, host.PromptReceipt) || !bytes.Equal(toolRaw, host.ToolLifecycleReceipt) {
		return errors.New("executioncell: registered nested receipts differ from the prepared harness")
	}
	return nil
}

// ValidatePreflightRegistrationRequest validates the complete closed request.
func ValidatePreflightRegistrationRequest(value PreflightRegistrationRequest) error {
	if value.ContractVersion != PreflightRegistrationContractVersion {
		return fmt.Errorf("executioncell: unsupported preflight registration version %q", value.ContractVersion)
	}
	if value.RuntimeBinding.ContractVersion != RuntimeBindingV2ContractVersion {
		return errors.New("executioncell: preflight registration requires runtime binding v2")
	}
	if err := validateRuntimeBindingValue(value.RuntimeBinding); err != nil {
		return err
	}
	if !isHex64(value.ReceiptSHA256) || !isHex64(value.OperationalPayloadDigest) {
		return errors.New("executioncell: preflight registration digests must be lowercase SHA-256")
	}
	receipt, err := base64.StdEncoding.DecodeString(value.ReceiptBytesBase64)
	if err != nil || len(receipt) == 0 || len(receipt) > MaxPreflightRegistrationReceiptBytes || base64.StdEncoding.EncodeToString(receipt) != value.ReceiptBytesBase64 {
		return errors.New("executioncell: preflight registration receipt bytes are invalid")
	}
	digest := sha256.Sum256(receipt)
	if hex.EncodeToString(digest[:]) != value.ReceiptSHA256 {
		return errors.New("executioncell: preflight registration receipt digest mismatch")
	}
	host, err := DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		return err
	}
	binding := value.RuntimeBinding
	if host.RequestID != binding.RequestID || host.WorkerID != binding.WorkerID || host.PlacementID != binding.PlacementID || host.ClaimID != binding.ClaimID {
		return errors.New("executioncell: preflight registration receipt binding mismatch")
	}
	if err := ValidateHostAdaptationForRegistration(binding, receipt, value.OperationalPayloadDigest); err != nil {
		return err
	}
	return nil
}

// DecodePreflightRegistrationRequest rejects unknown, duplicate, trailing and
// semantically invalid input.
func DecodePreflightRegistrationRequest(raw []byte) (PreflightRegistrationRequest, error) {
	members, err := rawObjectMembers(raw, "preflight registration request")
	if err != nil {
		return PreflightRegistrationRequest{}, err
	}
	bindingRaw, present := members["runtimeBinding"]
	if !present {
		return PreflightRegistrationRequest{}, errors.New("executioncell: preflight registration runtimeBinding is required")
	}
	if _, err := DecodeRuntimeBinding(bindingRaw); err != nil {
		return PreflightRegistrationRequest{}, err
	}
	var value PreflightRegistrationRequest
	if err := decodeClosed(raw, &value, "preflight registration request"); err != nil {
		return PreflightRegistrationRequest{}, err
	}
	if err := ValidatePreflightRegistrationRequest(value); err != nil {
		return PreflightRegistrationRequest{}, err
	}
	return value, nil
}

// ReceiptBytes returns a fresh copy of the exact validated receipt bytes.
func (value PreflightRegistrationRequest) ReceiptBytes() ([]byte, error) {
	if err := ValidatePreflightRegistrationRequest(value); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(value.ReceiptBytesBase64)
}

func validRefusalCode(code PreflightRegistrationRefusalCode) bool {
	switch code {
	case PreflightBindingMismatch, PreflightReceiptInvalid, PreflightReceiptDenied,
		PreflightClaimRetired, PreflightAuthorizationUnavailable, PreflightAlreadyStarted:
		return true
	default:
		return false
	}
}

// ValidatePreflightRegistrationResponse validates the closed response union.
func ValidatePreflightRegistrationResponse(value PreflightRegistrationResponse) error {
	if value.ContractVersion != PreflightRegistrationContractVersion {
		return fmt.Errorf("executioncell: unsupported preflight registration response version %q", value.ContractVersion)
	}
	switch value.Decision {
	case "authorized":
		if !validRuntimeRef(value.RegistrationID) || !isHex64(value.ReceiptSHA256) || value.RuntimeBinding == nil || value.AuthorizationRevision == 0 || value.Code != "" {
			return errors.New("executioncell: authorized preflight registration response is incomplete")
		}
		if value.RuntimeBinding.ContractVersion != RuntimeBindingV2ContractVersion {
			return errors.New("executioncell: authorized preflight registration response requires runtime binding v2")
		}
		return validateRuntimeBindingValue(*value.RuntimeBinding)
	case "refused":
		if !validRefusalCode(value.Code) || value.RegistrationID != "" || value.ReceiptSHA256 != "" || value.RuntimeBinding != nil || value.AuthorizationRevision != 0 {
			return errors.New("executioncell: refused preflight registration response is invalid")
		}
		return nil
	default:
		return errors.New("executioncell: preflight registration response decision must be authorized or refused")
	}
}

// DecodePreflightRegistrationResponse rejects unknown, duplicate, trailing and
// semantically invalid input.
func DecodePreflightRegistrationResponse(raw []byte) (PreflightRegistrationResponse, error) {
	members, err := rawObjectMembers(raw, "preflight registration response")
	if err != nil {
		return PreflightRegistrationResponse{}, err
	}
	var value PreflightRegistrationResponse
	if err := decodeClosed(raw, &value, "preflight registration response"); err != nil {
		return PreflightRegistrationResponse{}, err
	}
	switch value.Decision {
	case "authorized":
		if _, present := members["code"]; present {
			return PreflightRegistrationResponse{}, errors.New("executioncell: authorized preflight registration response cannot contain refusal fields")
		}
		bindingRaw, present := members["runtimeBinding"]
		if !present {
			return PreflightRegistrationResponse{}, errors.New("executioncell: authorized preflight registration runtimeBinding is required")
		}
		if _, err := DecodeRuntimeBinding(bindingRaw); err != nil {
			return PreflightRegistrationResponse{}, err
		}
	case "refused":
		for _, key := range []string{"registrationId", "receiptSha256", "runtimeBinding", "authorizationRevision"} {
			if _, present := members[key]; present {
				return PreflightRegistrationResponse{}, errors.New("executioncell: refused preflight registration response cannot contain authorization fields")
			}
		}
	}
	if err := ValidatePreflightRegistrationResponse(value); err != nil {
		return PreflightRegistrationResponse{}, err
	}
	return value, nil
}

// ValidateAuthorizedPreflightRegistration proves an authenticated response
// authorizes these exact receipt bytes and runtime binding.
func ValidateAuthorizedPreflightRegistration(request PreflightRegistrationRequest, response PreflightRegistrationResponse) error {
	if err := ValidatePreflightRegistrationRequest(request); err != nil {
		return err
	}
	if err := ValidatePreflightRegistrationResponse(response); err != nil {
		return err
	}
	if response.Decision != "authorized" {
		return fmt.Errorf("executioncell: preflight registration refused: %s", response.Code)
	}
	receipt, err := request.ReceiptBytes()
	if err != nil {
		return err
	}
	host, err := DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		return err
	}
	if host.Decision != "ready" {
		return errors.New("executioncell: denied host adaptation cannot be authorized")
	}
	if response.ReceiptSHA256 != request.ReceiptSHA256 || response.RuntimeBinding == nil || !reflect.DeepEqual(*response.RuntimeBinding, request.RuntimeBinding) {
		return errors.New("executioncell: preflight registration acknowledgement mismatch")
	}
	return nil
}
