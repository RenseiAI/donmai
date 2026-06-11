package codeintel

// arch_difffetch.go — native PR diff fetch for the arch-intel assess pipeline.
//
// archAssessNative previously fed an EMPTY PrDiff{} to ReadDiffObservations, so
// the native path emitted no observations for a real PR — it only knew the repo
// + PR number. This file closes that gap: it fetches the actual changed files,
// patches, title, and body for a PR via the GitHub CLI (`gh`), so the diff
// reader runs on REAL content.
//
// Transport: the GitHub CLI (`gh`) is the only dependency, matching the existing
// `gh api` usage in afcli/linear.go (check-deployment). `gh` carries the
// operator's GitHub auth, so no token handling lives here. When `gh` is not on
// PATH (or the call fails) the fetch returns an error the caller can surface or
// fall back from — it is NOT fatal to the binary.
//
// All fetch entry points go through package-level function vars so tests inject
// deterministic fixtures without a live network or `gh` install.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrDiffFetchUnavailable is returned when the GitHub CLI (`gh`) is not on PATH.
// Callers treat this as "fall back to metadata-only", not a fatal error.
var ErrDiffFetchUnavailable = errors.New(
	"gh CLI not found on PATH — install GitHub CLI (https://cli.github.com) " +
		"for native PR diff fetch, or set DONMAI_ARCH_BIN for the full pipeline",
)

// diffFetchWarnWriter receives the human-readable degrade warnings emitted when
// the PR diff fetch fails and arch assess falls back to metadata-only.
// Package-level var so tests capture the output without a real stderr.
var diffFetchWarnWriter io.Writer = os.Stderr

// diffFetchTimeout bounds a single gh invocation. A PR with thousands of files
// is rare; 60s is generous headroom over the typical sub-second `gh` call.
const diffFetchTimeout = 60 * time.Second

// ghPRFiles is the subset of `gh pr view --json files,title,body` we consume.
type ghPRFiles struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Files []struct {
		Path      string `json:"path"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	} `json:"files"`
}

// runGhPRView fetches PR metadata + the changed-file list as JSON. Package-level
// var so tests substitute a fixture. The endpoint is a full PR URL OR an
// "owner/repo#N" / "N" ref understood by `gh pr view`.
var runGhPRView = func(ctx context.Context, ref string) ([]byte, error) {
	return runGh(ctx, "pr", "view", ref, "--json", "title,body,files")
}

// runGhPRDiff fetches the unified diff for a PR. Package-level var for tests.
var runGhPRDiff = func(ctx context.Context, ref string) ([]byte, error) {
	return runGh(ctx, "pr", "diff", ref)
}

// runGh executes `gh <args...>` with a bounded context and returns stdout.
// A missing `gh` binary maps to ErrDiffFetchUnavailable so the caller can
// distinguish "no tool" from "tool errored".
func runGh(ctx context.Context, args ...string) ([]byte, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, ErrDiffFetchUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, diffFetchTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "gh", args...).Output() //nolint:gosec // G204: args are controlled flags + a caller-validated PR ref.
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("gh %s: exit %d: %s",
				strings.Join(args, " "), exitErr.ExitCode(), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// FetchPRDiff builds a fully-populated PrDiff for a PR by combining
// `gh pr view` (title, body, file list) with `gh pr diff` (per-file patches).
//
// repo is the "github.com/owner/repo" identifier; prNum is the PR number; ref is
// the gh-understood reference (a full URL when available, else "owner/repo#N").
// The returned PrDiff carries the real changed files + patches so
// ReadDiffObservations produces real observations.
//
// When `gh` is unavailable the error is ErrDiffFetchUnavailable and the caller
// falls back to a metadata-only PrDiff.
func FetchPRDiff(ctx context.Context, repo string, prNum int, ref string) (PrDiff, error) {
	viewOut, err := runGhPRView(ctx, ref)
	if err != nil {
		return PrDiff{}, err
	}

	var view ghPRFiles
	if err := json.Unmarshal(viewOut, &view); err != nil {
		return PrDiff{}, fmt.Errorf("arch diff-fetch: decode gh pr view: %w", err)
	}

	diff := PrDiff{
		Repository: repo,
		PrNumber:   prNum,
		Title:      view.Title,
		Body:       view.Body,
	}

	// Per-file patches come from the unified diff. A diff-fetch failure here is
	// non-fatal: we still emit file-list observations (zone patterns) even
	// without the +/- line content, so degrade rather than error out.
	patchesByPath := map[string]string{}
	if diffOut, derr := runGhPRDiff(ctx, ref); derr == nil {
		patchesByPath = splitUnifiedDiff(string(diffOut))
	}

	for _, f := range view.Files {
		diff.Files = append(diff.Files, PrFileDiff{
			Path:  f.Path,
			Patch: patchesByPath[f.Path],
			// A file with zero deletions and >0 additions is (heuristically) new.
			Added: f.Deletions == 0 && f.Additions > 0,
		})
	}

	return diff, nil
}

// splitUnifiedDiff parses a `gh pr diff` (git unified diff) into a per-file map
// of path → patch body. Each file section starts at a `diff --git a/X b/Y`
// header; the path key is the post-image path ("b/Y" → "Y") which matches the
// `path` field from `gh pr view --json files`. The patch body is the lines from
// the header (inclusive) up to the next file header.
func splitUnifiedDiff(diff string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(diff, "\n")

	var curPath string
	var cur strings.Builder
	flush := func() {
		if curPath != "" {
			out[curPath] = strings.TrimRight(cur.String(), "\n")
		}
		cur.Reset()
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			curPath = parseDiffGitPath(line)
		}
		if curPath != "" {
			cur.WriteString(line)
			cur.WriteByte('\n')
		}
	}
	flush()
	return out
}

// parseDiffGitPath extracts the post-image path from a `diff --git a/X b/Y`
// header line, stripping the "b/" prefix. Returns "" when the header is
// malformed.
func parseDiffGitPath(header string) string {
	fields := strings.Fields(header)
	// fields: ["diff", "--git", "a/X", "b/Y"]
	if len(fields) < 4 {
		return ""
	}
	bPath := fields[len(fields)-1]
	return strings.TrimPrefix(bPath, "b/")
}
