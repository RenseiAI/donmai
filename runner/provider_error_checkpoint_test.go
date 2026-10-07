package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

func TestRun_ProviderErrorPushesWIPCheckpoint(t *testing.T) {
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	harness, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	platform := newRecordingPlatformServer(t)
	provider := &verdictScriptProvider{HarnessProvider: harness, t: t, turns: []verdictScriptTurn{
		{files: map[string]string{"notes.txt": "checkpoint me\n"}, providerError: "503 Service Unavailable"},
		{providerError: "503 Service Unavailable"},
	}}
	bare := makeBareRepo(t)
	r := newFollowUpRunner(t, platform.URL, platform.Client(), provider, func(o *Options) {
		o.TurnContinuationLimit = 1
	})
	work := queuedWorkBase("WIP-CHECKPOINT")
	work.Repository = bare
	qw := QueuedWork{
		QueuedWork:      work,
		WorkerID:        "worker-1",
		AuthToken:       "tok",
		PlatformURL:     platform.URL,
		ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, _ := r.Run(ctx, qw)
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("status=%q failureMode=%q err=%q; want failed/%s", res.Status, res.FailureMode, res.Error, FailureProviderError)
	}
	if !res.Resumable || res.ResumeCheckpoint == nil {
		t.Fatalf("Resumable=%v ResumeCheckpoint=%+v; want a resumable WIP checkpoint", res.Resumable, res.ResumeCheckpoint)
	}
	wantBranch := "wip/" + qw.SessionID
	if res.ResumeCheckpoint.Branch != wantBranch {
		t.Fatalf("ResumeCheckpoint.Branch = %q, want %q", res.ResumeCheckpoint.Branch, wantBranch)
	}
	if got := gitRun(t, bare, "rev-parse", "refs/heads/"+wantBranch); got != res.ResumeCheckpoint.CommitSHA {
		t.Fatalf("remote WIP head = %q, want %q", got, res.ResumeCheckpoint.CommitSHA)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	gitRun(t, filepath.Dir(restored), "clone", "--quiet", bare, restored)
	gitRun(t, restored, "checkout", "--quiet", wantBranch)
	if got := readFile(t, filepath.Join(restored, "notes.txt")); got != "checkpoint me\n" {
		t.Fatalf("notes.txt = %q, want the dirty work on the WIP branch", got)
	}
}

func TestRun_ProviderErrorCheckpointFailureDoesNotMaskOriginalFailure(t *testing.T) {
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	harness, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	platform := newRecordingPlatformServer(t)
	provider := &verdictScriptProvider{HarnessProvider: harness, t: t, turns: []verdictScriptTurn{{files: map[string]string{"notes.txt": "checkpoint me\n"}, providerError: "400 The request contains invalid parameters"}}}
	bare := makeBareRepo(t)
	installProtectedBranchHook(t, bare, "wip/test-session-WIP-CHECKPOINT-FAIL")
	r := newFollowUpRunner(t, platform.URL, platform.Client(), provider, nil)
	work := queuedWorkBase("WIP-CHECKPOINT-FAIL")
	work.Repository = bare
	qw := QueuedWork{
		QueuedWork:      work,
		WorkerID:        "worker-1",
		AuthToken:       "tok",
		PlatformURL:     platform.URL,
		ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, _ := r.Run(ctx, qw)
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("status=%q failureMode=%q err=%q; want failed/%s", res.Status, res.FailureMode, res.Error, FailureProviderError)
	}
	if res.Resumable || res.ResumeCheckpoint != nil {
		t.Fatalf("Resumable=%v ResumeCheckpoint=%+v; want no retry checkpoint after a failed push", res.Resumable, res.ResumeCheckpoint)
	}
	if res.Error == "" || !strings.Contains(res.Error, "invalid parameters") {
		t.Fatalf("Error = %q; want the original provider error to stay authoritative", res.Error)
	}
	var checkpointWarnings []string
	for _, warning := range res.PostSessionWarnings {
		if strings.Contains(warning, "WIP checkpoint failed") {
			checkpointWarnings = append(checkpointWarnings, warning)
		}
	}
	if len(checkpointWarnings) != 1 {
		t.Fatalf("PostSessionWarnings = %v; want one checkpoint warning", res.PostSessionWarnings)
	}
}

// checkpointCheckout clones a fresh remote into a session checkout and returns
// the remote, the checkout, and a provider-error result whose one rescue
// target starts at the clone's HEAD.
func checkpointCheckout(t *testing.T) (string, string, *Result) {
	t.Helper()
	bare := makeBareRepo(t)
	wt := filepath.Join(t.TempDir(), "checkout")
	gitRun(t, filepath.Dir(wt), "clone", "--quiet", bare, wt)
	res := &Result{}
	res.Status = "failed"
	res.FailureMode = FailureProviderError
	res.Error = "the turn still ended on a model provider error after 1 retries: 503"
	res.WorktreePath = wt
	res.rescueTargets = []rescueTarget{{path: wt, base: gitRun(t, wt, "rev-parse", "HEAD")}}
	return bare, wt, res
}

func remoteBranchExists(bare, branch string) bool {
	_, err := runGit(context.Background(), bare, gitIdentity{}, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func TestCheckpointProviderError_NothingOfTheSessionsOwnPushesNoBranch(t *testing.T) {
	tests := []struct {
		name  string
		dirty map[string]string
	}{
		{name: "clean at the starting commit"},
		{name: "only runner state and dependency output", dirty: map[string]string{
			".agent/events.jsonl":     "{}\n",
			"node_modules/x/index.js": "x\n",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bare, wt, res := checkpointCheckout(t)
			for path, body := range tc.dirty {
				writeFile(t, wt, path, body)
			}
			qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-NOTHING")}
			(&Runner{logger: noopLogger()}).checkpointProviderError(qw, res)
			if remoteBranchExists(bare, "wip/"+qw.SessionID) {
				t.Fatalf("a session with no work of its own pushed wip/%s", qw.SessionID)
			}
			if !res.Resumable || res.ResumeCheckpoint != nil || len(res.PostSessionWarnings) != 0 {
				t.Fatalf("Resumable=%v ResumeCheckpoint=%+v warnings=%v; want resumable without a checkpoint",
					res.Resumable, res.ResumeCheckpoint, res.PostSessionWarnings)
			}
		})
	}
}

func TestCheckpointProviderError_CommittedWorkIsCheckpointedAtHead(t *testing.T) {
	bare, wt, res := checkpointCheckout(t)
	qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-COMMITTED")}
	gitRun(t, wt, "checkout", "--quiet", "-b", "agent/"+qw.SessionID)
	writeFile(t, wt, "feature.txt", "done\n")
	gitRun(t, wt, "add", "feature.txt")
	gitRun(t, wt, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "agent commit")
	head := gitRun(t, wt, "rev-parse", "HEAD")
	(&Runner{logger: noopLogger()}).checkpointProviderError(qw, res)
	if res.ResumeCheckpoint == nil || res.ResumeCheckpoint.CommitSHA != head {
		t.Fatalf("ResumeCheckpoint = %+v, want HEAD %s (warnings %v)", res.ResumeCheckpoint, head, res.PostSessionWarnings)
	}
	if got := gitRun(t, bare, "rev-parse", "refs/heads/wip/"+qw.SessionID); got != head {
		t.Fatalf("remote WIP head = %s, want %s", got, head)
	}
}

func TestCheckpointProviderError_VerdictOnlyWorkPushesNothing(t *testing.T) {
	bare, wt, res := checkpointCheckout(t)
	writeFile(t, wt, ".scratch/probe/notes.md", "reviewer scratch\n")
	work := queuedWorkBase("WIP-QA")
	work.WorkType = WorkTypeQAStr
	qw := QueuedWork{QueuedWork: work}
	(&Runner{logger: noopLogger()}).checkpointProviderError(qw, res)
	if remoteBranchExists(bare, "wip/"+qw.SessionID) {
		t.Fatalf("verdict-only session pushed its scratch to wip/%s", qw.SessionID)
	}
	if !res.Resumable || res.ResumeCheckpoint != nil {
		t.Fatalf("Resumable=%v ResumeCheckpoint=%+v; want resumable without a checkpoint", res.Resumable, res.ResumeCheckpoint)
	}
}

// TestCheckpointProviderError_OwnsOnlyTheRemoteWIPBranch pins that the
// checkpoint writes no local ref (so it cannot move the checked-out branch,
// even one named like the WIP branch) and that repository hooks do not run.
func TestCheckpointProviderError_OwnsOnlyTheRemoteWIPBranch(t *testing.T) {
	bare, wt, res := checkpointCheckout(t)
	qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-OWNERSHIP")}
	wipBranch := "wip/" + qw.SessionID
	gitRun(t, wt, "checkout", "--quiet", "-b", wipBranch)
	//nolint:gosec // G306: a hook stub must carry the exec bit.
	if err := os.WriteFile(filepath.Join(wt, ".git", "hooks", "pre-push"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write pre-push hook: %v", err)
	}
	writeFile(t, wt, "notes.txt", "work\n")
	headBefore := gitRun(t, wt, "rev-parse", "HEAD")
	statusBefore := gitRun(t, wt, "status", "--porcelain=v1")
	(&Runner{logger: noopLogger()}).checkpointProviderError(qw, res)
	if res.ResumeCheckpoint == nil {
		t.Fatalf("no checkpoint: warnings %v", res.PostSessionWarnings)
	}
	if got := gitRun(t, bare, "rev-parse", "refs/heads/"+wipBranch); got != res.ResumeCheckpoint.CommitSHA {
		t.Fatalf("remote WIP head = %s, want %s", got, res.ResumeCheckpoint.CommitSHA)
	}
	if got := gitRun(t, wt, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("checked-out branch moved: %s -> %s", headBefore, got)
	}
	if got := gitRun(t, wt, "status", "--porcelain=v1"); got != statusBefore {
		t.Fatalf("checkout status changed:\n%s\nwant:\n%s", got, statusBefore)
	}
}

func TestCheckpointProviderError_BoundedWhenTheTransportHangs(t *testing.T) {
	restore := providerErrorCheckpointTimeout
	providerErrorCheckpointTimeout = time.Second
	t.Cleanup(func() { providerErrorCheckpointTimeout = restore })

	_, wt, res := checkpointCheckout(t)
	// A transport child that outlives the killed git and holds its output.
	gitRun(t, wt, "remote", "set-url", "origin", "ssh://checkpoint.invalid/repo.git")
	gitRun(t, wt, "config", "core.sshCommand", "sleep 10; :")
	gitRun(t, wt, "config", "ssh.variant", "ssh") // skip the variant probe, which runs with closed output
	writeFile(t, wt, "notes.txt", "work\n")
	qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-HANG")}
	start := time.Now()
	(&Runner{logger: noopLogger()}).checkpointProviderError(qw, res)
	if elapsed := time.Since(start); elapsed > providerErrorCheckpointTimeout+providerErrorCheckpointWaitDelay+5*time.Second {
		t.Fatalf("checkpoint took %s past its %s bound", elapsed, providerErrorCheckpointTimeout)
	}
	if res.Resumable || len(res.PostSessionWarnings) != 1 {
		t.Fatalf("Resumable=%v warnings=%v; want one checkpoint warning", res.Resumable, res.PostSessionWarnings)
	}
}
