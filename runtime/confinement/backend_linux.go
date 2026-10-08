//go:build linux

package confinement

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/RenseiAI/donmai/agent"
)

// mountNamespaceProfileVersion is the Linux mount-namespace backend's
// implementation version. It is part of the backend version, so a self-test
// record taken under an older launcher, mount-tree shape or Landlock stage
// is stale.
const mountNamespaceProfileVersion = "mount-namespace-v2"

// DefaultBackend returns the confinement backend for the running OS: the
// Linux mount-namespace backend here.
func DefaultBackend() Backend { return &mountNamespaceBackend{} }

// ResolverSockets returns the OS resolver sockets a harness that needs name
// resolution declares in Spec.Sockets. The NSS stack reads the static
// resolver configuration and the host database directly, so those paths are
// what a harness declares; the mount tree binds them read-only and the
// Landlock stage grants them read access.
func ResolverSockets() []string { return []string{"/etc/resolv.conf"} }

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
// is reported — and with namespace_unavailable when unprivileged user
// namespaces cannot be created here, so a seat is never silently run
// unconfined.
func (b *mountNamespaceBackend) Check() error {
	if insideMountNamespace() {
		return refuse(ReasonNestedSandbox, "this process is already inside a mount-namespace boundary")
	}
	if err := probeUserNamespace(); err != nil {
		return err
	}
	return nil
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
		// No init namespace to compare against (a minimal container): the
		// marker alone decides.
		return false
	}
	return outer != inner
}

// mountNamespaceMarkEnv marks a process running inside the boundary. The
// namespace comparison above is the real nesting check; the marker covers a
// container whose init shares this mount namespace, where the comparison
// cannot tell.
const mountNamespaceMarkEnv = "DONMAI_MOUNT_NAMESPACE"

// probeUserNamespace reports whether this process can create an unprivileged
// user namespace right now. Where the kernel or the container disallows it
// the error carries namespace_unavailable with the actionable diagnostic,
// never a silent pass.
//
// The probe runs in a short-lived child: unsharing the caller would move the
// long-lived worker into a fresh, unmapped user namespace for the rest of
// its life, corrupting the artifact, receipt and control-channel I/O it
// still owns. The child unshares a user namespace the parent maps
// (CLONE_NEWUSER plus a single-uid/gid map of the caller's own identity,
// exactly what a spawn needs); the parent waits for its exit status, and
// the parent's own namespace is never touched.
func probeUserNamespace() error {
	cmd := exec.Command("/usr/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	if err := cmd.Run(); err != nil {
		return refuse(ReasonNamespaceUnavailable, "unprivileged user namespaces are unavailable: %v", usernsHint(err))
	}
	return nil
}

// usernsHint renders the actionable diagnostic for a refused user namespace:
// the Ubuntu AppArmor restriction names itself, and a bare EPERM inside a
// container usually means the container was started without user-namespace
// rights.
func usernsHint(err error) string {
	text := errnoText(err)
	if errors.Is(err, unix.EPERM) {
		return text + " (the kernel or AppArmor policy disallows unprivileged user namespaces; on Ubuntu 24.04+ check the AppArmor userns restriction, in a container the runtime must allow user namespaces)"
	}
	return text
}

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
	if !landlockAvailable() {
		return Applied{}, refuse(ReasonNamespaceUnavailable, "the Landlock stage is unavailable on this kernel; the boundary it enforces cannot be staged")
	}
	stage, err := buildLandlockStage(req.Resolved)
	if err != nil {
		return Applied{}, err
	}
	args, err := renderBubblewrap(req.Resolved, req.Rules, b.Canonical)
	if err != nil {
		return Applied{}, err
	}
	self, err := stageSelfExecutable()
	if err != nil {
		return Applied{}, err
	}
	rendered := "bubblewrap\n" + launcher + "\n" + stage.describe() + "\n" + strings.Join(args, "\n") + "\n"
	policy := stage.describe()
	return Applied{
		Rendered: []byte(rendered),
		Wrap: func(argv []string) []string {
			wrapped := append([]string{launcher}, append(args, "--")...)
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
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
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
	"/etc/ca-certificates",
	"/etc/gitconfig",
	"/etc/timezone",
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
// list. The bind order is the boundary: read-only binds first, then the
// writable set over them, then the read-only leaves, protected paths,
// workarea root, metadata, pin literals and composer denies as read-only
// binds of empty placeholders over those, so narrower rules win. Any rule
// the mount tree cannot express refuses the whole rendering; none is
// dropped. The harness process re-executed as the Landlock stage (see
// stage_linux.go) programs the Landlock policy from inside the mount tree
// and then execs the harness: Wrap runs `launcher args -- self-exe stage
// -- argv`, rebinding the current executable by its /proc/self/exe path.
// Returned args end before the first "--" separator; Wrap appends it.
func renderBubblewrap(r *Resolved, rules []Rule, canonical func(string) (string, error)) ([]string, error) {
	var args []string
	bind := func(flag, source, target string) {
		args = append(args, flag, source, target)
	}
	// A fresh user and mount namespace per command; the process tree stays
	// visible (no pid namespace: the harness is supervised, and
	// OnProcessSpawned reports the outer launcher pid, which stays the
	// parent the supervisor signals), and the boundary dies with the
	// command. Network stays shared: loopback TCP outside the declared
	// ports is closed by the Landlock stage, which allows connects only to
	// the declared ports; --unshare-net would isolate the loopback
	// interface entirely and strand even declared host listeners.
	args = append(args, "--unshare-user", "--die-with-parent", "--new-session")
	// Everything outside the binds below is hidden: the root is a tmpfs, so
	// a path that is bound nowhere reads as absent.
	args = append(args, "--tmpfs", "/")
	roBind := func(source string) {
		if _, err := os.Lstat(source); err != nil {
			return
		}
		bind("--ro-bind", source, source)
	}
	for _, path := range mountNamespaceRuntimeBinds {
		roBind(path)
	}
	for _, path := range mountNamespaceResolverBinds {
		roBind(path)
	}
	// Device nodes a process needs: /dev is a fresh tmpfs, so the host's
	// terminals never enter the boundary. --dev-bind (not --bind) carries
	// device nodes through: a plain bind of a device node does not work.
	// The Landlock stage grants open on this private /dev; nothing else
	// there exists to open.
	args = append(args, "--tmpfs", "/dev")
	for _, path := range []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/random"} {
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		bind("--dev-bind", path, path)
	}
	// The descriptor conduit shells need for process substitution.
	bind("--symlink", "/proc/self/fd", "/dev/fd")
	// A fresh proc mount: the host's process table stays outside, and
	// /proc/self/fd (behind /dev/fd) resolves inside.
	args = append(args, "--proc", "/proc")
	// The writable set: mutable leaves, harness state, session tmp and
	// session caches, bound read-write over the read-only tree.
	for _, root := range r.Writable {
		if _, err := os.Lstat(root.Path); err != nil {
			return nil, refuse(ReasonWritableSetUnrepresentable, "writable root is not reachable: %v", errnoText(err))
		}
		bind("--bind", root.Path, root.Path)
	}
	// Declared sockets outside the writable set: bound read-only by exact
	// path so name resolution and the control channel keep working — a
	// connect through a read-only bind succeeds, and the Landlock stage
	// grants the open. Every other socket outside the set has no bind and
	// stays unreachable.
	for _, socket := range r.Sockets {
		roBind(socket)
	}
	// The read-only leaves, protected paths, workarea metadata and pins are
	// overlaid read-only after the writable allows, so they win. An overlay
	// needs something to mount: an empty placeholder under the OS temporary
	// directory stands in for the denied tree. A denied path that does not
	// exist needs no overlay — the tmpfs root already hides it — but its
	// missing ancestors are created with --dir first, so the render never
	// names a destination bubblewrap cannot mount onto.
	deny := append(append([]string{}, r.ReadOnly...), r.Protected...)
	deny = append(deny, r.MetadataDir)
	deny = append(deny, r.Pins...)
	// The workarea root itself is never writable: the harness reaches its
	// leaves through it but cannot rename or create beside them.
	deny = append(deny, r.WorkareaRoot)
	for _, path := range deny {
		info, err := os.Lstat(path)
		if err != nil {
			// A denied path that does not exist needs no overlay: the
			// tmpfs root already hides it, and the Landlock stage
			// denies it too. Skipping keeps the render launchable —
			// binding a missing source onto its missing target would
			// fail the spawn.
			continue
		}
		empty, _, err := emptyPlaceholder(path)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			if parent := filepath.Dir(path); parent != "/" && parent != "." {
				args = append(args, "--dir", parent)
			}
			args = append(args, "--dir", path)
		}
		bind("--ro-bind", empty, path)
	}
	// The shared temporary locations are hidden the same way: a seat that
	// reaches for the host's shared tmp finds an empty directory, while its
	// own per-session tmp stays bound writable above. A writable root that
	// lives under a shared parent (a test layout) keeps its bind: hiding
	// its parent would bury it.
	shared, err := sharedLocations()
	if err != nil {
		return nil, refuse(ReasonBackendAbsent, "shared locations: %v", err)
	}
	for _, dir := range shared {
		buried := false
		for _, root := range r.Writable {
			if insideOrEqual(dir, root.Path) || insideOrEqual(root.Path, dir) {
				buried = true
				break
			}
		}
		if buried {
			continue
		}
		if _, err := os.Lstat(dir); err != nil {
			continue
		}
		args = append(args, "--dir", dir)
		empty, _, err := emptyPlaceholder(dir)
		if err != nil {
			return nil, err
		}
		bind("--ro-bind", empty, dir)
	}
	// Loopback TCP is closed by default and opened per declared port: the
	// mount tree carries the policy, and the Landlock stage enforces it —
	// handled_access_net covers connect, and each declared port is an
	// allow rule, so an undeclared dial fails closed with EACCES. The
	// declared ports ride the stage invocation after the second "--";
	// Wrap appends them there (see Apply).
	composer, err := renderMountComposerRules(r, rules, canonical)
	if err != nil {
		return nil, err
	}
	args = append(args, composer...)
	if r.ReadScope != "" {
		readArgs, err := renderMountReadScope(r)
		if err != nil {
			return nil, err
		}
		args = append(args, readArgs...)
	}
	// The boundary marks itself: the nested-sandbox check reads it back.
	args = append(args, "--setenv", mountNamespaceMarkEnv, "1")
	return args, nil
}

// renderMountComposerRules renders the deny-only composer rules as
// placeholder overlays. Any rule the mount tree cannot express refuses the
// whole rendering; none is dropped.
func renderMountComposerRules(r *Resolved, rules []Rule, canonical func(string) (string, error)) ([]string, error) {
	var args []string
	var writable []string
	for _, root := range r.Writable {
		writable = append(writable, root.Path)
	}
	for i, rule := range rules {
		switch rule.Kind {
		case RuleDenyRead, RuleDenyWrite:
			if rule.Service != "" || !filepath.IsAbs(rule.Path) {
				return nil, refuse(ReasonRuleUnrenderable, "composer rule %d: a path rule needs an absolute path and no service", i)
			}
			path, err := canonicalLoose(rule.Path, canonical)
			if err != nil {
				return nil, refuse(ReasonRuleUnrenderable, "composer rule %d: %v", i, err)
			}
			switch rule.Scope {
			case ScopeLiteral, ScopeSubtree:
			default:
				return nil, refuse(ReasonRuleUnrenderable, "composer rule %d: unknown scope %q", i, rule.Scope)
			}
			if _, err := os.Lstat(path); err != nil {
				// A deny over a path that does not exist needs no
				// overlay: the tmpfs root already hides it. Skipping
				// (rather than binding the missing path onto
				// itself) keeps the render launchable, and the
				// Landlock stage denies the path too.
				continue
			}
			empty, _, err := emptyPlaceholder(path)
			if err != nil {
				return nil, err
			}
			if parent := filepath.Dir(path); parent != "/" && parent != "." {
				args = append(args, "--dir", parent)
			}
			args = append(args, "--dir", path)
			args = append(args, "--ro-bind", empty, path)
			for _, pin := range ancestorPins(writable, []string{path}) {
				anchor, _, err := emptyPlaceholder(pin)
				if err != nil {
					return nil, err
				}
				args = append(args, "--dir", pin)
				args = append(args, "--ro-bind", anchor, pin)
			}
		case RuleDenyServiceLookup:
			if rule.Path != "" || rule.Scope != "" || !servicePattern.MatchString(rule.Service) {
				return nil, refuse(ReasonRuleUnrenderable, "composer rule %d: a service rule needs a plain service name and no path", i)
			}
			// No service namespace exists on Linux to close: the only
			// honest answer is to refuse the rendering, never to drop it.
			return nil, refuse(ReasonRuleUnrenderable, "composer rule %d: service lookup denies have no mount-tree rendering", i)
		default:
			return nil, refuse(ReasonRuleUnrenderable, "composer rule %d: unknown kind %q", i, rule.Kind)
		}
	}
	return args, nil
}

// renderMountReadScope renders the workarea read scope: the writable set
// stays read-write (the self-test probe writes its read-pass results into
// session tmp), while the read-only leaves and the declared read paths are
// bound read-only last, so a read of the allowlist succeeds and a read
// outside it falls to the Landlock stage, which denies it. Any other level
// is refused rather than rendered as open reads.
func renderMountReadScope(r *Resolved) ([]string, error) {
	switch r.ReadScope {
	case agent.FileReadWorkarea:
		var args []string
		seen := map[string]bool{}
		for _, root := range r.Writable {
			seen[root.Path] = true
		}
		for _, path := range r.ReadOnly {
			if seen[path] {
				continue
			}
			seen[path] = true
			if _, err := os.Lstat(path); err != nil {
				continue
			}
			args = append(args, "--ro-bind", path, path)
		}
		for _, path := range r.ReadPaths {
			if seen[path] {
				continue
			}
			seen[path] = true
			if _, err := os.Lstat(path); err != nil {
				continue
			}
			args = append(args, "--ro-bind", path, path)
		}
		return args, nil
	default:
		return nil, refuse(ReasonWritableSetUnrepresentable, "unknown read scope %q", r.ReadScope)
	}
}

// emptyPlaceholder returns a path whose bind hides the denied target: an
// empty directory for a directory target, an empty file otherwise, plus
// whether the target is a directory. The placeholder lives under the OS
// temporary directory, outside every session root. Callers skip targets
// that do not exist — the tmpfs root already hides those — so a missing
// target never reaches this function; it stays a caller-side decision,
// documented at each call site.
func emptyPlaceholder(target string) (string, bool, error) {
	info, err := os.Lstat(target)
	if err != nil {
		return "", false, refuse(ReasonWritableSetUnrepresentable, "deny target is not reachable: %v", errnoText(err))
	}
	base := filepath.Join(os.TempDir(), "donmai-confine-empty")
	if info.IsDir() {
		dir := filepath.Join(base, "dir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", false, refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
		}
		return dir, true, nil
	}
	file := filepath.Join(base, "file")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", false, refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return "", false, refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
	}
	_ = f.Close()
	return file, false, nil
}
