package runner

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
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
