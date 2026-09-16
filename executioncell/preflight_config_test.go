package executioncell

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

func validConfigRequirement() PreflightConfigRequirementV1 {
	return PreflightConfigRequirementV1{
		ContractVersion:          PreflightConfigRequirementContractVersion,
		RequirementID:            "example.session-config/v1",
		AuthorityBindingDigest:   strings.Repeat("a", 64),
		OperationalPayloadDigest: strings.Repeat("b", 64),
		Bindings: []PreflightConfigBindingV1{
			{TargetEnv: "DONMAI_API_URL", Source: PreflightConfigBindingSourceV1{Kind: PreflightConfigSourceOperationalEnvironment, EnvironmentName: "DONMAI_API_URL"}},
			{TargetEnv: PreflightConfigSessionIDTarget, Source: PreflightConfigBindingSourceV1{Kind: PreflightConfigSourceDaemonSessionID}},
			{TargetEnv: PreflightConfigSessionMCPBearerFileTarget, Source: PreflightConfigBindingSourceV1{Kind: PreflightConfigSourceSessionMCPBearerFile, Mode: PreflightConfigPrivateFileMode}},
		},
	}
}

func TestHostAdaptationV1StaysClosedAndV2RequiresConfig(t *testing.T) {
	t.Parallel()
	binding := runtimeBindingV2()
	v1 := readyHostReceipt(t, binding)
	if _, err := DecodeHostAdaptationReceipt(v1); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(v1, &document); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{
		"null":     nil,
		"empty":    []any{},
		"nonempty": []any{map[string]any{}},
	} {
		t.Run("v1 ready "+name, func(t *testing.T) {
			candidate := maps.Clone(document)
			candidate["configMaterializations"] = value
			raw, _ := json.Marshal(candidate)
			if _, err := DecodeHostAdaptationReceipt(raw); err == nil {
				t.Fatal("host-adaptation/v1 accepted present config materializations")
			}
		})
	}
	for name, value := range map[string]any{
		"missing": nil,
		"null":    nil,
		"empty":   []any{},
	} {
		t.Run("v2 ready "+name, func(t *testing.T) {
			candidate := maps.Clone(document)
			candidate["contractVersion"] = HostAdaptationV2ContractVersion
			if name == "missing" {
				delete(candidate, "configMaterializations")
			} else {
				candidate["configMaterializations"] = value
			}
			raw, _ := json.Marshal(candidate)
			if _, err := DecodeHostAdaptationReceipt(raw); err == nil {
				t.Fatal("host-adaptation/v2 accepted missing or empty config materializations")
			}
		})
	}

	materialization := validConfigMaterialization(strings.Repeat("a", 64))
	var validV2 HostAdaptationReceipt
	if err := json.Unmarshal(v1, &validV2); err != nil {
		t.Fatal(err)
	}
	validV2.ContractVersion = HostAdaptationV2ContractVersion
	validV2.ConfigMaterializations = []PreflightConfigMaterializationV1{materialization}
	raw, _ := json.Marshal(validV2)
	if _, err := DecodeHostAdaptationReceipt(raw); err != nil {
		t.Fatalf("valid ready host-adaptation/v2 rejected: %v", err)
	}
}

func TestHostAdaptationDeniedConfigVersionClosure(t *testing.T) {
	t.Parallel()
	denied := map[string]any{
		"contractVersion": HostAdaptationContractVersion,
		"requestId":       "request-1",
		"workerId":        "worker-1",
		"placementId":     "placement-1",
		"decision":        "denied",
		"denial":          "unsupported",
	}
	raw, _ := json.Marshal(denied)
	if _, err := DecodeHostAdaptationReceipt(raw); err != nil {
		t.Fatalf("legacy denied host-adaptation/v1 rejected: %v", err)
	}
	for name, value := range map[string]any{
		"null":     nil,
		"empty":    []any{},
		"nonempty": []any{validConfigMaterialization(strings.Repeat("a", 64))},
	} {
		t.Run("v1 present "+name, func(t *testing.T) {
			candidate := maps.Clone(denied)
			candidate["configMaterializations"] = value
			raw, _ := json.Marshal(candidate)
			if _, err := DecodeHostAdaptationReceipt(raw); err == nil {
				t.Fatal("denied host-adaptation/v1 accepted config member presence")
			}
		})
	}
	for name, value := range map[string]any{
		"missing":  nil,
		"null":     nil,
		"empty":    []any{},
		"nonempty": []any{validConfigMaterialization(strings.Repeat("a", 64))},
	} {
		t.Run("v2 "+name, func(t *testing.T) {
			candidate := maps.Clone(denied)
			candidate["contractVersion"] = HostAdaptationV2ContractVersion
			if name != "missing" {
				candidate["configMaterializations"] = value
			}
			raw, _ := json.Marshal(candidate)
			if _, err := DecodeHostAdaptationReceipt(raw); err == nil {
				t.Fatal("denied host-adaptation/v2 accepted")
			}
		})
	}
}

func TestHostAdaptationV3RequiresProtectedRuntimeMCPAndOlderVersionsRejectIt(t *testing.T) {
	t.Parallel()
	binding := runtimeBindingV2()
	var base HostAdaptationReceipt
	if err := json.Unmarshal(readyHostReceipt(t, binding), &base); err != nil {
		t.Fatal(err)
	}
	protected := validProtectedRuntimeMCPMaterialization(strings.Repeat("a", 64))
	var baseDocument map[string]any
	if err := json.Unmarshal(readyHostReceipt(t, binding), &baseDocument); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{HostAdaptationContractVersion, HostAdaptationV2ContractVersion} {
		for name, value := range map[string]any{"null": nil, "empty": []any{}, "nonempty": []any{protected}} {
			t.Run(version+" "+name, func(t *testing.T) {
				candidate := maps.Clone(baseDocument)
				candidate["contractVersion"] = version
				candidate["protectedRuntimeMcpConfigs"] = value
				if version == HostAdaptationV2ContractVersion {
					candidate["configMaterializations"] = []PreflightConfigMaterializationV1{validConfigMaterialization(strings.Repeat("a", 64))}
				}
				if _, err := DecodeHostAdaptationReceipt(mustJSON(t, candidate)); err == nil {
					t.Fatalf("%s accepted present protected runtime MCP configs", version)
				}
			})
		}
	}

	base.ContractVersion = HostAdaptationV3ContractVersion
	for name, value := range map[string]any{
		"missing": nil,
		"null":    nil,
		"empty":   []any{},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := maps.Clone(baseDocument)
			candidate["contractVersion"] = HostAdaptationV3ContractVersion
			if name != "missing" {
				candidate["protectedRuntimeMcpConfigs"] = value
			}
			if _, err := DecodeHostAdaptationReceipt(mustJSON(t, candidate)); err == nil {
				t.Fatal("host-adaptation/v3 accepted missing protected runtime MCP materialization")
			}
		})
	}
	base.ProtectedRuntimeMCPConfigs = []ProtectedRuntimeMCPConfigMaterializationV1{protected}
	if _, err := DecodeHostAdaptationReceipt(mustJSON(t, base)); err != nil {
		t.Fatalf("valid host-adaptation/v3 rejected: %v", err)
	}
}

func TestPreflightConfigRequirementClosesSourcesAndOrdering(t *testing.T) {
	t.Parallel()
	valid := validConfigRequirement()
	if err := ValidatePreflightConfigRequirement(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PreflightConfigRequirementV1){
		"remapped environment":  func(v *PreflightConfigRequirementV1) { v.Bindings[0].Source.EnvironmentName = "OTHER" },
		"arbitrary file target": func(v *PreflightConfigRequirementV1) { v.Bindings[2].TargetEnv = "OTHER_FILE" },
		"unsorted":              func(v *PreflightConfigRequirementV1) { v.Bindings[0], v.Bindings[1] = v.Bindings[1], v.Bindings[0] },
		"bad digest":            func(v *PreflightConfigRequirementV1) { v.AuthorityBindingDigest = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			value := validConfigRequirement()
			mutate(&value)
			if err := ValidatePreflightConfigRequirement(value); err == nil {
				t.Fatal("invalid requirement accepted")
			}
		})
	}
}

func TestPreflightConfigMaterializationDigestBindsEveryField(t *testing.T) {
	t.Parallel()
	value := validConfigMaterialization(strings.Repeat("b", 64))
	if err := ValidatePreflightConfigMaterialization(value); err != nil {
		t.Fatal(err)
	}
	value.Bindings[1].FileReferenceDigest = strings.Repeat("f", 64)
	if err := ValidatePreflightConfigMaterialization(value); err == nil {
		t.Fatal("divergent file reference accepted with stale combined digest")
	}
}

func validConfigMaterialization(operationalDigest string) PreflightConfigMaterializationV1 {
	value := PreflightConfigMaterializationV1{
		ContractVersion:          PreflightConfigMaterializationContractVersion,
		RequirementID:            "example.session-config/v1",
		AuthorityBindingDigest:   strings.Repeat("a", 64),
		OperationalPayloadDigest: operationalDigest,
		Bindings: []PreflightConfigBindingMaterializationV1{
			{TargetEnv: "DONMAI_API_URL", Source: PreflightConfigBindingSourceV1{Kind: PreflightConfigSourceOperationalEnvironment, EnvironmentName: "DONMAI_API_URL"}, ValueDigest: strings.Repeat("c", 64)},
			{TargetEnv: PreflightConfigSessionMCPBearerFileTarget, Source: PreflightConfigBindingSourceV1{Kind: PreflightConfigSourceSessionMCPBearerFile, Mode: PreflightConfigPrivateFileMode}, BearerContentDigest: strings.Repeat("d", 64), FileReferenceDigest: strings.Repeat("e", 64), Mode: PreflightConfigPrivateFileMode},
		},
	}
	digest, err := DigestPreflightConfigReference(value)
	if err != nil {
		panic(err)
	}
	value.ConfigReferenceDigest = digest
	return value
}

func validProtectedRuntimeMCPRequirement(operationalDigest string) ProtectedRuntimeMCPConfigRequirementV1 {
	return ProtectedRuntimeMCPConfigRequirementV1{
		ContractVersion: ProtectedRuntimeMCPConfigContractVersion,
		RequirementID:   "protected-runtime-mcp/v1", AuthorityBindingDigest: strings.Repeat("a", 64),
		OperationalPayloadDigest: operationalDigest,
		ServerName:               "donmai-platform", Transport: ProtectedRuntimeMCPTransportHTTP,
		EndpointDigest: strings.Repeat("c", 64),
		Headers:        []ProtectedRuntimeMCPHeaderV1{{Name: "Authorization", ValueDigest: strings.Repeat("d", 64)}},
	}
}

func validProtectedRuntimeMCPMaterialization(operationalDigest string) ProtectedRuntimeMCPConfigMaterializationV1 {
	requirement := validProtectedRuntimeMCPRequirement(operationalDigest)
	value := ProtectedRuntimeMCPConfigMaterializationV1{
		ContractVersion: requirement.ContractVersion, RequirementID: requirement.RequirementID,
		AuthorityBindingDigest: requirement.AuthorityBindingDigest, OperationalPayloadDigest: requirement.OperationalPayloadDigest,
		ServerName: requirement.ServerName, Transport: requirement.Transport, EndpointDigest: requirement.EndpointDigest,
		Headers: append([]ProtectedRuntimeMCPHeaderV1(nil), requirement.Headers...),
	}
	digest, err := DigestProtectedRuntimeMCPConfigReference(value)
	if err != nil {
		panic(err)
	}
	value.ConfigReferenceDigest = digest
	return value
}

func TestProtectedRuntimeMCPConfigClosesShapeAndBindsEveryField(t *testing.T) {
	t.Parallel()
	requirement := validProtectedRuntimeMCPRequirement(strings.Repeat("b", 64))
	if err := ValidateProtectedRuntimeMCPConfigRequirement(requirement); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ProtectedRuntimeMCPConfigRequirementV1){
		"transport":        func(v *ProtectedRuntimeMCPConfigRequirementV1) { v.Transport = "stdio" },
		"empty headers":    func(v *ProtectedRuntimeMCPConfigRequirementV1) { v.Headers = nil },
		"duplicate header": func(v *ProtectedRuntimeMCPConfigRequirementV1) { v.Headers = append(v.Headers, v.Headers[0]) },
		"raw endpoint": func(v *ProtectedRuntimeMCPConfigRequirementV1) {
			v.EndpointDigest = "https://example.invalid/api/mcp/session"
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := requirement
			candidate.Headers = append([]ProtectedRuntimeMCPHeaderV1(nil), requirement.Headers...)
			mutate(&candidate)
			if err := ValidateProtectedRuntimeMCPConfigRequirement(candidate); err == nil {
				t.Fatal("invalid protected runtime MCP requirement accepted")
			}
		})
	}
	materialization := validProtectedRuntimeMCPMaterialization(strings.Repeat("b", 64))
	materialization.Headers[0].ValueDigest = strings.Repeat("e", 64)
	if err := ValidateProtectedRuntimeMCPConfigMaterialization(materialization); err == nil {
		t.Fatal("mutated protected runtime MCP materialization accepted with stale reference digest")
	}
}

func TestPreflightRegistrationAcceptsValidProtectedRuntimeMCPV3(t *testing.T) {
	t.Parallel()
	binding := runtimeBindingV2()
	var host HostAdaptationReceipt
	if err := json.Unmarshal(readyHostReceipt(t, binding), &host); err != nil {
		t.Fatal(err)
	}
	host.ContractVersion = HostAdaptationV3ContractVersion
	host.ProtectedRuntimeMCPConfigs = []ProtectedRuntimeMCPConfigMaterializationV1{
		validProtectedRuntimeMCPMaterialization(strings.Repeat("a", 64)),
	}
	receipt := mustJSON(t, host)
	request, err := NewPreflightRegistrationRequest(binding, receipt, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePreflightRegistrationRequest(request); err != nil {
		t.Fatal(err)
	}
}
