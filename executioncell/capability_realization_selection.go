package executioncell

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// CapabilityRealizationSelectionContractVersionV1 is the closed operational
// payload selection contract. Authorization and registry membership are
// deliberately verified by later consumers.
const CapabilityRealizationSelectionContractVersionV1 = "donmai.capability-realization-selection/v1"

// CapabilityRealizationSelectionV1 identifies the one admitted realization
// that downstream host and child consumers must independently verify.
type CapabilityRealizationSelectionV1 struct {
	ContractVersion       string `json:"contractVersion"`
	CapabilityID          string `json:"capabilityId"`
	HarnessID             string `json:"harnessId"`
	AdapterVersion        string `json:"adapterVersion"`
	Mode                  string `json:"mode"`
	RecipeDigest          string `json:"recipeDigest"`
	DeclaredSurfaceDigest string `json:"declaredSurfaceDigest"`
	ObservationDigest     string `json:"observationDigest"`
}

const maxCapabilityRealizationSelectionBytes = 8192

var (
	capabilityRealizationSelectionRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,255}$`)
	capabilityRealizationSelectionFields = []string{
		"contractVersion", "capabilityId", "harnessId", "adapterVersion", "mode",
		"recipeDigest", "declaredSurfaceDigest", "observationDigest",
	}
)

func exactCapabilityRealizationSelectionObject(raw []byte) error {
	members, err := rawObjectMembers(raw, "capability realization selection")
	if err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(capabilityRealizationSelectionFields))
	for _, field := range capabilityRealizationSelectionFields {
		allowed[field] = struct{}{}
	}
	for field := range members {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("executioncell: capability realization selection has non-canonical field %q", field)
		}
	}
	for _, field := range capabilityRealizationSelectionFields {
		if _, ok := members[field]; !ok {
			return fmt.Errorf("executioncell: capability realization selection is missing field %q", field)
		}
	}
	return nil
}

func validateCapabilityRealizationSelectionBytes(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("executioncell: capability realization selection must be valid UTF-8")
	}
	if len(raw) > maxCapabilityRealizationSelectionBytes {
		return fmt.Errorf("executioncell: capability realization selection exceeds %d bytes", maxCapabilityRealizationSelectionBytes)
	}
	return nil
}

// ValidateCapabilityRealizationSelectionV1 validates only the closed wire
// grammar. Exact registry membership and evidence-digest equality are later
// authorization checks and are intentionally outside this codec.
func ValidateCapabilityRealizationSelectionV1(value CapabilityRealizationSelectionV1) error {
	if value.ContractVersion != CapabilityRealizationSelectionContractVersionV1 {
		return fmt.Errorf("executioncell: unsupported capability realization selection version %q", value.ContractVersion)
	}
	if !capabilityRealizationSelectionRef.MatchString(value.CapabilityID) ||
		value.HarnessID == "" ||
		!capabilityRealizationSelectionRef.MatchString(value.AdapterVersion) {
		return errors.New("executioncell: capability realization selection identity is invalid")
	}
	if value.Mode != "autonomous" && value.Mode != "human_controlled" {
		return errors.New("executioncell: capability realization selection mode is invalid")
	}
	if !isHex64(value.RecipeDigest) || !isHex64(value.DeclaredSurfaceDigest) || !isHex64(value.ObservationDigest) {
		return errors.New("executioncell: capability realization selection digests must be lowercase SHA-256")
	}
	return nil
}

// DecodeCapabilityRealizationSelectionV1 strictly decodes one selection. It
// rejects non-canonical keys before encoding/json's case-insensitive matching
// can normalize them.
func DecodeCapabilityRealizationSelectionV1(raw []byte) (CapabilityRealizationSelectionV1, error) {
	if err := validateCapabilityRealizationSelectionBytes(raw); err != nil {
		return CapabilityRealizationSelectionV1{}, err
	}
	if err := rejectDuplicateFields(raw); err != nil {
		return CapabilityRealizationSelectionV1{}, err
	}
	if err := exactCapabilityRealizationSelectionObject(raw); err != nil {
		return CapabilityRealizationSelectionV1{}, err
	}
	var value CapabilityRealizationSelectionV1
	if err := decodeClosed(raw, &value, "capability realization selection"); err != nil {
		return CapabilityRealizationSelectionV1{}, err
	}
	if err := ValidateCapabilityRealizationSelectionV1(value); err != nil {
		return CapabilityRealizationSelectionV1{}, err
	}
	return value, nil
}

// ExtractCapabilityRealizationSelectionV1 reads the closed selection from its
// retained operational payload. A missing member is the only absence case;
// present null and malformed selections refuse.
func ExtractCapabilityRealizationSelectionV1(operationalPayload []byte) (*CapabilityRealizationSelectionV1, error) {
	if !utf8.Valid(operationalPayload) {
		return nil, errors.New("executioncell: operational payload must be valid UTF-8")
	}
	if err := rejectDuplicateFields(operationalPayload); err != nil {
		return nil, err
	}
	members, err := rawObjectMembers(operationalPayload, "operational payload")
	if err != nil {
		return nil, err
	}
	raw, present := members["capabilityRealizationSelection"]
	if !present {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("executioncell: capability realization selection must not be null")
	}
	value, err := DecodeCapabilityRealizationSelectionV1(raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// CanonicalCapabilityRealizationSelectionV1 returns RFC 8785 bytes only after
// the structural selection grammar has been validated.
func CanonicalCapabilityRealizationSelectionV1(value CapabilityRealizationSelectionV1) ([]byte, error) {
	if err := ValidateCapabilityRealizationSelectionV1(value); err != nil {
		return nil, err
	}
	return CanonicalJSON(value)
}
