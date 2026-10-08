//go:build linux

package confinement

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// linuxWorld is a render-test session on real temporary directories: every
// writable root, read-only leaf, protected path and socket exists, so the
// render exercises the boundary rather than the setup. It mirrors
// newSpecWorld's shape (mutable leaf with its own .git, harness state with
// a protected artifact inside, session tmp and cache, one read-only leaf).
type linuxWorld struct {
	base, home, stateHome, profileDir             string
	ws, mut, ro, state, ext, tmp, cache, sock, rp string
}

func newLinuxWorld(t *testing.T) linuxWorld {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	w := linuxWorld{base: base}
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
	w.sock = filepath.Join(w.tmp, "agent.sock")
	w.rp = filepath.Join(base, "dotfiles")
	for _, dir := range []string{
		w.profileDir, filepath.Join(w.mut, ".git"), w.ro, w.ext,
		w.tmp, w.cache, w.rp, filepath.Join(w.ws, ".workarea"),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if err := os.WriteFile(w.sock, []byte{}, 0o600); err != nil {
		t.Fatalf("socket seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(w.rp, "gitconfig"), []byte("[user]\n"), 0o600); err != nil {
		t.Fatalf("read path seed: %v", err)
	}
	return w
}

func (w linuxWorld) resolved() *Resolved {
	return &Resolved{
		SessionID:    "s1",
		HarnessID:    "h1",
		WorkareaRoot: w.ws,
		MetadataDir:  filepath.Join(w.ws, ".workarea"),
		Writable: []WritableRoot{
			{Path: w.mut, Class: ClassMutableLeaf},
			{Path: w.state, Class: ClassHarnessState},
			{Path: w.tmp, Class: ClassSessionTmp},
			{Path: w.cache, Class: ClassSessionCache},
		},
		ReadOnly:   []string{w.ro},
		Protected:  []string{w.ext},
		Pins:       []string{w.mut, w.state},
		Sockets:    []string{w.sock},
		SessionTmp: w.tmp,
	}
}

func linuxIdentity(path string) (string, error) { return path, nil }

// mustRenderBubblewrap renders the Linux mount-tree arguments for a real
// session, failing the test on any refusal.
func mustRenderBubblewrap(t *testing.T, r *Resolved, rules []Rule) []string {
	t.Helper()
	args, err := renderBubblewrap(r, rules, linuxIdentity)
	if err != nil {
		t.Fatalf("renderBubblewrap: %v", err)
	}
	return args
}

func joinArgs(args []string) string { return "\n" + strings.Join(args, "\n") + "\n" }

// bindTargets returns the (flag, source, target) triples of a rendering.
func bindTargets(args []string) [][3]string {
	var out [][3]string
	for i := 0; i+2 < len(args); i++ {
		switch args[i] {
		case "--bind", "--ro-bind", "--dev-bind", "--symlink", "--dir", "--tmpfs", "--proc":
			out = append(out, [3]string{args[i], args[i+1], ""})
			if args[i] == "--bind" || args[i] == "--ro-bind" || args[i] == "--dev-bind" || args[i] == "--symlink" {
				out[len(out)-1][2] = args[i+2]
				i += 2
			} else {
				i++
			}
		}
	}
	return out
}

// TestRenderBubblewrap_BindOrder pins the order the boundary depends on:
// the user namespace first, the tmpfs root hiding everything, read-only
// binds of the OS userland, the writable set over them, the device tree,
// the declared sockets, the denies after the allows so narrower rules win,
// and the self-mark last. It never unshares the pid namespace: the harness
// stays supervised under the outer launcher pid.
func TestRenderBubblewrap_BindOrder(t *testing.T) {
	w := newLinuxWorld(t)
	text := joinArgs(mustRenderBubblewrap(t, w.resolved(), nil))
	order := []string{
		"--unshare-user",
		"--tmpfs\n/\n",
		"--ro-bind\n/usr\n/usr",
		"--tmpfs\n/dev\n",
		"--dev-bind\n/dev/null\n/dev/null",
		"--bind\n" + w.mut + "\n" + w.mut,
		"--bind\n" + w.tmp + "\n" + w.tmp,
		"--ro-bind\n" + w.sock + "\n" + w.sock,
		"--dir\n" + w.ws + "\n--dir\n" + w.ro,
		"--dir\n" + w.ws + "\n--ro-bind",
		"--setenv\n" + mountNamespaceMarkEnv + "\n1",
	}
	last := -1
	for _, needle := range order {
		at := strings.Index(text, needle)
		if at < 0 {
			t.Fatalf("rendering lacks %q:\n%s", needle, text)
		}
		if at <= last {
			t.Fatalf("%q is out of order:\n%s", needle, text)
		}
		last = at
	}
	for _, target := range []string{w.ro, w.ext, w.ws} {
		if !strings.Contains(text, "--ro-bind\n/tmp/donmai-confine-empty/dir\n"+target) {
			t.Fatalf("deny overlay for %q is not the empty placeholder:\n%s", target, text)
		}
	}
	if strings.Contains(text, "--unshare-pid") {
		t.Fatalf("rendering unshares the pid namespace; the harness must stay supervised:\n%s", text)
	}
	if strings.Contains(text, "--ro-bind\n/proc\n/proc") || strings.Contains(text, "--ro-bind\n/sys\n/sys") {
		t.Fatalf("rendering binds the host /proc or /sys; use the fresh mounts:\n%s", text)
	}
}

// TestRenderBubblewrap_AllowsOnlyTheDeclaredSet: the only read-write binds
// are the declared writable roots; device nodes ride --dev-bind, never
// --bind.
func TestRenderBubblewrap_AllowsOnlyTheDeclaredSet(t *testing.T) {
	w := newLinuxWorld(t)
	args := mustRenderBubblewrap(t, w.resolved(), nil)
	var writable []string
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+2] == args[i+1] {
			writable = append(writable, args[i+1])
		}
	}
	want := map[string]bool{w.mut: true, w.state: true, w.tmp: true, w.cache: true}
	if len(writable) != len(want) {
		t.Fatalf("writable binds = %v, want exactly %v", writable, want)
	}
	for _, path := range writable {
		if !want[path] {
			t.Errorf("writable bind names an undeclared path: %s", path)
		}
	}
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && strings.HasPrefix(args[i+1], "/dev/") {
			t.Errorf("--bind carries a device node %q; use --dev-bind", args[i+1])
		}
	}
}

// TestRenderBubblewrap_ComposerRulesWinLast: a composer deny inside the
// writable set is overlaid after the writable allow, its ancestors are
// pinned, a deny over a missing path emits no self-bind, and a service rule
// refuses the whole rendering instead of being dropped.
func TestRenderBubblewrap_ComposerRulesWinLast(t *testing.T) {
	w := newLinuxWorld(t)
	vendor := filepath.Join(w.mut, "vendor", "pkg")
	if err := os.MkdirAll(vendor, 0o750); err != nil {
		t.Fatal(err)
	}
	args := mustRenderBubblewrap(t, w.resolved(), []Rule{
		{Kind: RuleDenyWrite, Path: vendor, Scope: ScopeSubtree},
	})
	text := joinArgs(args)
	allowAt := strings.Index(text, "--bind\n"+w.mut+"\n"+w.mut)
	denyAt := strings.Index(text, vendor)
	if denyAt < 0 {
		t.Fatalf("rendering lacks the composer deny:\n%s", text)
	}
	if denyAt < allowAt {
		t.Fatalf("the composer deny renders before the writable allow; the allow would win:\n%s", text)
	}
	if !strings.Contains(text, filepath.Join(w.mut, "vendor")) {
		t.Fatalf("the composer deny's ancestors are not pinned:\n%s", text)
	}
	// A deny over a path that does not exist emits no bind at all: the
	// tmpfs root already hides it, and a self-bind would fail the spawn.
	missing := filepath.Join(w.mut, "not-yet-created")
	args = mustRenderBubblewrap(t, w.resolved(), []Rule{
		{Kind: RuleDenyWrite, Path: missing, Scope: ScopeSubtree},
	})
	for i := 0; i+2 < len(args); i++ {
		if (args[i] == "--bind" || args[i] == "--ro-bind") && args[i+1] == missing && args[i+2] == missing {
			t.Fatalf("rendering binds missing path %q onto itself; the spawn would fail", missing)
		}
	}
	for _, rule := range []Rule{
		{Kind: RuleDenyServiceLookup, Service: "com.example.agent"},
		{Kind: "allow_write", Path: "/x", Scope: ScopeSubtree},
		{Kind: RuleDenyWrite, Path: "relative", Scope: ScopeSubtree},
		{Kind: RuleDenyWrite, Path: "/x", Scope: "glob"},
	} {
		if _, err := renderBubblewrap(w.resolved(), []Rule{rule}, linuxIdentity); err == nil {
			t.Errorf("rule %+v rendered; want rule_unrenderable", rule)
		} else if reason, _ := ReasonOf(err); reason != ReasonRuleUnrenderable {
			t.Errorf("rule %+v: err=%v, want rule_unrenderable", rule, err)
		}
	}
}

// TestRenderBubblewrap_DeclaredPortsRideTheStage: declared loopback ports
// render (no refusal), and the stage policy carries exactly them as TCP
// allow rules — nothing is allowed by default.
func TestRenderBubblewrap_DeclaredPortsRideTheStage(t *testing.T) {
	w := newLinuxWorld(t)
	r := w.resolved()
	r.LoopbackTCPPorts = []int{1234, 8080}
	mustRenderBubblewrap(t, r, nil)
	stage, err := buildLandlockStage(r)
	if err != nil {
		t.Fatalf("buildLandlockStage: %v", err)
	}
	if !slices.Equal(stage.tcpPorts, []int{1234, 8080}) {
		t.Fatalf("stage ports = %v, want [1234 8080]", stage.tcpPorts)
	}
	text := stage.describe()
	for _, want := range []string{"allow-tcp 1234\n", "allow-tcp 8080\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("stage policy lacks %q:\n%s", want, text)
		}
	}
	// No declared ports: the policy names no allow-tcp rule, so every
	// loopback connect fails closed.
	empty, err := buildLandlockStage(w.resolved())
	if err != nil {
		t.Fatalf("buildLandlockStage: %v", err)
	}
	if strings.Contains(empty.describe(), "allow-tcp") {
		t.Fatalf("portless stage policy allows a TCP port:\n%s", empty.describe())
	}
	if got := stagePortArgs(empty); len(got) != 0 {
		t.Fatalf("portless stage argv carries port flags: %v", got)
	}
}

// TestRenderBubblewrap_ReadScopeKeepsTheSetWritable: under the workarea read
// scope the writable set stays read-write (the probe writes its read-pass
// results into session tmp), while read-only leaves and declared read paths
// bind read-only last; an unknown scope is refused, never rendered as open
// reads.
func TestRenderBubblewrap_ReadScopeKeepsTheSetWritable(t *testing.T) {
	w := newLinuxWorld(t)
	r := w.resolved()
	r.ReadScope = agent.FileReadWorkarea
	r.ReadPaths = []string{filepath.Join(w.rp, "gitconfig")}
	text := joinArgs(mustRenderBubblewrap(t, r, nil))
	for _, want := range []string{
		"--ro-bind\n" + w.ro + "\n" + w.ro,
		"--ro-bind\n" + filepath.Join(w.rp, "gitconfig") + "\n" + filepath.Join(w.rp, "gitconfig"),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendering lacks %q:\n%s", want, text)
		}
	}
	for _, root := range []string{w.mut, w.state, w.tmp, w.cache} {
		if strings.Contains(text, "--ro-bind\n"+root+"\n"+root) {
			t.Errorf("rendering re-binds writable root %q read-only; the write pass could never land:\n%s", root, text)
		}
	}
	plain := joinArgs(mustRenderBubblewrap(t, w.resolved(), nil))
	if strings.Contains(plain, "--ro-bind\n"+w.ro+"\n"+w.ro) {
		t.Fatalf("a session without a read scope binds its allowlist read-only:\n%s", plain)
	}
	r.ReadScope = "home-minus-secrets"
	if _, err := renderBubblewrap(r, nil, linuxIdentity); err == nil {
		t.Fatal("an unrendered read scope was rendered as open reads")
	}
}

// TestRenderBubblewrap_SocketsStayConnectable: no --ro-bind names a
// declared socket target... no: sockets ARE ro-bound (a connect through a
// read-only bind succeeds); what must never happen is a socket left
// entirely unbound. This test pins the bind the F1 fix keeps.
func TestRenderBubblewrap_SocketsAreBound(t *testing.T) {
	w := newLinuxWorld(t)
	text := joinArgs(mustRenderBubblewrap(t, w.resolved(), nil))
	if !strings.Contains(text, "--ro-bind\n"+w.sock+"\n"+w.sock) {
		t.Fatalf("rendering does not bind the declared socket %q:\n%s", w.sock, text)
	}
}

// TestRenderBubblewrap_MissingLauncherRefuses: Apply with no launcher
// refuses with backend_absent instead of rendering a boundary it cannot
// launch. It drives the production entry point below Check (Apply), so the
// test holds where user namespaces are unavailable too.
func TestRenderBubblewrap_MissingLauncherRefuses(t *testing.T) {
	w := newLinuxWorld(t)
	b := &mountNamespaceBackend{launcherOverride: "/nonexistent/bwrap-test-launcher"}
	resolved, err := resolveSpec(linuxSpec(w), linuxGuards(w), linuxIdentity)
	if err != nil {
		t.Fatalf("resolveSpec: %v", err)
	}
	if _, err := b.Apply(ApplyRequest{Resolved: resolved}); err == nil {
		t.Fatal("Apply returned a plan with no launcher present")
	} else if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
		t.Fatalf("Apply: err=%v, want backend_absent", err)
	}
}

// TestRenderBubblewrap_LauncherResolvesAbsolute: the resolved launcher is
// absolute, so the spawn cannot be redirected by a later PATH change.
func TestRenderBubblewrap_LauncherResolvesAbsolute(t *testing.T) {
	if _, err := resolveLauncher("/nonexistent/bwrap-test-launcher"); err == nil {
		t.Fatal("resolveLauncher accepted a missing absolute launcher")
	} else if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
		t.Fatalf("err=%v, want backend_absent", err)
	}
}

func linuxSpec(w linuxWorld) Spec {
	return Spec{
		SessionID:      "s1",
		HarnessID:      "h1",
		SessionMode:    agent.PromptModeAutonomous,
		WorkareaRoot:   w.ws,
		MutableLeaves:  []string{w.mut},
		HarnessState:   []string{w.state},
		SessionTmp:     w.tmp,
		Caches:         []Cache{{Env: "GOCACHE", Dir: w.cache}},
		ReadOnlyLeaves: []string{w.ro},
		Protected:      []string{w.ext},
		Sockets:        []string{w.sock},
	}
}

func linuxGuards(w linuxWorld) guards {
	return guards{home: w.home, stateHome: w.stateHome, profileDir: w.profileDir}
}

// TestBackendLinux_NameAndResolvers pins the attestation name and the
// resolver declaration the adapter binds into the boundary.
func TestBackendLinux_NameAndResolvers(t *testing.T) {
	b := &mountNamespaceBackend{}
	if b.Name() != BackendLinuxMountNamespace {
		t.Fatalf("Name = %q, want %q", b.Name(), BackendLinuxMountNamespace)
	}
	sockets := ResolverSockets()
	if len(sockets) != 1 || sockets[0] != "/etc/resolv.conf" {
		t.Fatalf("ResolverSockets = %v", sockets)
	}
	if _, err := b.Canonical("/tmp"); err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if _, err := b.Version(); err != nil {
		t.Fatalf("Version: %v", err)
	}
}

// TestLandlockStageDescribe pins the stage policy text: the writable set
// allows writes, the runtime binds allow reads, declared ports allow TCP,
// and nothing else is named.
func TestLandlockStageDescribe(t *testing.T) {
	w := newLinuxWorld(t)
	r := w.resolved()
	r.ReadScope = agent.FileReadWorkarea
	r.ReadPaths = []string{filepath.Join(w.rp, "gitconfig")}
	r.LoopbackTCPPorts = []int{1234}
	stage, err := buildLandlockStage(r)
	if err != nil {
		t.Fatalf("buildLandlockStage: %v", err)
	}
	text := stage.describe()
	for _, want := range []string{
		"allow-write " + w.mut + "\n",
		"allow-write " + w.tmp + "\n",
		"allow-read " + w.ro + "\n",
		"allow-read " + filepath.Join(w.rp, "gitconfig") + "\n",
		"allow-tcp 1234\n",
		"allow-dev\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("stage policy lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "allow-write "+w.ro) {
		t.Errorf("stage policy grants write on the read-only leaf:\n%s", text)
	}
}

// TestParseLandlockStage pins the stage invocation parser: the policy text
// splits into write/read roots, the ports parse in host order, and malformed
// invocations refuse.
func TestParseLandlockStage(t *testing.T) {
	w := newLinuxWorld(t)
	r := w.resolved()
	stage, err := buildLandlockStage(r)
	if err != nil {
		t.Fatalf("buildLandlockStage: %v", err)
	}
	args := []string{"/self", "stage", "--tcp", "8080", "--", "/bin/sh", "-c", "true"}
	got, argv, err := parseLandlockStage(stage.describe(), args)
	if err != nil {
		t.Fatalf("parseLandlockStage: %v", err)
	}
	if len(got.writeRoots) == 0 || len(got.readRoots) == 0 {
		t.Fatalf("parsed policy holds no roots: %+v", got)
	}
	found := false
	for _, root := range got.writeRoots {
		if root == w.mut {
			found = true
		}
	}
	if !found {
		t.Errorf("parsed write roots %v lack %q", got.writeRoots, w.mut)
	}
	if len(got.tcpPorts) != 1 || got.tcpPorts[0] != 8080 {
		t.Errorf("parsed ports = %v, want [8080]", got.tcpPorts)
	}
	if len(argv) != 3 || argv[0] != "/bin/sh" {
		t.Errorf("parsed harness argv = %v", argv)
	}
	for _, bad := range [][]string{
		{"/self", "exec", "--", "/bin/true"},
		{"/self", "stage", "--tcp"},
		{"/self", "stage", "--tcp", "99999", "--", "/bin/true"},
		{"/self", "stage", "--frobnicate", "--", "/bin/true"},
		{"/self", "stage", "--tcp", "80"},
	} {
		if _, _, err := parseLandlockStage(stage.describe(), bad); err == nil {
			t.Errorf("parseLandlockStage(%v) accepted a malformed invocation", bad)
		}
	}
	if _, _, err := parseLandlockStage("allow-tcp abc\n", []string{"/self", "stage", "--", "/bin/true"}); err == nil {
		t.Error("parseLandlockStage accepted a non-numeric port in the policy")
	}
}

// TestCheckLauncher pins the launcher gate: an absolute path that is not an
// executable refuses, and an empty PATH finds nothing.
func TestCheckLauncher(t *testing.T) {
	t.Setenv("PATH", "")
	if err := checkLauncher("bwrap"); err == nil {
		t.Fatal("checkLauncher found a launcher on an empty PATH")
	} else if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
		t.Fatalf("err=%v, want backend_absent", err)
	}
	if err := checkLauncher("/nonexistent/bwrap-test-launcher"); err == nil {
		t.Fatal("checkLauncher accepted a missing absolute launcher")
	} else if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
		t.Fatalf("err=%v, want backend_absent", err)
	}
}

// TestProbeUserNamespaceLeavesTheCaller: Check (and the user-namespace probe
// inside it) must not move the calling process into a new user namespace:
// the worker keeps its identity for the rest of its life.
func TestProbeUserNamespaceLeavesTheCaller(t *testing.T) {
	before, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		t.Skipf("no uid_map on this kernel: %v", err)
	}
	b := &mountNamespaceBackend{}
	if err := b.Check(); err != nil {
		if reason, _ := ReasonOf(err); reason == ReasonNamespaceUnavailable || reason == ReasonNestedSandbox {
			t.Skipf("user namespaces unavailable here: %v", err)
		}
		t.Fatalf("Check: %v", err)
	}
	after, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		t.Fatalf("uid_map after Check: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("Check moved the caller into a new user namespace:\nbefore %q\nafter  %q", before, after)
	}
}
