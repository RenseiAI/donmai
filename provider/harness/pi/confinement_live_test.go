//go:build darwin

package pi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/harnessstate"
)

// This file is the Done-when evidence: tests that drive pi through the
// provider's own production entry points — Provider.Spawn and
// Provider.Resume for the headless child, Provider.Spawn with an interactive
// spec for the PTY child — under the real macOS backend, with the production
// confinement gate (piConfinementEnabled) evaluated on its real inputs, and
// prove a forbidden write is refused in both modes. Every refusal case also
// runs through the same entry point without the request, where the same
// writes must succeed; that control is what makes each test fail when the
// wrap, the gate or the plan is removed.
//
// The harness binary is a shell script standing in for pi (fakeHarnessScript):
// the spawn path (admission, gate, state materialization, argv, sandbox-exec
// wrap, environment, working directory, inherited descriptors) is production
// code, while the script performs the filesystem operations a hostile tool
// execution would attempt and then answers the headless handshake the way
// the boundary extension does. A real pi binary cannot probe the OS boundary
// this way — its own policy layer adjudicates first — so the script isolates
// the OS backstop beneath it.
//
// Only the confiner's HOST directories are injected (Options.confinementDirs):
// throwaway profile, home, state-home and self-test scratch paths, so the
// tests never touch the operator's real state. Whether a session is confined
// is decided by the production gate from the session's own inputs: the host
// requirement (Options.RequireConfinement) or a declared repository
// authority.

// fakeHarnessScript is the stand-in harness. Its bytes never vary, so every
// live test's harness has the same digest and the self-tested confiner is
// shared across the package (ensurePiConfiner keys its cache by host
// directories plus digest). Per-test paths come from probe.env beside it.
//
// It attempts, in order: a write inside the mutable leaf, a write outside the
// set, a write into a read-only sibling, an append to the boundary extension
// (protected under confinement), and a write into $TMPDIR (the session tmp
// under confinement); it records $TMPDIR, $GOCACHE, $GOPATH and the pnpm
// store and the descriptors it holds; when the test names an outside file it
// reads it, lists the outside directory, reads inside the leaf and runs git
// there with the test's home; and, when the test names two loopback ports,
// whether it can connect to each. Every attempt sends its errors into the report and the script
// always exits zero, so the verdict comes from the report. In headless mode
// (`--mode rpc`) it then plays the RPC side: the handshake, and after the
// provider's first two commands (get_state plus the prompt or get_entries) a
// settled turn, then waits for stdin to close.
const fakeHarnessScript = `#!/bin/sh
. "$(dirname "$0")/probe.env"
r="$PROBE_MUT/report.txt"
: > "$r"
(echo inside-ok > "$PROBE_MUT/inside.txt") 2>>"$r"; echo "inside=$?" >> "$r"
(echo outside-breakout > "$PROBE_OUTSIDE/planted.txt") 2>>"$r"; echo "outside=$?" >> "$r"
(echo ro-breakout > "$PROBE_RO/planted.txt") 2>>"$r"; echo "ro=$?" >> "$r"
(echo tampered >> "$PROBE_EXTENSION") 2>>"$r"; echo "extension=$?" >> "$r"
(echo tmp-ok > "$TMPDIR/probe-tmp.txt") 2>>"$r"; echo "tmp=$?" >> "$r"
echo "tmpdir=$TMPDIR" >> "$r"
echo "gocache=$GOCACHE" >> "$r"
echo "gopath=$GOPATH" >> "$r"
echo "pnpm_store=$PNPM_CONFIG_STORE_DIR" >> "$r"
echo "xdg_runtime=$XDG_RUNTIME_DIR" >> "$r"
if [ -n "$PROBE_READ_OUTSIDE" ]; then
	(cat "$PROBE_READ_OUTSIDE" > /dev/null) 2>>"$r"; echo "read_outside=$?" >> "$r"
	(ls "$PROBE_OUTSIDE" > /dev/null) 2>>"$r"; echo "list_outside=$?" >> "$r"
	(cat "$PROBE_MUT/.git/HEAD" > /dev/null) 2>>"$r"; echo "read_inside=$?" >> "$r"
	(HOME="$PROBE_HOME" XDG_CONFIG_HOME= git -C "$PROBE_MUT" status --porcelain > /dev/null) 2>>"$r"; echo "git=$?" >> "$r"
fi
if [ -n "$PROBE_PORT_DECLARED" ]; then
	nc -z -G 3 127.0.0.1 "$PROBE_PORT_DECLARED" >/dev/null 2>&1; echo "declared=$?" >> "$r"
	nc -z -G 3 127.0.0.1 "$PROBE_PORT_UNDECLARED" >/dev/null 2>&1; echo "undeclared=$?" >> "$r"
fi
ls -l /dev/fd > "$PROBE_MUT/fds.txt" 2>/dev/null
case " $* " in
*" --mode rpc "*)
	cat "$PROBE_HANDSHAKE"
	read -r _ || exit 0
	read -r _ || exit 0
	cat "$PROBE_BODY"
	cat > /dev/null
	;;
esac
exit 0
`

// liveHost is the confiner host-directory fixture every live test shares,
// created once per test binary under /tmp (the self-test binds a socket in
// its scratch, and socket paths are short) and removed after the last test.
var liveHost struct {
	once sync.Once
	dirs piConfinementDirs
	err  error
}

func liveConfinementDirs(t *testing.T) *piConfinementDirs {
	t.Helper()
	liveHost.once.Do(func() {
		base, err := os.MkdirTemp("/tmp", "pch")
		if err != nil {
			liveHost.err = err
			return
		}
		testMainCleanups = append(testMainCleanups, func() { _ = os.RemoveAll(base) })
		if base, err = filepath.EvalSymlinks(base); err != nil {
			liveHost.err = err
			return
		}
		dirs := piConfinementDirs{
			home:       filepath.Join(base, "home"),
			stateHome:  filepath.Join(base, "state"),
			profileDir: filepath.Join(base, "state", "profiles"),
			scratchDir: filepath.Join(base, "scratch"),
			cacheDir:   filepath.Join(base, "state", "selftest-cache"),
		}
		for _, dir := range []string{dirs.home, dirs.stateHome, dirs.profileDir} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				liveHost.err = err
				return
			}
		}
		liveHost.dirs = dirs
	})
	if liveHost.err != nil {
		t.Fatalf("live confinement host directories: %v", liveHost.err)
	}
	dirs := liveHost.dirs
	return &dirs
}

// liveWorld is one session workarea: a root holding a mutable leaf (a real
// git repository, as a confined leaf must own its .git directory), a
// read-only sibling, the workarea metadata directory, and a decoy outside
// the set.
type liveWorld struct {
	root, mut, ro, outside string
}

func newLiveWorld(t *testing.T) liveWorld {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "piconf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	w := liveWorld{
		root:    filepath.Join(base, "ws"),
		outside: filepath.Join(base, "outside"),
	}
	w.mut = filepath.Join(w.root, "mut")
	w.ro = filepath.Join(w.root, "ro")
	for _, dir := range []string{w.mut, w.ro, w.outside, filepath.Join(w.root, ".workarea")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	gitInit(t, w.mut)
	return w
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "-c", "init.defaultBranch=main", "init", "--quiet", dir) //nolint:gosec // G204: test fixture; dir is a test temp path.
	cmd.Env = harnessstate.GitLocationNeutralEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
}

// spec returns the session spec for the world's mutable leaf.
func (w liveWorld) spec() agent.Spec {
	return agent.Spec{SessionName: "live", Cwd: w.mut, Prompt: "probe"}
}

// authority is a declared repository authority over the world: the mutable
// leaf writable, the sibling read-only.
func (w liveWorld) authority() *agent.RepositoryAuthorityPolicy {
	return &agent.RepositoryAuthorityPolicy{
		Protocol:      "session-root-v1",
		WorkareaRoot:  w.root,
		SelectedPath:  w.mut,
		MutablePaths:  []string{w.mut},
		ReadOnlyPaths: []string{w.ro},
	}
}

// writeLiveHarness writes the fake harness and its per-test inputs into a
// fresh directory outside the set and returns the harness path. extra are
// further KEY=VALUE lines for probe.env.
func writeLiveHarness(t *testing.T, w liveWorld, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-pi")
	if err := os.WriteFile(bin, []byte(fakeHarnessScript), 0o755); err != nil { //nolint:gosec // G306: test fixture needs the exec bit.
		t.Fatal(err)
	}
	handshake := filepath.Join(dir, "handshake.jsonl")
	body := filepath.Join(dir, "body.jsonl")
	if err := os.WriteFile(handshake, []byte(handshakeEvent("h1")), 0o600); err != nil {
		t.Fatal(err)
	}
	settled := getStateResponse("ses_live") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	if err := os.WriteFile(body, []byte(settled), 0o600); err != nil {
		t.Fatal(err)
	}
	extension := newSessionLayoutForSpec(w.spec()).extension
	env := fmt.Sprintf("PROBE_MUT='%s'\nPROBE_OUTSIDE='%s'\nPROBE_RO='%s'\nPROBE_EXTENSION='%s'\nPROBE_HANDSHAKE='%s'\nPROBE_BODY='%s'\n",
		w.mut, w.outside, w.ro, extension, handshake, body)
	for _, line := range extra {
		env += line + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin
}

// liveProvider is the provider under test: the fake harness as the binary,
// the host requirement as given, the handshake token the fake echoes, and
// the shared throwaway host directories. Nothing here bypasses the gate.
func liveProvider(t *testing.T, bin string, requireConfinement bool) *Provider {
	t.Helper()
	return &Provider{binary: bin, opts: Options{
		HandshakeTimeout:   30 * time.Second,
		RequireConfinement: requireConfinement,
		handshakeToken:     testHandshakeToken,
		confinementDirs:    liveConfinementDirs(t),
	}}
}

// liveCtx bounds one entry-point call, including the one-time self-test.
func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// finishHeadless reads the session to its terminal event, then stops it.
func finishHeadless(t *testing.T, h agent.Handle) {
	t.Helper()
	defer func() { _ = h.Stop(context.Background()) }()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return
			}
			switch ev.(type) {
			case agent.ResultEvent, agent.ErrorEvent:
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the headless session to settle")
		}
	}
}

// finishInteractive waits for the PTY child to exit.
func finishInteractive(t *testing.T, h agent.Handle) {
	t.Helper()
	defer func() { _ = h.Stop(context.Background()) }()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the interactive probe to exit")
		}
	}
}

// probeReport is the parsed report the fake harness wrote.
type probeReport struct {
	raw  string
	vals map[string]string
}

func readProbeReport(t *testing.T, w liveWorld) probeReport {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(w.mut, "report.txt"))
	if err != nil {
		t.Fatalf("read probe report: %v", err)
	}
	r := probeReport{raw: string(body), vals: map[string]string{}}
	for _, line := range strings.Split(r.raw, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && !strings.ContainsAny(key, " :/") {
			r.vals[key] = value
		}
	}
	return r
}

// resetProbe clears what a previous run left, so a control run's report is
// its own.
func resetProbe(w liveWorld) {
	for _, path := range []string{
		filepath.Join(w.mut, "report.txt"),
		filepath.Join(w.mut, "fds.txt"),
		filepath.Join(w.outside, "planted.txt"),
		filepath.Join(w.ro, "planted.txt"),
	} {
		_ = os.Remove(path)
	}
}

// assertConfined checks a confined run: the write inside the leaf landed;
// the writes outside the set, into the read-only sibling and into the
// protected boundary extension were refused and left nothing behind; the
// session tmp and cache variables point into the session state root, and the
// write into the session tmp landed.
func assertConfined(t *testing.T, w liveWorld) {
	t.Helper()
	r := readProbeReport(t, w)
	layout := newSessionLayoutForSpec(w.spec())
	if r.vals["inside"] != "0" {
		t.Errorf("confined: the write inside the mutable leaf failed:\n%s", r.raw)
	}
	for _, key := range []string{"outside", "ro", "extension"} {
		if got, ok := r.vals[key]; !ok || got == "0" {
			t.Errorf("confined: the %s write was not refused (status %q):\n%s", key, got, r.raw)
		}
	}
	for _, planted := range []string{filepath.Join(w.outside, "planted.txt"), filepath.Join(w.ro, "planted.txt")} {
		if _, err := os.Lstat(planted); err == nil {
			t.Errorf("confined: the run planted %s", planted)
		}
	}
	if got, err := os.ReadFile(layout.extension); err != nil || string(got) != string(extensionSource()) {
		t.Errorf("confined: the boundary extension changed under the run (err %v)", err)
	}
	if want := filepath.Join(layout.root, piSessionTmpDir); r.vals["tmpdir"] != want {
		t.Errorf("confined: TMPDIR = %q, want the session tmp %q", r.vals["tmpdir"], want)
	}
	if r.vals["tmp"] != "0" {
		t.Errorf("confined: the write into the session tmp failed:\n%s", r.raw)
	}
	if want := filepath.Join(layout.root, piSessionCacheDir, "go-build"); r.vals["gocache"] != want {
		t.Errorf("confined: GOCACHE = %q, want the session cache %q", r.vals["gocache"], want)
	}
	assertNoLeakedDescriptors(t, w)
}

// assertUnconfined checks a control run through the same entry point
// without a confinement request: the same writes succeed. If they cannot,
// the confined assertions prove nothing about the confinement.
func assertUnconfined(t *testing.T, w liveWorld) {
	t.Helper()
	r := readProbeReport(t, w)
	for _, key := range []string{"inside", "outside", "ro", "extension"} {
		if r.vals[key] != "0" {
			t.Errorf("UNCONFINED control: the %s write failed, so the test cannot discriminate:\n%s", key, r.raw)
		}
	}
}

// assertNoLeakedDescriptors checks the descriptor listing the probe left:
// every descriptor the confined child holds must resolve to a standard
// stream, the descriptor directory itself, or a file inside the mutable
// leaf (the listing's own output redirect) — never a file outside the set.
// The headless child inherits the three stdio pipes and the interactive
// child the PTY slave; anything beyond those would be a descriptor open on
// a file the boundary does not judge.
func assertNoLeakedDescriptors(t *testing.T, w liveWorld) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.mut, "fds.txt"))
	if err != nil {
		t.Fatalf("read fds: %v", err)
	}
	list := strings.TrimSpace(string(raw))
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || fields[0] == "total" {
			continue
		}
		name := fields[len(fields)-1]
		if _, perr := strconv.Atoi(strings.TrimSpace(name)); perr != nil && !strings.HasPrefix(name, "/dev/fd") && !strings.HasPrefix(name, w.mut+"/") {
			t.Errorf("confined child holds descriptor open on %q (full list %q): only stdio may be inherited", name, list)
		}
	}
}

// TestPiConfinement_HeadlessSpawnRefusesForbiddenWrites is the headless half
// of Done-when through Provider.Spawn: a host that requires confinement
// confines the session, and the forbidden writes are refused; the same
// Spawn without the requirement runs unconfined and the writes succeed.
func TestPiConfinement_HeadlessSpawnRefusesForbiddenWrites(t *testing.T) {
	w := newLiveWorld(t)
	bin := writeLiveHarness(t, w)

	h, err := liveProvider(t, bin, true).Spawn(liveCtx(t), w.spec())
	if err != nil {
		t.Fatalf("confined Spawn: %v", err)
	}
	finishHeadless(t, h)
	assertConfined(t, w)

	resetProbe(w)
	h, err = liveProvider(t, bin, false).Spawn(liveCtx(t), w.spec())
	if err != nil {
		t.Fatalf("unconfined Spawn: %v", err)
	}
	finishHeadless(t, h)
	assertUnconfined(t, w)
}

// TestPiConfinement_HeadlessResumeRefusesForbiddenWrites is the same through
// Provider.Resume: a resume is a new process and is re-confined, never run
// unconfined (ADR-2026-10-03 D4.5).
func TestPiConfinement_HeadlessResumeRefusesForbiddenWrites(t *testing.T) {
	w := newLiveWorld(t)
	bin := writeLiveHarness(t, w)

	h, err := liveProvider(t, bin, true).Resume(liveCtx(t), "ses_live", w.spec())
	if err != nil {
		t.Fatalf("confined Resume: %v", err)
	}
	finishHeadless(t, h)
	assertConfined(t, w)

	resetProbe(w)
	h, err = liveProvider(t, bin, false).Resume(liveCtx(t), "ses_live", w.spec())
	if err != nil {
		t.Fatalf("unconfined Resume: %v", err)
	}
	finishHeadless(t, h)
	assertUnconfined(t, w)
}

// TestPiConfinement_InteractiveSpawnRefusesForbiddenWrites is the
// interactive half of Done-when: Provider.Spawn with an interactive spec
// takes the PTY path, and the same refusals hold there.
func TestPiConfinement_InteractiveSpawnRefusesForbiddenWrites(t *testing.T) {
	w := newLiveWorld(t)
	bin := writeLiveHarness(t, w)
	spec := w.spec()
	spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}

	h, err := liveProvider(t, bin, true).Spawn(liveCtx(t), spec)
	if err != nil {
		t.Fatalf("confined interactive Spawn: %v", err)
	}
	finishInteractive(t, h)
	assertConfined(t, w)

	resetProbe(w)
	h, err = liveProvider(t, bin, false).Spawn(liveCtx(t), spec)
	if err != nil {
		t.Fatalf("unconfined interactive Spawn: %v", err)
	}
	finishInteractive(t, h)
	assertUnconfined(t, w)
}

// TestPiConfinement_AuthorityRequestConfinesThroughLaunch covers the other
// gate input: a declared repository authority requests confinement, and the
// per-session confinement record names its read-only leaves. pi's manifest
// declares no multi-repository workarea protocol yet, so admission refuses
// such a spec at Spawn (asserted first); the test therefore enters at
// launch, the shared post-admission entry of Spawn and Resume, with the gate
// still evaluated on the spec. When the manifest gains a protocol, the
// admission assertion fails and this test should move to Spawn.
func TestPiConfinement_AuthorityRequestConfinesThroughLaunch(t *testing.T) {
	w := newLiveWorld(t)
	bin := writeLiveHarness(t, w)
	spec := w.spec()
	spec.RepositoryAuthority = w.authority()
	p := liveProvider(t, bin, false)

	if h, err := p.Spawn(liveCtx(t), spec); err == nil {
		_ = h.Stop(context.Background())
		t.Fatalf("Spawn admitted a repository authority for pi; drive this test through Spawn now")
	}

	h, err := p.launch(liveCtx(t), spec, launchPrompt, "")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	plan := h.(*Handle).confinement
	finishHeadless(t, h)
	if plan == nil {
		t.Fatalf("an authority-bearing session spawned without a confinement plan")
	}
	record := plan.Record()
	if !slices.Contains(record.ReadOnlyLeaves, filepath.Base(w.ro)) {
		t.Errorf("confinement record read-only leaves = %v, want %q", record.ReadOnlyLeaves, filepath.Base(w.ro))
	}
	for _, class := range []confinement.WritableClass{confinement.ClassMutableLeaf, confinement.ClassHarnessState, confinement.ClassSessionTmp, confinement.ClassSessionCache} {
		if !slices.Contains(record.WritableClasses, class) {
			t.Errorf("confinement record writable classes = %v, missing %s", record.WritableClasses, class)
		}
	}
	assertConfined(t, w)
}

// TestPiConfinement_InlineRequiredExtensionStartsConfined pins the
// code-intelligence shape: a confined session carrying a Required inline
// extension starts, and its materialized copy is its own file, not a hard
// link to the shared materialization cache (which the confinement refuses as
// a link out of the writable set).
func TestPiConfinement_InlineRequiredExtensionStartsConfined(t *testing.T) {
	w := newLiveWorld(t)
	bin := writeLiveHarness(t, w)
	body := []byte("export default function activate(pi) {}\n")
	spec := w.spec()
	spec.AdditionalExtensions = []agent.ExtensionDelivery{
		{ID: "bridge", Kind: agent.ExtensionDeliveryInline, Source: body, Basename: "bridge.ts", Digest: sha256Hex(body), Required: true},
	}

	h, err := liveProvider(t, bin, true).Spawn(liveCtx(t), spec)
	if err != nil {
		t.Fatalf("confined Spawn with a Required inline extension: %v", err)
	}
	finishHeadless(t, h)
	assertConfined(t, w)

	injected := filepath.Join(newSessionLayoutForSpec(spec).injected, sanitizeInjectedBasename("bridge", "bridge.ts"))
	info, err := os.Lstat(injected)
	if err != nil {
		t.Fatalf("injected extension: %v", err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 {
		t.Errorf("injected extension has %v links, want exactly one", info.Sys())
	}
}

// plantedLinkCase is one link the seat could have left in its state during
// a previous run, and the refusal it must produce.
type plantedLinkCase struct {
	name  string
	plant func(t *testing.T, w liveWorld, layout sessionLayout)
	want  string // the refusal names this
}

func plantedLinkCases() []plantedLinkCase {
	return []plantedLinkCase{
		{"git exclude file", func(t *testing.T, w liveWorld, _ sessionLayout) {
			target := filepath.Join(w.outside, "exclude")
			mustWrite(t, target, "keep\n")
			path := filepath.Join(w.mut, ".git", "info", "exclude")
			_ = os.Remove(path)
			mustSymlink(t, target, path)
		}, "symbolic link"},
		{"hard-linked git exclude file", func(t *testing.T, w liveWorld, _ sessionLayout) {
			target := filepath.Join(w.outside, "exclude")
			mustWrite(t, target, "keep\n")
			path := filepath.Join(w.mut, ".git", "info", "exclude")
			_ = os.Remove(path)
			if err := os.Link(target, path); err != nil {
				t.Fatal(err)
			}
		}, "hard link"},
		{"injected extensions directory", func(t *testing.T, w liveWorld, layout sessionLayout) {
			mustMkdir(t, filepath.Join(w.outside, "injected"))
			mustMkdir(t, layout.root)
			mustSymlink(t, filepath.Join(w.outside, "injected"), layout.injected)
		}, "symbolic link"},
		{"session cache directory", func(t *testing.T, w liveWorld, layout sessionLayout) {
			mustMkdir(t, filepath.Join(w.outside, "cache"))
			mustMkdir(t, layout.root)
			mustSymlink(t, filepath.Join(w.outside, "cache"), filepath.Join(layout.root, piSessionCacheDir))
		}, "symbolic link"},
		{"boundary extension file", func(t *testing.T, w liveWorld, layout sessionLayout) {
			target := filepath.Join(w.outside, "policy.ts")
			mustWrite(t, target, "keep\n")
			mustMkdir(t, layout.root)
			mustSymlink(t, target, layout.extension)
		}, "symbolic link"},
		{"session state root", func(t *testing.T, w liveWorld, layout sessionLayout) {
			mustMkdir(t, filepath.Join(w.outside, "state"))
			mustSymlink(t, filepath.Join(w.outside, "state"), layout.root)
		}, "symbolic link"},
		{"working directory", func(t *testing.T, w liveWorld, _ sessionLayout) {
			leaf := filepath.Join(w.outside, "leaf")
			mustMkdir(t, leaf)
			gitInit(t, leaf)
			if err := os.RemoveAll(w.mut); err != nil {
				t.Fatal(err)
			}
			mustSymlink(t, leaf, w.mut)
		}, "symbolic link"},
	}
}

// runPlantedLinkCases plants each case in a fresh world, starts the session
// through start (a confined entry point), and asserts it was refused for the
// link before the parent wrote anything outside the set, and that the child
// never ran.
func runPlantedLinkCases(t *testing.T, mode func(agent.Spec) agent.Spec, start func(p *Provider, spec agent.Spec) (agent.Handle, error)) {
	t.Helper()
	body := []byte("export default function activate(pi) {}\n")
	for _, tc := range plantedLinkCases() {
		t.Run(tc.name, func(t *testing.T) {
			w := newLiveWorld(t)
			bin := writeLiveHarness(t, w)
			spec := mode(w.spec())
			spec.AdditionalExtensions = []agent.ExtensionDelivery{
				{ID: "bridge", Kind: agent.ExtensionDeliveryInline, Source: body, Basename: "bridge.ts", Digest: sha256Hex(body), Required: true},
			}
			tc.plant(t, w, newSessionLayoutForSpec(spec))
			before := snapshotTree(t, w.outside)

			h, err := start(liveProvider(t, bin, true), spec)
			if err == nil {
				_ = h.Stop(context.Background())
				t.Fatalf("the session started with a planted link")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused for another reason (want %q): %v", tc.want, err)
			}
			if after := snapshotTree(t, w.outside); after != before {
				t.Errorf("the parent wrote outside the set before refusing.\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if _, err := os.Lstat(filepath.Join(w.outside, "leaf", "report.txt")); err == nil {
				t.Errorf("the child ran")
			}
		})
	}
}

// TestPiConfinement_ResumeRefusesPlantedLinksBeforeWriting pins the resume
// hazard: the seat had the whole previous run to leave a symbolic link (or a
// hard link) where the parent expects state. The unconfined parent must
// refuse before any write, never follow the link: nothing outside the set is
// created or modified, and the child never runs.
func TestPiConfinement_ResumeRefusesPlantedLinksBeforeWriting(t *testing.T) {
	runPlantedLinkCases(t, func(spec agent.Spec) agent.Spec { return spec },
		func(p *Provider, spec agent.Spec) (agent.Handle, error) {
			return p.Resume(liveCtx(t), "ses_live", spec)
		})
}

// TestPiConfinement_InteractiveRefusesPlantedLinksBeforeWriting is the same
// for the interactive path: an interactive session started in a working
// directory a previous run left links in is refused the same way.
func TestPiConfinement_InteractiveRefusesPlantedLinksBeforeWriting(t *testing.T) {
	runPlantedLinkCases(t, func(spec agent.Spec) agent.Spec {
		spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}
		return spec
	}, func(p *Provider, spec agent.Spec) (agent.Handle, error) {
		return p.Spawn(liveCtx(t), spec)
	})
}

// TestPiConfinement_LoopbackEndpointPortIsTheOnlyLoopbackOpening pins the
// declared-port rule under the real backend: a confined session whose model
// endpoint is on loopback can connect to that port and to no other loopback
// port; the unconfined control reaches both. An endpoint naming the daemon
// control API port is refused before anything is written or run.
func TestPiConfinement_LoopbackEndpointPortIsTheOnlyLoopbackOpening(t *testing.T) {
	declared := liveListener(t)
	undeclared := liveListener(t)
	w := newLiveWorld(t)
	bin := writeLiveHarness(t, w,
		fmt.Sprintf("PROBE_PORT_DECLARED='%d'", declared),
		fmt.Sprintf("PROBE_PORT_UNDECLARED='%d'", undeclared))
	spec := w.spec()
	spec.Endpoint = liveEndpoint(fmt.Sprintf("http://127.0.0.1:%d/v1", declared))

	h, err := liveProvider(t, bin, true).Spawn(liveCtx(t), spec)
	if err != nil {
		t.Fatalf("confined Spawn: %v", err)
	}
	finishHeadless(t, h)
	r := readProbeReport(t, w)
	if r.vals["declared"] != "0" {
		t.Errorf("confined: the declared endpoint port was not reachable:\n%s", r.raw)
	}
	if got, ok := r.vals["undeclared"]; !ok || got == "0" {
		t.Errorf("confined: an undeclared loopback port was reachable (status %q):\n%s", got, r.raw)
	}

	resetProbe(w)
	h, err = liveProvider(t, bin, false).Spawn(liveCtx(t), spec)
	if err != nil {
		t.Fatalf("unconfined Spawn: %v", err)
	}
	finishHeadless(t, h)
	r = readProbeReport(t, w)
	if r.vals["declared"] != "0" || r.vals["undeclared"] != "0" {
		t.Errorf("UNCONFINED control: a loopback port was unreachable, so the test cannot discriminate:\n%s", r.raw)
	}

	resetProbe(w)
	daemonPort := spec
	daemonPort.Endpoint = liveEndpoint(fmt.Sprintf("http://127.0.0.1:%d/v1", runtimeenv.DefaultDaemonControlPort))
	if h, err := liveProvider(t, bin, true).Spawn(liveCtx(t), daemonPort); err == nil {
		_ = h.Stop(context.Background())
		t.Fatalf("a confined session whose endpoint names the daemon control port started")
	} else if !strings.Contains(err.Error(), "daemon control") {
		t.Errorf("refused for another reason: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(w.mut, "report.txt")); err == nil {
		t.Errorf("the child ran for an endpoint naming the daemon control port")
	}
}

// liveListener accepts and closes loopback connections until the test ends,
// and returns its port.
func liveListener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// liveEndpoint is a model endpoint binding at baseURL that admission
// accepts; nothing in these tests calls it as a model.
func liveEndpoint(baseURL string) *agent.EndpointBinding {
	return &agent.EndpointBinding{
		Company:  agent.CompanyStub,
		Model:    "live-endpoint-model",
		BaseURL:  baseURL,
		Protocol: agent.ProtoOpenAIChat,
		Host:     agent.HostDirect,
		Env:      map[string]string{"OPENAI_API_KEY": "live-endpoint-placeholder"}, //nolint:gosec // G101: fixture placeholder, not a credential.
	}
}

// TestPiConfinement_DigestChangeRetiresAttestation pins the record's
// staleness: a confiner holding a passing self-test attests; a confiner for
// a changed harness binary — the same host directories under a different
// executable digest — holds no record and refuses to prepare, instead of
// reusing the cached attestation.
//
// RED: wire a constant digest into ensurePiConfiner and the second Prepare
// succeeds.
func TestPiConfinement_DigestChangeRetiresAttestation(t *testing.T) {
	w := newLiveWorld(t)
	bin := writeLiveHarness(t, w)
	dirs := liveConfinementDirs(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ensurePiConfiner(liveCtx(t), bin, *dirs, []string{exe})
	if err != nil {
		t.Fatalf("ensurePiConfiner: %v", err)
	}
	if _, ok := c.Attestation(); !ok {
		t.Fatalf("passing self-test does not attest")
	}
	spec := w.spec()
	spec.RepositoryAuthority = w.authority()
	layout, err := materializeExtensionForSpec(spec, true)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := confinePiSession(spec, layout, c, piReadScope{})
	if err != nil {
		t.Fatalf("Prepare under the tested digest: %v", err)
	}
	t.Cleanup(func() { _ = plan.Release() })

	changed, err := confinement.New(confinement.Options{
		Backend:          confinement.DefaultBackend(),
		ProfileDir:       dirs.profileDir,
		Home:             dirs.home,
		StateHome:        dirs.stateHome,
		ExecutableDigest: "sha256:changed-harness-binary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := changed.Attestation(); ok {
		t.Fatalf("a confiner that never ran the self-test attests")
	}
	if _, err := confinePiSession(spec, layout, changed, piReadScope{}); err == nil {
		t.Fatalf("Prepare under a changed digest succeeded without a self-test: the cached attestation was reused")
	}
}

// snapshotTree renders every entry under dir — kind, link target or content
// digest — so any creation or modification shows as a difference.
func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			lines = append(lines, rel+" -> "+target)
		case d.IsDir():
			lines = append(lines, rel+"/")
		default:
			raw, readErr := os.ReadFile(path) //nolint:gosec // G304: test fixture tree.
			if readErr != nil {
				return readErr
			}
			sum := sha256.Sum256(raw)
			lines = append(lines, rel+" "+hex.EncodeToString(sum[:8]))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// TestRealBinary_ConfinedTurnReachesLoopbackEndpoint runs the REAL pi binary
// under the confinement a host requirement requests, against a model stub
// on loopback TCP — the shape of a gateway-routed session — and proves the
// turn completes: the boundary extension loads and handshakes inside the
// profile, and the declared endpoint port is reachable while the rest of
// loopback stays closed. Skips without pi on PATH (hosted CI has none).
func TestRealBinary_ConfinedTurnReachesLoopbackEndpoint(t *testing.T) {
	realBinaryAvailable(t)
	w := newLiveWorld(t)
	stub := newRealBinaryStub(t, realBinaryModel)
	spec := realBinarySpec(w.mut, "reply with a short greeting and nothing else", stub.baseURL())
	p, err := New(Options{HandshakeTimeout: 60 * time.Second, RequireConfinement: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.opts.confinementDirs = liveConfinementDirs(t)
	h, err := p.Spawn(liveCtx(t), spec)
	if err != nil {
		t.Fatalf("confined Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	if h.(*Handle).confinement == nil {
		t.Fatalf("the host requires confinement but the session spawned without a plan")
	}
	sawReply := false
	for _, ev := range drainToResult(t, h, 90*time.Second) {
		switch e := ev.(type) {
		case agent.AssistantTextEvent:
			if strings.HasPrefix(e.Text, "stub-reply-") {
				sawReply = true
			}
		case agent.ErrorEvent:
			t.Fatalf("confined session ended with an ErrorEvent: %+v", e)
		}
	}
	if !sawReply || len(stub.recordedTurns()) == 0 {
		t.Fatalf("the confined turn never reached the loopback model endpoint (reply=%v, requests=%d)", sawReply, len(stub.recordedTurns()))
	}
}

// TestRealBinary_ReadScopedTurnCompletes runs the REAL pi binary under the
// workarea read scope against the loopback model stub and proves the turn
// completes: pi reads its own install, its per-session agent home and the
// boundary extension inside the profile, and tolerates what it may no
// longer read — a home holding user-level skills it would otherwise load.
// Skips without pi on PATH (hosted CI has none).
func TestRealBinary_ReadScopedTurnCompletes(t *testing.T) {
	realBinaryAvailable(t)
	w := newLiveWorld(t)
	home := filepath.Join(filepath.Dir(w.root), "home")
	skill := filepath.Join(home, ".agents", "skills", "operator-skill")
	mustMkdir(t, skill)
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: operator-skill\ndescription: x\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	stub := newRealBinaryStub(t, realBinaryModel)
	spec := realBinarySpec(w.mut, "reply with a short greeting and nothing else", stub.baseURL())
	p, err := New(Options{HandshakeTimeout: 60 * time.Second, ConfinementReadScope: agent.FileReadWorkarea})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dirs := *liveConfinementDirs(t)
	dirs.home = home
	p.opts.confinementDirs = &dirs
	h, err := p.Spawn(liveCtx(t), spec)
	if err != nil {
		t.Fatalf("read-scoped Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	if h.(*Handle).confinement == nil {
		t.Fatalf("the host read scope requires confinement but the session spawned without a plan")
	}
	sawReply := false
	for _, ev := range drainToResult(t, h, 90*time.Second) {
		switch e := ev.(type) {
		case agent.AssistantTextEvent:
			if strings.HasPrefix(e.Text, "stub-reply-") {
				sawReply = true
			}
		case agent.ErrorEvent:
			t.Fatalf("read-scoped session ended with an ErrorEvent: %+v", e)
		}
	}
	if !sawReply || len(stub.recordedTurns()) == 0 {
		t.Fatalf("the read-scoped turn never completed (reply=%v, requests=%d)", sawReply, len(stub.recordedTurns()))
	}
}

// TestPiConfinement_ReadScopeConfinesReads drives Provider.Spawn under the
// host read scope: the seat reads inside its leaf and runs git with the
// user configuration the adapter declared readable, and its own harness
// install is readable, while reading or listing outside the set is refused.
// The same Spawn confined with reads open succeeds at every read, so the
// refusals are the read scope's.
func TestPiConfinement_ReadScopeConfinesReads(t *testing.T) {
	w := newLiveWorld(t)
	dirs := liveConfinementDirs(t)
	secret := filepath.Join(w.outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitconfig := filepath.Join(dirs.home, ".gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = probe\n\temail = probe@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(gitconfig) })
	bin := writeLiveHarness(t, w, "PROBE_READ_OUTSIDE='"+secret+"'", "PROBE_HOME='"+dirs.home+"'")

	opts := Options{ConfinementReadScope: agent.FileReadWorkarea}
	if err := applyHostReadScope(&opts); err != nil {
		t.Fatalf("applyHostReadScope: %v", err)
	}
	if !opts.RequireConfinement {
		t.Fatal("a read scope did not require confinement")
	}
	p := liveProvider(t, bin, opts.RequireConfinement)
	p.opts.ConfinementReadScope = opts.ConfinementReadScope
	h, err := p.Spawn(liveCtx(t), w.spec())
	if err != nil {
		t.Fatalf("read-scoped Spawn: %v", err)
	}
	finishHeadless(t, h)
	r := readProbeReport(t, w)
	for _, key := range []string{"inside", "read_inside", "git"} {
		if r.vals[key] != "0" {
			t.Errorf("read scope: %s failed:\n%s", key, r.raw)
		}
	}
	for _, key := range []string{"read_outside", "list_outside", "outside"} {
		if got, ok := r.vals[key]; !ok || got == "0" {
			t.Errorf("read scope: %s was not refused (status %q):\n%s", key, got, r.raw)
		}
	}
	layout := newSessionLayoutForSpec(w.spec())
	runtimeDir := filepath.Join(layout.root, piSessionCacheDir, "run")
	for key, want := range map[string]string{
		"gopath":      filepath.Join(layout.root, piSessionCacheDir, "go-path"),
		"pnpm_store":  filepath.Join(layout.root, piSessionCacheDir, "pnpm-store"),
		"xdg_runtime": runtimeDir,
	} {
		if r.vals[key] != want {
			t.Errorf("read scope: %s = %q, want the session cache %q", key, r.vals[key], want)
		}
	}
	// Tools only keep locks in a runtime directory their user owns alone.
	if info, err := os.Stat(runtimeDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the session runtime directory is not owner-only: %v %v", info, err)
	}

	resetProbe(w)
	h, err = liveProvider(t, bin, true).Spawn(liveCtx(t), w.spec())
	if err != nil {
		t.Fatalf("confined Spawn with reads open: %v", err)
	}
	finishHeadless(t, h)
	r = readProbeReport(t, w)
	for _, key := range []string{"read_outside", "list_outside", "read_inside", "git"} {
		if r.vals[key] != "0" {
			t.Errorf("CONTROL with reads open: %s failed, so the test cannot discriminate:\n%s", key, r.raw)
		}
	}
}

// TestPiConfinement_SecondWorkerReusesTheSelfTest: every seat is its own
// worker process, so the in-process confiner cache starts empty each time.
// After one worker's self-test passes, the next worker's confinement setup —
// the confiner (self-test from the host cache) plus the session plan under
// the read scope — takes under a second, reusing the same record.
func TestPiConfinement_SecondWorkerReusesTheSelfTest(t *testing.T) {
	w := newLiveWorld(t)
	bin := writeLiveHarness(t, w)
	dirs := liveConfinementDirs(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	first, err := ensurePiConfiner(liveCtx(t), bin, *dirs, []string{exe})
	if err != nil {
		t.Fatalf("first worker: %v", err)
	}
	firstRecord, _ := first.SelfTestRecord()

	// A new worker process: nothing cached in memory.
	piConfinerCache.Lock()
	piConfinerCache.entries = nil
	piConfinerCache.Unlock()

	p := &Provider{binary: bin, opts: Options{ConfinementReadScope: agent.FileReadWorkarea, RequireConfinement: true, confinementDirs: dirs}}
	start := time.Now()
	second, err := ensurePiConfiner(liveCtx(t), bin, *dirs, []string{exe})
	if err != nil {
		t.Fatalf("second worker: %v", err)
	}
	layout, err := materializeExtensionForSpec(w.spec(), true)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.confineSession(w.spec(), layout, second)
	setup := time.Since(start)
	if err != nil {
		t.Fatalf("second worker's session plan: %v", err)
	}
	t.Cleanup(func() { _ = plan.Release() })
	secondRecord, _ := second.SelfTestRecord()
	if secondRecord.Digest != firstRecord.Digest {
		t.Fatalf("the second worker probed again: record %s, want the cached %s", secondRecord.Digest, firstRecord.Digest)
	}
	if setup >= time.Second {
		t.Fatalf("the second worker's confinement setup took %s, want under 1s", setup)
	}
	t.Logf("second worker confinement setup: %s", setup.Round(time.Millisecond))
}
