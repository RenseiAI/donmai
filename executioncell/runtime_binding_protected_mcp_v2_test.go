package executioncell

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func validProtectedRuntimeMCPMaterializationV2(operationalDigest, requirementID string) ProtectedRuntimeMCPConfigMaterializationV2 {
	value := ProtectedRuntimeMCPConfigMaterializationV2{
		ContractVersion:          ProtectedRuntimeMCPConfigContractVersionV2,
		RequirementID:            requirementID,
		AuthorityBindingDigest:   strings.Repeat("b", 64),
		OperationalPayloadDigest: operationalDigest,
		ServerName:               "example-gateway",
		Transport:                ProtectedRuntimeMCPTransportHTTP,
		EndpointDigest:           strings.Repeat("c", 64),
		Headers:                  []ProtectedRuntimeMCPHeaderV1{{Name: "Authorization", ValueDigest: strings.Repeat("d", 64)}},
		AuthorizationSource: ProtectedRuntimeMCPAuthorizationSourceMaterializationV2{
			Kind: PreflightConfigSourceSessionMCPBearerFile, ConfigRequirementID: "example.session-config/v1",
			TargetEnv: PreflightConfigSessionMCPBearerFileTarget, Mode: PreflightConfigPrivateFileMode,
			ConfigReferenceDigest: strings.Repeat("e", 64), FileReferenceDigest: strings.Repeat("f", 64),
			HelperCommandDigest: strings.Repeat("0", 64),
		},
	}
	var err error
	value.ConfigReferenceDigest, err = DigestProtectedRuntimeMCPConfigReferenceV2(value)
	if err != nil {
		panic(err)
	}
	return value
}

func readyHostAdaptationForProtectedV2(t *testing.T) HostAdaptationReceipt {
	t.Helper()
	var host HostAdaptationReceipt
	if err := json.Unmarshal(readyHostReceipt(t, runtimeBindingV2()), &host); err != nil {
		t.Fatal(err)
	}
	host.ContractVersion = HostAdaptationV3ContractVersion
	host.ProtectedRuntimeMCPConfigsV2 = []ProtectedRuntimeMCPConfigMaterializationV2{
		validProtectedRuntimeMCPMaterializationV2(strings.Repeat("a", 64), "protected-runtime-mcp/v2"),
	}
	return host
}

func TestHostAdaptationProtectedMCPV1MarshalBytesStayIdentical(t *testing.T) {
	t.Parallel()
	host := HostAdaptationReceipt{
		ContractVersion: HostAdaptationV3ContractVersion, RequestID: "request", WorkerID: "worker", PlacementID: "host",
		Decision: "ready", Plan: json.RawMessage(`{"operationalPayloadDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		PlanDigest: strings.Repeat("b", 64), PromptReceipt: json.RawMessage(`{"decision":"ready"}`),
		ToolLifecycleReceipt:       json.RawMessage(`{"decision":"ready"}`),
		ProtectedRuntimeMCPConfigs: []ProtectedRuntimeMCPConfigMaterializationV1{validProtectedRuntimeMCPMaterialization(strings.Repeat("a", 64))},
	}
	want, err := json.Marshal(hostAdaptationReceiptAlias(host))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("v1 host receipt bytes changed\ngot:  %s\nwant: %s", got, want)
	}
}

func TestHostAdaptationProtectedMCPV2UsesExistingWireKey(t *testing.T) {
	t.Parallel()
	host := readyHostAdaptationForProtectedV2(t)
	raw, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"protectedRuntimeMcpConfigs"`)) || bytes.Contains(raw, []byte(`"protectedRuntimeMcpConfigsV2"`)) {
		t.Fatalf("v2 host receipt uses wrong wire key: %s", raw)
	}
	decoded, err := DecodeHostAdaptationReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.ProtectedRuntimeMCPConfigs) != 0 || len(decoded.ProtectedRuntimeMCPConfigsV2) != 1 {
		t.Fatalf("decoded protected variants = v1:%d v2:%d", len(decoded.ProtectedRuntimeMCPConfigs), len(decoded.ProtectedRuntimeMCPConfigsV2))
	}
	roundTrip, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip, raw) {
		t.Fatalf("v2 host receipt round trip changed bytes\ngot:  %s\nwant: %s", roundTrip, raw)
	}
}

func TestHostAdaptationProtectedMCPVersionsStayClosed(t *testing.T) {
	t.Parallel()
	host := readyHostAdaptationForProtectedV2(t)
	host.ProtectedRuntimeMCPConfigs = []ProtectedRuntimeMCPConfigMaterializationV1{
		validProtectedRuntimeMCPMaterialization(strings.Repeat("a", 64)),
	}
	if _, err := json.Marshal(host); err == nil {
		t.Fatal("mixed protected materialization versions marshaled")
	}

	host = readyHostAdaptationForProtectedV2(t)
	raw, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"case alias": func(value []byte) []byte {
			return bytes.Replace(value, []byte(`"contractVersion":"execution-preflight-protected-mcp-config/v2"`), []byte(`"ContractVersion":"execution-preflight-protected-mcp-config/v2"`), 1)
		},
		"unknown version": func(value []byte) []byte {
			return bytes.Replace(value, []byte(ProtectedRuntimeMCPConfigContractVersionV2), []byte("execution-preflight-protected-mcp-config/v9"), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeHostAdaptationReceipt(mutate(append([]byte(nil), raw...))); err == nil {
				t.Fatal("invalid protected materialization version decoded")
			}
		})
	}
}

func TestProtectedRuntimeMCPConfigMaterializationsV2RequireUniqueOrdering(t *testing.T) {
	t.Parallel()
	a := validProtectedRuntimeMCPMaterializationV2(strings.Repeat("a", 64), "a")
	z := validProtectedRuntimeMCPMaterializationV2(strings.Repeat("a", 64), "z")
	if err := ValidateProtectedRuntimeMCPConfigMaterializationsV2([]ProtectedRuntimeMCPConfigMaterializationV2{a, z}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProtectedRuntimeMCPConfigMaterializationsV2([]ProtectedRuntimeMCPConfigMaterializationV2{z, a}); err == nil {
		t.Fatal("unsorted v2 materializations accepted")
	}
	if err := ValidateProtectedRuntimeMCPConfigMaterializationsV2([]ProtectedRuntimeMCPConfigMaterializationV2{a, a}); err == nil {
		t.Fatal("duplicate v2 materializations accepted")
	}
}
