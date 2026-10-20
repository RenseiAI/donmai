package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

func TestRun_FailedSessionWIPRef(t *testing.T) {
	for _, scenario := range []string{"dirty", "role", "committed", "clean", "published", "completed", "review", "provider", "reject", "hang", "unsafe"} {
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
			if scenario == "role" {
				work.WorkType, work.WorkTypeRole = "custom-builder", "inflight"
				scenario = "dirty"
			}
			if scenario == "review" {
				work.WorkType = WorkTypeQAStr
			}
			if scenario == "unsafe" {
				work.SessionID = "bad..id"
			}
			wipPrefix := "refs/wip/" + work.SessionID + "/"
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
					for _, path := range []string{".env", ".env.local", ".npmrc", "credentials.json", "sub/.pi/session.jsonl"} {
						writeFile(t, cwd, path, "checkpoint must exclude this\n")
					}
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
			var posted agent.WIPRef
			if err := json.Unmarshal(body["wipRef"], &posted); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(posted.Ref, wipPrefix) || strings.Contains(strings.TrimPrefix(posted.Ref, wipPrefix), "/") {
				t.Fatalf("checkpoint ref %q is not one per-attempt leaf under %q", posted.Ref, wipPrefix)
			}
			sha := gitRun(t, bare, "rev-parse", posted.Ref)
			if scenario == "dirty" {
				tree := strings.Split(gitRun(t, bare, "ls-tree", "-r", "--name-only", posted.Ref), "\n")
				for _, path := range []string{".env", ".env.local", ".npmrc", "credentials.json", "sub/.pi/session.jsonl"} {
					if slices.Contains(tree, path) {
						t.Errorf("secret-bearing %q reached checkpoint", path)
					}
				}
			}
			if scenario == "committed" && sha != commitSHA {
				t.Fatal("committed-only checkpoint changed HEAD")
			}
			if posted.CommitSHA != sha || res.WIPRef == nil || *res.WIPRef != posted {
				t.Fatalf("checkpoint=%+v remote=%s", posted, sha)
			}
			if got := gitRun(t, bare, "for-each-ref", "--format=%(refname)", "refs/heads"); got != heads {
				t.Fatalf("new branch created: %s", got)
			}
			restored := filepath.Join(t.TempDir(), "restored")
			gitRun(t, filepath.Dir(restored), "clone", "-q", bare, restored)
			gitRun(t, restored, "fetch", "-q", "origin", posted.Ref)
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

// TestCheckpointFailedSession_ExcludesSecretsFromWIPRef drives
// checkpointFailedSession directly: deny-listed files plus a nested
// harness-state dir sit next to real work with no info/exclude in
// place, and none of them may reach the pushed reserved ref while the
// real work does. The restore-half of workarea_rescue_test.go covers
// the archive path; this covers the pushed ref.
func TestCheckpointFailedSession_ExcludesSecretsFromWIPRef(t *testing.T) {
	bare, wt, res := checkpointCheckout(t)
	qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-SECRETS")}
	res.rescueTargets = []rescueTarget{{path: wt, base: gitRun(t, wt, "rev-parse", "HEAD")}}
	res.FailureMode = FailureBudgetExceeded
	res.Error = "budget exceeded: max-tokens"
	// A session env value the content gate must treat as a secret.
	t.Setenv("WIP_CHECKPOINT_TEST_TOKEN", "sentinel-test-value-42")
	secrets := map[string]string{
		".env":                                 "SECRET=top\n",
		".ENV":                                 "SECRET=top\n",
		".env.local":                           "SECRET=top\n",
		".Env.local":                           "SECRET=top\n",
		".envrc":                               "export SECRET=top\n",
		".npmrc":                               "//registry/:_authToken=top\n",
		".NPMRC":                               "//registry/:_authToken=top\n",
		".netrc":                               "machine example password top\n",
		".NETRC":                               "machine example password top\n",
		".pypirc":                              "password = top\n",
		".git-credentials":                     "https://user:top@example.test\n",
		"tls/server.pem":                       "PRIVATE\n",
		"deploy.key":                           "PRIVATE\n",
		"bundle.p12":                           "PRIVATE\n",
		"id_rsa":                               "PRIVATE\n",
		"home/.ssh/id_ed25519":                 "PRIVATE\n",
		"credentials.json":                     `{"secret":"top"}`,
		"credentials":                          "aws_secret_access_key = faketop\n",
		"application_default_credentials.json": `{"client_secret":"faketop"}`,
		"mycredentials.json":                   `{"client_secret":"faketop"}`,
		".docker/config.json":                  `{"auths":{"example.test":{"auth":"ZmFrZTp0b3A="}}}`,
		".kube/config":                         "token: faketop\n",
		".yarnrc.yml":                          "npmAuthToken: faketop\n",
		".pgpass":                              "localhost:5432:db:user:faketop\n",
		"prod.tfvars":                          "db_password = \"faketop\"\n",
		"terraform.tfstate":                    `{"password":"faketop"}`,
		"svc/.env/production":                  "SECRET=faketop\n",
		".env-prod":                            "SECRET=faketop\n",
		".env~":                                "SECRET=faketop\n",
		".npmrc.bak":                           "//registry/:_authToken=faketop\n",
		"id_ecdsa":                             "PRIVATE\n",
		// Innocent names with secret-bearing content: the content gate,
		// not the name list, keeps these out of the pushed ref.
		"config/secrets.yml":               "api_token: ghp_faketoken0123456789ab\n",
		"notes.md":                         "rotate sk-ant-faketoken-0123456789 soon\n",
		"env-dump.txt":                     "dump: sentinel-test-value-42\n",
		"config/credentials-prod.json":     `{"secret":"top"}`,
		"config/service-account-prod.json": `{"secret":"top"}`,
		".aws/credentials":                 "[default]\naws_secret_access_key = top\n",
		".AWS/credentials":                 "[default]\naws_secret_access_key = top\n",
		".ssh/config":                      "IdentityFile top\n",
		".SSH/config":                      "IdentityFile top\n",
		".gnupg/secring.gpg":               "PRIVATE\n",
		"sub/.pi/session.jsonl":            `{"spawnEnv":{"SECRET":"top"}}`,
		"sub/.PI/session.jsonl":            `{"spawnEnv":{"SECRET":"top"}}`,
		"sub/.pi/agent-home/config.json":   "{}",
		"sub/.agent/events.jsonl":          "{}",
		"sub/.scratch/notes.md":            "scratch\n",
		".agent/events.jsonl":              "{}",
		"node_modules/index.js":            "// build artifact\n",
	}
	for path, body := range secrets {
		writeFile(t, wt, path, body)
	}
	writeFile(t, wt, "feature.txt", "real work\n")
	writeFile(t, wt, ".env.example", "PLACEHOLDER=\n")
	writeFile(t, wt, "docs/credentials.md", "how to rotate\n")
	(&Runner{logger: noopLogger()}).checkpointFailedSession(qw, res)
	if res.WIPRef == nil {
		t.Fatalf("no WIP checkpoint: warnings %v", res.PostSessionWarnings)
	}
	if !strings.HasPrefix(res.WIPRef.Ref, "refs/wip/"+qw.SessionID+"/") {
		t.Fatalf("WIP ref %q is not a per-attempt leaf", res.WIPRef.Ref)
	}
	tree := gitRun(t, bare, "ls-tree", "-r", "--name-only", res.WIPRef.Ref)
	got := map[string]bool{}
	for _, line := range strings.Split(tree, "\n") {
		if strings.TrimSpace(line) != "" {
			got[line] = true
		}
	}
	for path := range secrets {
		if got[path] {
			t.Errorf("secret-bearing %q reached %s", path, res.WIPRef.Ref)
		}
	}
	for _, path := range []string{"feature.txt", ".env.example", "docs/credentials.md"} {
		if !got[path] {
			t.Errorf("%q missing from %s (tree:\n%s)", path, res.WIPRef.Ref, tree)
		}
	}
	// Content-dropped files warn with the path, never the value.
	var sawContentWarning bool
	for _, w := range res.PostSessionWarnings {
		if strings.Contains(w, `"notes.md"`) {
			sawContentWarning = true
		}
		if strings.Contains(w, "sk-ant-faketoken-0123456789") || strings.Contains(w, "sentinel-test-value-42") {
			t.Errorf("checkpoint warning carries a secret value: %q", w)
		}
	}
	if !sawContentWarning {
		t.Errorf("no checkpoint warning names the content-dropped notes.md (warnings %v)", res.PostSessionWarnings)
	}
}

// TestCheckpointFailedSession_SkipsPushWhenUnpushedHistoryHoldsSecrets pins
// the committed-history gate: a secret the session committed (rather than
// left dirty) must not leave the host through the reserved ref. The remote
// checkpoint is skipped with a warning naming the path, while the local
// checkout keeps everything for the local rescue patch.
func TestCheckpointFailedSession_SkipsPushWhenUnpushedHistoryHoldsSecrets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "name-denied",
			files: map[string]string{
				"feature.txt":     "real work\n",
				".env.production": "SECRET=faketop\n",
			},
			want: ".env.production",
		},
		{
			name: "secret-content",
			files: map[string]string{
				"feature.txt": "real work\n",
				"notes.md":    "rotate ghp_faketoken0123456789ab soon\n",
			},
			want: "notes.md",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bare, wt, res := checkpointCheckout(t)
			qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-HISTORY-" + tc.name)}
			res.FailureMode = FailureBudgetExceeded
			res.Error = "budget exceeded: max-tokens"
			for path, body := range tc.files {
				writeFile(t, wt, path, body)
			}
			gitRun(t, wt, "add", "-A")
			gitRun(t, wt, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "agent commit")
			(&Runner{logger: noopLogger()}).checkpointFailedSession(qw, res)
			if res.WIPRef != nil {
				t.Fatalf("WIPRef = %+v; want nil when unpushed history holds secrets", res.WIPRef)
			}
			var sawPath bool
			for _, w := range res.PostSessionWarnings {
				if strings.Contains(w, tc.want) {
					sawPath = true
				}
			}
			if !sawPath {
				t.Errorf("no checkpoint warning names %q (warnings %v)", tc.want, res.PostSessionWarnings)
			}
			if got := gitRun(t, bare, "for-each-ref", "--format=%(refname)", "refs/wip"); got != "" {
				t.Errorf("reserved ref pushed despite denied history: %s", got)
			}
			for path, want := range tc.files {
				if got := readFile(t, filepath.Join(wt, path)); got != want {
					t.Errorf("%s=%q after the skipped checkpoint, want it untouched", path, got)
				}
			}
		})
	}
}

// TestCheckpointFailedSession_NeverForcesAnExistingRef pre-creates the
// session's reserved ref on origin pointing at an unrelated commit,
// then pins that a repeated failure lands on its own per-attempt ref:
// the pre-existing ref is unchanged, a warning is recorded, and the
// second failure's work is still preserved under its own leaf. Pushing
// with --force would move the pre-existing ref instead; adding it to
// the push must turn this test RED.
func TestCheckpointFailedSession_NeverForcesAnExistingRef(t *testing.T) {
	bare, wt, res := checkpointCheckout(t)
	qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-NOFORCE")}
	res.rescueTargets = []rescueTarget{{path: wt, base: gitRun(t, wt, "rev-parse", "HEAD")}}
	res.FailureMode = FailureBudgetExceeded
	res.Error = "budget exceeded: max-tokens"
	unrelated := gitRun(t, wt, "rev-parse", "HEAD")
	firstRef := "refs/wip/" + qw.SessionID + "/20060102T150405Z-first0000"
	gitRun(t, wt, "update-ref", firstRef, unrelated)
	gitRun(t, wt, "push", "origin", firstRef+":"+firstRef)
	pinned := gitRun(t, bare, "rev-parse", firstRef)
	if pinned != unrelated {
		t.Fatalf("fixture ref = %s, want %s", pinned, unrelated)
	}
	writeFile(t, wt, "retry.txt", "second attempt work\n")
	(&Runner{logger: noopLogger()}).checkpointFailedSession(qw, res)
	if res.WIPRef == nil {
		t.Fatalf("no WIP checkpoint: warnings %v", res.PostSessionWarnings)
	}
	if res.WIPRef.Ref == firstRef {
		t.Fatalf("second failure reused %s instead of its own per-attempt ref", firstRef)
	}
	if got := gitRun(t, bare, "rev-parse", firstRef); got != unrelated {
		t.Fatalf("pre-existing %s moved %s -> %s: the checkpoint must never force", firstRef, unrelated, got)
	}
	tree := gitRun(t, bare, "ls-tree", "-r", "--name-only", res.WIPRef.Ref)
	if !strings.Contains(tree, "retry.txt") {
		t.Fatalf("second-attempt work missing from %s (tree:\n%s)", res.WIPRef.Ref, tree)
	}
}

// TestCheckpointFailedSession_NeverForcesThePushedRef pins never-force
// on the push: origin's copy of the exact per-attempt ref the
// checkpoint will push is pointed at an UNRELATED commit (an empty
// commit on no shared history), so the checkpoint's push is refused as
// a non-fast-forward, the remote ref is unchanged, a warning is
// recorded, and no WIP ref is reported. Adding --force to the push
// turns this test RED (the push then succeeds and moves the ref). The
// unrelated commit must differ from the checkout's HEAD: pointing the
// ref at HEAD itself would make the push a no-op success on any code.
func TestCheckpointFailedSession_NeverForcesThePushedRef(t *testing.T) {
	bare, wt, res := checkpointCheckout(t)
	qw := QueuedWork{QueuedWork: queuedWorkBase("WIP-FORCEPIN")}
	res.rescueTargets = []rescueTarget{{path: wt, base: gitRun(t, wt, "rev-parse", "HEAD")}}
	res.FailureMode = FailureBudgetExceeded
	res.Error = "budget exceeded: max-tokens"
	oldNow, oldRand := wipRefAttemptNow, wipRefAttemptRand
	t.Cleanup(func() { wipRefAttemptNow, wipRefAttemptRand = oldNow, oldRand })
	wipRefAttemptNow = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	wipRefAttemptRand = func() string { return "deadbeef" }
	wantRef := wipAttemptRef(qw.SessionID)
	unrelated := gitRun(t, wt, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit-tree", gitRun(t, wt, "rev-parse", "HEAD^{tree}"), "-m", "unrelated")
	gitRun(t, wt, "update-ref", wantRef, unrelated)
	gitRun(t, wt, "push", "origin", wantRef+":"+wantRef)
	if got := gitRun(t, bare, "rev-parse", wantRef); got != unrelated {
		t.Fatalf("fixture ref = %s, want %s", got, unrelated)
	}
	writeFile(t, wt, "work.txt", "work\n")
	(&Runner{logger: noopLogger()}).checkpointFailedSession(qw, res)
	if res.WIPRef != nil {
		t.Fatalf("WIPRef = %+v; want nil after a refused push", res.WIPRef)
	}
	if len(res.PostSessionWarnings) == 0 {
		t.Fatal("no checkpoint warning recorded for the refused push")
	}
	if got := gitRun(t, bare, "rev-parse", wantRef); got != unrelated {
		t.Fatalf("%s moved %s -> %s: the checkpoint must never force", wantRef, unrelated, got)
	}
}

// TestWipAttemptRef_PerSessionUniqueness pins the ref layout: two
// attempts of one session land on distinct per-attempt leaves, so a
// re-entered generation or a racing stale worker can never collide or
// fast-forward the other's ref.
func TestWipAttemptRef_PerSessionUniqueness(t *testing.T) {
	t.Parallel()
	oldNow, oldRand := wipRefAttemptNow, wipRefAttemptRand
	t.Cleanup(func() { wipRefAttemptNow, wipRefAttemptRand = oldNow, oldRand })
	base := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	randValues := []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"}
	i := 0
	wipRefAttemptNow = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
	wipRefAttemptRand = func() string {
		v := randValues[i%len(randValues)]
		i++
		return v
	}
	first := wipAttemptRef("sess-1")
	second := wipAttemptRef("sess-1")
	if first == second {
		t.Fatalf("two attempts produced the same ref %q", first)
	}
	for _, ref := range []string{first, second} {
		if !strings.HasPrefix(ref, "refs/wip/sess-1/") || strings.Contains(strings.TrimPrefix(ref, "refs/wip/sess-1/"), "/") {
			t.Errorf("ref %q is not one leaf under refs/wip/sess-1/", ref)
		}
	}
}
