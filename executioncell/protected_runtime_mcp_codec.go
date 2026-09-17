package executioncell

import (
	"encoding/json"
	"fmt"
)

var (
	protectedRuntimeMCPRequirementV1Fields = []string{
		"contractVersion", "requirementId", "authorityBindingDigest", "operationalPayloadDigest",
		"serverName", "transport", "endpointDigest", "headers",
	}
	protectedRuntimeMCPMaterializationV1Fields = append(
		append([]string(nil), protectedRuntimeMCPRequirementV1Fields...),
		"configReferenceDigest",
	)
	protectedRuntimeMCPRequirementV2Fields = append(
		append([]string(nil), protectedRuntimeMCPRequirementV1Fields...),
		"authorizationSource",
	)
	protectedRuntimeMCPMaterializationV2Fields = append(
		append([]string(nil), protectedRuntimeMCPRequirementV2Fields...),
		"configReferenceDigest",
	)
	protectedRuntimeMCPAuthorizationSourceV2Fields = []string{
		"kind", "configRequirementId", "targetEnv", "mode",
	}
	protectedRuntimeMCPAuthorizationSourceMaterializationV2Fields = append(
		append([]string(nil), protectedRuntimeMCPAuthorizationSourceV2Fields...),
		"configReferenceDigest", "fileReferenceDigest", "helperCommandDigest",
	)
	protectedRuntimeMCPHeaderFields = []string{"name", "valueDigest"}
)

func exactProtectedRuntimeMCPObject(raw []byte, label string, fields []string) (map[string]json.RawMessage, error) {
	members, err := rawObjectMembers(raw, label)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field] = struct{}{}
	}
	for field := range members {
		if _, ok := allowed[field]; !ok {
			return nil, fmt.Errorf("executioncell: %s has non-canonical field %q", label, field)
		}
	}
	for _, field := range fields {
		if _, ok := members[field]; !ok {
			return nil, fmt.Errorf("executioncell: %s is missing field %q", label, field)
		}
	}
	return members, nil
}

func validateProtectedRuntimeMCPWireKeys(raw []byte, label string, rootFields, sourceFields []string) error {
	members, err := exactProtectedRuntimeMCPObject(raw, label, rootFields)
	if err != nil {
		return err
	}
	var headers []json.RawMessage
	if err := json.Unmarshal(members["headers"], &headers); err != nil {
		return fmt.Errorf("executioncell: decode %s headers: %w", label, err)
	}
	for index, header := range headers {
		if _, err := exactProtectedRuntimeMCPObject(header, fmt.Sprintf("%s header %d", label, index), protectedRuntimeMCPHeaderFields); err != nil {
			return err
		}
	}
	if sourceFields != nil {
		if _, err := exactProtectedRuntimeMCPObject(members["authorizationSource"], label+" authorization source", sourceFields); err != nil {
			return err
		}
	}
	return nil
}

// DecodeProtectedRuntimeMCPConfigRequirementV1 strictly decodes and validates
// one v1 protected runtime MCP requirement. V2-only fields are rejected.
func DecodeProtectedRuntimeMCPConfigRequirementV1(raw []byte) (ProtectedRuntimeMCPConfigRequirementV1, error) {
	if err := validateProtectedRuntimeMCPWireKeys(raw, "protected runtime MCP config requirement v1", protectedRuntimeMCPRequirementV1Fields, nil); err != nil {
		return ProtectedRuntimeMCPConfigRequirementV1{}, err
	}
	var value ProtectedRuntimeMCPConfigRequirementV1
	if err := decodeClosed(raw, &value, "protected runtime MCP config requirement v1"); err != nil {
		return ProtectedRuntimeMCPConfigRequirementV1{}, err
	}
	if err := ValidateProtectedRuntimeMCPConfigRequirement(value); err != nil {
		return ProtectedRuntimeMCPConfigRequirementV1{}, err
	}
	return value, nil
}

// DecodeProtectedRuntimeMCPConfigMaterializationV1 strictly decodes and
// validates one v1 protected runtime MCP materialization. V2-only fields are rejected.
func DecodeProtectedRuntimeMCPConfigMaterializationV1(raw []byte) (ProtectedRuntimeMCPConfigMaterializationV1, error) {
	if err := validateProtectedRuntimeMCPWireKeys(raw, "protected runtime MCP config materialization v1", protectedRuntimeMCPMaterializationV1Fields, nil); err != nil {
		return ProtectedRuntimeMCPConfigMaterializationV1{}, err
	}
	var value ProtectedRuntimeMCPConfigMaterializationV1
	if err := decodeClosed(raw, &value, "protected runtime MCP config materialization v1"); err != nil {
		return ProtectedRuntimeMCPConfigMaterializationV1{}, err
	}
	if err := ValidateProtectedRuntimeMCPConfigMaterialization(value); err != nil {
		return ProtectedRuntimeMCPConfigMaterializationV1{}, err
	}
	return value, nil
}

// DecodeProtectedRuntimeMCPConfigRequirementV2 strictly decodes and validates
// one v2 protected runtime MCP requirement. Structural validity is not proof of
// an authoritative common-materialization join.
func DecodeProtectedRuntimeMCPConfigRequirementV2(raw []byte) (ProtectedRuntimeMCPConfigRequirementV2, error) {
	if err := validateProtectedRuntimeMCPWireKeys(raw, "protected runtime MCP config requirement v2", protectedRuntimeMCPRequirementV2Fields, protectedRuntimeMCPAuthorizationSourceV2Fields); err != nil {
		return ProtectedRuntimeMCPConfigRequirementV2{}, err
	}
	var value ProtectedRuntimeMCPConfigRequirementV2
	if err := decodeClosed(raw, &value, "protected runtime MCP config requirement v2"); err != nil {
		return ProtectedRuntimeMCPConfigRequirementV2{}, err
	}
	if err := ValidateProtectedRuntimeMCPConfigRequirementV2(value); err != nil {
		return ProtectedRuntimeMCPConfigRequirementV2{}, err
	}
	return value, nil
}

// DecodeProtectedRuntimeMCPConfigMaterializationV2 strictly decodes and
// validates structural v2 materialization evidence. Runtime authorization
// additionally requires the exact common-materialization join.
func DecodeProtectedRuntimeMCPConfigMaterializationV2(raw []byte) (ProtectedRuntimeMCPConfigMaterializationV2, error) {
	if err := validateProtectedRuntimeMCPWireKeys(raw, "protected runtime MCP config materialization v2", protectedRuntimeMCPMaterializationV2Fields, protectedRuntimeMCPAuthorizationSourceMaterializationV2Fields); err != nil {
		return ProtectedRuntimeMCPConfigMaterializationV2{}, err
	}
	var value ProtectedRuntimeMCPConfigMaterializationV2
	if err := decodeClosed(raw, &value, "protected runtime MCP config materialization v2"); err != nil {
		return ProtectedRuntimeMCPConfigMaterializationV2{}, err
	}
	if err := ValidateProtectedRuntimeMCPConfigMaterializationV2(value); err != nil {
		return ProtectedRuntimeMCPConfigMaterializationV2{}, err
	}
	return value, nil
}
