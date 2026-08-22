package workarea_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestRepositoryLeaf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"https url", "https://github.com/Example/docs-corpus", "docs-corpus"},
		{"https url with .git", "https://github.com/Example/docs-corpus.git", "docs-corpus"},
		{"scp-style url", "git@github.com:Example/notes.git", "notes"},
		{"file url", "file:///tmp/fixtures/repo.git", "repo"},
		{"trailing slash", "https://example.com/org/repo/", "repo"},
		{"owner name slug", "github.com/acme/web", "web"},
		{"empty is empty", "", ""},
		{"whitespace is empty", "   ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workarea.RepositoryLeaf(tt.url); got != tt.want {
				t.Errorf("RepositoryLeaf(%q) = %q; want %q", tt.url, got, tt.want)
			}
		})
	}
}

// TestRepositoryLeafIsCollisionSafe pins that a leaf is never a bare dot dir, a
// reserved session-root name, or a name two different URLs can silently share
// after sanitizing — the session root holds several repositories at once, so a
// collision would have one repository clobber another.
func TestRepositoryLeafIsCollisionSafe(t *testing.T) {
	t.Parallel()
	unsafe := []string{
		"..", ".", "https://example.com/org/..", "https://example.com/org/.agent",
		"https://example.com/org/a b", "https://example.com/org/../../etc",
	}
	seen := make(map[string]string, len(unsafe))
	for _, url := range unsafe {
		leaf := workarea.RepositoryLeaf(url)
		if !workarea.SafeRepositoryLeaf(leaf) {
			t.Errorf("RepositoryLeaf(%q) = %q, which is not a safe leaf", url, leaf)
		}
		if prev, dup := seen[leaf]; dup {
			t.Errorf("RepositoryLeaf collision: %q and %q both yield %q", prev, url, leaf)
		}
		seen[leaf] = url
	}
	// Stable across calls — restart adoption depends on rederiving the same leaf.
	for i := 0; i < 3; i++ {
		if got := workarea.RepositoryLeaf("https://example.com/org/a b"); got != workarea.RepositoryLeaf("https://example.com/org/a b") {
			t.Fatalf("RepositoryLeaf is not deterministic (iteration %d, got %q)", i, got)
		}
	}
}

func TestSafeRepositoryLeaf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{`a\b`, false},
		{".agent", false},
		{".donmai", false},
		{".terminal-leases", false},
		{"docs-corpus", true},
		{"repo.name", true},
	}
	for _, tt := range tests {
		if got := workarea.SafeRepositoryLeaf(tt.name); got != tt.want {
			t.Errorf("SafeRepositoryLeaf(%q) = %v; want %v", tt.name, got, tt.want)
		}
	}
}

// TestNewLayoutSeparatesRootFromRepository is the typing control:
// the session-owned root and the selected repository worktree are distinct
// values, and the repository is a leaf INSIDE the root.
func TestNewLayoutSeparatesRootFromRepository(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	layout, err := workarea.NewLayout(parent, "sess-1", "web")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	wantRoot := filepath.Join(parent, "sess-1")
	if layout.Root.String() != wantRoot {
		t.Errorf("Root = %q; want %q", layout.Root, wantRoot)
	}
	if want := filepath.Join(wantRoot, "web"); layout.Repository.String() != want {
		t.Errorf("Repository = %q; want %q", layout.Repository, want)
	}
	if !layout.IsNested() {
		t.Error("IsNested() = false; want true for a nested layout")
	}
}

func TestNewLayoutRejectsUnsafeLeaves(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	for _, leaf := range []string{"", "..", "a/b", ".agent"} {
		if _, err := workarea.NewLayout(parent, "sess-1", leaf); !errors.Is(err, workarea.ErrUnsafeRepositoryLeaf) {
			t.Errorf("NewLayout(leaf=%q) error = %v; want ErrUnsafeRepositoryLeaf", leaf, err)
		}
	}
	if _, err := workarea.NewLayout(parent, "a/b", "web"); !errors.Is(err, workarea.ErrUnsafeRepositoryLeaf) {
		t.Errorf("NewLayout(sessionID=%q) error = %v; want ErrUnsafeRepositoryLeaf", "a/b", err)
	}
	if _, err := workarea.NewLayout(parent, "  ", "web"); err == nil {
		t.Error("NewLayout with blank session id: want error")
	}
}

// TestFlatLayoutIsRetained pins mixed-version compatibility: the pre-nesting
// shape, where the repository clone IS the session directory, stays expressible
// and reports Root == Repository.
func TestFlatLayoutIsRetained(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	layout, err := workarea.FlatLayout(parent, "sess-1")
	if err != nil {
		t.Fatalf("FlatLayout: %v", err)
	}
	want := filepath.Join(parent, "sess-1")
	if layout.Root.String() != want || layout.Repository.String() != want {
		t.Errorf("FlatLayout = {%q, %q}; want both %q", layout.Root, layout.Repository, want)
	}
	if layout.IsNested() {
		t.Error("IsNested() = true; want false for the flat layout")
	}
}

func TestSiblingPath(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	layout, err := workarea.NewLayout(parent, "sess-1", "web")
	if err != nil {
		t.Fatal(err)
	}
	got, err := layout.SiblingPath("corpus")
	if err != nil {
		t.Fatalf("SiblingPath: %v", err)
	}
	if want := filepath.Join(parent, "sess-1", "corpus"); got != want {
		t.Errorf("SiblingPath = %q; want %q", got, want)
	}
	if _, err := layout.SiblingPath("web"); !errors.Is(err, workarea.ErrUnsafeRepositoryLeaf) {
		t.Errorf("SiblingPath colliding with the selected repository: err = %v; want ErrUnsafeRepositoryLeaf", err)
	}
	if _, err := layout.SiblingPath("../escape"); !errors.Is(err, workarea.ErrUnsafeRepositoryLeaf) {
		t.Errorf("SiblingPath escaping the root: err = %v; want ErrUnsafeRepositoryLeaf", err)
	}
}

// TestSiblingPathsAreSessionOwned is the ownership control: two sessions naming
// the same secondary repository resolve to DIFFERENT paths, each inside its own
// session root, so neither can see, freshen, or delete the other's copy.
func TestSiblingPathsAreSessionOwned(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	a, err := workarea.NewLayout(parent, "session-a", "web")
	if err != nil {
		t.Fatal(err)
	}
	b, err := workarea.NewLayout(parent, "session-b", "web")
	if err != nil {
		t.Fatal(err)
	}
	pathA, err := a.SiblingPath("corpus")
	if err != nil {
		t.Fatal(err)
	}
	pathB, err := b.SiblingPath("corpus")
	if err != nil {
		t.Fatal(err)
	}
	if pathA == pathB {
		t.Fatalf("two sessions share one secondary-repo path %q", pathA)
	}
	for _, p := range []string{pathA, pathB} {
		if filepath.Dir(p) == parent {
			t.Errorf("secondary repo %q is a global peer under the worktree root", p)
		}
	}
}

func TestDiscoverLayoutFindsNestedRepository(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	repo := filepath.Join(parent, "sess-1", "web")
	mkdirAll(t, filepath.Join(repo, ".git"))

	layout, found := workarea.DiscoverLayout(parent, "sess-1", "web")
	if !found {
		t.Fatal("DiscoverLayout: want found")
	}
	if layout.Repository.String() != repo {
		t.Errorf("Repository = %q; want %q", layout.Repository, repo)
	}
	if layout.Root.String() != filepath.Join(parent, "sess-1") {
		t.Errorf("Root = %q; want %q", layout.Root, filepath.Join(parent, "sess-1"))
	}
}

// TestDiscoverLayoutFindsRetainedFlatWorkarea is the legacy-discovery control:
// a workarea provisioned by a pre-nesting binary — the
// repository clone IS <parent>/<sessionID> — stays discoverable and recoverable
// after the upgrade, and is NOT reported as a nested layout.
func TestDiscoverLayoutFindsRetainedFlatWorkarea(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	flat := filepath.Join(parent, "legacy-session")
	mkdirAll(t, filepath.Join(flat, ".git"))

	layout, found := workarea.DiscoverLayout(parent, "legacy-session", "web")
	if !found {
		t.Fatal("DiscoverLayout: retained flat workarea not found")
	}
	if layout.Root.String() != flat || layout.Repository.String() != flat {
		t.Errorf("flat layout = {%q, %q}; want both %q", layout.Root, layout.Repository, flat)
	}
	if layout.IsNested() {
		t.Error("retained flat workarea reported as nested")
	}
}

// TestDiscoverLayoutRecoversSoleRepositoryLeaf covers restart adoption when the
// caller no longer knows which repository the session selected: exactly one git
// repository directly under the session root identifies it unambiguously.
func TestDiscoverLayoutRecoversSoleRepositoryLeaf(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "sess-1")
	mkdirAll(t, filepath.Join(root, "only-repo", ".git"))

	layout, found := workarea.DiscoverLayout(parent, "sess-1", "")
	if !found {
		t.Fatal("DiscoverLayout: sole repository leaf not recovered")
	}
	if want := filepath.Join(root, "only-repo"); layout.Repository.String() != want {
		t.Errorf("Repository = %q; want %q", layout.Repository, want)
	}

	// Two repositories are ambiguous — recovery must not guess.
	mkdirAll(t, filepath.Join(root, "second-repo", ".git"))
	if _, found := workarea.DiscoverLayout(parent, "sess-1", ""); found {
		t.Error("DiscoverLayout guessed a repository from an ambiguous session root")
	}
}

// TestDiscoverLayoutReturnsProspectiveNestedPath pins that a not-yet-provisioned
// session reports the nested path it SHOULD occupy, so a caller can publish the
// path before the worker has cloned anything.
func TestDiscoverLayoutReturnsProspectiveNestedPath(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	layout, found := workarea.DiscoverLayout(parent, "sess-new", "web")
	if found {
		t.Error("DiscoverLayout: want found=false for an unprovisioned session")
	}
	want := filepath.Join(parent, "sess-new", "web")
	if layout.Repository.String() != want {
		t.Errorf("Repository = %q; want prospective nested %q", layout.Repository, want)
	}
	if !strings.HasPrefix(layout.Repository.String(), layout.Root.String()+string(filepath.Separator)) {
		t.Errorf("Repository %q is not inside Root %q", layout.Repository, layout.Root)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
}
