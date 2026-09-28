package codeintel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGhPRCommandsSanitizeRunnerOnlyEnvironment(t *testing.T) {
	binDir := t.TempDir()
	fakeGh := filepath.Join(binDir, "gh")
	const script = `#!/bin/sh
set -eu
if [ "${ATTACH_TOKEN+x}" = x ] || [ "${ATTACH_TOKEN_FILE+x}" = x ] || [ "${ATTACH_URL+x}" = x ]; then
	printf 'runner-only environment leaked into gh %s %s\n' "$1" "$2" >&2
	exit 97
fi
if [ "${SAFE_CHILD_ENV:-}" != "kept" ]; then
	printf 'safe inherited environment missing from gh %s %s\n' "$1" "$2" >&2
	exit 98
fi
case "$1 $2" in
	"pr view") printf '%s\n' '{"title":"fake view","body":"","changedFiles":0,"files":[]}' ;;
	"pr diff") printf '%s\n' 'diff --git a/a.go b/a.go' ;;
	*) printf 'unexpected gh arguments: %s\n' "$*" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(fakeGh, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write fake gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ATTACH_TOKEN", "parent-secret")
	t.Setenv("ATTACH_TOKEN_FILE", "/parent/token")
	t.Setenv("ATTACH_URL", "wss://relay.invalid/v1/rooms/parent")
	t.Setenv("SAFE_CHILD_ENV", "kept")

	tests := []struct {
		name string
		run  func(context.Context, string) ([]byte, error)
		want string
	}{
		{name: "pr view", run: runGhPRView, want: `"title":"fake view"`},
		{name: "pr diff", run: runGhPRDiff, want: "diff --git a/a.go b/a.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.run(context.Background(), "owner/repo#1")
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("%s output = %q, want substring %q", tc.name, out, tc.want)
			}
		})
	}
}

func TestParseDiffGitPath(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"diff --git a/src/auth/login.ts b/src/auth/login.ts", "src/auth/login.ts"},
		{"diff --git a/old/name.go b/new/name.go", "new/name.go"},
		{"diff --git a/x b/x", "x"},
		{"malformed header", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := parseDiffGitPath(tt.header); got != tt.want {
			t.Errorf("parseDiffGitPath(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestSplitUnifiedDiff(t *testing.T) {
	diff := `diff --git a/src/auth/login.ts b/src/auth/login.ts
index 111..222 100644
--- a/src/auth/login.ts
+++ b/src/auth/login.ts
@@ -1,2 +1,3 @@
 const x = 1
+const y = 2
diff --git a/src/db/schema.ts b/src/db/schema.ts
index 333..444 100644
--- a/src/db/schema.ts
+++ b/src/db/schema.ts
@@ -1 +1,2 @@
+export interface User {}
`
	got := splitUnifiedDiff(diff)
	if len(got) != 2 {
		t.Fatalf("got %d file sections, want 2: %v", len(got), got)
	}
	login := got["src/auth/login.ts"]
	if login == "" {
		t.Fatalf("missing login.ts patch")
	}
	// The patch must include the file header AND the added line.
	for _, want := range []string{"diff --git a/src/auth/login.ts", "+const y = 2"} {
		if !strings.Contains(login, want) {
			t.Errorf("login patch missing %q:\n%s", want, login)
		}
	}
	// The login section must NOT bleed into the schema section.
	if strings.Contains(login, "export interface User") {
		t.Errorf("login section leaked schema content:\n%s", login)
	}
	if schema := got["src/db/schema.ts"]; !strings.Contains(schema, "+export interface User {}") {
		t.Errorf("schema patch missing added line:\n%s", schema)
	}
}

func TestSplitUnifiedDiff_Empty(t *testing.T) {
	if got := splitUnifiedDiff(""); len(got) != 0 {
		t.Errorf("empty diff should yield no sections, got %v", got)
	}
}

func TestFetchPRDiff_CombinesViewAndDiff(t *testing.T) {
	origView, origDiff := runGhPRView, runGhPRDiff
	t.Cleanup(func() { runGhPRView, runGhPRDiff = origView, origDiff })

	runGhPRView = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`{
			"title": "Add Result<T,E> error handling",
			"body": "We chose Result over exceptions for the auth layer.",
			"changedFiles": 2,
			"files": [
				{"path": "src/auth/login.ts", "additions": 10, "deletions": 0},
				{"path": "src/db/schema.ts", "additions": 5, "deletions": 2}
			]
		}`), nil
	}
	runGhPRDiff = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`diff --git a/src/auth/login.ts b/src/auth/login.ts
@@ -1 +1,2 @@
+const r: Result<User, Error> = ok(user)
diff --git a/src/db/schema.ts b/src/db/schema.ts
@@ -1 +1,2 @@
+export interface User {}
`), nil
	}

	diff, err := FetchPRDiff(context.Background(), "github.com/org/repo", 123, "https://github.com/org/repo/pull/123")
	if err != nil {
		t.Fatalf("FetchPRDiff: %v", err)
	}
	if diff.Title != "Add Result<T,E> error handling" {
		t.Errorf("title = %q", diff.Title)
	}
	if diff.Repository != "github.com/org/repo" || diff.PrNumber != 123 {
		t.Errorf("repo/pr = %q/%d", diff.Repository, diff.PrNumber)
	}
	if len(diff.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(diff.Files))
	}
	// login.ts: 0 deletions + additions → Added=true; patch carries the +line.
	login := diff.Files[0]
	if !login.Added {
		t.Errorf("login.ts should be marked Added")
	}
	if !strings.Contains(login.Patch, "Result<User, Error>") {
		t.Errorf("login patch not wired:\n%s", login.Patch)
	}
	// schema.ts: has deletions → not Added.
	if diff.Files[1].Added {
		t.Errorf("schema.ts should not be marked Added")
	}

	// And the fetched diff must yield real observations (proves diff-fetch feeds
	// the reader). A Result<T,E> convention + zone patterns are expected.
	obs := ReadDiffObservations(diff, "project")
	if len(obs) == 0 {
		t.Fatal("expected observations from fetched diff, got none")
	}
}

func TestFetchPRDiff_PaginatesMoreThanOneHundredFiles(t *testing.T) {
	binDir := t.TempDir()
	fakeGh := filepath.Join(binDir, "gh")
	const script = `#!/bin/sh
set -eu
case "$1 $2" in
  "pr view") [ "$3 $4" = "https://github.com/org/repo/pull/7 --json" ] && cat "$PR_VIEW_FIXTURE" ;;
  "api --paginate") [ "$3" = "repos/org/repo/pulls/7/files?per_page=100" ] && cat "$PR_FILES_FIXTURE" ;;
  "pr diff") [ "$3" = "https://github.com/org/repo/pull/7" ] && cat "$PR_DIFF_FIXTURE" ;;
  *) exit 99 ;;
esac
`
	if err := os.WriteFile(fakeGh, []byte(script), 0o755); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	type fixtureFile struct {
		Path      string `json:"path"`
		Filename  string `json:"filename"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	}
	viewFiles := make([]fixtureFile, 0, 100)
	allFiles := make([]fixtureFile, 0, 108)
	var patch strings.Builder
	for i := range 108 {
		path := fmt.Sprintf("src/file-%03d.go", i)
		f := fixtureFile{Path: path, Filename: path, Additions: 1}
		allFiles = append(allFiles, f)
		if i < 100 {
			viewFiles = append(viewFiles, f)
		}
		fmt.Fprintf(&patch, "diff --git a/%s b/%s\n@@ -0,0 +1 @@\n+package example\n", path, path)
	}
	changedFiles := 108
	view, err := json.Marshal(struct {
		Title        string        `json:"title"`
		ChangedFiles int           `json:"changedFiles"`
		Files        []fixtureFile `json:"files"`
	}{Title: "many files", ChangedFiles: changedFiles, Files: viewFiles})
	if err != nil {
		t.Fatal(err)
	}
	viewPath := filepath.Join(binDir, "view.json")
	if err := os.WriteFile(viewPath, view, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PR_VIEW_FIXTURE", viewPath)
	pages := func(includeSecond, aggregate bool) []byte {
		if aggregate {
			merged, _ := json.Marshal(allFiles)
			return merged
		}
		first, _ := json.Marshal(allFiles[:100])
		if !includeSecond {
			return first
		}
		second, _ := json.Marshal(allFiles[100:])
		return append(append(first, '\n'), second...)
	}
	for _, tc := range []struct {
		name          string
		includeSecond bool
		aggregate     bool
		patches       int
		wantErr       string
	}{
		{name: "complete", includeSecond: true, patches: 108},
		{name: "aggregated pages", includeSecond: true, aggregate: true, patches: 108},
		{name: "missing second page", includeSecond: false, patches: 108, wantErr: "incomplete changed-file list"},
		{name: "missing patch", includeSecond: true, patches: 107, wantErr: "patch sections do not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filesPath := filepath.Join(t.TempDir(), "files.json")
			if err := os.WriteFile(filesPath, pages(tc.includeSecond, tc.aggregate), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PR_FILES_FIXTURE", filesPath)
			patchPath := filepath.Join(t.TempDir(), "patch.diff")
			patchLines := strings.SplitAfter(patch.String(), "+package example\n")
			if err := os.WriteFile(patchPath, []byte(strings.Join(patchLines[:tc.patches], "")), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PR_DIFF_FIXTURE", patchPath)
			got, err := fetchPRDiff(context.Background(), "github.com/org/repo", 7, "https://github.com/org/repo/pull/7", true)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("fetch error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Files) != 108 {
				t.Fatalf("file count = %d, want 108", len(got.Files))
			}
			if got.Files[107].Path != "src/file-107.go" || got.Files[107].Patch == "" || !got.Files[107].Added {
				t.Fatalf("last file missing metadata or patch: %+v", got.Files[107])
			}
		})
	}
}

func TestFetchPRDiff_DiffFailureIsNonFatal(t *testing.T) {
	origView, origDiff := runGhPRView, runGhPRDiff
	t.Cleanup(func() { runGhPRView, runGhPRDiff = origView, origDiff })

	runGhPRView = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`{"title":"t","body":"b","changedFiles":1,"files":[{"path":"a/b.go","additions":1,"deletions":0}]}`), nil
	}
	// Diff fetch fails — file list still produces metadata; patches are empty.
	runGhPRDiff = func(_ context.Context, _ string) ([]byte, error) {
		return nil, errors.New("gh pr diff boom")
	}

	diff, err := FetchPRDiff(context.Background(), "github.com/org/repo", 1, "ref")
	if err != nil {
		t.Fatalf("FetchPRDiff should not fail when only the diff call errors: %v", err)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "a/b.go" {
		t.Fatalf("file list not preserved: %+v", diff.Files)
	}
	if diff.Files[0].Patch != "" {
		t.Errorf("patch should be empty on diff failure, got %q", diff.Files[0].Patch)
	}
}

func TestFetchPRDiff_ViewFailurePropagates(t *testing.T) {
	origView := runGhPRView
	t.Cleanup(func() { runGhPRView = origView })

	sentinel := errors.New("gh pr view boom")
	runGhPRView = func(_ context.Context, _ string) ([]byte, error) { return nil, sentinel }

	if _, err := FetchPRDiff(context.Background(), "r", 1, "ref"); err == nil {
		t.Fatal("expected view failure to propagate")
	}
}

func TestFetchDiffOrMeta_PaginatedFileFailureFallsBack(t *testing.T) {
	origView, origFiles, origWarn := runGhPRView, runGhPRFiles, diffFetchWarnWriter
	t.Cleanup(func() { runGhPRView, runGhPRFiles, diffFetchWarnWriter = origView, origFiles, origWarn })
	files := make([]map[string]any, 100)
	for i := range files {
		files[i] = map[string]any{"path": fmt.Sprintf("file-%03d.go", i), "additions": 1, "deletions": 0}
	}
	view, err := json.Marshal(map[string]any{"title": "large PR", "changedFiles": 101, "files": files})
	if err != nil {
		t.Fatal(err)
	}
	runGhPRView = func(_ context.Context, _ string) ([]byte, error) { return view, nil }
	runGhPRFiles = func(_ context.Context, _ string, _ int) ([]byte, error) { return nil, errors.New("page unavailable") }
	var warning bytes.Buffer
	diffFetchWarnWriter = &warning
	got := New(t.TempDir()).fetchDiffOrMeta(context.Background(), "github.com/org/repo", 7, "https://github.com/org/repo/pull/7")
	if len(got.Files) != 0 || got.Repository != "github.com/org/repo" || got.PrNumber != 7 {
		t.Fatalf("fallback = %+v", got)
	}
	if !strings.Contains(warning.String(), "page unavailable") || !strings.Contains(warning.String(), "metadata-only") {
		t.Fatalf("fallback warning = %q", warning.String())
	}
}

// TestFetchDiffOrMeta_SurfacesDegradeReason pins the loud-degrade contract:
// when the diff fetch fails, fetchDiffOrMeta must explain WHY on the warn
// writer before falling back to the metadata-only PrDiff. A silent fallback
// regression would leave `arch assess` emitting zero observations with no
// explanation (the gh-missing case in particular must surface the install
// instructions carried by ErrDiffFetchUnavailable).
func TestFetchDiffOrMeta_SurfacesDegradeReason(t *testing.T) {
	tests := []struct {
		name       string
		fetchErr   error // nil → fetch succeeds
		prURL      string
		repo       string
		prNum      int
		wantInWarn []string // substrings the warning must carry; empty → no warning
	}{
		{
			name:     "gh missing surfaces install instructions",
			fetchErr: ErrDiffFetchUnavailable,
			prURL:    "https://github.com/owner/repo/pull/7",
			repo:     "github.com/owner/repo",
			prNum:    7,
			wantInWarn: []string{
				"gh CLI not found on PATH",
				"https://cli.github.com",
				"metadata-only",
			},
		},
		{
			name:     "fetch error names the ref and reason",
			fetchErr: errors.New("gh pr view: exit 1: HTTP 502"),
			repo:     "github.com/owner/repo",
			prNum:    9,
			wantInWarn: []string{
				"owner/repo#9",
				"HTTP 502",
				"metadata-only",
			},
		},
		{
			name:       "successful fetch is silent",
			repo:       "github.com/owner/repo",
			prNum:      3,
			wantInWarn: nil,
		},
		{
			name:       "no ref resolvable stays silent",
			repo:       "",
			prNum:      0,
			wantInWarn: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origView, origDiff, origWarn := runGhPRView, runGhPRDiff, diffFetchWarnWriter
			t.Cleanup(func() {
				runGhPRView, runGhPRDiff, diffFetchWarnWriter = origView, origDiff, origWarn
			})

			var warnBuf bytes.Buffer
			diffFetchWarnWriter = &warnBuf
			runGhPRView = func(_ context.Context, _ string) ([]byte, error) {
				if tc.fetchErr != nil {
					return nil, tc.fetchErr
				}
				return []byte(`{"title":"t","body":"b","changedFiles":0,"files":[]}`), nil
			}
			runGhPRDiff = func(_ context.Context, _ string) ([]byte, error) {
				return []byte(""), nil
			}

			diff := New(t.TempDir()).fetchDiffOrMeta(context.Background(), tc.repo, tc.prNum, tc.prURL)
			if diff.Repository != tc.repo || diff.PrNumber != tc.prNum {
				t.Errorf("PrDiff identity = (%q, %d), want (%q, %d)",
					diff.Repository, diff.PrNumber, tc.repo, tc.prNum)
			}

			warn := warnBuf.String()
			if len(tc.wantInWarn) == 0 {
				if warn != "" {
					t.Fatalf("expected no warning, got %q", warn)
				}
				return
			}
			for _, want := range tc.wantInWarn {
				if !strings.Contains(warn, want) {
					t.Errorf("warning missing %q:\n%s", want, warn)
				}
			}
		})
	}
}
