package executioncell

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// RuntimeBindingContractVersion identifies the authenticated poll-to-host
// execution target. The binding is deliberately separate from receipt and
// effective-cell evidence: the authenticated poll claim owns these values.
const (
	RuntimeBindingContractVersion = "execution-runtime-binding/v1"
	// HostAdaptationContractVersion identifies the daemon's durable pre-credential receipt.
	HostAdaptationContractVersion = "host-adaptation/v1"
)

// RuntimeBinding binds receipt-bearing work to the request, current worker,
// effective placement, and (for claim-bound work) active claim that owns it.
type RuntimeBinding struct {
	ContractVersion string `json:"contractVersion"`
	RequestID       string `json:"requestId"`
	WorkerID        string `json:"workerId"`
	PlacementID     string `json:"placementId"`
	ClaimID         string `json:"claimId,omitempty"`
}

// HostAdaptationReceipt is the secret-free ready-or-denied envelope produced
// by the host compiler before credentials or child processes exist.
type HostAdaptationReceipt struct {
	ContractVersion      string          `json:"contractVersion"`
	RequestID            string          `json:"requestId"`
	WorkerID             string          `json:"workerId"`
	PlacementID          string          `json:"placementId"`
	ClaimID              string          `json:"claimId,omitempty"`
	Decision             string          `json:"decision"`
	Plan                 json.RawMessage `json:"plan,omitempty"`
	PlanDigest           string          `json:"planDigest,omitempty"`
	PromptReceipt        json.RawMessage `json:"promptReceipt,omitempty"`
	ToolLifecycleReceipt json.RawMessage `json:"toolLifecycleReceipt,omitempty"`
	Denial               string          `json:"denial,omitempty"`
}

// DecodeHostAdaptationReceipt strictly decodes a closed host receipt.
func DecodeHostAdaptationReceipt(raw []byte) (HostAdaptationReceipt, error) {
	if err := rejectDuplicateFields(raw); err != nil {
		return HostAdaptationReceipt{}, err
	}
	var receipt HostAdaptationReceipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return HostAdaptationReceipt{}, fmt.Errorf("executioncell: decode host adaptation receipt: %w", err)
	}
	if receipt.ContractVersion != HostAdaptationContractVersion || strings.TrimSpace(receipt.RequestID) == "" || strings.TrimSpace(receipt.WorkerID) == "" || strings.TrimSpace(receipt.PlacementID) == "" {
		return HostAdaptationReceipt{}, errors.New("executioncell: invalid host adaptation receipt identity")
	}
	if receipt.Decision != "ready" && receipt.Decision != "denied" {
		return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation decision must be ready or denied")
	}
	if receipt.Decision == "ready" && (len(receipt.Plan) == 0 || receipt.PlanDigest == "" || len(receipt.PromptReceipt) == 0 || len(receipt.ToolLifecycleReceipt) == 0 || receipt.Denial != "") {
		return HostAdaptationReceipt{}, errors.New("executioncell: ready host adaptation requires complete receipts and no denial")
	}
	if receipt.Decision == "ready" {
		sum := sha256.Sum256(receipt.Plan)
		if hex.EncodeToString(sum[:]) != receipt.PlanDigest {
			return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation plan digest mismatch")
		}
		for name, nested := range map[string]json.RawMessage{"promptReceipt": receipt.PromptReceipt, "toolLifecycleReceipt": receipt.ToolLifecycleReceipt} {
			var projection struct {
				Decision string `json:"decision"`
			}
			if err := json.Unmarshal(nested, &projection); err != nil || projection.Decision != "ready" {
				return HostAdaptationReceipt{}, fmt.Errorf("executioncell: ready host adaptation has non-ready %s", name)
			}
		}
	}
	return receipt, nil
}

// DecodeRuntimeBinding is a closed decoder. Unknown or duplicate fields fail.
func DecodeRuntimeBinding(raw []byte) (RuntimeBinding, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return RuntimeBinding{}, errors.New("executioncell: runtime binding is required")
	}
	if err := rejectDuplicateFields(raw); err != nil {
		return RuntimeBinding{}, err
	}
	var value RuntimeBinding
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return RuntimeBinding{}, fmt.Errorf("executioncell: decode runtime binding: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeBinding{}, errors.New("executioncell: runtime binding has trailing JSON")
	}
	if value.ContractVersion != RuntimeBindingContractVersion {
		return RuntimeBinding{}, fmt.Errorf("executioncell: unsupported runtime binding version %q", value.ContractVersion)
	}
	if strings.TrimSpace(value.RequestID) == "" || strings.TrimSpace(value.WorkerID) == "" || strings.TrimSpace(value.PlacementID) == "" {
		return RuntimeBinding{}, errors.New("executioncell: runtime binding requestId, workerId, and placementId are required")
	}
	return value, nil
}

var operationalSidecarKeys = map[string]struct{}{
	"admissionReceipt": {}, "claimReceipt": {}, "effectiveCell": {},
	"executionRuntimeBinding": {}, "operationalPayload": {},
	"hostAdaptationReceipt": {},
}

// ProjectOperationalPayload projects raw poll JSON once, before any typed
// omitempty mirror can erase present-empty state. Producers and verifiers use
// these exact canonical bytes; contract/evidence sidecars are the only fields
// excluded from the operational payload.
func ProjectOperationalPayload(raw []byte) ([]byte, error) {
	if err := rejectDuplicateFields(raw); err != nil {
		return nil, err
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("executioncell: decode operational source: %w", err)
	}
	if document == nil {
		return nil, errors.New("executioncell: operational source must be an object")
	}
	for key := range operationalSidecarKeys {
		delete(document, key)
	}
	return CanonicalJSON(document)
}

// NormalizeOperationalPayload validates and canonicalizes an already-projected
// payload without reconstructing it through a typed wire shape.
func NormalizeOperationalPayload(raw []byte) ([]byte, error) {
	if err := rejectDuplicateFields(raw); err != nil {
		return nil, err
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("executioncell: decode operational payload: %w", err)
	}
	if document == nil {
		return nil, errors.New("executioncell: operational payload must be an object")
	}
	for key := range operationalSidecarKeys {
		if _, exists := document[key]; exists {
			return nil, fmt.Errorf("executioncell: operational payload contains forbidden sidecar %q", key)
		}
	}
	return CanonicalJSON(document)
}

// DigestOperationalPayload returns the stable SHA-256 identifier used by the
// admission receipt.
func DigestOperationalPayload(raw []byte) (string, error) {
	canonical, err := NormalizeOperationalPayload(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
