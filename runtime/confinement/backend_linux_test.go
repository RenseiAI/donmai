//go:build linux

package confinement

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

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
	w.sock = filepath.Join(base, "run", "agent.sock")
	w.rp = filepath.Join(base, "dotfiles")
	for _, dir := range []string{
		w.profileDir, filepath.Join(w.mut, ".git"), w.ro, w.ext,
		w.tmp, w.cache, w.rp, filepath.Join(w.ws, ".workarea"), filepath.Dir(w.sock),
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
// binds of the OS userland and the host directories, the declared sockets,
// the device tree, the writable set over them, the write denies bound
// read-only over themselves after the allows so narrower rules win, and
// the self-mark last. The process namespace is private, so no process
// outside the boundary is visible, and the outer launcher pid stays the
// one the supervisor signals. A write deny stays
// readable: the protected artifact inside harness state and the read-only
// leaf are bound read-only over themselves, never replaced by an empty
// placeholder, and the workarea root itself is never mounted.
func TestRenderBubblewrap_BindOrder(t *testing.T) {
	w := newLinuxWorld(t)
	args := mustRenderBubblewrap(t, w.resolved(), nil)
	text := joinArgs(args)
	order := []string{
		"--unshare-user",
		"--tmpfs\n/\n",
		"--ro-bind\n/usr\n/usr",
		"--ro-bind\n" + w.home + "\n" + w.home,
		"--tmpfs\n/dev\n",
		"--dev-bind\n/dev/null\n/dev/null",
		"--bind\n" + w.mut + "\n" + w.mut,
		"--bind\n" + w.tmp + "\n" + w.tmp,
		"--ro-bind\n" + w.ro + "\n" + w.ro,
		"--ro-bind\n" + w.ext + "\n" + w.ext,
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
	// No path is hidden behind an empty placeholder: nothing here is a
	// read deny, and a write deny must stay readable.
	if strings.Contains(text, "donmai-confine-empty") {
		t.Fatalf("rendering hides a write deny behind an empty placeholder; the harness could not read it:\n%s", text)
	}
	// With reads open the workarea metadata is readable and never
	// writable: bound read-only over itself.
	meta := filepath.Join(w.ws, ".workarea")
	if !hasTriple(bindTargets(args), "--ro-bind", meta, meta) {
		t.Fatalf("rendering does not hold the workarea metadata read-only:\n%s", text)
	}
	// The workarea root is reachable but never mounted: no bind names it.
	for _, triple := range bindTargets(args) {
		if triple[2] == w.ws {
			t.Fatalf("rendering mounts the workarea root %q, burying or opening the leaves:\n%s", w.ws, text)
		}
	}
	// A private process namespace (D3.1) and, for a headless session, a
	// session of its own; never a private network namespace, which would
	// strand the declared loopback ports and the external network.
	for _, flag := range []string{"--unshare-pid", "--new-session"} {
		if !strings.Contains(text, "\n"+flag+"\n") {
			t.Fatalf("headless rendering lacks %s:\n%s", flag, text)
		}
	}
	if strings.Contains(text, "--unshare-net") {
		t.Fatalf("rendering unshares the network namespace; the harness must keep its egress:\n%s", text)
	}
	// An interactive session keeps the PTY host's session: the harness
	// takes its terminal's foreground (the stage), which a new session
	// would leave it without.
	interactive := w.resolved()
	interactive.SessionMode = agent.PromptModeHumanControlled
	itext := joinArgs(mustRenderBubblewrap(t, interactive, nil))
	if !strings.Contains(itext, "\n--unshare-pid\n") || strings.Contains(itext, "\n--new-session\n") {
		t.Fatalf("interactive rendering must unshare the pid namespace and keep the session:\n%s", itext)
	}
	if strings.Contains(text, "--ro-bind\n/proc\n/proc") || strings.Contains(text, "--ro-bind\n/sys\n/sys") {
		t.Fatalf("rendering binds the host /proc or /sys; use the fresh mounts:\n%s", text)
	}
}

// TestRenderBubblewrap_RenameAnchors: an ancestor of a write deny strictly
// inside a writable root is bound read-write over itself — a mount point no
// rename can move — before the deny beneath it is bound read-only, so the
// later bind of the ancestor can never bury the deny. Writable roots are
// mount points already and gain no second bind.
func TestRenderBubblewrap_RenameAnchors(t *testing.T) {
	w := newLinuxWorld(t)
	nested := filepath.Join(w.mut, "pkg", "ext")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	r := w.resolved()
	r.Protected = []string{nested}
	r.Pins = ancestorPins([]string{w.mut, w.state, w.tmp, w.cache}, []string{nested})
	args := mustRenderBubblewrap(t, r, nil)
	text := joinArgs(args)
	anchor := filepath.Join(w.mut, "pkg")
	anchorAt := strings.Index(text, "--bind\n"+anchor+"\n"+anchor+"\n")
	denyAt := strings.Index(text, "--ro-bind\n"+nested+"\n"+nested+"\n")
	if anchorAt < 0 || denyAt < 0 {
		t.Fatalf("rendering lacks the anchor %q or the deny %q:\n%s", anchor, nested, text)
	}
	if denyAt < anchorAt {
		t.Fatalf("the anchor binds after the deny beneath it and buries it:\n%s", text)
	}
	count := strings.Count(text, "--bind\n"+w.mut+"\n"+w.mut+"\n")
	if count != 1 {
		t.Fatalf("writable root %q bound %d times, want once:\n%s", w.mut, count, text)
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
		// A read deny over the host state home would bury the writable
		// roots bound beneath it.
		{Kind: RuleDenyRead, Path: w.stateHome, Scope: ScopeSubtree},
	} {
		if _, err := renderBubblewrap(w.resolved(), []Rule{rule}, linuxIdentity, ""); err == nil {
			t.Errorf("rule %+v rendered; want rule_unrenderable", rule)
		} else if reason, _ := ReasonOf(err); reason != ReasonRuleUnrenderable {
			t.Errorf("rule %+v: err=%v, want rule_unrenderable", rule, err)
		}
	}
}

// TestRenderBubblewrap_DeclaredPortsRideTheStage: declared loopback ports
// render (no refusal), and the stage policy records exactly them — while
// granting nothing for them. Landlock port rules carry no address, so
// handling CONNECT_TCP would deny every undeclared port on any address,
// including the remote endpoints the contract leaves open; the stage
// therefore handles no network right, and loopback egress outside the
// declared ports is a documented gap, not a denied one.
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
	// No declared ports: the policy names no allow-tcp rule.
	empty, err := buildLandlockStage(w.resolved())
	if err != nil {
		t.Fatalf("buildLandlockStage: %v", err)
	}
	if strings.Contains(empty.describe(), "allow-tcp") {
		t.Fatalf("portless stage policy names a TCP port:\n%s", empty.describe())
	}
	if got := stagePortArgs(empty); len(got) != 0 {
		t.Fatalf("portless stage argv carries port flags: %v", got)
	}
}

// TestLandlockStage_LeavesTCPOpen pins the B1 fix at the production entry
// point: restrictLandlockFull builds a ruleset that handles no network
// right, so a confined seat keeps the TCP egress the contract leaves open
// (remote endpoints, ordinary fetches) however many ports the session
// declared. If the stage ever handles CONNECT_TCP again with only the
// declared-port allows, every connect to an undeclared port — on any
// address — fails, and this test goes red.

// TestRenderBubblewrap_ComposerWriteDenyStaysReadable: a composer write
// deny inside the writable set is bound read-only over itself — readable,
// not writable — after a read-write anchor on its ancestor, never hidden
// behind an empty placeholder that would also hide its siblings.
func TestRenderBubblewrap_ComposerWriteDenyStaysReadable(t *testing.T) {
	w := newLinuxWorld(t)
	pkg := filepath.Join(w.mut, "vendor", "pkg")
	if err := os.MkdirAll(pkg, 0o750); err != nil {
		t.Fatal(err)
	}
	args := mustRenderBubblewrap(t, w.resolved(), []Rule{{Kind: RuleDenyWrite, Path: pkg, Scope: ScopeSubtree}})
	triples := bindTargets(args)
	vendor := filepath.Join(w.mut, "vendor")
	if !hasTriple(triples, "--bind", vendor, vendor) {
		t.Fatalf("the write deny's ancestor is not anchored read-write:\n%s", joinArgs(args))
	}
	if !hasTriple(triples, "--ro-bind", pkg, pkg) {
		t.Fatalf("the write deny is not bound read-only over itself:\n%s", joinArgs(args))
	}
	if strings.Contains(joinArgs(args), "donmai-confine-empty") {
		t.Fatalf("a write deny hides its path or its siblings behind an empty placeholder:\n%s", joinArgs(args))
	}
}

// TestRenderBubblewrap_ComposerReadDenyHidesWhereRevealed: a composer read
// deny is hidden behind an empty placeholder wherever a bind would reveal
// it — inside the writable set, and inside the operator home the tree binds
// read-only for metadata (where, with reads open, the Landlock stage grants
// reads) — and renders nothing where no bind reveals it. Dropping it under
// the home would leave the path readable.
func TestRenderBubblewrap_ComposerReadDenyHidesWhereRevealed(t *testing.T) {
	w := newLinuxWorld(t)
	secret := filepath.Join(w.home, ".secrets")
	inside := filepath.Join(w.mut, "private")
	for _, dir := range []string{secret, inside} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := filepath.Join(w.base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o750); err != nil {
		t.Fatal(err)
	}
	args := mustRenderBubblewrap(t, w.resolved(), []Rule{
		{Kind: RuleDenyRead, Path: secret, Scope: ScopeSubtree},
		{Kind: RuleDenyRead, Path: inside, Scope: ScopeSubtree},
		{Kind: RuleDenyRead, Path: elsewhere, Scope: ScopeSubtree},
	})
	text := joinArgs(args)
	empty := filepath.Join(os.TempDir(), "donmai-confine-empty", "dir")
	for _, path := range []string{secret, inside} {
		if !hasTriple(bindTargets(args), "--ro-bind", empty, path) {
			t.Errorf("read deny %q is not hidden behind the empty placeholder:\n%s", path, text)
		}
	}
	if strings.Contains(text, elsewhere) {
		t.Errorf("read deny %q no bind reveals still renders:\n%s", elsewhere, text)
	}
	// A read deny inside another one is hidden with it: the placeholder
	// over the outer path is empty and read-only, so a second placeholder
	// could not take a mount point inside it and the spawn would fail.
	nested := filepath.Join(secret, "token")
	if err := os.WriteFile(nested, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	both := mustRenderBubblewrap(t, w.resolved(), []Rule{
		{Kind: RuleDenyRead, Path: nested, Scope: ScopeLiteral},
		{Kind: RuleDenyRead, Path: secret, Scope: ScopeSubtree},
	})
	for _, triple := range bindTargets(both) {
		if triple[2] == nested {
			t.Fatalf("a read deny inside a hidden path renders its own bind; the spawn would fail:\n%s", joinArgs(both))
		}
	}
	if !hasTriple(bindTargets(both), "--ro-bind", empty, secret) {
		t.Fatalf("the outer read deny %q is not hidden:\n%s", secret, joinArgs(both))
	}
	// Hides render after every reveal: the last placeholder follows the
	// last read-only self-bind.
	if strings.LastIndex(text, "--ro-bind\n"+w.ext+"\n") > strings.Index(text, "--ro-bind\n"+empty+"\n") {
		t.Fatalf("a hide renders before a later reveal could bury it:\n%s", text)
	}
}

// TestRenderBubblewrap_ReadScopeKeepsTheSetWritable: under the workarea read
// scope the writable set stays read-write (the probe writes its read-pass
// results into session tmp), read-only leaves and declared read paths bind
// read-only, and the workarea metadata stays hidden; with reads open the
// read-only leaf binds too but no read path does; an unknown scope is
// refused, never rendered as open reads.
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
	// Under the scope the workarea metadata stays hidden, like on the
	// profile backend; with reads open it is bound read-only.
	meta := filepath.Join(w.ws, ".workarea")
	if strings.Contains(text, "--ro-bind\n"+meta+"\n"+meta) {
		t.Errorf("a read-scoped rendering reveals the workarea metadata:\n%s", text)
	}
	plain := joinArgs(mustRenderBubblewrap(t, w.resolved(), nil))
	if strings.Contains(plain, filepath.Join(w.rp, "gitconfig")) {
		t.Fatalf("a session without a read scope binds a read path:\n%s", plain)
	}
	if !strings.Contains(plain, "--ro-bind\n"+w.ro+"\n"+w.ro) {
		t.Fatalf("a session with reads open hides its read-only leaf:\n%s", plain)
	}
	r.ReadScope = "home-minus-secrets"
	if _, err := renderBubblewrap(r, nil, linuxIdentity, ""); err == nil {
		t.Fatal("an unrendered read scope was rendered as open reads")
	}
}

// TestRenderBubblewrap_SocketsAreBound: a declared socket outside the
// writable set is bound read-only by exact path — a connect through a
// read-only bind succeeds — and never left unbound, which would hide it. A
// socket inside the writable set is reachable through the writable bind
// already and gains no read-only bind that would pin it.
func TestRenderBubblewrap_SocketsAreBound(t *testing.T) {
	w := newLinuxWorld(t)
	inside := filepath.Join(w.tmp, "inside.sock")
	r := w.resolved()
	r.Sockets = append(r.Sockets, inside)
	if err := os.WriteFile(inside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	args := mustRenderBubblewrap(t, r, nil)
	if !hasTriple(bindTargets(args), "--ro-bind", w.sock, w.sock) {
		t.Fatalf("rendering does not bind the declared socket %q:\n%s", w.sock, joinArgs(args))
	}
	if hasTriple(bindTargets(args), "--ro-bind", inside, inside) {
		t.Fatalf("rendering pins a socket inside the writable set read-only:\n%s", joinArgs(args))
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
// resolver shape: the boundary binds the resolver inputs itself, and a
// harness declares no resolver socket.
func TestBackendLinux_NameAndResolvers(t *testing.T) {
	b := &mountNamespaceBackend{}
	if b.Name() != BackendLinuxMountNamespace {
		t.Fatalf("Name = %q, want %q", b.Name(), BackendLinuxMountNamespace)
	}
	// The resolver inputs are bound by every boundary, so a harness
	// declares no socket for them: a declared /etc/resolv.conf would refuse
	// every spawn where systemd-resolved makes it a link.
	if sockets := ResolverSockets(); len(sockets) != 0 {
		t.Fatalf("ResolverSockets = %v, want none", sockets)
	}
	text := joinArgs(mustRenderBubblewrap(t, newLinuxWorld(t).resolved(), nil))
	for _, path := range mountNamespaceResolverBinds {
		if _, err := os.Lstat(path); err == nil && !strings.Contains(text, "\n--ro-bind\n"+path+"\n"+path+"\n") {
			t.Errorf("rendering does not bind the resolver input %q:\n%s", path, text)
		}
	}
	if _, err := b.Canonical("/tmp"); err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if _, err := b.Version(); err != nil {
		t.Fatalf("Version: %v", err)
	}
}

// TestLandlockStageDescribe pins the stage policy text: the writable set
// allows writes, the runtime binds and the read allowlist allow reads, each
// at exactly the path the tree binds, declared ports are recorded, and the
// host directories grant reads only with reads open.
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
		// A file grants exactly itself.
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
	// A declared file never opens the directory around it: a
	// configuration file in the operator home would otherwise open the
	// whole home under the read scope, which the tree binds for metadata.
	if strings.Contains(text, "allow-read "+w.rp+"\n") {
		t.Errorf("stage policy grants the directory around a declared file:\n%s", text)
	}
	// Under the scope the host directories grant nothing; with reads open
	// they grant reads, and the workarea metadata with them.
	for _, dir := range []string{w.home, w.stateHome, filepath.Join(w.ws, ".workarea")} {
		if strings.Contains(text, "allow-read "+dir+"\n") {
			t.Errorf("read-scoped stage policy grants %q:\n%s", dir, text)
		}
	}
	open, err := buildLandlockStage(w.resolved())
	if err != nil {
		t.Fatalf("buildLandlockStage: %v", err)
	}
	for _, dir := range []string{w.home, w.stateHome, w.ro, filepath.Join(w.ws, ".workarea")} {
		if !strings.Contains(open.describe(), "allow-read "+dir+"\n") {
			t.Errorf("open-read stage policy lacks a read grant on %q:\n%s", dir, open.describe())
		}
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
	// The stage and harness executables gain read-and-execute grants on
	// the files themselves, so binaries outside every other grant run.
	if len(parsed.execRoots) != 2 || parsed.execRoots[0] != "/self" {
		t.Fatalf("the parsed stage grants executables %v, want the stage and the harness", parsed.execRoots)
	}
	if info, err := os.Stat(parsed.execRoots[1]); err != nil || info.IsDir() {
		t.Fatalf("the harness grant %q is not the executable file itself", parsed.execRoots[1])
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
	stubLandlock(t, 6)
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

// TestApply_LandlockGateRefusesClosed: the Landlock gate pins every branch
// — a kernel without Landlock, and one whose ABI cannot express the rights
// the stage needs, refuse with backend_absent (the enforcing stage is
// missing, not a namespace) instead of staging a weaker boundary, and a
// capable one proceeds.
func TestApply_LandlockGateRefusesClosed(t *testing.T) {
	fx := newLinuxApplyWorld(t)
	for _, abi := range []int{0, 1} {
		stubLandlock(t, abi)
		_, err := fx.backend.Apply(ApplyRequest{Resolved: fx.resolved})
		if err == nil {
			t.Fatalf("Apply staged a boundary on Landlock ABI %d", abi)
		}
		if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
			t.Fatalf("Apply on Landlock ABI %d: err=%v, want backend_absent", abi, err)
		}
	}
	stubLandlock(t, landlockMinABI)
	if _, err := fx.backend.Apply(ApplyRequest{Resolved: fx.resolved}); err != nil {
		t.Fatalf("Apply with Landlock present: %v", err)
	}
}

// TestPrepare_WrapShapeThroughConfiner drives the full production entry
// point — Confiner.Prepare through plan.Command — asserting the wrapped
// argv starts with the resolved launcher, invokes the stage, and ends with
// the harness. A stand-in launcher answers Check's capability probe, so the
// test runs on any Linux host; the live tests prove the real launcher.
func TestPrepare_WrapShapeThroughConfiner(t *testing.T) {
	w := newLinuxWorld(t)
	// The stand-in launcher answers the capability probe and is never
	// asked to run the harness: Prepare only renders the plan.
	launcher := fakeLauncher(t, "exit 0")
	stubLandlock(t, 6)
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

// stubLandlock pins the Landlock ABI the gates see for one test, so both
// branches run on any kernel; production always probes the running one.
func stubLandlock(t *testing.T, abi int) {
	t.Helper()
	old := landlockProbe
	landlockProbe = func() int { return abi }
	t.Cleanup(func() { landlockProbe = old })
}

// fakeLauncher writes a stand-in launcher that records its arguments, one
// per line, beside itself and then runs body, so a test can drive Check's
// capability probe without bubblewrap.
func fakeLauncher(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	launcher := filepath.Join(dir, "bwrap")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"$(dirname \"$0\")/args\"\n" + body + "\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil { //nolint:gosec // G306: a stand-in launcher executable.
		t.Fatalf("launcher seed: %v", err)
	}
	return launcher
}

// TestCheck_ProbesTheLauncher: Check proves the host can build a boundary
// by running the launcher itself with the namespace flags, the tmpfs root,
// a fresh proc and a private /dev around a fixed no-op executable — what a
// bare user-namespace probe cannot see (a security module that allows the
// namespace but denies the mounts in it). A launcher that refuses turns
// into namespace_unavailable carrying its own diagnostic and the hint; a
// missing launcher or Landlock is backend_absent; a working one passes.
func TestCheck_ProbesTheLauncher(t *testing.T) {
	t.Setenv(mountNamespaceMarkEnv, "")
	stubLandlock(t, 6)

	ok := fakeLauncher(t, "exit 0")
	if err := (&mountNamespaceBackend{launcherOverride: ok}).Check(); err != nil {
		t.Fatalf("Check with a working launcher: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(ok), "args"))
	if err != nil {
		t.Fatalf("the launcher was never run: %v", err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if !slices.Equal(args[:len(mountNamespaceFlags)], mountNamespaceFlags) {
		t.Fatalf("probe args = %v, want the spawn's namespace flags first", args)
	}
	text := joinArgs(args)
	for _, want := range []string{"\n--tmpfs\n/\n", "\n--proc\n/proc\n", "\n--tmpfs\n/dev\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("probe args lack %q:\n%s", want, text)
		}
	}
	if n := len(args); n < 2 || args[n-2] != "--" || !slices.Contains(launcherProbeTargets, args[n-1]) {
		t.Fatalf("probe args = %v, want a fixed no-op target after the separator", args)
	}

	refused := fakeLauncher(t, "echo 'bwrap: setting up uid map: Permission denied' >&2; exit 1")
	err = (&mountNamespaceBackend{launcherOverride: refused}).Check()
	if reason, _ := ReasonOf(err); reason != ReasonNamespaceUnavailable {
		t.Fatalf("Check with a refusing launcher: err=%v, want namespace_unavailable", err)
	}
	for _, want := range []string{"setting up uid map: Permission denied", "AppArmor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}

	if err := (&mountNamespaceBackend{launcherOverride: "/nonexistent/bwrap-test-launcher"}).Check(); err == nil {
		t.Fatal("Check passed with no launcher")
	} else if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
		t.Fatalf("Check with no launcher: err=%v, want backend_absent", err)
	}
	for _, abi := range []int{0, 1} {
		stubLandlock(t, abi)
		if err := (&mountNamespaceBackend{launcherOverride: ok}).Check(); err == nil {
			t.Fatalf("Check passed on Landlock ABI %d", abi)
		} else if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
			t.Fatalf("Check on Landlock ABI %d: err=%v, want backend_absent", abi, err)
		}
	}
}

// TestLandlockRuleset_HandlesNoNetwork pins the stage's handled rights at
// every Landlock ABI: the filesystem rights that ABI knows, the signal and
// abstract-socket scopes from ABI 6, and no network right, ever. Handling
// TCP connects with only declared-port allow rules would deny every
// connect to an undeclared port on any address, remote endpoints included,
// because a port rule carries no address. restrictLandlockFull creates
// exactly this ruleset.
func TestLandlockRuleset_HandlesNoNetwork(t *testing.T) {
	for abi := 1; abi <= 8; abi++ {
		attr := landlockRuleset(abi)
		if attr.Access_net != 0 {
			t.Fatalf("ABI %d ruleset handles network %#x; want none", abi, attr.Access_net)
		}
		wantScope := uint64(0)
		if abi >= 6 {
			wantScope = unix.LANDLOCK_SCOPE_SIGNAL | unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET
		}
		if attr.Scoped != wantScope {
			t.Fatalf("ABI %d ruleset scopes %#x, want %#x", abi, attr.Scoped, wantScope)
		}
		want := uint64(landlockHandledFS)
		if abi < 3 {
			want &^= landlockAccessFSTruncate
		}
		if attr.Access_fs != want {
			t.Fatalf("ABI %d ruleset handles filesystem rights %#x, want %#x", abi, attr.Access_fs, want)
		}
	}
}

// TestLandlockStage_ScopesSignalsAndAbstractSockets proves the scopes live
// at the stage alone, without the process namespace that hides outside
// processes in a full boundary: the test binary re-executes itself through
// the real stage dispatcher and, from inside, signals a decoy outside its
// Landlock domain, attaches to it, and connects to an abstract socket
// created outside. Each refuses. The control runs the same checks without
// the stage, where each goes through, so a refusal means the ruleset held
// it. It needs Landlock ABI 6, and reports a skip on an older kernel.
func TestLandlockStage_ScopesSignalsAndAbstractSockets(t *testing.T) {
	if abi := landlockABI(); abi < landlockScopeABI {
		t.Skipf("Landlock ABI %d on this kernel; the scopes need %d", abi, landlockScopeABI)
	}
	steps := func(pid int, abstract string) []seatCheckStep {
		return []seatCheckStep{
			{ID: "signal", Op: "signal", Target: strconv.Itoa(pid)},
			{ID: "abstract", Op: "dial_abstract", Target: abstract},
			{ID: "attach", Op: "ptrace", Target: strconv.Itoa(pid)},
		}
	}
	run := func(staged bool) (map[string]seatCheckResult, string, *countingListener) {
		marker := filepath.Join(shortTempDir(t, "dcm"), "signalled")
		pid := startTestDecoy(t, marker)
		abstract := newAbstractListener(t)
		work := shortTempDir(t, "dcl")
		policy := &landlockPolicy{writeRoots: []string{work}, dev: true}
		for _, path := range mountNamespaceRuntimeBinds {
			if _, err := os.Lstat(path); err == nil {
				policy.readRoots = append(policy.readRoots, path)
			}
		}
		results := runSeatCheck(t, work, steps(pid, abstract.name), func(self, planPath string) (string, error) {
			args := []string{"-test.run=^$"}
			env := append(os.Environ(), seatCheckEnv+"="+planPath)
			if staged {
				args = append([]string{"stage", "--", self}, args...)
				env = append(env, landlockStageEnv+"="+policy.describe())
			}
			cmd := exec.Command(self, args...) //nolint:gosec // G204: the test binary re-executed, through the stage or not.
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			return string(out), err
		})
		return results, marker, abstract.counter
	}
	open, marker, accepts := run(false)
	for _, id := range []string{"signal", "abstract", "attach"} {
		if res := open[id]; res.Err != "" {
			t.Fatalf("control without the stage: %s refused (%s); the decoy must be reachable for the staged refusal to mean anything", id, res.Err)
		}
	}
	if !waitExists(marker) || !accepts.reached() {
		t.Fatal("control without the stage: the decoy was not signalled or the abstract socket not reached")
	}
	staged, marker, accepts := run(true)
	for _, id := range []string{"signal", "abstract", "attach"} {
		if res := staged[id]; res.Err == "" {
			t.Errorf("%s from inside the stage went through; the Landlock scope must refuse it", id)
		} else {
			t.Logf("%s refused inside the stage: %s", id, res.Err)
		}
	}
	if exists(marker) || accepts.accepted() > 0 {
		t.Fatalf("an effect crossed the stage: signalled=%v abstract accepts=%d", exists(marker), accepts.accepted())
	}
}

// TestLandlockStage_LeavesTCPOpen proves the network shape live, at the
// production entry point: the test binary re-executes itself through the
// real stage dispatcher (RunLandlockStageFromEnv, as main does), which
// restricts itself with the production ruleset and execs the seat check.
// Inside, a dial to an undeclared loopback port and one to the declared
// port both connect, while a read of a file no rule grants refuses — the
// control proving the ruleset was in force when the dials went through. If
// the stage ever handles TCP connects again with only the declared-port
// allows, the undeclared dial refuses and this test goes red. It needs a
// kernel with Landlock, and reports a skip where there is none.
func TestLandlockStage_LeavesTCPOpen(t *testing.T) {
	if abi := landlockABI(); abi < landlockMinABI {
		t.Skipf("Landlock ABI %d on this kernel; the stage needs %d", abi, landlockMinABI)
	}
	undeclared := newCountingListener(t)
	declared := newCountingListener(t)
	work := shortTempDir(t, "dcl")
	secret := filepath.Join(shortTempDir(t, "dcs"), "secret")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := &landlockPolicy{writeRoots: []string{work}, dev: true, tcpPorts: []int{declared.port}}
	for _, path := range mountNamespaceRuntimeBinds {
		if _, err := os.Lstat(path); err == nil {
			policy.readRoots = append(policy.readRoots, path)
		}
	}
	results := runSeatCheck(t, work, []seatCheckStep{
		{ID: "undeclared", Op: "dial", Target: undeclared.addr},
		{ID: "declared", Op: "dial", Target: declared.addr},
		{ID: "ungranted", Op: "read", Target: secret},
	}, func(self, planPath string) (string, error) {
		cmd := exec.Command(self, append([]string{"stage"}, append(stagePortArgs(policy), "--", self, "-test.run=^$")...)...) //nolint:gosec // G204: the test binary re-executed through the stage.
		cmd.Env = append(os.Environ(), landlockStageEnv+"="+policy.describe(), seatCheckEnv+"="+planPath)
		out, err := cmd.CombinedOutput()
		return string(out), err
	})
	if res := results["ungranted"]; !strings.Contains(res.Err, "permission denied") {
		t.Fatalf("a read no rule grants: %+v; want permission denied (the ruleset was not in force)", res)
	}
	for _, id := range []string{"undeclared", "declared"} {
		if res := results[id]; res.Err != "" {
			t.Errorf("%s loopback dial from inside the stage: %s; the stage must leave TCP connects unhandled", id, res.Err)
		}
	}
	if !undeclared.reached() || !declared.reached() {
		t.Fatalf("listener accepts: undeclared=%d declared=%d; want both reached", undeclared.accepted(), declared.accepted())
	}
}
