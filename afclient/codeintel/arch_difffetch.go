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
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
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

// ghPRFiles is the subset of `gh pr view` we consume. GitHub caps its files
// connection at 100 entries, so changedFiles is needed to detect that cap.
type ghPRFiles struct {
	Title        string     `json:"title"`
	Body         string     `json:"body"`
	ChangedFiles *int       `json:"changedFiles"`
	Files        []ghPRFile `json:"files"`
}

type ghPRFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Filename  string `json:"filename"`
	Patch     string `json:"patch"`
}

// runGhPRView fetches PR metadata + the changed-file list as JSON. Package-level
// var so tests substitute a fixture. The endpoint is a full PR URL OR an
// "owner/repo#N" / "N" ref understood by `gh pr view`.
var runGhPRView = func(ctx context.Context, ref string) ([]byte, error) {
	return runGh(ctx, "pr", "view", ref, "--json", "title,body,changedFiles,files")
}

// runGhPRFiles fetches the REST file pages. gh --paginate can emit one merged
// JSON array or page-separated arrays; either must match changedFiles exactly.
var runGhPRFiles = func(ctx context.Context, repo string, prNum int) ([]byte, error) {
	parts := strings.Split(strings.TrimPrefix(repo, "github.com/"), "/")
	if len(parts) != 2 || !validGhPathSegment(parts[0]) || !validGhPathSegment(parts[1]) || prNum <= 0 {
		return nil, errors.New("arch diff-fetch: invalid GitHub repository or PR number")
	}
	endpoint := "repos/" + parts[0] + "/" + parts[1] + "/pulls/" + strconv.Itoa(prNum) + "/files?per_page=100"
	return runGh(ctx, "api", "--paginate", endpoint)
}

func validGhPathSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func completePRFiles(ctx context.Context, repo string, prNum int, view ghPRFiles) ([]ghPRFile, bool, error) {
	if view.Files == nil {
		return nil, false, errors.New("arch diff-fetch: PR metadata is missing the changed-file list")
	}
	if view.ChangedFiles == nil || *view.ChangedFiles < 0 || *view.ChangedFiles > 3000 {
		return nil, false, errors.New("arch diff-fetch: missing or unsupported changed-file count")
	}
	want := *view.ChangedFiles
	if len(view.Files) > want {
		return nil, false, errors.New("arch diff-fetch: PR metadata exceeds the changed-file count")
	}
	if len(view.Files) == want {
		files, err := validatePRFiles(view.Files, want)
		return files, false, err
	}

	out, err := runGhPRFiles(ctx, repo, prNum)
	if err != nil {
		return nil, false, fmt.Errorf("arch diff-fetch: fetch paginated PR files: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	files := make([]ghPRFile, 0, want)
	pages := 0
	for {
		var page []ghPRFile
		err := dec.Decode(&page)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("arch diff-fetch: decode paginated PR files: %w", err)
		}
		pages++
		if pages > 30 || len(page) == 0 || len(files)+len(page) > want {
			return nil, false, errors.New("arch diff-fetch: invalid paginated PR file response")
		}
		files = append(files, page...)
	}
	files, err = validatePRFiles(files, want)
	if err != nil {
		return nil, false, err
	}
	if err := matchPRViewFiles(view.Files, files); err != nil {
		return nil, false, err
	}
	return files, true, nil
}

// The GraphQL connection may expose only its first 100 files. Those entries
// must still describe the same paths and change counts as the complete REST
// pages. This detects metadata disagreement, not a change of commit with
// identical file metadata; the Action fences head/base before and after.
func matchPRViewFiles(viewFiles, restFiles []ghPRFile) error {
	byPath := make(map[string]ghPRFile, len(restFiles))
	for _, file := range restFiles {
		byPath[file.Path] = file
	}
	seen := make(map[string]bool, len(viewFiles))
	for _, file := range viewFiles {
		name := file.Path
		if name == "" {
			name = file.Filename
		}
		match, ok := byPath[name]
		if name == "" || seen[name] || !ok || match.Additions != file.Additions || match.Deletions != file.Deletions {
			return errors.New("arch diff-fetch: PR file identity changed during pagination")
		}
		seen[name] = true
	}
	return nil
}

func validatePRFiles(files []ghPRFile, want int) ([]ghPRFile, error) {
	if len(files) != want {
		return nil, fmt.Errorf("arch diff-fetch: incomplete changed-file list: got %d, want %d", len(files), want)
	}
	seen := make(map[string]bool, len(files))
	for i := range files {
		if files[i].Path == "" {
			files[i].Path = files[i].Filename // REST names this field filename.
		}
		f := files[i]
		if f.Path == "" || seen[f.Path] || f.Additions < 0 || f.Deletions < 0 {
			return nil, errors.New("arch diff-fetch: invalid or duplicate changed-file metadata")
		}
		seen[f.Path] = true
	}
	return files, nil
}

const maxRESTPatchBytes = 16 << 20

var restHunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?:[ \t].*)?$`)

// completeRESTPatches uses exact GitHub hunk bytes, not a fabricated unified
// diff header. The validated filename remains a separate PrFileDiff.Path, so
// an untrusted path cannot inject a header or change the patch's attribution.
func completeRESTPatches(files []ghPRFile) (map[string]string, error) {
	patches := make(map[string]string, len(files))
	for _, file := range files {
		if !safeRESTPatchPath(file.Path) || (file.Filename != "" && file.Filename != file.Path) {
			return nil, errors.New("arch diff-fetch: unsafe or changed REST patch path")
		}
		if err := validateRESTPatch(file); err != nil {
			return nil, err
		}
		patches[file.Path] = file.Patch
	}
	return patches, nil
}

func safeRESTPatchPath(name string) bool {
	if name == "" || !utf8.ValidString(name) || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, `\`) {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func parseRESTHunkNumber(value string, omittedCount bool) (int, error) {
	if omittedCount && value == "" {
		return 1, nil
	}
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, errors.New("arch diff-fetch: malformed REST patch hunk number")
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, errors.New("arch diff-fetch: overflowing REST patch hunk number")
	}
	return n, nil
}

func validateRESTPatch(file ghPRFile) error {
	if file.Patch == "" {
		return errors.New("arch diff-fetch: missing or binary REST patch")
	}
	if len(file.Patch) > maxRESTPatchBytes || !utf8.ValidString(file.Patch) {
		return errors.New("arch diff-fetch: oversized or invalid UTF-8 REST patch")
	}
	for _, r := range file.Patch {
		if r == utf8.RuneError {
			return errors.New("arch diff-fetch: oversized or invalid UTF-8 REST patch")
		}
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			return errors.New("arch diff-fetch: REST patch contains control text")
		}
	}
	lines := strings.Split(file.Patch, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	added, deleted, hunks := 0, 0, 0
	lastOldEnd, lastNewEnd := -1, -1
	for i := 0; i < len(lines); {
		groups := restHunkHeader.FindStringSubmatch(lines[i])
		if groups == nil {
			return errors.New("arch diff-fetch: malformed REST patch hunk header")
		}
		oldStart, err := parseRESTHunkNumber(groups[1], false)
		if err != nil {
			return err
		}
		oldCount, err := parseRESTHunkNumber(groups[2], true)
		if err != nil {
			return err
		}
		newStart, err := parseRESTHunkNumber(groups[3], false)
		if err != nil {
			return err
		}
		newCount, err := parseRESTHunkNumber(groups[4], true)
		if err != nil {
			return err
		}
		if (oldStart == 0 && oldCount != 0) || (newStart == 0 && newCount != 0) {
			return errors.New("arch diff-fetch: invalid zero-start REST patch hunk")
		}
		maxInt := int(^uint(0) >> 1)
		if oldCount > len(lines) || newCount > len(lines) || oldStart > maxInt-oldCount || newStart > maxInt-newCount ||
			(lastOldEnd >= 0 && (oldStart < lastOldEnd || newStart < lastNewEnd)) {
			return errors.New("arch diff-fetch: invalid or overflowing REST patch hunk span")
		}
		lastOldEnd, lastNewEnd = oldStart+oldCount, newStart+newCount
		hunks++
		i++
		oldUsed, newUsed, changed := 0, 0, false
		lastContent := false
		for i < len(lines) && !strings.HasPrefix(lines[i], "@@ ") {
			line := lines[i]
			switch {
			case line == `\ No newline at end of file` && lastContent:
				lastContent = false
			case strings.HasPrefix(line, " "):
				oldUsed++
				newUsed++
				lastContent = true
			case strings.HasPrefix(line, "+"):
				newUsed++
				added++
				changed, lastContent = true, true
			case strings.HasPrefix(line, "-"):
				oldUsed++
				deleted++
				changed, lastContent = true, true
			default:
				return errors.New("arch diff-fetch: malformed REST patch hunk line")
			}
			if oldUsed > oldCount || newUsed > newCount || added > file.Additions || deleted > file.Deletions {
				return errors.New("arch diff-fetch: REST patch hunk exceeds declared counts")
			}
			i++
		}
		if !changed || oldUsed != oldCount || newUsed != newCount {
			return errors.New("arch diff-fetch: incomplete REST patch hunk")
		}
	}
	if hunks == 0 || added != file.Additions || deleted != file.Deletions {
		return errors.New("arch diff-fetch: REST patch line counts disagree with metadata")
	}
	return nil
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

	cmd := exec.CommandContext(ctx, "gh", args...) //nolint:gosec // G204: args are controlled flags + a caller-validated PR ref.
	cmd.Env = runtimeenv.ComposeChildEnv(os.Environ())
	out, err := cmd.Output()
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
	return fetchPRDiff(ctx, repo, prNum, ref, false)
}

// fetchPRDiff retains the public fetcher's metadata fallback unless the caller
// requires patches. Strict assessments must not turn a partial fetch into a
// successful check of code that was never read.
func fetchPRDiff(ctx context.Context, repo string, prNum int, ref string, requirePatches bool) (PrDiff, error) {
	viewOut, err := runGhPRView(ctx, ref)
	if err != nil {
		return PrDiff{}, err
	}

	var view ghPRFiles
	if err := json.Unmarshal(viewOut, &view); err != nil {
		return PrDiff{}, fmt.Errorf("arch diff-fetch: decode gh pr view: %w", err)
	}
	files, completeRESTPages, err := completePRFiles(ctx, repo, prNum, view)
	if err != nil {
		return PrDiff{}, err
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
	} else if requirePatches {
		// GitHub refuses a full diff over its 20,000-line API limit. Strict
		// mode may use the per-file patches already obtained from complete
		// REST pagination, but only after validating every hunk. A small PR
		// whose GraphQL file list was complete has no REST patch provenance
		// and still fails closed on this path.
		if !completeRESTPages {
			return PrDiff{}, fmt.Errorf("arch diff-fetch: fetch patches: %w", derr)
		}
		patchesByPath, err = completeRESTPatches(files)
		if err != nil {
			return PrDiff{}, fmt.Errorf("arch diff-fetch: complete paginated patches: %w", err)
		}
	}
	if requirePatches && len(patchesByPath) != len(files) {
		return PrDiff{}, errors.New("arch diff-fetch: patch sections do not match the changed-file list")
	}

	for _, f := range files {
		if requirePatches && patchesByPath[f.Path] == "" {
			return PrDiff{}, fmt.Errorf("arch diff-fetch: missing patch section for %q", f.Path)
		}
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
