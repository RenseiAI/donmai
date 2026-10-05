package confinement

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// specWorld is a minimal session on disk: a workarea with a mutable and a
// read-only leaf, harness state inside the mutable leaf, and session tmp and
// cache beside the root.
type specWorld struct {
	base, home, stateHome, profileDir   string
	ws, mut, ro, state, ext, tmp, cache string
}

func newSpecWorld(t *testing.T) specWorld {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	w := specWorld{base: base}
	w.home = filepath.Join(base, "home")
	w.stateHome = filepath.Join(w.home, ".state")
	w.profileDir = filepath.Join(w.stateHome, "profiles")
	w.ws = filepath.Join(w.stateHome, "sessions", "s1")
	w.mut = filepath.Join(w.ws, "mut")
	w.ro = filepath.Join(w.ws, "ro")
	w.state = filepath.Join(w.mut, ".h")
	w.ext = filepath.Join(w.state, "ext")
	w.tmp = filepath.Join(w.stateHome, "tmp", "s1")
	w.cache = filepath.Join(w.stateHome, "cache", "s1")
	for _, dir := range []string{w.profileDir, filepath.Join(w.mut, ".git"), w.ro, w.ext, w.tmp, w.cache, filepath.Join(w.ws, ".workarea")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	return w
}

func (w specWorld) spec() Spec {
	return Spec{
		SessionID:      "s1",
		HarnessID:      "h1",
		SessionMode:    "autonomous",
		WorkareaRoot:   w.ws,
		MutableLeaves:  []string{w.mut},
		HarnessState:   []string{w.state},
		SessionTmp:     w.tmp,
		Caches:         []Cache{{Env: "GOCACHE", Dir: w.cache}},
		ReadOnlyLeaves: []string{w.ro},
		Protected:      []string{w.ext},
	}
}

func (w specWorld) guards() guards {
	return guards{home: w.home, stateHome: w.stateHome, profileDir: w.profileDir}
}

func evalSymlinks(path string) (string, error) { return filepath.EvalSymlinks(path) }

func TestResolveSpec_DeclaredSetResolves(t *testing.T) {
	w := newSpecWorld(t)
	resolved, err := resolveSpec(w.spec(), w.guards(), evalSymlinks)
	if err != nil {
		t.Fatalf("resolveSpec: %v", err)
	}
	var got []string
	for _, root := range resolved.Writable {
		got = append(got, string(root.Class)+"="+root.Path)
	}
	want := []string{
		"mutable_leaf=" + w.mut,
		"harness_state=" + w.state,
		"session_tmp=" + w.tmp,
		"session_cache=" + w.cache,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writable set:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Join(resolved.Pins, ",") != strings.Join([]string{w.mut, w.state}, ",") {
		t.Fatalf("pins = %v, want the mutable leaf and the state directory above the protected path", resolved.Pins)
	}
	if strings.Join(resolved.ReadOnlyLeafNames, ",") != "ro" {
		t.Fatalf("read-only leaf names = %v", resolved.ReadOnlyLeafNames)
	}
	if resolved.MetadataDir != filepath.Join(w.ws, ".workarea") {
		t.Fatalf("metadata dir = %q", resolved.MetadataDir)
	}
	env := strings.Join(resolved.environment(), "\n")
	for _, binding := range []string{"TMPDIR=" + w.tmp, "TMP=" + w.tmp, "TEMP=" + w.tmp, "GOCACHE=" + w.cache} {
		if !strings.Contains(env, binding) {
			t.Errorf("environment lacks %s:\n%s", binding, env)
		}
	}
}

func TestResolveSpec_RefusesUnrepresentableSets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, w specWorld, spec *Spec, g *guards)
	}{
		{"no session tmp", func(_ *testing.T, _ specWorld, spec *Spec, _ *guards) { spec.SessionTmp = "" }},
		{"no session id", func(_ *testing.T, _ specWorld, spec *Spec, _ *guards) { spec.SessionID = " " }},
		{"relative mutable leaf", func(_ *testing.T, _ specWorld, spec *Spec, _ *guards) { spec.MutableLeaves = []string{"mut"} }},
		{"missing mutable leaf", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.MutableLeaves = []string{filepath.Join(w.ws, "absent")}
		}},
		{"mutable leaf replaced by a link to the home", func(t *testing.T, w specWorld, spec *Spec, _ *guards) {
			link := filepath.Join(w.ws, "linked")
			if err := os.Symlink(w.home, link); err != nil {
				t.Fatal(err)
			}
			spec.MutableLeaves = []string{link}
		}},
		{"mutable leaf outside the workarea root", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.MutableLeaves = []string{w.cache}
		}},
		{"mutable leaf is a worktree with a shared common directory", func(t *testing.T, w specWorld, spec *Spec, _ *guards) {
			leaf := filepath.Join(w.ws, "worktree")
			if err := os.MkdirAll(leaf, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(leaf, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			spec.MutableLeaves = append(spec.MutableLeaves, leaf)
		}},
		{"session tmp covers the operator home", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) { spec.SessionTmp = w.home }},
		{"cache covers the host state home", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.Caches = []Cache{{Env: "GOCACHE", Dir: w.stateHome}}
		}},
		{"harness state covers the workarea root", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) { spec.HarnessState = []string{w.ws} }},
		{"session tmp is the filesystem root", func(_ *testing.T, _ specWorld, spec *Spec, _ *guards) { spec.SessionTmp = "/" }},
		{"harness state inside a read-only leaf", func(t *testing.T, w specWorld, spec *Spec, _ *guards) {
			dir := filepath.Join(w.ro, "state")
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			spec.HarnessState = []string{dir}
		}},
		{"leaf declared both mutable and read-only", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.ReadOnlyLeaves = append(spec.ReadOnlyLeaves, w.mut)
		}},
		{"harness state through a link planted in the mutable leaf", func(t *testing.T, w specWorld, spec *Spec, _ *guards) {
			elsewhere := filepath.Join(w.base, "elsewhere", "state")
			if err := os.MkdirAll(elsewhere, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Dir(elsewhere), filepath.Join(w.mut, "planted")); err != nil {
				t.Fatal(err)
			}
			spec.HarnessState = []string{filepath.Join(w.mut, "planted", "state")}
		}},
		{"a file in the set hard-linked to a file outside it", func(t *testing.T, w specWorld, _ *Spec, _ *guards) {
			outside := filepath.Join(w.base, "store-file")
			if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, filepath.Join(w.state, "linked")); err != nil {
				t.Fatal(err)
			}
		}},
		{"reserved cache variable", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.Caches = []Cache{{Env: "TMPDIR", Dir: w.cache}}
		}},
		{"malformed cache variable", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.Caches = []Cache{{Env: "GO CACHE", Dir: w.cache}}
		}},
		{"repeated cache variable", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.Caches = []Cache{{Env: "GOCACHE", Dir: w.cache}, {Env: "GOCACHE", Dir: w.tmp}}
		}},
		{"profile directory inside the workarea root", func(_ *testing.T, w specWorld, _ *Spec, g *guards) {
			g.profileDir = filepath.Join(w.ws, "profiles")
		}},
		{"profile directory inside the writable set", func(_ *testing.T, w specWorld, _ *Spec, g *guards) {
			g.profileDir = filepath.Join(w.tmp, "profiles")
		}},
		{"read-only leaf outside the workarea root", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.ReadOnlyLeaves = []string{w.cache}
		}},
		{"missing protected path", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.Protected = []string{filepath.Join(w.state, "absent")}
		}},
		{"read paths without a read scope", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.ReadPaths = []string{filepath.Join(w.home, ".gitconfig")}
		}},
		{"read scope home-minus-secrets", func(_ *testing.T, _ specWorld, spec *Spec, _ *guards) {
			spec.ReadScope = agent.FileReadHomeMinusSecrets
		}},
		{"unknown read scope", func(_ *testing.T, _ specWorld, spec *Spec, _ *guards) { spec.ReadScope = "everything" }},
		{"relative read path", func(_ *testing.T, _ specWorld, spec *Spec, _ *guards) {
			spec.ReadScope, spec.ReadPaths = agent.FileReadWorkarea, []string{".gitconfig"}
		}},
		{"read path is the filesystem root", func(_ *testing.T, _ specWorld, spec *Spec, _ *guards) {
			spec.ReadScope, spec.ReadPaths = agent.FileReadWorkarea, []string{"/"}
		}},
		{"read path covers the operator home", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.ReadScope, spec.ReadPaths = agent.FileReadWorkarea, []string{w.home}
		}},
		{"read path covers the host state home", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.ReadScope, spec.ReadPaths = agent.FileReadWorkarea, []string{w.stateHome}
		}},
		{"read path covers the workarea root", func(_ *testing.T, w specWorld, spec *Spec, _ *guards) {
			spec.ReadScope, spec.ReadPaths = agent.FileReadWorkarea, []string{filepath.Dir(w.ws)}
		}},
		{"read path through a link planted in harness state", func(t *testing.T, w specWorld, spec *Spec, _ *guards) {
			if err := os.Symlink(w.home, filepath.Join(w.state, "home")); err != nil {
				t.Fatal(err)
			}
			spec.ReadScope, spec.ReadPaths = agent.FileReadWorkarea, []string{filepath.Join(w.state, "home", ".ssh")}
		}},
		{"relative declared socket", func(_ *testing.T, _ specWorld, spec *Spec, _ *guards) { spec.Sockets = []string{"sock"} }},
		{"declared socket through a link planted in harness state", func(t *testing.T, w specWorld, spec *Spec, _ *guards) {
			elsewhere := filepath.Join(w.base, "runtime")
			if err := os.MkdirAll(elsewhere, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(w.state, "run")); err != nil {
				t.Fatal(err)
			}
			spec.Sockets = []string{filepath.Join(w.state, "run", "agent.sock")}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newSpecWorld(t)
			spec, g := w.spec(), w.guards()
			tt.mutate(t, w, &spec, &g)
			_, err := resolveSpec(spec, g, evalSymlinks)
			if reason, _ := ReasonOf(err); reason != ReasonWritableSetUnrepresentable {
				t.Fatalf("resolveSpec: err=%v, want writable_set_unrepresentable", err)
			}
			if err != nil && strings.Contains(err.Error(), w.home) {
				t.Errorf("refusal detail carries a raw home path: %v", err)
			}
		})
	}
}

func TestCheckHardLinks_LinksInsideTheSetAreFine(t *testing.T) {
	w := newSpecWorld(t)
	inside := filepath.Join(w.mut, "a")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// One link in the leaf, one in the nested state directory: both inside,
	// and counted once each even though the state directory is also a root.
	if err := os.Link(inside, filepath.Join(w.state, "b")); err != nil {
		t.Fatal(err)
	}
	if err := checkHardLinks([]string{w.mut, w.state}); err != nil {
		t.Fatalf("checkHardLinks: %v", err)
	}
	outside := filepath.Join(w.base, "c")
	if err := os.Link(inside, outside); err != nil {
		t.Fatal(err)
	}
	if reason, _ := ReasonOf(checkHardLinks([]string{w.mut, w.state})); reason != ReasonWritableSetUnrepresentable {
		t.Fatal("a third link outside the set was not refused")
	}
}

func TestAncestorPins(t *testing.T) {
	tests := []struct {
		name     string
		writable []string
		denied   []string
		want     []string
	}{
		{"sibling leaves need no pin", []string{"/w/mut"}, []string{"/w/ro"}, nil},
		{"nested path pins the root and each ancestor", []string{"/w/mut"}, []string{"/w/mut/a/b/c"}, []string{"/w/mut", "/w/mut/a", "/w/mut/a/b"}},
		{"the outermost root is pinned", []string{"/w/mut/a", "/w/mut"}, []string{"/w/mut/a/x"}, []string{"/w/mut", "/w/mut/a"}},
		{"a denied root itself is not an ancestor", []string{"/w/mut"}, []string{"/w/mut"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ancestorPins(tt.writable, tt.denied)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("ancestorPins = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInsideOrEqual_FoldsCase(t *testing.T) {
	tests := []struct {
		path, base string
		want       bool
	}{
		{"/a/b", "/a", true},
		{"/a", "/a", true},
		{"/A/B", "/a", true},
		{"/ab", "/a", false},
		{"/a", "/a/b", false},
		{"/x", "/", true},
	}
	for _, tt := range tests {
		if got := insideOrEqual(tt.path, tt.base); got != tt.want {
			t.Errorf("insideOrEqual(%q, %q) = %v, want %v", tt.path, tt.base, got, tt.want)
		}
	}
}

func TestResolveSpec_LoopbackTCPPorts(t *testing.T) {
	w := newSpecWorld(t)
	spec := w.spec()
	spec.LoopbackTCPPorts = []int{8080, 22}
	resolved, err := resolveSpec(spec, w.guards(), evalSymlinks)
	if err != nil {
		t.Fatalf("resolveSpec: %v", err)
	}
	if len(resolved.LoopbackTCPPorts) != 2 || resolved.LoopbackTCPPorts[0] != 22 || resolved.LoopbackTCPPorts[1] != 8080 {
		t.Fatalf("ports = %v, want sorted [22 8080]", resolved.LoopbackTCPPorts)
	}
	for _, ports := range [][]int{{0}, {65536}, {-1}, {80, 80}} {
		bad := w.spec()
		bad.LoopbackTCPPorts = ports
		_, err := resolveSpec(bad, w.guards(), evalSymlinks)
		if reason, _ := ReasonOf(err); reason != ReasonWritableSetUnrepresentable {
			t.Fatalf("ports %v: err=%v, want writable_set_unrepresentable", ports, err)
		}
	}
}

// TestResolveSpec_ReadScope: a workarea read scope resolves with its
// declared read paths canonical, sorted and without repeats, and the
// session's read allowlist is its writable roots, read-only leaves and read
// paths. Open reads resolve to no scope.
func TestResolveSpec_ReadScope(t *testing.T) {
	w := newSpecWorld(t)
	dotfiles := filepath.Join(w.base, "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotfiles, "gitconfig"), []byte("[user]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(w.home, ".gitconfig")
	if err := os.Symlink(filepath.Join(dotfiles, "gitconfig"), link); err != nil {
		t.Fatal(err)
	}
	spec := w.spec()
	spec.ReadScope = agent.FileReadWorkarea
	absent := filepath.Join(w.home, ".config", "git")
	spec.ReadPaths = []string{link, absent, link}
	resolved, err := resolveSpec(spec, w.guards(), evalSymlinks)
	if err != nil {
		t.Fatalf("resolveSpec: %v", err)
	}
	if resolved.ReadScope != agent.FileReadWorkarea {
		t.Fatalf("read scope = %q", resolved.ReadScope)
	}
	wantPaths := []string{filepath.Join(dotfiles, "gitconfig"), absent}
	sort.Strings(wantPaths)
	if strings.Join(resolved.ReadPaths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("read paths = %v, want %v (a link resolves to its target, an absent path stays, repeats fold)", resolved.ReadPaths, wantPaths)
	}
	allow := resolved.ReadAllowlist()
	for _, want := range append([]string{w.mut, w.state, w.tmp, w.cache, w.ro}, wantPaths...) {
		if !slices.Contains(allow, want) {
			t.Errorf("read allowlist lacks %s: %v", want, allow)
		}
	}
	for _, never := range []string{w.ws, w.home, w.stateHome, filepath.Join(w.ws, ".workarea")} {
		if slices.Contains(allow, never) {
			t.Errorf("read allowlist names %s", never)
		}
	}
	if !sort.StringsAreSorted(allow) {
		t.Errorf("read allowlist is not sorted: %v", allow)
	}

	for _, open := range []agent.ExecutionSecurityLevel{"", agent.FileReadHost} {
		spec := w.spec()
		spec.ReadScope = open
		resolved, err := resolveSpec(spec, w.guards(), evalSymlinks)
		if err != nil || resolved.ReadScope != "" {
			t.Fatalf("read scope %q: resolved %q, err %v; want open reads", open, resolved.ReadScope, err)
		}
	}
}
