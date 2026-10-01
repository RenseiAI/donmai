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
  "pr diff")
    [ "$3" = "https://github.com/org/repo/pull/7" ]
    if [ "${PR_DIFF_HTTP406:-}" = "1" ]; then
      printf '%s\n' 'HTTP 406: diff exceeded 20000 lines' >&2
      exit 1
    fi
    cat "$PR_DIFF_FIXTURE" ;;
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
		Patch     string `json:"patch,omitempty"`
	}
	viewFiles := make([]fixtureFile, 0, 100)
	allFiles := make([]fixtureFile, 0, 130)
	var patch strings.Builder
	for i := range 130 {
		path := fmt.Sprintf("src/file-%03d.go", i)
		f := fixtureFile{Path: path, Filename: path, Additions: 1, Patch: "@@ -0,0 +1 @@\n+const r: Result<User, Error> = ok(user)"}
		allFiles = append(allFiles, f)
		if i < 100 {
			f.Patch = "" // GraphQL view supplies metadata, not REST patch bodies.
			viewFiles = append(viewFiles, f)
		}
		fmt.Fprintf(&patch, "diff --git a/%s b/%s\n@@ -0,0 +1 @@\n+package example\n", path, path)
	}
	changedFiles := 130
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
		http406       bool
		wantErr       string
	}{
		{name: "complete", includeSecond: true, patches: 130},
		{name: "aggregated pages", includeSecond: true, aggregate: true, patches: 130},
		{name: "missing second page", includeSecond: false, patches: 130, wantErr: "incomplete changed-file list"},
		{name: "missing patch", includeSecond: true, patches: 129, wantErr: "patch sections do not match"},
		{name: "complete REST after HTTP 406", includeSecond: true, patches: 130, http406: true},
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
			t.Setenv("PR_DIFF_HTTP406", "")
			if tc.http406 {
				t.Setenv("PR_DIFF_HTTP406", "1")
				t.Setenv("DONMAI_ARCH_BIN", "")
				out, err := New(t.TempDir()).ArchAssess(ArchAssessOptions{
					PrURL: "https://github.com/org/repo/pull/7", RequireDiff: true, GatePolicy: "none",
				})
				if err != nil {
					t.Fatalf("strict native assessment refused complete REST patches: %v", err)
				}
				report, ok := out.(map[string]any)
				if !ok || report["mode"] != "native-diff-only" || report["gated"] != false {
					t.Fatalf("strict native report = %#v", out)
				}
				observations, ok := report["observations"].([]any)
				foundPatchSignal := false
				if ok {
					for _, observation := range observations {
						value, ok := observation.(map[string]any)
						if !ok || value["kind"] != "convention" {
							continue
						}
						payload, ok := value["payload"].(map[string]any)
						if ok && payload["title"] == "Result<T, E> error handling" {
							foundPatchSignal = true
						}
					}
				}
				if !foundPatchSignal {
					t.Fatalf("strict assessment omitted convention found only in REST patch bodies: %#v", observations)
				}
				return
			}
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
			if len(got.Files) != 130 {
				t.Fatalf("file count = %d, want 130", len(got.Files))
			}
			if got.Files[129].Path != "src/file-129.go" || got.Files[129].Patch == "" || !got.Files[129].Added {
				t.Fatalf("last file missing metadata or patch: %+v", got.Files[129])
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

func TestFetchPRDiffRejectsIncompletePaginatedRESTPatches(t *testing.T) {
	origView, origFiles, origDiff := runGhPRView, runGhPRFiles, runGhPRDiff
	t.Cleanup(func() { runGhPRView, runGhPRFiles, runGhPRDiff = origView, origFiles, origDiff })
	view, err := json.Marshal(ghPRFiles{
		ChangedFiles: func() *int { n := 2; return &n }(),
		Files:        []ghPRFile{{Path: "src/a.ts", Additions: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runGhPRView = func(context.Context, string) ([]byte, error) { return view, nil }
	runGhPRDiff = func(context.Context, string) ([]byte, error) { return nil, errors.New("HTTP 406 full diff too large") }
	// Keep transport fixtures independent of new production fields so these
	// final test bytes still compile against the original fetcher for RED.
	type restFixtureFile struct {
		Filename  string `json:"filename"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Patch     string `json:"patch"`
	}
	base := []restFixtureFile{
		{Filename: "src/a.ts", Additions: 1, Patch: "@@ -0,0 +1 @@\n+const a = 1"},
		{Filename: "src/b.ts", Additions: 1, Patch: "@@ -0,0 +1 @@\n+const b = 2"},
	}
	for _, tc := range []struct {
		name    string
		mutate  func([]restFixtureFile) []restFixtureFile
		wantErr string
	}{
		{name: "binary or missing patch", mutate: func(f []restFixtureFile) []restFixtureFile { f[1].Patch = ""; return f }, wantErr: "missing or binary REST patch"},
		{name: "truncated hunk", mutate: func(f []restFixtureFile) []restFixtureFile {
			f[1].Additions = 2
			f[1].Patch = "@@ -0,0 +1,2 @@\n+const b = 2"
			return f
		}, wantErr: "incomplete REST patch hunk"},
		{name: "metadata line count mismatch", mutate: func(f []restFixtureFile) []restFixtureFile { f[1].Additions = 2; return f }, wantErr: "line counts disagree"},
		{name: "malformed hunk", mutate: func(f []restFixtureFile) []restFixtureFile { f[1].Patch = "@@ bad @@\n+const b = 2"; return f }, wantErr: "malformed REST patch hunk header"},
		{name: "zero start with nonzero span", mutate: func(f []restFixtureFile) []restFixtureFile {
			f[1].Deletions = 1
			f[1].Patch = "@@ -0,1 +0,1 @@\n-const b = 1\n+const b = 2"
			return f
		}, wantErr: "invalid zero-start REST patch hunk"},
		{name: "extra unprefixed text", mutate: func(f []restFixtureFile) []restFixtureFile { f[1].Patch += "\nforeign text"; return f }, wantErr: "malformed REST patch hunk line"},
		{name: "overflowing hunk", mutate: func(f []restFixtureFile) []restFixtureFile {
			f[1].Patch = "@@ -999999999999999999999999999,1 +1 @@\n+const b = 2"
			return f
		}, wantErr: "overflowing REST patch hunk number"},
		{name: "control byte", mutate: func(f []restFixtureFile) []restFixtureFile { f[1].Patch += "\x1b"; return f }, wantErr: "control text"},
		{name: "invalid UTF-8 replacement", mutate: func(f []restFixtureFile) []restFixtureFile { f[1].Patch += "\ufffd"; return f }, wantErr: "invalid UTF-8 REST patch"},
		{name: "unsafe relative path", mutate: func(f []restFixtureFile) []restFixtureFile { f[1].Filename = "../escape.ts"; return f }, wantErr: "unsafe or changed REST patch path"},
		{name: "header injection path", mutate: func(f []restFixtureFile) []restFixtureFile {
			f[1].Filename = "src/b.ts\ndiff --git a/evil b/evil"
			return f
		}, wantErr: "unsafe or changed REST patch path"},
		{name: "duplicate identity", mutate: func(f []restFixtureFile) []restFixtureFile { f[1].Filename = f[0].Filename; return f }, wantErr: "invalid or duplicate changed-file metadata"},
		{name: "changed first-page identity", mutate: func(f []restFixtureFile) []restFixtureFile { f[0].Filename = "src/changed.ts"; return f }, wantErr: "file identity changed during pagination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := append([]restFixtureFile(nil), base...)
			files = tc.mutate(files)
			payload, err := json.Marshal(files)
			if err != nil {
				t.Fatal(err)
			}
			runGhPRFiles = func(context.Context, string, int) ([]byte, error) { return payload, nil }
			got, err := fetchPRDiff(context.Background(), "github.com/org/repo", 7, "https://github.com/org/repo/pull/7", true)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || len(got.Files) != 0 {
				t.Fatalf("strict incomplete REST assessment = %+v, error %v; want %q refusal", got, err, tc.wantErr)
			}
		})
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
