package pi

import (
	"os"
	"path/filepath"
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

func mkdirAll(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// TestPolicyEngine_WalkersReachDeniedPaths pins that grep, find and ls are
// denied whenever the directory they walk could reach a path a read deny
// names: an ancestor of the deny's literal directory, a directory beneath a
// "**" or a brace set, and the filesystem root. A walk that cannot reach a
// denied path stays allowed.
func TestPolicyEngine_WalkersReachDeniedPaths(t *testing.T) {
	t.Parallel()
	walk := func(k ToolKind, p string) ToolCall { return ToolCall{Kind: k, Path: p} }
	for _, tc := range []struct {
		name string
		deny []string
		call ToolCall
		want bool
	}{
		{"any-depth deny reaches a subdirectory", []string{"Read(.env)"}, walk(ToolGrep, "/w/src"), false},
		{"absolute ** deny reaches any directory", []string{"Read(/**/secret)"}, walk(ToolFind, "/w/src"), false},
		{"brace alternative reaches its own tree", []string{"Read(/h/{.ssh/**,.aws/**})"}, walk(ToolGrep, "/h/.ssh"), false},
		{"brace base reaches a descendant", []string{"Read(/h/{.ssh,.aws}/**)"}, walk(ToolLs, "/h/.aws/sso"), false},
		{"nested braces", []string{"Read(/s/{a,{b,c/**/x}})"}, walk(ToolFind, "/s/c/d"), false},
		{"root deny reaches every walk", []string{"Read(/)"}, walk(ToolGrep, "/w/src"), false},
		{"walking the filesystem root", []string{"Read(/w/secret/*.key)"}, walk(ToolFind, "/"), false},
		{"walking an ancestor", []string{"Read(/w/secret/*.key)"}, walk(ToolGrep, "/w"), false},
		{"walking a sibling", []string{"Read(/w/secret/*.key)"}, walk(ToolGrep, "/w/src"), true},
		{"walking a subdirectory a one-level glob cannot reach", []string{"Read(/w/secret/*.key)"}, walk(ToolFind, "/w/secret/sub"), true},
		{"brace deny leaves other trees", []string{"Read(/h/{.ssh,.aws}/**)"}, walk(ToolGrep, "/h/src"), true},
		{"brace alternatives with a / leave other trees", []string{"Read(/h/{.ssh/**,.aws/**})"}, walk(ToolGrep, "/h/src"), true},
		{"nested braces leave other trees", []string{"Read(/s/{a,{b,c/**/x}})"}, walk(ToolFind, "/s/d"), true},
		{"brace sets past the expansion cap reach every walk", []string{"Read(/w/{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}/x)"}, walk(ToolGrep, "/w/src"), false},
		{"escaped brace is literal", []string{`Read(/w/\{a,b\}/**)`}, walk(ToolGrep, "/w/a"), true},
		{"escaped brace tree", []string{`Read(/w/\{a,b\}/**)`}, walk(ToolGrep, "/w/{a,b}/x"), false},
		{"class segment", []string{"Read(/w/[ab]x/**)"}, walk(ToolGrep, "/w/ax/deep"), false},
		{"class segment leaves others", []string{"Read(/w/[ab]x/**)"}, walk(ToolGrep, "/w/cx"), true},
		{"read of a file is not a walk", []string{"Read(.env)"}, walk(ToolRead, "/w/src/a.go"), true},
		{"walk with no directory at all", []string{"Read(/w/secret/**)"}, ToolCall{Kind: ToolGrep}, false},
		{"walk of the call's own directory", []string{"Read(/other/secret/**)"}, ToolCall{Kind: ToolFind, Cwd: "/other"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cwd := "/w"
			if tc.call.Kind == ToolGrep && tc.call.Path == "" {
				cwd = ""
			}
			got := NewPolicyEngine(agent.Spec{Cwd: cwd, DisallowedTools: tc.deny}).Evaluate(tc.call)
			if got.Allow != tc.want {
				t.Errorf("%+v: allowed = %v (%s), want %v", tc.call, got.Allow, got.Reason, tc.want)
			}
		})
	}
}

// TestPolicyEngine_PathDeniesFailClosed pins that a path deny cannot be
// stepped around through another spelling of the same file: a symlinked or
// relative workspace root, a symlink into the denied tree (dangling too), a
// symlinked deny directory, glob characters in the root's name, letter case
// on a case-insensitive volume, control characters, a path that cannot be
// resolved, and a relative path with no root.
func TestPolicyEngine_PathDeniesFailClosed(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	mkdirAll(t, filepath.Join(root, "secret"), filepath.Join(root, "src"), filepath.Join(root, "data"))
	link := filepath.Join(canonicalTempDir(t), "ln")
	symlinkOrSkip(t, root, link)
	symlinkOrSkip(t, filepath.Join(root, "secret"), filepath.Join(root, "alias"))
	symlinkOrSkip(t, filepath.Join(root, "secret", "new"), filepath.Join(root, "dangling"))
	symlinkOrSkip(t, filepath.Join(root, "loop"), filepath.Join(root, "loop"))
	symlinkOrSkip(t, filepath.Join(root, "x\ny"), filepath.Join(root, "nl"))
	dataLink := filepath.Join(canonicalTempDir(t), "data-link")
	symlinkOrSkip(t, filepath.Join(root, "data"), dataLink)
	mkdirAll(t, filepath.Join(root, "real-secrets"), filepath.Join(root, "a"))
	symlinkOrSkip(t, filepath.Join(root, "real-secrets"), filepath.Join(root, "a", "secrets"))
	meta := filepath.Join(canonicalTempDir(t), "a[1]{x}")
	mkdirAll(t, filepath.Join(meta, "secret"))
	metaLink := filepath.Join(canonicalTempDir(t), "l*k")
	symlinkOrSkip(t, meta, metaLink)

	caseDeny := foldPathCase
	read := func(p string) ToolCall { return ToolCall{Kind: ToolRead, Path: p} }
	secret := []string{"Read(secret/**)"}
	for _, tc := range []struct {
		name string
		cwd  string
		deny []string
		call ToolCall
		want bool
	}{
		{"symlinked root, canonical path", link, secret, read(filepath.Join(root, "secret", "k")), false},
		{"canonical root, path through a symlinked root", root, secret, read(filepath.Join(link, "secret", "k")), false},
		{"symlinked root, any-depth deny", link, []string{"Read(*.key)"}, read(filepath.Join(root, "src", "a.key")), false},
		{"symlinked root, walker", link, []string{"Read(.env)"}, ToolCall{Kind: ToolGrep, Path: filepath.Join(root, "src")}, false},
		{"symlinked root leaves other paths", link, secret, read(filepath.Join(root, "src", "a.go")), true},
		{"relative root denies every path of a relative deny", "w", secret, read("/elsewhere/a.go"), false},
		{"relative path with no root", "", []string{"Read(/abs/**)"}, read("rel/a.go"), false},
		{"relative path under a relative root", "w", []string{"Read(/abs/**)"}, read("rel/a.go"), false},
		{"deny directory that cannot be resolved", root, []string{"Read(" + filepath.Join(root, "loop", "x") + "/**)"}, read(filepath.Join(root, "src", "a.go")), false},
		{"symlink into the denied tree", root, secret, read(filepath.Join(root, "alias", "k")), false},
		{"dangling symlink into the denied tree", root, []string{"Edit(secret/**)"}, ToolCall{Kind: ToolWrite, Path: filepath.Join(root, "dangling")}, false},
		{"symlink after a glob character, as written", root, []string{"Edit(*/secrets/**)"}, ToolCall{Kind: ToolEdit, Path: filepath.Join(root, "a", "secrets", "k")}, false},
		{"symlinked deny directory, canonical path", root, []string{"Read(" + dataLink + "/**)"}, read(filepath.Join(root, "data", "x")), false},
		{"symlinked deny directory, walker", root, []string{"Read(" + dataLink + "/**)"}, ToolCall{Kind: ToolFind, Path: filepath.Join(root, "data")}, false},
		{"glob characters in a symlinked root", metaLink, secret, read(filepath.Join(meta, "secret", "k")), false},
		{"glob characters in the root, walker", meta, []string{"Read(secret/*.key)"}, ToolCall{Kind: ToolGrep, Path: meta}, false},
		{"glob characters in the root leave others", meta, secret, read(filepath.Join(meta, "src", "a.go")), true},
		{"letter case", root, secret, read(filepath.Join(root, "SECRET", "k")), !caseDeny},
		{"letter case, walker", root, []string{"Read(secret/*.key)"}, ToolCall{Kind: ToolGrep, Path: filepath.Join(root, "Secret")}, !caseDeny},
		{"Kelvin sign", root, []string{"Read(key/**)"}, read(filepath.Join(root, "Key", "a")), !caseDeny},
		{"sharp s", root, []string{"Read(strasse/**)"}, read(filepath.Join(root, "stra\u00dfe", "a")), !caseDeny},
		{"decomposed letter", root, []string{"Read(café/**)"}, read(filepath.Join(root, "café", "a")), !caseDeny},
		{"decomposed letter under ?", root, []string{"Read(caf?/**)"}, read(filepath.Join(root, "café", "a")), !caseDeny},
		{"non-ASCII literal", root, []string{"Read(café/**)"}, read(filepath.Join(root, "café", "a")), false},
		{"non-ASCII under ?", root, []string{"Read(caf?/**)"}, read(filepath.Join(root, "café", "a")), false},
		{"non-ASCII leaves others", root, []string{"Read(café/**)"}, read(filepath.Join(root, "cafe", "a")), true},
		{"control character in the path", root, secret, read(filepath.Join(root, "src", "a\nb")), false},
		{"control character through a symlink", root, secret, read(filepath.Join(root, "nl")), false},
		{"symlink loop", root, secret, read(filepath.Join(root, "loop", "x")), false},
		{"no deny leaves a control character alone", root, nil, read(filepath.Join(root, "src", "a\nb")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NewPolicyEngine(agent.Spec{Cwd: tc.cwd, DisallowedTools: tc.deny}).Evaluate(tc.call)
			if got.Allow != tc.want {
				t.Errorf("%+v: allowed = %v (%s), want %v", tc.call, got.Allow, got.Reason, tc.want)
			}
		})
	}
}

// TestParseAdjudicateCall_MirrorsPiPathNormalization pins that a tool path is
// judged as the file pi's own path resolution opens: a leading "@" is
// dropped, "~" is the home directory, a file:// URL is its path and Unicode
// spaces are plain spaces. Judged as written, each would be a name under cwd.
func TestParseAdjudicateCall_MirrorsPiPathNormalization(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for _, tc := range []struct{ in, want string }{
		{"@/etc/x", "/etc/x"},
		{"@~/.ssh/id_rsa", home + "/.ssh/id_rsa"},
		{"~", home},
		{"file:///etc/passwd", "/etc/passwd"},
		{"@file:///etc/pass%77d", "/etc/passwd"},
		{"file://localhost/etc/passwd", "/etc/passwd"},
		{"a b", "/w/a b"},
		{"@@x", "/w/@x"},
		{"src/a.go", "/w/src/a.go"},
	} {
		call := parseAdjudicateCall(map[string]any{"toolName": "read", "input": map[string]any{"path": tc.in}}, "/w")
		if call.Path != tc.want {
			t.Errorf("path %q judged as %q, want %q", tc.in, call.Path, tc.want)
		}
	}
	engine := NewPolicyEngine(agent.Spec{Cwd: "/w", Autonomous: true, DisallowedTools: []string{"Read(~/.ssh/**)"}})
	for _, in := range []string{"@~/.ssh/id_rsa", "file://" + home + "/.ssh/id_rsa"} {
		call := parseAdjudicateCall(map[string]any{"toolName": "read", "input": map[string]any{"path": in}}, "/w")
		if engine.Evaluate(call).Allow {
			t.Errorf("read %q allowed under a ~/.ssh deny", in)
		}
	}
	write := parseAdjudicateCall(map[string]any{"toolName": "write", "input": map[string]any{"path": "@/etc/x"}}, "/w")
	if engine.Evaluate(write).Allow {
		t.Error("write @/etc/x allowed: judged inside the worktree")
	}
}

// TestPolicyEngine_ReadVariants pins that a read deny covers the alternate
// names pi's read tool falls back to when the typed name does not exist.
func TestPolicyEngine_ReadVariants(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ deny, path string }{
		{"Read(/w/Shot 1 PM.png)", "/w/Shot 1 PM.png"},
		{"Read(/w/d’x)", "/w/d'x"},
	} {
		engine := NewPolicyEngine(agent.Spec{Cwd: "/w", DisallowedTools: []string{tc.deny}})
		if got := engine.Evaluate(ToolCall{Kind: ToolRead, Path: tc.path}); got.Allow {
			t.Errorf("read %q allowed under %s", tc.path, tc.deny)
		}
	}
}
