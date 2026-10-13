package muse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	credentials "github.com/RenseiAI/donmai/credentials-client"
	"github.com/RenseiAI/donmai/runtime/confinement"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// This file wraps the `muse serve` child in the executor OS confinement
// (runtime/confinement): the seatbelt profile on macOS, the mount-namespace
// stage on Linux. It mirrors provider/harness/pi/confinement.go's shape
// (gate → self-tested confiner → per-session plan → argv/env wrap) at the
// smaller surface this host needs: the serve child takes no state root, no
// policy extension, and no credential file — just argv, env, and a working
// directory — so the writable set is the workspace leaf plus a per-session
// temporary directory and toolchain caches outside every repository leaf.
//
// Where the session writes: the declared mutable leaves (the workspace leaf
// itself, or the authority's mutable leaves when the session carries one),
// plus a per-session temporary directory bound to TMPDIR/TMP/TEMP and
// per-session toolchain caches bound to their variables. Everything else —
// the workarea root itself, its metadata, read-only leaves, and any path
// outside the set — is refused by the OS profile, judged on open, not only
// on write.
//
// What the harness inherits: the serve child inherits exactly three
// descriptors, the stdin/stdout/stderr pipes os/exec creates (ExtraFiles is
// never set — see Handle.spawnChild), never a descriptor open on an
// out-of-set file: the boundary judges an open, so an inherited descriptor
// would bypass it.
//
// What the harness may read: everything, unless the host sets a read scope
// (Options.ConfinementReadScope, or DONMAI_MUSE_CONFINEMENT_READ=workarea,
// which also requires confinement). Under the workarea read scope the
// harness reads its writable set and read-only leaves, the runtime and
// toolchain paths the backend declares, the serve binary's own install
// root, the git user configuration under the operator home, and the paths
// the host declares (Options.ConfinementReadPaths,
// DONMAI_MUSE_CONFINEMENT_READ_PATHS) — per-session credential or
// configuration files an embedder places outside the workarea. Everything
// else refuses file contents and directory listings; metadata stays
// readable. The read settings are host-owned: a work item cannot set them.
//
// When confinement applies: exactly when it is requested
// (museConfinementEnabled) — the host's own configuration requires it for
// this harness (Options.RequireConfinement, or
// DONMAI_MUSE_CONFINEMENT=required), or the session carries a declared
// repository authority. A request is never best effort: no backend for this
// OS, a session with no working directory, a set the backend cannot
// represent, a missing or stale self-test, or an unsafe state path refuses
// the spawn. A session that does not request confinement spawns exactly as
// before.
//
// Composition with the host's own sandbox flags: the confinement wraps the
// whole serve argv AFTER serveArgs appends the session's lane flags
// (--disable-shell --disable-write on the read-only lane, --disable-sandbox
// when the provider opts out). The OS boundary sits outside whatever the
// host enforces itself: on the read-only lane both apply (defense in
// depth); with --disable-sandbox the host enforces nothing and the OS
// profile is the only boundary — which is exactly why a requested
// confinement is never dropped when the host sandbox is off. The lane flags
// are never removed or weakened by the confinement path.

// museHarnessID is the harness identity the confinement record carries.
const museHarnessID = "muse"

// museSelfTestTimeout bounds the one-time startup self-test. Zero means the
// confinement default.
var museSelfTestTimeout time.Duration

// executableDigestOf returns the sha256 digest of the harness executable at
// path, in the form the confinement self-test record carries. The digest is
// read from the file's actual bytes on every spawn-path setup — a constant
// or empty digest would defeat the record's staleness check, so this must
// stay a hash of the binary, never a label.
func executableDigestOf(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the resolved harness binary this provider will exec, not session input.
	if err != nil {
		return "", fmt.Errorf("muse: digest harness executable: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// museConfinementDirs carries the host directories the confiner needs. It is
// a struct so tests can point every directory at throwaway paths.
type museConfinementDirs struct {
	profileDir string
	home       string
	stateHome  string
	scratchDir string
	// cacheDir keeps a passing self-test for later worker processes on
	// this host (confinement.SelfTestOptions.CacheDir). Empty disables it.
	cacheDir string
}

// productionMuseConfinementDirs resolves the host directories from the
// operator home and the host state seam. Empty values mark what could not
// be resolved; the caller fails closed on those.
func productionMuseConfinementDirs() museConfinementDirs {
	home, _ := os.UserHomeDir()
	return museConfinementDirs{
		profileDir: statehome.StateDir("confinement-profiles"),
		home:       home,
		stateHome:  statehome.StateDir(""),
		scratchDir: filepath.Join(os.TempDir(), "muse-confinement-selftest"),
		cacheDir:   statehome.StateDir("confinement-selftest"),
	}
}

// seatDaemonPrivate resolves the daemon-private paths and directories for a
// seat whose work area is rooted at workareaRoot. It mirrors pi's
// seatDaemonPrivate: the token is the daemon's control-token file (the live
// bearer for the host's privileged control API), resolved the way the
// operator's CLI resolves it — the file-env override wins when absolute,
// else the host state home — so this package never spells a
// brand-specific path. It is denied outright. Its directory and the host
// state home hold the token beside the daemon's other files and, on a
// default host, the per-session work areas, so they are denied a listing
// instead of hidden: a seat working beneath them keeps working, while a
// token minted there after the seat starts is never enumerable. Every
// directory between one of them and the work area is denied a listing too,
// so the other sessions' work areas beside this one are not enumerable
// either.
//
// tokenPath is the daemon-resolved token path the spawner states for this
// session (Options.ControlTokenPath): the daemon strips the override
// variable from every worker it spawns, so reading it here would protect
// the default path while the live token sits at the override. An explicit
// path always wins; empty falls back to the process environment (a
// hand-started worker outside the spawner still sees the override).
func seatDaemonPrivate(workareaRoot, tokenPath string) daemonPrivate {
	var d daemonPrivate
	addDir := func(dir string) {
		if dir != "" && !slices.Contains(d.dirs, dir) {
			d.dirs = append(d.dirs, dir)
		}
	}
	var tops []string
	override := strings.TrimSpace(tokenPath)
	if override == "" {
		override = strings.TrimSpace(os.Getenv(runtimeenv.ControlTokenPathEnv))
	}
	if override != "" {
		// A relative override resolves to no token at all (the daemon
		// mints none there either), never to a guess.
		if filepath.IsAbs(override) {
			token := filepath.Clean(override)
			d.paths = append(d.paths, token)
			tops = append(tops, filepath.Dir(token))
		}
	} else if dir := statehome.StateDir(""); dir != "" {
		d.paths = append(d.paths, filepath.Join(dir, controlTokenLeaf))
	}
	if dir := statehome.StateDir(""); dir != "" {
		tops = append(tops, filepath.Clean(dir))
	}
	root := filepath.Clean(workareaRoot)
	for _, top := range tops {
		addDir(top)
		if workareaRoot == "" || !strings.HasPrefix(root, top+string(filepath.Separator)) {
			continue
		}
		for dir := filepath.Dir(root); dir != top && strings.HasPrefix(dir, top+string(filepath.Separator)); dir = filepath.Dir(dir) {
			addDir(dir)
		}
	}
	return d
}

// daemonPrivate is what a confined seat is denied of the daemon's own
// state: the paths it may neither read nor write (Spec.DeniedPaths) and the
// directories it may not list (Spec.DeniedListings).
type daemonPrivate struct {
	paths []string
	dirs  []string
}

// controlTokenLeaf is the token file leaf under the host state directory.
// It names the same file the host's control API mints and its CLI reads;
// the constant lives here (rather than importing the client package) so
// the provider does not depend on the control-client surface.
const controlTokenLeaf = "control-token"

// applyHostReadScope folds the host's read-scope settings into opts. The
// host environment can only tighten the scope; a level this provider cannot
// confine reads to, or a relative read path, refuses the provider rather
// than leaving reads open. A read scope requires confinement.
func applyHostReadScope(opts *Options) error {
	scope, err := supportedReadScope(opts.ConfinementReadScope)
	if err != nil {
		return err
	}
	if raw := strings.TrimSpace(os.Getenv(museConfinementReadEnvVar)); raw != "" {
		fromHost, err := supportedReadScope(agent.ExecutionSecurityLevel(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", museConfinementReadEnvVar, err)
		}
		if fromHost != "" {
			scope = fromHost
		}
	}
	opts.ConfinementReadScope = scope
	paths := append([]string(nil), opts.ConfinementReadPaths...)
	for _, path := range filepath.SplitList(os.Getenv(museConfinementReadPathsEnvVar)) {
		if path = strings.TrimSpace(path); path != "" {
			paths = append(paths, path)
		}
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("confinement read path %q is not absolute", path)
		}
	}
	opts.ConfinementReadPaths = paths
	if scope != "" {
		opts.RequireConfinement = true
	}
	return nil
}

// supportedReadScope normalizes a requested read scope: open reads to
// empty, workarea to itself; anything else is refused.
func supportedReadScope(level agent.ExecutionSecurityLevel) (agent.ExecutionSecurityLevel, error) {
	switch level {
	case "", agent.FileReadHost:
		return "", nil
	case agent.FileReadWorkarea:
		return agent.FileReadWorkarea, nil
	default:
		return "", fmt.Errorf("read scope %q is not supported for muse confinement (supported: %s, %s)", level, agent.FileReadHost, agent.FileReadWorkarea)
	}
}

// museReadScope is the read scope a confined session runs under and the
// paths outside its workarea it may read.
type museReadScope struct {
	level agent.ExecutionSecurityLevel
	paths []string
}

// sessionReadScope returns the read scope for this provider's confined
// sessions. Under a read scope the seat may also read the serve binary's
// own install root, the git user configuration (an unreadable one is fatal
// to the tools a seat runs), and the paths the host declares. The serve
// binary's own configuration lives under the operator home's login state,
// which the seat inherits through the host session — it is not a file the
// boundary grants.
func (p *Provider) sessionReadScope(home string) (museReadScope, error) {
	if p.opts.ConfinementReadScope == "" {
		return museReadScope{}, nil
	}
	root, err := confinement.InstallRoot(p.museBin)
	if err != nil {
		return museReadScope{}, fmt.Errorf("%w: muse confinement read scope: %v", agent.ErrSpawnFailed, err)
	}
	paths := []string{root}
	paths = append(paths, museHomeReadPaths(home)...)
	paths = append(paths, p.opts.ConfinementReadPaths...)
	return museReadScope{level: p.opts.ConfinementReadScope, paths: paths}, nil
}

// museHomeReadPaths are the user configuration files and directories under
// the operator home that the tools a seat runs read, declared whether or
// not they exist: git's user configuration (~/.gitconfig, ~/.config/git) —
// git treats an unreadable one as fatal. Kept to version-control
// configuration: the serve host's own login state is inherited through the
// host session, not read as files under the boundary.
func museHomeReadPaths(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".config", "git"),
	}
}

// museConfinerEntry is one cached, self-tested confiner.
type museConfinerEntry struct {
	confiner *confinement.Confiner
	digest   string
}

// museConfinementBackend returns the confinement backend for this OS. A
// package variable so a test can pin the no-backend refusal on a host that
// has one; production always takes the OS default.
var museConfinementBackend = confinement.DefaultBackend

var museConfinerCache struct {
	sync.Mutex
	entries map[string]*museConfinerEntry
}

// ensureMuseConfiner returns a confiner whose self-test passed for the
// harness binary's CURRENT digest, running the self-test on first use. It
// is called only for a session that requested confinement, so a host with
// no backend refuses (never run unconfined when confinement was
// requested). The cache is keyed by the host directories plus the
// executable digest: when the harness binary changes, its digest changes,
// the old entry no longer matches, and the cached attestation is not
// reused — the new binary earns its own self-test record. A process that
// cannot run the self-test fails here, before any harness spawns. Each
// worker is its own process, so a passing self-test is also kept on disk
// (dirs.cacheDir): a later worker reuses it while the backend, probe set,
// harness and worker executables and host directories are unchanged and
// the record is under a day old, instead of probing again on every seat
// start.
func ensureMuseConfiner(ctx context.Context, binary string, dirs museConfinementDirs, probeCommand []string) (*confinement.Confiner, error) {
	backend := museConfinementBackend()
	if backend == nil {
		return nil, fmt.Errorf("%w: muse confinement requested: %w", agent.ErrSpawnFailed,
			&confinement.Error{Reason: confinement.ReasonBackendAbsent, Detail: "no confinement backend for this operating system"})
	}
	if dirs.profileDir == "" || dirs.home == "" || dirs.stateHome == "" || dirs.scratchDir == "" {
		return nil, fmt.Errorf("%w: muse confinement host directories are unresolved", agent.ErrSpawnFailed)
	}
	digest, err := executableDigestOf(binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	key := strings.Join([]string{dirs.profileDir, dirs.home, dirs.stateHome, digest}, "\x00")
	museConfinerCache.Lock()
	if museConfinerCache.entries == nil {
		museConfinerCache.entries = map[string]*museConfinerEntry{}
	}
	if entry := museConfinerCache.entries[key]; entry != nil {
		museConfinerCache.Unlock()
		return entry.confiner, nil
	}
	museConfinerCache.Unlock()

	c, err := confinement.New(confinement.Options{
		Backend:          backend,
		ProfileDir:       dirs.profileDir,
		Home:             dirs.home,
		StateHome:        dirs.stateHome,
		ExecutableDigest: digest,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: muse confinement: %v", agent.ErrSpawnFailed, err)
	}
	selfCtx := ctx
	cancel := func() {}
	if museSelfTestTimeout > 0 {
		selfCtx, cancel = context.WithTimeout(ctx, museSelfTestTimeout)
	}
	defer cancel()
	if _, err := c.SelfTest(selfCtx, confinement.SelfTestOptions{
		ProbeCommand: probeCommand,
		ScratchDir:   dirs.scratchDir,
		Timeout:      museSelfTestTimeout,
		CacheDir:     dirs.cacheDir,
	}); err != nil {
		return nil, fmt.Errorf("%w: muse confinement self-test: %v", agent.ErrSpawnFailed, err)
	}
	museConfinerCache.Lock()
	museConfinerCache.entries[key] = &museConfinerEntry{confiner: c, digest: digest}
	museConfinerCache.Unlock()
	return c, nil
}

// confineMuseSession prepares the session's confinement plan: the closed
// writable set for this spawn. The workarea mapping follows the declared
// authority when the session carries one — mutable leaves writable,
// read-only leaves read-only — and otherwise treats the working directory
// itself as the one mutable leaf under its parent. The per-session
// temporary directory and toolchain caches live outside every repository
// leaf (sessionDir): confined builds keep working without reaching the
// shared caches outside the set. Resolver sockets are declared so name
// resolution keeps working inside the profile. The daemon's control-token
// file rides Spec.DeniedPaths and the directories holding it (and the work
// area) Spec.DeniedListings (seatDaemonPrivate): the spawner-stated token
// path when the daemon states one, else resolved the same way the
// operator's CLI resolves the token, so the seat's boundary denies the
// live token. A failure to resolve them is not fatal — an empty set
// confines exactly as before — but a resolved path that the boundary
// cannot render refuses the spawn: the session never runs half-denied.
//
// sessionDir is the caller-owned per-session directory holding the tmp and
// cache subdirectories; the caller creates it (os.MkdirTemp in spawnHandle)
// and removes it when the session stops. A nil confiner means the session
// did not request confinement: no plan. Any failure refuses the spawn —
// the plan never degrades to a weaker set.
func confineMuseSession(spec agent.Spec, workspaceRoot, sessionDir string, confiner *confinement.Confiner, reads museReadScope, tokenPath string) (*confinement.Plan, error) {
	if confiner == nil {
		return nil, nil
	}
	if sessionDir == "" {
		return nil, fmt.Errorf("%w: muse confinement needs a per-session directory", agent.ErrSpawnFailed)
	}
	tmpDir := filepath.Join(sessionDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: muse confinement session tmp: %v", agent.ErrSpawnFailed, err)
	}
	cacheBase := filepath.Join(sessionDir, "cache")
	caches := []confinement.Cache{
		{Env: "GOCACHE", Dir: filepath.Join(cacheBase, "go-build")},
		{Env: "GOMODCACHE", Dir: filepath.Join(cacheBase, "go-mod")},
		{Env: "NPM_CONFIG_CACHE", Dir: filepath.Join(cacheBase, "npm")},
	}
	for _, cache := range caches {
		if err := os.MkdirAll(cache.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("%w: muse confinement session cache: %v", agent.ErrSpawnFailed, err)
		}
	}
	cspec := museSeatSpec(spec, workspaceRoot, tmpDir, caches, reads, tokenPath)
	plan, err := confiner.Prepare(cspec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	return plan, nil
}

// museSeatSpec builds the seat confinement spec the spawner prepares: the
// session, its work area and the daemon-private paths it is denied.
// tokenPath is the spawner-stated daemon token path (Options, threaded
// from the daemon that minted the token): it reaches the daemon-private
// deny explicitly, never through the inherited environment the spawner
// strips.
func museSeatSpec(spec agent.Spec, workspaceRoot, tmpDir string, caches []confinement.Cache, reads museReadScope, tokenPath string) confinement.Spec {
	cspec := confinement.Spec{
		SessionID:   "muse-" + sanitizeMuseSessionKey(workspaceRoot),
		HarnessID:   museHarnessID,
		SessionMode: agent.PromptModeForSpec(spec),
		SessionTmp:  tmpDir,
		Caches:      caches,
		Sockets:     confinement.ResolverSockets(),
		ReadScope:   reads.level,
		ReadPaths:   reads.paths,
	}
	if authority := spec.RepositoryAuthority; authority != nil && authority.WorkareaRoot != "" {
		cspec.WorkareaRoot = authority.WorkareaRoot
		cspec.MutableLeaves = append([]string(nil), authority.MutablePaths...)
		cspec.ReadOnlyLeaves = append([]string(nil), authority.ReadOnlyPaths...)
	} else {
		cwd := filepath.Clean(workspaceRoot)
		cspec.WorkareaRoot = filepath.Dir(cwd)
		cspec.MutableLeaves = []string{cwd}
	}
	private := seatDaemonPrivate(cspec.WorkareaRoot, tokenPath)
	cspec.DeniedPaths, cspec.DeniedListings = private.paths, private.dirs
	return cspec
}

// sanitizeMuseSessionKey reduces the workspace path to the safe alphabet
// the profile renderer accepts. Empty maps to "session".
func sanitizeMuseSessionKey(s string) string {
	var b strings.Builder
	for _, r := range filepath.Base(filepath.Clean(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	if b.Len() == 0 {
		return "session"
	}
	return b.String()
}

// confineMuseSeatSpecForTest builds the seat spec the production entry
// point builds, minus the session tmp and caches: the work area and
// daemon-private lines under test are shared through museSeatSpec, not
// copied.
func confineMuseSeatSpecForTest(spec agent.Spec, workspaceRoot string, reads museReadScope, tokenPath string) confinement.Spec {
	return museSeatSpec(spec, workspaceRoot, "", nil, reads, tokenPath)
}

// endpointLoopbackPorts returns the port of the session's own model endpoint
// when that endpoint is on this machine — a local gateway binding or a local
// model server. Today the serve host reaches its models through the host
// session, not a loopback endpoint, so this always returns nil: it exists
// so a future keyed-local route declares its port the same way pi does
// (endpointLoopbackPorts there), rather than inventing a second shape.
func endpointLoopbackPorts(spec agent.Spec) ([]int, error) {
	if spec.Endpoint == nil || strings.TrimSpace(spec.Endpoint.BaseURL) == "" {
		return nil, nil
	}
	raw := spec.Endpoint.BaseURL
	if socket := strings.TrimSpace(os.Getenv(credentials.SocketEnvVar)); socket != "" {
		unescaped, err := url.PathUnescape(raw)
		if strings.Contains(raw, socket) || (err == nil && strings.Contains(unescaped, socket)) {
			return nil, fmt.Errorf("%w: the model endpoint names the credential socket", agent.ErrSpawnFailed)
		}
	}
	port, ok := loopbackPort(raw)
	if !ok {
		return nil, nil
	}
	for _, control := range daemonControlPorts() {
		if port == control {
			return nil, fmt.Errorf("%w: the model endpoint names the daemon control API port %d", agent.ErrSpawnFailed, port)
		}
	}
	return []int{port}, nil
}

// loopbackPort returns the port of rawURL when its host is this machine
// (localhost or a loopback address), defaulting by scheme.
func loopbackPort(rawURL string) (int, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return 0, false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return 0, false
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return 0, false
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// daemonControlPorts are the loopback ports of the daemon control API this
// worker can reach: the well-known port, and the port of the control URL the
// daemon stated in this worker's environment when it is on this machine.
func daemonControlPorts() []int {
	ports := []int{runtimeenv.DefaultDaemonControlPort}
	if raw := strings.TrimSpace(os.Getenv(runtimeenv.DaemonControlURLEnv)); raw != "" {
		if port, ok := loopbackPort(raw); ok && port != runtimeenv.DefaultDaemonControlPort {
			ports = append(ports, port)
		}
	}
	return ports
}

// confineMuseArgv wraps argv in the session's confinement plan. A nil plan
// (a session that did not request confinement) returns argv unchanged.
func confineMuseArgv(plan *confinement.Plan, argv []string) ([]string, error) {
	if plan == nil {
		return argv, nil
	}
	return plan.Command(argv)
}

// confineMuseEnv appends the plan's environment bindings (TMPDIR/TMP/TEMP
// and any cache bindings) after the already-composed child environment, so
// they win under last-entry-wins semantics. A nil plan adds nothing.
func confineMuseEnv(childEnv []string, plan *confinement.Plan) []string {
	if plan == nil {
		return childEnv
	}
	return append(childEnv, plan.Environment()...)
}
