//go:build linux

package confinement

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/RenseiAI/donmai/agent"
)

// mountNamespaceProfileVersion is the Linux mount-namespace backend's
// implementation version. It is part of the backend version, so a self-test
// record taken under an older launcher, mount-tree shape or Landlock stage
// is stale.
const mountNamespaceProfileVersion = "mount-namespace-v5"

// DefaultBackend returns the confinement backend for the running OS: the
// Linux mount-namespace backend here.
func DefaultBackend() Backend { return &mountNamespaceBackend{} }

// ResolverSockets returns the OS resolver sockets a harness that needs name
// resolution declares in Spec.Sockets: none here. The NSS stack reads the
// static resolver configuration and the host database, which every boundary
// binds read-only and the Landlock stage grants read access
// (mountNamespaceResolverBinds), whether or not a harness declares them.
// Declaring /etc/resolv.conf would also refuse every spawn on a host where
// it is a link (systemd-resolved points it at its stub file): a declared
// socket must not be a symbolic link.
func ResolverSockets() []string { return nil }

// sharedLocations are the shared temporary locations the boundary hides: a
// confined seat reaches its own per-session tmp through the TMPDIR binding,
// never the host's shared directories.
func sharedLocations() ([]string, error) {
	return []string{"/tmp", "/var/tmp"}, nil
}

type mountNamespaceBackend struct {
	// launcherOverride pins the launcher executable, for tests. Empty
	// resolves bubblewrap on PATH; a missing launcher refuses with
	// backend_absent, never a silent unwrapped run.
	launcherOverride string
}

func (b *mountNamespaceBackend) Name() BackendName { return BackendLinuxMountNamespace }

// leavesLoopbackEgressOpen declares the boundary's network shape: outbound
// TCP is never filtered, to the local machine or anywhere else. The
// Landlock stage handles no network right (a port rule carries no address,
// so handling TCP connects would cut every undeclared port on every
// address, remote model endpoints and ordinary fetches included), and the
// network namespace stays shared (a private one would strand the host
// loopback and every external endpoint with it). The self-test holds the
// backend to this declaration: its undeclared loopback dials must go
// through.
func (b *mountNamespaceBackend) leavesLoopbackEgressOpen() bool { return true }

func (b *mountNamespaceBackend) Version() (string, error) {
	release, err := linuxRelease()
	if err != nil {
		return "", err
	}
	return mountNamespaceProfileVersion + "+linux-" + release, nil
}

// Check refuses with nested_sandbox when this process already runs inside a
// mount-namespace boundary — whether or not a second boundary would apply,
// the harness must never run under an outer boundary while the inner level
// is reported — with backend_absent when the launcher or the Landlock stage
// is missing, and with namespace_unavailable when the launcher cannot build
// a boundary on this host, so a seat is never silently run unconfined.
func (b *mountNamespaceBackend) Check() error {
	if insideMountNamespace() {
		return refuse(ReasonNestedSandbox, "this process is already inside a mount-namespace boundary")
	}
	launcher, err := resolveLauncher(b.launcher())
	if err != nil {
		return err
	}
	if err := checkLandlock(); err != nil {
		return err
	}
	return probeLauncher(launcher)
}

// insideMountNamespace reports whether this process already runs inside a
// mount-namespace boundary: the helper marks every boundary it builds, and a
// mount namespace that differs from init's marks one built by any tool, so
// a harness process (or a worker wrapped by mistake) cannot build a second
// one and report the inner level for the outer.
func insideMountNamespace() bool {
	if os.Getenv(mountNamespaceMarkEnv) != "" {
		return true
	}
	outer, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil || outer == "" {
		return false
	}
	inner, err := os.Readlink("/proc/1/ns/mnt")
	if err != nil || inner == "" {
		// No init namespace to compare against (a minimal container, or
		// an unprivileged process that may not inspect init): the marker
		// alone decides.
		return false
	}
	return outer != inner
}

// mountNamespaceMarkEnv marks a process running inside the boundary. The
// namespace comparison above is the real nesting check; the marker covers a
// container whose init shares this mount namespace, where the comparison
// cannot tell.
const mountNamespaceMarkEnv = "DONMAI_MOUNT_NAMESPACE"

// mountNamespaceFlags open every boundary: a fresh user, mount and process
// namespace per command, torn down with the command. In the private
// process namespace the launcher runs an init that reaps orphans, and no
// process outside the boundary is visible to signal, attach to or read
// (ADR-2026-10-03 D3.1). The launcher pid the supervisor holds stays
// outside, and killing it kills the namespace's init, which takes every
// process inside with it, descendants that left the session included. The
// network namespace stays shared (see leavesLoopbackEgressOpen).
var mountNamespaceFlags = []string{"--unshare-user", "--unshare-pid", "--die-with-parent"}

// sessionFlags are the namespace flags for one session mode. A headless
// harness runs in a session of its own, so it cannot push input into a
// terminal outside. An interactive harness keeps the session the PTY host
// gave the launcher: that terminal is the session's own, and the harness
// needs it as its controlling terminal for job control and Ctrl-C (the
// stage makes the harness its foreground process group; see
// takeForeground).
func sessionFlags(mode agent.PromptSessionMode) []string {
	flags := append([]string{}, mountNamespaceFlags...)
	if mode != agent.PromptModeHumanControlled {
		flags = append(flags, "--new-session")
	}
	return flags
}

// launcherProbeTargets are the fixed executables the capability probe runs
// inside its boundary: the first that exists is used. Both are constants,
// never session or probe input.
var launcherProbeTargets = []string{"/usr/bin/true", "/bin/true"}

// launcherProbeTimeout bounds the capability probe: building a namespace
// and exiting takes milliseconds, so a hang means the host cannot.
const launcherProbeTimeout = 15 * time.Second

// probeLauncher runs the launcher once with the namespace flags, the tmpfs
// root, the OS userland binds, a fresh proc and a private /dev — the
// operations every spawn performs — around a fixed no-op executable. It
// answers what a bare user-namespace probe cannot: whether this host lets
// an unprivileged launcher mount inside its namespace (a security module
// may allow the namespace and deny the mounts, and a container's masked
// /proc refuses a fresh proc). A failure refuses with namespace_unavailable
// carrying the launcher's own diagnostic, never a silent pass.
//
// The probe runs in a child process the launcher creates: the worker never
// unshares anything itself, so its own namespaces and identity stay intact
// for the artifact, receipt and control-channel I/O it still owns.
func probeLauncher(launcher string) error {
	target := ""
	for _, candidate := range launcherProbeTargets {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			target = candidate
			break
		}
	}
	if target == "" {
		return refuse(ReasonNamespaceUnavailable, "the boundary cannot be probed: no probe executable (%s) exists", strings.Join(launcherProbeTargets, ", "))
	}
	ctx, cancel := context.WithTimeout(context.Background(), launcherProbeTimeout)
	defer cancel()
	// launcher is resolveLauncher's absolute path, stat-checked as a regular
	// executable (bubblewrap from the operator's PATH, or a test override);
	// the arguments are fixed flags, the OS userland paths and a constant
	// probe target — no session or probe input reaches this command.
	cmd := exec.CommandContext(ctx, launcher, launcherProbeArgs(target)...) //nolint:gosec // G204: resolved, stat-checked launcher with fixed arguments (see above).
	var diagnostic boundedBuffer
	cmd.Stdout = &diagnostic
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		return refuse(ReasonNamespaceUnavailable, "the mount-namespace launcher cannot build a boundary here: %s", launcherHint(err, diagnostic.String()))
	}
	return nil
}

// launcherProbeArgs renders the capability probe's boundary around target.
func launcherProbeArgs(target string) []string {
	args := sessionFlags(agent.PromptModeAutonomous)
	args = append(args, "--tmpfs", "/")
	for _, path := range mountNamespaceRuntimeBinds {
		if _, err := os.Lstat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	args = append(args, "--proc", "/proc", "--tmpfs", "/dev", "--dev-bind", "/dev/null", "/dev/null", "--", target)
	return args
}

// launcherHint renders the actionable diagnostic for a launcher that could
// not build a boundary: its own first line of output, plus where to look
// when the refusal is a permission one.
func launcherHint(err error, output string) string {
	line := strings.TrimSpace(output)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		line = errnoText(err)
	}
	if strings.Contains(line, "Permission denied") || strings.Contains(line, "Operation not permitted") || strings.Contains(line, "No permissions") {
		return line + " (the kernel or a security module disallows unprivileged user namespaces or the mounts inside them: on Ubuntu 23.10+ check the AppArmor user-namespace restriction (kernel.apparmor_restrict_unprivileged_userns) or install an AppArmor profile for bwrap; in a container the runtime must allow user namespaces and an unmasked /proc)"
	}
	return line
}

// boundedBuffer keeps the first bytes a probe writes: enough for a
// diagnostic line, never an unbounded capture.
type boundedBuffer struct{ data []byte }

const boundedBufferLimit = 512

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := boundedBufferLimit - len(b.data); room > 0 {
		b.data = append(b.data, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return string(b.data) }

// Canonical resolves symbolic links, so a path is spelled the way the mount
// tree matches it.
func (b *mountNamespaceBackend) Canonical(path string) (string, error) {
	return canonicalLinux(path)
}

func canonicalLinux(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

func (b *mountNamespaceBackend) Apply(req ApplyRequest) (Applied, error) {
	if req.Resolved == nil {
		return Applied{}, refuse(ReasonWritableSetUnrepresentable, "no resolved set to apply")
	}
	launcher, err := resolveLauncher(b.launcher())
	if err != nil {
		return Applied{}, err
	}
	if err := checkLandlock(); err != nil {
		return Applied{}, err
	}
	stage, err := buildLandlockStage(req.Resolved)
	if err != nil {
		return Applied{}, err
	}
	self, err := stageSelfExecutable()
	if err != nil {
		return Applied{}, err
	}
	args, err := renderBubblewrap(req.Resolved, req.Rules, b.Canonical, self)
	if err != nil {
		return Applied{}, err
	}
	resolved := req.Resolved
	rendered := "bubblewrap\n" + launcher + "\n" + stage.describe() + "\n" + strings.Join(args, "\n") + "\n"
	policy := stage.describe()
	return Applied{
		Rendered: []byte(rendered),
		Wrap: func(argv []string) []string {
			tree := append([]string{}, args...)
			tree = append(tree, execBinds(argv, self, resolved)...)
			wrapped := append([]string{launcher}, append(tree, "--")...)
			wrapped = append(wrapped, self, "stage")
			wrapped = append(wrapped, stagePortArgs(stage)...)
			wrapped = append(wrapped, "--")
			wrapped = append(wrapped, argv...)
			return wrapped
		},
		Environ: func() []string {
			return []string{landlockStageEnv + "=" + policy}
		},
	}, nil
}

// landlockProbe returns the running kernel's Landlock ABI version, 0 where
// Landlock is absent or disabled. A package variable so tests pin both
// branches of the gate without a capable kernel; production always probes
// the running kernel.
var landlockProbe = landlockABI

// checkLandlock refuses with backend_absent unless the running kernel's
// Landlock ABI carries what the stage needs (landlockMinABI): the stage is
// the half of the boundary that confines reads, and a seat is never run
// with a weaker boundary than rendered.
func checkLandlock() error {
	abi := landlockProbe()
	if abi >= landlockMinABI {
		return nil
	}
	if abi <= 0 {
		return refuse(ReasonBackendAbsent, "the Landlock stage is unavailable: this kernel has no Landlock (Linux 5.19+ with the landlock security module enabled is needed)")
	}
	return refuse(ReasonBackendAbsent, "the Landlock stage is unavailable: this kernel offers Landlock ABI %d, and ABI %d (Linux 5.19+) is needed", abi, landlockMinABI)
}

// execBinds renders the read-only binds Wrap adds for the executables the
// spawn re-executes inside the mount tree: the harness binary argv[0]. The
// stage executable itself is bound at render time (see renderBubblewrap);
// argv[0] is only known here, and the mount options must precede the first
// "--" separator, so Wrap splices these binds into the tree it closes
// over. A binary already reachable (the stage itself, or anything inside
// the writable set) needs no second bind: re-binding a writable root
// read-only would shadow it. A missing parent is created with --dir first,
// so the render never names a destination the launcher cannot mount onto.
// A binary that cannot be canonicalized still renders its absolute path:
// the launcher then fails closed instead of running unwrapped.
func execBinds(argv []string, self string, r *Resolved) []string {
	if len(argv) == 0 || argv[0] == "" {
		return nil
	}
	binary := argv[0]
	if !filepath.IsAbs(binary) {
		return nil
	}
	if binary == self {
		return nil
	}
	for _, root := range r.Writable {
		if insideOrEqual(binary, root.Path) {
			return nil
		}
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
		if binary == self {
			return nil
		}
		for _, root := range r.Writable {
			if insideOrEqual(binary, root.Path) {
				return nil
			}
		}
	}
	var args []string
	if parent := filepath.Dir(binary); parent != "/" && parent != "." {
		args = append(args, "--dir", parent)
	}
	return append(args, "--ro-bind", binary, binary)
}

// launcher resolves the helper executable: the test override, else
// bubblewrap on PATH. Apply never renders a boundary it cannot launch:
// resolveLauncher refuses with backend_absent when it is missing, and
// returns the absolute path so the spawn cannot be redirected by a later
// PATH change between render and exec.
func (b *mountNamespaceBackend) launcher() string {
	if b.launcherOverride != "" {
		return b.launcherOverride
	}
	return "bwrap"
}

// resolveLauncher resolves the launcher to an absolute executable path, so
// a seat is never run unwrapped and never through a PATH lookup at spawn.
// A bare name is resolved on PATH once, at Apply; an absolute test
// override must already name an executable file.
func resolveLauncher(name string) (string, error) {
	if filepath.IsAbs(name) {
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", refuse(ReasonBackendAbsent, "the mount-namespace launcher is missing")
		}
		return name, nil
	}
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		// PATH entries are the operator's own launch environment, and the candidate must stat as a regular executable file before it is used.
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate) //nolint:gosec // G703: PATH join over the operator's launch environment; the result is checked as a regular executable below.
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", refuse(ReasonBackendAbsent, "the mount-namespace launcher is missing")
}

// checkLauncher refuses with backend_absent when the launcher is missing, so
// a seat is never run unwrapped. It is the boolean gate over
// resolveLauncher, kept for callers that only need the verdict.
func checkLauncher(name string) error {
	_, err := resolveLauncher(name)
	return err
}

var (
	linuxReleaseOnce  sync.Once
	linuxReleaseValue string
	linuxReleaseErr   error
)

func linuxRelease() (string, error) {
	linuxReleaseOnce.Do(func() {
		var uts unix.Utsname
		if err := unix.Uname(&uts); err != nil {
			linuxReleaseErr = err
			return
		}
		linuxReleaseValue = unix.ByteSliceToString(uts.Release[:])
		if linuxReleaseValue == "" {
			linuxReleaseErr = errors.New("empty kernel release")
		}
	})
	return linuxReleaseValue, linuxReleaseErr
}

// mountNamespaceRuntimeBinds are the runtime and toolchain paths bound
// read-only into every boundary: the OS userland and toolchains, the CA
// bundle and timezone data TLS and name resolution need. Entries that do not
// exist on the host are skipped at render; the session's own allowlist is
// bound after, so it wins over nothing here — these paths never overlap the
// writable set, which resolveSpec already guarantees.
//
// Device nodes, /proc and /sys are deliberately absent: /dev is a fresh
// tmpfs populated below with --dev-bind device nodes (a plain --bind of a
// device node does not carry it through), /proc is a fresh proc mount so no
// host process table leaks in, and /sys stays hidden behind the tmpfs root.
var mountNamespaceRuntimeBinds = []string{
	"/usr",
	"/bin",
	"/sbin",
	"/lib",
	"/lib64",
	"/etc/ssl",
	// The CA bundle on Fedora and RHEL-family hosts: /etc/ssl/certs there
	// is a link into it, so TLS fails without it.
	"/etc/pki",
	"/etc/ca-certificates",
	"/etc/gitconfig",
	"/etc/timezone",
	"/etc/localtime",
	"/usr/share/zoneinfo",
}

// mountNamespaceResolverBinds are the resolver inputs bound read-only into
// every boundary: the static configuration NSS reads and the host database
// NSS consults for the loopback names the loopback probes dial. Declared
// session sockets are bound after them.
var mountNamespaceResolverBinds = []string{
	"/etc/resolv.conf",
	"/etc/nsswitch.conf",
	"/etc/hosts",
}

// renderBubblewrap renders the session's boundary as a bubblewrap argument
// list. The bind order is the boundary:
//
//  1. a tmpfs root hides everything no bind names;
//  2. read-only binds reveal the OS userland, the resolver inputs, the
//     operator home and the host state home (so file metadata stays
//     readable), the declared read paths under a read scope, the declared
//     sockets and the stage executable;
//  3. a private /dev and a fresh /proc;
//  4. the writable set, bound read-write over them;
//  5. rename anchors: every ancestor of a write deny strictly inside a
//     writable root is bound read-write over itself, which makes it a
//     mount point no rename or removal can move (a mount follows its
//     directory, so nothing beneath it can be moved out from under its
//     deny either);
//  6. write denies: read-only leaves, protected paths, the workarea
//     metadata (with reads open) and composer write denies, each bound
//     read-only over itself — readable, never writable, never renamed;
//  7. hide denies: composer read denies, an empty placeholder bound over
//     each path some bind above would otherwise reveal (hideDenies is the
//     one seam a further read-and-write deny list plugs into);
//  8. the boundary marker.
//
// The namespace flags open it (sessionFlags): private user, mount and
// process namespaces, and a session of its own for a headless harness.
// Any rule the mount tree cannot express refuses the whole rendering; none
// is dropped. The harness process re-executed as the Landlock stage (see
// stage_linux.go) programs the Landlock policy from inside the mount tree
// and then execs the harness: Wrap runs `launcher args -- self-exe stage
// -- argv`. self is the stage executable's own canonical path: it is bound
// read-only into the tree here, because the root is a tmpfs and the
// executable would otherwise be absent inside. An empty self skips the
// bind; Apply always passes the real one. Returned args end before the
// first "--" separator; Wrap appends it.
func renderBubblewrap(r *Resolved, rules []Rule, canonical func(string) (string, error), self string) ([]string, error) {
	composer, err := planMountComposerRules(r, rules, canonical)
	if err != nil {
		return nil, err
	}
	if r.ReadScope != "" && r.ReadScope != agent.FileReadWorkarea {
		return nil, refuse(ReasonWritableSetUnrepresentable, "unknown read scope %q", r.ReadScope)
	}
	t := &mountTree{writable: r.Writable}
	t.add(sessionFlags(r.SessionMode)...)
	t.add("--tmpfs", "/")
	for _, path := range mountNamespaceRuntimeBinds {
		t.reveal(path)
	}
	for _, path := range mountNamespaceResolverBinds {
		t.reveal(path)
	}
	// The operator home and the host state home are bound read-only so
	// file metadata stays readable, as the seatbelt contract promises:
	// path lookups and stat keep working there. File contents and
	// directory listings are governed by the Landlock stage, which grants
	// read access there with reads open and nothing under a read scope.
	for _, path := range []string{r.Home, r.StateHome} {
		if path != "" {
			t.revealUnder(path)
		}
	}
	// Declared read paths bind before the writable set, so a read path
	// that covers a writable root never buries its read-write bind. A read
	// path inside the writable set is already revealed.
	if r.ReadScope != "" {
		for _, path := range r.ReadPaths {
			if !t.insideWritable(path) {
				t.reveal(path)
			}
		}
	}
	// Declared sockets outside the writable set: bound read-only by exact
	// path, so name resolution and the control channel keep working — a
	// connect through a read-only bind succeeds. Every other socket outside
	// the set has no bind and stays unreachable.
	for _, socket := range r.Sockets {
		if !t.insideWritable(socket) {
			t.reveal(socket)
		}
	}
	// The stage executable re-executes inside the tree (see Apply): bind it
	// read-only by exact path, with its parent created first. Without this
	// the tmpfs root hides it and the launcher cannot exec the stage.
	if self != "" {
		t.revealUnder(self)
	}
	// Device nodes a process needs: /dev is a fresh tmpfs, so the host's
	// terminals never enter the boundary. --dev-bind (not --bind) carries
	// device nodes through: a plain bind of a device node does not work.
	// The Landlock stage grants open on this private /dev; nothing else
	// there exists to open.
	t.add("--tmpfs", "/dev")
	// /dev/tty names the opener's own controlling terminal: an
	// interactive harness reaches its session's terminal through it, and a
	// headless one, which has none, gets ENXIO.
	for _, path := range []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/random", "/dev/tty"} {
		if _, err := os.Lstat(path); err == nil {
			t.add("--dev-bind", path, path)
		}
	}
	// The descriptor conduit shells need for process substitution.
	t.add("--symlink", "/proc/self/fd", "/dev/fd")
	// A fresh proc mount: the host's process table stays outside, and
	// /proc/self/fd (behind /dev/fd) resolves inside.
	t.add("--proc", "/proc")
	// The writable set: mutable leaves, harness state, session tmp and
	// session caches, bound read-write over the read-only tree.
	for _, root := range r.Writable {
		if _, err := os.Lstat(root.Path); err != nil {
			return nil, refuse(ReasonWritableSetUnrepresentable, "writable root is not reachable: %v", errnoText(err))
		}
		t.add("--bind", root.Path, root.Path)
		t.revealed = append(t.revealed, root.Path)
	}
	// Rename anchors, shallowest first, before any write deny beneath them
	// is bound: a later read-write bind of an ancestor would bury the
	// read-only bind of the deny inside it.
	anchors := append(append([]string{}, r.Pins...), composer.anchors...)
	sort.Strings(anchors)
	seen := map[string]bool{}
	for _, anchor := range anchors {
		if seen[anchor] || !t.strictlyInsideWritable(anchor) || isWritableRoot(r.Writable, anchor) {
			continue
		}
		seen[anchor] = true
		if _, err := os.Lstat(anchor); err == nil {
			t.add("--bind", anchor, anchor)
		}
	}
	// Write denies, after every read-write bind: each is bound read-only
	// over itself, so its contents stay readable while every write,
	// create, rename, remove, permission, timestamp and extended-attribute
	// change fails, and a hard link never crosses into it. A deny that does
	// not exist yet needs no bind: outside the writable set nothing can
	// create it, and the Landlock stage grants nothing there.
	//
	// Read-only leaves are revealed wherever they live: they are readable
	// under both read levels. The workarea metadata is readable with reads
	// open and hidden under a read scope, like on the profile backend.
	// Protected paths and composer write denies only need the bind where a
	// writable root would otherwise show them writable; anywhere else the
	// tree already holds them read-only or hidden.
	for _, path := range r.ReadOnly {
		t.reveal(path)
	}
	for _, path := range r.Protected {
		if t.strictlyInsideWritable(path) {
			t.reveal(path)
		}
	}
	if r.ReadScope == "" || t.strictlyInsideWritable(r.MetadataDir) {
		t.reveal(r.MetadataDir)
	}
	for _, path := range composer.writeDenies {
		if t.strictlyInsideWritable(path) || insideOrEqual(path, "/dev") {
			t.reveal(path)
		}
	}
	// Hide denies last, after every bind that could reveal them, outermost
	// first: a path inside one already hidden is covered by it, and the
	// placeholder over the outer one could not take a mount point anyway.
	hides := hideDenies(composer)
	sort.Strings(hides)
	for _, path := range hides {
		if err := t.hide(path); err != nil {
			return nil, err
		}
	}
	// The boundary marks itself: the nested-sandbox check reads it back.
	t.add("--setenv", mountNamespaceMarkEnv, "1")
	return t.args, nil
}

// hideDenies are the paths the boundary hides on read as well as on write
// wherever a bind would reveal them: the composer's read denies. This is
// the one seam a further deny list that must win on read as well as on
// write (secrets a seat may neither read nor change) plugs into: its paths
// join this list and render the same way, after every allow.
//
// A path must exist at spawn to be hidden: a placeholder needs something
// to mount over, and creating one would write into the host. A path minted
// later under a read-only bind is created by the host alone, and one minted
// inside the writable set by the seat; a deny list that must cover a
// secret not yet minted names the directory it will be minted in, which
// hides everything later created there.
func hideDenies(composer mountComposerPlan) []string {
	return append([]string(nil), composer.hideDenies...)
}

// mountTree accumulates a bubblewrap argument list and the host paths its
// binds reveal inside the boundary, so a hide deny knows whether anything
// would show its path and whether hiding it would bury another bind.
type mountTree struct {
	args     []string
	revealed []string
	hidden   []string
	writable []WritableRoot
}

func (t *mountTree) add(args ...string) { t.args = append(t.args, args...) }

// reveal binds path read-only over itself when it exists. Binding the host
// path at its own spelling keeps every rule — the mount tree's and the
// Landlock stage's — naming the same path inside and out.
func (t *mountTree) reveal(path string) {
	if path == "" {
		return
	}
	if _, err := os.Lstat(path); err != nil {
		return
	}
	t.add("--ro-bind", path, path)
	t.revealed = append(t.revealed, path)
}

// revealUnder is reveal with the parent directory created first, for a
// path whose parent no earlier bind provides.
func (t *mountTree) revealUnder(path string) {
	if _, err := os.Lstat(path); err != nil {
		return
	}
	if parent := filepath.Dir(path); parent != "/" && parent != "." {
		t.add("--dir", parent)
	}
	t.reveal(path)
}

// insideWritable reports whether path is a writable root or below one.
func (t *mountTree) insideWritable(path string) bool {
	for _, root := range t.writable {
		if insideOrEqual(path, root.Path) {
			return true
		}
	}
	return false
}

// strictlyInsideWritable reports whether path sits strictly inside a
// writable root, where only a bind of its own keeps it read-only.
func (t *mountTree) strictlyInsideWritable(path string) bool {
	for _, root := range t.writable {
		if strictlyInside(path, root.Path) {
			return true
		}
	}
	return false
}

// hide binds an empty placeholder over path where some bind reveals it, so
// its contents read as nothing and writes fail. A path no bind reveals is
// already hidden by the tmpfs root, and the Landlock stage grants nothing
// there; a path that does not exist has nothing to hide; a path inside one
// already hidden is hidden with it. A hide that would
// bury another bind (a writable root, a read-only leaf, a socket) cannot be
// rendered as declared, and refuses instead of shadowing what the session
// needs.
func (t *mountTree) hide(path string) error {
	for _, hidden := range t.hidden {
		if insideOrEqual(path, hidden) {
			return nil
		}
	}
	for _, revealed := range t.revealed {
		if strictlyInside(revealed, path) {
			return refuse(ReasonRuleUnrenderable, "a read deny over a path the boundary binds below it cannot be rendered")
		}
	}
	for _, root := range t.writable {
		if insideOrEqual(root.Path, path) {
			return refuse(ReasonRuleUnrenderable, "a read deny over the writable set cannot be rendered")
		}
	}
	revealed := false
	for _, base := range t.revealed {
		if insideOrEqual(path, base) {
			revealed = true
			break
		}
	}
	if !revealed {
		return nil
	}
	if _, err := os.Lstat(path); err != nil {
		return nil
	}
	empty, _, err := emptyPlaceholder(path)
	if err != nil {
		return err
	}
	t.add("--ro-bind", empty, path)
	t.hidden = append(t.hidden, path)
	return nil
}

// mountComposerPlan is the composer's deny-only rules sorted by how the
// mount tree renders them.
type mountComposerPlan struct {
	// writeDenies are bound read-only over themselves (see renderBubblewrap).
	writeDenies []string
	// hideDenies are hidden behind an empty placeholder.
	hideDenies []string
	// anchors are the ancestors of the write and read denies inside the
	// writable set, made rename-proof mount points.
	anchors []string
}

// planMountComposerRules validates the composer's deny-only rules and sorts
// them for the render: a write deny holds the path read-only, a read deny
// hides it. Any rule the mount tree cannot express refuses the whole
// rendering; none is dropped. Both scopes render the same way: a mount
// covers the subtree, which is never narrower than a literal deny.
func planMountComposerRules(r *Resolved, rules []Rule, canonical func(string) (string, error)) (mountComposerPlan, error) {
	var plan mountComposerPlan
	var writable []string
	for _, root := range r.Writable {
		writable = append(writable, root.Path)
	}
	for i, rule := range rules {
		switch rule.Kind {
		case RuleDenyRead, RuleDenyWrite:
			if rule.Service != "" || !filepath.IsAbs(rule.Path) {
				return plan, refuse(ReasonRuleUnrenderable, "composer rule %d: a path rule needs an absolute path and no service", i)
			}
			switch rule.Scope {
			case ScopeLiteral, ScopeSubtree:
			default:
				return plan, refuse(ReasonRuleUnrenderable, "composer rule %d: unknown scope %q", i, rule.Scope)
			}
			path, err := canonicalLoose(rule.Path, canonical)
			if err != nil {
				return plan, refuse(ReasonRuleUnrenderable, "composer rule %d: %v", i, err)
			}
			// A deny that covers a whole writable root cannot be confined
			// as declared: binding over it would shadow the bind the
			// session needs, and skipping it would drop the rule.
			for _, root := range r.Writable {
				if insideOrEqual(root.Path, path) {
					return plan, refuse(ReasonRuleUnrenderable, "composer rule %d: a deny over the writable set cannot be rendered", i)
				}
			}
			if rule.Kind == RuleDenyWrite {
				plan.writeDenies = append(plan.writeDenies, path)
			} else {
				plan.hideDenies = append(plan.hideDenies, path)
			}
			plan.anchors = append(plan.anchors, ancestorPins(writable, []string{path})...)
		case RuleDenyServiceLookup:
			if rule.Path != "" || rule.Scope != "" || !servicePattern.MatchString(rule.Service) {
				return plan, refuse(ReasonRuleUnrenderable, "composer rule %d: a service rule needs a plain service name and no path", i)
			}
			// No service namespace exists on Linux to close: the only
			// honest answer is to refuse the rendering, never to drop it.
			return plan, refuse(ReasonRuleUnrenderable, "composer rule %d: service lookup denies have no mount-tree rendering", i)
		default:
			return plan, refuse(ReasonRuleUnrenderable, "composer rule %d: unknown kind %q", i, rule.Kind)
		}
	}
	return plan, nil
}

// isWritableRoot reports whether path is itself a writable root, whose
// own read-write bind is already a mount point.
func isWritableRoot(writable []WritableRoot, path string) bool {
	for _, root := range writable {
		if root.Path == path {
			return true
		}
	}
	return false
}

// emptyPlaceholder returns a path whose bind hides the denied target: an
// empty directory for a directory target, an empty file otherwise, plus
// whether the target is a directory. The placeholder lives under a
// per-user directory in the OS temporary directory (0700, so one user's
// denied listing never shows through another user's placeholder), outside
// every session root. Callers skip targets
// that do not exist — the tmpfs root already hides those — so a missing
// target never reaches this function; it stays a caller-side decision,
// documented at each call site.
func emptyPlaceholder(target string) (string, bool, error) {
	info, err := os.Lstat(target)
	if err != nil {
		return "", false, refuse(ReasonWritableSetUnrepresentable, "deny target is not reachable: %v", errnoText(err))
	}
	base, err := emptyPlaceholderBase()
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		dir := filepath.Join(base, "dir")
		// The placeholder is an empty directory by construction; 0700 keeps it owner-only.
		if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // G301: an empty overlay directory.
			return "", false, refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
		}
		return dir, true, nil
	}
	file := filepath.Join(base, "file")
	// The placeholder parent holds only the empty file below; 0700 keeps it owner-only.
	if err := os.MkdirAll(base, 0o700); err != nil { //nolint:gosec // G301: the empty overlay parent.
		return "", false, refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
	}
	// The placeholder is an empty file by construction; 0600 keeps it owner-only.
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDONLY, 0o600) //nolint:gosec // G302: an empty overlay file.
	if err != nil {
		return "", false, refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
	}
	_ = f.Close()
	return file, false, nil
}

// emptyPlaceholderBase is the per-user directory holding the empty overlay
// paths: the OS temporary directory plus the caller's user id, created
// owner-only, so placeholders staged by one user are never listed through
// another user's denied bind. The suffix is numeric (never a name), so the
// path carries no identity.
func emptyPlaceholderBase() (string, error) {
	base := filepath.Join(os.TempDir(), fmt.Sprintf("donmai-confine-empty-%d", os.Getuid()))
	// The placeholder parent holds only the empty overlays below; 0700 keeps it owner-only.
	if err := os.MkdirAll(base, 0o700); err != nil { //nolint:gosec // G301: the empty overlay parent.
		return "", refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
	}
	return base, nil
}
