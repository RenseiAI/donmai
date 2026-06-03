package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
// programmer errors (provider-resolve).
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
			if got := shouldBackstop(tc.res); got != tc.want {
				t.Fatalf("shouldBackstop = %v; want %v", got, tc.want)
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
	logOut, _ := runGit(context.Background(), repo, "log", "--name-only", "--pretty=format:")
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

// TestRunBackstop_CleanTreeShortCircuits is the regression test for the
// SUP-1840 blocked-outcome bug: an agent that deliberately produced no
// changes (clean worktree, no commits ahead of base) must NOT trigger a
// futile push / `gh pr create` — that emitted the misleading "No commits
// between main and agent/<sid>" error. The backstop must short-circuit
// with an honest "agent produced no commits" diagnostic and Pushed=false.
//
// We point gh at a non-existent binary path to prove the short-circuit
// happens BEFORE any gh invocation: if the code tried to push/PR the
// test would still pass on diagnostics, so we assert specifically on the
// honest diagnostic string and Pushed=false.
func TestRunBackstop_CleanTreeShortCircuits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	gitInit(t, repo)
	// Feature branch off main, no new commits, clean working tree —
	// exactly the "agent wrote no code" shape.
	checkout(t, repo, "agent/blocked-sid")

	r := minimalRunner(t)
	res := &Result{}
	res.WorktreePath = repo

	report := r.runBackstop(context.Background(), QueuedWork{
		QueuedWork: queuedWorkBase("REN-T-CLEAN"),
	}, "agent/blocked-sid", res)

	if report.Pushed {
		t.Errorf("Pushed = true; want false (nothing to push)")
	}
	if report.PRCreated {
		t.Errorf("PRCreated = true; want false")
	}
	if !strings.Contains(report.Diagnostics, "no commits") {
		t.Errorf("Diagnostics = %q; want it to mention 'no commits'", report.Diagnostics)
	}
	if strings.Contains(report.Diagnostics, "No commits between") {
		t.Errorf("Diagnostics leaked the misleading gh error: %q", report.Diagnostics)
	}
	// And no commit should have been fabricated on the branch.
	logOut, _ := runGit(context.Background(), repo, "rev-list", "--count", "main..HEAD")
	if strings.TrimSpace(logOut) != "0" {
		t.Errorf("expected 0 commits ahead of main; got %q", logOut)
	}
}

// TestBranchHasCommitsAhead exercises the helper directly: a fresh
// feature branch has none, and one with a commit has some.
func TestBranchHasCommitsAhead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	gitInit(t, repo)
	checkout(t, repo, "feature/ahead-test")

	if branchHasCommitsAhead(context.Background(), repo) {
		t.Errorf("fresh feature branch: branchHasCommitsAhead = true; want false")
	}

	// Add a commit; now it's ahead.
	writeFile(t, repo, "new.go", "package main\n")
	for _, args := range [][]string{{"add", "new.go"}, {"commit", "-m", "work"}} {
		if _, err := runGit(context.Background(), repo, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if !branchHasCommitsAhead(context.Background(), repo) {
		t.Errorf("branch with a commit: branchHasCommitsAhead = false; want true")
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
