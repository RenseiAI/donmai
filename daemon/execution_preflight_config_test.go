package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

const testPlatformMCPServerName = "example-platform"

func TestResolvePlatformMCPServerNameCapturesDefaultAndRejectsMalformedExplicit(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)
	statehome.SetBrand("example-local-a")
	name, err := resolvePlatformMCPServerName("")
	if err != nil || name != "example-local-a-platform" {
		t.Fatalf("default name = %q err=%v", name, err)
	}
	statehome.SetBrand("example-local-b")
	if name != "example-local-a-platform" {
		t.Fatalf("captured default changed to %q", name)
	}
	if explicit, err := resolvePlatformMCPServerName(testPlatformMCPServerName); err != nil || explicit != testPlatformMCPServerName {
		t.Fatalf("explicit name = %q err=%v", explicit, err)
	}
	if _, err := resolvePlatformMCPServerName(" example-platform"); err == nil {
		t.Fatal("malformed explicit name accepted")
	}
}

func TestMaterializeExecutionPreflightConfigAppliesAndReadsBackCommonConfig(t *testing.T) {
	t.Parallel()
	sessionID := "session-config-control"
	token := "session-bearer-control"
	operational := json.RawMessage(`{"env":{"EXAMPLE_OPERATIONS":"create,read","DONMAI_API_URL":"https://example.invalid"},"mcpAuthToken":"` + token + `"}`)
	digest, err := executioncell.DigestOperationalPayload(operational)
	if err != nil {
		t.Fatal(err)
	}
	requirement := executioncell.PreflightConfigRequirementV1{
		ContractVersion:          executioncell.PreflightConfigRequirementContractVersion,
		RequirementID:            "example.common-config/v1",
		AuthorityBindingDigest:   strings.Repeat("a", 64),
		OperationalPayloadDigest: digest,
		Bindings: []executioncell.PreflightConfigBindingV1{
			{TargetEnv: "DONMAI_API_URL", Source: executioncell.PreflightConfigBindingSourceV1{Kind: executioncell.PreflightConfigSourceOperationalEnvironment, EnvironmentName: "DONMAI_API_URL"}},
			{TargetEnv: "DONMAI_SESSION_ID", Source: executioncell.PreflightConfigBindingSourceV1{Kind: executioncell.PreflightConfigSourceDaemonSessionID}},
			{TargetEnv: "EXAMPLE_OPERATIONS", Source: executioncell.PreflightConfigBindingSourceV1{Kind: executioncell.PreflightConfigSourceOperationalEnvironment, EnvironmentName: "EXAMPLE_OPERATIONS"}},
			{TargetEnv: "MCP_GATEWAY_TOKEN_FILE", Source: executioncell.PreflightConfigBindingSourceV1{Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, Mode: "0600"}},
		},
	}
	spec := SessionSpec{SessionID: sessionID}
	detail := &SessionDetail{SessionID: sessionID, OperationalPayload: operational, McpAuthToken: token}
	configDir := t.TempDir()
	materialized, lease, err := materializeExecutionPreflightConfig(&spec, detail, []executioncell.PreflightConfigRequirementV1{requirement}, configDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.cleanup)
	if spec.Env["DONMAI_API_URL"] != "https://example.invalid" || spec.Env["DONMAI_SESSION_ID"] != sessionID || spec.Env["EXAMPLE_OPERATIONS"] != "create,read" {
		t.Fatalf("materialized env = %#v", spec.Env)
	}
	path := spec.Env["MCP_GATEWAY_TOKEN_FILE"]
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != token {
		t.Fatalf("bearer file readback=%q err=%v", raw, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("bearer file mode=%v err=%v", info.Mode(), err)
	}
	encoded, err := json.Marshal(materialized)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), path) {
		t.Fatalf("materialization leaked token or path: %s", encoded)
	}
	if len(materialized) != 1 || executioncell.ValidatePreflightConfigMaterialization(materialized[0]) != nil {
		t.Fatalf("invalid materialization: %#v", materialized)
	}

	secondSpec := SessionSpec{SessionID: sessionID}
	replayed, secondLease, err := materializeExecutionPreflightConfig(&secondSpec, detail, []executioncell.PreflightConfigRequirementV1{requirement}, configDir)
	secondLease.cleanup()
	if err != nil || len(replayed) != 1 {
		t.Fatalf("independent materialization failed: %#v err=%v", replayed, err)
	}
}

func TestMaterializeExecutionPreflightConfigPreservesExistingFileOwnership(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/refreshed-token"
	if err := os.WriteFile(path, []byte("bearer"), 0o600); err != nil {
		t.Fatal(err)
	}
	operational := json.RawMessage(`{"mcpAuthToken":"bearer"}`)
	digest, _ := executioncell.DigestOperationalPayload(operational)
	requirement := executioncell.PreflightConfigRequirementV1{ContractVersion: executioncell.PreflightConfigRequirementContractVersion, RequirementID: "example.file/v1", AuthorityBindingDigest: strings.Repeat("a", 64), OperationalPayloadDigest: digest, Bindings: []executioncell.PreflightConfigBindingV1{{TargetEnv: "MCP_GATEWAY_TOKEN_FILE", Source: executioncell.PreflightConfigBindingSourceV1{Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, Mode: "0600"}}}}
	spec := SessionSpec{SessionID: "session-existing", Env: map[string]string{"MCP_GATEWAY_TOKEN_FILE": path}}
	detail := &SessionDetail{SessionID: spec.SessionID, OperationalPayload: operational, McpAuthToken: "bearer"}
	_, lease, err := materializeExecutionPreflightConfig(&spec, detail, []executioncell.PreflightConfigRequirementV1{requirement}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lease.cleanup()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("existing refreshed file was removed: %v", err)
	}
}

func TestMaterializeProtectedRuntimeMCPConfigBindsExactRuntimeWithoutSecrets(t *testing.T) {
	t.Parallel()
	detail := &SessionDetail{
		SessionID: "session-protected-mcp", PlatformURL: "https://platform.example/", McpAuthToken: "session-bearer",
		OperationalPayload: json.RawMessage(`{"issueIdentifier":"TEST-1"}`),
	}
	operationalDigest, err := executioncell.DigestOperationalPayload(detail.OperationalPayload)
	if err != nil {
		t.Fatal(err)
	}
	requirement := executioncell.ProtectedRuntimeMCPConfigRequirementV1{
		ContractVersion: executioncell.ProtectedRuntimeMCPConfigContractVersion,
		RequirementID:   "protected-runtime-mcp/v1", AuthorityBindingDigest: strings.Repeat("a", 64),
		OperationalPayloadDigest: operationalDigest,
		ServerName:               testPlatformMCPServerName, Transport: executioncell.ProtectedRuntimeMCPTransportHTTP,
		EndpointDigest: digestConfigValue("https://platform.example/api/mcp/" + detail.SessionID),
		Headers: []executioncell.ProtectedRuntimeMCPHeaderV1{{
			Name: "Authorization", ValueDigest: digestConfigValue("Bearer " + detail.McpAuthToken),
		}},
	}
	materialized, err := materializeProtectedRuntimeMCPConfigs(detail, []executioncell.ProtectedRuntimeMCPConfigRequirementV1{requirement}, testPlatformMCPServerName)
	if err != nil {
		t.Fatal(err)
	}
	if len(materialized) != 1 || executioncell.ValidateProtectedRuntimeMCPConfigMaterialization(materialized[0]) != nil {
		t.Fatalf("materialization = %+v", materialized)
	}
	if _, err := materializeProtectedRuntimeMCPConfigs(detail, []executioncell.ProtectedRuntimeMCPConfigRequirementV1{requirement}, "example-local-b-platform"); err == nil || !strings.Contains(err.Error(), "current runtime authority") {
		t.Fatalf("different V1 process authority error = %v", err)
	}
	raw, err := json.Marshal(materialized)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), detail.McpAuthToken) || strings.Contains(string(raw), "https://platform.example/api/mcp/") {
		t.Fatalf("protected materialization leaked runtime values: %s", raw)
	}

	binding := executioncell.RuntimeBinding{RequestID: detail.SessionID, WorkerID: "worker", PlacementID: "host"}
	source := readyPreflightReceipt(t, binding, operationalDigest)
	v3, err := hostReceiptWithProtectedRuntimeMCPConfigs(source, materialized)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := executioncell.DecodeHostAdaptationReceipt(v3)
	if err != nil || decoded.ContractVersion != executioncell.HostAdaptationV3ContractVersion || len(decoded.ProtectedRuntimeMCPConfigs) != 1 {
		t.Fatalf("v3 receipt = %+v err=%v", decoded, err)
	}
	replayed, err := materializeProtectedRuntimeMCPConfigs(detail, []executioncell.ProtectedRuntimeMCPConfigRequirementV1{requirement}, testPlatformMCPServerName)
	if err != nil || !reflect.DeepEqual(replayed, materialized) {
		t.Fatalf("unchanged replay = %+v err=%v", replayed, err)
	}

	for name, mutate := range map[string]func(*SessionDetail){
		"rotated bearer":   func(v *SessionDetail) { v.McpAuthToken = "rotated" },
		"changed endpoint": func(v *SessionDetail) { v.PlatformURL = "https://other.example" },
		"changed session":  func(v *SessionDetail) { v.SessionID = "other-session" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := *detail
			mutate(&candidate)
			if _, err := materializeProtectedRuntimeMCPConfigs(&candidate, []executioncell.ProtectedRuntimeMCPConfigRequirementV1{requirement}, testPlatformMCPServerName); err == nil {
				t.Fatal("changed runtime authority accepted")
			}
		})
	}
}

func protectedRuntimeMCPV2MaterializationFixture(t *testing.T) (
	*SessionDetail,
	SessionSpec,
	executioncell.ProtectedRuntimeMCPConfigRequirementV2,
	[]executioncell.PreflightConfigMaterializationV1,
	preflightConfigLease,
) {
	t.Helper()
	token := "session-bearer-v2"
	operational := json.RawMessage(`{"mcpAuthToken":"` + token + `"}`)
	digest, err := executioncell.DigestOperationalPayload(operational)
	if err != nil {
		t.Fatal(err)
	}
	commonRequirement := executioncell.PreflightConfigRequirementV1{
		ContractVersion: executioncell.PreflightConfigRequirementContractVersion,
		RequirementID:   "example.session-config/v1", AuthorityBindingDigest: strings.Repeat("a", 64),
		OperationalPayloadDigest: digest,
		Bindings: []executioncell.PreflightConfigBindingV1{{
			TargetEnv: executioncell.PreflightConfigSessionMCPBearerFileTarget,
			Source: executioncell.PreflightConfigBindingSourceV1{
				Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, Mode: executioncell.PreflightConfigPrivateFileMode,
			},
		}},
	}
	detail := &SessionDetail{
		SessionID: "session-protected-mcp-v2", PlatformURL: "https://platform.example/",
		McpAuthToken: token, OperationalPayload: operational,
	}
	path := filepath.Join(t.TempDir(), "session-token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := SessionSpec{SessionID: detail.SessionID, Env: map[string]string{
		executioncell.PreflightConfigSessionMCPBearerFileTarget: path,
	}}
	common, lease, err := materializeExecutionPreflightConfig(&spec, detail, []executioncell.PreflightConfigRequirementV1{commonRequirement}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	requirement := executioncell.ProtectedRuntimeMCPConfigRequirementV2{
		ContractVersion: executioncell.ProtectedRuntimeMCPConfigContractVersionV2,
		RequirementID:   "protected-runtime-mcp/v2", AuthorityBindingDigest: strings.Repeat("b", 64),
		OperationalPayloadDigest: digest,
		ServerName:               testPlatformMCPServerName, Transport: executioncell.ProtectedRuntimeMCPTransportHTTP,
		EndpointDigest: digestConfigValue("https://platform.example/api/mcp/" + detail.SessionID),
		Headers:        []executioncell.ProtectedRuntimeMCPHeaderV1{{Name: "Authorization", ValueDigest: digestConfigValue("Bearer " + token)}},
		AuthorizationSource: executioncell.ProtectedRuntimeMCPAuthorizationSourceV2{
			Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, ConfigRequirementID: commonRequirement.RequirementID,
			TargetEnv: executioncell.PreflightConfigSessionMCPBearerFileTarget, Mode: executioncell.PreflightConfigPrivateFileMode,
		},
	}
	return detail, spec, requirement, common, lease
}

func TestMaterializeProtectedRuntimeMCPV2JoinsActualCommonEvidence(t *testing.T) {
	t.Parallel()
	detail, spec, requirement, common, lease := protectedRuntimeMCPV2MaterializationFixture(t)
	t.Cleanup(lease.cleanup)
	build := func(path string) (string, error) {
		return "/example/donmai mcp gateway-headers --token-file " + path, nil
	}
	materialized, err := materializeProtectedRuntimeMCPConfigsV2(detail, &spec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{requirement}, common, build, testPlatformMCPServerName)
	if err != nil {
		t.Fatal(err)
	}
	if len(materialized) != 1 || executioncell.ValidateProtectedRuntimeMCPConfigMaterializationV2(materialized[0]) != nil {
		t.Fatalf("v2 materialization = %+v", materialized)
	}
	if _, err := materializeProtectedRuntimeMCPConfigsV2(detail, &spec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{requirement}, common, build, "example-local-b-platform"); err == nil || !strings.Contains(err.Error(), "current runtime authority") {
		t.Fatalf("different V2 process authority error = %v", err)
	}
	got := materialized[0].AuthorizationSource
	wantBinding := common[0].Bindings[0]
	if got.ConfigReferenceDigest != common[0].ConfigReferenceDigest || got.FileReferenceDigest != wantBinding.FileReferenceDigest || got.HelperCommandDigest != digestConfigValue(mustBuildHelperCommand(t, build, spec.Env[executioncell.PreflightConfigSessionMCPBearerFileTarget])) {
		t.Fatalf("joined authorization source = %+v", got)
	}
	raw, err := json.Marshal(materialized)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), detail.McpAuthToken) || strings.Contains(string(raw), spec.Env[executioncell.PreflightConfigSessionMCPBearerFileTarget]) {
		t.Fatalf("v2 materialization leaked bearer or path: %s", raw)
	}
	replayed, err := materializeProtectedRuntimeMCPConfigsV2(detail, &spec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{requirement}, common, build, testPlatformMCPServerName)
	if err != nil || !reflect.DeepEqual(replayed, materialized) {
		t.Fatalf("unchanged v2 replay = %+v err=%v", replayed, err)
	}

	binding := executioncell.RuntimeBinding{RequestID: detail.SessionID, WorkerID: "worker", PlacementID: "host"}
	source := readyPreflightReceipt(t, binding, requirement.OperationalPayloadDigest)
	v3, err := hostReceiptWithProtectedRuntimeMCPConfigsV2(source, materialized)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := executioncell.DecodeHostAdaptationReceipt(v3)
	if err != nil || len(decoded.ProtectedRuntimeMCPConfigs) != 0 || len(decoded.ProtectedRuntimeMCPConfigsV2) != 1 {
		t.Fatalf("v2 host receipt = %+v err=%v", decoded, err)
	}
}

func mustBuildHelperCommand(t *testing.T, build ProtectedRuntimeMCPHelperCommandBuilder, path string) string {
	t.Helper()
	command, err := build(path)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func TestMaterializeProtectedRuntimeMCPV2RefusesIncompleteOrChangedJoin(t *testing.T) {
	t.Parallel()
	detail, spec, requirement, common, lease := protectedRuntimeMCPV2MaterializationFixture(t)
	t.Cleanup(lease.cleanup)
	build := func(path string) (string, error) { return "helper --token-file " + path, nil }
	if _, err := materializeProtectedRuntimeMCPConfigsV2(detail, &spec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{requirement}, common, nil, testPlatformMCPServerName); err == nil {
		t.Fatal("nil trusted helper builder accepted")
	}
	if _, err := materializeProtectedRuntimeMCPConfigsV2(detail, &spec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{requirement}, nil, build, testPlatformMCPServerName); err == nil {
		t.Fatal("missing actual common materialization accepted")
	}
	duplicate := append(append([]executioncell.PreflightConfigMaterializationV1(nil), common...), common...)
	if _, err := materializeProtectedRuntimeMCPConfigsV2(detail, &spec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{requirement}, duplicate, build, testPlatformMCPServerName); err == nil {
		t.Fatal("duplicate common materialization accepted")
	}
	duplicateBindings := append([]executioncell.PreflightConfigMaterializationV1(nil), common...)
	duplicateBindings[0].Bindings = append(append([]executioncell.PreflightConfigBindingMaterializationV1(nil), common[0].Bindings...), common[0].Bindings[0])
	duplicateBindings[0].ConfigReferenceDigest, _ = executioncell.DigestPreflightConfigReference(duplicateBindings[0])
	if _, err := materializeProtectedRuntimeMCPConfigsV2(detail, &spec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{requirement}, duplicateBindings, build, testPlatformMCPServerName); err == nil {
		t.Fatal("duplicate matching common file binding accepted")
	}
	changedSpec := spec
	changedSpec.Env = map[string]string{}
	for key, value := range spec.Env {
		changedSpec.Env[key] = value
	}
	changedSpec.Env[executioncell.PreflightConfigSessionMCPBearerFileTarget] += ".changed"
	if _, err := materializeProtectedRuntimeMCPConfigsV2(detail, &changedSpec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{requirement}, common, build, testPlatformMCPServerName); err == nil {
		t.Fatal("changed actual file reference accepted")
	}
	changedCommon := append([]executioncell.PreflightConfigMaterializationV1(nil), common...)
	changedCommon[0].Bindings = append([]executioncell.PreflightConfigBindingMaterializationV1(nil), common[0].Bindings...)
	changedCommon[0].Bindings[0].BearerContentDigest = strings.Repeat("9", 64)
	changedCommon[0].ConfigReferenceDigest, _ = executioncell.DigestPreflightConfigReference(changedCommon[0])
	if _, err := materializeProtectedRuntimeMCPConfigsV2(detail, &spec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{requirement}, changedCommon, build, testPlatformMCPServerName); err == nil {
		t.Fatal("changed initial bearer evidence accepted")
	}
	changedRequirement := requirement
	changedRequirement.Headers = append([]executioncell.ProtectedRuntimeMCPHeaderV1(nil), requirement.Headers...)
	changedRequirement.Headers[0].ValueDigest = strings.Repeat("8", 64)
	if _, err := materializeProtectedRuntimeMCPConfigsV2(detail, &spec, []executioncell.ProtectedRuntimeMCPConfigRequirementV2{changedRequirement}, common, build, testPlatformMCPServerName); err == nil {
		t.Fatal("changed initial Authorization evidence accepted")
	}
}

func TestPreflightConfigCleanupIsGenerationSafe(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/owned-token"
	if err := os.WriteFile(path, []byte("bearer"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	registry := newPreflightConfigRegistry()
	lease := preflightConfigLease{sessionID: "session-generation", files: []preflightOwnedFile{{path: path, rootDir: filepath.Dir(path), name: filepath.Base(path), info: info, owned: true}}}
	generation, reserved := registry.reserve("session-generation")
	if !reserved || !registry.complete("session-generation", generation, lease) {
		t.Fatal("install failed")
	}
	if registry.cleanupIfOwner("session-generation", generation+1) {
		t.Fatal("stale generation cleaned current file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale cleanup removed file: %v", err)
	}
	if !registry.cleanupIfOwner("session-generation", generation) {
		t.Fatal("owning generation did not clean")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned file survived cleanup: %v", err)
	}
}

func TestPreflightConfigCleanupCannotEscapeOwnedRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "owned")
	if err := os.Mkdir(rootDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside-token")
	if err := os.WriteFile(outside, []byte("bearer"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	lease := preflightConfigLease{sessionID: "session-escape", files: []preflightOwnedFile{{
		path:    outside,
		rootDir: rootDir,
		name:    filepath.Join("..", filepath.Base(outside)),
		info:    info,
		owned:   true,
	}}}
	lease.cleanup()
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "bearer" {
		t.Fatalf("cleanup escaped its owned root: content=%q err=%v", raw, err)
	}
}

func TestMaterializeExecutionPreflightConfigRefusesDivergentArtifact(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(*testing.T) (SessionSpec, *SessionDetail, executioncell.PreflightConfigRequirementV1){
		"bearer content": func(_ *testing.T) (SessionSpec, *SessionDetail, executioncell.PreflightConfigRequirementV1) {
			raw := json.RawMessage(`{"mcpAuthToken":"source-bearer"}`)
			digest, _ := executioncell.DigestOperationalPayload(raw)
			return SessionSpec{SessionID: "session-divergent"}, &SessionDetail{SessionID: "session-divergent", OperationalPayload: raw, McpAuthToken: "different-bearer"}, executioncell.PreflightConfigRequirementV1{ContractVersion: executioncell.PreflightConfigRequirementContractVersion, RequirementID: "example.file/v1", AuthorityBindingDigest: strings.Repeat("a", 64), OperationalPayloadDigest: digest, Bindings: []executioncell.PreflightConfigBindingV1{{TargetEnv: "MCP_GATEWAY_TOKEN_FILE", Source: executioncell.PreflightConfigBindingSourceV1{Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, Mode: "0600"}}}}
		},
		"existing file mode": func(t *testing.T) (SessionSpec, *SessionDetail, executioncell.PreflightConfigRequirementV1) {
			path := t.TempDir() + "/wide"
			if err := os.WriteFile(path, []byte("bearer"), 0o644); err != nil { //nolint:gosec // Intentional overbroad-mode refusal fixture.
				t.Fatal(err)
			}
			raw := json.RawMessage(`{"mcpAuthToken":"bearer"}`)
			digest, _ := executioncell.DigestOperationalPayload(raw)
			return SessionSpec{SessionID: "session-mode", Env: map[string]string{"MCP_GATEWAY_TOKEN_FILE": path}}, &SessionDetail{SessionID: "session-mode", OperationalPayload: raw, McpAuthToken: "bearer"}, executioncell.PreflightConfigRequirementV1{ContractVersion: executioncell.PreflightConfigRequirementContractVersion, RequirementID: "example.file/v1", AuthorityBindingDigest: strings.Repeat("a", 64), OperationalPayloadDigest: digest, Bindings: []executioncell.PreflightConfigBindingV1{{TargetEnv: "MCP_GATEWAY_TOKEN_FILE", Source: executioncell.PreflightConfigBindingSourceV1{Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, Mode: "0600"}}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec, detail, requirement := prepare(t)
			if _, _, err := materializeExecutionPreflightConfig(&spec, detail, []executioncell.PreflightConfigRequirementV1{requirement}, t.TempDir()); err == nil {
				t.Fatal("divergent artifact materialized")
			}
		})
	}
}
