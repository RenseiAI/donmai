package muse

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Skipf("file name unsupported: %v", err)
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
	workflows := filepath.Join(root, ".github", "workflows")
	mkdirs(t, workflows)
	writeFile(t, filepath.Join(workflows, "b\n.yml"))
	writeFile(t, filepath.Join(root, "docs", "a.md"))
	symlink(t, filepath.Join(workflows, "b\n.yml"), filepath.Join(root, "src", "nl-existing"))
	symlink(t, filepath.Join(workflows, "c\n.yml"), filepath.Join(root, "src", "nl-dangling"))
	symlink(t, filepath.Join(root, "docs", "a.md"), filepath.Join(root, "src", "sp "))
	symlink(t, filepath.Join(root, "docs", "a.md"), filepath.Join(root, "src", "nl\n"))
	symlink(t, filepath.Join(root, "src", "a.go"), filepath.Join(root, "docs", "n\nl"))

	secret := filepath.Join(canonicalTempDir(t), "secret")
	mkdirs(t, secret)
	secretLink := filepath.Join(canonicalTempDir(t), "secret-link")
	symlink(t, secret, secretLink)
	metaLink := filepath.Join(canonicalTempDir(t), "link[1]")
	symlink(t, secret, metaLink)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
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
		{"symlinked root with a glob character in its name", []string{"Read(*.key)"}, metaLink, file(filepath.Join(secret, "a.key")), ActionDecline},
		{"dangling symlink a write would create in a denied tree", edit, root, file(filepath.Join(root, "dangling")), ActionDecline},
		{"dot-dot into a denied tree", edit, root, file(root + "/src/../docs/a.md"), ActionDecline},
		{"relative dot-dot into a denied tree", edit, root, file("src/../docs/a.md"), ActionDecline},
		{"letter case on macOS", edit, root, file(filepath.Join(root, "DOCS", "a.md")), caseInsensitive},
		{"long s folds to s on macOS", edit, root, file(filepath.Join(root, "doc\u017f", "a.md")), caseInsensitive},
		{"long s home secret on macOS", []string{"Read(~/.ssh/**)"}, root, file(filepath.Join(home, ".\u017f\u017fh", "id_rsa")), caseInsensitive},
		{"sharp s folds to ss on macOS", []string{"Read(~/.ssh/**)"}, root, file(filepath.Join(home, ".\u00dfh", "id_rsa")), caseInsensitive},
		{"capital sharp s folds to ss on macOS", []string{"Read(~/.ssh/**)"}, root, file(filepath.Join(home, ".\u1e9eh", "id_rsa")), caseInsensitive},
		{"fl ligature folds to fl on macOS", []string{"Edit(.github/workflows/**)"}, root, file(filepath.Join(workflows, "..", "work\ufb02ows", "a.yml")), caseInsensitive},
		{"non-ASCII deny matches its fold on macOS", []string{"Edit(stra\u00dfe/**)"}, root, file(filepath.Join(root, "STRASSE", "x")), caseInsensitive},
		{"non-ASCII deny leaves other names", []string{"Edit(stra\u00dfe/**)"}, root, file(filepath.Join(root, "stra\u00dfe.go")), ActionAllow},
		{"composed deny matches decomposed path on macOS", []string{"Edit(caf\u00e9/**)"}, root, file(filepath.Join(root, "cafe\u0301", "a")), caseInsensitive},
		{"non-ASCII deny, same spelling", []string{"Edit(caf\u00e9/**)"}, root, file(filepath.Join(root, "caf\u00e9", "a")), ActionDecline},
		{"non-ASCII letter in a deny class", []string{"Edit([\u00e9a]bc/**)"}, root, file(filepath.Join(root, "\u00e9bc", "x")), ActionDecline},
		{"non-ASCII workspace root", edit, filepath.Join(root, "jos\u00e9"), file(filepath.Join(root, "jos\u00e9", "docs", "a.md")), ActionDecline},
		{"non-ASCII workspace root leaves other paths", edit, filepath.Join(root, "jos\u00e9"), file(filepath.Join(root, "jos\u00e9", "src", "a.go")), ActionAllow},
		{"symlink to a newline target", []string{"Edit(.github/workflows/**)"}, root, file(filepath.Join(root, "src", "nl-existing")), ActionDecline},
		{"dangling symlink to a newline target", []string{"Edit(.github/workflows/**)"}, root, file(filepath.Join(root, "src", "nl-dangling")), ActionDecline},
		{"trailing-space symlink into a denied tree", edit, root, file(filepath.Join(root, "src", "sp ")), ActionDecline},
		{"trailing-newline symlink into a denied tree", edit, root, file(filepath.Join(root, "src", "nl\n")), ActionDecline},
		{"newline-named symlink in a denied tree to an allowed file", edit, root, file(filepath.Join(root, "docs", "n\nl")), ActionDecline},
		{"leading space on an absolute path", edit, root, file(" " + filepath.Join(root, "docs", "a.md")), ActionDecline},
		{"newline in an absolute path", []string{"Edit(.github/workflows/**)"}, root, file(filepath.Join(root, ".github", "workflows", "a\n.yml")), ActionDecline},
		{"newline in a relative path", []string{"Edit(.github/workflows/**)"}, root, file(".github/workflows/a\n.yml"), ActionDecline},
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

// TestApprovalBridge_UnresolvableDenyDirectory pins that a deny whose own
// directory cannot be resolved still denies every file access, and says why.
func TestApprovalBridge_UnresolvableDenyDirectory(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	mkdirs(t, filepath.Join(root, "src"))
	loop := filepath.Join(canonicalTempDir(t), "loop")
	symlink(t, loop, loop)

	bridge := newApprovalBridgeAt(&agent.PermissionConfig{DisallowPatterns: []string{"Read(" + loop + "/inner/**)"}}, root)
	got := bridge.Evaluate(ApprovalRequest{Kind: ApprovalKindFileAccess, Path: filepath.Join(root, "src", "a.go")})
	if got.Action != ActionDecline || !strings.Contains(got.Reason, "cannot be resolved") {
		t.Errorf("file access with an unresolvable deny directory = %s (%s), want decline naming the unresolvable deny", got.Action, got.Reason)
	}
}

// TestPathDenies_NonASCIIHome pins that a home directory with a non-ASCII
// letter still anchors "~/" denies.
func TestPathDenies_NonASCIIHome(t *testing.T) {
	t.Parallel()
	home := "/Users/jos\u00e9"
	d, errs := compilePathDenies([]string{"Read(~/.ssh/**)"}, "/w", home, false)
	if len(errs) > 0 {
		t.Fatalf("compile errors: %v", errs)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{home + "/.ssh/id_rsa", true},
		{home + "/src/a.go", false},
	} {
		if got, reason := d.denies(tc.path); got != tc.want {
			t.Errorf("denies(%q) = %v (%s), want %v", tc.path, got, reason, tc.want)
		}
	}
}
