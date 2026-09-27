package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// manifestWritingProvider wraps the stub harness and plays the agent's part of
// the turn-result contract: it writes `.agent/turn-result.json` into the
// session worktree either while the FIRST turn runs (Spawn) or while the
// steering follow-up turn runs (Resume — the stub's resume-steer script with
// injection disabled forces the runner's stop-and-resume steering rail).
type manifestWritingProvider struct {
	agent.HarnessProvider
	t        *testing.T
	manifest string
	onSpawn  bool
	onResume bool
}

func (p *manifestWritingProvider) write(cwd string) {
	p.t.Helper()
	dir := filepath.Join(cwd, state.AgentDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		p.t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(p.manifest), 0o600); err != nil {
		p.t.Fatalf("write turn-result manifest: %v", err)
	}
}

func (p *manifestWritingProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if p.onSpawn {
		p.write(spec.Cwd)
	}
	return p.HarnessProvider.Spawn(ctx, spec)
}

func (p *manifestWritingProvider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	if p.onResume {
		p.write(spec.Cwd)
	}
	return p.HarnessProvider.Resume(ctx, sessionID, spec)
}

// TestRun_TurnManifestVerdictSurvivesSteeringFollowUp pins the verdict
// resolution across a steering follow-up turn. The first turn finishes cleanly
// without a PR, so steering fires; the follow-up turn opens the PR and ends
// with a terminal message that carries only a PR pointer. When the agent's
// turn-result manifest says anything other than "passed" — whether it was
// written during the follow-up turn or before it — the envelope must carry
// that verdict and the manifest's own summary, never the follow-up's bare
// terminal text. With no manifest the envelope keeps today's behaviour.
func TestRun_TurnManifestVerdictSurvivesSteeringFollowUp(t *testing.T) {
	const (
		prURL          = "https://github.com/example/repo/pull/7"
		failedManifest = `{"schemaVersion":1,"verdict":"failed","summary":"round-two items not implemented; round-one work pushed","pullRequestUrl":"` + prURL + `"}`
		failedSummary  = "round-two items not implemented; round-one work pushed"
		steeredSummary = "Stub resumed run complete (PR: stub://pr/123)"
	)
	cases := []struct {
		name           string
		onSpawn        bool
		onResume       bool
		manifest       string
		wantWorkResult string
		wantSummary    string
		wantPR         string
		wantManifest   bool
	}{
		{
			name:           "manifest written during the steering follow-up turn is read",
			onResume:       true,
			manifest:       failedManifest,
			wantWorkResult: "failed",
			wantSummary:    failedSummary,
			wantPR:         prURL,
			wantManifest:   true,
		},
		{
			name:           "first-turn manifest outranks the follow-up terminal message",
			onSpawn:        true,
			manifest:       `{"schemaVersion":1,"verdict":"failed","summary":"` + failedSummary + `"}`,
			wantWorkResult: "failed",
			wantSummary:    failedSummary,
			wantManifest:   true,
		},
		{
			name:        "no manifest keeps the follow-up terminal message",
			wantSummary: steeredSummary,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, err := stub.New()
			if err != nil {
				t.Fatalf("stub.New: %v", err)
			}
			harness, ok := base.(agent.HarnessProvider)
			if !ok {
				t.Fatal("stub provider is not a HarnessProvider")
			}
			r, platformURL := newFollowUpRunner(t, &manifestWritingProvider{
				HarnessProvider: harness,
				t:               t,
				manifest:        tc.manifest,
				onSpawn:         tc.onSpawn,
				onResume:        tc.onResume,
			})
			qw := QueuedWork{
				QueuedWork:      queuedWorkBase("MANIFEST-FOLLOWUP"),
				WorkerID:        "worker-1",
				AuthToken:       "tok",
				PlatformURL:     platformURL,
				ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
			}
			qw.Repository = makeBareRepo(t)
			qw.ResolvedProfile.ProviderConfig = map[string]any{
				"stub.behavior":          string(stub.BehaviorResumeSteer),
				"stub.injectUnsupported": true,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := r.Run(ctx, qw)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !res.SteeringTriggered || !res.SteeringResumeFallback {
				t.Fatalf("steering follow-up did not run (triggered=%v resume=%v)", res.SteeringTriggered, res.SteeringResumeFallback)
			}
			if res.Status != "completed" {
				t.Fatalf("Status = %q; want completed (FailureMode=%q, Error=%q)", res.Status, res.FailureMode, res.Error)
			}
			if res.WorkResult != tc.wantWorkResult {
				t.Errorf("WorkResult = %q; want %q", res.WorkResult, tc.wantWorkResult)
			}
			if res.Summary != tc.wantSummary {
				t.Errorf("Summary = %q; want %q", res.Summary, tc.wantSummary)
			}
			if tc.wantPR != "" && res.PullRequestURL != tc.wantPR {
				t.Errorf("PullRequestURL = %q; want %q", res.PullRequestURL, tc.wantPR)
			}
			if got := res.Manifest != nil; got != tc.wantManifest {
				t.Fatalf("Manifest present = %v; want %v", got, tc.wantManifest)
			}
			if tc.wantManifest && res.Manifest.Verdict != tc.wantWorkResult {
				t.Errorf("Manifest.Verdict = %q; want %q", res.Manifest.Verdict, tc.wantWorkResult)
			}
		})
	}
}

// newFollowUpRunner builds a Runner with tail steering enabled around the
// supplied provider. Backstop and post-session stay off so the test observes
// exactly the manifest / steering interaction. Returns the runner and the mock
// platform URL the QueuedWork must carry.
func newFollowUpRunner(t *testing.T, p agent.Provider) (*Runner, string) {
	t.Helper()
	srv := mockPlatformServer(t)
	t.Cleanup(srv.Close)
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	reg := NewRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry:               reg,
		WorktreeManager:        wtm,
		Poster:                 poster,
		HTTPClient:             srv.Client(),
		SkipBackstop:           true,
		SkipPostSession:        true,
		PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, srv.URL
}
