package afcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

const localFactorySyntheticAuth = `{"auth_mode":"chatgpt","tokens":{"id_token":"e30.e30.signature","access_token":"synthetic-access","refresh_token":"synthetic-refresh"}}`

func localFactoryBinary(t *testing.T, name, script string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(path, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return directory
}

func localFactorySettings(harness, model, author string) *daemon.LocalRuntimeConfig {
	return &daemon.LocalRuntimeConfig{Harness: harness, Model: model, ModelAuthor: author, ExecutionSecurity: daemon.InitialLocalExecutionSecurity(), Repositories: []daemon.LocalGitHubRepository{{RepositoryID: 42, OwnerRepo: "example/project", Label: "donmai", Ref: "main"}}}
}

func localFactoryRequest() daemon.LocalCompileRequest {
	return daemon.LocalCompileRequest{SessionID: "local-session", ReceiptID: "local-receipt", ChallengeID: "local-challenge", Source: daemon.LocalIntakeRequest{RepositoryID: 42, OwnerRepo: "example/project", IssueNumber: 1, IssueURL: "https://github.com/example/project/issues/1", Title: "Authored issue", Body: "Implement the authored change."}}
}

func localFactoryCalls(t *testing.T, directory string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(directory, "calls"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestLocalCompilerCodexRequiresCurrentNativeHostLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	directory := localFactoryBinary(t, "codex", `
dir="${0%/*}"
if [ "$1" = '-c' ] && [ "$2" = 'cli_auth_credentials_store="file"' ] && [ "$3" = 'login' ] && [ "$4" = 'status' ]; then
 printf 'status\n' >> "$dir/calls"
 printf 'Logged in using ChatGPT\n' >&2
 exit 0
fi
printf 'spawn\n' >> "$dir/calls"
exit 91
`)
	compiler, err := newLocalCompiler(daemon.LocalRuntimeIdentity{ScopeID: "local-scope", HostID: "local-host", WorkerID: "local-worker"}, localFactorySettings("codex", "gpt-6-sol", "openai"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = compiler.Close() })
	auth := filepath.Join(home, "auth.json")
	for _, payload := range []string{"", "{}", `{"auth_mode":"api_key","OPENAI_API_KEY":"synthetic"}`} {
		if payload != "" {
			if err = os.WriteFile(auth, []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = compiler.Compile(context.Background(), localFactoryRequest()); err == nil {
			t.Fatal("production compiler admitted missing/malformed/non-host-login authority")
		}
		if payload != "" {
			if err = os.Remove(auth); err != nil {
				t.Fatal(err)
			}
		}
	}
	if localFactoryCalls(t, directory) != "" {
		t.Fatal("invalid login started a native probe or harness")
	}
	if err = os.WriteFile(auth, []byte(localFactorySyntheticAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	admitted, err := compiler.Compile(context.Background(), localFactoryRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertLocalBaseBranchEvidence(t, admitted)
	if localFactoryCalls(t, directory) != "status\n" {
		t.Fatal("production compiler did not perform exactly the native status observation")
	}
	if err = os.Remove(auth); err != nil {
		t.Fatal(err)
	}
	if err = compiler.Revalidate(context.Background(), admitted); err == nil {
		t.Fatal("revalidation accepted a deleted host credential")
	}
	if strings.Contains(localFactoryCalls(t, directory), "spawn") {
		t.Fatal("admission or revalidation spawned a harness")
	}
}

func TestLocalCompilerClaudeChecksModelVersionBeforeLogin(t *testing.T) {
	directory := localFactoryBinary(t, "claude", `
dir="${0%/*}"
if [ "$1" = '--version' ]; then
 printf 'version\n' >> "$dir/calls"
 cat "$dir/version"
 exit 0
fi
if [ "$1" = 'auth' ] && [ "$2" = 'status' ] && [ "$3" = '--json' ]; then
 printf 'login\n' >> "$dir/calls"
 printf '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}\n'
 exit 0
fi
printf 'spawn\n' >> "$dir/calls"
exit 91
`)
	version := filepath.Join(directory, "version")
	if err := os.WriteFile(version, []byte("2.1.196 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := localFactorySettings("claude-code", "claude-sonnet-5", "anthropic")
	compiler, err := newLocalCompiler(daemon.LocalRuntimeIdentity{ScopeID: "local-scope", HostID: "local-host", WorkerID: "local-worker"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = compiler.Close() })
	// Caller mutation must not change the already selected model's readiness gate.
	settings.Model = "claude-sonnet-4"
	if _, err = compiler.Compile(context.Background(), localFactoryRequest()); err == nil {
		t.Fatal("production compiler admitted an unsupported installed Claude version")
	}
	if localFactoryCalls(t, directory) != "version\n" {
		t.Fatal("unsupported version reached host-login observation or harness spawn")
	}
	if err = os.WriteFile(version, []byte("2.1.197 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admitted, err := compiler.Compile(context.Background(), localFactoryRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertLocalBaseBranchEvidence(t, admitted)
	if localFactoryCalls(t, directory) != "version\nversion\nlogin\n" {
		t.Fatal("supported version did not precede login observation")
	}
	if err = os.WriteFile(version, []byte("unknown\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = compiler.Revalidate(context.Background(), admitted); err == nil {
		t.Fatal("revalidation bypassed current version evidence")
	}
	if localFactoryCalls(t, directory) != "version\nversion\nlogin\nversion\n" {
		t.Fatal("failed version revalidation reached login or harness spawn")
	}
}

func assertLocalBaseBranchEvidence(t *testing.T, admitted daemon.LocalAdmissionEvidence) {
	t.Helper()
	var work struct{ BaseRef, Ref string }
	if err := json.Unmarshal(admitted.OperationalPayload, &work); err != nil {
		t.Fatal(err)
	}
	if work.BaseRef != "main" || work.Ref != "" {
		t.Fatal("local producer lost new-branch source semantics")
	}
	var evidence struct {
		Configuration struct{ RuntimeTransportMode string }
	}
	if err := json.Unmarshal(admitted.ProducerEvidence, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Configuration.RuntimeTransportMode != "local/v2" {
		t.Fatal("producer did not bind new branch semantics to versioned local transport")
	}
}
