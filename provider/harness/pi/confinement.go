package pi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	credentials "github.com/RenseiAI/donmai/credentials-client"
	"github.com/RenseiAI/donmai/runtime/confinement"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/harnessstate"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// This file wraps the pi harness process in the executor OS confinement
// (runtime/confinement) on both spawn paths — the headless RPC child and the
// interactive PTY child — and gives pi's own per-session state one declared
// home, so the confinement can name it as harness state.
//
// Where the state lives: sessionStateRoot returns <cwd>/.pi-<leaf>, INSIDE
// the session working directory (the mutable repository leaf), keyed by the
// working directory's own name. This is deliberate. The executor-owned home
// for harness state outside every repository leaf belongs to a workarea
// layout that is still proposed, not accepted; until it lands, the
// confinement record treats harness state as living inside the selected
// leaf, which already covers it (ADR-2026-10-03 D2 and D2.6). A directory
// beside the working directory would sit in the parent every flat-layout
// session shares, where sessions can collide and which teardown does not
// remove; inside the working directory, the worktree lifecycle that removes
// the workarea removes the state with it, and no two sessions share it. The
// state root is still declared to the confinement as its own harness_state
// root, and the boundary extension inside it is protected, so the seat can
// write its state but cannot rewrite the extension that polices it.
//
// What the harness may write under confinement: the declared mutable leaves,
// the harness state root, and a per-session temporary directory the
// confinement binds to TMPDIR/TMP/TEMP, plus per-session toolchain caches
// bound to their variables. Everything else — the workarea root itself, its
// metadata, read-only leaves, the materialized boundary extension, and any
// path outside the set — is refused by the OS profile, judged on open, not
// only on write.
//
// What the parent writes before the child starts: the state root, the
// extension files and the tmp/cache directories, all through
// sessionStateFS (state_fs.go), which refuses a symbolic link anywhere on
// the way — on Resume the seat has had the whole previous run to plant one.
//
// What the harness inherits: the headless child inherits exactly three
// descriptors, the stdin/stdout/stderr pipes os/exec creates (ExtraFiles is
// never set — see newHeadlessChildCommand), and the interactive child
// inherits the PTY slave as its three standard descriptors. Neither spawn
// path hands the confined process a descriptor open on an out-of-set file:
// the boundary judges an open, so an inherited descriptor would bypass it.
// The descriptor tests in confinement_live_test.go prove this by listing the
// descriptors a confined child actually holds.
//
// What the harness may read: everything, unless the host sets a read scope
// (Options.ConfinementReadScope, or DONMAI_PI_CONFINEMENT_READ=workarea,
// which also requires confinement). Under the workarea read scope the
// harness reads its writable set and read-only leaves, the runtime and
// toolchain paths the backend declares, pi's own install root, the git, npm
// and pnpm user configuration under the operator home (homeReadPaths; the
// npm one is secret-bearing), and the paths the host declares
// (Options.ConfinementReadPaths, DONMAI_PI_CONFINEMENT_READ_PATHS) —
// per-session credential or configuration files an embedder places outside
// the workarea. Everything else refuses file contents and directory
// listings; metadata stays readable. The read settings are host-owned: a
// work item cannot set them.
//
// When confinement applies: exactly when it is requested
// (piConfinementEnabled, ADR-2026-10-03 D5.1) — the host's own configuration
// requires it for pi (Options.RequireConfinement, or DONMAI_PI_CONFINEMENT=
// required), or the session carries a declared repository authority. A
// request is never best effort: no backend for this OS, a session with no
// working directory, a set the backend cannot represent, a missing or stale
// self-test, or an unsafe state path refuses the spawn. A session that does
// not request confinement spawns exactly as before.

// piHarnessID is the harness identity the confinement record carries.
const piHarnessID = "pi"

// piSessionTmpDir is the per-session temporary directory name under the
// session state root. The confinement binds TMPDIR, TMP and TEMP to it.
const piSessionTmpDir = "tmp"

// piSessionCacheDir is the per-session toolchain-cache directory name under
// the session state root. Each toolchain cache (build, module and package
// manager state) gets its own subdirectory, bound to its environment
// variable, so confined builds keep working without reaching the shared
// caches outside the set.
const piSessionCacheDir = "cache"

// piSelfTestTimeout bounds the one-time startup self-test. Zero means the
// confinement default.
var piSelfTestTimeout time.Duration

// sanitizeStateKey reduces s to the safe alphabet the profile renderer and
// the filesystem both accept. Empty maps to "session".
func sanitizeStateKey(s string) string {
	var b strings.Builder
	for _, r := range s {
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

// sessionLeafKey derives the per-session directory key from the session's own
// worktree leaf (the base name of the working directory), never from the
// display name: the display name is shared across concurrent sessions running
// the same workflow, while the worktree leaf is unique per session. It is
// stable across a Resume of the same session, so a resumed session finds the
// state its first incarnation wrote.
func sessionLeafKey(spec agent.Spec) string {
	if base := filepath.Base(filepath.Clean(spec.Cwd)); base != "" && base != "." && base != string(filepath.Separator) {
		return sanitizeStateKey(base)
	}
	return "session"
}

// sessionStateRoot is the session's harness state directory:
// <cwd>/.pi-<worktree-leaf>. It holds everything pi writes that is not
// repository content: session storage (--session-dir), the per-session agent
// home (PI_CODING_AGENT_DIR), the materialized boundary extension and any
// injected deliveries. It lives inside the session's own workarea, keyed by
// the worktree leaf, so concurrent sessions never share it and the worktree
// lifecycle that removes the workarea removes the state with it.
func sessionStateRoot(spec agent.Spec) string {
	cwd := filepath.Clean(spec.Cwd)
	if cwd == "" || cwd == "." {
		return filepath.Join(os.TempDir(), ".pi-session")
	}
	return filepath.Join(cwd, ".pi-"+sessionLeafKey(spec))
}

// newSessionLayoutForSpec builds the session layout rooted at the session
// state root. The three paths keep their relative shapes (root/extension,
// root/extensions-injected, root/agent-home) so pi's own session-resume
// lookup — which requires the agent directory and the session-storage
// directory to differ — is unaffected; only the parent moved off the legacy
// shared in-checkout directory.
func newSessionLayoutForSpec(spec agent.Spec) sessionLayout {
	root := sessionStateRoot(spec)
	return sessionLayout{
		root:      root,
		extension: filepath.Join(root, extensionFileName),
		injected:  filepath.Join(root, injectedExtensionsDir),
		agentHome: filepath.Join(root, agentHomeDir),
	}
}

// materializeExtensionForSpec materializes the boundary extension into the
// session state root, after creating the root, and records the root in the
// checkout's exclude file. confined marks the layout strict: the session will
// run under the confinement, so the working directory itself must be a real
// directory and the exclude entry is written into the leaf's own .git
// directory without asking git (state_fs.go). Every write goes through
// sessionStateFS, so a symbolic link planted in the state refuses the spawn
// before anything is written. The extension is written fresh (never
// hard-linked from the shared cache): the confinement accounts hard links
// inside the writable set, and a link to a blob outside it would make the set
// unrepresentable.
func materializeExtensionForSpec(spec agent.Spec, confined bool) (sessionLayout, error) {
	layout := newSessionLayoutForSpec(spec)
	layout.strict = confined
	state, err := openSessionStateFS(layout)
	if err != nil {
		return layout, fmt.Errorf("pi: open state dir: %w", err)
	}
	defer func() { _ = state.Close() }()
	if err := state.mkdirAll(layout.root, 0o700); err != nil {
		return layout, fmt.Errorf("pi: create state dir: %w", err)
	}
	// Keep this session's state dir out of `git status` for the checkout
	// it sits in. The static harnessstate table cannot name it — the leaf
	// is per-session (`.pi-<worktree-leaf>`) — so the exact entry is
	// written at materialize time. A glob would over-match
	// prefix-sharing names the table deliberately leaves visible.
	// Deliberately best-effort: a session whose exclude file could not be
	// written is noisier, not broken. The one exception is a confined
	// session whose exclude path is a planted link: that refuses.
	entry := filepath.Base(layout.root) + "/"
	if confined {
		if err := state.ensureGitExcluded(entry); err != nil {
			return layout, fmt.Errorf("pi: git exclude: %w", err)
		}
	} else {
		_ = harnessstate.EnsureGitExcluded(spec.Cwd, entry)
	}
	if err := state.writeFile(layout.extension, extensionSource(), 0o600); err != nil {
		return layout, fmt.Errorf("pi: write policy extension: %w", err)
	}
	return layout, nil
}

// executableDigestOf returns the sha256 digest of the harness executable at
// path, in the form the confinement self-test record carries. The digest is
// read from the file's actual bytes on every spawn-path setup — a constant
// or empty digest would defeat the record's staleness check, so this must
// stay a hash of the binary, never a label.
func executableDigestOf(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the resolved harness binary this provider will exec, not session input.
	if err != nil {
		return "", fmt.Errorf("pi: digest harness executable: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// piConfinementDirs carries the host directories the confiner needs. It is a
// struct so tests can point every directory at throwaway paths.
type piConfinementDirs struct {
	profileDir string
	home       string
	stateHome  string
	scratchDir string
	// cacheDir keeps a passing self-test for later worker processes on
	// this host (confinement.SelfTestOptions.CacheDir). Empty disables it.
	cacheDir string
}

// productionConfinementDirs resolves the host directories from the operator
// home and the host state seam. Empty values mark what could not be
// resolved; the caller fails closed on those.
func productionConfinementDirs() piConfinementDirs {
	home, _ := os.UserHomeDir()
	return piConfinementDirs{
		profileDir: statehome.StateDir("confinement-profiles"),
		home:       home,
		stateHome:  statehome.StateDir(""),
		scratchDir: filepath.Join(os.TempDir(), "pi-confinement-selftest"),
		cacheDir:   statehome.StateDir("confinement-selftest"),
	}
}

// applyHostReadScope folds the host's read-scope settings into opts. The
// host environment can only tighten the scope; a level this provider cannot
// confine reads to, or a relative read path, refuses the provider rather
// than leaving reads open. A read scope requires confinement.
func applyHostReadScope(opts *Options) error {
	scope, err := supportedReadScope(opts.ConfinementReadScope)
	if err != nil {
		return err
	}
	if raw := strings.TrimSpace(os.Getenv(piConfinementReadEnvVar)); raw != "" {
		fromHost, err := supportedReadScope(agent.ExecutionSecurityLevel(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", piConfinementReadEnvVar, err)
		}
		if fromHost != "" {
			scope = fromHost
		}
	}
	opts.ConfinementReadScope = scope
	paths := append([]string(nil), opts.ConfinementReadPaths...)
	for _, path := range filepath.SplitList(os.Getenv(piConfinementReadPathsEnvVar)) {
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
		return "", fmt.Errorf("read scope %q is not supported for pi confinement (supported: %s, %s)", level, agent.FileReadHost, agent.FileReadWorkarea)
	}
}

// piReadScope is the read scope a confined session runs under and the
// paths outside its workarea it may read.
type piReadScope struct {
	level agent.ExecutionSecurityLevel
	paths []string
}

// sessionReadScope returns the read scope for this provider's confined
// sessions. Under a read scope a pi seat may also read pi's own install
// (the binary's install root: its node_modules tree, or its directory),
// the user configuration of the tools a seat runs (homeReadPaths), and the
// paths the host declares. pi's own configuration lives in the session
// state (PI_CODING_AGENT_DIR), inside the workarea already.
func (p *Provider) sessionReadScope(home string) (piReadScope, error) {
	if p.opts.ConfinementReadScope == "" {
		return piReadScope{}, nil
	}
	root, err := confinement.InstallRoot(p.binary)
	if err != nil {
		return piReadScope{}, fmt.Errorf("%w: pi confinement read scope: %v", agent.ErrSpawnFailed, err)
	}
	paths := []string{root}
	paths = append(paths, homeReadPaths(home)...)
	paths = append(paths, p.opts.ConfinementReadPaths...)
	return piReadScope{level: p.opts.ConfinementReadScope, paths: paths}, nil
}

// homeReadPaths are the user configuration files and directories under the
// operator home that the tools a seat runs read, declared whether or not
// they exist:
//
//   - git's user configuration (~/.gitconfig, ~/.config/git): git treats an
//     unreadable one as fatal;
//   - npm's user configuration (~/.npmrc), which pnpm reads too, and
//     pnpm's global configuration (~/Library/Preferences/pnpm on macOS,
//     ~/.config/pnpm elsewhere): pnpm 12 refuses to start when it cannot
//     read its global configuration.
//
// ~/.npmrc is secret-bearing: it holds the registry tokens private
// installs authenticate with, so declaring it keeps the reach a seat had
// with reads open, until those credentials are injected per session.
func homeReadPaths(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".config", "git"),
		filepath.Join(home, ".npmrc"),
		filepath.Join(home, "Library", "Preferences", "pnpm"),
		filepath.Join(home, ".config", "pnpm"),
	}
}

// piConfinerEntry is one cached, self-tested confiner.
type piConfinerEntry struct {
	confiner *confinement.Confiner
	digest   string
}

var piConfinerCache struct {
	sync.Mutex
	entries map[string]*piConfinerEntry
}

// ensurePiConfiner returns a confiner whose self-test passed for the harness
// binary's CURRENT digest, running the self-test on first use. It is called
// only for a session that requested confinement, so a host with no backend
// refuses (ADR-2026-10-03 D5.3: never run unconfined when confinement was
// requested). The cache is
// keyed by the host directories plus the executable digest: when the harness
// binary changes, its digest changes, the old entry no longer matches, and
// the cached attestation is not reused — the new binary earns its own
// self-test record. A process that cannot run the self-test fails here,
// before any harness spawns. Each worker is its own process, so a passing
// self-test is also kept on disk (dirs.cacheDir): a later worker reuses it
// while the backend, probe set, harness and worker executables and host
// directories are unchanged and the record is under a day old, instead of
// probing again on every seat start.
func ensurePiConfiner(ctx context.Context, binary string, dirs piConfinementDirs, probeCommand []string) (*confinement.Confiner, error) {
	backend := confinement.DefaultBackend()
	if backend == nil {
		return nil, fmt.Errorf("%w: pi confinement requested: %w", agent.ErrSpawnFailed,
			&confinement.Error{Reason: confinement.ReasonBackendAbsent, Detail: "no confinement backend for this operating system"})
	}
	if dirs.profileDir == "" || dirs.home == "" || dirs.stateHome == "" || dirs.scratchDir == "" {
		return nil, fmt.Errorf("%w: pi confinement host directories are unresolved", agent.ErrSpawnFailed)
	}
	digest, err := executableDigestOf(binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	key := strings.Join([]string{dirs.profileDir, dirs.home, dirs.stateHome, digest}, "\x00")
	piConfinerCache.Lock()
	if piConfinerCache.entries == nil {
		piConfinerCache.entries = map[string]*piConfinerEntry{}
	}
	if entry := piConfinerCache.entries[key]; entry != nil {
		piConfinerCache.Unlock()
		return entry.confiner, nil
	}
	piConfinerCache.Unlock()

	c, err := confinement.New(confinement.Options{
		Backend:          backend,
		ProfileDir:       dirs.profileDir,
		Home:             dirs.home,
		StateHome:        dirs.stateHome,
		ExecutableDigest: digest,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: pi confinement: %v", agent.ErrSpawnFailed, err)
	}
	selfCtx := ctx
	cancel := func() {}
	if piSelfTestTimeout > 0 {
		selfCtx, cancel = context.WithTimeout(ctx, piSelfTestTimeout)
	}
	defer cancel()
	if _, err := c.SelfTest(selfCtx, confinement.SelfTestOptions{
		ProbeCommand: probeCommand,
		ScratchDir:   dirs.scratchDir,
		Timeout:      piSelfTestTimeout,
		CacheDir:     dirs.cacheDir,
	}); err != nil {
		return nil, fmt.Errorf("%w: pi confinement self-test: %v", agent.ErrSpawnFailed, err)
	}
	piConfinerCache.Lock()
	piConfinerCache.entries[key] = &piConfinerEntry{confiner: c, digest: digest}
	piConfinerCache.Unlock()
	return c, nil
}

// confinePiSession prepares the session's confinement plan: the closed
// writable set for this spawn. The workarea mapping follows the declared
// authority when the session carries one — mutable leaves writable,
// read-only leaves read-only — and otherwise treats the working directory
// itself as the one mutable leaf under its parent. The session state root
// (already materialized, inside the session workarea) is the harness_state
// class; the per-session temporary directory is created here and bound to
// TMPDIR/TMP/TEMP; per-session toolchain caches (build, module and package
// manager state) are declared so confined builds keep working; the
// materialized boundary extension is protected even though it sits inside
// harness state, so a confined rename cannot carry it out from under its
// rule. Resolver sockets are declared so name resolution keeps working
// inside the profile, and a model endpoint on this machine has its port
// declared (endpointLoopbackPorts) so model calls keep working; an endpoint
// naming the daemon control API or the credential socket is refused first,
// before anything is written.
//
// The tmp and cache directories are created through sessionStateFS, so a
// link planted there refuses the spawn before anything is created outside.
// A nil confiner means the session did not request confinement: no plan.
// Any failure refuses the spawn — the plan never degrades to a weaker set.
func confinePiSession(spec agent.Spec, layout sessionLayout, confiner *confinement.Confiner, reads piReadScope) (*confinement.Plan, error) {
	if confiner == nil {
		return nil, nil
	}
	loopbackPorts, err := endpointLoopbackPorts(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: pi confinement: %v", agent.ErrSpawnFailed, err)
	}
	state, err := openSessionStateFS(layout)
	if err != nil {
		return nil, fmt.Errorf("%w: pi confinement state dir: %v", agent.ErrSpawnFailed, err)
	}
	defer func() { _ = state.Close() }()
	tmpDir := filepath.Join(layout.root, piSessionTmpDir)
	if err := state.mkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: pi confinement session tmp: %v", agent.ErrSpawnFailed, err)
	}
	cacheBase := filepath.Join(layout.root, piSessionCacheDir)
	// GOPATH holds the checksum database's state beside the module cache;
	// pnpm reads its store location from NPM_CONFIG_STORE_DIR up to
	// version 10 and from PNPM_CONFIG_STORE_DIR after, so both name the
	// one per-session store. XDG_RUNTIME_DIR is the per-user runtime
	// directory tools keep cross-process locks in: pnpm 12 takes its store
	// operation locks there, and in a fixed shared directory under /tmp
	// otherwise, which the confinement denies. The store is per-session, so
	// its lock domain is too.
	pnpmStore := filepath.Join(cacheBase, "pnpm-store")
	caches := []confinement.Cache{
		{Env: "GOCACHE", Dir: filepath.Join(cacheBase, "go-build")},
		{Env: "GOMODCACHE", Dir: filepath.Join(cacheBase, "go-mod")},
		{Env: "GOPATH", Dir: filepath.Join(cacheBase, "go-path")},
		{Env: "NPM_CONFIG_CACHE", Dir: filepath.Join(cacheBase, "npm")},
		{Env: "NPM_CONFIG_STORE_DIR", Dir: pnpmStore},
		{Env: "PNPM_CONFIG_STORE_DIR", Dir: pnpmStore},
		{Env: "XDG_RUNTIME_DIR", Dir: filepath.Join(cacheBase, "run")},
	}
	for _, cache := range caches {
		if err := state.mkdirAll(cache.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("%w: pi confinement session cache: %v", agent.ErrSpawnFailed, err)
		}
	}
	cspec := confinement.Spec{
		SessionID:   "pi-" + sessionLeafKey(spec),
		HarnessID:   piHarnessID,
		SessionMode: agent.PromptModeForSpec(spec),
		HarnessState: []string{
			layout.root,
		},
		SessionTmp:       tmpDir,
		Caches:           caches,
		Sockets:          confinement.ResolverSockets(),
		LoopbackTCPPorts: loopbackPorts,
		ReadScope:        reads.level,
		ReadPaths:        reads.paths,
	}
	if authority := spec.RepositoryAuthority; authority != nil && authority.WorkareaRoot != "" {
		cspec.WorkareaRoot = authority.WorkareaRoot
		cspec.MutableLeaves = append([]string(nil), authority.MutablePaths...)
		cspec.ReadOnlyLeaves = append([]string(nil), authority.ReadOnlyPaths...)
	} else {
		cwd := filepath.Clean(spec.Cwd)
		cspec.WorkareaRoot = filepath.Dir(cwd)
		cspec.MutableLeaves = []string{cwd}
	}
	if layout.extension != "" {
		if _, err := os.Lstat(layout.extension); err == nil {
			cspec.Protected = append(cspec.Protected, layout.extension)
		}
	}
	plan, err := confiner.Prepare(cspec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	return plan, nil
}

// endpointLoopbackPorts returns the port of the session's own model endpoint
// when that endpoint is on this machine — a local gateway binding or a local
// model server. The confinement closes outbound TCP to the local machine
// except on declared ports, and the harness's own model channel is one the
// adapter declares for the session (ADR-2026-10-03 D2.5); without it every
// model call of a gateway-routed session would fail. A remote endpoint, or
// none, declares nothing.
//
// The declaration opens the port, not one address: the backend renders it
// as localhost:<port>, which also opens that port on ::1 and on the host's
// own addresses. So an endpoint that names a supervisor channel is refused
// rather than opened: the daemon control API (its well-known port and the
// one this worker was told to dial) and the credential socket.
func endpointLoopbackPorts(spec agent.Spec) ([]int, error) {
	if spec.Endpoint == nil || strings.TrimSpace(spec.Endpoint.BaseURL) == "" {
		return nil, nil
	}
	raw := spec.Endpoint.BaseURL
	if socket := strings.TrimSpace(os.Getenv(credentials.SocketEnvVar)); socket != "" {
		unescaped, err := url.PathUnescape(raw)
		if strings.Contains(raw, socket) || (err == nil && strings.Contains(unescaped, socket)) {
			return nil, errors.New("the model endpoint names the credential socket")
		}
	}
	port, ok := loopbackPort(raw)
	if !ok {
		return nil, nil
	}
	for _, control := range daemonControlPorts() {
		if port == control {
			return nil, fmt.Errorf("the model endpoint names the daemon control API port %d", port)
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

// confinePiArgv wraps argv in the session's confinement plan. A nil plan (a
// session that did not request confinement) returns argv unchanged.
func confinePiArgv(plan *confinement.Plan, argv []string) ([]string, error) {
	if plan == nil {
		return argv, nil
	}
	return plan.Command(argv)
}

// confinePiEnv applies the plan's environment bindings (TMPDIR/TMP/TEMP and
// any cache bindings) to the already-composed child environment. The plan's
// session-tmp binding wins exactly once: a prior TMPDIR/TMP/TEMP entry —
// for example the executor-owned session scratch bound before the worker
// started — is replaced, never shadowed by a duplicate. A nil plan adds
// nothing.
func confinePiEnv(childEnv []string, plan *confinement.Plan) []string {
	if plan == nil {
		return childEnv
	}
	return plan.ApplyToEnv(childEnv)
}
