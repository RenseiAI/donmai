//go:build linux

package pi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// This file is the Linux half of the Done-when evidence: tests that drive pi
// through the provider's own production entry points — Provider.Spawn and
// Provider.Resume for the headless child, Provider.Spawn with an interactive
// spec for the PTY child — under the real mount-namespace backend, with the
// production confinement gate (piConfinementEnabled) evaluated on its real
// inputs, and prove a forbidden write is refused in both modes. Every
// refusal case also runs through the same entry point without the request,
// where the same writes must succeed; that control is what makes each test
// fail when the wrap, the gate or the plan is removed.
//
// It mirrors confinement_live_test.go (the macOS proof, darwin-only) probe
// for probe: the same fake harness, the same live world, the same gate
// inputs, the same confined/unconfined verdicts. The verdicts are the same
// because the boundary contract is the same: mutable leaves writable, the
// workarea root, read-only leaves, the boundary extension and everything
// else refused.
//
// The tests need the launcher, unprivileged user namespaces and the Landlock
// stage, so each skips honestly — never passes silently — where the kernel,
// a security module or the container disallows them. The unsupported-host
// refusal test inverts the gate: it runs only where the startup check
// fails, and pins the clean refusal there.

// fakeLinuxHarnessScript is the stand-in harness, the same shape as the
// macOS proof's: the spawn path (admission, gate, state materialization,
// argv, mount-namespace wrap, environment, working directory, inherited
// descriptors) is production code, while the script performs the filesystem
// operations a hostile tool execution would attempt and then answers the
// headless handshake the way the boundary extension does. A real pi binary
// cannot probe the OS boundary this way — its own policy layer adjudicates
// first — so the script isolates the OS backstop beneath it.
const fakeLinuxHarnessScript = `#!/bin/sh
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

// linuxLiveHost is the confiner host-directory fixture every Linux live
// test shares, created once per test binary under /tmp (the self-test binds
// a socket in its scratch, and socket paths are short) and removed after
// the last test.
var linuxLiveHost struct {
	once sync.Once
	dirs piConfinementDirs
	err  error
}

func linuxLiveConfinementDirs(t *testing.T) *piConfinementDirs {
	t.Helper()
	linuxLiveHost.once.Do(func() {
		base, err := os.MkdirTemp("/tmp", "pclx")
		if err != nil {
			linuxLiveHost.err = err
			return
		}
		testMainCleanups = append(testMainCleanups, func() { _ = os.RemoveAll(base) })
		if base, err = filepath.EvalSymlinks(base); err != nil {
			linuxLiveHost.err = err
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
				linuxLiveHost.err = err
				return
			}
		}
		linuxLiveHost.dirs = dirs
	})
	if linuxLiveHost.err != nil {
		t.Fatalf("live confinement host directories: %v", linuxLiveHost.err)
	}
	dirs := linuxLiveHost.dirs
	return &dirs
}

// linuxLiveAvailable reports whether this host can run a confined pi seat
// at all: the backend exists and its startup check passes. The test skips —
// never passes silently — where user namespaces are blocked, the launcher
// is missing, or the Landlock stage is unavailable, with the diagnostic the
// check carries.
func linuxLiveAvailable(t *testing.T) bool {
	t.Helper()
	backend := piConfinementBackend()
	if backend == nil {
		t.Skip("no confinement backend for this operating system")
	}
	if err := backend.Check(); err != nil {
		t.Skipf("confinement unavailable here: %v", err)
	}
	return true
}

// linuxLiveWorld is one session workarea: a root holding a mutable leaf (a
// real git repository, as a confined leaf must own its .git directory), a
// read-only sibling, the workarea metadata directory, and a decoy outside
// the set.
type linuxLiveWorld struct {
	root, mut, ro, outside string
}

func newLinuxLiveWorld(t *testing.T) linuxLiveWorld {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := linuxLiveWorld{
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

func (w linuxLiveWorld) spec() agent.Spec {
	return agent.Spec{SessionName: "live", Cwd: w.mut, Prompt: "probe"}
}

// writeLinuxLiveHarness writes the fake harness and its per-test inputs
// into the session tmp — inside the writable set, where the boundary
// grants full access — and returns the harness path. A script outside the
// set would need its interpreter resolved through Landlock grants the
// boundary never gives; inside, the seat reads and executes its own tree.
func writeLinuxLiveHarness(t *testing.T, w linuxLiveWorld) string {
	t.Helper()
	dir := filepath.Join(w.mut, ".pi-probe-"+sanitizeStateKey(t.Name()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-pi")
	if err := os.WriteFile(bin, []byte(fakeLinuxHarnessScript), 0o755); err != nil { //nolint:gosec // G306: test fixture needs the exec bit.
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
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin
}

// linuxLiveProvider is the provider under test: the fake harness as the
// binary, the host requirement as given, the handshake token the fake
// echoes, and the shared throwaway host directories. Nothing here bypasses
// the gate.
func linuxLiveProvider(t *testing.T, bin string, requireConfinement bool) *Provider {
	t.Helper()
	return &Provider{binary: bin, opts: Options{
		HandshakeTimeout:   30 * time.Second,
		RequireConfinement: requireConfinement,
		handshakeToken:     testHandshakeToken,
		confinementDirs:    linuxLiveConfinementDirs(t),
	}}
}

// linuxLiveCtx bounds one entry-point call, including the one-time
// self-test.
func linuxLiveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// finishLinuxHeadless reads the session to its terminal event, then stops
// it.
func finishLinuxHeadless(t *testing.T, h agent.Handle) {
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

// finishLinuxInteractive waits for the PTY child to exit.
func finishLinuxInteractive(t *testing.T, h agent.Handle) {
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

// linuxProbeReport is the parsed report the fake harness wrote.
type linuxProbeReport struct {
	raw  string
	vals map[string]string
}

func readLinuxProbeReport(t *testing.T, w linuxLiveWorld) linuxProbeReport {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(w.mut, "report.txt"))
	if err != nil {
		t.Fatalf("read probe report: %v", err)
	}
	r := linuxProbeReport{raw: string(body), vals: map[string]string{}}
	for _, line := range strings.Split(r.raw, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && !strings.ContainsAny(key, " :/") {
			r.vals[key] = value
		}
	}
	return r
}

// resetLinuxProbe clears what a previous run left, so a control run's
// report is its own.
func resetLinuxProbe(w linuxLiveWorld) {
	for _, path := range []string{
		filepath.Join(w.mut, "report.txt"),
		filepath.Join(w.mut, "fds.txt"),
		filepath.Join(w.outside, "planted.txt"),
		filepath.Join(w.ro, "planted.txt"),
	} {
		_ = os.Remove(path)
	}
}

// assertLinuxConfined checks a confined run: the write inside the leaf
// landed; the writes outside the set, into the read-only sibling and into
// the protected boundary extension were refused and left nothing behind;
// the session tmp and cache variables point into the session state root,
// and the write into the session tmp landed.
func assertLinuxConfined(t *testing.T, w linuxLiveWorld) {
	t.Helper()
	r := readLinuxProbeReport(t, w)
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
	assertLinuxNoLeakedDescriptors(t, w)
}

// assertLinuxUnconfined checks a control run through the same entry point
// without a confinement request: the same writes succeed. If they cannot,
// the confined assertions prove nothing about the confinement.
func assertLinuxUnconfined(t *testing.T, w linuxLiveWorld) {
	t.Helper()
	r := readLinuxProbeReport(t, w)
	for _, key := range []string{"inside", "outside", "ro", "extension"} {
		if r.vals[key] != "0" {
			t.Errorf("UNCONFINED control: the %s write failed, so the test cannot discriminate:\n%s", key, r.raw)
		}
	}
}

// assertLinuxNoLeakedDescriptors checks the descriptor listing the probe
// left: every descriptor the confined child holds must resolve to a
// standard stream, the descriptor directory itself, or a file inside the
// mutable leaf (the listing's own output redirect) — never a file outside
// the set. The headless child inherits the three stdio pipes and the
// interactive child the PTY slave; anything beyond those would be a
// descriptor open on a file the boundary does not judge.
func assertLinuxNoLeakedDescriptors(t *testing.T, w linuxLiveWorld) {
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
		// A bare number is a standard stream or a pipe the listing
		// itself holds; /dev/fd and /proc/<pid>/fd are the descriptor
		// directory the listing reads (on Linux ls shows its target,
		// /proc/self/fd); anything under the mutable leaf is the
		// listing's own output redirect.
		if _, perr := strconv.Atoi(strings.TrimSpace(name)); perr == nil ||
			strings.HasPrefix(name, "/dev/fd") ||
			(strings.HasPrefix(name, "/proc/") && strings.HasSuffix(name, "/fd")) ||
			strings.HasPrefix(name, w.mut+"/") {
			continue
		}
		t.Errorf("confined child holds descriptor open on %q (full list %q): only stdio may be inherited", name, list)
	}
}

// TestPiConfinementLinux_HeadlessSpawnRefusesForbiddenWrites is the headless
// half of Done-when through Provider.Spawn: a host that requires
// confinement confines the session under the mount-namespace backend, and
// the forbidden writes are refused; the same Spawn without the requirement
// runs unconfined and the writes succeed.
func TestPiConfinementLinux_HeadlessSpawnRefusesForbiddenWrites(t *testing.T) {
	if !linuxLiveAvailable(t) {
		return
	}
	w := newLinuxLiveWorld(t)
	bin := writeLinuxLiveHarness(t, w)

	h, err := linuxLiveProvider(t, bin, true).Spawn(linuxLiveCtx(t), w.spec())
	if err != nil {
		t.Fatalf("confined Spawn: %v", err)
	}
	finishLinuxHeadless(t, h)
	assertLinuxConfined(t, w)

	resetLinuxProbe(w)
	h, err = linuxLiveProvider(t, bin, false).Spawn(linuxLiveCtx(t), w.spec())
	if err != nil {
		t.Fatalf("unconfined Spawn: %v", err)
	}
	finishLinuxHeadless(t, h)
	assertLinuxUnconfined(t, w)
}

// TestPiConfinementLinux_HeadlessResumeRefusesForbiddenWrites is the same
// through Provider.Resume: a resume is a new process and is re-confined,
// never run unconfined.
func TestPiConfinementLinux_HeadlessResumeRefusesForbiddenWrites(t *testing.T) {
	if !linuxLiveAvailable(t) {
		return
	}
	w := newLinuxLiveWorld(t)
	bin := writeLinuxLiveHarness(t, w)

	h, err := linuxLiveProvider(t, bin, true).Resume(linuxLiveCtx(t), "ses_live", w.spec())
	if err != nil {
		t.Fatalf("confined Resume: %v", err)
	}
	finishLinuxHeadless(t, h)
	assertLinuxConfined(t, w)

	resetLinuxProbe(w)
	h, err = linuxLiveProvider(t, bin, false).Resume(linuxLiveCtx(t), "ses_live", w.spec())
	if err != nil {
		t.Fatalf("unconfined Resume: %v", err)
	}
	finishLinuxHeadless(t, h)
	assertLinuxUnconfined(t, w)
}

// TestPiConfinementLinux_InteractiveSpawnRefusesForbiddenWrites is the
// interactive half of Done-when: Provider.Spawn with an interactive spec
// takes the PTY path, and the same refusals hold there.
func TestPiConfinementLinux_InteractiveSpawnRefusesForbiddenWrites(t *testing.T) {
	if !linuxLiveAvailable(t) {
		return
	}
	w := newLinuxLiveWorld(t)
	bin := writeLinuxLiveHarness(t, w)
	spec := w.spec()
	spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}

	h, err := linuxLiveProvider(t, bin, true).Spawn(linuxLiveCtx(t), spec)
	if err != nil {
		t.Fatalf("confined interactive Spawn: %v", err)
	}
	finishLinuxInteractive(t, h)
	assertLinuxConfined(t, w)

	resetLinuxProbe(w)
	h, err = linuxLiveProvider(t, bin, false).Spawn(linuxLiveCtx(t), spec)
	if err != nil {
		t.Fatalf("unconfined interactive Spawn: %v", err)
	}
	finishLinuxInteractive(t, h)
	assertLinuxUnconfined(t, w)
}

// TestPiConfinementLinux_UnsupportedHostRefusesReadOnlyDeclaration pins the
// clean refusal on a host that cannot confine: a session that declares a
// repository authority — whose read-only leaves need the executor boundary
// — is refused with a typed confinement reason, never run unconfined, when
// the backend's startup check fails (for example where user namespaces are
// blocked). The refused spawn must leave no seat behind: nothing is planted
// outside the set, and the child never runs.
func TestPiConfinementLinux_UnsupportedHostRefusesReadOnlyDeclaration(t *testing.T) {
	backend := piConfinementBackend()
	if backend == nil {
		t.Skip("no confinement backend for this operating system")
	}
	if err := backend.Check(); err == nil {
		t.Skip("this host can confine; the refusal needs one that cannot")
	} else {
		t.Logf("host check fails as the test needs: %v", err)
	}
	w := newLinuxLiveWorld(t)
	bin := writeLinuxLiveHarness(t, w)
	spec := w.spec()
	spec.RepositoryAuthority = &agent.RepositoryAuthorityPolicy{
		Protocol:      "session-root-v1",
		WorkareaRoot:  w.root,
		SelectedPath:  w.mut,
		MutablePaths:  []string{w.mut},
		ReadOnlyPaths: []string{w.ro},
	}
	before := snapshotLinuxTree(t, w.outside)
	// Enter at launch, the shared post-admission entry of Spawn and Resume,
	// with the gate still evaluated on the spec: admission owns the
	// authority protocol independent of the host, while the refusal under
	// test is the host's inability to confine.
	h, err := linuxLiveProvider(t, bin, false).launch(linuxLiveCtx(t), spec, launchPrompt, "")
	if err == nil {
		_ = h.Stop(context.Background())
		t.Fatal("a read-only-declared session started on a host that cannot confine")
	}
	if reason, ok := confinement.ReasonOf(err); !ok || !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("refused without a typed confinement reason: %v", err)
	} else {
		t.Logf("refused with confinement reason %s", reason)
	}
	if after := snapshotLinuxTree(t, w.outside); after != before {
		t.Errorf("the parent wrote outside the set before refusing.\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Lstat(filepath.Join(w.mut, "report.txt")); err == nil {
		t.Error("the child ran on a host that cannot confine")
	}
}

// snapshotLinuxTree renders every entry under dir — kind, link target or
// content digest — so any creation or modification shows as a difference.
func snapshotLinuxTree(t *testing.T, dir string) string {
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
	return strings.Join(lines, "\n")
}
