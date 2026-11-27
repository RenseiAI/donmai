package muse

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// canonicalTempDir returns a fresh temp directory by its canonical path (the
// macOS temp dir sits behind the /var -> /private/var symlink).
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// TestHandle_PathDeniesAnchorAtWorkspaceRoot pins that the handle anchors
// relative path denies at the canonical workspace root the Muse session
// starts in. Muse reports file approvals under that root, so a symlinked or
// relative Spec.Cwd must not let a relative deny miss, nor deny every path.
func TestHandle_PathDeniesAnchorAtWorkspaceRoot(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	mkdirs(t, filepath.Join(root, "docs"), filepath.Join(root, "src"))
	link := filepath.Join(canonicalTempDir(t), "ln")
	symlink(t, root, link)

	for _, cwd := range []struct{ name, cwd string }{
		{"symlinked cwd", link},
		{"relative cwd", "ws"},
	} {
		t.Run(cwd.name, func(t *testing.T) {
			t.Parallel()
			spec := agent.Spec{Cwd: cwd.cwd, PermissionConfig: &agent.PermissionConfig{DisallowPatterns: []string{"Edit(docs/**)"}}}
			h := newHandle(nil, spec, SpawnPlan{WorkspaceRoot: root}, HandleOptions{})
			for _, tc := range []struct {
				path string
				want ApprovalAction
			}{
				{filepath.Join(root, "docs", "a.md"), ActionDecline},
				{filepath.Join(root, "src", "a.go"), ActionAllow},
			} {
				got := h.bridge.Evaluate(ApprovalRequest{Kind: ApprovalKindFileAccess, Path: tc.path})
				if got.Action != tc.want {
					t.Errorf("file %q = %s (%s), want %s", tc.path, got.Action, got.Reason, tc.want)
				}
			}
		})
	}
}

// TestApprovalBridge_PathDeniesFailClosed pins that a path deny cannot be
// stepped around through another spelling of the same file: a symlink into
// the denied tree, a symlinked deny anchor, "..", a relative path, letter
// case on a case-insensitive volume, the filesystem root, and a path that
// cannot be resolved.
func TestApprovalBridge_PathDeniesFailClosed(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	mkdirs(t, filepath.Join(root, "docs"), filepath.Join(root, "src"))
	symlink(t, filepath.Join(root, "docs"), filepath.Join(root, "alias"))
	symlink(t, filepath.Join(root, "loop"), filepath.Join(root, "loop"))
	symlink(t, filepath.Join(root, "docs", "new.md"), filepath.Join(root, "dangling"))

	secret := filepath.Join(canonicalTempDir(t), "secret")
	mkdirs(t, secret)
	secretLink := filepath.Join(canonicalTempDir(t), "secret-link")
	symlink(t, secret, secretLink)

	caseInsensitive := ActionAllow
	if runtime.GOOS == "darwin" {
		caseInsensitive = ActionDecline
	}
	file := func(p string) ApprovalRequest { return ApprovalRequest{Kind: ApprovalKindFileAccess, Path: p} }
	edit := []string{"Edit(docs/**)"}
	for _, tc := range []struct {
		name string
		deny []string
		root string
		req  ApprovalRequest
		want ApprovalAction
	}{
		{"symlink into a denied tree", edit, root, file(filepath.Join(root, "alias", "a.md")), ActionDecline},
		{"relative symlink into a denied tree", edit, root, file("alias/a.md"), ActionDecline},
		{"symlinked absolute deny, canonical path", []string{"Read(" + secretLink + "/**)"}, root, file(filepath.Join(secret, "k")), ActionDecline},
		{"symlinked root anchor, canonical path", edit, secretLink, file(filepath.Join(secret, "docs", "a.md")), ActionDecline},
		{"symlinked root anchor, any-depth deny", []string{"Read(*.key)"}, secretLink, file(filepath.Join(secret, "a.key")), ActionDecline},
		{"dangling symlink a write would create in a denied tree", edit, root, file(filepath.Join(root, "dangling")), ActionDecline},
		{"dot-dot into a denied tree", edit, root, file(root + "/src/../docs/a.md"), ActionDecline},
		{"relative dot-dot into a denied tree", edit, root, file("src/../docs/a.md"), ActionDecline},
		{"letter case on macOS", edit, root, file(filepath.Join(root, "DOCS", "a.md")), caseInsensitive},
		{"filesystem root deny", []string{"Read(/)"}, root, file("/etc/passwd"), ActionDecline},
		{"unresolvable symlink loop", edit, root, file(filepath.Join(root, "loop", "a.md")), ActionDecline},
		{"relative path without a root", []string{"Read(/srv/**)"}, "", file("a.txt"), ActionDecline},
		{"relative root fails closed", edit, "ws", file(filepath.Join(root, "src", "a.go")), ActionDecline},
		{"other paths stay allowed", edit, root, file(filepath.Join(root, "src", "a.go")), ActionAllow},
		{"other paths through a symlink stay allowed", []string{"Read(" + secretLink + "/**)"}, root, file(filepath.Join(root, "alias", "a.md")), ActionAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bridge := newApprovalBridgeAt(&agent.PermissionConfig{DisallowPatterns: tc.deny}, tc.root)
			if got := bridge.Evaluate(tc.req); got.Action != tc.want {
				t.Errorf("file %q = %s (%s), want %s", tc.req.Path, got.Action, got.Reason, tc.want)
			}
		})
	}
}
