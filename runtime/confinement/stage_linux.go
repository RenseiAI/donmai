//go:build linux

package confinement

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This file is the Landlock stage of the Linux mount-namespace backend: the
// half of the boundary the mount tree cannot express. Bubblewrap hides and
// reveals paths; Landlock governs what the confined process may do with the
// paths it sees: reads outside the allowlist (under a read scope) and TCP
// connects outside the declared loopback ports are denied. The stage runs as
// the harness process itself re-executed — Wrap renders the current
// executable into the mount tree and invokes it with a stage marker — so no
// second binary is shipped, versioned or resolved.
//
// Layering, from outer to inner: the worker (outside) renders the mount
// tree; bubblewrap enters the user and mount namespaces and binds the tree;
// the stage applies the Landlock policy onto itself inside the namespaces
// and execs the harness. Landlock rules name the inside view — the same
// absolute paths the mount tree binds — because the stage runs after the
// binds are in place.
//
// Landlock needs no privilege beyond the user namespace: creating a ruleset
// and restricting oneself requires only no_new_privs, which the stage sets
// on itself. Where the kernel predates Landlock (5.13+) the stage refuses
// closed with backend_absent when the policy needs it, and Apply refuses
// closed with namespace_unavailable when the probe child cannot even create
// a ruleset: a seat is never run with a weaker boundary than rendered.

// landlockStageEnv marks a harness-process execution as the Landlock stage:
// the process programs the policy described in the invocation and execs the
// harness. The harness proper never sets it.
const landlockStageEnv = "DONMAI_CONFINEMENT_LANDLOCK_STAGE"

// landlockExecAccess is the access the stage grants on the directories
// holding the executables it re-executes: read and execute, so the stage
// and the harness binaries load and run, without directory listings —
// siblings stay unnamed. Executing needs the read as well as the execute
// right; listings stay denied.
const landlockExecAccess = landlockAccessFSExecute | landlockAccessFSReadFile

// landlockStage is one session's Landlock policy: the read allowlist (under
// a read scope) plus the declared loopback TCP ports. A nil allowlist with
// no ports is still a policy: nothing outside the mount tree's read-write
// set may be written, and no loopback TCP may connect.
type landlockPolicy struct {
	// readRoots are the filesystem subtrees granted read access, plus the
	// writable roots granted full access. Sorted, deduplicated.
	readRoots []string
	// writeRoots are the subtrees granted full access: the writable set.
	writeRoots []string
	// tcpPorts are the loopback TCP ports granted connect, sorted.
	tcpPorts []int
	// dev is whether the private /dev tree was rendered (tmpfs + dev-binds):
	// the stage grants the device access reads and writes need.
	dev bool
	// sockets are the exact socket paths the mount tree binds: the stage
	// grants them read access so connects through them succeed.
	sockets []string
	// execRoots are the directories holding the executables the stage
	// re-executes (itself and the harness): granted read and execute, so
	// binaries outside every other grant still load and run, without
	// opening their siblings to listings.
	execRoots []string
}

// landlockStage builds the stage policy for one resolved session: the
// writable set is granted full access, the read allowlist (read-only leaves
// and declared read paths under a read scope) is granted read access, the
// runtime binds the mount tree carries are granted read access, and the
// declared loopback ports are granted TCP connect. Everything else the
// mount tree reveals stays read-only-or-hidden by the combination: the
// mount tree binds it read-only, and Landlock withholds write.
//
// Landlock path rules name directories: the kernel's path-beneath rule
// takes a directory descriptor, so a file in the allowlist grants its
// parent directory. That is no wider than the mount tree: anything the
// parent reveals that the tree did not bind stays hidden behind the tmpfs
// root, and the stage runs after the binds are in place, on the inside
// view.
func buildLandlockStage(r *Resolved) (*landlockPolicy, error) {
	stage := &landlockPolicy{tcpPorts: append([]int(nil), r.LoopbackTCPPorts...)}
	seen := map[string]bool{}
	add := func(list *[]string, path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		*list = append(*list, path)
	}
	// dirRule returns the directory the stage grants for path: the path
	// itself when it names a directory, else its parent. Symbolic links
	// resolve first, so a link to a directory grants the target (already
	// covered when the target is bound too) instead of the link's parent.
	dirRule := func(path string) string {
		resolved := path
		if target, err := filepath.EvalSymlinks(path); err == nil {
			resolved = target
		}
		if info, err := os.Lstat(resolved); err == nil && !info.IsDir() {
			return filepath.Dir(resolved)
		}
		return resolved
	}
	for _, root := range r.Writable {
		add(&stage.writeRoots, root.Path)
	}
	for _, path := range mountNamespaceRuntimeBinds {
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		add(&stage.readRoots, dirRule(path))
	}
	for _, path := range mountNamespaceResolverBinds {
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		add(&stage.readRoots, dirRule(path))
	}
	if r.ReadScope != "" {
		for _, path := range r.ReadOnly {
			if _, err := os.Lstat(path); err != nil {
				continue
			}
			if seen[dirRule(path)] {
				continue
			}
			add(&stage.readRoots, dirRule(path))
		}
		for _, path := range r.ReadPaths {
			if _, err := os.Lstat(path); err != nil {
				continue
			}
			if seen[dirRule(path)] {
				continue
			}
			add(&stage.readRoots, dirRule(path))
		}
	}
	for _, socket := range r.Sockets {
		add(&stage.sockets, dirRule(socket))
	}
	// With reads open (no read scope) the host directories the tree
	// binds read-only stay readable, matching the seatbelt contract that
	// reads are open without a scope: tools the seat runs (version
	// control, package managers) read their user configuration there.
	// Under a read scope they stay visible for metadata only, with no
	// grant, so contents and listings refuse.
	if r.ReadScope == "" {
		for _, path := range []string{r.Home, r.StateHome} {
			if _, err := os.Lstat(path); err != nil {
				continue
			}
			if seen[dirRule(path)] {
				continue
			}
			add(&stage.readRoots, dirRule(path))
		}
	}
	stage.dev = true
	return stage, nil
}

// describe renders the stage policy as stable text for the Rendered digest:
// the record pins what was proved, and a policy change stales the record
// through the backend version.
func (s *landlockPolicy) describe() string {
	var b strings.Builder
	b.WriteString("landlock-stage\n")
	for _, path := range s.writeRoots {
		fmt.Fprintf(&b, "allow-write %s\n", path)
	}
	for _, path := range s.readRoots {
		fmt.Fprintf(&b, "allow-read %s\n", path)
	}
	for _, path := range s.sockets {
		fmt.Fprintf(&b, "allow-socket %s\n", path)
	}
	for _, path := range s.execRoots {
		fmt.Fprintf(&b, "allow-exec %s\n", path)
	}
	if s.dev {
		b.WriteString("allow-dev\n")
	}
	for _, port := range s.tcpPorts {
		fmt.Fprintf(&b, "allow-tcp %d\n", port)
	}
	return b.String()
}

// stageSelfExecutable returns the canonical path of the current process
// executable: the Landlock stage re-executes it inside the mount tree, so
// the mount tree binds exactly this path read-only.
func stageSelfExecutable() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", refuse(ReasonBackendAbsent, "the Landlock stage cannot find its own executable: %v", errnoText(err))
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", refuse(ReasonBackendAbsent, "the Landlock stage cannot resolve its own executable: %v", errnoText(err))
	}
	return resolved, nil
}

// stagePortArgs renders the declared-port flags Wrap splices into the stage
// invocation: `--tcp <p1,p2>`. Empty when no port is declared; the stage
// then denies every loopback connect.
func stagePortArgs(s *landlockPolicy) []string {
	if len(s.tcpPorts) == 0 {
		return nil
	}
	ports := make([]string, 0, len(s.tcpPorts))
	for _, port := range s.tcpPorts {
		ports = append(ports, strconv.Itoa(port))
	}
	return []string{"--tcp", strings.Join(ports, ",")}
}

// RunLandlockStageFromEnv runs the Landlock stage when the stage marker is
// set and reports whether it did, with the exit code the process should
// end with. The caller exits; a library never does. The stage reads the
// policy from the environment, restricts itself onto the inside view of
// the mount tree, and execs the harness argv it was given: it never
// returns on success, so falling off the end is a refusal. On other
// operating systems it never handles (see stage_other.go).
func RunLandlockStageFromEnv() (handled bool, exitCode int) {
	if os.Getenv(landlockStageEnv) == "" {
		return false, 0
	}
	if err := landlockApply(); err != nil {
		fmt.Fprintf(os.Stderr, "confinement landlock stage: %v\n", err)
		return true, 2
	}
	// landlockApply execs on success; reaching here means the exec failed.
	fmt.Fprintf(os.Stderr, "confinement landlock stage: exec did not transfer control\n")
	return true, 2
}

// landlockApply programs the stage policy onto this process and execs the
// harness. It runs inside the mount tree, so every path it names is the
// inside view. The policy arrives in landlockStageEnv as the describe()
// text plus the allowed TCP ports; the harness argv follows the stage's
// own "--" separator in os.Args.
func landlockApply() error {
	stage, argv, err := parseLandlockStage(os.Getenv(landlockStageEnv), os.Args)
	if err != nil {
		return err
	}
	if err := restrictLandlockFull(stage); err != nil {
		return err
	}
	if len(argv) == 0 {
		return fmt.Errorf("confinement: landlock stage: no harness command")
	}
	// The harness inherits the stage's environment minus the stage marker:
	// the marker names this invocation only, and a harness that is the
	// same executable (the self-test probe, the donmai binary) would
	// otherwise re-enter the stage on its harness argv and refuse it.
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, landlockStageEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	return unix.Exec(argv[0], argv, env)
}

// parseLandlockStage splits a stage invocation: the environment carries the
// describe() policy text, and os.Args carries
// `<self> stage --tcp <ports> -- <harness...>`. It returns the allow roots
// (read and write folded: the mount tree already separates them, Landlock
// needs the union for path grants), the allowed TCP ports in host order,
// and the harness argv.
func parseLandlockStage(env string, args []string) (*landlockPolicy, []string, error) {
	stage := &landlockPolicy{}
	seen := map[string]bool{}
	remember := func(list *[]string, value string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		*list = append(*list, value)
	}
	for _, line := range strings.Split(env, "\n") {
		// The header and the device flag carry no value; everything
		// else is a "name value" pair.
		if line == "allow-dev" {
			stage.dev = true
			continue
		}
		field, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch field {
		case "allow-write":
			remember(&stage.writeRoots, value)
		case "allow-read", "allow-socket":
			remember(&stage.readRoots, value)
		case "allow-exec":
			remember(&stage.execRoots, value)
		case "allow-tcp":
			port, convErr := strconv.Atoi(strings.TrimSpace(value))
			if convErr != nil || port < 1 || port > 65535 {
				return nil, nil, fmt.Errorf("confinement: landlock stage: bad port %q", value)
			}
			if !slices.Contains(stage.tcpPorts, port) {
				stage.tcpPorts = append(stage.tcpPorts, port)
			}
		}
	}
	i := 0
	for i < len(args) && args[i] != "stage" {
		i++
	}
	if i >= len(args) {
		return nil, nil, fmt.Errorf("confinement: landlock stage: no stage marker in argv")
	}
	i++
	for i < len(args) && args[i] != "--" {
		switch args[i] {
		case "--tcp":
			i++
			if i >= len(args) {
				return nil, nil, fmt.Errorf("confinement: landlock stage: --tcp without ports")
			}
			for _, raw := range strings.Split(args[i], ",") {
				port, convErr := strconv.Atoi(strings.TrimSpace(raw))
				if convErr != nil || port < 1 || port > 65535 {
					return nil, nil, fmt.Errorf("confinement: landlock stage: bad port %q", raw)
				}
				// The declared ports ride both the policy text and the
				// invocation; the ruleset merges identical port rules,
				// but keep one copy so the applied set stays explicit.
				if !slices.Contains(stage.tcpPorts, port) {
					stage.tcpPorts = append(stage.tcpPorts, port)
				}
			}
		default:
			return nil, nil, fmt.Errorf("confinement: landlock stage: unknown flag %q", args[i])
		}
		i++
	}
	if i >= len(args) {
		return nil, nil, fmt.Errorf("confinement: landlock stage: no harness separator")
	}
	argv := append([]string(nil), args[i+1:]...)
	// The executables the stage re-executes need traverse and execute
	// grants on their directories: the stage itself (args[0]) and the
	// harness (argv[0]) may live outside every other grant — the test
	// binary, a provider harness — and without the grant the exec fails
	// closed. Directories already granted (a /usr binary) dedupe out
	// through the shared seen set.
	if len(args) > 0 {
		remember(&stage.execRoots, execDirRule(args[0]))
	}
	if len(argv) > 0 {
		remember(&stage.execRoots, execDirRule(argv[0]))
	}
	return stage, argv, nil
}

// execDirRule returns the directory the stage grants traverse and execute
// on for one executable path: its containing directory (resolving links
// first), or empty when the path is not absolute — a relative executable
// never reaches the stage, since the spawn binding resolves it first.
func execDirRule(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	resolved := path
	if target, err := filepath.EvalSymlinks(path); err == nil {
		resolved = target
	}
	if info, err := os.Lstat(resolved); err == nil && !info.IsDir() {
		return filepath.Dir(resolved)
	}
	return resolved
}

// landlockAvailable reports whether the running kernel supports Landlock
// (5.13+): the create-ruleset call exists and answers. It is the
// capability gate beside the user-namespace probe: Apply refuses closed
// where the policy needs Landlock and the kernel has none.
func landlockAvailable() bool {
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return false
	}
	_ = unix.Close(int(fd))
	return true
}

// Landlock access rights. The numeric values are the kernel UAPI (ABI-1
// filesystem rights plus ABI-4 TCP rights); the names mirror the kernel
// headers so a reader can check them against linux/landlock.h.
const (
	landlockAccessFSExecute   = 0x1
	landlockAccessFSWriteFile = 0x2
	landlockAccessFSReadFile  = 0x4
	landlockAccessFSReadDir   = 0x8
	landlockAccessFSRemoveDir = 0x10
	landlockAccessFSRemoveFil = 0x20
	landlockAccessFSMakeChar  = 0x40
	landlockAccessFSMakeDir   = 0x80
	landlockAccessFSMakeReg   = 0x100
	landlockAccessFSMakeSock  = 0x200
	landlockAccessFSMakeFifo  = 0x400
	landlockAccessFSMakeBlock = 0x800
	landlockAccessFSMakeSym   = 0x1000
	landlockAccessFSRefer     = 0x2000
	landlockAccessFSTruncate  = 0x4000

	landlockAccessNetBindTCP    = 0x1
	landlockAccessNetConnectTCP = 0x2

	landlockRulePathBeneath = 0x1
	landlockRuleNetPort     = 0x2
)

// landlockHandledFS is the filesystem access set the stage handles: every
// right the kernel knows at ABI-1, so nothing the mount tree reveals stays
// reachable by omission.
const landlockHandledFS = landlockAccessFSExecute |
	landlockAccessFSWriteFile |
	landlockAccessFSReadFile |
	landlockAccessFSReadDir |
	landlockAccessFSRemoveDir |
	landlockAccessFSRemoveFil |
	landlockAccessFSMakeChar |
	landlockAccessFSMakeDir |
	landlockAccessFSMakeReg |
	landlockAccessFSMakeSock |
	landlockAccessFSMakeFifo |
	landlockAccessFSMakeBlock |
	landlockAccessFSMakeSym |
	landlockAccessFSRefer |
	landlockAccessFSTruncate

// landlockReadAccess is the access a read-only allow grants: execute (to
// run a bound interpreter or tool), read of files and listings of
// directories. Writes, removals, makes, renames and truncates stay denied.
const landlockReadAccess = landlockAccessFSExecute |
	landlockAccessFSReadFile |
	landlockAccessFSReadDir

// landlockDevAccess is the access the private /dev tree grants: reads and
// writes on the bound device nodes, without creation or removal — a seat
// must not smuggle new devices into /dev.
const landlockDevAccess = landlockAccessFSReadFile |
	landlockAccessFSReadDir |
	landlockAccessFSWriteFile

// landlockPathRule is the kernel's struct landlock_path_beneath_attr. The
// add-rule call takes the attribute with size 0 (kernel default); the rule
// type rides the call's second argument, not the struct.
type landlockPathRule struct {
	AllowedAccess uint64
	ParentFD      int32
	_             int32
}

// landlockNetRule is the kernel's struct landlock_net_port_attr: the
// allowed access plus the port in host byte order (proven by experiment:
// network order denies even the allowed port).
type landlockNetRule struct {
	AllowedAccess uint64
	Port          uint16
	_             [6]byte
}

// restrictLandlock programs one ruleset onto this process: full access on
// the writable roots, read access on the read roots and the bound sockets,
// device access on the private /dev, TCP connect only on the declared
// ports, no_new_privs, then restrict_self. Every error refuses closed.
func restrictLandlockFull(stage *landlockPolicy) error {
	ruleset, err := landlockCreateRuleset(landlockHandledFS, landlockAccessNetBindTCP|landlockAccessNetConnectTCP, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(ruleset) }()
	for _, root := range stage.writeRoots {
		if err := landlockAddPath(ruleset, root, landlockHandledFS); err != nil {
			return err
		}
	}
	for _, root := range stage.readRoots {
		if err := landlockAddPath(ruleset, root, landlockReadAccess); err != nil {
			return err
		}
	}
	for _, root := range stage.execRoots {
		if err := landlockAddPath(ruleset, root, landlockExecAccess); err != nil {
			return err
		}
	}
	if stage.dev {
		if err := landlockAddPath(ruleset, "/dev", landlockDevAccess); err != nil {
			return err
		}
	}
	for _, port := range stage.tcpPorts {
		if err := landlockAddPort(ruleset, port, landlockAccessNetConnectTCP); err != nil {
			return err
		}
	}
	if err := landlockNoNewPrivs(); err != nil {
		return err
	}
	return landlockRestrictSelf(ruleset)
}

// landlockCreateRuleset creates a Landlock ruleset handling the filesystem
// and TCP rights the stage governs. A kernel without Landlock answers with
// ENOSYS/EINVAL, which the caller turns into a closed refusal.
func landlockCreateRuleset(handledFS, handledNet, scoped uint64) (int, error) {
	attr := unix.LandlockRulesetAttr{Access_fs: handledFS, Access_net: handledNet, Scoped: scoped}
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0) //nolint:gosec // G103: the raw-syscall ABI takes uintptr; no typed wrapper exists for Landlock.
	if errno != 0 {
		return -1, refuse(ReasonBackendAbsent, "the Landlock stage is unavailable: %v", errnoText(errno))
	}
	return int(ruleset), nil
}

// landlockAddPath grants access on the subtree rooted at path. The add-rule
// call takes the attribute with size 0 (kernel default); the rule type
// rides the call's second argument.
func landlockAddPath(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY, 0) //nolint:gosec // G703: path is the backend-rendered allowlist from the resolved session spec, never probe or session input; a failure refuses closed below.
	if err != nil {
		// A rule over a path the mount tree hides is already denied twice
		// over: skip it rather than refuse a boundary that holds without
		// it. A rule over a path the mount tree REVEALS must hold, and the
		// open of a revealed path does not fail.
		if _, statErr := os.Lstat(path); statErr != nil { //nolint:gosec // G703: same backend-rendered allowlist as the open above.
			return nil
		}
		return refuse(ReasonWritableSetUnrepresentable, "the Landlock stage cannot open %s: %v", filepath.Base(path), errnoText(err))
	}
	defer func() { _ = unix.Close(fd) }()
	rule := landlockPathRule{AllowedAccess: access, ParentFD: int32(fd)} //nolint:gosec // G115: kernel-issued descriptors are small and non-negative.
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(ruleset), uintptr(landlockRulePathBeneath), uintptr(unsafe.Pointer(&rule))) //nolint:gosec // G103: the raw-syscall ABI takes uintptr; no typed wrapper exists for Landlock.
	if errno != 0 {
		return refuse(ReasonBackendAbsent, "the Landlock stage cannot add a path rule: %v", errnoText(errno))
	}
	return nil
}

// landlockAddPort grants TCP access on one port in host byte order. The
// port is validated 1-65535 by resolveSpec and again by parseLandlockStage
// before it reaches here.
func landlockAddPort(ruleset, port int, access uint64) error {
	rule := landlockNetRule{AllowedAccess: access, Port: uint16(port)} //nolint:gosec // G115: validated 1-65535 by the callers above.
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(ruleset), uintptr(landlockRuleNetPort), uintptr(unsafe.Pointer(&rule))) //nolint:gosec // G103: the raw-syscall ABI takes uintptr; no typed wrapper exists for Landlock.
	if errno != 0 {
		return refuse(ReasonBackendAbsent, "the Landlock stage cannot add a port rule: %v", errnoText(errno))
	}
	return nil
}

// landlockNoNewPrivs sets no_new_privs on this process, which restricting
// oneself requires.
func landlockNoNewPrivs() error {
	_, _, errno := unix.Syscall(unix.SYS_PRCTL, uintptr(unix.PR_SET_NO_NEW_PRIVS), 1, 0)
	if errno != 0 {
		return refuse(ReasonBackendAbsent, "the Landlock stage cannot set no_new_privs: %v", errnoText(errno))
	}
	return nil
}

// landlockRestrictSelf restricts this process onto the ruleset. On success
// the policy holds for this process and every descendant, including the
// harness exec that follows.
func landlockRestrictSelf(ruleset int) error {
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0)
	if errno != 0 {
		return refuse(ReasonBackendAbsent, "the Landlock stage cannot restrict itself: %v", errnoText(errno))
	}
	return nil
}
