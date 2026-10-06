package runner

import (
	"context"
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
