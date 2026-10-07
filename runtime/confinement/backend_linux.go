//go:build linux

package confinement

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/RenseiAI/donmai/agent"
)

// mountNamespaceProfileVersion is the Linux mount-namespace backend's
// implementation version. It is part of the backend version, so a self-test
// record taken under an older launcher or mount-tree shape is stale.
const mountNamespaceProfileVersion = "mount-namespace-v1"

// DefaultBackend returns the confinement backend for the running OS: the
// Linux mount-namespace backend here.
func DefaultBackend() Backend { return &mountNamespaceBackend{} }

// ResolverSockets returns the OS resolver sockets a harness that needs name
// resolution declares in Spec.Sockets. The NSS stack reads the static
// resolver configuration and the host database directly, so those paths are
// what a harness declares; the mount tree binds them read-only.
func ResolverSockets() []string { return []string{"/etc/resolv.conf"} }

// sharedLocations are the shared temporary locations the boundary hides: a
// confined seat reaches its own per-session tmp through the TMPDIR binding,
// never the host's shared directories.
func sharedLocations() ([]string, error) {
	return []string{"/tmp", "/var/tmp"}, nil
}

type mountNamespaceBackend struct {
	// launcherOverride pins the launcher executable, for tests. Empty
	// resolves bubblewrap on PATH, falling back to the direct helper.
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
func probeUserNamespace() error {
	if err := unix.Unshare(unix.CLONE_NEWUSER); err != nil {
		return refuse(ReasonNamespaceUnavailable, "unprivileged user namespaces are unavailable: %v", usernsHint(err))
	}
	// The unshare above moved this process into a fresh user namespace;
	// that change is per-process and dies with it, so nothing is undone.
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
	args, err := renderBubblewrap(req.Resolved, req.Rules, b.Canonical)
	if err != nil {
		return Applied{}, err
	}
	if err := checkLauncher(b.launcher()); err != nil {
		return Applied{}, err
	}
	rendered := "bubblewrap\n" + strings.Join(args, "\n") + "\n"
	return Applied{
		Rendered: []byte(rendered),
		Wrap: func(argv []string) []string {
			return append([]string{b.launcher()}, append(args, append([]string{"--"}, argv...)...)...)
		},
	}, nil
}

// launcher resolves the helper executable: the test override, else
// bubblewrap on PATH. Apply never renders a boundary it cannot launch:
// checkLauncher refuses with backend_absent when it is missing.
func (b *mountNamespaceBackend) launcher() string {
	if b.launcherOverride != "" {
		return b.launcherOverride
	}
	return "bwrap"
}

// checkLauncher refuses with backend_absent when the launcher is missing, so
// a seat is never run unwrapped.
func checkLauncher(name string) error {
	if filepath.IsAbs(name) {
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return refuse(ReasonBackendAbsent, "the mount-namespace launcher is missing")
		}
		return nil
	}
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return nil
		}
	}
	return refuse(ReasonBackendAbsent, "the mount-namespace launcher is missing")
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
// bundle and timezone data TLS and name resolution need, and the device
// nodes a process needs (bound through by name, never a terminal or a
// pseudo-terminal multiplexer). Entries that do not exist on the host are
// skipped at render; the session's own allowlist is bound after, so it wins
// over nothing here — these paths never overlap the writable set, which
// resolveSpec already guarantees.
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
	"/proc",
	"/sys",
	"/dev/null",
	"/dev/zero",
	"/dev/random",
	"/dev/urandom",
	"/dev/tty",
	"/dev/fd",
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
// dropped. Returned args end before the "--" separator; Wrap appends it.
func renderBubblewrap(r *Resolved, rules []Rule, canonical func(string) (string, error)) ([]string, error) {
	var args []string
	bind := func(flag, source, target string) {
		args = append(args, flag, source, target)
	}
	// A fresh user and mount namespace per command; the process tree stays
	// visible (no pid namespace: the harness is supervised, not isolated
	// from its supervisor), and the boundary dies with the command.
	args = append(args, "--unshare-user", "--unshare-pid", "--die-with-parent", "--new-session")
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
	// The writable set: mutable leaves, harness state, session tmp and
	// session caches, bound read-write over the read-only tree.
	for _, root := range r.Writable {
		if _, err := os.Lstat(root.Path); err != nil {
			return nil, refuse(ReasonWritableSetUnrepresentable, "writable root is not reachable: %v", errnoText(err))
		}
		bind("--bind", root.Path, root.Path)
	}
	// Device nodes a process must write stay writable; the read-only bind
	// above made them read-only, so they are re-bound read-write after it.
	for _, path := range []string{"/dev/null", "/dev/zero"} {
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		bind("--bind", path, path)
	}
	// Declared sockets outside the writable set: bound read-only by exact
	// path so name resolution and the control channel keep working. Every
	// other socket outside the set has no bind and stays unreachable.
	for _, socket := range r.Sockets {
		roBind(socket)
	}
	// The read-only leaves, protected paths, workarea metadata and pins are
	// overlaid read-only after the writable allows, so they win. An overlay
	// needs something to mount: an empty placeholder under the profile
	// directory stands in for the denied tree.
	deny := append(append([]string{}, r.ReadOnly...), r.Protected...)
	deny = append(deny, r.MetadataDir)
	deny = append(deny, r.Pins...)
	// The workarea root itself is never writable: the harness reaches its
	// leaves through it but cannot rename or create beside them.
	deny = append(deny, r.WorkareaRoot)
	for _, path := range deny {
		empty, err := emptyPlaceholder(path)
		if err != nil {
			return nil, err
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
		empty, err := emptyPlaceholder(dir)
		if err != nil {
			return nil, err
		}
		bind("--ro-bind", empty, dir)
	}
	// Loopback TCP outside the declared ports is closed by removing the
	// loopback interface from the boundary's network view: an isolated
	// loopback device with no addresses answers nothing, while each
	// declared port is re-opened by forwarding it from the host into the
	// boundary. Nothing is allowed by default.
	//
	// The forward needs a stable host-side listener per declared port,
	// which Apply cannot keep alive across Wrap calls; ports are instead
	// carried as TCP allow rules the helper programs with iptables once it
	// owns the namespace. Until that helper lands, a spec that declares
	// loopback ports is refused rather than rendered without its network
	// rule: the plan never degrades to a weaker set.
	if len(r.LoopbackTCPPorts) > 0 {
		return nil, refuse(ReasonRuleUnrenderable, "declared loopback TCP ports need the namespace network helper, which is not staged yet")
	}
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
			empty, err := emptyPlaceholder(path)
			if err != nil {
				return nil, err
			}
			args = append(args, "--ro-bind", empty, path)
			for _, pin := range ancestorPins(writable, []string{path}) {
				anchor, err := emptyPlaceholder(pin)
				if err != nil {
					return nil, err
				}
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

// renderMountReadScope renders the workarea read scope: with the root
// hidden by tmpfs, reads outside the allowlist already fail, so the scope
// binds the session's allowlist read-only last — the writable roots, the
// read-only leaves and the declared read paths — and refuses any other
// level rather than rendering it as open reads.
func renderMountReadScope(r *Resolved) ([]string, error) {
	switch r.ReadScope {
	case agent.FileReadWorkarea:
		var args []string
		for _, path := range r.ReadAllowlist() {
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
// empty directory for a directory target, an empty file otherwise. The
// placeholder lives under the OS temporary directory, outside every
// session root. Landlock defence-in-depth is staged by the helper where
// landlockAvailable reports kernel support; the mount tree is the boundary
// either way, and the backend version names the kernel release so the
// self-test pins what it proved.
func emptyPlaceholder(target string) (string, error) {
	info, err := os.Lstat(target)
	if err != nil {
		// A denied path that does not exist needs no overlay: there is
		// nothing there to reach. Bind the target onto itself read-only so
		// the render still names it and a later creation lands on the
		// read-only tmpfs root, not on the host.
		return target, nil
	}
	base := filepath.Join(os.TempDir(), "donmai-confine-empty")
	if info.IsDir() {
		dir := filepath.Join(base, "dir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
		}
		return dir, nil
	}
	file := filepath.Join(base, "file")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return "", refuse(ReasonNamespaceUnavailable, "cannot stage empty overlay: %v", errnoText(err))
	}
	_ = f.Close()
	return file, nil
}

// landlockAvailable reports whether the running kernel supports Landlock
// (5.13+): the create-ruleset call exists and answers. It is defence in
// depth only; the mount tree is the boundary either way, and the backend
// version names the kernel release so the self-test pins what it proved.
func landlockAvailable() bool {
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return false
	}
	_ = unix.Close(int(fd))
	return true
}
