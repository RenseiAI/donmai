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
		Home:         w.home,
		StateHome:    w.stateHome,
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
// session, failing the test on any refusal. It passes no stage executable:
// the dedicated Wrap test pins the self bind, and the boundary shape below
// does not depend on it.
func mustRenderBubblewrap(t *testing.T, r *Resolved, rules []Rule) []string {
	t.Helper()
	args, err := renderBubblewrap(r, rules, linuxIdentity, "")
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
// binds of the OS userland and the host directories, the writable set
// over them, the device tree, the declared sockets, the denies after the
// allows so narrower rules win, and the self-mark last. It never unshares
// the pid namespace: the harness stays supervised under the outer launcher
// pid. Denies render only where host content would otherwise show through
// (strictly inside a writable root): top-level leaves, the metadata, the
// workarea root and the shared locations stay hidden behind the tmpfs root
// instead, so a refused listing fails instead of succeeding on an empty
// overlay.
func TestRenderBubblewrap_BindOrder(t *testing.T) {
	w := newLinuxWorld(t)
	text := joinArgs(mustRenderBubblewrap(t, w.resolved(), nil))
	order := []string{
		"--unshare-user",
		"--tmpfs\n/\n",
		"--ro-bind\n/usr\n/usr",
		"--ro-bind\n" + w.home + "\n" + w.home,
		"--tmpfs\n/dev\n",
		"--dev-bind\n/dev/null\n/dev/null",
		"--bind\n" + w.mut + "\n" + w.mut,
		"--bind\n" + w.tmp + "\n" + w.tmp,
		"--ro-bind\n" + w.sock + "\n" + w.sock,
		"--ro-bind\n/tmp/donmai-confine-empty/dir\n" + w.ext,
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
	// The protected artifact sits inside harness state, so its overlay
	// renders; nothing else is overlaid.
	if !strings.Contains(text, "--ro-bind\n/tmp/donmai-confine-empty/dir\n"+w.ext) {
		t.Fatalf("deny overlay for %q is not the empty placeholder:\n%s", w.ext, text)
	}
	for _, target := range []string{w.ro, w.ws, filepath.Join(w.ws, ".workarea"), "/var/tmp", w.mut, w.state} {
		if strings.Contains(text, "--ro-bind\n/tmp/donmai-confine-empty/dir\n"+target+"\n") {
			t.Fatalf("rendering overlays %q, which the tmpfs root already hides (or which carries a writable bind):\n%s", target, text)
		}
	}
	// The workarea root is reachable but never mounted: no bind names it.
	for _, triple := range bindTargets(mustRenderBubblewrap(t, w.resolved(), nil)) {
		if triple[2] == w.ws {
			t.Fatalf("rendering mounts the workarea root %q, burying or opening the leaves:\n%s", w.ws, text)
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
// writable set is overlaid after the writable allow, its inside ancestors
// are pinned, a deny over a missing or outside path emits no overlay at
// all, a deny covering a whole writable root refuses instead of being
// dropped or shadowing the bind, and a service rule refuses the whole
// rendering instead of being dropped.
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
	// A deny outside the writable set is hidden by the tmpfs root and
	// denied by the Landlock stage: no overlay renders.
	outside := filepath.Join(w.base, "elsewhere")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	text = joinArgs(mustRenderBubblewrap(t, w.resolved(), []Rule{
		{Kind: RuleDenyWrite, Path: outside, Scope: ScopeSubtree},
	}))
	if strings.Contains(text, outside) {
		t.Fatalf("rendering overlays the outside deny %q the tmpfs root already hides:\n%s", outside, text)
	}
	for _, rule := range []Rule{
		{Kind: RuleDenyServiceLookup, Service: "com.example.agent"},
		{Kind: "allow_write", Path: "/x", Scope: ScopeSubtree},
		{Kind: RuleDenyWrite, Path: "relative", Scope: ScopeSubtree},
		{Kind: RuleDenyWrite, Path: "/x", Scope: "glob"},
		{Kind: RuleDenyWrite, Path: w.mut, Scope: ScopeSubtree},
		{Kind: RuleDenyRead, Path: w.state, Scope: ScopeLiteral},
	} {
		if _, err := renderBubblewrap(w.resolved(), []Rule{rule}, linuxIdentity, ""); err == nil {
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
	if _, err := renderBubblewrap(r, nil, linuxIdentity, ""); err == nil {
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
		// Files grant their parent directory: the kernel's path-beneath
		// rule takes a directory descriptor, and the mount tree scopes
		// what the grant can reach.
		"allow-read " + w.rp + "\n",
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

// TestParseLandlockStage_RoundTrip pins the describe/parse round trip the
// stage runs live: every policy line, including the valueless device flag,
// must survive into the applied stage, and ports riding both the policy
// text and the invocation must land once. A dropped allow-dev silently
// ungrants the private device tree the mount tree binds.
func TestParseLandlockStage_RoundTrip(t *testing.T) {
	w := newLinuxWorld(t)
	r := w.resolved()
	r.ReadScope = agent.FileReadWorkarea
	r.ReadPaths = []string{filepath.Join(w.rp, "gitconfig")}
	r.LoopbackTCPPorts = []int{1234}
	built, err := buildLandlockStage(r)
	if err != nil {
		t.Fatalf("buildLandlockStage: %v", err)
	}
	if !built.dev {
		t.Fatal("buildLandlockStage left the device tree ungranted")
	}
	args := append([]string{"/self", "stage"}, append(stagePortArgs(built), "--", "/bin/sh")...)
	parsed, argv, err := parseLandlockStage(built.describe(), args)
	if err != nil {
		t.Fatalf("parseLandlockStage: %v", err)
	}
	if !parsed.dev {
		t.Fatal("the parsed stage dropped the device grant; the private /dev would refuse writes")
	}
	if len(parsed.tcpPorts) != 1 || parsed.tcpPorts[0] != 1234 {
		t.Fatalf("parsed ports = %v, want exactly [1234]", parsed.tcpPorts)
	}
	if len(parsed.writeRoots) != len(built.writeRoots) {
		t.Fatalf("parsed write roots = %v, want %v", parsed.writeRoots, built.writeRoots)
	}
	// The stage and harness executables gain traverse-and-execute grants
	// on their directories, so binaries outside every other grant run.
	if len(parsed.execRoots) == 0 {
		t.Fatal("the parsed stage grants no executable directory; an uncovered harness could not exec")
	}
	if len(argv) != 1 || argv[0] != "/bin/sh" {
		t.Fatalf("parsed harness argv = %v", argv)
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

// linuxApplyWorld is an Apply-level session: a fake launcher executable (so
// the test needs no bubblewrap on PATH) and the Landlock probe stubbed to
// present, so the test runs on any Linux kernel, including one without
// Landlock or user namespaces.
type linuxApplyWorld struct {
	world    linuxWorld
	backend  *mountNamespaceBackend
	resolved *Resolved
}

func newLinuxApplyWorld(t *testing.T) linuxApplyWorld {
	t.Helper()
	w := newLinuxWorld(t)
	launcher := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec \"$@\"\n"), 0o700); err != nil { //nolint:gosec // G306: a stand-in launcher executable.
		t.Fatalf("launcher seed: %v", err)
	}
	t.Setenv("PATH", "")
	oldProbe := landlockProbe
	landlockProbe = func() bool { return true }
	t.Cleanup(func() { landlockProbe = oldProbe })
	b := &mountNamespaceBackend{launcherOverride: launcher}
	resolved, err := resolveSpec(linuxSpec(w), linuxGuards(w), linuxIdentity)
	if err != nil {
		t.Fatalf("resolveSpec: %v", err)
	}
	return linuxApplyWorld{world: w, backend: b, resolved: resolved}
}

// TestApply_WrapBindsStageAndHarnessExecutables drives the production spawn
// path below Check: Apply renders the mount tree with the stage executable
// bound read-only, and Wrap splices the harness binary bind into the tree
// before the first "--", then invokes the stage with the declared ports
// before the harness argv. It uses bindTargets, the render-triple helper,
// so the assertion reads the boundary rather than substrings.
func TestApply_WrapBindsStageAndHarnessExecutables(t *testing.T) {
	fx := newLinuxApplyWorld(t)
	w := fx.world
	fx.resolved.LoopbackTCPPorts = []int{1234, 8080}
	self, err := stageSelfExecutable()
	if err != nil {
		t.Fatalf("stageSelfExecutable: %v", err)
	}
	applied, err := fx.backend.Apply(ApplyRequest{Resolved: fx.resolved})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	harness := filepath.Join(w.base, "harness")
	if err := os.WriteFile(harness, []byte("#!/bin/sh\ntrue\n"), 0o700); err != nil { //nolint:gosec // G306: a stand-in harness executable.
		t.Fatalf("harness seed: %v", err)
	}
	wrapped := mustWrap(t, applied, []string{harness, "-c", "true"})
	// Split the wrapped argv at the two "--" separators: the launcher and
	// tree, the stage invocation, and the harness argv.
	first, rest := splitSeparator(wrapped)
	second, harnessArgv := splitSeparator(rest)
	if len(first) == 0 || first[0] != fx.backend.launcherOverride {
		t.Fatalf("wrapped argv does not start with the resolved launcher:\n%v", wrapped)
	}
	if len(harnessArgv) != 3 || harnessArgv[0] != harness {
		t.Fatalf("harness argv = %v, want the inner command last", harnessArgv)
	}
	if len(second) != 4 || second[0] != self || second[1] != "stage" {
		t.Fatalf("stage invocation = %v, want [self stage --tcp ports]", second)
	}
	if second[2] != "--tcp" || second[3] != "1234,8080" {
		t.Fatalf("stage ports = %v, want [--tcp 1234,8080]", second)
	}
	// The tree binds the stage executable read-only by exact path (with
	// its parent created first), and Wrap spliced the harness binary bind
	// in after it.
	triples := bindTargets(first[1:])
	if !hasTriple(triples, "--ro-bind", self, self) {
		t.Fatalf("tree never binds the stage executable %q:\n%v", self, first)
	}
	if !hasTriple(triples, "--dir", filepath.Dir(self), "") {
		t.Fatalf("tree never creates the stage parent %q:\n%v", filepath.Dir(self), first)
	}
	if !hasTriple(triples, "--ro-bind", harness, harness) {
		t.Fatalf("tree never binds the harness binary %q:\n%v", harness, first)
	}
	if !hasTriple(triples, "--dir", filepath.Dir(harness), "") {
		t.Fatalf("tree never creates the harness parent %q:\n%v", filepath.Dir(harness), first)
	}
	// The boundary marker closes the rendered tree; Wrap spliced the
	// harness binary bind in after it.
	markerAt := -1
	for i := 0; i+2 < len(first); i++ {
		if first[i] == "--setenv" && first[i+1] == mountNamespaceMarkEnv && first[i+2] == "1" {
			markerAt = i
		}
	}
	if markerAt < 0 {
		t.Fatalf("tree never sets the boundary marker:\n%v", first)
	}
	harnessBindAt := -1
	for i := 0; i+2 < len(first); i++ {
		if first[i] == "--ro-bind" && first[i+1] == harness && first[i+2] == harness {
			harnessBindAt = i
		}
	}
	if harnessBindAt < markerAt {
		t.Fatalf("the harness bind does not follow the rendered tree:\n%v", first)
	}
	// Environ carries the stage policy the stage reads back.
	env := applied.Environ()
	if len(env) != 1 || !strings.HasPrefix(env[0], landlockStageEnv+"=") {
		t.Fatalf("Environ = %v, want the stage policy binding", env)
	}
	for _, want := range []string{"allow-write " + w.mut + "\n", "allow-tcp 1234\n", "allow-tcp 8080\n"} {
		if !strings.Contains(env[0], want) {
			t.Errorf("stage policy lacks %q:\n%s", want, env[0])
		}
	}
}

// TestApply_WrapSkipsRedundantExecutableBinds: a harness binary identical
// to the stage, or inside the writable set, is already reachable and gains
// no second bind — and a read-only shadow over a writable root must never
// render. The assertion compares the wrapped tree against a fresh render:
// Wrap must add nothing.
func TestApply_WrapSkipsRedundantExecutableBinds(t *testing.T) {
	fx := newLinuxApplyWorld(t)
	w := fx.world
	self, err := stageSelfExecutable()
	if err != nil {
		t.Fatalf("stageSelfExecutable: %v", err)
	}
	applied, err := fx.backend.Apply(ApplyRequest{Resolved: fx.resolved})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	tree, err := renderBubblewrap(fx.resolved, nil, linuxIdentity, self)
	if err != nil {
		t.Fatalf("renderBubblewrap: %v", err)
	}
	want := append([]string{fx.backend.launcherOverride}, tree...)
	for _, argv := range [][]string{{self, "stage", "--", "/bin/sh"}, {filepath.Join(w.mut, "tool"), "--version"}} {
		wrapped := mustWrap(t, applied, argv)
		first, _ := splitSeparator(wrapped)
		if strings.Join(first, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("Wrap added a bind for the already-reachable executable %q:\n%v", argv[0], wrapped)
		}
	}
}

// TestApply_LandlockGateRefusesClosed: the Landlock availability gate pins
// both branches — an unavailable kernel refuses with namespace_unavailable
// instead of staging a weaker boundary, and a present one proceeds.
func TestApply_LandlockGateRefusesClosed(t *testing.T) {
	fx := newLinuxApplyWorld(t)
	oldProbe := landlockProbe
	landlockProbe = func() bool { return false }
	defer func() { landlockProbe = oldProbe }()
	if _, err := fx.backend.Apply(ApplyRequest{Resolved: fx.resolved}); err == nil {
		t.Fatal("Apply staged a boundary without Landlock")
	} else if reason, _ := ReasonOf(err); reason != ReasonNamespaceUnavailable {
		t.Fatalf("Apply: err=%v, want namespace_unavailable", err)
	}
	landlockProbe = func() bool { return true }
	if _, err := fx.backend.Apply(ApplyRequest{Resolved: fx.resolved}); err != nil {
		t.Fatalf("Apply with Landlock present: %v", err)
	}
}

// TestPrepare_WrapShapeThroughConfiner drives the full production entry
// point — Confiner.Prepare through plan.Command — asserting the wrapped
// argv starts with the resolved launcher, invokes the stage, and ends with
// the harness. It needs user namespaces, so it skips honestly where the
// kernel or container disallows them.
func TestPrepare_WrapShapeThroughConfiner(t *testing.T) {
	w := newLinuxWorld(t)
	launcher := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec \"$@\"\n"), 0o700); err != nil { //nolint:gosec // G306: a stand-in launcher executable.
		t.Fatalf("launcher seed: %v", err)
	}
	oldProbe := landlockProbe
	landlockProbe = func() bool { return true }
	t.Cleanup(func() { landlockProbe = oldProbe })
	c, err := New(Options{
		Backend:          &mountNamespaceBackend{launcherOverride: launcher},
		ProfileDir:       w.profileDir,
		Home:             w.home,
		StateHome:        w.stateHome,
		ExecutableDigest: "sha256:test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	both := []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled}
	current, err := c.fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	c.store(SelfTestRecord{
		Backend: current.backend, BackendVersion: current.backendVersion, ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: current.executableDigest, SessionModes: both, Passed: true,
		Probes: []ProbeOutcome{{ID: "p", Pass: true}},
	})
	spec := linuxSpec(w)
	spec.LoopbackTCPPorts = []int{1234}
	plan, err := c.Prepare(spec)
	if err != nil {
		if reason, _ := ReasonOf(err); reason == ReasonNamespaceUnavailable || reason == ReasonNestedSandbox {
			t.Skipf("user namespaces unavailable here: %v", err)
		}
		t.Fatalf("Prepare: %v", err)
	}
	argv, err := plan.Command([]string{"/bin/sh", "-c", "true"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if len(argv) < 6 || argv[0] != launcher || argv[len(argv)-3] != "/bin/sh" {
		t.Fatalf("wrapped command = %v, want [launcher tree -- self stage -- harness]", argv)
	}
	joined := "\n" + strings.Join(argv, "\n") + "\n"
	self, err := stageSelfExecutable()
	if err != nil {
		t.Fatalf("stageSelfExecutable: %v", err)
	}
	for _, want := range []string{"\n--\n", "\n" + self + "\nstage\n", "\n--tcp\n1234\n", "\n/bin/sh\n-c\ntrue\n"} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrapped command lacks %q:\n%v", want, argv)
		}
	}
	if env := plan.Environment(); len(env) == 0 {
		t.Fatal("plan carries no environment bindings")
	}
	if got := plan.Record(); got.Backend != BackendLinuxMountNamespace {
		t.Fatalf("record backend = %q, want the mount-namespace backend", got.Backend)
	}
}

// TestRunLandlockStageFromEnv_ReachesTheStage: the dispatcher the spawn
// chain calls must be the real stage, not the other-OS stub. With the stage
// marker set but no stage marker in argv, the real stage reaches its parser
// and refuses (handled); the stub would report unhandled.
func TestRunLandlockStageFromEnv_ReachesTheStage(t *testing.T) {
	t.Setenv(landlockStageEnv, "landlock-stage\nallow-tcp 1\n")
	handled, code := RunLandlockStageFromEnv()
	if !handled || code == 0 {
		t.Fatalf("RunLandlockStageFromEnv = (%v, %d), want the stage to handle the marker and refuse the malformed invocation", handled, code)
	}
	t.Setenv(landlockStageEnv, "")
	if handled, _ := RunLandlockStageFromEnv(); handled {
		t.Fatal("RunLandlockStageFromEnv handled an unset marker")
	}
}

// TestCheck_RefusesNestedSandbox: a worker already inside a boundary —
// marked by the helper — refuses with nested_sandbox instead of building a
// second boundary and reporting the inner level.
func TestCheck_RefusesNestedSandbox(t *testing.T) {
	t.Setenv(mountNamespaceMarkEnv, "1")
	b := &mountNamespaceBackend{}
	if err := b.Check(); err == nil {
		t.Fatal("Check built a boundary inside a boundary")
	} else if reason, _ := ReasonOf(err); reason != ReasonNestedSandbox {
		t.Fatalf("Check: err=%v, want nested_sandbox", err)
	}
}

// TestEmptyPlaceholder_Permissions: the deny overlays are empty by
// construction and stay owner-only, so a denied path's placeholder leaks
// nothing to other users.
func TestEmptyPlaceholder_Permissions(t *testing.T) {
	w := newLinuxWorld(t)
	dir, isDir, err := emptyPlaceholder(w.ro)
	if err != nil {
		t.Fatalf("emptyPlaceholder(dir): %v", err)
	}
	if !isDir {
		t.Fatalf("emptyPlaceholder(%q) isDir = false, want true", w.ro)
	}
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat placeholder: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("placeholder dir mode = %o, want 700", info.Mode().Perm())
	}
	seed := filepath.Join(w.ro, "f")
	if err := os.WriteFile(seed, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	file, isDir, err := emptyPlaceholder(seed)
	if err != nil {
		t.Fatalf("emptyPlaceholder(file): %v", err)
	}
	if isDir {
		t.Fatalf("emptyPlaceholder(%q) isDir = true, want false", seed)
	}
	if info, err := os.Stat(file); err != nil {
		t.Fatalf("stat placeholder: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("placeholder file mode = %o, want 600", info.Mode().Perm())
	}
	if _, _, err := emptyPlaceholder(filepath.Join(w.base, "absent")); err == nil {
		t.Fatal("emptyPlaceholder accepted a missing target")
	}
}

// TestRenderBubblewrap_SharedParentKeepsTheWritableBind: a session that
// lives under a shared parent keeps its writable bind — hiding the parent
// would bury it — and the shared parent itself is never mounted: behind
// the tmpfs root it reads as an empty directory, never as the host's
// listing. A shared location with no writable root near it stays hidden
// the same way, with no overlay and no bind.
func TestRenderBubblewrap_SharedParentKeepsTheWritableBind(t *testing.T) {
	w := newLinuxWorld(t)
	text := joinArgs(mustRenderBubblewrap(t, w.resolved(), nil))
	if !strings.Contains(text, "--bind\n"+w.tmp+"\n"+w.tmp) {
		t.Fatalf("rendering lost the session-tmp bind under its shared parent:\n%s", text)
	}
	for _, dir := range []string{"/tmp", "/var/tmp"} {
		for _, triple := range bindTargets(mustRenderBubblewrap(t, w.resolved(), nil)) {
			if triple[2] == dir || (triple[0] == "--dir" && triple[1] == dir) {
				t.Fatalf("rendering mounts the shared location %q:\n%s", dir, text)
			}
		}
	}
}

func mustWrap(t *testing.T, applied Applied, argv []string) []string {
	t.Helper()
	if applied.Wrap == nil {
		t.Fatal("Apply returned no Wrap")
	}
	return applied.Wrap(argv)
}

// splitSeparator splits argv at the first "--", returning the head and the
// tail after it. It fails the test when no separator is present.
func splitSeparator(argv []string) (head, tail []string) {
	for i, arg := range argv {
		if arg == "--" {
			return argv[:i], argv[i+1:]
		}
	}
	return argv, nil
}

// hasTriple reports whether triples holds (flag, source, target).
func hasTriple(triples [][3]string, flag, source, target string) bool {
	for _, triple := range triples {
		if triple == [3]string{flag, source, target} {
			return true
		}
	}
	return false
}
