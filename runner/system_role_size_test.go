package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/provider/harness/muse"
	providerstub "github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// Long-role matrix (revision 224fcc8d, headless autonomous runs).
// Mode headless, transports: claude CLI argument, codex thread/start
// instructions, muse turn/start leading text. Units UTF-8 bytes. No
// adapter declares a role-byte cap, so every successful probe below is a
// lower bound only. Largest proven role per row is the 80000-byte sample.
func sizedRole(t *testing.T, n int) string {
	t.Helper()
	start := "[[ROLEBEGIN-9f3a]]"
	end := "]]-9f3a-ROLEEND-\u2713"
	chunk := "q\u00e9\u2713\n"
	overhead := len(start) + len(end)
	if n <= overhead {
		t.Fatalf("fixture size %d too small for markers", n)
	}
	middleLen := n - overhead
	middle := strings.Repeat(chunk, middleLen/len(chunk)) + strings.Repeat("q", middleLen%len(chunk))
	role := start + middle + end
	if len(role) != n {
		t.Fatalf("fixture is %d bytes, want %d", len(role), n)
	}
	if role != strings.TrimSpace(role) {
		t.Fatal("fixture has outer whitespace")
	}
	return role
}

type roleCaptureProvider struct {
	agent.Provider
	name     agent.ProviderName
	caps     agent.Capabilities
	manifest agent.HarnessManifest
	raw      agent.Spec
	adapted  agent.Spec
}

func (p *roleCaptureProvider) Name() agent.ProviderName         { return p.name }
func (p *roleCaptureProvider) Capabilities() agent.Capabilities { return p.caps }
func (p *roleCaptureProvider) Manifest() agent.HarnessManifest  { return p.manifest }
func (p *roleCaptureProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.raw = spec
	adapted, err := agent.PrepareHarness(spec, p.manifest)
	if err != nil {
		return nil, err
	}
	p.adapted = adapted
	return p.Provider.Spawn(ctx, adapted)
}

func TestRun_SystemRoleReachesSpawnIntact(t *testing.T) {
	harnesses := []struct {
		name     agent.ProviderName
		caps     agent.Capabilities
		manifest agent.HarnessManifest
	}{
		{agent.ProviderClaude, (&claude.Provider{}).Capabilities(), (&claude.Provider{}).Manifest()},
		{agent.ProviderCodex, (&codex.Provider{}).Capabilities(), (&codex.Provider{}).Manifest()},
		{agent.ProviderMuse, (&muse.Provider{}).Capabilities(), (&muse.Provider{}).Manifest()},
	}
	for _, h := range harnesses {
		for _, size := range []int{32768, 38912, 80000} {
			role := sizedRole(t, size)
			inner, err := providerstub.New()
			if err != nil {
				t.Fatalf("stub.New: %v", err)
			}
			provider := &roleCaptureProvider{Provider: inner, name: h.name, caps: h.caps, manifest: h.manifest}
			registry := NewRegistry()
			if err := registry.Register(provider); err != nil {
				t.Fatalf("Register: %v", err)
			}
			server := mockPlatformServer(t)
			t.Cleanup(server.Close)
			manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatalf("worktree.NewManager: %v", err)
			}
			poster, err := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: "w", AuthToken: "t", HTTPClient: server.Client(), BaseDelay: 1})
			if err != nil {
				t.Fatalf("result.NewPoster: %v", err)
			}
			run, err := New(Options{Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(), PreserveWorktreeAlways: true, SkipBackstop: true, SkipSteering: true, SkipPostSession: true})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			qw := QueuedWork{
				QueuedWork:  queuedWorkBase("role-size"),
				WorkerID:    "w",
				AuthToken:   "t",
				PlatformURL: server.URL,
			}
			qw.SessionID = "role-size-" + string(h.name) + "-" + string(rune('0'+size%10))
			qw.SystemPromptOverride = role
			qw.Repository = makeBareRepo(t)
			qw.ResolvedProfile.Provider = h.name
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			got, err := run.Run(ctx, qw)
			cancel()
			if err != nil {
				t.Fatalf("%s size %d: Run: %v", h.name, size, err)
			}
			if got.Status != "completed" {
				t.Fatalf("%s size %d: status %q error %q", h.name, size, got.Status, got.Error)
			}
			if provider.raw.PromptPlan == nil || provider.raw.PromptPlan.RoleIntent == nil || provider.raw.PromptPlan.RoleIntent.Text != role {
				t.Fatalf("%s size %d: RoleIntent mismatch", h.name, size)
			}
			if c := strings.Count(provider.adapted.SystemPromptAppend, role); c != 1 {
				t.Fatalf("%s size %d: role occurrences %d, want 1", h.name, size, c)
			}
		}
	}
}
