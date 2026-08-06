package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexConfigBoundaryRejectsSymlinkedConfigWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	boundary, err := newCodexConfigBoundary(root)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	target := filepath.Join(root, "must-survive")
	const targetBody = "not provider-owned"
	if err := os.WriteFile(target, []byte(targetBody), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Remove(boundary.configPath); err != nil {
		t.Fatalf("remove owned config fixture: %v", err)
	}
	if err := os.Symlink(target, boundary.configPath); err != nil {
		t.Fatalf("symlink config fixture: %v", err)
	}
	if err := boundary.validate(); err == nil {
		t.Fatal("symlinked config unexpectedly passed validation")
	}
	if err := boundary.remove(); err != nil {
		t.Fatalf("remove boundary: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != targetBody {
		t.Fatalf("boundary cleanup followed config symlink: body=%q err=%v", body, err)
	}
}

func TestCodexConfigBoundaryRemovalPinsParentIdentityAndPersistsFailure(t *testing.T) {
	root := t.TempDir()
	boundary, err := newCodexConfigBoundary(root)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	otherInfo, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatalf("stat other parent: %v", err)
	}
	boundary.parentInfo = otherInfo

	first := boundary.remove()
	if first == nil || !strings.Contains(first.Error(), "parent identity changed") {
		t.Fatalf("first remove error = %v, want pinned-parent refusal", first)
	}
	second := boundary.remove()
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("second remove error = %v, want persistent %v", second, first)
	}
	if _, err := os.Stat(boundary.home); err != nil {
		t.Fatalf("refused boundary unexpectedly removed: %v", err)
	}
}
