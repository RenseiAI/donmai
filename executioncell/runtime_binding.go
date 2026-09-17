package executioncell

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var stableRuntimeRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$`)

func validRuntimeRef(value string) bool { return stableRuntimeRef.MatchString(value) }

func rawObjectMembers(raw []byte, label string) (map[string]json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, fmt.Errorf("executioncell: %s must be an object", label)
	}
	return members, nil
}

func presentJSONString(members map[string]json.RawMessage, key string) (string, bool) {
	raw, present := members[key]
	if !present {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// RuntimeBindingContractVersion identifies the authenticated poll-to-host
// execution target. The binding is deliberately separate from receipt and
// effective-cell evidence: the authenticated poll claim owns these values.
const (
	RuntimeBindingContractVersion = "execution-runtime-binding/v1"
	// RuntimeBindingV2ContractVersion requires controller registration of the
	// exact fsynced host receipt before credentials or spawn.
	RuntimeBindingV2ContractVersion = "execution-runtime-binding/v2"
	// PreflightRegistrationContractVersion identifies both the exact receipt
	// registration request and its authenticated acknowledgement.
	PreflightRegistrationContractVersion = "execution-preflight-registration/v1"
	// HostAdaptationContractVersion identifies the daemon's durable pre-credential receipt.
	HostAdaptationContractVersion = "host-adaptation/v1"
	// HostAdaptationV2ContractVersion includes applied common-config evidence.
	HostAdaptationV2ContractVersion = "host-adaptation/v2"
	// HostAdaptationV3ContractVersion includes protected runtime MCP evidence.
	HostAdaptationV3ContractVersion = "host-adaptation/v3"
)

// RuntimeBinding binds receipt-bearing work to the request, current worker,
// effective placement, and (for claim-bound work) active claim that owns it.
type RuntimeBinding struct {
	ContractVersion       string                    `json:"contractVersion"`
	RequestID             string                    `json:"requestId"`
	WorkerID              string                    `json:"workerId"`
	PlacementID           string                    `json:"placementId"`
	ClaimID               string                    `json:"claimId,omitempty"`
	PreflightRegistration *PreflightRegistrationRef `json:"preflightRegistration,omitempty"`
}

// PreflightRegistrationRef is public correlation minted by the admission
// owner. It is not a bearer capability or an independent authorization.
type PreflightRegistrationRef struct {
	ContractVersion string `json:"contractVersion"`
	Required        bool   `json:"required"`
	ChallengeID     string `json:"challengeId"`
}

// HostAdaptationReceipt is the secret-free ready-or-denied envelope produced
// by the host compiler before credentials or child processes exist.
type HostAdaptationReceipt struct {
	ContractVersion              string                                       `json:"contractVersion"`
	RequestID                    string                                       `json:"requestId"`
	WorkerID                     string                                       `json:"workerId"`
	PlacementID                  string                                       `json:"placementId"`
	ClaimID                      string                                       `json:"claimId,omitempty"`
	Decision                     string                                       `json:"decision"`
	Plan                         json.RawMessage                              `json:"plan,omitempty"`
	PlanDigest                   string                                       `json:"planDigest,omitempty"`
	PromptReceipt                json.RawMessage                              `json:"promptReceipt,omitempty"`
	ToolLifecycleReceipt         json.RawMessage                              `json:"toolLifecycleReceipt,omitempty"`
	ConfigMaterializations       []PreflightConfigMaterializationV1           `json:"configMaterializations,omitempty"`
	ProtectedRuntimeMCPConfigs   []ProtectedRuntimeMCPConfigMaterializationV1 `json:"protectedRuntimeMcpConfigs,omitempty"`
	ProtectedRuntimeMCPConfigsV2 []ProtectedRuntimeMCPConfigMaterializationV2 `json:"-"`
	Denial                       string                                       `json:"denial,omitempty"`
}

type hostAdaptationReceiptAlias HostAdaptationReceipt

type hostAdaptationReceiptV2Wire struct {
	ContractVersion            string                                       `json:"contractVersion"`
	RequestID                  string                                       `json:"requestId"`
	WorkerID                   string                                       `json:"workerId"`
	PlacementID                string                                       `json:"placementId"`
	ClaimID                    string                                       `json:"claimId,omitempty"`
	Decision                   string                                       `json:"decision"`
	Plan                       json.RawMessage                              `json:"plan,omitempty"`
	PlanDigest                 string                                       `json:"planDigest,omitempty"`
	PromptReceipt              json.RawMessage                              `json:"promptReceipt,omitempty"`
	ToolLifecycleReceipt       json.RawMessage                              `json:"toolLifecycleReceipt,omitempty"`
	ConfigMaterializations     []PreflightConfigMaterializationV1           `json:"configMaterializations,omitempty"`
	ProtectedRuntimeMCPConfigs []ProtectedRuntimeMCPConfigMaterializationV2 `json:"protectedRuntimeMcpConfigs"`
	Denial                     string                                       `json:"denial,omitempty"`
}

// MarshalJSON preserves every v1 receipt's historical encoding while placing
// the closed v2 variant under the same protectedRuntimeMcpConfigs wire key.
func (value HostAdaptationReceipt) MarshalJSON() ([]byte, error) {
	if len(value.ProtectedRuntimeMCPConfigsV2) == 0 {
		return json.Marshal(hostAdaptationReceiptAlias(value))
	}
	if len(value.ProtectedRuntimeMCPConfigs) != 0 {
		return nil, errors.New("executioncell: host adaptation cannot mix protected runtime MCP config versions")
	}
	return json.Marshal(hostAdaptationReceiptV2Wire{
		ContractVersion: value.ContractVersion, RequestID: value.RequestID,
		WorkerID: value.WorkerID, PlacementID: value.PlacementID, ClaimID: value.ClaimID,
		Decision: value.Decision, Plan: value.Plan, PlanDigest: value.PlanDigest,
		PromptReceipt: value.PromptReceipt, ToolLifecycleReceipt: value.ToolLifecycleReceipt,
		ConfigMaterializations:     value.ConfigMaterializations,
		ProtectedRuntimeMCPConfigs: value.ProtectedRuntimeMCPConfigsV2,
		Denial:                     value.Denial,
	})
}

type hostAdaptationReceiptDecodeWire struct {
	ContractVersion            string                             `json:"contractVersion"`
	RequestID                  string                             `json:"requestId"`
	WorkerID                   string                             `json:"workerId"`
	PlacementID                string                             `json:"placementId"`
	ClaimID                    string                             `json:"claimId,omitempty"`
	Decision                   string                             `json:"decision"`
	Plan                       json.RawMessage                    `json:"plan,omitempty"`
	PlanDigest                 string                             `json:"planDigest,omitempty"`
	PromptReceipt              json.RawMessage                    `json:"promptReceipt,omitempty"`
	ToolLifecycleReceipt       json.RawMessage                    `json:"toolLifecycleReceipt,omitempty"`
	ConfigMaterializations     []PreflightConfigMaterializationV1 `json:"configMaterializations,omitempty"`
	ProtectedRuntimeMCPConfigs []json.RawMessage                  `json:"protectedRuntimeMcpConfigs,omitempty"`
	Denial                     string                             `json:"denial,omitempty"`
}

func decodeProtectedRuntimeMCPMaterializations(rawValues []json.RawMessage) ([]ProtectedRuntimeMCPConfigMaterializationV1, []ProtectedRuntimeMCPConfigMaterializationV2, error) {
	var v1 []ProtectedRuntimeMCPConfigMaterializationV1
	var v2 []ProtectedRuntimeMCPConfigMaterializationV2
	for _, raw := range rawValues {
		members, err := rawObjectMembers(raw, "protected runtime MCP config materialization")
		if err != nil {
			return nil, nil, err
		}
		version, ok := presentJSONString(members, "contractVersion")
		if !ok {
			return nil, nil, errors.New("executioncell: protected runtime MCP config materialization requires exact contractVersion")
		}
		switch version {
		case ProtectedRuntimeMCPConfigContractVersion:
			value, err := DecodeProtectedRuntimeMCPConfigMaterializationV1(raw)
			if err != nil {
				return nil, nil, err
			}
			v1 = append(v1, value)
		case ProtectedRuntimeMCPConfigContractVersionV2:
			value, err := DecodeProtectedRuntimeMCPConfigMaterializationV2(raw)
			if err != nil {
				return nil, nil, err
			}
			v2 = append(v2, value)
		default:
			return nil, nil, fmt.Errorf("executioncell: unsupported protected runtime MCP config version %q", version)
		}
	}
	if len(v1) != 0 && len(v2) != 0 {
		return nil, nil, errors.New("executioncell: host adaptation cannot mix protected runtime MCP config versions")
	}
	return v1, v2, nil
}

// DecodeHostAdaptationReceipt strictly decodes a closed host receipt.
func DecodeHostAdaptationReceipt(raw []byte) (HostAdaptationReceipt, error) {
	if err := rejectDuplicateFields(raw); err != nil {
		return HostAdaptationReceipt{}, err
	}
	var wire hostAdaptationReceiptDecodeWire
	members, err := rawObjectMembers(raw, "host adaptation receipt")
	if err != nil {
		return HostAdaptationReceipt{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return HostAdaptationReceipt{}, fmt.Errorf("executioncell: decode host adaptation receipt: %w", err)
	}
	v1, v2, err := decodeProtectedRuntimeMCPMaterializations(wire.ProtectedRuntimeMCPConfigs)
	if err != nil {
		return HostAdaptationReceipt{}, err
	}
	receipt := HostAdaptationReceipt{
		ContractVersion: wire.ContractVersion, RequestID: wire.RequestID,
		WorkerID: wire.WorkerID, PlacementID: wire.PlacementID, ClaimID: wire.ClaimID,
		Decision: wire.Decision, Plan: wire.Plan, PlanDigest: wire.PlanDigest,
		PromptReceipt: wire.PromptReceipt, ToolLifecycleReceipt: wire.ToolLifecycleReceipt,
		ConfigMaterializations:     wire.ConfigMaterializations,
		ProtectedRuntimeMCPConfigs: v1, ProtectedRuntimeMCPConfigsV2: v2,
		Denial: wire.Denial,
	}
	if (receipt.ContractVersion != HostAdaptationContractVersion && receipt.ContractVersion != HostAdaptationV2ContractVersion && receipt.ContractVersion != HostAdaptationV3ContractVersion) || strings.TrimSpace(receipt.RequestID) == "" || strings.TrimSpace(receipt.WorkerID) == "" || strings.TrimSpace(receipt.PlacementID) == "" {
		return HostAdaptationReceipt{}, errors.New("executioncell: invalid host adaptation receipt identity")
	}
	if receipt.Decision != "ready" && receipt.Decision != "denied" {
		return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation decision must be ready or denied")
	}
	_, configsPresent := members["configMaterializations"]
	_, protectedMCPPresent := members["protectedRuntimeMcpConfigs"]
	if receipt.ContractVersion == HostAdaptationContractVersion && configsPresent {
		return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation v1 cannot carry config materializations")
	}
	if (receipt.ContractVersion == HostAdaptationContractVersion || receipt.ContractVersion == HostAdaptationV2ContractVersion) && protectedMCPPresent {
		return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation v1/v2 cannot carry protected runtime MCP configs")
	}
	if receipt.ContractVersion == HostAdaptationV2ContractVersion && receipt.Decision != "ready" {
		return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation v2 requires a ready decision with config materializations")
	}
	if receipt.ContractVersion == HostAdaptationV3ContractVersion && receipt.Decision != "ready" {
		return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation v3 requires a ready decision with protected runtime MCP configs")
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
		if receipt.ContractVersion == HostAdaptationV2ContractVersion || (receipt.ContractVersion == HostAdaptationV3ContractVersion && configsPresent) {
			if !configsPresent {
				return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation v2 requires config materializations")
			}
			if err := ValidatePreflightConfigMaterializations(receipt.ConfigMaterializations); err != nil {
				return HostAdaptationReceipt{}, err
			}
			var planIdentity struct {
				OperationalPayloadDigest string `json:"operationalPayloadDigest"`
			}
			if err := json.Unmarshal(receipt.Plan, &planIdentity); err != nil || !validSHA256(planIdentity.OperationalPayloadDigest) {
				return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation v2 plan operational digest is invalid")
			}
			for _, materialization := range receipt.ConfigMaterializations {
				if materialization.OperationalPayloadDigest != planIdentity.OperationalPayloadDigest {
					return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation config operational digest mismatch")
				}
			}
		}
		if receipt.ContractVersion == HostAdaptationV3ContractVersion {
			if !protectedMCPPresent {
				return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation v3 requires protected runtime MCP configs")
			}
			switch {
			case len(receipt.ProtectedRuntimeMCPConfigs) > 0:
				if err := ValidateProtectedRuntimeMCPConfigMaterializations(receipt.ProtectedRuntimeMCPConfigs); err != nil {
					return HostAdaptationReceipt{}, err
				}
			case len(receipt.ProtectedRuntimeMCPConfigsV2) > 0:
				if err := ValidateProtectedRuntimeMCPConfigMaterializationsV2(receipt.ProtectedRuntimeMCPConfigsV2); err != nil {
					return HostAdaptationReceipt{}, err
				}
			default:
				return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation v3 requires protected runtime MCP configs")
			}
			var planIdentity struct {
				OperationalPayloadDigest string `json:"operationalPayloadDigest"`
			}
			if err := json.Unmarshal(receipt.Plan, &planIdentity); err != nil || !validSHA256(planIdentity.OperationalPayloadDigest) {
				return HostAdaptationReceipt{}, errors.New("executioncell: host adaptation v3 plan operational digest is invalid")
			}
			for _, materialization := range receipt.ProtectedRuntimeMCPConfigs {
				if materialization.OperationalPayloadDigest != planIdentity.OperationalPayloadDigest {
					return HostAdaptationReceipt{}, errors.New("executioncell: protected runtime MCP config operational digest mismatch")
				}
			}
			for _, materialization := range receipt.ProtectedRuntimeMCPConfigsV2 {
				if materialization.OperationalPayloadDigest != planIdentity.OperationalPayloadDigest {
					return HostAdaptationReceipt{}, errors.New("executioncell: protected runtime MCP v2 config operational digest mismatch")
				}
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
	members, err := rawObjectMembers(raw, "runtime binding")
	if err != nil {
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
	if value.ContractVersion != RuntimeBindingContractVersion && value.ContractVersion != RuntimeBindingV2ContractVersion {
		return RuntimeBinding{}, fmt.Errorf("executioncell: unsupported runtime binding version %q", value.ContractVersion)
	}
	if strings.TrimSpace(value.RequestID) == "" || strings.TrimSpace(value.WorkerID) == "" || strings.TrimSpace(value.PlacementID) == "" {
		return RuntimeBinding{}, errors.New("executioncell: runtime binding requestId, workerId, and placementId are required")
	}
	if value.ContractVersion == RuntimeBindingContractVersion {
		if _, present := members["preflightRegistration"]; present {
			return RuntimeBinding{}, errors.New("executioncell: runtime binding v1 cannot require preflight registration")
		}
	}
	if _, present := members["claimId"]; present {
		if decoded, ok := presentJSONString(members, "claimId"); !ok || decoded == "" || decoded != value.ClaimID {
			return RuntimeBinding{}, errors.New("executioncell: runtime binding claimId must be an omitted or nonempty string")
		}
	}
	if value.ContractVersion == RuntimeBindingContractVersion {
		return value, nil
	}
	registration := value.PreflightRegistration
	if registration == nil || registration.ContractVersion != PreflightRegistrationContractVersion || !registration.Required ||
		!validRuntimeRef(value.RequestID) || !validRuntimeRef(value.WorkerID) || !validRuntimeRef(value.PlacementID) ||
		(value.ClaimID != "" && !validRuntimeRef(value.ClaimID)) || !validRuntimeRef(registration.ChallengeID) {
		return RuntimeBinding{}, errors.New("executioncell: runtime binding v2 requires exact preflight registration")
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
