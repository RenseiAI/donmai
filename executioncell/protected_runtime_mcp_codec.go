package executioncell

// DecodeProtectedRuntimeMCPConfigRequirementV1 strictly decodes and validates
// one v1 protected runtime MCP requirement. V2-only fields are rejected.
func DecodeProtectedRuntimeMCPConfigRequirementV1(raw []byte) (ProtectedRuntimeMCPConfigRequirementV1, error) {
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
	var value ProtectedRuntimeMCPConfigMaterializationV2
	if err := decodeClosed(raw, &value, "protected runtime MCP config materialization v2"); err != nil {
		return ProtectedRuntimeMCPConfigMaterializationV2{}, err
	}
	if err := ValidateProtectedRuntimeMCPConfigMaterializationV2(value); err != nil {
		return ProtectedRuntimeMCPConfigMaterializationV2{}, err
	}
	return value, nil
}
