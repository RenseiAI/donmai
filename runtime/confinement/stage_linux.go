//go:build linux

package confinement

import (
	"fmt"
	"os"
	"os/signal"
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
// paths it sees: writes land only in the writable set, and reads outside
// the allowlist (under a read scope) are denied. The stage runs as the
// harness process itself re-executed — Wrap renders the current executable
// into the mount tree and invokes it with a stage marker — so no second
// binary is shipped, versioned or resolved.
//
// TCP connections are deliberately NOT handled here. Landlock port rules
// carry no address field, so handling CONNECT_TCP with only declared-port
// allow rules would deny every TCP connect to an undeclared port on any
// address — including remote model endpoints and ordinary external fetches
// the contract leaves open (the macOS backend denies loopback only). The
// backend therefore declares loopback egress open
// (leavesLoopbackEgressOpen), and the self-test holds it to that: its
// undeclared-port loopback dials must go through. landlockRuleset is the
// one place the handled rights are chosen; TestLandlockRuleset_HandlesNoNetwork
// pins it at every ABI and TestLandlockStage_LeavesTCPOpen proves it live.
// Declared loopback ports still ride the stage invocation (see
// stagePortArgs) so the policy text records what the session declared, but
// the stage grants no network access and handles no network right.
//
// Layering, from outer to inner: the worker (outside) renders the mount
// tree; bubblewrap enters the user and mount namespaces and binds the tree;
// the stage applies the Landlock policy onto itself inside the namespaces
// and execs the harness. Landlock rules name the inside view — the same
// absolute paths the mount tree binds, at the same spelling — because the
// stage runs after the binds are in place. Every rule names exactly the
// path the tree binds: a file grants that file, never its directory, so a
// declared configuration file in the operator home opens that file and
// nothing beside it.
//
// Landlock needs no privilege beyond the user namespace: creating a ruleset
// and restricting oneself requires only no_new_privs, which the stage sets
// on itself. Where the kernel's Landlock ABI is older than landlockMinABI
// the backend refuses closed with backend_absent: a seat is never run with
// a weaker boundary than rendered.

// landlockExecAccess is the access the stage grants on each executable it
// re-executes, the file alone: read and execute, so the stage and the
// harness binaries load and run while their siblings stay ungranted.
// Executing needs the read as well as the execute right.
const landlockExecAccess = landlockAccessFSExecute | landlockAccessFSReadFile

// landlockPolicy is one session's Landlock policy: the write and read
// grants, plus the session's declared loopback TCP ports, carried for the
// record only. TCP connects are never granted or denied: Landlock port
// rules carry no address field, so handling CONNECT_TCP would cut external
// TCP the contract leaves open (see the file header).
type landlockPolicy struct {
	// writeRoots are the subtrees granted full access: the writable set.
	writeRoots []string
	// readRoots are the paths granted read access, each exactly as the
	// mount tree binds it: a directory grants its subtree, a file grants
	// itself.
	readRoots []string
	// tcpPorts are the session's declared loopback TCP ports, carried for
	// the record. The applied ruleset ignores them: the stage handles no
	// network right, so no connect is ever granted or denied by Landlock.
	tcpPorts []int
	// dev is whether the private /dev tree was rendered (tmpfs + dev-binds):
	// the stage grants the device access reads and writes need.
	dev bool
	// sockets are the exact declared socket paths the mount tree binds.
	// Connecting to a path socket is not a Landlock-mediated access; the
	// read grant covers a declared path that is a plain file.
	sockets []string
	// execRoots are the executables the stage re-executes (itself and the
	// harness), granted read and execute on the file alone, so binaries
	// outside every other grant still load and run without opening their
	// siblings.
	execRoots []string
}

// buildLandlockStage builds the stage policy for one resolved session: the
// writable set is granted full access; the OS userland and resolver binds,
// the read-only leaves, the declared sockets and — with reads open — the
// workarea metadata, the operator home and the host state home are granted
// read access; under a read scope the declared read paths are granted read
// access instead of the host directories. The declared loopback ports are
// recorded on the policy (see describe) but granted nothing: the stage
// handles no network right. Everything else the mount tree reveals stays
// read-only-or-hidden by the combination: the mount tree binds it
// read-only, and Landlock withholds write.
//
// Every grant names the path exactly as the tree binds it, never a parent:
// the stage runs on the inside view, where the tree binds each path at its
// own spelling (a host link is bound as its target, at the link's path).
func buildLandlockStage(r *Resolved) (*landlockPolicy, error) {
	stage := &landlockPolicy{tcpPorts: append([]int(nil), r.LoopbackTCPPorts...), dev: true}
	seen := map[string]bool{}
	add := func(list *[]string, path string) {
		if path == "" || seen[path] {
			return
		}
		if _, err := os.Lstat(path); err != nil {
			return
		}
		seen[path] = true
		*list = append(*list, path)
	}
	for _, root := range r.Writable {
		add(&stage.writeRoots, root.Path)
	}
	for _, path := range mountNamespaceRuntimeBinds {
		add(&stage.readRoots, path)
	}
	for _, path := range mountNamespaceResolverBinds {
		add(&stage.readRoots, path)
	}
	for _, path := range r.ReadOnly {
		add(&stage.readRoots, path)
	}
	if r.ReadScope != "" {
		for _, path := range r.ReadPaths {
			add(&stage.readRoots, path)
		}
	} else {
		// With reads open the host directories the tree binds read-only
		// stay readable, matching the seatbelt contract that reads are
		// open without a scope: tools the seat runs (version control,
		// package managers) read their user configuration there. Under a
		// read scope they stay visible for metadata only, with no grant,
		// so contents and listings refuse.
		for _, path := range []string{r.MetadataDir, r.Home, r.StateHome} {
			add(&stage.readRoots, path)
		}
	}
	for _, socket := range r.Sockets {
		add(&stage.sockets, socket)
	}
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
// invocation: `--tcp <p1,p2>`. Empty when no port is declared. The ports
// ride the invocation so the rendered boundary records what the session
// declared; the applied ruleset grants nothing for them (see
// restrictLandlockFull), so connects stay governed by the surrounding
// network, exactly as on an unhandled right.
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
	if len(argv) == 0 {
		return fmt.Errorf("confinement: landlock stage: no harness command")
	}
	if err := takeForeground(); err != nil {
		return err
	}
	if err := restrictLandlockFull(stage); err != nil {
		return err
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

// takeForeground makes the stage, and so the harness it execs, the
// foreground process group of its controlling terminal when it has one —
// the interactive spawn path, where the launcher keeps the PTY host's
// session (see sessionFlags). Ctrl-C and the other terminal signals then
// reach the harness and its jobs instead of the launcher, and a shell the
// harness runs can hand the terminal to its own jobs. A headless stage has
// no controlling terminal on standard input and is left as it is. It is
// part of the stage's setup and runs before the Landlock restriction.
func takeForeground() error {
	if _, err := unix.IoctlGetInt(0, unix.TIOCGPGRP); err != nil {
		return nil // no controlling terminal on standard input
	}
	if err := unix.Setpgid(0, 0); err != nil {
		return refuse(ReasonNamespaceUnavailable, "the stage cannot start its own process group: %v", errnoText(err))
	}
	// The new group is in the background until the ioctl below; ignore the
	// stop signal the terminal sends a background writer, and restore the
	// default before the harness is exec'd (an ignored signal would carry
	// over the exec).
	signal.Ignore(unix.SIGTTOU)
	defer signal.Reset(unix.SIGTTOU)
	if err := unix.IoctlSetPointerInt(0, unix.TIOCSPGRP, unix.Getpgrp()); err != nil {
		return refuse(ReasonNamespaceUnavailable, "the stage cannot take its terminal's foreground: %v", errnoText(err))
	}
	return nil
}

// parseLandlockStage splits a stage invocation: the environment carries the
// describe() policy text, and os.Args carries
// `<self> stage --tcp <ports> -- <harness...>`. It returns the policy
// (write roots, read roots with the declared sockets folded in, the device
// flag, the declared TCP ports in host order, and the executables to grant)
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
		remember(&stage.execRoots, execFileRule(args[0]))
	}
	if len(argv) > 0 {
		remember(&stage.execRoots, execFileRule(argv[0]))
	}
	return stage, argv, nil
}

// execFileRule returns the executable the stage grants read and execute
// on: the path with links resolved on the inside view (the stage runs
// there), or empty when the path is not absolute — a relative executable
// never reaches the stage, since the spawn binding resolves it first. The
// input is the stage or harness executable path the spawn chain rendered,
// never probe or session input.
func execFileRule(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	if target, err := filepath.EvalSymlinks(path); err == nil {
		return target
	}
	return path
}

// landlockMinABI is the oldest Landlock ABI the stage runs on. ABI 1
// forbids every rename and link across directories once a ruleset is in
// force (it cannot express the right to reparent), which breaks ordinary
// tools inside the writable set — version control moves objects between
// directories — so the stage needs ABI 2 (Linux 5.19), where the right
// exists and is granted on the writable set.
const landlockMinABI = 2

// landlockABI returns the running kernel's Landlock ABI version, or 0 where
// Landlock is absent or disabled. It is the capability gate beside the
// launcher probe: Check and Apply refuse closed below landlockMinABI.
func landlockABI() int {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(abi) //nolint:gosec // G115: the kernel returns a small ABI version, or the call failed above.
}

// Landlock access rights. The numeric values are the kernel UAPI; the
// names mirror the kernel headers so a reader can check them against
// linux/landlock.h. Refer arrived with ABI 2 and truncate with ABI 3.
// Network rights are deliberately absent: the stage handles no network
// access (see the file header).
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

	landlockRulePathBeneath = 0x1
)

// landlockHandledFS is the filesystem access set the stage handles on a
// kernel at ABI 3 or later: every filesystem right up to truncate, so
// nothing the mount tree reveals stays reachable by omission. An ABI 2
// kernel does not know truncate; landlockRuleset drops it there, and the
// mount tree still holds every path outside the writable set read-only.
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

// landlockFileAccess are the rights a rule on a file (rather than a
// directory) may carry: the kernel refuses directory rights on a file.
const landlockFileAccess = landlockAccessFSExecute |
	landlockAccessFSWriteFile |
	landlockAccessFSReadFile |
	landlockAccessFSTruncate

// landlockScopeABI is the first Landlock ABI with scopes: signal and
// abstract-socket scoping (the scope layer) need ABI 6, which ships in
// Linux 6.12. Hosts below it run the seat without that layer (see
// ScopesAvailable), and the self-test never attests them as fully
// confined.
const landlockScopeABI = 6

// landlockScopes are the scopes the stage sets where the kernel has them:
// no signal to a process outside the boundary's Landlock domain, and no
// connection to an abstract unix socket created outside it. The process
// namespace already hides every outside process; the signal scope holds
// the same line from inside Landlock. Abstract sockets live in the shared
// network namespace, so the scope is the only thing that closes them.
const landlockScopes = unix.LANDLOCK_SCOPE_SIGNAL | unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET

// landlockRuleset returns the ruleset attributes the stage creates on a
// kernel at the given Landlock ABI: the filesystem rights that ABI knows,
// the signal and abstract-socket scopes from ABI 6, and no network right,
// ever. Handling CONNECT_TCP (or BIND_TCP) here would deny every TCP
// connect to an undeclared port on any address, remote endpoints included,
// because a port rule carries no address. It is the one place the handled
// set is chosen; restrictLandlockFull creates exactly this. Below ABI 6 the
// scopes are absent: signals stay held by the process namespace, and
// abstract sockets outside stay reachable, which the self-test records as
// unheld for this kernel (see abstractSocketsScoped) rather than passing
// silently.
func landlockRuleset(abi int) unix.LandlockRulesetAttr {
	handled := uint64(landlockHandledFS)
	if abi < 3 {
		handled &^= landlockAccessFSTruncate
	}
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	if abi >= landlockScopeABI {
		attr.Scoped = landlockScopes
	}
	return attr
}

// ScopesAvailable reports whether this kernel carries the scope layer the
// full boundary needs: the signal and abstract-socket scopes need Landlock
// ABI 6 (Linux 6.12). Below it the seat still starts — the mount tree, the
// process namespace and the Landlock filesystem rules hold — but signals to
// same-user processes outside and abstract sockets outside stay reachable,
// so a record taken there is degraded, never a full attestation (see
// SelfTestRecord.Degraded). The string names the missing layer when it is
// absent. Production probes the running kernel; tests stub landlockProbe.
func ScopesAvailable() (bool, string) {
	abi := landlockProbe()
	if abi >= landlockScopeABI {
		return true, ""
	}
	return false, fmt.Sprintf("the scope layer is unenforced: Landlock ABI %d has no signal or abstract-socket scope (ABI %d, Linux 6.12, needed)", abi, landlockScopeABI)
}

// abstractSocketsScoped reports whether this kernel's Landlock can close
// abstract unix sockets outside the boundary, and why not when it cannot.
func abstractSocketsScoped() (bool, string) {
	abi := landlockABI()
	if abi >= landlockScopeABI {
		return true, ""
	}
	return false, fmt.Sprintf("Landlock ABI %d has no abstract-socket scope (ABI %d, Linux 6.12, needed): abstract unix sockets outside the boundary stay reachable", abi, landlockScopeABI)
}

// landlockPathRule is the kernel's struct landlock_path_beneath_attr. The
// add-rule call takes the attribute with size 0 (kernel default); the rule
// type rides the call's second argument, not the struct.
type landlockPathRule struct {
	AllowedAccess uint64
	ParentFD      int32
	_             int32
}

// restrictLandlockFull programs one ruleset onto this process: full
// access on the writable roots, read access on the read roots and the bound
// sockets, read and execute on the re-executed executables, device access
// on the private /dev, no_new_privs, then restrict_self. The handled rights
// come from landlockRuleset alone, which handles no network right: the
// declared ports ride the policy text for the record only. Every error
// refuses closed.
func restrictLandlockFull(stage *landlockPolicy) error {
	abi := landlockABI()
	if abi < landlockMinABI {
		return refuse(ReasonBackendAbsent, "the Landlock stage is unavailable: Landlock ABI %d, and ABI %d is needed", abi, landlockMinABI)
	}
	attr := landlockRuleset(abi)
	ruleset, err := landlockCreateRuleset(attr)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(ruleset) }()
	grant := func(paths []string, access uint64) error {
		for _, path := range paths {
			if err := landlockAddPath(ruleset, path, access&attr.Access_fs); err != nil {
				return err
			}
		}
		return nil
	}
	if err := grant(stage.writeRoots, landlockHandledFS); err != nil {
		return err
	}
	if err := grant(stage.readRoots, landlockReadAccess); err != nil {
		return err
	}
	if err := grant(stage.sockets, landlockReadAccess); err != nil {
		return err
	}
	if err := grant(stage.execRoots, landlockExecAccess); err != nil {
		return err
	}
	if stage.dev {
		if err := grant([]string{"/dev"}, landlockDevAccess); err != nil {
			return err
		}
	}
	if err := landlockNoNewPrivs(); err != nil {
		return err
	}
	return landlockRestrictSelf(ruleset)
}

// landlockCreateRuleset creates a Landlock ruleset with the given handled
// rights. A kernel without Landlock answers with ENOSYS/EOPNOTSUPP, which
// refuses closed.
func landlockCreateRuleset(attr unix.LandlockRulesetAttr) (int, error) {
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0) //nolint:gosec // G103: the raw-syscall ABI takes uintptr; no typed wrapper exists for Landlock.
	if errno != 0 {
		return -1, refuse(ReasonBackendAbsent, "the Landlock stage is unavailable: %v", errnoText(errno))
	}
	return int(ruleset), nil //nolint:gosec // G115: the kernel returns a small non-negative descriptor; a failure refused above.
}

// landlockAddPath grants access beneath path: on a directory, its subtree;
// on any other file, that file alone, with the directory-only rights
// stripped (the kernel refuses them on a file). The add-rule call takes the
// attribute with size 0 (kernel default); the rule type rides the call's
// second argument.
func landlockAddPath(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0) //nolint:gosec // G703: path is the backend-rendered allowlist from the resolved session spec, never probe or session input; a failure refuses closed below.
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
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return refuse(ReasonWritableSetUnrepresentable, "the Landlock stage cannot inspect %s: %v", filepath.Base(path), errnoText(err))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= landlockFileAccess
	}
	if access == 0 {
		return nil
	}
	rule := landlockPathRule{AllowedAccess: access, ParentFD: int32(fd)} //nolint:gosec // G115: kernel-issued descriptors are small and non-negative.
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(ruleset), uintptr(landlockRulePathBeneath), uintptr(unsafe.Pointer(&rule))) //nolint:gosec // G103: the raw-syscall ABI takes uintptr; no typed wrapper exists for Landlock.
	if errno != 0 {
		return refuse(ReasonBackendAbsent, "the Landlock stage cannot add a path rule: %v", errnoText(errno))
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
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0) //nolint:gosec // G115: ruleset is the kernel-issued descriptor landlockCreateRuleset returned.
	if errno != 0 {
		return refuse(ReasonBackendAbsent, "the Landlock stage cannot restrict itself: %v", errnoText(errno))
	}
	return nil
}
