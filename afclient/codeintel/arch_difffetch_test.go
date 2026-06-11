package codeintel

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

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

func TestFetchPRDiff_DiffFailureIsNonFatal(t *testing.T) {
	origView, origDiff := runGhPRView, runGhPRDiff
	t.Cleanup(func() { runGhPRView, runGhPRDiff = origView, origDiff })

	runGhPRView = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`{"title":"t","body":"b","files":[{"path":"a/b.go","additions":1,"deletions":0}]}`), nil
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
				return []byte(`{"title":"t","body":"b","files":[]}`), nil
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
