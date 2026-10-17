package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

func TestRun_FailedSessionWIPRef(t *testing.T) {
	for _, scenario := range []string{"dirty", "committed", "clean", "published", "completed", "review", "provider", "reject", "hang", "unsafe"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "hang" {
				old := providerErrorCheckpointTimeout
				providerErrorCheckpointTimeout = time.Second
				t.Cleanup(func() { providerErrorCheckpointTimeout = old })
			}

			base, err := stub.New()
			if err != nil {
				t.Fatal(err)
			}
			platform := newRecordingPlatformServer(t)
			bare := makeBareRepo(t)
			heads := gitRun(t, bare, "for-each-ref", "--format=%(refname)", "refs/heads")
			work := queuedWorkBase("checkpoint")
			work.Repository = bare
			if scenario == "review" {
				work.WorkType = WorkTypeQAStr
			}
			if scenario == "unsafe" {
				work.SessionID = "bad..id"
			}
			ref := "refs/wip/" + work.SessionID
			work.StageBudget = &prompt.StageBudget{MaxTokens: 1}
			turn := verdictScriptTurn{events: []agent.Event{agent.LlmCallEvent{InputTokens: 2, UsageSource: agent.LlmUsageProvider}}}
			if scenario == "completed" {
				work.StageBudget = nil
				turn.events = nil
				turn.manifest = `{"schemaVersion":1,"verdict":"passed","summary":"reviewed"}`
			}
			if scenario == "provider" {
				work.StageBudget = nil
				turn.events = nil
				turn.providerError = "400 The request contains invalid parameters"
			}
			var commitSHA string
			turn.during = func(t *testing.T, cwd string) {
				if scenario == "clean" {
					return
				}
				writeFile(t, cwd, "committed.txt", "local commit\n")
				gitRun(t, cwd, "add", "committed.txt")
				gitRun(t, cwd, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "local work")
				commitSHA = gitRun(t, cwd, "rev-parse", "HEAD")
				if scenario == "committed" {
					return
				}
				if scenario == "published" {
					gitRun(t, cwd, "push", "origin", "HEAD:refs/heads/published")
					return
				}
				writeFile(t, cwd, "README.md", "modified\n")
				writeFile(t, cwd, "new.txt", "untracked\n")
				if scenario == "dirty" {
					hooks := t.TempDir()
					gitRun(t, cwd, "config", "core.hooksPath", hooks)
					//nolint:gosec // Executable test hook in a temporary repository.
					if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "hang" {
					gitRun(t, cwd, "remote", "set-url", "origin", "ssh://checkpoint.invalid/repo.git")
					gitRun(t, cwd, "config", "core.sshCommand", "sleep 10; :")
					gitRun(t, cwd, "config", "ssh.variant", "ssh")
				}
			}
			if scenario == "reject" {
				//nolint:gosec // Executable test hook in a temporary bare repository.
				if err := os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			provider := &verdictScriptProvider{HarnessProvider: base.(agent.HarnessProvider), t: t, turns: []verdictScriptTurn{turn}}
			r := newFollowUpRunner(t, platform.URL, platform.Client(), provider, nil)
			r.logger = noopLogger()
			r.skipSteering = scenario != "provider"
			r.skipBackstop = true
			r.preserveAlways = false
			r.rescueDir = t.TempDir()
			qw := QueuedWork{QueuedWork: work, WorkerID: "worker", AuthToken: "tok", PlatformURL: platform.URL, ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub}}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			start := time.Now()
			res, runErr := r.Run(ctx, qw)
			if time.Since(start) > 10*time.Second {
				t.Fatal("Run exceeded checkpoint bound plus slack")
			}
			body := platform.terminalStatus(t)
			if scenario == "completed" {
				if res.Status != "completed" {
					t.Fatalf("status=%s error=%s", res.Status, res.Error)
				}
			} else if res.Status != "failed" {
				t.Fatalf("status=%s", res.Status)
			}
			if scenario == "provider" {
				if res.ResumeCheckpoint == nil || !res.Resumable {
					t.Fatalf("provider checkpoint missing: %+v", res)
				}
			}
			if scenario != "dirty" && scenario != "committed" {
				if res.WIPRef != nil || body["wipRef"] != nil || gitRun(t, bare, "for-each-ref", "--format=%(refname)", "refs/wip") != "" {
					t.Fatal("unexpected reserved WIP ref")
				}
				if scenario == "reject" || scenario == "hang" || scenario == "unsafe" {
					var warnings []string
					if err := json.Unmarshal(body["postSessionWarnings"], &warnings); err != nil || len(warnings) == 0 || !slices.Equal(warnings, res.PostSessionWarnings) {
						t.Fatal("missing checkpoint warning")
					}
					wantError := "budget exceeded: max-tokens (max-tokens exceeded: observed=2 limit=1)"
					if res.FailureMode != FailureBudgetExceeded || res.Error != wantError || runErr == nil || runErr.Error() != wantError {
						t.Fatalf("original failure changed: %s %s", res.FailureMode, res.Error)
					}
					onlyRescuePatch(t, r.rescueDir)
				}
				return
			}
			sha := gitRun(t, bare, "rev-parse", ref)
			if scenario == "committed" && sha != commitSHA {
				t.Fatal("committed-only checkpoint changed HEAD")
			}
			var posted agent.WIPRef
			if err := json.Unmarshal(body["wipRef"], &posted); err != nil {
				t.Fatal(err)
			}
			if posted.Ref != ref || posted.CommitSHA != sha || res.WIPRef == nil || *res.WIPRef != posted {
				t.Fatalf("checkpoint=%+v remote=%s", posted, sha)
			}
			if got := gitRun(t, bare, "for-each-ref", "--format=%(refname)", "refs/heads"); got != heads {
				t.Fatalf("new branch created: %s", got)
			}
			restored := filepath.Join(t.TempDir(), "restored")
			gitRun(t, filepath.Dir(restored), "clone", "-q", bare, restored)
			gitRun(t, restored, "fetch", "-q", "origin", ref)
			gitRun(t, restored, "checkout", "-q", "FETCH_HEAD")
			gitRun(t, restored, "merge-base", "--is-ancestor", commitSHA, "HEAD")
			files := map[string]string{"committed.txt": "local commit\n"}
			if scenario == "dirty" {
				files["README.md"] = "modified\n"
				files["new.txt"] = "untracked\n"
			}
			for path, want := range files {
				if got := readFile(t, filepath.Join(restored, path)); got != want {
					t.Fatalf("%s=%q", path, got)
				}
			}
			onlyRescuePatch(t, r.rescueDir)
			if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
				t.Fatalf("worktree not torn down: %v", err)
			}
		})
	}
}
