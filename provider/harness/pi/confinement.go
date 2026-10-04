package pi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
	"github.com/RenseiAI/donmai/runtime/harnessstate"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// This file wraps the pi harness process in the executor OS confinement
// (runtime/confinement) on both spawn paths — the headless RPC child and the
// interactive PTY child — and moves pi's own per-session state out of the
// working folder into a per-session harness state directory beside it, so the
// working folder itself can be confined as repository content.
//
// Where the state lives: sessionStateRoot returns the session's harness
// state directory. It is a sibling of the session working directory
// (<parent-of-cwd>/.pi-<key>), NOT inside it: the working directory is the
// mutable repository leaf the confinement declares, and pi's session
// storage, agent home and materialized extensions cannot live inside a leaf
// the harness itself may rewrite. The key is the session name when set, else
// the working directory's own name, sanitized to a safe alphabet. Sibling
// placement keeps cleanup owned by the same lifecycle that removes the
// workarea (the worktree manager deletes the parent), so no new orphan sweep
// is needed; DONMAI_PI_STATE_DIR overrides the parent for tests.
//
// What the harness may write under confinement: the declared mutable leaves,
// the harness state root, and a per-session temporary directory the
// confinement binds to TMPDIR/TMP/TEMP. Everything else — the workarea root
// itself, its metadata, read-only leaves, the materialized boundary
// extension, and any path outside the set — is refused by the OS profile,
// judged on open, not only on write.
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
// When confinement applies: only where a backend exists (macOS today) and
// the session has a working directory. Elsewhere the spawn proceeds exactly
// as before. Where a backend exists the wrap is mandatory, never best
// effort: a set the backend cannot represent, a missing self-test, or a
// stale record fails the spawn closed. The one deliberate fallback is a
// session with no working directory at all, which has nothing to confine.

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

// sessionLeafKey derives the per-session directory key from the session's own
// worktree leaf (the base name of the working directory), never from the
// display name: the display name is shared across concurrent sessions running
// the same workflow, while the worktree leaf is unique per session. It is
// stable across a Resume of the same session, so a resumed session finds the
// state its first incarnation wrote.
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
// session state root. It is materializeExtension rooted at the per-session
// state directory; the write, mode and digest-verification semantics are
// identical. The extension is written fresh (never hard-linked from the
// shared cache): the confinement accounts hard links inside the writable
// set, and a link to a blob outside it would make the set unrepresentable.
func materializeExtensionForSpec(spec agent.Spec) (sessionLayout, error) {
	layout := newSessionLayoutForSpec(spec)
	if err := os.MkdirAll(layout.root, 0o700); err != nil {
		return layout, fmt.Errorf("pi: create state dir: %w", err)
	}
	// Keep this session's state dir out of `git status` for the checkout
	// it sits in. The static harnessstate table cannot name it — the leaf
	// is per-session (`.pi-<worktree-leaf>`) — so the exact entry is
	// written at materialize time. A glob would over-match
	// prefix-sharing names the table deliberately leaves visible.
	// Deliberately best-effort, mirroring materializeExtension: a session
	// whose exclude file could not be written is noisier, not broken.
	_ = harnessstate.EnsureGitExcluded(spec.Cwd, filepath.Base(layout.root)+"/")
	if err := os.WriteFile(layout.extension, extensionSource(), 0o600); err != nil {
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
// binary's CURRENT digest, running the self-test on first use. The cache is
// keyed by the host directories plus the executable digest: when the harness
// binary changes, its digest changes, the old entry no longer matches, and
// the cached attestation is not reused — the new binary earns its own
// self-test record. A process that cannot run the self-test fails here,
// before any harness spawns.
func ensurePiConfiner(ctx context.Context, binary string, dirs piConfinementDirs, probeCommand []string) (*confinement.Confiner, error) {
	backend := confinement.DefaultBackend()
	if backend == nil {
		return nil, nil
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
// inside the profile.
//
// A nil confiner means no backend exists for this OS: the caller spawns
// unconfined, exactly as before. Any other failure refuses the spawn — the
// plan never degrades to a weaker set.
func confinePiSession(spec agent.Spec, layout sessionLayout, confiner *confinement.Confiner) (*confinement.Plan, error) {
	if confiner == nil {
		return nil, nil
	}
	tmpDir := filepath.Join(layout.root, piSessionTmpDir)
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: pi confinement session tmp: %v", agent.ErrSpawnFailed, err)
	}
	cacheBase := filepath.Join(layout.root, piSessionCacheDir)
	caches := []confinement.Cache{
		{Env: "GOCACHE", Dir: filepath.Join(cacheBase, "go-build")},
		{Env: "GOMODCACHE", Dir: filepath.Join(cacheBase, "go-mod")},
		{Env: "NPM_CONFIG_CACHE", Dir: filepath.Join(cacheBase, "npm")},
	}
	for _, cache := range caches {
		if err := os.MkdirAll(cache.Dir, 0o700); err != nil {
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
		SessionTmp: tmpDir,
		Caches:     caches,
		Sockets:    confinement.ResolverSockets(),
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

// confinePiArgv wraps argv in the session's confinement plan. A nil plan is
// the no-backend case and returns argv unchanged.
func confinePiArgv(plan *confinement.Plan, argv []string) ([]string, error) {
	if plan == nil {
		return argv, nil
	}
	return plan.Command(argv)
}

// confinePiEnv appends the plan's environment bindings (TMPDIR/TMP/TEMP and
// any cache bindings) after the already-composed child environment, so they
// win under last-entry-wins semantics. A nil plan adds nothing.
func confinePiEnv(childEnv []string, plan *confinement.Plan) []string {
	if plan == nil {
		return childEnv
	}
	return append(childEnv, plan.Environment()...)
}
