package executioncell

import (
	"encoding/json"
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
	document["configMaterializations"] = []any{}
	withEmpty, _ := json.Marshal(document)
	if _, err := DecodeHostAdaptationReceipt(withEmpty); err == nil {
		t.Fatal("host-adaptation/v1 accepted present config materializations")
	}
	document["contractVersion"] = HostAdaptationV2ContractVersion
	missing, _ := json.Marshal(document)
	if _, err := DecodeHostAdaptationReceipt(missing); err == nil {
		t.Fatal("host-adaptation/v2 accepted empty config materializations")
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
	value := PreflightConfigMaterializationV1{
		ContractVersion:          PreflightConfigMaterializationContractVersion,
		RequirementID:            "example.session-config/v1",
		AuthorityBindingDigest:   strings.Repeat("a", 64),
		OperationalPayloadDigest: strings.Repeat("b", 64),
		Bindings: []PreflightConfigBindingMaterializationV1{
			{TargetEnv: "DONMAI_API_URL", Source: PreflightConfigBindingSourceV1{Kind: PreflightConfigSourceOperationalEnvironment, EnvironmentName: "DONMAI_API_URL"}, ValueDigest: strings.Repeat("c", 64)},
			{TargetEnv: PreflightConfigSessionMCPBearerFileTarget, Source: PreflightConfigBindingSourceV1{Kind: PreflightConfigSourceSessionMCPBearerFile, Mode: PreflightConfigPrivateFileMode}, BearerContentDigest: strings.Repeat("d", 64), FileReferenceDigest: strings.Repeat("e", 64), Mode: PreflightConfigPrivateFileMode},
		},
	}
	digest, err := DigestPreflightConfigReference(value)
	if err != nil {
		t.Fatal(err)
	}
	value.ConfigReferenceDigest = digest
	if err := ValidatePreflightConfigMaterialization(value); err != nil {
		t.Fatal(err)
	}
	value.Bindings[1].FileReferenceDigest = strings.Repeat("f", 64)
	if err := ValidatePreflightConfigMaterialization(value); err == nil {
		t.Fatal("divergent file reference accepted with stale combined digest")
	}
}
