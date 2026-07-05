package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveIndexRoot_RootValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "afile.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name    string
		root    string
		wantErr string
	}{
		{"empty root", "", "required"},
		{"relative root", "some/rel/path", "absolute"},
		{"missing root", filepath.Join(dir, "does-not-exist"), "no such file"},
		{"root is a file", file, "not a directory"},
		{"valid root", dir, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveIndexRoot(tc.root, "")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("resolveIndexRoot(%q) unexpected error: %v", tc.root, err)
				}
				if got != filepath.Clean(tc.root) {
					t.Fatalf("resolveIndexRoot(%q) = %q, want %q", tc.root, got, filepath.Clean(tc.root))
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveIndexRoot(%q) = nil error, want error containing %q", tc.root, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("resolveIndexRoot(%q) error = %v, want containing %q", tc.root, err, tc.wantErr)
			}
		})
	}
}

func TestResolveIndexRoot_RepoPathScoping(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sub := filepath.Join(root, "pkg", "svc")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Valid relative subtree.
	got, err := resolveIndexRoot(root, filepath.Join("pkg", "svc"))
	if err != nil {
		t.Fatalf("valid repo-path: %v", err)
	}
	if got != sub {
		t.Fatalf("repo-path root = %q, want %q", got, sub)
	}

	// Absolute repo-path is rejected.
	if _, err := resolveIndexRoot(root, sub); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("absolute repo-path should be rejected, got %v", err)
	}

	// Traversal escape is rejected.
	if _, err := resolveIndexRoot(root, filepath.Join("..", "..", "etc")); err == nil ||
		!strings.Contains(err.Error(), "escapes") {
		t.Fatalf("traversal repo-path should be rejected, got %v", err)
	}

	// Non-existent subtree is rejected.
	if _, err := resolveIndexRoot(root, "nope"); err == nil {
		t.Fatalf("non-existent repo-path should be rejected, got nil")
	}
}

func TestValidateTools(t *testing.T) {
	t.Parallel()

	// Empty subset expands to all six tools, in canonical order.
	all, err := validateTools(nil)
	if err != nil {
		t.Fatalf("validateTools(nil): %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("validateTools(nil) len = %d, want 6", len(all))
	}
	want := []string{
		ToolGetRepoMap, ToolSearchSymbols, ToolSearchCode,
		ToolCheckDuplicate, ToolFindTypeUsages, ToolValidateCrossDeps,
	}
	for i := range want {
		if all[i] != want[i] {
			t.Fatalf("validateTools(nil)[%d] = %q, want %q", i, all[i], want[i])
		}
	}

	// A valid subset is preserved and de-duplicated.
	sub, err := validateTools([]string{ToolSearchSymbols, ToolSearchSymbols, ToolGetRepoMap})
	if err != nil {
		t.Fatalf("validateTools(subset): %v", err)
	}
	if len(sub) != 2 || sub[0] != ToolSearchSymbols || sub[1] != ToolGetRepoMap {
		t.Fatalf("validateTools(subset) = %v, want [%s %s]", sub, ToolSearchSymbols, ToolGetRepoMap)
	}

	// An unknown tool name is a hard error.
	if _, err := validateTools([]string{ToolGetRepoMap, "af_code_bogus"}); err == nil ||
		!strings.Contains(err.Error(), "af_code_bogus") {
		t.Fatalf("unknown tool should error, got %v", err)
	}
}

func TestResolveScopedFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inside := filepath.Join(root, "snippet.go")
	if err := os.WriteFile(inside, []byte("package x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := resolveScopedFile(root, "snippet.go")
	if err != nil {
		t.Fatalf("resolveScopedFile inside: %v", err)
	}
	if got != inside {
		t.Fatalf("resolveScopedFile = %q, want %q", got, inside)
	}

	// Absolute path rejected.
	if _, err := resolveScopedFile(root, inside); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("absolute contentFile should be rejected, got %v", err)
	}

	// Traversal escape rejected (skip on Windows path separators).
	if runtime.GOOS != "windows" {
		if _, err := resolveScopedFile(root, "../../../etc/passwd"); err == nil ||
			!strings.Contains(err.Error(), "escapes") {
			t.Fatalf("traversal contentFile should be rejected, got %v", err)
		}
	}
}
