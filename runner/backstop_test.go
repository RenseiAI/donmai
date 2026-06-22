package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
// decision against the data tables. The rows mirror the legacy TS
// shouldExcludeFromBackstop test cases verbatim.
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

		// Top-level only — `.agent/state.json` excluded; a file
		// literally named `.agent` is not.
		{".agent/state.json", true},
		{".agent", false},
		{"app/.agent", false},

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
	}, "feature/x", res)

	// Push will fail (no remote), but the commit should already have
	// happened. Check the staged-then-committed file is `src/main.go`.
	logOut, _ := runGit(context.Background(), repo, gitIdentity{}, "log", "--name-only", "--pretty=format:")
	if !strings.Contains(logOut, "src/main.go") {
		t.Errorf("expected src/main.go in commit log; got %q", logOut)
	}
	if strings.Contains(logOut, "node_modules/index.js") {
		t.Errorf("node_modules/index.js should have been excluded; got %q", logOut)
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

// TestRunBackstop_CommitUsesSessionIdentity confirms that a backstop
// commit carries the agent-session author/committer identity derived
// from QueuedWork (via buildSessionEnv), not the stale inherited env.
// Regression test for the NO-OP: previously runGit set cmd.Env =
// os.Environ() with no identity appended, so the commit used whatever
// git config / inherited env happened to be present.
func TestRunBackstop_CommitUsesSessionIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// Poison the inherited process env with a stale identity so a
	// regression would produce a wrong author on the commit.
	t.Setenv("GIT_AUTHOR_NAME", "Stale Process Env")
	t.Setenv("GIT_AUTHOR_EMAIL", "stale@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Stale Process Env")
	t.Setenv("GIT_COMMITTER_EMAIL", "stale@example.com")

	repo := t.TempDir()
	gitInit(t, repo)
	writeFile(t, repo, "fix.go", "package fix\n")
	checkout(t, repo, "feature/session-identity")

	qw := QueuedWork{QueuedWork: queuedWorkBase("ENG-42")}
	r := minimalRunner(t)
	res := &Result{}
	res.WorktreePath = repo

	// runBackstop will stage + commit fix.go then fail on push (no remote).
	r.runBackstop(context.Background(), qw, "feature/session-identity", res)

	// Inspect the commit author the backstop wrote.
	authorOut, err := runGit(context.Background(), repo, gitIdentity{}, "log", "-1", "--pretty=format:%an <%ae>")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	// buildSessionEnv produces "Donmai Agent (ENG-42)" + "agent+test-session-ENG-42@donmai.dev"
	wantName := "Donmai Agent (ENG-42)"
	wantEmail := "agent+test-session-ENG-42@donmai.dev"
	if !strings.Contains(authorOut, wantName) {
		t.Errorf("commit author = %q; want name %q", authorOut, wantName)
	}
	if !strings.Contains(authorOut, wantEmail) {
		t.Errorf("commit author = %q; want email %q", authorOut, wantEmail)
	}
	if strings.Contains(authorOut, "Stale Process Env") {
		t.Errorf("commit author = %q; stale inherited identity leaked", authorOut)
	}
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

	report := r.runBackstop(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("REN-T-2")}, "main", res)

	if !strings.Contains(report.Diagnostics, "main/master") {
		t.Fatalf("expected main/master refusal in diagnostics; got %q", report.Diagnostics)
	}
	if report.PRCreated {
		t.Fatalf("expected no PR created on main")
	}
}
