package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	shellprovider "github.com/RenseiAI/donmai/provider/harness/shell"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// promptCaptureInteractiveProvider runs the real runner-to-adapter contract:
// Runner.Run constructs the Spec from QueuedWork, then Spawn compiles it with
// the exact production manifest. Existing provider-package tests cover the
// final adapted Spec -> native argv mapping; this fixture proves the upstream
// InitialPrompt authority is not synthesized in those tests.
type promptCaptureInteractiveProvider struct {
	name     agent.ProviderName
	caps     agent.Capabilities
	manifest agent.HarnessManifest
	raw      agent.Spec
	adapted  agent.Spec
	session  *recordingInteractiveSession
}

func (p *promptCaptureInteractiveProvider) Name() agent.ProviderName { return p.name }
func (p *promptCaptureInteractiveProvider) Capabilities() agent.Capabilities {
	return p.caps
}

func (p *promptCaptureInteractiveProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.raw = spec
	adapted, err := agent.PreparePrompt(spec, p.manifest)
	if err != nil {
		return nil, err
	}
	p.adapted = adapted
	p.session = completedRecordingInteractiveSession()
	return &testInteractiveHandle{
		Handle:  &fakeHandle{events: make(chan agent.Event)},
		session: p.session,
	}, nil
}

func (*promptCaptureInteractiveProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (*promptCaptureInteractiveProvider) Shutdown(context.Context) error { return nil }

// toolLifecycleInteractiveProvider exercises the production runner callback
// through the same PrepareHarness entry point used by real harness providers.
// It records whether receipt persistence completed before the first simulated
// provider side effect.
type toolLifecycleInteractiveProvider struct {
	manifest                  agent.HarnessManifest
	raw                       agent.Spec
	seedToolReceipt           *agent.ToolLifecycleReceipt
	breakToolReceiptStore     bool
	persistedBeforeSideEffect bool
	sideEffects               int
}

func (*toolLifecycleInteractiveProvider) Name() agent.ProviderName { return agent.ProviderClaude }
func (*toolLifecycleInteractiveProvider) Capabilities() agent.Capabilities {
	return (&claude.Provider{}).Capabilities()
}

func (p *toolLifecycleInteractiveProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.raw = spec
	adapted, err := agent.PreparePrompt(spec, p.manifest)
	if err != nil {
		return nil, err
	}
	if p.seedToolReceipt != nil {
		if _, err := state.NewStore().Update(spec.Cwd, func(st *state.State) error {
			st.AppendToolLifecycleReceipt(*p.seedToolReceipt)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("seed restored tool receipt: %w", err)
		}
	}
	if p.breakToolReceiptStore {
		agentDir := filepath.Join(spec.Cwd, state.AgentDirName)
		if err := os.RemoveAll(agentDir); err != nil {
			return nil, fmt.Errorf("remove agent dir: %w", err)
		}
		if err := os.WriteFile(agentDir, []byte("not-a-directory"), 0o600); err != nil {
			return nil, fmt.Errorf("break receipt store: %w", err)
		}
	}
	if _, err := agent.PrepareToolLifecycle(adapted, p.manifest); err != nil {
		return nil, err
	}
	persisted, err := state.NewStore().Read(spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("verify pre-side-effect tool receipt: %w", err)
	}
	if persisted.ToolLifecycleReceipt == nil || persisted.ToolLifecycleReceipt.Decision != "ready" {
		return nil, fmt.Errorf("verify pre-side-effect tool receipt: got %+v", persisted.ToolLifecycleReceipt)
	}
	p.persistedBeforeSideEffect = true
	p.sideEffects++
	session := completedRecordingInteractiveSession()
	return &testInteractiveHandle{
		Handle:  &fakeHandle{events: make(chan agent.Event)},
		session: session,
	}, nil
}

func (*toolLifecycleInteractiveProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (*toolLifecycleInteractiveProvider) Shutdown(context.Context) error { return nil }

type codexCLIInteractiveProvider struct {
	binary string
	raw    agent.Spec
}

func (*codexCLIInteractiveProvider) Name() agent.ProviderName { return agent.ProviderCodex }
func (*codexCLIInteractiveProvider) Capabilities() agent.Capabilities {
	return (&codex.Provider{}).Capabilities()
}

func (p *codexCLIInteractiveProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.raw = spec
	return codex.SpawnInteractive(ctx, codex.Options{CodexBin: p.binary}, spec)
}

func (*codexCLIInteractiveProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (*codexCLIInteractiveProvider) Shutdown(context.Context) error { return nil }

func TestRun_InteractiveInitialPromptUsesTypedClaudeAndCodexNativeAuthority(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const seed = "  investigate 雪\nwithout trimming  "
	tests := []struct {
		name         string
		providerName agent.ProviderName
		caps         agent.Capabilities
		manifest     agent.HarnessManifest
		wantDelivery agent.PromptDeliveryKind
	}{
		{
			name:         "claude interactive positional prompt",
			providerName: agent.ProviderClaude,
			caps:         (&claude.Provider{}).Capabilities(),
			manifest:     (&claude.Provider{}).Manifest(),
			wantDelivery: agent.PromptDeliveryClaudePTYSeed,
		},
		{
			name:         "codex interactive PTY seed",
			providerName: agent.ProviderCodex,
			caps:         (&codex.Provider{}).Capabilities(),
			manifest:     (&codex.Provider{}).Manifest(),
			wantDelivery: agent.PromptDeliveryCodexPTYSeed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envAttachURL, "")
			t.Setenv(envAttachToken, "")
			server := mockPlatformServer(t)
			t.Cleanup(server.Close)
			manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatalf("worktree.NewManager: %v", err)
			}
			poster, err := result.NewPoster(result.Options{
				PlatformURL: server.URL,
				WorkerID:    "worker-1",
				AuthToken:   "token",
				HTTPClient:  server.Client(),
				BaseDelay:   1,
			})
			if err != nil {
				t.Fatalf("result.NewPoster: %v", err)
			}
			provider := &promptCaptureInteractiveProvider{
				name: tt.providerName, caps: tt.caps, manifest: tt.manifest,
			}
			registry := NewRegistry()
			if err := registry.Register(provider); err != nil {
				t.Fatalf("Register: %v", err)
			}
			runner, err := New(Options{
				Registry:               registry,
				WorktreeManager:        manager,
				Poster:                 poster,
				HTTPClient:             server.Client(),
				PreserveWorktreeAlways: true,
				MaxSessionDuration:     -1,
				SkipBackstop:           true,
				SkipSteering:           true,
				SkipPostSession:        true,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			qw := QueuedWork{
				QueuedWork: prompt.QueuedWork{
					SessionID:       "interactive-prompt-" + string(tt.providerName),
					IssueID:         "issue-id",
					IssueIdentifier: "ISSUE-1",
					WorkType:        "development",
					Mode:            prompt.InteractiveRunMode,
					InitialPrompt:   seed,
					Repository:      makeBareRepo(t),
				},
				WorkerID:        "worker-1",
				AuthToken:       "token",
				PlatformURL:     server.URL,
				ResolvedProfile: ResolvedProfile{Provider: tt.providerName},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			got, err := runner.Run(ctx, qw)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.Status != "completed" {
				t.Fatalf("Run status = %q error=%q", got.Status, got.Error)
			}
			if provider.raw.PromptPlan == nil || provider.raw.PromptPlan.UserPrompt.Text != seed || !provider.raw.PromptPlan.UserPrompt.Required {
				t.Fatalf("runner PromptPlan user task = %+v, want exact required InitialPrompt", provider.raw.PromptPlan)
			}
			if provider.raw.Prompt != seed || provider.adapted.Prompt != seed {
				t.Fatalf("prompt bytes raw=%q adapted=%q, want %q", provider.raw.Prompt, provider.adapted.Prompt, seed)
			}
			if provider.session.writeCount() != 0 {
				t.Fatalf("dispatchInteractive wrote task %d time(s), want zero after native delivery", provider.session.writeCount())
			}
			receipt := provider.adapted.PromptReceipt
			if receipt == nil || receipt.Decision != "ready" {
				t.Fatalf("adapted receipt = %+v", receipt)
			}
			assertInteractiveUserTaskReceipt(t, receipt, tt.wantDelivery)
			persisted, err := state.NewStore().Read(provider.raw.Cwd)
			if err != nil {
				t.Fatalf("read state: %v", err)
			}
			if persisted.PromptReceipt == nil || persisted.PromptReceipt.ProfileID != receipt.ProfileID {
				t.Fatalf("persisted receipt = %+v, want profile %q", persisted.PromptReceipt, receipt.ProfileID)
			}
			assertInteractiveUserTaskReceipt(t, persisted.PromptReceipt, tt.wantDelivery)
		})
	}
}

func TestRun_ToolLifecycleAdmissionPersistsDenialAndFailsClosed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tests := []struct {
		name        string
		wantCode    agent.ToolAdaptationDenialCode
		configure   func(*toolLifecycleInteractiveProvider)
		assertState func(*testing.T, *toolLifecycleInteractiveProvider)
	}{
		{
			name:     "restored ready projection becomes append-only denied projection",
			wantCode: agent.ToolDenialDeliveryUnsupported,
			configure: func(p *toolLifecycleInteractiveProvider) {
				p.seedToolReceipt = &agent.ToolLifecycleReceipt{ContractVersion: agent.ToolLifecycleContractVersion, ProfileID: "prior/profile", Decision: "ready"}
				for i := range p.manifest.ToolLifecycle {
					p.manifest.ToolLifecycle[i].NativeToolPolicyDelivery = agent.ToolDeliveryUnsupported
				}
			},
			assertState: func(t *testing.T, p *toolLifecycleInteractiveProvider) {
				persisted, err := state.NewStore().Read(p.raw.Cwd)
				if err != nil {
					t.Fatalf("read denied state: %v", err)
				}
				if persisted.ToolLifecycleReceipt == nil || persisted.ToolLifecycleReceipt.Decision != "denied" {
					t.Fatalf("current projection = %+v, want denied", persisted.ToolLifecycleReceipt)
				}
				if len(persisted.ToolLifecycleReceiptHistory) != 2 || persisted.ToolLifecycleReceiptHistory[0].Decision != "ready" || persisted.ToolLifecycleReceiptHistory[1].Decision != "denied" {
					t.Fatalf("append-only history = %+v, want ready then denied", persisted.ToolLifecycleReceiptHistory)
				}
			},
		},
		{
			name:     "receipt store failure",
			wantCode: agent.ToolDenialApplicationFailed,
			configure: func(p *toolLifecycleInteractiveProvider) {
				p.breakToolReceiptStore = true
			},
			assertState: func(_ *testing.T, _ *toolLifecycleInteractiveProvider) {},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envAttachURL, "")
			t.Setenv(envAttachToken, "")
			server := mockPlatformServer(t)
			defer server.Close()
			manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			poster, err := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: "w", AuthToken: "t", HTTPClient: server.Client(), BaseDelay: 1})
			if err != nil {
				t.Fatal(err)
			}
			provider := &toolLifecycleInteractiveProvider{manifest: (&claude.Provider{}).Manifest()}
			tt.configure(provider)
			registry := NewRegistry()
			if err := registry.Register(provider); err != nil {
				t.Fatal(err)
			}
			runner, err := New(Options{Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(), PreserveWorktreeAlways: true, MaxSessionDuration: -1, SkipBackstop: true, SkipSteering: true, SkipPostSession: true})
			if err != nil {
				t.Fatal(err)
			}
			qw := QueuedWork{
				QueuedWork: prompt.QueuedWork{SessionID: "tool-lifecycle-" + tt.name, IssueID: "issue", IssueIdentifier: "ISSUE-TOOL", WorkType: "development", Mode: prompt.InteractiveRunMode, InitialPrompt: "test", Repository: makeBareRepo(t), AllowedTools: []string{"Read"}},
				WorkerID:   "w", AuthToken: "t", PlatformURL: server.URL, ResolvedProfile: ResolvedProfile{Provider: agent.ProviderClaude},
			}
			got, runErr := runner.Run(context.Background(), qw)
			if runErr == nil || got.FailureMode != FailureSpawn {
				t.Fatalf("Run result=%+v err=%v, want spawn failure", got, runErr)
			}
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(runErr, &adaptationErr) || adaptationErr.Code != tt.wantCode {
				t.Fatalf("error = %v, want typed %s tool adaptation error", runErr, tt.wantCode)
			}
			if provider.sideEffects != 0 || provider.persistedBeforeSideEffect {
				t.Fatalf("provider side effects=%d persisted-ready=%v, want zero", provider.sideEffects, provider.persistedBeforeSideEffect)
			}
			tt.assertState(t, provider)
		})
	}
}

func TestRun_InteractiveDefaultHTTPMCPReachesCodexCLI(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")
	t.Setenv("OPENAI_API_KEY", "runner-codex-auth-fixture")
	server := mockPlatformServer(t)
	defer server.Close()
	bin := filepath.Join(t.TempDir(), "fake-codex")
	script := `#!/bin/bash
if [[ " $* " == *" mcp list --json "* || " $* " == *" mcp get "* ]]; then
  for arg in "$@"; do
    case "$arg" in
      mcp_servers=*) mcp_config="$arg" ;;
    esac
  done
  url=$(printf '%s' "$mcp_config" | sed -n 's/.*"url"="\([^"]*\)".*/\1/p')
  header_env=$(printf '%s' "$mcp_config" | sed -n 's/.*"Authorization"="\([^"]*\)".*/\1/p')
  server=$(printf '{"name":"donmai-platform","enabled":true,"disabled_reason":null,"transport":{"type":"streamable_http","url":"%s","bearer_token_env_var":null,"http_headers":null,"env_http_headers":{"Authorization":"%s"},"http_headers_helper":null},"enabled_tools":null,"disabled_tools":null,"startup_timeout_sec":null,"tool_timeout_sec":null}' "$url" "$header_env")
  if [[ " $* " == *" mcp list --json "* ]]; then
    printf '[%s]\n' "$server"
  else
    printf '%s\n' "$server"
  fi
  exit 0
fi
printf '%s\n' "$@" > "$PWD/codex-argv"
env | LC_ALL=C sort | grep '^DONMAI_MCP_HEADER_' > "$PWD/codex-mcp-env" || true
`
	if err := os.WriteFile(bin, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: "w", AuthToken: "t", HTTPClient: server.Client(), BaseDelay: 1})
	if err != nil {
		t.Fatal(err)
	}
	provider := &codexCLIInteractiveProvider{binary: bin}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	runner, err := New(Options{Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(), PreserveWorktreeAlways: true, MaxSessionDuration: -1, SkipBackstop: true, SkipSteering: true, SkipPostSession: true})
	if err != nil {
		t.Fatal(err)
	}
	const token = "RUNNER_MCP_SECRET_DO_NOT_PUT_IN_ARGV"
	qw := QueuedWork{
		QueuedWork: prompt.QueuedWork{SessionID: "codex-default-mcp", IssueID: "issue", IssueIdentifier: "ISSUE-CODEX", WorkType: "development", Mode: prompt.InteractiveRunMode, InitialPrompt: "test", Repository: makeBareRepo(t)},
		WorkerID:   "w", AuthToken: token, PlatformURL: server.URL, ResolvedProfile: ResolvedProfile{Provider: agent.ProviderCodex},
	}
	got, err := runner.Run(context.Background(), qw)
	if err != nil || got.Status != "completed" {
		t.Fatalf("Run result=%+v err=%v", got, err)
	}
	argv, err := os.ReadFile(filepath.Join(provider.raw.Cwd, "codex-argv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "mcp_servers=") || !strings.Contains(string(argv), "/api/mcp/codex-default-mcp") || strings.Contains(string(argv), token) {
		t.Fatalf("Codex argv did not carry a secret-free default HTTP MCP override: %s", argv)
	}
	childEnv, err := os.ReadFile(filepath.Join(provider.raw.Cwd, "codex-mcp-env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(childEnv), "Bearer "+token) {
		t.Fatalf("Codex child env omitted HTTP header secret: %s", childEnv)
	}
	persisted, err := state.NewStore().Read(provider.raw.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ToolLifecycleReceipt == nil || persisted.ToolLifecycleReceipt.Decision != "ready" || len(persisted.ToolLifecycleReceiptHistory) != 1 {
		t.Fatalf("tool lifecycle state = %+v history=%+v", persisted.ToolLifecycleReceipt, persisted.ToolLifecycleReceiptHistory)
	}
}

func assertInteractiveUserTaskReceipt(t *testing.T, receipt *agent.PromptDeliveryReceipt, want agent.PromptDeliveryKind) {
	t.Helper()
	for _, entry := range receipt.Entries {
		if entry.ID != "runner-user-task" {
			continue
		}
		if entry.Channel != agent.PromptChannelUserPrompt || entry.Outcome != agent.PromptOutcomeDelivered || entry.Delivery != want {
			t.Fatalf("runner user-task receipt = %+v, want delivered via %s", entry, want)
		}
		return
	}
	t.Fatal("receipt omitted runner-user-task")
}

func TestRun_InteractiveInitialPromptOversizeFailsBeforeProviderSpawn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// The direct dispatch test keeps its defensive boundary; this regression
	// proves the real Runner.Run path rejects before Provider.Spawn or receipt.
	provider := &promptCaptureInteractiveProvider{
		name: agent.ProviderClaude, caps: (&claude.Provider{}).Capabilities(), manifest: (&claude.Provider{}).Manifest(),
	}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	server := mockPlatformServer(t)
	defer server.Close()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: "w", AuthToken: "t", HTTPClient: server.Client(), BaseDelay: 1})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := New(Options{Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(), PreserveWorktreeAlways: true, SkipBackstop: true, SkipSteering: true, SkipPostSession: true})
	if err != nil {
		t.Fatal(err)
	}
	qw := QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID: "oversize", IssueID: "issue", IssueIdentifier: "ISSUE-2", WorkType: "development",
			Mode: prompt.InteractiveRunMode, InitialPrompt: string(make([]byte, maxInitialPromptBytes+1)), Repository: makeBareRepo(t),
		},
		WorkerID: "w", AuthToken: "t", PlatformURL: server.URL, ResolvedProfile: ResolvedProfile{Provider: agent.ProviderClaude},
	}
	got, err := runner.Run(context.Background(), qw)
	if err == nil || got.FailureMode != FailureInteractiveInput {
		t.Fatalf("Run result=%+v err=%v, want pre-spawn interactive-input failure", got, err)
	}
	if provider.raw.PromptPlan != nil || provider.session != nil {
		t.Fatal("provider Spawn ran for oversized InitialPrompt")
	}
}

func TestRun_ShellExecutesOnlyExplicitUserSeed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell PTY regression is unix-only")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")
	t.Setenv("SHELL", "/bin/sh")

	tests := []struct {
		name       string
		userSeed   bool
		wantUserFX bool
	}{
		{name: "explicit user task executes", userSeed: true, wantUserFX: true},
		{name: "empty initial prompt executes nothing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			markerDir := t.TempDir()
			userMarker := filepath.Join(markerDir, "user-executed")
			protocolMarker := filepath.Join(markerDir, "protocol-executed")
			roleMarker := filepath.Join(markerDir, "role-executed")
			contextMarker := filepath.Join(markerDir, "context-executed")

			server := mockPlatformServer(t)
			defer server.Close()
			manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatalf("worktree.NewManager: %v", err)
			}
			poster, err := result.NewPoster(result.Options{
				PlatformURL: server.URL,
				WorkerID:    "worker-shell",
				AuthToken:   "token",
				HTTPClient:  server.Client(),
				BaseDelay:   1,
			})
			if err != nil {
				t.Fatalf("result.NewPoster: %v", err)
			}
			provider, err := shellprovider.New()
			if err != nil {
				t.Fatalf("shell.New: %v", err)
			}
			registry := NewRegistry()
			if err := registry.Register(provider); err != nil {
				t.Fatalf("Register: %v", err)
			}
			runner, err := New(Options{
				Registry:               registry,
				WorktreeManager:        manager,
				Poster:                 poster,
				HTTPClient:             server.Client(),
				PreserveWorktreeAlways: true,
				MaxSessionDuration:     -1,
				SkipBackstop:           true,
				SkipSteering:           true,
				SkipPostSession:        true,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			initialPrompt := ""
			if tt.userSeed {
				initialPrompt = "printf user > " + strconv.Quote(userMarker) + "; exit"
			}
			qw := QueuedWork{
				QueuedWork: prompt.QueuedWork{
					SessionID:            "interactive-shell",
					IssueID:              "issue-id",
					IssueIdentifier:      "ISSUE-SHELL",
					WorkType:             "development",
					Mode:                 prompt.InteractiveRunMode,
					InitialPrompt:        initialPrompt,
					Repository:           makeBareRepo(t),
					SystemPromptOverride: "touch " + strconv.Quote(roleMarker),
					MemoryBlock:          "touch " + strconv.Quote(contextMarker),
					Skills: []prompt.SkillSpec{{
						ID: "forbidden-system-command", Body: "touch " + strconv.Quote(protocolMarker),
					}},
					InterviewBudget: &prompt.InterviewBudget{MaxWallClockSeconds: 1},
				},
				WorkerID:        "worker-shell",
				AuthToken:       "token",
				PlatformURL:     server.URL,
				ResolvedProfile: ResolvedProfile{Provider: agent.ProviderShell},
			}
			got, runErr := runner.Run(context.Background(), qw)
			if tt.userSeed {
				if runErr != nil || got.Status != "completed" {
					t.Fatalf("Run result=%+v err=%v, want completed user-seeded shell", got, runErr)
				}
			} else if got.WorktreePath == "" {
				t.Fatalf("Run result=%+v err=%v, shell was not provisioned", got, runErr)
			}

			if _, err := os.Stat(userMarker); tt.wantUserFX && err != nil {
				t.Fatalf("explicit user seed was not executed: %v", err)
			} else if !tt.wantUserFX && err == nil {
				t.Fatal("empty initial prompt caused a user command side effect")
			}
			for authority, marker := range map[string]string{
				"harness protocol": protocolMarker,
				"role intent":      roleMarker,
				"initial context":  contextMarker,
			} {
				if _, err := os.Stat(marker); err == nil {
					t.Fatalf("%s was executed by the bare shell", authority)
				} else if !os.IsNotExist(err) {
					t.Fatalf("stat %s marker: %v", authority, err)
				}
			}
		})
	}
}
