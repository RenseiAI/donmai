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

// TestCheckpointFailedSession_ExcludesSecretsFromWIPCommit pins the deny
// list: deny-listed files plus a nested state dir (with no info/exclude)
// next to real work must not appear in the pushed WIP tree, while the real
// work does. Reverting the deny list turns it RED.
func TestCheckpointFailedSession_ExcludesSecretsFromWIPCommit(t *testing.T) {
	bare := makeBareRepo(t)
	wt := filepath.Join(t.TempDir(), "checkout")
	gitRun(t, filepath.Dir(wt), "clone", "--quiet", bare, wt)
	res := &Result{}
	res.Status = "failed"
	res.FailureMode = FailureBudgetExceeded
	res.WorktreePath = wt
	res.rescueTargets = []rescueTarget{{path: wt, base: gitRun(t, wt, "rev-parse", "HEAD")}}

	writeFile(t, wt, "feature.txt", "real work\n")
	secrets := map[string]string{
		".env":                      "KEY=live\n",
		".env.local":                "KEY=live\n",
		"config/.env.staging":       "KEY=live\n",
		".envrc":                    "export KEY=live\n",
		".npmrc":                    "//registry/:_authToken=live\n",
		".pypirc":                   "password=live\n",
		".netrc":                    "password live\n",
		".git-credentials":          "https://user:live@example.test\n",
		"deploy.pem":                "private\n",
		"tls/server.key":            "private\n",
		"bundle.p12":                "private\n",
		"store.pfx":                 "private\n",
		"id_rsa":                    "private\n",
		"keys/id_ed25519_ci":        "private\n",
		"credentials.json":          `{"k":"live"}` + "\n",
		"service-account-prod.json": `{"k":"live"}` + "\n",
		".aws/credentials":          "aws_secret_access_key=live\n",
		".ssh/config":               "IdentityFile live\n",
		"sub/.gnupg/pubring.kbx":    "keyring\n",
		"sub/.pi/session.jsonl":     "transcript with spawn env\n",
		"sub/.agent/state.json":     "{}\n",
		"sub/.scratch/notes.md":     "scratch\n",
		"sub/.claude/settings.json": "{}\n",
		"sub/.codex/history.jsonl":  "{}\n",
	}
	for path, body := range secrets {
		writeFile(t, wt, path, body)
	}
	// Allowed lookalikes: the checked-in dotenv variants and ordinary
	// project content that merely shares a name or prefix with a secret.
	allowed := map[string]string{
		".env.example":           "KEY=placeholder\n",
		".env.sample":            "KEY=placeholder\n",
		".env.template":          "KEY=placeholder\n",
		"docs/rotate-secrets.md": "how to rotate credentials\n",
	}
	for path, body := range allowed {
		writeFile(t, wt, path, body)
	}

	// Fixed stamp so the ref is exact.
	r := &Runner{logger: noopLogger(), now: func() time.Time { return time.Date(2026, 10, 17, 13, 1, 15, 0, time.UTC) }}
	qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-SECRETS")}
	r.checkpointFailedSession(qw, res)
	if len(res.PostSessionWarnings) != 0 {
		t.Fatalf("warnings=%v; want none", res.PostSessionWarnings)
	}
	if res.WIPRef == nil {
		t.Fatal("no WIP ref pushed")
	}
	wantRef := "refs/wip/" + qw.SessionID + "/20261017T130115Z"
	if res.WIPRef.Ref != wantRef {
		t.Fatalf("WIPRef.Ref = %q, want %q", res.WIPRef.Ref, wantRef)
	}
	tree := gitRun(t, bare, "ls-tree", "-r", "--name-only", wantRef)
	got := map[string]bool{}
	for _, line := range strings.Split(tree, "\n") {
		if strings.TrimSpace(line) != "" {
			got[line] = true
		}
	}
	if !got["feature.txt"] {
		t.Fatalf("WIP tree %q misses the real work; tree:\n%s", wantRef, tree)
	}
	for path := range secrets {
		if got[path] {
			t.Errorf("WIP tree %q contains secret-bearing %q", wantRef, path)
		}
	}
	for path := range allowed {
		if !got[path] {
			t.Errorf("WIP tree %q misses allowed %q", wantRef, path)
		}
	}
}

// TestCheckpointFailedSession_NeverForcesTheWIPRef pins the never-force
// contract: with the ref pre-created on the origin at an unrelated commit,
// the push is refused, the remote ref is unchanged, and a warning is
// recorded. Adding --force to the push turns it RED.
func TestCheckpointFailedSession_NeverForcesTheWIPRef(t *testing.T) {
	bare := makeBareRepo(t)
	wt := filepath.Join(t.TempDir(), "checkout")
	gitRun(t, filepath.Dir(wt), "clone", "--quiet", bare, wt)
	res := &Result{}
	res.Status = "failed"
	res.FailureMode = FailureBudgetExceeded
	res.WorktreePath = wt
	res.rescueTargets = []rescueTarget{{path: wt, base: gitRun(t, wt, "rev-parse", "HEAD")}}

	// Real work so there is something to push.
	writeFile(t, wt, "feature.txt", "real work\n")

	// Pre-create the exact per-attempt ref on the origin at an unrelated
	// commit: a second failure of the same session in the same second.
	qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-FORCE")}
	stamp := time.Date(2026, 10, 17, 13, 2, 30, 0, time.UTC)
	other := t.TempDir()
	gitInit(t, other)
	writeFile(t, other, "other.txt", "unrelated\n")
	gitRun(t, other, "add", "other.txt")
	gitRun(t, other, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "unrelated")
	unrelated := gitRun(t, other, "rev-parse", "HEAD")
	wantRef := "refs/wip/" + qw.SessionID + "/" + wipRefAttemptStamp(stamp)
	gitRun(t, other, "remote", "add", "origin", bare)
	gitRun(t, other, "push", "--quiet", "origin", unrelated+":"+wantRef)

	r := &Runner{logger: noopLogger(), now: func() time.Time { return stamp }}
	r.checkpointFailedSession(qw, res)
	if res.WIPRef != nil {
		t.Fatalf("WIPRef = %+v; want nil after a refused push", res.WIPRef)
	}
	if len(res.PostSessionWarnings) != 1 {
		t.Fatalf("warnings=%v; want one checkpoint warning", res.PostSessionWarnings)
	}
	if got := gitRun(t, bare, "rev-parse", wantRef); got != unrelated {
		t.Fatalf("remote %s moved to %s; the push must never force it", wantRef, got)
	}
}

// TestWipRefAttemptStamp_SortsLexicographically pins the stamp layout: the
// newest attempt is the last ref in a lexicographic listing.
func TestWipRefAttemptStamp_SortsLexicographically(t *testing.T) {
	earlier := wipRefAttemptStamp(time.Date(2026, 10, 17, 13, 0, 0, 0, time.UTC))
	later := wipRefAttemptStamp(time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC))
	if earlier >= later {
		t.Fatalf("stamps do not sort: %q vs %q", earlier, later)
	}
}

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
