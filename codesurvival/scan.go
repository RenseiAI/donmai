package codesurvival

import (
	"context"
	"os/exec"
	"strings"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// gitRunner abstracts `git` invocation so tests can substitute a fixture
// without spawning child processes. repoPath is the cwd; args are passed to git
// verbatim. Mirrors the GitRunner dependency injection in
// platform/src/lib/factory/code-survival.ts.
type gitRunner interface {
	run(ctx context.Context, repoPath string, args ...string) (string, error)
}

// execGitRunner is the production gitRunner: exec.CommandContext("git", …) with
// CombinedOutput, matching runner.runGit's behaviour.
type execGitRunner struct{}

func (execGitRunner) run(ctx context.Context, repoPath string, args ...string) (string, error) {
	//nolint:gosec // G204: name is the hard-coded "git" binary; args are
	// constructed from validated batch-work fields at the call sites below.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoPath
	cmd.Env = runtimeenv.FilterRunnerOnly(cmd.Environ())
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), " \n\t"), err
}

// scanPrSurvivalResult is the accumulated survival counts for one merged PR.
type scanPrSurvivalResult struct {
	linesTotalAtMerge int
	linesSurviving    int
	status            ScanStatus
	filesScanned      []string
	filesMissing      []string
	// survivingByFile maps a (repo-relative) file path to the set of HEAD line
	// numbers still attributed to the merge SHA. RW4 reachability consumes this
	// to map each surviving line onto a symbol; it is empty in the survival-only
	// fast path when no per-line capture is requested. Keyed by the file's path
	// at HEAD (the path `git blame HEAD -- <file>` was run against).
	survivingByFile map[string][]int
	// errorMessage is set when diff-tree itself failed (status will be empty).
	errorMessage string
	// diffTreeFailed is true when the initial diff-tree call errored — the
	// caller maps this to skipped/shallow_history.
	diffTreeFailed bool
}

// scanPrSurvival drives `git diff-tree` + `git blame --line-porcelain` for one
// merged PR and yields the survival counts. It is a port of scanPrSurvival in
// platform/src/lib/factory/code-survival.ts:181-288, with the git runner
// injected.
//
//  1. `git diff-tree --no-commit-id --name-only -r --diff-filter=AM <mergeSha>`
//     lists the files the merge added/modified.
//  2. For each file, `git blame -l --line-porcelain <mergeSha> -- <file>` counts
//     lines authored at merge.
//  3. For the same file at HEAD, count lines still attributed to the merge SHA —
//     that's "surviving". A missing file at HEAD trips status=partial.
func scanPrSurvival(ctx context.Context, runner gitRunner, repoPath, mergeSha string) scanPrSurvivalResult {
	res := scanPrSurvivalResult{survivingByFile: map[string][]int{}}

	stdout, err := runner.run(ctx, repoPath,
		"diff-tree", "--no-commit-id", "--name-only", "-r", "--diff-filter=AM", mergeSha)
	if err != nil {
		res.diffTreeFailed = true
		res.errorMessage = err.Error()
		return res
	}

	var files []string
	for _, s := range strings.Split(stdout, "\n") {
		if t := strings.TrimSpace(s); t != "" {
			files = append(files, t)
		}
	}

	sawAnyFailure := false

	for _, file := range files {
		res.filesScanned = append(res.filesScanned, file)

		// Lines authored at merge: blame the merge SHA itself.
		blameAtMerge, mErr := runner.run(ctx, repoPath,
			"blame", "-l", "--line-porcelain", mergeSha, "--", file)
		if mErr != nil {
			// File may have moved/been deleted, or the merge SHA can't see it.
			// Skip — don't count toward total since we can't bound it.
			sawAnyFailure = true
			continue
		}
		atMergeLines := countLinesByCommit(blameAtMerge, mergeSha)
		if atMergeLines == 0 {
			// No lines attributable to this merge in this file. Skip to keep the
			// rate honest.
			continue
		}
		res.linesTotalAtMerge += atMergeLines

		// Lines surviving at HEAD: blame HEAD; count lines still attributed to
		// the merge SHA. A missing file at HEAD errors → 0 surviving + partial.
		blameAtHead, hErr := runner.run(ctx, repoPath,
			"blame", "-l", "--line-porcelain", "HEAD", "--", file)
		if hErr != nil {
			res.filesMissing = append(res.filesMissing, file)
			sawAnyFailure = true
			continue
		}
		survivingLines := survivingLinesByCommit(blameAtHead, mergeSha)
		res.linesSurviving += len(survivingLines)
		if len(survivingLines) > 0 {
			res.survivingByFile[file] = survivingLines
		}
	}

	switch {
	case len(files) == 0:
		// diff-tree returned nothing — benign for empty PRs.
		res.status = StatusOK
	case sawAnyFailure:
		res.status = StatusPartial
	default:
		res.status = StatusOK
	}
	return res
}
