package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestPullRequestFactFromGitHubView(t *testing.T) {
	t.Parallel()
	valid := ghPullRequestView{Number: 77, URL: "https://github.com/RenseiAI/donmai/pull/77", BaseRefName: "main", HeadRefName: "agent/test-fact"}
	got := pullRequestFactFromGitHubView(valid)
	want := &agent.PullRequestFact{Provider: "github", Number: 77, Repository: "RenseiAI/donmai", URL: "https://github.com/RenseiAI/donmai/pull/77", BaseBranch: "main", HeadBranch: "agent/test-fact"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pullRequestFactFromGitHubView() = %+v, want %+v", got, want)
	}

	invalid := valid
	invalid.HeadRefName = ""
	if got := pullRequestFactFromGitHubView(invalid); got != nil {
		t.Fatalf("pullRequestFactFromGitHubView(incomplete) = %+v, want nil", got)
	}
}

func TestPullRequestFactFromGitHubViewRejectsSpoofedURLs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		view ghPullRequestView
	}{
		{
			name: "host spoof",
			view: ghPullRequestView{Number: 77, URL: "https://github.com.evil.example/RenseiAI/donmai/pull/77", BaseRefName: "main", HeadRefName: "agent/test-fact"},
		},
		{
			name: "userinfo spoof",
			view: ghPullRequestView{Number: 77, URL: "https://token@github.com/RenseiAI/donmai/pull/77", BaseRefName: "main", HeadRefName: "agent/test-fact"},
		},
		{
			name: "port spoof",
			view: ghPullRequestView{Number: 77, URL: "https://github.com:444/RenseiAI/donmai/pull/77", BaseRefName: "main", HeadRefName: "agent/test-fact"},
		},
		{
			name: "path spoof",
			view: ghPullRequestView{Number: 77, URL: "https://github.com/RenseiAI/donmai/issues/77", BaseRefName: "main", HeadRefName: "agent/test-fact"},
		},
		{
			name: "number spoof",
			view: ghPullRequestView{Number: 77, URL: "https://github.com/RenseiAI/donmai/pull/78", BaseRefName: "main", HeadRefName: "agent/test-fact"},
		},
		{
			name: "query spoof",
			view: ghPullRequestView{Number: 77, URL: "https://github.com/RenseiAI/donmai/pull/77?tab=files", BaseRefName: "main", HeadRefName: "agent/test-fact"},
		},
		{
			name: "fragment spoof",
			view: ghPullRequestView{Number: 77, URL: "https://github.com/RenseiAI/donmai/pull/77#discussion_r1", BaseRefName: "main", HeadRefName: "agent/test-fact"},
		},
		{
			name: "http scheme",
			view: ghPullRequestView{Number: 77, URL: "http://github.com/RenseiAI/donmai/pull/77", BaseRefName: "main", HeadRefName: "agent/test-fact"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pullRequestFactFromGitHubView(tc.view); got != nil {
				t.Fatalf("pullRequestFactFromGitHubView(%+v) = %+v, want nil", tc.view, got)
			}
		})
	}
}

func TestCanonicalGitHubOriginRepository(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		origin string
		want   string
	}{
		{
			name:   "accepts canonical https origin",
			origin: "https://github.com/RenseiAI/donmai.git",
			want:   "renseiai/donmai",
		},
		{
			name:   "accepts canonical ssh origin",
			origin: "git@github.com:RenseiAI/donmai.git",
			want:   "renseiai/donmai",
		},
		{
			name:   "rejects github lookalike host",
			origin: "https://github.com.evil.example/RenseiAI/donmai.git",
			want:   "",
		},
		{
			name:   "rejects https credentials spoofing",
			origin: "https://token:x-oauth-basic@github.com/RenseiAI/donmai.git",
			want:   "",
		},
		{
			name:   "rejects local path origin",
			origin: "../example/src/donmai",
			want:   "",
		},
		{
			name:   "rejects non github ssh host",
			origin: "git@evil.example:RenseiAI/donmai.git",
			want:   "",
		},
		{
			name:   "rejects ssh url form outside canonical allowlist",
			origin: "ssh://git@github.com/RenseiAI/donmai.git",
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workarea.CanonicalGitHubRepositorySource(tc.origin); got != tc.want {
				t.Fatalf("CanonicalGitHubRepositorySource(%q) = %q, want %q", tc.origin, got, tc.want)
			}
		})
	}
}

func TestLookupGitHubPullRequestUsesSupportedGhFields(t *testing.T) {
	worktree := t.TempDir()
	gitInit(t, worktree)
	const prURL = "https://github.com/RenseiAI/donmai/pull/463"
	stubGhOnPathExpectingPullRequestView(t, prURL, 0, `{"number":463,"url":"https://github.com/RenseiAI/donmai/pull/463","baseRefName":"main","headRefName":"worktree-pr-fact-github-origin-final-clean"}`)
	fact := lookupGitHubPullRequest(context.Background(), worktree, prURL, map[string]struct{}{
		"renseiai/donmai": {},
	})
	if fact == nil {
		t.Fatal("lookupGitHubPullRequest() = nil, want fact")
	}
	if fact.Repository != "RenseiAI/donmai" || fact.Number != 463 {
		t.Fatalf("lookupGitHubPullRequest() = %+v, want RenseiAI/donmai#463", fact)
	}

	stubGhOnPathExpectingPullRequestView(t, prURL, 0, `{"number":463,"url":"https://github.com/RenseiAI/docs/pull/463","baseRefName":"main","headRefName":"worktree-pr-fact-github-origin-final-clean"}`)
	if fact := lookupGitHubPullRequest(context.Background(), worktree, prURL, map[string]struct{}{
		"renseiai/donmai": {},
	}); fact != nil {
		t.Fatalf("lookupGitHubPullRequest() = %+v, want nil for unauthorized repository", fact)
	}
}

// TestCaptureHeadSHA table-tests the correlation-key capture for the
// orchestration-owned durable CI wait (ADR-2026-06-10-durable-ci-wait.md).
// A worktree with a commit yields its full hex object name; a non-repo
// directory and an unborn-HEAD repo error instead of leaking a git
// diagnostic onto the wire field.
func TestCaptureHeadSHA(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	committed := t.TempDir()
	gitInit(t, committed)
	wantSHA, err := runGit(context.Background(), committed, gitIdentity{}, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("fixture rev-parse: %v", err)
	}
	wantSHA = strings.TrimSpace(wantSHA)

	unborn := t.TempDir()
	//nolint:gosec // G204: test fixture, args are hard-coded literals.
	initCmd := exec.Command("git", "init", "-b", "main")
	initCmd.Dir = unborn
	if out, initErr := initCmd.CombinedOutput(); initErr != nil {
		t.Fatalf("git init: %v\n%s", initErr, out)
	}

	cases := []struct {
		name    string
		dir     string
		want    string
		wantErr bool
	}{
		{name: "repo with commit", dir: committed, want: wantSHA},
		{name: "not a git repo", dir: t.TempDir(), wantErr: true},
		{name: "unborn HEAD (no commits)", dir: unborn, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, gotErr := captureHeadSHA(context.Background(), tc.dir)
			if tc.wantErr {
				if gotErr == nil {
					t.Fatalf("captureHeadSHA(%q) = %q, want error", tc.dir, got)
				}
				if got != "" {
					t.Errorf("captureHeadSHA error path returned non-empty sha %q", got)
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("captureHeadSHA(%q): %v", tc.dir, gotErr)
			}
			if got != tc.want {
				t.Errorf("captureHeadSHA = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestShouldExcludeFromBackstop_Table table-tests the path-exclude
// decision against the data tables. The rows cover the legacy TS cases plus
// Donmai's Go-native code-intel cache.
func TestShouldExcludeFromBackstop_Table(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Anywhere-depth directory matches.
		{"node_modules/foo.js", true},
		{"packages/app/node_modules/index.js", true},
		{".next/build/manifest.json", true},
		{"dist/server.js", true},
		{".cache/eslint/cache.json", true},
		{".turbo/cache/x", true},
		{"__pycache__/m.cpython-311.pyc", true},
		{".venv/lib/python3.11/site-packages/foo.py", true},
		{"go-build/01/abc", true},
		{".gocache/x", true},
		{"gocache/x", true},
		{".golangci-lint-cache/x", true},

		// Top-level only — a state dir's contents are excluded; a file
		// literally named like one, or a nested directory of that name,
		// is not.
		{".agent/state.json", true},
		{".agent", false},
		{"app/.agent", false},
		{".pi/session.jsonl", true},
		{".pi/agent-home/config.json", true},
		{".pi", false},
		{"app/.pi", false},
		{".claude/settings.local.json", true},
		{".codex/history.jsonl", true},
		// A directory that merely shares a prefix with a state dir is
		// ordinary project content.
		{".pi-cache/blob", false},
		{".agentic/plan.md", false},
		// `.donmai` is tracked repo configuration, not harness state —
		// only its generated index is excluded (path-prefix table below).
		{".donmai/config.yaml", false},

		// Extensions.
		{"server.log", true},
		{"hello.tmp", true},
		{"compiled.pyc", true},
		{"src/server.go", false},
		{"README.md", false},

		// Basename prefixes.
		{"__debug_bin1234567", true},
		{"__debug_binmoo", true},
		{"src/__debug_bin", true},
		{"src/foo.go", false},

		// Path-prefix.
		{"target/debug/main", true},
		{"target/release/main", true},
		{"target/debug", true},
		{"target/test/x", false},
		{".donmai/code-index/index.json", true},
		{".donmai/code-index", true},
		{".donmai/config.yaml", false},

		// Empty / safe.
		{"", false},
		{"src/main.go", false},
	}

	for _, tc := range cases {
		got := shouldExcludeFromBackstop(tc.path)
		if got != tc.want {
			t.Errorf("shouldExcludeFromBackstop(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

// TestShouldBackstop_FailureModes confirms the runner skips the
// deterministic backstop for failure modes that imply we no longer
// own the worktree (lost-ownership, timeout) or for unrecoverable
// programmer errors (provider-resolve). All cases use a result-
// sensitive work type (development) so the contract gate is open and
// only the failure-mode rule decides — see TestShouldBackstop_ContractGate
// for the work-type gate.
func TestShouldBackstop_FailureModes(t *testing.T) {
	cases := []struct {
		name string
		res  *Result
		want bool
	}{
		{"nil", nil, false},
		{"already has PR", &Result{Result: agentResultWithPR("https://example.test/pr/1")}, false},
		{"lost ownership", &Result{Result: agentResultWithFailure(FailureLostOwnership)}, false},
		{"timeout", &Result{Result: agentResultWithFailure(FailureTimeout)}, false},
		{"provider resolve", &Result{Result: agentResultWithFailure(FailureProviderResolve)}, false},
		{"no PR, completed", &Result{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldBackstop(tc.res, WorkTypeDevelopmentStr); got != tc.want {
				t.Fatalf("shouldBackstop = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestShouldBackstop_ContractGate asserts the work-type gate: a
// non-result-sensitive work type (backlog-groomer, research,
// refinement) is NEVER backstopped — even on a completed, PR-less
// result that would otherwise trigger the deterministic git backstop.
// This is the root-cause fix for the empty marker PR. Result-sensitive
// types (development, qa, acceptance) are unchanged.
func TestShouldBackstop_ContractGate(t *testing.T) {
	// A result that WOULD backstop under a result-sensitive type:
	// completed, no PR, no skip-worthy failure mode.
	completedNoPR := &Result{}

	cases := []struct {
		workType string
		want     bool
	}{
		// Non-result-sensitive → never backstopped (empty-commit fix).
		{WorkTypeBacklogGroomer, false},
		{WorkTypeResearch, false},
		{WorkTypeRefinement, false},
		{WorkTypeBacklogCreation, false},
		{"imaginary-future-type", false},
		// Interactive work items ("interactive" is the platform's work type for
		// a PTY-hosted session) are not result-sensitive, so the whole backstop
		// block — including the ref-bearing "skip gh pr create" arm — is
		// unreachable for them. A ref on an interactive item only pins the
		// checkout; it never changes what the runner does at teardown.
		{"interactive", false},
		// Result-sensitive → behaviour preserved (still backstops).
		{WorkTypeDevelopmentStr, true},
		{WorkTypeInflight, true},
		{WorkTypeQAStr, true},
		{WorkTypeAcceptance, true},
	}
	for _, tc := range cases {
		t.Run(tc.workType, func(t *testing.T) {
			if got := shouldBackstop(completedNoPR, tc.workType); got != tc.want {
				t.Fatalf("shouldBackstop(workType=%q) = %v; want %v", tc.workType, got, tc.want)
			}
		})
	}
}

// TestRunBackstop_AbortsOnDirtyWorktreeWithBuildArtifacts confirms the
// path-exclude list filters node_modules out of an auto-commit. The
// test creates a fresh git repo with a node_modules dir + a real source
// file, runs the backstop, and asserts the only committed file is the
// source file.
//
// Skipped when git is not on PATH so CI on a barebones runner doesn't
// fail.
func TestRunBackstop_FiltersBuildArtifacts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	gitInit(t, repo)
	// Put a file in node_modules — it should be unstaged by the
	// path-exclude filter.
	if err := os.MkdirAll(filepath.Join(repo, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "node_modules/index.js", "// build artifact")
	writeFile(t, repo, ".donmai/code-index/index.json", `{"generated":true}`)
	writeFile(t, repo, "src/main.go", "package main\nfunc main(){}\n")

	r := minimalRunner(t)
	res := &Result{}
	res.WorktreePath = repo

	// Force the runner onto a feature branch so the push step's
	// "refused to push from main/master" guard doesn't short-circuit
	// before we test the staging logic. We use a non-existent remote
	// so the push fails harmlessly — diagnostics will record that, but
	// the unstage-and-commit logic runs first.
	checkout(t, repo, "feature/x")

	report := r.runBackstop(context.Background(), QueuedWork{
		QueuedWork: queuedWorkBase("REN-T-1"),
	}, "feature/x", res, nil)

	// Push will fail (no remote), but the commit should already have
	// happened. Check the staged-then-committed file is `src/main.go`.
	logOut, _ := runGit(context.Background(), repo, gitIdentity{}, "log", "--name-only", "--pretty=format:")
	if !strings.Contains(logOut, "src/main.go") {
		t.Errorf("expected src/main.go in commit log; got %q", logOut)
	}
	if strings.Contains(logOut, "node_modules/index.js") {
		t.Errorf("node_modules/index.js should have been excluded; got %q", logOut)
	}
	if strings.Contains(logOut, ".donmai/code-index/index.json") {
		t.Errorf("generated code-intel index should have been excluded; got %q", logOut)
	}
	// Push diagnostics expected (no real remote).
	if report.PRCreated {
		t.Errorf("expected no PR created (no remote); got PRCreated=true")
	}
}

// TestGitIdentityEnvOverrides asserts that gitIdentity.envOverrides
// produces the four GIT_AUTHOR_*/GIT_COMMITTER_* entries and that the
// values WIN over an earlier conflicting entry in a merged slice —
// the mechanism runGit relies on to override inherited process env.
//
// This is the regression test for the NO-OP bug where runGit set
// cmd.Env = os.Environ() (no override appended), which is equivalent
// to leaving cmd.Env nil: Go already inherits the process env for both,
// so the agent-session identity from buildSessionEnv never reached
// backstop git-commit subprocesses.
func TestGitIdentityEnvOverrides(t *testing.T) {
	cases := []struct {
		name          string
		id            gitIdentity
		wantOverrides []string
		wantEmpty     bool
	}{
		{
			name:      "zero identity produces no overrides",
			id:        gitIdentity{},
			wantEmpty: true,
		},
		{
			name: "populated identity produces all four vars",
			id:   gitIdentity{Name: "Donmai Agent (ENG-1)", Email: "agent+sess-abc@donmai.dev"},
			wantOverrides: []string{
				"GIT_AUTHOR_NAME=Donmai Agent (ENG-1)",
				"GIT_AUTHOR_EMAIL=agent+sess-abc@donmai.dev",
				"GIT_COMMITTER_NAME=Donmai Agent (ENG-1)",
				"GIT_COMMITTER_EMAIL=agent+sess-abc@donmai.dev",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.id.envOverrides()
			if tc.wantEmpty {
				if len(got) != 0 {
					t.Fatalf("envOverrides() = %v; want empty", got)
				}
				return
			}
			if len(got) != len(tc.wantOverrides) {
				t.Fatalf("envOverrides() len = %d; want %d\ngot: %v", len(got), len(tc.wantOverrides), got)
			}
			for i, want := range tc.wantOverrides {
				if got[i] != want {
					t.Errorf("envOverrides()[%d] = %q; want %q", i, got[i], want)
				}
			}
		})
	}
}

// TestRunGitEnvContainsIdentityOverrides verifies that runGit builds a
// cmd.Env containing the GIT_AUTHOR_*/GIT_COMMITTER_* override vars and
// that a value supplied via gitIdentity WINS over an earlier stale entry
// in the inherited slice (later entries override earlier in execve env).
//
// Strategy: set a stale GIT_AUTHOR_NAME in the test process env, invoke
// runGit with a gitIdentity carrying the correct session name, and use
// `git var GIT_AUTHOR_IDENT` to read back which name git actually used.
// The output from `git var GIT_AUTHOR_IDENT` is "<name> <<email>> <date>"
// so we assert our name appears and the stale value does not.
func TestRunGitEnvContainsIdentityOverrides(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// Set a stale name in the process env that should be overridden.
	const staleName = "Stale Inherited Identity"
	const wantName = "Donmai Agent (ENG-42)"
	const wantEmail = "agent+test-session-42@donmai.dev"

	t.Setenv("GIT_AUTHOR_NAME", staleName)
	t.Setenv("GIT_AUTHOR_EMAIL", "stale@example.com")
	t.Setenv("GIT_COMMITTER_NAME", staleName)
	t.Setenv("GIT_COMMITTER_EMAIL", "stale@example.com")

	// Need a real git repo dir for `git var` to work (git requires a CWD
	// it can inspect; t.TempDir() is sufficient — no commits needed).
	repo := t.TempDir()
	//nolint:gosec // G204: test fixture, args are hard-coded literals.
	initCmd := exec.Command("git", "init", "-b", "main")
	initCmd.Dir = repo
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	id := gitIdentity{Name: wantName, Email: wantEmail}
	out, err := runGit(context.Background(), repo, id, "var", "GIT_AUTHOR_IDENT")
	if err != nil {
		t.Fatalf("runGit var GIT_AUTHOR_IDENT: %v (output: %q)", err, out)
	}

	if !strings.Contains(out, wantName) {
		t.Errorf("git var GIT_AUTHOR_IDENT = %q; want it to contain %q", out, wantName)
	}
	if strings.Contains(out, staleName) {
		t.Errorf("git var GIT_AUTHOR_IDENT = %q; stale value %q should have been overridden", out, staleName)
	}
}

// unsetGitIdentityEnv clears the GIT_AUTHOR_*/GIT_COMMITTER_* process env for
// the duration of the test so buildSessionEnv exercises its session-derived
// fallback. t.Setenv cannot unset a var, so we save + restore manually.
func unsetGitIdentityEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		if orig, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, orig) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Unsetenv(k)
	}
}

// TestRunBackstop_CommitIdentity confirms the backstop commit's author/committer
// is single-sourced through buildSessionEnv (rank 15f): a provisioner-supplied
// GIT_AUTHOR_*/GIT_COMMITTER_* identity is HONORED so backstop commits match the
// agent's own in-box commits; absent one, the session-derived "Donmai Agent
// (<issue>)" default is used. runGit appends the chosen identity AFTER
// os.Environ() so it wins for the commit subprocess (regression test for the
// earlier NO-OP where cmd.Env = os.Environ() appended no identity at all).
func TestRunBackstop_CommitIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	commitAuthor := func(t *testing.T) string {
		t.Helper()
		repo := t.TempDir()
		gitInit(t, repo)
		writeFile(t, repo, "fix.go", "package fix\n")
		checkout(t, repo, "feature/session-identity")

		qw := QueuedWork{QueuedWork: queuedWorkBase("ENG-42")}
		r := minimalRunner(t)
		res := &Result{}
		res.WorktreePath = repo

		// runBackstop stages + commits fix.go then fails on push (no remote).
		r.runBackstop(context.Background(), qw, "feature/session-identity", res, nil)

		authorOut, err := runGit(context.Background(), repo, gitIdentity{}, "log", "-1", "--pretty=format:%an <%ae>")
		if err != nil {
			t.Fatalf("git log: %v", err)
		}
		return authorOut
	}

	t.Run("honors provisioner-supplied identity", func(t *testing.T) {
		// The runtime provisioner stamps this into the environment.
		t.Setenv("GIT_AUTHOR_NAME", "Provisioned Agent")
		t.Setenv("GIT_AUTHOR_EMAIL", "agent@example.com")
		t.Setenv("GIT_COMMITTER_NAME", "Provisioned Agent")
		t.Setenv("GIT_COMMITTER_EMAIL", "agent@example.com")

		authorOut := commitAuthor(t)
		if !strings.Contains(authorOut, "Provisioned Agent") || !strings.Contains(authorOut, "agent@example.com") {
			t.Errorf("commit author = %q; want provisioner identity \"Provisioned Agent <agent@example.com>\"", authorOut)
		}
		if strings.Contains(authorOut, "Donmai Agent") {
			t.Errorf("commit author = %q; provisioner identity should win over the session default", authorOut)
		}
	})

	t.Run("falls back to session default when unset", func(t *testing.T) {
		unsetGitIdentityEnv(t)

		authorOut := commitAuthor(t)
		// buildSessionEnv produces "Donmai Agent (ENG-42)" + "agent+test-session-ENG-42@donmai.dev".
		wantName := "Donmai Agent (ENG-42)"
		wantEmail := "agent+test-session-ENG-42@donmai.dev"
		if !strings.Contains(authorOut, wantName) {
			t.Errorf("commit author = %q; want name %q", authorOut, wantName)
		}
		if !strings.Contains(authorOut, wantEmail) {
			t.Errorf("commit author = %q; want email %q", authorOut, wantEmail)
		}
	})
}

// TestRunBackstop_RefusesMain ensures the backstop refuses to push
// from main/master.
func TestRunBackstop_RefusesMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	gitInit(t, repo)
	writeFile(t, repo, "src/main.go", "package main\nfunc main(){}\n")

	r := minimalRunner(t)
	res := &Result{}
	res.WorktreePath = repo

	report := r.runBackstop(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("REN-T-2")}, "main", res, nil)

	if !strings.Contains(report.Diagnostics, "main/master") {
		t.Fatalf("expected main/master refusal in diagnostics; got %q", report.Diagnostics)
	}
	if report.PRCreated {
		t.Fatalf("expected no PR created on main")
	}
}

// TestRunBackstop_RecoversExistingPR is the regression guard for the
// backstop already-exists recovery (finding donmai[5]): when
// `gh pr create` fails because a PR already exists for the branch, the
// backstop recovers the existing PR URL from gh's failure output and
// reports success (non-empty PRURL, PRCreated=true, no diagnostics)
// rather than a pushed-but-no-PR failure — which the loop's 11c-b block
// would otherwise reclassify as a failed session.
//
// The test pushes to a local bare remote so the push step succeeds, then
// shadows `gh` on PATH with a stub that emulates the "already exists"
// failure (exit 1 with the existing PR URL in its output, which runGh
// folds into its combined output via CombinedOutput).
func TestRunBackstop_RecoversExistingPR(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// Bare remote the backstop's `git push -u origin <branch>` targets.
	remote := t.TempDir()
	//nolint:gosec // G204: test fixture, args are hard-coded literals.
	bareCmd := exec.Command("git", "init", "--bare", "-b", "main", remote)
	if out, err := bareCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	repo := t.TempDir()
	gitInit(t, repo)
	//nolint:gosec // G204: test fixture, args are hard-coded literals.
	remoteAdd := exec.Command("git", "remote", "add", "origin", remote)
	remoteAdd.Dir = repo
	if out, err := remoteAdd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}
	checkout(t, repo, "feature/already-exists")
	// Uncommitted change so the backstop has something to commit + push.
	writeFile(t, repo, "src/fix.go", "package fix\n")

	const wantURL = "https://github.com/RenseiAI/donmai/pull/4242"
	stubGhOnPath(t, 1, "a pull request for branch \"feature/already-exists\" into branch \"main\" already exists:\n"+wantURL+"\n")

	r := minimalRunner(t)
	res := &Result{}
	res.WorktreePath = repo

	report := r.runBackstop(context.Background(), QueuedWork{
		QueuedWork: queuedWorkBase("ENG-77"),
	}, "feature/already-exists", res, nil)

	if report.Diagnostics != "" {
		t.Fatalf("expected no diagnostics on already-exists recovery; got %q", report.Diagnostics)
	}
	if !report.Pushed {
		t.Errorf("expected Pushed=true (push to the bare remote should succeed)")
	}
	if !report.PRCreated {
		t.Errorf("expected PRCreated=true after recovering an existing PR")
	}
	if report.PRURL != wantURL {
		t.Errorf("report.PRURL = %q; want %q", report.PRURL, wantURL)
	}
}

// stubGhOnPath writes a fake `gh` executable that echoes output to stderr
// and exits with exitCode, then prepends its directory to PATH for the
// test's duration so runGh resolves the stub instead of a real gh. The
// output is written to a sibling file the stub cats, so arbitrary
// multi-line text survives without shell-escaping hazards.
func stubGhOnPath(t *testing.T, exitCode int, output string) {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "gh-output.txt")
	if err := os.WriteFile(outFile, []byte(output), 0o600); err != nil {
		t.Fatalf("write gh stub output: %v", err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncat %q 1>&2\nexit %d\n", outFile, exitCode)
	ghPath := filepath.Join(dir, "gh")
	//nolint:gosec // G306: a stub executable must carry the exec bit.
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func stubGhOnPathExpectingPullRequestView(t *testing.T, expectedURL string, exitCode int, output string) {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "gh-output.txt")
	if err := os.WriteFile(outFile, []byte(output), 0o600); err != nil {
		t.Fatalf("write gh stub output: %v", err)
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$#" -ne 5 ] || [ "$1" != "pr" ] || [ "$2" != "view" ] || [ "$3" != %[1]q ] || [ "$4" != "--json" ] || [ "$5" != "number,url,baseRefName,headRefName" ]; then
  echo "unexpected gh argv: $*" 1>&2
  exit 1
fi
cat %[2]q 1>&2
exit %[3]d
`, expectedURL, outFile, exitCode)
	ghPath := filepath.Join(dir, "gh")
	//nolint:gosec // G306: a stub executable must carry the exec bit.
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write gh validating stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
