package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/spf13/cobra"
)

func TestLocalWorkerUnsupportedContractRefusesBeforeDetail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, contract string
		local          bool
	}{{"unknown", "local/v3", true}, {"wrong mode", "local/v2", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusUnauthorized) }))
			t.Cleanup(server.Close)
			err := runAgentRun(context.Background(), &cobra.Command{}, &agentRunOpts{sessionID: "contract-check", daemonURL: server.URL, localRuntime: tc.local, localRuntimeContract: tc.contract})
			if err == nil || !strings.Contains(err.Error(), "unsupported local worker transport contract") || calls.Load() != 0 {
				t.Fatalf("err=%v detail requests=%d", err, calls.Load())
			}
		})
	}
}

// localCodexAdmittedDetail uses the shipped local compiler and daemon detail
// projection. It never supplies an auth hint that the producer did not stamp.
func localCodexAdmittedDetail(t *testing.T) (*daemon.SessionDetail, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	localFactoryBinary(t, "codex", `
if [ "$1" = '-c' ] && [ "$2" = 'cli_auth_credentials_store="file"' ] && [ "$3" = login ] && [ "$4" = status ]; then
 printf 'Logged in using ChatGPT\n' >&2
 exit 0
fi
exit 91
`)
	auth := filepath.Join(home, "auth.json")
	if err := os.WriteFile(auth, []byte(localFactorySyntheticAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := daemon.LocalRuntimeIdentity{ScopeID: "local-scope", HostID: "local-host", WorkerID: "local-worker"}
	compiler, err := newLocalCompiler(identity, localFactorySettings("codex", "gpt-6-sol", "openai"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = compiler.Close() })
	admitted, err := compiler.Compile(t.Context(), localFactoryRequest())
	if err != nil {
		t.Fatal(err)
	}
	var item daemon.PollWorkItem
	if err = json.Unmarshal(admitted.OperationalPayload, &item); err != nil {
		t.Fatal(err)
	}
	item.AdmissionReceipt = admitted.Receipt
	item.EffectiveCell = admitted.EffectiveCell
	item.ExecutionRuntimeBinding = admitted.RuntimeBinding
	item.OperationalPayload = admitted.OperationalPayload
	detail := daemon.PollItemToSessionDetail(item, nil, "http://127.0.0.1:7734/api/daemon/local", "", identity.WorkerID)
	detail.HostAdaptationReceipt = admitted.HostPreflight
	if detail.ResolvedProfile == nil || detail.ResolvedProfile.AuthMode != "" {
		t.Fatal("fixture manufactured a controller auth-mode hint")
	}
	return detail, auth
}

func cloneLocalWorkerDetail(t *testing.T, original *daemon.SessionDetail) *daemon.SessionDetail {
	t.Helper()
	copied := *original
	copied.AdmissionReceipt = bytes.Clone(original.AdmissionReceipt)
	copied.EffectiveCell = bytes.Clone(original.EffectiveCell)
	copied.ExecutionRuntimeBinding = bytes.Clone(original.ExecutionRuntimeBinding)
	copied.HostAdaptationReceipt = bytes.Clone(original.HostAdaptationReceipt)
	if original.ResolvedProfile != nil {
		profile := *original.ResolvedProfile
		if profile.Endpoint != nil {
			endpoint := *profile.Endpoint
			profile.Endpoint = &endpoint
		}
		copied.ResolvedProfile = &profile
	}
	return &copied
}

func TestLocalCodexHintRequiresExactAdmittedHostBinding(t *testing.T) {
	detail, _ := localCodexAdmittedDetail(t)
	enabled, err := localCodexHostSessionHint(detail)
	if err != nil || !enabled {
		t.Fatalf("exact local Codex host binding not selected: enabled=%v err=%v", enabled, err)
	}
	if agentRunHints(detail).CodexHostSessionAuth {
		t.Fatal("controller hint was silently enabled by the local binding")
	}
	for _, change := range []struct {
		name   string
		mutate func(*daemon.SessionDetail)
	}{
		{"missing-receipt", func(d *daemon.SessionDetail) { d.AdmissionReceipt = nil }},
		{"missing-host", func(d *daemon.SessionDetail) { d.HostAdaptationReceipt = nil }},
		{"missing-binding", func(d *daemon.SessionDetail) { d.ExecutionRuntimeBinding = nil }},
		{"wrong-worker", func(d *daemon.SessionDetail) { d.WorkerID = "foreign-worker" }},
		{"wrong-session", func(d *daemon.SessionDetail) { d.SessionID = "foreign-session" }},
		{"unselected-harness", func(d *daemon.SessionDetail) { d.ResolvedProfile.Harness = "claude-code" }},
		{"foreign-model", func(d *daemon.SessionDetail) { d.ResolvedProfile.Model = "other-model" }},
		{"foreign-auth-mode", func(d *daemon.SessionDetail) { d.ResolvedProfile.AuthMode = "byok" }},
		{"changed-endpoint", func(d *daemon.SessionDetail) { d.ResolvedProfile.Endpoint.AuthBindingID = "other-binding" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := cloneLocalWorkerDetail(t, detail)
			change.mutate(candidate)
			if enabled, err := localCodexHostSessionHint(candidate); err == nil || enabled {
				t.Fatalf("changed detail enabled host login: enabled=%v err=%v", enabled, err)
			}
		})
	}
}

func TestRunAgentRunLocalV2ConstructsOnlyAdmittedHostAuthProvider(t *testing.T) {
	detail, _ := localCodexAdmittedDetail(t)
	t.Setenv("DONMAI_RUNTIME_JWT", "synthetic-attempt-only")
	// A local spawn also states the session read credential; the local
	// receiver below authenticates the attempt credential only.
	t.Setenv("DONMAI_SESSION_READ_TOKEN", "synthetic-read-credential")
	// If a mutant drops the local host-auth selector, stop before any worktree
	// clone or provider Spawn. The correct path refuses at construction first.
	stopBeforeWorktree := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(stopBeforeWorktree, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/daemon/sessions/"+detail.SessionID || r.Header.Get("Authorization") != "Bearer synthetic-attempt-only" {
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The fixture emits only the exact nonsecret fields the local worker
		// consumes. A SessionDetail also has credential fields by design.
		wire := map[string]any{
			"sessionId": detail.SessionID, "organizationId": detail.OrganizationID,
			"workerId": detail.WorkerID, "harness": detail.Harness,
			"issueIdentifier": detail.IssueIdentifier, "projectName": detail.ProjectName,
			"repository": detail.Repository, "ref": detail.Ref, "baseRef": detail.BaseRef,
			"branch": detail.Branch, "workType": detail.WorkType, "mode": detail.Mode,
			"title": detail.Title, "body": detail.Body,
			"platformUrl": detail.PlatformURL, "resolvedProfile": detail.ResolvedProfile,
			"operationalPayload":      json.RawMessage(detail.OperationalPayload),
			"admissionReceipt":        json.RawMessage(detail.AdmissionReceipt),
			"effectiveCell":           json.RawMessage(detail.EffectiveCell),
			"executionRuntimeBinding": json.RawMessage(detail.ExecutionRuntimeBinding),
			"hostAdaptationReceipt":   json.RawMessage(detail.HostAdaptationReceipt),
		}
		if err := json.NewEncoder(w).Encode(wire); err != nil {
			t.Errorf("encode fixture detail: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	detail.PlatformURL = server.URL + "/api/daemon/local"
	// The controller's valid file-backed login is no longer reachable from
	// this worker. The real Codex constructor checks its host-home reference
	// only when runAgentRun carried the admitted local/v2 host-auth binding.
	// Without that caller wiring it would register a credential-less provider.
	t.Setenv("CODEX_HOME", "relative-untrusted-home")
	err := runAgentRun(t.Context(), &cobra.Command{}, &agentRunOpts{
		localRuntime: true, localRuntimeContract: "local/v2", sessionID: detail.SessionID, daemonURL: server.URL,
		worktree: stopBeforeWorktree,
	})
	if err == nil || !strings.Contains(err.Error(), "no available provider") {
		t.Fatalf("invalid host login home reached an unauthenticated worker provider: %v", err)
	}
}
