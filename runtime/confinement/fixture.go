package confinement

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// The probe classes.
const (
	classPositive  = "positive"
	classOutside   = "outside"
	classReadOnly  = "read_only_leaf"
	classProtected = "protected"
	classWidening  = "widening"
	classReadScope = "read_scope"
	// classEgress marks the outbound-TCP probes a backend that declares
	// loopback egress open must let through (see loopbackEgressDeclarer).
	classEgress = "egress"
	// classDaemonPrivate probes are held by the daemon-private denies, which
	// render in every read scope: they stay refused with reads open.
	classDaemonPrivate = "daemon_private"
)

// probeCacheEnv is the cache variable the self-test binds, standing in for a
// toolchain cache.
const probeCacheEnv = "DONMAI_CONFINEMENT_PROBE_CACHE"

// lookupControlService is a service every process may reach. Its lookup is
// the positive control for the lookup probes: without it a lookup that
// failed for any reason would read as refused.
const lookupControlService = "com.apple.system.notification_center"

const decoyContent = "decoy\n"

var decoyTime = time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)

type harnessStep struct {
	probe          probeStep
	class          string
	expectAccepted bool
	// effect reports whether the probed change happened, judged from the
	// filesystem (or, where nothing can be observed, the tool's answer).
	effect func(stepResult) bool
	// setupErr marks a probe the fixture could not prepare; it fails.
	setupErr string
	// heldBy names what other than the boundary refuses this probe on this
	// host (a rename across filesystems), so the record never counts it as
	// held by the backend. Empty: only the boundary can refuse it.
	heldBy string
}

// fixture is one session mode's probe world: a workarea with a mutable and a
// read-only leaf, harness state with a protected artifact inside it, session
// tmp and cache, decoys outside the set, daemon-private secrets standing in
// for the host state (a minted control token beside a sibling secret, in a
// daemon-private directory that holds the whole world the way a host state
// home holds the per-session work areas), and the write proxies in reach.
// The same world serves the read-scope pass (readSteps), which runs under the
// read scope with a declared read path (rp) outside the set.
type fixture struct {
	dir, ws, meta, mut, git, state, ext, ro, rom, tmp, cache, out, rp string
	// daemonSecrets holds the daemon-private decoys: a minted token file
	// (Spec.DeniedPaths) and a sibling secret beside it, directly in the
	// fixture directory, which is denied its listing (Spec.DeniedListings)
	// while the work area beneath it keeps working, plus a second token
	// nested inside the writable set (harness state) the way the resolver
	// keeps a denied path inside the set denied instead of refusing the
	// spawn.
	// The nested one is the executing pin for the daemon-private write
	// deny: the blanket write deny cannot cover anything inside the
	// writable set, so only that deny holds it.
	daemonFile, daemonSibling, daemonNested, daemonNestedFile string
	tag                                                       string
	steps                                                     []harnessStep
	readSteps                                                 []harnessStep
	cleanups                                                  []func()
	listener                                                  net.Listener
	accepts                                                   atomic.Int32
	tcpOpen, tcpClosed, tcpOpen6, tcpClosed6                  net.Listener
	tcpOpenPort, tcpClosedPort                                int
	tcpAccepts                                                atomic.Int32
	// mounted records whether the mount probe got a volume over the
	// read-only leaf; settle takes it before detaching.
	mounted bool
	// moves are the renames that would move a whole leaf or directory the
	// other probes are judged in; settle records and undoes them.
	moves []move
	moved map[string]bool
	// rpf is a declared read path that is a single file in the operator
	// home, the shape of a tool's user configuration (~/.gitconfig): its
	// grant must not open the directory around it.
	rpf string
	// loopbackOpen is whether the backend under test declares outbound
	// TCP to the local machine open on undeclared ports.
	loopbackOpen bool
	// probeCommand starts the probe executable; the Linux widening probes
	// run it outside the boundary as their decoy.
	probeCommand []string
	// notProbed are the probes of the set this host cannot run, and why.
	notProbed []NotProbed
}

// move is one directory rename probe: from is where the directory belongs.
type move struct{ id, from, to string }

// fixtureHost is what a fixture needs from the host and the backend under
// test: the operator home and host state home the decoys sit in, the macOS
// disk image the mount probe attaches, and the backend's declared loopback
// egress.
type fixtureHost struct {
	home, stateHome string
	diskImage       string
	diskImageErr    error
	loopbackOpen    bool
	probeCommand    []string
}

func newFixture(dir string, mode agent.PromptSessionMode, host fixtureHost) (fx *fixture, err error) {
	home, stateHome := host.home, host.stateHome
	built := &fixture{dir: dir, tag: randomSuffix(), loopbackOpen: host.loopbackOpen, probeCommand: host.probeCommand}
	fx = built
	defer func() {
		// A failed build returns no fixture; what it already started (a
		// listener, a decoy process) is still released.
		if err != nil {
			built.cleanup()
		}
	}()
	fx.ws = filepath.Join(dir, "ws")
	fx.meta = filepath.Join(fx.ws, workarea.DeclarationDirName)
	fx.mut = filepath.Join(fx.ws, "mut")
	fx.git = filepath.Join(fx.mut, ".git")
	fx.state = filepath.Join(fx.mut, ".h")
	fx.ext = filepath.Join(fx.state, "ext")
	fx.ro = filepath.Join(fx.ws, "ro")
	fx.rom = filepath.Join(fx.ws, "rom")
	fx.tmp = filepath.Join(dir, "t")
	fx.cache = filepath.Join(dir, "c")
	fx.out = filepath.Join(dir, "o")
	fx.rp = filepath.Join(dir, "rp")
	for _, path := range []string{fx.meta, fx.git, fx.ext, fx.ro, fx.rom, fx.tmp, fx.cache, fx.out, fx.rp} {
		if err := os.MkdirAll(path, 0o755); err != nil { //nolint:gosec // G301: fixture directories.
			return nil, fmt.Errorf("confinement: fixture: %w", err)
		}
	}
	if err := os.Chmod(fx.ro, 0o755); err != nil { //nolint:gosec // G302: a known mode the chmod probe compares against.
		return nil, fmt.Errorf("confinement: fixture: %w", err)
	}
	for _, path := range []string{filepath.Join(fx.git, "HEAD"), filepath.Join(fx.meta, workarea.DeclarationFileName), filepath.Join(fx.ext, "boundary")} {
		if err := seed(path); err != nil {
			return nil, err
		}
	}
	// Daemon-private secrets: a minted token file and a sibling secret
	// directly in the fixture directory, the way a host state home holds
	// the control token beside the per-session work areas. The token is
	// denied outright and the directory its listing, through the
	// production DeniedPaths and DeniedListings path, so every positive
	// control below also proves a work area beneath a daemon-private
	// directory keeps working. The token is seeded before the boundary
	// renders so both the live rendering and the loose canonicalization
	// (which must also cover a token minted after the seat starts) are
	// judged. dir stands in for the real host state home, so the probe
	// never touches operator state.
	fx.daemonFile = filepath.Join(dir, "control-token")
	fx.daemonSibling = filepath.Join(dir, "sibling-secret")
	if err := os.WriteFile(fx.daemonFile, []byte("sentinel-control-token\n"), 0o600); err != nil { //nolint:gosec // G306: fixture secret.
		return nil, fmt.Errorf("confinement: fixture: daemon secrets: %w", err)
	}
	if err := seed(fx.daemonSibling); err != nil {
		return nil, err
	}
	// The nested token lives inside the writable set (harness state),
	// so the blanket write deny cannot cover it: only the
	// daemon-private write deny holds it.
	fx.daemonNested = filepath.Join(fx.state, "daemon-state")
	fx.daemonNestedFile = filepath.Join(fx.daemonNested, "control-token")
	if err := os.MkdirAll(fx.daemonNested, 0o700); err != nil {
		return nil, fmt.Errorf("confinement: fixture: daemon secrets: %w", err)
	}

	// Positive controls first: without them a profile that denies
	// everything passes.
	for _, class := range []struct{ name, dir string }{
		{"mutable_leaf", fx.mut},
		{"mutable_leaf_git", fx.git},
		{"harness_state", fx.state},
		{"session_tmp", fx.tmp},
		{"session_cache", fx.cache},
	} {
		if err := fx.positive(class.name, class.dir); err != nil {
			return nil, err
		}
	}
	reported := func(res stepResult) bool { return res.Err == "" }
	fx.add(classPositive, true, probeStep{ID: "allow.session_tmp.socket", Op: opListen, Path: filepath.Join(fx.tmp, "s")}, reported)
	fx.add(classPositive, true, probeStep{ID: "allow.dev.null", Op: opDevWrite, Path: "/dev/null"}, reported)
	fx.add(classPositive, true, probeStep{ID: "allow.stdout", Op: opStdout}, reported)
	// A controlling terminal only exists where the boundary carries one:
	// the macOS profile in an interactive session. The mount-namespace
	// backend's private /dev holds no tty, so the probe cannot pass there.
	if mode == agent.PromptModeHumanControlled && runtime.GOOS == "darwin" {
		fx.add(classPositive, true, probeStep{ID: "allow.dev.tty", Op: opDevWrite, Path: "/dev/tty"}, reported)
	}
	// The lookup positive needs the macOS scripting tool: off darwin the
	// tool is missing and the probe would fail for want of a tool, not of
	// a boundary. The lookup denies below still run everywhere: without
	// the tool they report nothing reachable, which is the denied shape.
	if runtime.GOOS == "darwin" {
		fx.add(classPositive, true, probeStep{ID: "allow.service_lookup", Op: opLookup, Services: []string{lookupControlService}},
			func(res stepResult) bool { code, ok := res.Lookups[lookupControlService]; return ok && code == 0 })
	}

	// Outside the writable set.
	shared, err := sharedLocations()
	if err != nil {
		return nil, fmt.Errorf("confinement: fixture: shared locations: %w", err)
	}
	sharedNames := []string{"shared_tmp", "shared_var_tmp", "user_tmp", "user_cache"}
	homeCaches := filepath.Join(home, "Library", "Caches")
	if err := os.MkdirAll(homeCaches, 0o700); err != nil {
		return nil, fmt.Errorf("confinement: fixture: %w", err)
	}
	decoy := ".donmai-confinement-" + fx.tag
	locations := []struct{ id, base, dir string }{
		{"home", home, filepath.Join(home, decoy)},
		{"state_home", stateHome, filepath.Join(stateHome, decoy)},
		{"home_caches", homeCaches, filepath.Join(homeCaches, decoy)},
		{"sibling_session", filepath.Join(dir, "sib"), filepath.Join(dir, "sib", "ws", "repo")},
		{"workarea_root", fx.ws, fx.meta},
		{"git_common_dir", filepath.Join(dir, "base"), filepath.Join(dir, "base", ".git")},
	}
	for i, location := range shared {
		name := fmt.Sprintf("shared_%d", i)
		if i < len(sharedNames) {
			name = sharedNames[i]
		}
		locations = append(locations, struct{ id, base, dir string }{name, location, filepath.Join(location, decoy)})
	}
	for _, location := range locations {
		if err := fx.outside(location.id, location.base, location.dir); err != nil {
			return nil, err
		}
	}
	outsideDirs := make([]readLocation, 0, len(locations))
	for _, location := range locations {
		outsideDirs = append(outsideDirs, readLocation{id: location.id, dir: location.dir})
	}
	if err := fx.readScope(home, stateHome, outsideDirs); err != nil {
		return nil, err
	}

	if err := fx.readOnly(host.diskImage, host.diskImageErr); err != nil {
		return nil, err
	}
	if err := fx.protected(); err != nil {
		return nil, err
	}
	if err := fx.daemonSecrets(); err != nil {
		return nil, err
	}
	if err := fx.widening(shared); err != nil {
		return nil, err
	}

	// Renames that move a whole leaf or an ancestor of a protected path run
	// last, and settle undoes any that got through before anything is
	// judged, so they cannot move paths the other probes are judged in.
	// The profile backend denies these renames as operations; a mount-tree
	// backend holds them because every writable root and protected path is
	// a mount point there, and a mount point cannot be renamed.
	fx.moveProbe(classProtected, "protected.rename", fx.ext, filepath.Join(fx.state, "ext-moved"))
	fx.moveProbe(classProtected, "protected.rename_state", fx.state, filepath.Join(fx.mut, ".h-moved"))
	fx.moveProbe(classProtected, "protected.rename_leaf_into_tmp", fx.mut, filepath.Join(fx.tmp, "mut-moved"))
	fx.moveProbe(classReadOnly, "ro.rename_leaf", fx.ro, filepath.Join(fx.tmp, "ro-moved"))
	return fx, nil
}

func (fx *fixture) moveProbe(class, id, from, to string) {
	fx.moves = append(fx.moves, move{id: id, from: from, to: to})
	fx.add(class, false, probeStep{ID: id, Op: opRename, Path: from, Path2: to}, func(stepResult) bool { return fx.moved[id] })
}

func (fx *fixture) spec(mode agent.PromptSessionMode) Spec {
	return Spec{
		SessionID:        "self-test-" + fx.tag,
		HarnessID:        "confinement-probe",
		SessionMode:      mode,
		WorkareaRoot:     fx.ws,
		MutableLeaves:    []string{fx.mut},
		HarnessState:     []string{fx.state},
		SessionTmp:       fx.tmp,
		Caches:           []Cache{{Env: probeCacheEnv, Dir: fx.cache}},
		ReadOnlyLeaves:   []string{fx.ro, fx.rom},
		Protected:        []string{fx.ext},
		DeniedPaths:      []string{fx.daemonFile, fx.daemonNestedFile, fx.daemonNested},
		DeniedListings:   []string{fx.dir},
		LoopbackTCPPorts: []int{fx.tcpOpenPort},
	}
}

// readSpec is the spec of the read-scope pass: the same world under the
// workarea read scope, with one declared read path outside the set.
func (fx *fixture) readSpec(mode agent.PromptSessionMode) Spec {
	spec := fx.spec(mode)
	spec.ReadScope = agent.FileReadWorkarea
	spec.ReadPaths = []string{fx.rp, fx.rpf}
	return spec
}

func (fx *fixture) add(class string, expectAccepted bool, step probeStep, effect func(stepResult) bool) {
	fx.steps = append(fx.steps, harnessStep{probe: step, class: class, expectAccepted: expectAccepted, effect: effect})
}

// markCrossDevice records, on the probe just added, that the host itself
// refuses it when a and b sit on different filesystems: a rename never
// crosses one, boundary or not.
func (fx *fixture) markCrossDevice(a, b string) {
	var sa, sb unix.Stat_t
	if unix.Stat(a, &sa) != nil || unix.Stat(b, &sb) != nil || sa.Dev == sb.Dev {
		return
	}
	fx.steps[len(fx.steps)-1].heldBy = heldByCrossDevice
}

// notProbe records a probe of the set this host cannot run.
func (fx *fixture) notProbe(id string, reason NotProbedReason, detail string) {
	fx.notProbed = append(fx.notProbed, NotProbed{ID: id, Reason: reason, Detail: detail})
}

func (fx *fixture) addRead(class string, expectAccepted bool, step probeStep, effect func(stepResult) bool) {
	fx.readSteps = append(fx.readSteps, harnessStep{probe: step, class: class, expectAccepted: expectAccepted, effect: effect})
}

// readLocation is one decoy directory outside the read allowlist.
type readLocation struct{ id, dir string }

// readXattrName is the extended attribute the read-scope pass plants on its
// decoys, so a read of an attribute is judged where a read of the content is.
const readXattrName = "user.donmai.confinement-read"

// compressedSources are OS files that are transparently compressed on every
// macOS install; the fixture clones one, still compressed, as a decoy.
var compressedSources = []string{
	"/usr/share/man/man1/ls.1",
	"/usr/share/man/man1/cat.1",
	"/usr/share/man/man1/cp.1",
	"/usr/share/man/man1/rm.1",
}

// The attributes a transparently compressed file keeps its contents in.
const (
	decmpfsXattr = "com.apple.decmpfs"
	resourceFork = "com.apple.ResourceFork"
	showCompress = xattrShowCompression
)

// readScope builds the read-scope pass. Positive controls first: every root
// of the writable set, the read-only leaf, the declared read path and a
// runtime path stay readable and listable, their extended attributes are
// readable, metadata stays readable outside the allowlist, and a write
// inside the set still lands. Then the refusals: reading a file in, reading
// an extended attribute of, and listing, every decoy outside the set,
// including the package data trees under the runtime allows and a
// transparently compressed file, whose contents live in attributes; opening
// a terminal; and listing the operator home and the host state home
// themselves — the shape of a search over the whole disk. The declared read
// path stays read-only, by the write rules.
func (fx *fixture) readScope(home, stateHome string, outside []readLocation) error {
	reported := func(res stepResult) bool { return res.Err == "" }
	pair := func(class string, expectAccepted bool, prefix, dir string, xattr bool) error {
		file := filepath.Join(dir, "rd")
		if err := seed(file); err != nil {
			return err
		}
		fx.addRead(class, expectAccepted, probeStep{ID: prefix + ".file", Op: opRead, Path: file}, reported)
		fx.addRead(class, expectAccepted, probeStep{ID: prefix + ".list", Op: opList, Path: dir}, reported)
		if !xattr {
			return nil
		}
		fx.addReadPlanted(class, expectAccepted, probeStep{ID: prefix + ".xattr", Op: opGetxattr, Path: file, Label: readXattrName}, reported, func() error {
			return unix.Setxattr(file, readXattrName, []byte("1"), 0)
		})
		return nil
	}
	for _, in := range []struct{ name, dir string }{
		{"mutable_leaf", fx.mut},
		{"mutable_leaf_git", fx.git},
		{"harness_state", fx.state},
		{"session_tmp", fx.tmp},
		{"session_cache", fx.cache},
		{"read_only_leaf", fx.ro},
		{"read_path", fx.rp},
		// A protected path stays readable: the harness loads the
		// runner-injected artifact it may not change.
		{"protected", fx.ext},
	} {
		if err := pair(classPositive, true, "allow.read."+in.name, in.dir, true); err != nil {
			return err
		}
	}
	// A declared read path that is one file in the operator home opens
	// that file and nothing beside it: the decoys in the home below and
	// the home listing still refuse.
	fx.rpf = filepath.Join(home, ".donmai-confinement-rc-"+fx.tag)
	if err := seed(fx.rpf); err != nil {
		return err
	}
	fx.cleanups = append(fx.cleanups, func() { _ = os.Remove(fx.rpf) })
	fx.addRead(classPositive, true, probeStep{ID: "allow.read.read_path_file", Op: opRead, Path: fx.rpf}, reported)
	fx.addRead(classPositive, true, probeStep{ID: "allow.read.runtime", Op: opRead, Path: "/bin/sh"}, reported)
	// The device nodes a process needs stay readable: the random devices and
	// the directory itself.
	fx.addRead(classPositive, true, probeStep{ID: "allow.read.dev.urandom", Op: opOpen, Path: "/dev/urandom"}, reported)
	fx.addRead(classPositive, true, probeStep{ID: "allow.read.dev.list", Op: opList, Path: "/dev"}, reported)
	written := filepath.Join(fx.mut, "rw")
	fx.addRead(classPositive, true, probeStep{ID: "allow.read.mutable_leaf.write", Op: opCreate, Path: written}, existsEffect(written))
	for _, location := range outside {
		// Extended-attribute reads are not mediated by Landlock: on a
		// backend that leaves the location metadata-visible (the Linux
		// mount tree binds the operator home and the host state home
		// read-only so stat keeps working), the attribute reads too.
		// Those three locations' xattr probes stay darwin-only, where
		// the profile denies attributes together with contents; every
		// other location stays hidden, and its xattr probe still runs.
		visible := runtime.GOOS != "darwin" && (location.id == "home" || location.id == "home_caches" || location.id == "state_home")
		if err := pair(classReadScope, false, "read.outside."+location.id, location.dir, !visible); err != nil {
			return err
		}
	}
	if len(outside) > 0 {
		fx.addRead(classPositive, true, probeStep{ID: "allow.read.metadata_outside", Op: opStat, Path: filepath.Join(outside[0].dir, "rd")}, reported)
	}
	fx.addRead(classReadScope, false, probeStep{ID: "read.outside.operator_home.list", Op: opList, Path: home}, reported)
	fx.addRead(classReadScope, false, probeStep{ID: "read.outside.state_home.list", Op: opList, Path: stateHome}, reported)
	if err := fx.packageData(pair); err != nil {
		return err
	}
	fx.terminal(reported)
	// Transparent compression is a macOS feature: off darwin the clone
	// fails with a setup error, and the self-test would fail for want of
	// a tool, not of a boundary.
	if len(outside) > 0 && runtime.GOOS == "darwin" {
		fx.compressed(reported, outside[0].dir)
	}
	// A declared read path is outside the writable set: the write rules,
	// not the read rules, keep it read-only.
	created := filepath.Join(fx.rp, "n")
	fx.addRead(classOutside, false, probeStep{ID: "outside.read_path.create", Op: opCreate, Path: created}, existsEffect(created))
	return nil
}

// addReadPlanted adds a read-pass probe that needs the fixture to plant
// something first, outside the boundary; a plant that fails fails the probe.
func (fx *fixture) addReadPlanted(class string, expectAccepted bool, step probeStep, effect func(stepResult) bool, plant func() error) {
	harness := harnessStep{probe: step, class: class, expectAccepted: expectAccepted, effect: effect}
	if err := plant(); err != nil {
		harness.setupErr = "fixture: " + step.ID + ": " + errnoText(err)
	}
	fx.readSteps = append(fx.readSteps, harness)
}

// packageData adds the decoys under the package manager data trees the
// runtime allows would otherwise open (Homebrew's var holds database
// directories and TLS keys). A tree is probed where the host has it and the
// operator may write it, which is where a package manager keeps its own
// data; a host without one has nothing there to read.
func (fx *fixture) packageData(pair func(class string, expectAccepted bool, prefix, dir string, xattr bool) error) error {
	for _, tree := range seatbeltPackageData {
		info, err := os.Stat(tree)
		if err != nil || !info.IsDir() || unix.Access(tree, unix.W_OK) != nil {
			continue
		}
		dir := filepath.Join(tree, ".donmai-confinement-"+fx.tag)
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: a decoy directory.
			return fmt.Errorf("confinement: fixture: package data decoy: %s", errnoText(err))
		}
		fx.cleanups = append(fx.cleanups, func() { _ = removeAllForce(dir) })
		id := "package_var." + filepath.Base(filepath.Dir(tree))
		if err := pair(classReadScope, false, "read.outside."+id, dir, true); err != nil {
			return err
		}
	}
	return nil
}

// terminal adds the device node probes: a pseudo-terminal slave the fixture
// owns must not open for reading, which would capture what is typed into any
// terminal on the host.
func (fx *fixture) terminal(reported func(stepResult) bool) {
	step := harnessStep{probe: probeStep{ID: "read.outside.dev_pty", Op: opOpen}, class: classReadScope, effect: reported}
	master, slave, err := pty.Open()
	if err != nil {
		step.setupErr = "fixture: pty: " + errnoText(err)
		fx.readSteps = append(fx.readSteps, step)
		return
	}
	fx.cleanups = append(fx.cleanups, func() { _ = slave.Close(); _ = master.Close() })
	step.probe.Path = slave.Name()
	// Outside the boundary the slave opens; that is what makes a refusal
	// under the profile mean something.
	if fd, err := unix.Open(step.probe.Path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0); err != nil {
		step.setupErr = "fixture: pty slave does not open outside the boundary: " + errnoText(err)
	} else {
		_ = unix.Close(fd)
	}
	fx.readSteps = append(fx.readSteps, step)
}

// compressed adds the compressed file probes. A transparently compressed
// file keeps its contents in extended attributes that getxattr returns when
// asked to show compression, whatever the file's own read rules say, so a
// decoy outside the allowlist must refuse them and the same file inside it
// (the declared read path) must serve them: without the control a refusal
// could be a missing attribute.
func (fx *fixture) compressed(reported func(stepResult) bool, outsideDir string) {
	for _, place := range []struct {
		prefix, dir    string
		class          string
		expectAccepted bool
	}{
		{"allow.read.compressed", fx.rp, classPositive, true},
		{"read.outside.compressed", outsideDir, classReadScope, false},
	} {
		path, err := cloneCompressed(place.dir)
		if err != nil {
			fx.readSteps = append(fx.readSteps, harnessStep{
				probe: probeStep{ID: place.prefix + ".decmpfs", Op: opGetxattr, Path: filepath.Join(place.dir, "cmp"), Label: decmpfsXattr, Flags: showCompress},
				class: place.class, expectAccepted: place.expectAccepted, effect: reported, setupErr: "fixture: compressed decoy: " + err.Error(),
			})
			continue
		}
		for _, attr := range []struct{ id, name string }{{"decmpfs", decmpfsXattr}, {"resource_fork", resourceFork}} {
			// Outside the boundary the attribute reads; that is what makes a
			// refusal under the profile mean something. A small file keeps its
			// contents in the decmpfs attribute alone.
			if _, err := getxattr(path, attr.name, nil, showCompress); err != nil {
				continue
			}
			fx.addRead(place.class, place.expectAccepted, probeStep{ID: place.prefix + "." + attr.id, Op: opGetxattr, Path: path, Label: attr.name, Flags: showCompress}, reported)
		}
	}
}

// cloneCompressed copies a compressed OS file into dir as "cmp", keeping it
// compressed, and returns its path.
func cloneCompressed(dir string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("transparent compression is a macOS feature")
	}
	dst := filepath.Join(dir, "cmp")
	for _, source := range compressedSources {
		if _, err := getxattr(source, decmpfsXattr, nil, showCompress); err != nil {
			continue
		}
		_ = os.Remove(dst)
		if out, err := exec.Command("/usr/bin/ditto", source, dst).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed tool, fixed sources.
			return "", fmt.Errorf("ditto %s: %w: %s", source, err, strings.TrimSpace(string(out)))
		}
		if _, err := getxattr(dst, decmpfsXattr, nil, showCompress); err != nil {
			return "", fmt.Errorf("the clone of %s is not compressed: %s", source, errnoText(err))
		}
		return dst, nil
	}
	return "", errors.New("no compressed OS file to clone")
}

// fileOps adds the file operation probes for one directory: create, write,
// truncate, permission, timestamp and extended-attribute change, remove and
// mkdir. Every target is seeded with known content, mode and time, so a
// change is observed rather than asserted.
func (fx *fixture) fileOps(class, prefix string, expectAccepted bool, dir string) error {
	for _, name := range []string{"w", "tr", "ch", "ut", "xa", "rm"} {
		if err := seed(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	at := func(name string) string { return filepath.Join(dir, name) }
	fx.add(class, expectAccepted, probeStep{ID: prefix + ".create", Op: opCreate, Path: at("n")}, existsEffect(at("n")))
	fx.add(class, expectAccepted, probeStep{ID: prefix + ".write", Op: opWrite, Path: at("w")}, changedEffect(at("w")))
	fx.add(class, expectAccepted, probeStep{ID: prefix + ".truncate", Op: opTruncate, Path: at("tr")}, changedEffect(at("tr")))
	fx.add(class, expectAccepted, probeStep{ID: prefix + ".chmod", Op: opChmod, Path: at("ch")}, modeEffect(at("ch"), 0o644))
	fx.add(class, expectAccepted, probeStep{ID: prefix + ".utimes", Op: opUtimes, Path: at("ut")}, timeEffect(at("ut")))
	fx.add(class, expectAccepted, probeStep{ID: prefix + ".xattr", Op: opXattr, Path: at("xa")}, xattrEffect(at("xa")))
	fx.add(class, expectAccepted, probeStep{ID: prefix + ".remove", Op: opRemove, Path: at("rm")}, func(stepResult) bool { return !exists(at("rm")) })
	fx.add(class, expectAccepted, probeStep{ID: prefix + ".mkdir", Op: opMkdir, Path: at("d")}, existsEffect(at("d")))
	return nil
}

func (fx *fixture) positive(class, dir string) error {
	prefix := "allow." + class
	if err := fx.fileOps(classPositive, prefix, true, dir); err != nil {
		return err
	}
	for _, name := range []string{"mv", "ln"} {
		if err := seed(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	at := func(name string) string { return filepath.Join(dir, name) }
	fx.add(classPositive, true, probeStep{ID: prefix + ".rename", Op: opRename, Path: at("mv"), Path2: at("mv2")},
		func(stepResult) bool { return exists(at("mv2")) && !exists(at("mv")) })
	fx.add(classPositive, true, probeStep{ID: prefix + ".hardlink", Op: opLink, Path: at("ln"), Path2: at("ln2")}, existsEffect(at("ln2")))
	fx.add(classPositive, true, probeStep{ID: prefix + ".symlink", Op: opSymlink, Path: at("sl"), Path2: at("w")}, existsEffect(at("sl")))
	return nil
}

// outside adds the refused probes for one location outside the writable
// set: the file operations inside a decoy directory there, a create directly
// in the location, and a rename into and out of it.
func (fx *fixture) outside(id, base, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: a decoy directory.
		return fmt.Errorf("confinement: fixture: decoy for %s: %s", id, errnoText(err))
	}
	if !strings.HasPrefix(dir, fx.dir+string(filepath.Separator)) {
		fx.cleanups = append(fx.cleanups, func() { _ = removeAllForce(dir) })
	}
	prefix := "outside." + id
	if err := fx.fileOps(classOutside, prefix, false, dir); err != nil {
		return err
	}
	direct := filepath.Join(base, ".donmai-confinement-direct-"+fx.tag)
	fx.cleanups = append(fx.cleanups, func() { _ = os.Remove(direct) })
	fx.add(classOutside, false, probeStep{ID: prefix + ".create_direct", Op: opCreate, Path: direct}, existsEffect(direct))
	source := filepath.Join(dir, "mv")
	if err := seed(source); err != nil {
		return err
	}
	stolen := filepath.Join(fx.mut, "stolen-"+id)
	fx.add(classOutside, false, probeStep{ID: prefix + ".rename_out", Op: opRename, Path: source, Path2: stolen},
		func(stepResult) bool { return !exists(source) || exists(stolen) })
	fx.markCrossDevice(dir, fx.mut)
	plant := filepath.Join(fx.mut, "plant-"+id)
	if err := seed(plant); err != nil {
		return err
	}
	planted := filepath.Join(dir, "planted")
	fx.add(classOutside, false, probeStep{ID: prefix + ".rename_in", Op: opRename, Path: plant, Path2: planted}, existsEffect(planted))
	fx.markCrossDevice(dir, fx.mut)
	return nil
}

// readOnly adds the read-only leaf probes: the file operations, renames
// within, into and out of the leaf, a hard link and a symbolic link planted
// in the mutable sibling, and a permission change on the leaf itself. The
// mount probe targets a second read-only leaf, so a volume it wrongly gets
// mounted cannot hide what the probes on the first one changed.
func (fx *fixture) readOnly(diskImage string, diskImageErr error) error {
	if err := fx.fileOps(classReadOnly, "ro", false, fx.ro); err != nil {
		return err
	}
	at := func(name string) string { return filepath.Join(fx.ro, name) }
	for _, name := range []string{"mv", "out", "ln", "sw"} {
		if err := seed(at(name)); err != nil {
			return err
		}
	}
	fx.add(classReadOnly, false, probeStep{ID: "ro.rename_within", Op: opRename, Path: at("mv"), Path2: at("mv2")},
		func(stepResult) bool { return !exists(at("mv")) || exists(at("mv2")) })
	outTarget := filepath.Join(fx.mut, "ro-out")
	fx.add(classReadOnly, false, probeStep{ID: "ro.rename_out", Op: opRename, Path: at("out"), Path2: outTarget},
		func(stepResult) bool { return !exists(at("out")) || exists(outTarget) })
	inSource := filepath.Join(fx.mut, "ro-in")
	if err := seed(inSource); err != nil {
		return err
	}
	fx.add(classReadOnly, false, probeStep{ID: "ro.rename_in", Op: opRename, Path: inSource, Path2: at("in")}, existsEffect(at("in")))
	hardlink := filepath.Join(fx.mut, "ro-hardlink")
	// The profile backend denies the link itself; a mount-tree backend
	// holds it because the read-only leaf and the mutable leaf are
	// different mounts, and a hard link never crosses one.
	fx.add(classReadOnly, false, probeStep{ID: "ro.hardlink_from_mutable", Op: opLink, Path: at("ln"), Path2: hardlink}, existsEffect(hardlink))
	fx.add(classReadOnly, false, probeStep{ID: "ro.symlink_write_from_mutable", Op: opSymlinkWrite, Path: at("sw"), Path2: filepath.Join(fx.mut, "ro-symlink")}, changedEffect(at("sw")))
	fx.add(classReadOnly, false, probeStep{ID: "ro.chmod_leaf", Op: opChmod, Path: fx.ro}, modeEffect(fx.ro, 0o755))
	// The mount probe takes the platform's own route to a mount over the
	// second read-only leaf. On macOS an unprivileged process attaches a
	// disk image. On Linux the only route is a nested user and mount
	// namespace, where the probe remounts the leaf read-write and creates a
	// file through it: the boundary's mounts are locked there, and the
	// file must never reach the host.
	switch runtime.GOOS {
	case "darwin":
		mount := harnessStep{
			probe:  probeStep{ID: "ro.mount_over", Op: opMount, Path: fx.rom, Path2: diskImage},
			class:  classReadOnly,
			effect: func(stepResult) bool { return fx.mounted },
		}
		if diskImageErr != nil {
			mount.setupErr = "fixture: disk image: " + errnoText(diskImageErr)
		}
		fx.steps = append(fx.steps, mount)
	case "linux":
		through := filepath.Join(fx.rom, "remounted")
		fx.add(classReadOnly, false, probeStep{ID: "ro.mount_over", Op: opNestedRemount, Path: fx.rom, Path2: through}, existsEffect(through))
	}
	fx.cleanups = append(fx.cleanups, fx.detach)
	return nil
}

// settle runs after the probe exits and before any effect is judged: it
// records whether a volume was mounted over the read-only leaf and detaches
// it, then records and undoes every directory move that got through, last
// first.
func (fx *fixture) settle() {
	fx.mounted = isMountPoint(fx.rom)
	fx.detach()
	fx.moved = map[string]bool{}
	for i := len(fx.moves) - 1; i >= 0; i-- {
		m := fx.moves[i]
		if !exists(m.from) && exists(m.to) {
			fx.moved[m.id] = true
			_ = os.Rename(m.to, m.from)
		}
	}
}

func (fx *fixture) detach() {
	if isMountPoint(fx.rom) {
		_ = exec.Command("/usr/bin/hdiutil", "detach", "-force", "-quiet", fx.rom).Run() //nolint:gosec // G204: fixed tool, fixture path.
	}
}

// protected adds the probes for a runner-injected artifact kept outside the
// set although it sits inside harness state.
func (fx *fixture) protected() error {
	at := func(name string) string { return filepath.Join(fx.ext, name) }
	for _, name := range []string{"w", "rm", "ch"} {
		if err := seed(at(name)); err != nil {
			return err
		}
	}
	// The harness reads the artifact it may not change: a boundary that
	// hides it instead of holding it read-only strips the harness of it.
	fx.add(classPositive, true, probeStep{ID: "allow.protected.read", Op: opRead, Path: filepath.Join(fx.ext, "boundary")}, func(res stepResult) bool { return res.Err == "" })
	fx.add(classProtected, false, probeStep{ID: "protected.write", Op: opWrite, Path: at("w")}, changedEffect(at("w")))
	fx.add(classProtected, false, probeStep{ID: "protected.create", Op: opCreate, Path: at("n")}, existsEffect(at("n")))
	fx.add(classProtected, false, probeStep{ID: "protected.remove", Op: opRemove, Path: at("rm")}, func(stepResult) bool { return !exists(at("rm")) })
	fx.add(classProtected, false, probeStep{ID: "protected.chmod", Op: opChmod, Path: at("ch")}, modeEffect(at("ch"), 0o644))
	return nil
}

// daemonSecrets adds the probes for the daemon-private paths and directory,
// in both session modes. With reads open (the write pass) the token refuses
// every read — contents, an open, its metadata — and the fixture directory
// that holds it refuses a listing, while every positive control, all of
// them beneath that directory, still works. Under the read scope the same
// reads refuse, and so does a read of a second token nested inside the
// writable set, which the session's read allowlist covers: only a
// daemon-private deny rendered after the allowlist holds it. Writes,
// creates and permission changes refuse too; the nested token's write is
// held by the daemon-private write deny alone, since the blanket write deny
// cannot cover anything inside the writable set. Writes are judged by
// effect on disk, so a backend that merely hides the error string still
// fails. The sibling secret beside the token is held by the blanket rules
// (a listing deny does not hide a named file), so its probes stay in their
// classes.
func (fx *fixture) daemonSecrets() error {
	reported := func(res stepResult) bool { return res.Err == "" }
	// The token holds a sentinel, not the decoy content: changedEffect
	// compares against the decoy, so the probes below judge against the
	// exact bytes each operation would leave.
	tokenChanged := func(stepResult) bool {
		raw, err := os.ReadFile(fx.daemonFile) //nolint:gosec // G304: fixture path.
		return err != nil || string(raw) != "sentinel-control-token\n"
	}
	// Reads with reads open: the daemon-private denies render in every
	// read scope.
	fx.add(classDaemonPrivate, false, probeStep{ID: "daemon.token.read", Op: opRead, Path: fx.daemonFile}, reported)
	fx.add(classDaemonPrivate, false, probeStep{ID: "daemon.token.open", Op: opOpen, Path: fx.daemonFile}, reported)
	fx.add(classDaemonPrivate, false, probeStep{ID: "daemon.token.stat", Op: opStat, Path: fx.daemonFile}, reported)
	fx.add(classDaemonPrivate, false, probeStep{ID: "daemon.dir.list", Op: opList, Path: fx.dir}, reported)
	fx.add(classDaemonPrivate, false, probeStep{ID: "daemon.nested.read", Op: opRead, Path: fx.daemonNestedFile}, reported)
	fx.add(classDaemonPrivate, false, probeStep{ID: "daemon.token.write", Op: opWrite, Path: fx.daemonFile}, tokenChanged)
	fx.add(classDaemonPrivate, false, probeStep{ID: "daemon.token.chmod", Op: opChmod, Path: fx.daemonFile}, modeEffect(fx.daemonFile, 0o600))
	siblingNew := filepath.Join(filepath.Dir(fx.daemonSibling), "planted")
	fx.add(classOutside, false, probeStep{ID: "daemon.sibling.create", Op: opCreate, Path: siblingNew}, existsEffect(siblingNew))
	// The nested token keeps its sentinel bytes and the planted sibling
	// never lands. The nested file must exist before the boundary
	// renders: resolution needs nothing on disk (loose
	// canonicalization), but the passes must judge a read and a write
	// that would otherwise land.
	if err := os.WriteFile(fx.daemonNestedFile, []byte("sentinel-nested-token\n"), 0o600); err != nil { //nolint:gosec // G306: fixture secret.
		return fmt.Errorf("confinement: fixture: daemon secrets: %w", err)
	}
	nestedChanged := func(stepResult) bool {
		raw, err := os.ReadFile(fx.daemonNestedFile) //nolint:gosec // G304: fixture path.
		return err != nil || string(raw) != "sentinel-nested-token\n"
	}
	nestedNew := filepath.Join(fx.daemonNested, "planted")
	fx.add(classDaemonPrivate, false, probeStep{ID: "daemon.nested.write", Op: opWrite, Path: fx.daemonNestedFile}, nestedChanged)
	fx.add(classDaemonPrivate, false, probeStep{ID: "daemon.nested.create", Op: opCreate, Path: nestedNew}, existsEffect(nestedNew))
	// Under the read scope.
	fx.addRead(classDaemonPrivate, false, probeStep{ID: "read.outside.daemon_token.file", Op: opRead, Path: fx.daemonFile}, reported)
	fx.addRead(classDaemonPrivate, false, probeStep{ID: "read.outside.daemon_token.open", Op: opOpen, Path: fx.daemonFile}, reported)
	fx.addRead(classDaemonPrivate, false, probeStep{ID: "read.outside.daemon_token.list", Op: opList, Path: fx.dir}, reported)
	fx.addRead(classDaemonPrivate, false, probeStep{ID: "read.daemon_nested.file", Op: opRead, Path: fx.daemonNestedFile}, reported)
	fx.addRead(classReadScope, false, probeStep{ID: "read.outside.daemon_sibling.file", Op: opRead, Path: fx.daemonSibling}, reported)
	return nil
}

// widening adds the probes that try to leave or replace the boundary:
// re-entering the backend, submitting a job to the per-user job launcher,
// having the preferences daemon write a domain, opening an application,
// reaching the scripting, launch and mount services, attaching to a process
// outside, connecting to a socket outside, dialing loopback TCP, and
// reaching the pasteboard.
func (fx *fixture) widening(shared []string) error {
	switch runtime.GOOS {
	case "darwin":
		if err := fx.wideningDarwin(); err != nil {
			return err
		}
	case "linux":
		if err := fx.wideningLinux(); err != nil {
			return err
		}
	}
	if len(shared) == 0 {
		return errors.New("confinement: fixture: no shared temporary location")
	}
	socket := filepath.Join(shared[0], "dcs-"+fx.tag+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("confinement: fixture: outside socket: %w", err)
	}
	fx.listener = listener
	fx.cleanups = append(fx.cleanups, func() { _ = listener.Close(); _ = os.Remove(socket) })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			fx.accepts.Add(1)
			_ = conn.Close()
		}
	}()
	fx.add(classWidening, false, probeStep{ID: "widen.socket_outside", Op: opDial, Path: socket},
		func(res stepResult) bool { return res.Err == "" || fx.accepts.Load() > 0 })

	// The loopback TCP write proxy: a listener on the loopback interface
	// answers every dial, so a dial that gets through is observed. One port
	// is declared for the probe session and one is not. The numeric IPv6
	// loopback and the hostname get their own probes on the same ports:
	// a profile rule that only matches IPv4 leaves them open.
	openListener, openPort, err := listenLoopback()
	if err != nil {
		return fmt.Errorf("confinement: fixture: loopback listener: %w", err)
	}
	fx.tcpOpen = openListener
	fx.tcpOpenPort = openPort
	fx.cleanups = append(fx.cleanups, func() { _ = openListener.Close() })
	go acceptLoop(openListener, &fx.tcpAccepts)
	closedListener, closedPort, err := listenLoopback()
	if err != nil {
		return fmt.Errorf("confinement: fixture: loopback listener: %w", err)
	}
	fx.tcpClosed = closedListener
	fx.tcpClosedPort = closedPort
	fx.cleanups = append(fx.cleanups, func() { _ = closedListener.Close() })
	go acceptLoop(closedListener, &fx.tcpAccepts)
	// The undeclared-port dials are judged by what the backend declares.
	// The macOS profile denies loopback except on declared ports, so there
	// they are widening probes and must refuse. The Linux mount-namespace
	// backend declares loopback egress open (loopbackEgressDeclarer): a
	// Landlock port rule carries no address, so filtering TCP connects by
	// port would cut every undeclared port on every address, remote
	// endpoints included. There they are egress probes and must go
	// through, which also proves the boundary left outbound TCP alone. A
	// backend that silently changes its network handle fails exactly the
	// probes that name it, either way.
	reported := func(res stepResult) bool { return res.Err == "" }
	undeclared := func(id, host string, port int) {
		if fx.loopbackOpen {
			fx.add(classEgress, true, probeStep{ID: "egress." + id + "_undeclared", Op: opTCPDial, Host: host, Port: port}, reported)
			return
		}
		fx.add(classWidening, false, probeStep{ID: "widen." + id, Op: opTCPDial, Host: host, Port: port}, reported)
	}
	undeclared("loopback_tcp", "", closedPort)
	fx.add(classPositive, true, probeStep{ID: "allow.loopback_tcp_declared", Op: opTCPDial, Port: openPort}, reported)
	// The IPv6 listeners reuse the same two ports, one declared and one
	// not. Where IPv6 loopback is unavailable and the backend must refuse
	// the undeclared dial, the probe fails closed with a setup error, like
	// the mount probe without its disk image: the self-test never passes
	// silently where it cannot judge a deny. A backend that leaves loopback
	// open has no deny to judge there, so it adds no IPv6 probe at all.
	ipv6Unavailable := func(err error) {
		if fx.loopbackOpen {
			return
		}
		fx.add(classWidening, false, probeStep{ID: "widen.loopback_tcp6", Op: opTCPDial, Host: "::1", Port: closedPort},
			func(stepResult) bool { return false })
		fx.steps[len(fx.steps)-1].setupErr = "fixture: IPv6 loopback: " + errnoText(err)
	}
	open6, err := listenLoopback6(openPort)
	if err != nil {
		ipv6Unavailable(err)
	} else {
		fx.tcpOpen6 = open6
		fx.cleanups = append(fx.cleanups, func() { _ = open6.Close() })
		go acceptLoop(open6, &fx.tcpAccepts)
		closed6, err := listenLoopback6(closedPort)
		if err != nil {
			_ = open6.Close()
			ipv6Unavailable(err)
		} else {
			fx.tcpClosed6 = closed6
			fx.cleanups = append(fx.cleanups, func() { _ = closed6.Close() })
			go acceptLoop(closed6, &fx.tcpAccepts)
			undeclared("loopback_tcp6", "::1", closedPort)
			fx.add(classPositive, true, probeStep{ID: "allow.loopback_tcp6_declared", Op: opTCPDial, Host: "::1", Port: openPort}, reported)
		}
	}
	// The hostname probe resolves to a loopback address on this host; it
	// dials the undeclared port so a name-resolution bypass fails it.
	undeclared("loopback_tcp_hostname", "localhost", closedPort)
	return nil
}

// wideningDarwin adds the macOS widening probes: re-entering the profile
// backend, submitting a job to the per-user job launcher, having the
// preferences daemon write a domain, opening an application, reaching the
// scripting, launch and mount services, and attaching to a process outside.
func (fx *fixture) wideningDarwin() error {
	reenter := filepath.Join(fx.out, "reenter")
	fx.add(classWidening, false, probeStep{ID: "widen.reenter_backend", Op: opReenter, Path: reenter}, existsEffect(reenter))

	job := filepath.Join(fx.out, "job")
	label := "dev.donmai.confinement.selftest." + fx.tag
	fx.cleanups = append(fx.cleanups, func() { _ = exec.Command("/bin/launchctl", "remove", label).Run() }) //nolint:gosec // G204: fixed tool, generated label.
	fx.add(classWidening, false, probeStep{ID: "widen.job_launcher", Op: opJobSubmit, Label: label, Path: job},
		func(res stepResult) bool { return res.Exit == 0 || waitExists(job) })

	domain := "dev.donmai.confinement.selftest." + fx.tag
	fx.cleanups = append(fx.cleanups, func() { removePreferenceDomain(domain) })
	fx.add(classWidening, false, probeStep{ID: "widen.preferences_write", Op: opPrefWrite, Label: domain},
		func(res stepResult) bool {
			return res.Exit == 0 || exec.Command("/usr/bin/defaults", "read", domain, "probe").Run() == nil //nolint:gosec // G204: fixed tool, generated domain.
		})

	app := filepath.Join(fx.out, "app")
	bundle, err := writeProbeApp(filepath.Join(fx.dir, "app"), app)
	if err != nil {
		return err
	}
	fx.add(classWidening, false, probeStep{ID: "widen.open_application", Op: opAppOpen, Path: app, Path2: bundle},
		func(res stepResult) bool { return res.Exit == 0 || waitExists(app) })

	for _, deny := range seatbeltLookupDenies {
		services := deny.services
		fx.add(classWidening, false, probeStep{ID: "widen.service_lookup." + deny.class, Op: opLookup, Services: services},
			func(res stepResult) bool {
				for _, service := range services {
					if code, ok := res.Lookups[service]; ok && code == 0 {
						return true
					}
				}
				return false
			})
	}

	fx.add(classWidening, false, probeStep{ID: "widen.attach_process", Op: opAttach, PID: os.Getpid()},
		func(res stepResult) bool { return res.Exit == 0 })
	return nil
}

// macOSWideningProbes are the macOS widening probes, by the mechanism each
// reaches through. None of these services exists on Linux; the per-user
// bus and service manager probes stand in for them there where the host
// runs one, and the record names each as not probed.
var macOSWideningProbes = []string{
	"widen.job_launcher", "widen.preferences_write", "widen.open_application",
	"widen.service_lookup.mount", "widen.service_lookup.apple_events",
	"widen.service_lookup.launch_services", "widen.service_lookup.pasteboard",
}

// wideningLinux adds the Linux widening probes, each aimed at something the
// fixture placed outside the boundary and judged by its effect there: a
// decoy process to signal, attach to and read, a nested boundary built from
// inside, an abstract unix socket, and the per-user bus and service manager
// where the host runs them. The macOS probes whose services Linux lacks are
// recorded as not probed, never counted as held.
func (fx *fixture) wideningLinux() error {
	for _, id := range macOSWideningProbes {
		fx.notProbe(id, NotProbedPlatform, "a macOS service; Linux has none to reach")
	}
	reported := func(res stepResult) bool { return res.Err == "" }

	// The decoy: the probe executable, outside the boundary, open to
	// attachment by any process of this user, creating a marker when
	// signalled.
	marker := filepath.Join(fx.out, "signalled")
	pid, decoyErr := fx.startDecoy(marker)
	// Signal first: an attached decoy would hold the signal in a stop.
	fx.add(classWidening, false, probeStep{ID: "widen.signal_process", Op: opSignal, PID: pid}, func(stepResult) bool { return waitExists(marker) })
	fx.add(classWidening, false, probeStep{ID: "widen.read_process", Op: opProcRead, PID: pid}, reported)
	fx.add(classWidening, false, probeStep{ID: "widen.attach_process", Op: opPtrace, PID: pid}, reported)
	if decoyErr != nil {
		// No decoy, nothing to judge: the three probes fail closed, like
		// the mount probe without its disk image.
		for i := len(fx.steps) - 3; i < len(fx.steps); i++ {
			fx.steps[i].setupErr = errnoText(decoyErr)
		}
	}

	// A nested boundary binding everything the probe sees must not reach
	// the output directory the outer boundary hides.
	reenter := filepath.Join(fx.out, "reenter")
	launcher, lookErr := exec.LookPath("bwrap")
	touch := firstExisting("/usr/bin/touch", "/bin/touch")
	if lookErr != nil || touch == "" {
		fx.notProbe("widen.reenter_backend", NotProbedHostService, "no bubblewrap or touch on this host to build a nested boundary with")
	} else {
		fx.add(classWidening, false, probeStep{ID: "widen.reenter_backend", Op: opNestedBoundary, Path: reenter, Path2: launcher, Label: touch}, existsEffect(reenter))
	}

	// The signal probe stays live on every kernel: the process namespace
	// hides the decoy, so a signal from inside must refuse whether or not
	// the signal scope exists. Only the abstract socket — reachable through
	// the shared network namespace without its scope — is recorded as
	// unholdable where the scope layer is missing.
	if scoped, why := abstractSocketsScoped(); !scoped {
		fx.notProbe("widen.abstract_socket", NotProbedKernel, why)
	} else {
		name := "donmai-confinement-" + fx.tag
		listener, err := net.Listen("unix", "@"+name)
		if err != nil {
			return fmt.Errorf("confinement: fixture: abstract socket: %w", err)
		}
		fx.cleanups = append(fx.cleanups, func() { _ = listener.Close() })
		var accepts atomic.Int32
		go acceptLoop(listener, &accepts)
		fx.add(classWidening, false, probeStep{ID: "widen.abstract_socket", Op: opDialAbstract, Label: name},
			func(res stepResult) bool { return res.Err == "" || accepts.Load() > 0 })
	}

	// The per-user bus and service manager: the Linux counterparts of the
	// macOS job launcher and services, where this host runs them.
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	for _, service := range []struct{ id, path string }{
		{"widen.user_bus", filepath.Join(runtimeDir, "bus")},
		{"widen.service_manager", filepath.Join(runtimeDir, "systemd", "private")},
	} {
		// Probed only where it answers outside the boundary: a socket file
		// with no service behind it refuses everyone, so a refusal there
		// would say nothing about the boundary.
		info, err := os.Stat(service.path)
		if err != nil || info.Mode()&fs.ModeSocket == 0 || dial(service.path) != nil {
			fx.notProbe(service.id, NotProbedHostService, "no per-user "+strings.TrimPrefix(service.id, "widen.")+" answers on this host")
			continue
		}
		fx.add(classWidening, false, probeStep{ID: service.id, Op: opDial, Path: service.path}, reported)
	}
	return nil
}

// startDecoy runs the probe executable outside the boundary as the decoy
// and returns its pid once it reports ready. Cleanup closes its input and
// reaps it.
func (fx *fixture) startDecoy(marker string) (int, error) {
	if len(fx.probeCommand) == 0 {
		return 0, errors.New("confinement: fixture: no probe command to run the decoy")
	}
	cmd := exec.Command(fx.probeCommand[0], fx.probeCommand[1:]...) //nolint:gosec // G204: the self-test's own probe command.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); key == ProbeEnv || key == landlockStageEnv {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, probeDecoyEnv+"="+marker)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, fmt.Errorf("confinement: fixture: decoy: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("confinement: fixture: decoy: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("confinement: fixture: decoy: %w", err)
	}
	fx.cleanups = append(fx.cleanups, func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != "ready\n" {
		return 0, fmt.Errorf("confinement: fixture: the decoy never became ready: %v", err)
	}
	return cmd.Process.Pid, nil
}

// firstExisting returns the first path that exists, or empty.
func firstExisting(paths ...string) string {
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// listenLoopback binds one loopback TCP port on a free port chosen by
// the kernel and returns the listener and its port.
func listenLoopback() (net.Listener, int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, 0, errors.New("not a TCP listener")
	}
	return listener, addr.Port, nil
}

// listenLoopback6 binds the IPv6 loopback on one fixed port: the same port
// the IPv4 listener already holds, so the declared-port allow is judged by
// address family, not by port number.
func listenLoopback6(port int) (net.Listener, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return listener, nil
}

// acceptLoop answers every dial on a loopback probe listener.
func acceptLoop(listener net.Listener, accepted *atomic.Int32) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted.Add(1)
		_ = conn.Close()
	}
}

func (fx *fixture) cleanup() {
	for i := len(fx.cleanups) - 1; i >= 0; i-- {
		fx.cleanups[i]()
	}
	fx.cleanups = nil
}

// removePreferenceDomain deletes a probe preference domain the preferences
// daemon wrote, and its file in the user's real home, which is the daemon's
// and not this process's HOME.
func removePreferenceDomain(domain string) {
	_ = exec.Command("/usr/bin/defaults", "delete", domain).Run() //nolint:gosec // G204: fixed tool, generated domain.
	if account, err := user.Current(); err == nil && account.HomeDir != "" {
		_ = os.Remove(filepath.Join(account.HomeDir, "Library", "Preferences", domain+".plist"))
	}
}

// writeProbeApp writes a background-only application bundle whose only act
// is to create marker. Opening it from inside the boundary must fail.
func writeProbeApp(dir, marker string) (string, error) {
	if strings.ContainsAny(marker, "'\n") {
		return "", errors.New("confinement: fixture: unusable marker path")
	}
	bundle := filepath.Join(dir, "Probe.app")
	contents := filepath.Join(bundle, "Contents")
	if err := os.MkdirAll(filepath.Join(contents, "MacOS"), 0o755); err != nil { //nolint:gosec // G301: an application bundle.
		return "", fmt.Errorf("confinement: fixture: probe application: %w", err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>probe</string>
<key>CFBundleIdentifier</key><string>dev.donmai.confinement.probe</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>LSBackgroundOnly</key><true/>
</dict></plist>
`
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(plist), 0o644); err != nil { //nolint:gosec // G306: a bundle manifest.
		return "", fmt.Errorf("confinement: fixture: probe application: %w", err)
	}
	script := "#!/bin/sh\n/usr/bin/touch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(contents, "MacOS", "probe"), []byte(script), 0o755); err != nil { //nolint:gosec // G306: the bundle executable.
		return "", fmt.Errorf("confinement: fixture: probe application: %w", err)
	}
	return bundle, nil
}

// createProbeDiskImage creates the small disk image the mount probe tries to
// attach over the read-only leaf. It is made outside the boundary, once per
// self-test.
func createProbeDiskImage(ctx context.Context, dir string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("no disk image tool on this OS")
	}
	path := filepath.Join(dir, "p.dmg")
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", "create", "-size", "1m", "-fs", "HFS+", "-volname", "dcprobe", "-ov", "-quiet", path).CombinedOutput() //nolint:gosec // G204: fixed tool, scratch path.
	if err != nil {
		return "", fmt.Errorf("hdiutil create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
}

func seed(path string) error {
	if err := os.WriteFile(path, []byte(decoyContent), 0o644); err != nil { //nolint:gosec // G306: fixture file.
		return fmt.Errorf("confinement: fixture: seed: %s", errnoText(err))
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: a known mode the chmod probe compares against.
		return fmt.Errorf("confinement: fixture: seed: %s", errnoText(err))
	}
	if err := os.Chtimes(path, decoyTime, decoyTime); err != nil {
		return fmt.Errorf("confinement: fixture: seed: %s", errnoText(err))
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func waitExists(path string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if exists(path) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return exists(path)
}

func existsEffect(path string) func(stepResult) bool {
	return func(stepResult) bool { return exists(path) }
}

func changedEffect(path string) func(stepResult) bool {
	return func(stepResult) bool {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: fixture path.
		return err != nil || string(raw) != decoyContent
	}
}

func modeEffect(path string, seeded fs.FileMode) func(stepResult) bool {
	return func(stepResult) bool {
		info, err := os.Stat(path)
		return err != nil || info.Mode().Perm() != seeded
	}
}

func timeEffect(path string) func(stepResult) bool {
	return func(stepResult) bool {
		info, err := os.Stat(path)
		return err != nil || !info.ModTime().Equal(decoyTime)
	}
}

func xattrEffect(path string) func(stepResult) bool {
	return func(stepResult) bool {
		_, err := unix.Getxattr(path, probeXattr, nil)
		return err == nil
	}
}

func isMountPoint(path string) bool {
	var self, parent syscall.Stat_t
	if syscall.Lstat(path, &self) != nil || syscall.Lstat(filepath.Dir(path), &parent) != nil {
		return false
	}
	return self.Dev != parent.Dev
}

// removeAllForce restores owner permissions below path, then removes it.
func removeAllForce(path string) error {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: restoring access to remove a fixture.
		}
		return nil
	})
	return os.RemoveAll(path)
}
