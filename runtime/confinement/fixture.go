package confinement

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

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
}

// fixture is one session mode's probe world: a workarea with a mutable and a
// read-only leaf, harness state with a protected artifact inside it, session
// tmp and cache, decoys outside the set, and the write proxies in reach.
type fixture struct {
	dir, ws, meta, mut, git, state, ext, ro, tmp, cache, out string
	tag                                                      string
	steps                                                    []harnessStep
	cleanups                                                 []func()
	listener                                                 net.Listener
	accepts                                                  atomic.Int32
	// mounted records whether the mount probe got a volume over the
	// read-only leaf; settle takes it before detaching.
	mounted bool
}

func newFixture(dir string, mode agent.PromptSessionMode, home, stateHome, diskImage string, diskImageErr error) (fx *fixture, err error) {
	fx = &fixture{dir: dir, tag: randomSuffix()}
	defer func() {
		if err != nil {
			fx.cleanup()
		}
	}()
	fx.ws = filepath.Join(dir, "ws")
	fx.meta = filepath.Join(fx.ws, workarea.DeclarationDirName)
	fx.mut = filepath.Join(fx.ws, "mut")
	fx.git = filepath.Join(fx.mut, ".git")
	fx.state = filepath.Join(fx.mut, ".h")
	fx.ext = filepath.Join(fx.state, "ext")
	fx.ro = filepath.Join(fx.ws, "ro")
	fx.tmp = filepath.Join(dir, "t")
	fx.cache = filepath.Join(dir, "c")
	fx.out = filepath.Join(dir, "o")
	for _, path := range []string{fx.meta, fx.git, fx.ext, fx.ro, fx.tmp, fx.cache, fx.out} {
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
	if mode == agent.PromptModeHumanControlled {
		fx.add(classPositive, true, probeStep{ID: "allow.dev.tty", Op: opDevWrite, Path: "/dev/tty"}, reported)
	}
	fx.add(classPositive, true, probeStep{ID: "allow.service_lookup", Op: opLookup, Services: []string{lookupControlService}},
		func(res stepResult) bool { code, ok := res.Lookups[lookupControlService]; return ok && code == 0 })

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

	if err := fx.readOnly(diskImage, diskImageErr); err != nil {
		return nil, err
	}
	if err := fx.protected(); err != nil {
		return nil, err
	}
	if err := fx.widening(shared); err != nil {
		return nil, err
	}

	// Renames that move a whole leaf or an ancestor of a protected path run
	// last: when one wrongly succeeds it moves paths the earlier probes use.
	gone := func(path string) func(stepResult) bool {
		return func(stepResult) bool { return !exists(path) }
	}
	fx.add(classProtected, false, probeStep{ID: "protected.rename_ancestor", Op: opRename, Path: fx.ext, Path2: filepath.Join(fx.state, "ext-moved")}, gone(fx.ext))
	fx.add(classProtected, false, probeStep{ID: "protected.rename_state", Op: opRename, Path: fx.state, Path2: filepath.Join(fx.mut, ".h-moved")}, gone(fx.state))
	fx.add(classProtected, false, probeStep{ID: "protected.rename_leaf_into_tmp", Op: opRename, Path: fx.mut, Path2: filepath.Join(fx.tmp, "mut-moved")}, gone(fx.mut))
	fx.add(classReadOnly, false, probeStep{ID: "ro.rename_leaf", Op: opRename, Path: fx.ro, Path2: filepath.Join(fx.tmp, "ro-moved")}, gone(fx.ro))
	return fx, nil
}

func (fx *fixture) spec(mode agent.PromptSessionMode) Spec {
	return Spec{
		SessionID:      "self-test-" + fx.tag,
		HarnessID:      "confinement-probe",
		SessionMode:    mode,
		WorkareaRoot:   fx.ws,
		MutableLeaves:  []string{fx.mut},
		HarnessState:   []string{fx.state},
		SessionTmp:     fx.tmp,
		Caches:         []Cache{{Env: probeCacheEnv, Dir: fx.cache}},
		ReadOnlyLeaves: []string{fx.ro},
		Protected:      []string{fx.ext},
	}
}

func (fx *fixture) add(class string, expectAccepted bool, step probeStep, effect func(stepResult) bool) {
	fx.steps = append(fx.steps, harnessStep{probe: step, class: class, expectAccepted: expectAccepted, effect: effect})
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
	plant := filepath.Join(fx.mut, "plant-"+id)
	if err := seed(plant); err != nil {
		return err
	}
	planted := filepath.Join(dir, "planted")
	fx.add(classOutside, false, probeStep{ID: prefix + ".rename_in", Op: opRename, Path: plant, Path2: planted}, existsEffect(planted))
	return nil
}

// readOnly adds the read-only leaf probes: the file operations, renames
// within, into and out of the leaf, a hard link and a symbolic link planted
// in the mutable sibling, a permission change on the leaf itself and a mount
// over it.
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
	fx.add(classReadOnly, false, probeStep{ID: "ro.hardlink_from_mutable", Op: opLink, Path: at("ln"), Path2: hardlink}, existsEffect(hardlink))
	fx.add(classReadOnly, false, probeStep{ID: "ro.symlink_write_from_mutable", Op: opSymlinkWrite, Path: at("sw"), Path2: filepath.Join(fx.mut, "ro-symlink")}, changedEffect(at("sw")))
	fx.add(classReadOnly, false, probeStep{ID: "ro.chmod_leaf", Op: opChmod, Path: fx.ro}, modeEffect(fx.ro, 0o755))
	mount := harnessStep{
		probe:  probeStep{ID: "ro.mount_over", Op: opMount, Path: fx.ro, Path2: diskImage},
		class:  classReadOnly,
		effect: func(stepResult) bool { return fx.mounted },
	}
	if diskImageErr != nil {
		mount.setupErr = "fixture: disk image: " + errnoText(diskImageErr)
	}
	fx.steps = append(fx.steps, mount)
	fx.cleanups = append(fx.cleanups, fx.detach)
	return nil
}

// settle runs after the probe exits and before any effect is judged: it
// records whether a volume was mounted over the read-only leaf, then
// detaches it, so the volume cannot hide what the other probes changed.
func (fx *fixture) settle() {
	fx.mounted = isMountPoint(fx.ro) || isMountPoint(filepath.Join(fx.tmp, "ro-moved"))
	fx.detach()
}

func (fx *fixture) detach() {
	for _, path := range []string{fx.ro, filepath.Join(fx.tmp, "ro-moved")} {
		if isMountPoint(path) {
			_ = exec.Command("/usr/bin/hdiutil", "detach", "-force", "-quiet", path).Run() //nolint:gosec // G204: fixed tool, fixture path.
		}
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
	fx.add(classProtected, false, probeStep{ID: "protected.write", Op: opWrite, Path: at("w")}, changedEffect(at("w")))
	fx.add(classProtected, false, probeStep{ID: "protected.create", Op: opCreate, Path: at("n")}, existsEffect(at("n")))
	fx.add(classProtected, false, probeStep{ID: "protected.remove", Op: opRemove, Path: at("rm")}, func(stepResult) bool { return !exists(at("rm")) })
	fx.add(classProtected, false, probeStep{ID: "protected.chmod", Op: opChmod, Path: at("ch")}, modeEffect(at("ch"), 0o644))
	return nil
}

// widening adds the probes that try to leave or replace the boundary:
// re-entering the backend, submitting a job to the per-user job launcher,
// opening an application, reaching the scripting, launch and mount services,
// attaching to a process outside, and connecting to a socket outside.
func (fx *fixture) widening(shared []string) error {
	reenter := filepath.Join(fx.out, "reenter")
	fx.add(classWidening, false, probeStep{ID: "widen.reenter_backend", Op: opReenter, Path: reenter}, existsEffect(reenter))

	job := filepath.Join(fx.out, "job")
	label := "dev.donmai.confinement.selftest." + fx.tag
	fx.cleanups = append(fx.cleanups, func() { _ = exec.Command("/bin/launchctl", "remove", label).Run() }) //nolint:gosec // G204: fixed tool, generated label.
	fx.add(classWidening, false, probeStep{ID: "widen.job_launcher", Op: opJobSubmit, Label: label, Path: job},
		func(res stepResult) bool { return res.Exit == 0 || waitExists(job) })

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
	return nil
}

func (fx *fixture) cleanup() {
	for i := len(fx.cleanups) - 1; i >= 0; i-- {
		fx.cleanups[i]()
	}
	fx.cleanups = nil
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
