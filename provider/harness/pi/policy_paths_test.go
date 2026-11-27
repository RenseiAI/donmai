package pi

import (
	"os"
	"path/filepath"
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

// TestPolicyEngine_WalkersReachDeniedPaths pins the structural walk rule:
// grep, find and ls are denied on a deny's literal directory, its ancestors
// (the filesystem root included) and inside a denied tree, per brace
// alternative. A walk that cannot reach a denied path stays allowed.
func TestPolicyEngine_WalkersReachDeniedPaths(t *testing.T) {
	t.Parallel()
	walk := func(k ToolKind, p string) ToolCall { return ToolCall{Kind: k, Path: p} }
	for _, tc := range []struct {
		name string
		deny []string
		call ToolCall
		want bool
	}{
		{"brace alternative reaches its own tree", []string{"Read(/h/{.ssh/**,.aws/**})"}, walk(ToolGrep, "/h/.ssh"), false},
		{"brace base reaches a descendant", []string{"Read(/h/{.ssh,.aws}/**)"}, walk(ToolLs, "/h/.aws/sso"), false},
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

// TestPolicyEngine_WalkersScanForDeniedEntries pins that a walk below a
// wildcard of a deny is denied only when a matching entry exists beneath it:
// the scan counts hidden names, does not follow symlinks (as fd and rg do not),
// lists one level for ls, and denies when it cannot finish.
func TestPolicyEngine_WalkersScanForDeniedEntries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		deny    []string // "$R" is the temp root; "$W" the worktree
		entries []string // under the worktree: "d/" a directory, "d/!" an unreadable one, "a->b" a symlink to $R/me/b
		call    func(w string) ToolCall
		limit   int
		want    bool
	}{
		// The home-wide baseline shape, with the worktree beneath its "**".
		{"baseline, clean worktree, grep root", baseline, []string{"src/pkg/a.go"}, grepAt(""), 0, true},
		{"baseline, clean worktree, grep with no path", baseline, []string{"src/pkg/a.go"}, func(string) ToolCall { return ToolCall{Kind: ToolGrep} }, 0, true},
		{"baseline, clean worktree, grep src", baseline, []string{"src/pkg/a.go"}, grepAt("src"), 0, true},
		{"baseline, clean worktree, find", baseline, []string{"src/pkg/a.go"}, kindAt(ToolFind, ""), 0, true},
		{"baseline, clean worktree, ls src/pkg", baseline, []string{"src/pkg/a.go"}, kindAt(ToolLs, "src/pkg"), 0, true},
		{"baseline, .env beneath, grep root", baseline, []string{"src/pkg/a.go", "src/.env.local"}, grepAt(""), 0, false},
		{"baseline, .env beneath, grep with no path", baseline, []string{"src/.env.local"}, func(string) ToolCall { return ToolCall{Kind: ToolGrep} }, 0, false},
		{"baseline, .env beneath, grep src", baseline, []string{"src/.env.local"}, grepAt("src"), 0, false},
		{"baseline, .env elsewhere, grep docs", baseline, []string{"src/.env.local", "docs/a.md"}, grepAt("docs"), 0, true},
		{"baseline, .ssh beneath, find", baseline, []string{"a/.ssh/id_rsa"}, kindAt(ToolFind, ""), 0, false},
		{"baseline, empty .ssh beneath, find", baseline, []string{"a/.ssh/"}, kindAt(ToolFind, ""), 0, false},
		{"baseline, hidden directory", baseline, []string{".cache/x/.env"}, grepAt(""), 0, false},
		{"baseline, ls lists one level", baseline, []string{"src/.env.local"}, kindAt(ToolLs, ""), 0, true},
		{"baseline, ls of the directory holding it", baseline, []string{"src/.env.local"}, kindAt(ToolLs, "src"), 0, false},
		{"baseline, symlinked directory not followed", baseline, []string{"out/.env", "src/ln->wt/out"}, grepAt("src"), 0, true},
		{"baseline, symlink named like a denied file", baseline, []string{"src/.env->wt/src/a.go", "src/a.go"}, grepAt("src"), 0, false},
		{"scan past its bound", baseline, []string{"src/a", "src/b", "src/c", "src/d"}, grepAt("src"), 3, false},
		{"scan within its bound", baseline, []string{"src/a", "src/b"}, grepAt("src"), 3, true},
		{"unreadable directory beneath", baseline, []string{"src/a.go", "src/locked/!"}, grepAt("src"), 0, false},
		// A bare-name relative deny and its root-only spelling.
		{"Read(.env) walks the root", []string{"Read(.env)"}, nil, grepAt(""), 0, false},
		{"Read(.env), clean subdirectory", []string{"Read(.env)"}, []string{"src/a.go"}, grepAt("src"), 0, true},
		{"Read(.env), .env in the subdirectory", []string{"Read(.env)"}, []string{"src/deep/.env"}, grepAt("src"), 0, false},
		{"Read(./.env) walks the root", []string{"Read(./.env)"}, nil, grepAt(""), 0, false},
		{"Read(./.env), .env in a subdirectory", []string{"Read(./.env)"}, []string{"src/.env"}, grepAt("src"), 0, true},
		// Other wildcard shapes.
		{"absolute ** deny, match beneath", []string{"Read($R/**/secret)"}, []string{"src/x/secret"}, kindAt(ToolFind, "src"), 0, false},
		{"absolute ** deny, no match", []string{"Read($R/**/secret)"}, []string{"src/x/other"}, kindAt(ToolFind, "src"), 0, true},
		{"nested braces, match beneath", []string{"Read($W/{a,{b,c/**/x}})"}, []string{"c/d/e/x"}, kindAt(ToolFind, "c/d"), 0, false},
		{"nested braces, no match", []string{"Read($W/{a,{b,c/**/x}})"}, []string{"c/d/e/y"}, kindAt(ToolFind, "c/d"), 0, true},
		{"wildcard segment, match beneath", []string{"Read($W/*/secret/**)"}, []string{"a/secret/k"}, grepAt("a"), 0, false},
		{"wildcard segment, no match", []string{"Read($W/*/secret/**)"}, []string{"a/public/k"}, grepAt("a"), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := canonicalTempDir(t)
			w := filepath.Join(r, "me", "wt")
			mkdirAll(t, w)
			for _, e := range tc.entries {
				if name, target, ok := strings.Cut(e, "->"); ok {
					mkdirAll(t, filepath.Dir(filepath.Join(w, name)))
					symlinkOrSkip(t, filepath.Join(r, "me", target), filepath.Join(w, name))
					continue
				}
				if dir, ok := strings.CutSuffix(e, "/!"); ok {
					if os.Geteuid() == 0 {
						t.Skip("root reads every directory")
					}
					// Empty, so the temp-dir cleanup can still remove it.
					locked := filepath.Join(w, dir)
					mkdirAll(t, locked)
					if err := os.Chmod(locked, 0); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if strings.HasSuffix(e, "/") {
					mkdirAll(t, filepath.Join(w, e))
					continue
				}
				mkdirAll(t, filepath.Dir(filepath.Join(w, e)))
				if err := os.WriteFile(filepath.Join(w, e), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var deny []string
			for _, d := range tc.deny {
				deny = append(deny, strings.NewReplacer("$R", r, "$W", w).Replace(d))
			}
			engine := NewPolicyEngine(agent.Spec{Cwd: w, Autonomous: true, DisallowedTools: deny})
			if tc.limit > 0 {
				engine.denyReads.scanLimit = tc.limit
			}
			call := tc.call(w)
			if got := engine.Evaluate(call); got.Allow != tc.want {
				t.Errorf("%+v: allowed = %v (%s), want %v", call, got.Allow, got.Reason, tc.want)
			}
		})
	}
}

// baseline is the shape of a home-wide credential deny list, anchored at the
// test's temp root in place of the home parent directory.
var baseline = []string{"Read($R/**/.env*)", "Read($R/**/.ssh/**)", "Read(~/.aws/**)", "Bash(env)"}

func grepAt(rel string) func(string) ToolCall { return kindAt(ToolGrep, rel) }

func kindAt(k ToolKind, rel string) func(string) ToolCall {
	return func(w string) ToolCall { return ToolCall{Kind: k, Path: filepath.Join(w, rel)} }
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
		{"symlinked root, walker", link, []string{"Read(.env)"}, ToolCall{Kind: ToolGrep, Path: root}, false},
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
		{"glob characters in the root, bare-name deny", meta, []string{"Read(.env)"}, read(filepath.Join(meta, "src", ".env")), false},
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

// TestParseAdjudicateCall_FileURLsGoCannotReproduceDeny pins that a file://
// path is denied when its reading could differ from pi's (WHATWG URL parsing
// and fileURLToPath): pi opens the path on the right of each, and judged any
// other way it would be a name under cwd.
func TestParseAdjudicateCall_FileURLsGoCannotReproduceDeny(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	engine := NewPolicyEngine(agent.Spec{Cwd: "/w", Autonomous: true, DisallowedTools: []string{"Read(/etc/passwd)", "Read(.env)"}})
	for _, in := range []string{
		"file://LOCALHOST" + home + "/.ssh/id_rsa",
		"file://%6Cocalhost" + home + "/.ssh/id_rsa",
		"file://" + home + "/.ssh/id_rsa#%zz",
		"file:///w/.env#x",
		"file:///w/.env?x",
		"file:///w/.env ",
		"file:///w/.env\t",
		"file:///w/src\\..\\.env",
		"file://LOCALHOST/etc/passwd",
		"file://%6Cocalhost/etc/passwd",
		"file://host/etc/passwd",
		"file:///w/a%zz",
		"file:///w/a%2Fb",
	} {
		call := parseAdjudicateCall(map[string]any{"toolName": "read", "input": map[string]any{"path": in}}, "/w")
		if got := engine.Evaluate(call); got.Allow {
			t.Errorf("read %q allowed (judged as %q)", in, call.Path)
		}
	}
	for _, in := range []string{"file:///w/src/a.go", "file://localhost/w/src/a.go", "file://LOCALHOST/w/src/a.go", "file://%6Cocalhost/w/src/a.go", "file:///w/caf%C3%A9"} {
		call := parseAdjudicateCall(map[string]any{"toolName": "read", "input": map[string]any{"path": in}}, "/w")
		if got := engine.Evaluate(call); !got.Allow {
			t.Errorf("read %q denied (%s)", in, got.Reason)
		}
	}
}
