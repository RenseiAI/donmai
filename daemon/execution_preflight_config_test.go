package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/executioncell"
)

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
