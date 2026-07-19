package worktree

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/internal/gitexec"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// MaxSpawnRetries is the maximum number of attempts Provision will
// make before failing. Mirrors MAX_SPAWN_RETRIES in the legacy TS
// worker-runner.ts.
const MaxSpawnRetries = 3

// SpawnRetryDelay is the wait between Provision attempts. Mirrors
// SPAWN_RETRY_DELAY_MS in the legacy TS.
const SpawnRetryDelay = 15 * time.Second

// CloneStrategy selects the underlying git operation Provision uses
// to materialize a worktree directory.
type CloneStrategy int

const (
	// StrategyClone runs `git clone` into a fresh directory. Used when
	// no parent worktree exists yet, or when the daemon is configured
	// to keep sessions fully isolated.
	StrategyClone CloneStrategy = iota
	// StrategyWorktreeAdd runs `git worktree add` off an existing
	// parent clone. Cheap and ideal when many sessions share a
	// long-lived parent under the daemon's clone directory.
	StrategyWorktreeAdd
)

// String returns a stable name for the strategy — used in log lines
// and error messages.
func (s CloneStrategy) String() string {
	switch s {
	case StrategyClone:
		return "clone"
	case StrategyWorktreeAdd:
		return "worktree-add"
	default:
		return "unknown"
	}
}

// Sentinel errors callers may type-check via errors.Is.
var (
	// ErrLostOwnership is returned by Provision when the OwnershipProber
	// reports that another worker has claimed this session between
	// retry attempts. The runner halts work without further retries.
	ErrLostOwnership = errors.New("runtime/worktree: ownership lost during retry")

	// ErrUnknownSession is returned by Path when the session id has
	// no recorded worktree.
	ErrUnknownSession = errors.New("runtime/worktree: unknown session id")

	// ErrNoParentDir is returned by Provision when the manager has no
	// ParentDir configured and the strategy needs one.
	ErrNoParentDir = errors.New("runtime/worktree: no parent directory configured")
)

// OwnershipProber is the runner-supplied callback Provision uses to
// confirm session ownership before each retry. Implementations
// typically call afclient.GetSession and compare OwnerWorkerID to the
// daemon's local id. Returning (false, nil) means "lost"; an error
// is treated as "transient — keep retrying".
type OwnershipProber func(ctx context.Context, sessionID string) (owned bool, err error)

// CommandRunner abstracts process execution for tests. The default
// implementation is exec.CommandContext + cmd.CombinedOutput().
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// EnvCommandRunner is the env-aware variant of CommandRunner. It is only
// consulted on the GitAuth-engaged path: when a GitAuth callback is set, the
// manager builds a hardened env via gitexec.HardenedEnv and runs the git
// invocation through this runner so the env reaches the subprocess. The default
// (no GitAuth) path never touches it, keeping behaviour byte-identical to a
// manager built without the seam.
//
// extraEnv entries (each "KEY=VALUE") are appended to the inherited process
// environment after runner-only attach controls are removed from both layers.
// The default implementation is exec.CommandContext + cmd.CombinedOutput().
type EnvCommandRunner func(ctx context.Context, extraEnv []string, name string, args ...string) ([]byte, error)

// GitAuth is the daemon-supplied, per-invocation git auth resolver. Given the
// repo URL about to be cloned/operated on, it returns the HTTP authorization
// header to inject (e.g. "Authorization: Bearer <token>" or
// "AUTHORIZATION: basic <base64>") and whether the OS credential helper must be
// suppressed (which avoids the launchd keychain-popup hang).
//
// The seam is INERT by default: when GitAuth is nil the manager runs git
// exactly as before — no env hardening, no URL rewriting. When set, each git
// invocation that touches a remote applies gitexec.HardenedEnv(env, suppress,
// header), and a clone of a URL carrying embedded userinfo clones the
// userinfo-stripped URL so the token never persists in .git/config (auth flows
// through the injected http.extraHeader instead).
//
// Returning an error aborts the provisioning attempt with that error wrapped.
// Returning ("", false, nil) is valid and means "no header, do not suppress" —
// equivalent to a no-op for that invocation.
type GitAuth func(ctx context.Context, repoURL string) (authHeader string, suppressHelper bool, err error)

// defaultRunner is the production CommandRunner; tests inject a stub.
func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	// nolint:gosec // G204: name is a hard-coded "git" binary; args are
	// constructed from validated ProvisionSpec fields at this layer.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = runtimeenv.FilterRunnerOnly(cmd.Environ())
	return cmd.CombinedOutput()
}

// defaultEnvRunner is the production EnvCommandRunner; tests inject a stub.
func defaultEnvRunner(ctx context.Context, extraEnv []string, name string, args ...string) ([]byte, error) {
	// nolint:gosec // G204: name is a hard-coded "git" binary; args are
	// constructed from validated ProvisionSpec fields at this layer.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(runtimeenv.FilterRunnerOnly(cmd.Environ()), runtimeenv.FilterRunnerOnly(extraEnv)...)
	return cmd.CombinedOutput()
}

// ProvisionResult is the per-session bookkeeping the manager records.
// Returned alongside the worktree path for callers that need both.
type ProvisionResult struct {
	// Path is the absolute worktree path on disk.
	Path string
	// Strategy is the strategy that succeeded.
	Strategy CloneStrategy
	// ParentRepoPath is the parent clone used by StrategyWorktreeAdd.
	ParentRepoPath string
	// Attempts is the number of attempts taken (1 on success first try).
	Attempts int
}

// Manager owns the lifecycle of per-session worktrees. The zero value
// is unusable; build via NewManager.
//
// Concurrency: the Manager serializes Provision/Teardown for the
// same session id but allows different sessions to run in parallel.
type Manager struct {
	parentDir string
	logger    *slog.Logger
	prober    OwnershipProber
	runner    CommandRunner
	envRunner EnvCommandRunner
	gitAuth   GitAuth
	delay     time.Duration
	leases    *workarea.LeaseStore

	mu           sync.Mutex
	sessions     map[string]*ProvisionResult
	sessionLocks map[string]*sync.Mutex
}

// Options configures NewManager. ParentDir is required.
type Options struct {
	// ParentDir is the daemon-controlled directory under which
	// per-session worktrees are created. Required.
	ParentDir string
	// Logger overrides slog.Default().
	Logger *slog.Logger
	// OwnershipProber is invoked between retries; nil disables the
	// ownership check (useful for unit tests with no platform).
	OwnershipProber OwnershipProber
	// CommandRunner overrides the default exec.CommandContext runner. Used on
	// the inert path (no GitAuth) and for env-free git invocations.
	CommandRunner CommandRunner
	// EnvCommandRunner overrides the default env-aware runner. It is only
	// consulted when GitAuth is set; nil defaults to defaultEnvRunner. Tests
	// inject a recording stub to assert the hardened env reaches the
	// subprocess.
	EnvCommandRunner EnvCommandRunner
	// GitAuth, when non-nil, engages the credential-hardening seam: each git
	// invocation that touches a remote runs with gitexec.HardenedEnv applied,
	// and clones strip embedded userinfo from the URL. Nil leaves the manager
	// byte-identical to today (no env hardening, no URL rewriting).
	GitAuth GitAuth
	// RetryDelay overrides SpawnRetryDelay. Useful for tests.
	RetryDelay time.Duration
	// Now supplies the clock used by the durable terminal lease store.
	Now func() time.Time
	// LeaseStore overrides the crash-recoverable terminal lease store. Nil
	// creates one beside ParentDir.
	LeaseStore *workarea.LeaseStore
}

// NewManager returns a Manager configured by opts.
func NewManager(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.ParentDir) == "" {
		return nil, ErrNoParentDir
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runner := opts.CommandRunner
	if runner == nil {
		runner = defaultRunner
	}
	envRunner := opts.EnvCommandRunner
	if envRunner == nil {
		envRunner = defaultEnvRunner
	}
	delay := opts.RetryDelay
	if delay == 0 {
		delay = SpawnRetryDelay
	}
	abs, err := filepath.Abs(opts.ParentDir)
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: resolve ParentDir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("runtime/worktree: mkdir ParentDir: %w", err)
	}
	leases := opts.LeaseStore
	if leases == nil {
		leases, err = workarea.NewLeaseStore(workarea.StoreOptions{
			Dir: filepath.Join(abs, ".terminal-leases"),
			Now: opts.Now,
		})
		if err != nil {
			return nil, fmt.Errorf("runtime/worktree: terminal lease store: %w", err)
		}
	}
	return &Manager{
		parentDir:    abs,
		logger:       logger,
		prober:       opts.OwnershipProber,
		runner:       runner,
		envRunner:    envRunner,
		gitAuth:      opts.GitAuth,
		delay:        delay,
		leases:       leases,
		sessions:     make(map[string]*ProvisionResult),
		sessionLocks: make(map[string]*sync.Mutex),
	}, nil
}

// ParentDir returns the absolute path of the manager's parent
// directory.
func (m *Manager) ParentDir() string {
	return m.parentDir
}

// ProvisionSpec is one Provision call's input.
type ProvisionSpec struct {
	// SessionID is the platform session UUID — also the
	// worktree-directory leaf name.
	SessionID string
	// RepoURL is the git URL (or local path) to clone from. Required
	// for StrategyClone; may be empty for StrategyWorktreeAdd when
	// ParentRepoPath is set.
	RepoURL string
	// Branch is the branch to check out. Empty falls back to the
	// remote default branch.
	Branch string
	// Strategy selects clone vs worktree-add.
	Strategy CloneStrategy
	// ParentRepoPath is the existing parent clone for
	// StrategyWorktreeAdd. Empty defaults to ParentDir/<repo-leaf>.
	ParentRepoPath string
	// LeafName overrides the directory name under ParentDir. Empty
	// defaults to SessionID.
	LeafName string
}

// Provision creates a worktree for the session, retrying up to
// MaxSpawnRetries times with SpawnRetryDelay between attempts. Before
// each retry, OwnershipProber (if set) is consulted; ownership lost
// short-circuits with ErrLostOwnership.
//
// Returns the worktree path on success.
func (m *Manager) Provision(ctx context.Context, spec ProvisionSpec) (string, error) {
	if spec.SessionID == "" {
		return "", errors.New("runtime/worktree: SessionID required")
	}
	unlock := m.lockSession(spec.SessionID)
	defer unlock()
	parentRepoPath := spec.ParentRepoPath
	if parentRepoPath != "" {
		var err error
		parentRepoPath, err = filepath.Abs(parentRepoPath)
		if err != nil {
			return "", fmt.Errorf("runtime/worktree: resolve ParentRepoPath: %w", err)
		}
	}

	leaf := spec.LeafName
	if leaf == "" {
		leaf = spec.SessionID
	}
	dst := filepath.Join(m.parentDir, leaf)
	if filepath.Clean(dst) == filepath.Clean(m.leases.Dir()) {
		return "", errors.New("runtime/worktree: destination conflicts with terminal lease state directory")
	}
	retained, err := m.retainedPath(dst)
	if err != nil {
		return "", fmt.Errorf("runtime/worktree: check terminal lease before provision: %w", err)
	}
	if retained {
		return "", fmt.Errorf("%w: %s", workarea.ErrWorkareaLeased, dst)
	}

	var lastErr error
	var attempts int
	for attempt := 1; attempt <= MaxSpawnRetries; attempt++ {
		attempts = attempt
		// Probe ownership before any retry (skip on the very first
		// attempt — the platform claim already happened).
		if attempt > 1 && m.prober != nil {
			owned, probeErr := m.prober(ctx, spec.SessionID)
			if probeErr == nil && !owned {
				return "", fmt.Errorf("%w: session %s", ErrLostOwnership, spec.SessionID)
			}
			// probeErr is non-fatal — keep retrying.
			if probeErr != nil {
				m.logger.Warn("worktree ownership probe error",
					"sessionId", spec.SessionID, "err", probeErr)
			}
		}

		err := m.provisionOnce(ctx, dst, spec)
		if err == nil {
			res := &ProvisionResult{
				Path:           dst,
				Strategy:       spec.Strategy,
				ParentRepoPath: parentRepoPath,
				Attempts:       attempts,
			}
			m.mu.Lock()
			m.sessions[spec.SessionID] = res
			m.mu.Unlock()
			m.logger.Debug("worktree provisioned",
				"sessionId", spec.SessionID, "path", dst,
				"strategy", spec.Strategy.String(), "attempts", attempt)
			return dst, nil
		}
		lastErr = err

		if !isRetriable(err) {
			return "", err
		}
		// Cleanup may proceed only when no durable terminal lease retains the
		// exact destination. Lease-state read failures fail closed.
		if cleanupErr := m.cleanupConflict(ctx, dst, spec); cleanupErr != nil {
			return "", cleanupErr
		}

		if attempt < MaxSpawnRetries {
			m.logger.Warn("worktree provision failed; retrying",
				"sessionId", spec.SessionID, "attempt", attempt,
				"max", MaxSpawnRetries, "err", err)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(m.delay):
			}
		}
	}
	return "", fmt.Errorf("runtime/worktree: provisioning failed after %d attempts: %w",
		attempts, lastErr)
}

// Teardown removes the session's worktree. An active or release-pending
// terminal lease defers teardown and retains the exact leaf. Unknown sessions
// remain idempotent no-ops.
func (m *Manager) Teardown(ctx context.Context, sessionID string) error {
	unlock := m.lockSession(sessionID)
	defer unlock()

	m.mu.Lock()
	res, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	retained, err := m.requestReleasePath(ctx, res.Path)
	if err != nil {
		return fmt.Errorf("runtime/worktree: check terminal lease before teardown: %w", err)
	}
	if retained {
		// The lease store durably recorded ReleaseRequested. The exact leaf stays
		// owned by this session until acknowledgement or expiry drives the same
		// teardown through the release callback.
		return nil
	}
	if err := m.teardownResult(ctx, *res); err != nil {
		return err
	}
	m.mu.Lock()
	if m.sessions[sessionID] == res {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	return nil
}

// AcquireTerminalLease persists a provider-neutral lease for a workarea already
// owned by the session. WorkareaID is an opaque stable identity derived from the
// exact local path; the path itself remains only in durable local state.
func (m *Manager) AcquireTerminalLease(ctx context.Context, spec workarea.AcquireSpec) (*workarea.TerminalLease, error) {
	unlock := m.lockSession(spec.SessionID)
	defer unlock()

	m.mu.Lock()
	res := m.sessions[spec.SessionID]
	m.mu.Unlock()
	if res == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownSession, spec.SessionID)
	}
	workareaID, err := workarea.IDForPath(res.Path)
	if err != nil {
		return nil, err
	}
	if spec.WorkareaPath == "" {
		spec.WorkareaPath = res.Path
	}
	if spec.WorkareaID == "" {
		spec.WorkareaID = workareaID
	}
	if spec.WorkareaPath != res.Path || spec.WorkareaID != workareaID {
		return nil, fmt.Errorf("%w: terminal lease must retain the manager-owned workarea", workarea.ErrLeaseConflict)
	}
	metadata := make(map[string]string, len(spec.ReleaseMetadata)+2)
	for key, value := range spec.ReleaseMetadata {
		metadata[key] = value
	}
	metadata["strategy"] = strconv.Itoa(int(res.Strategy))
	if res.ParentRepoPath != "" {
		metadata["parentRepoPath"] = res.ParentRepoPath
	}
	spec.ReleaseMetadata = metadata
	return m.leases.Acquire(ctx, spec)
}

// RenewTerminalLease extends a lease under the same bounded identity.
func (m *Manager) RenewTerminalLease(ctx context.Context, renewal workarea.RenewSpec) (*workarea.TerminalLease, error) {
	return m.leases.Renew(ctx, renewal)
}

// TerminalLease returns one durable local lease by its opaque id.
func (m *Manager) TerminalLease(leaseID string) (*workarea.TerminalLease, error) {
	return m.leases.Get(leaseID)
}

// ClaimTerminalLeaseExecution durably binds one invocation/claim pair as the
// exclusive workarea-backed verifier for a lease.
func (m *Manager) ClaimTerminalLeaseExecution(ctx context.Context, claim workarea.ExecutionClaimSpec) (*workarea.TerminalLease, error) {
	return m.leases.ClaimExecution(ctx, claim)
}

// MarkTerminalResultObserved records durable receiver observation after the
// initial terminal status post succeeds.
func (m *Manager) MarkTerminalResultObserved(ctx context.Context, leaseID, sessionID, terminalResultID, workareaID string) (*workarea.TerminalLease, error) {
	return m.leases.MarkTerminalResultObserved(ctx, leaseID, sessionID, terminalResultID, workareaID)
}

// ReplayTerminalResults performs one bounded durable outbox recovery pass.
func (m *Manager) ReplayTerminalResults(ctx context.Context, batch int, attemptTimeout time.Duration, poster workarea.TerminalResultPoster) (int, error) {
	return m.leases.ReplayTerminalResults(ctx, batch, attemptTimeout, poster)
}

// RunTerminalResultReplayer runs durable terminal status recovery until ctx is
// cancelled. Embedders should wire the same receiver used for initial posting.
func (m *Manager) RunTerminalResultReplayer(ctx context.Context, opts workarea.TerminalResultReplayOptions, poster workarea.TerminalResultPoster) {
	m.leases.RunTerminalResultReplayer(ctx, opts, poster)
}

// AcknowledgeTerminalResult consumes explicit semantic acknowledgement and
// invokes the manager's deferred ordinary teardown policy.
func (m *Manager) AcknowledgeTerminalResult(ctx context.Context, ack workarea.TerminalResultAcknowledgement) (*workarea.TerminalLease, error) {
	unlock := m.lockSession(ack.SessionID)
	defer unlock()
	return m.leases.Acknowledge(ctx, ack, m.releaseLeasedWorkareaUnlocked)
}

// ReapExpiredTerminalLeases performs one bounded reaper pass. Failures stay
// release-pending and unavailable for a later pass.
func (m *Manager) ReapExpiredTerminalLeases(ctx context.Context, batch int, attemptTimeout time.Duration) (int, error) {
	return m.leases.ReapExpired(ctx, batch, attemptTimeout, m.releaseLeasedWorkarea)
}

// RunTerminalLeaseReaper runs the bounded durable reaper until ctx is
// cancelled. Embedders should start one for the daemon lifetime.
func (m *Manager) RunTerminalLeaseReaper(ctx context.Context, opts workarea.ReaperOptions) {
	m.leases.RunReaper(ctx, opts, m.releaseLeasedWorkarea)
}

func (m *Manager) releaseLeasedWorkarea(ctx context.Context, lease workarea.TerminalLease) error {
	// The lease store already holds the per-result/workarea lock while invoking
	// this callback. Do not take the manager's session lock here: Acquire/Teardown
	// take session then lease locks, so the reverse order would deadlock an
	// acknowledgement or reaper racing those operations. release-pending durable
	// state keeps Provision/Teardown fail-closed during provider disposition.
	return m.releaseLeasedWorkareaUnlocked(ctx, lease)
}

func (m *Manager) releaseLeasedWorkareaUnlocked(ctx context.Context, lease workarea.TerminalLease) error {
	strategyValue, err := strconv.Atoi(lease.ReleaseMetadata["strategy"])
	if err != nil {
		return fmt.Errorf("runtime/worktree: decode leased release strategy: %w", err)
	}
	res := ProvisionResult{
		Path:           lease.WorkareaPath,
		Strategy:       CloneStrategy(strategyValue),
		ParentRepoPath: lease.ReleaseMetadata["parentRepoPath"],
	}
	if err := m.teardownResult(ctx, res); err != nil {
		return err
	}
	m.mu.Lock()
	if current := m.sessions[lease.SessionID]; current != nil && current.Path == lease.WorkareaPath {
		delete(m.sessions, lease.SessionID)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) teardownResult(ctx context.Context, res ProvisionResult) error {
	if res.Strategy == StrategyWorktreeAdd && res.ParentRepoPath != "" {
		out, err := m.runGit(ctx, "", "-C", res.ParentRepoPath, "worktree", "remove", "--force", res.Path)
		if err != nil {
			removed, verifyErr := m.worktreeAlreadyRemoved(ctx, res.ParentRepoPath, res.Path)
			if verifyErr != nil {
				return fmt.Errorf("runtime/worktree: git worktree remove %q: %w (%s); verify prior removal: %v", res.Path, err, strings.TrimSpace(string(out)), verifyErr)
			}
			if !removed {
				return fmt.Errorf("runtime/worktree: git worktree remove %q: %w (%s)", res.Path, err, strings.TrimSpace(string(out)))
			}
		}
	}
	if err := os.RemoveAll(res.Path); err != nil {
		return fmt.Errorf("runtime/worktree: remove %q: %w", res.Path, err)
	}
	return nil
}

// worktreeAlreadyRemoved closes the release-pending crash window: a repeated
// worktree removal is successful only when the exact leaf is absent from both
// the filesystem and the parent repository's authoritative worktree registry.
func (m *Manager) worktreeAlreadyRemoved(ctx context.Context, parentRepoPath, path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat worktree path: %w", err)
	}
	out, err := m.runGit(ctx, "", "-C", parentRepoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git worktree list: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	wanted, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve worktree path: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		const prefix = "worktree "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		listed, resolveErr := filepath.Abs(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		if resolveErr != nil {
			return false, fmt.Errorf("resolve registered worktree path: %w", resolveErr)
		}
		if filepath.Clean(listed) == filepath.Clean(wanted) {
			return false, nil
		}
	}
	return true, nil
}

// Path returns the worktree path for a previously-provisioned session.
// Returns ErrUnknownSession when the session id is not tracked.
func (m *Manager) Path(sessionID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res, ok := m.sessions[sessionID]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownSession, sessionID)
	}
	return res.Path, nil
}

// provisionOnce runs one git invocation per spec.Strategy.
func (m *Manager) provisionOnce(ctx context.Context, dst string, spec ProvisionSpec) error {
	if _, err := os.Stat(dst); err == nil {
		// Path exists. For StrategyWorktreeAdd this is a conflict;
		// for StrategyClone too. Either way, surface as conflict.
		return fmt.Errorf("destination already exists: %s", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}

	switch spec.Strategy {
	case StrategyClone:
		if spec.RepoURL == "" {
			return errors.New("RepoURL required for StrategyClone")
		}
		// When the credential-hardening seam is engaged, clone the
		// userinfo-stripped URL and rely on the injected http.extraHeader for
		// auth, so the token never lands in the persisted .git/config remote.
		// When inert, RepoURL is left untouched (current behaviour).
		cloneURL := spec.RepoURL
		if m.gitAuth != nil {
			if clean, stripped := gitexec.CleanURL(spec.RepoURL); stripped {
				cloneURL = clean
			}
		}
		args := []string{"clone"}
		if spec.Branch != "" {
			args = append(args, "--branch", spec.Branch)
		}
		args = append(args, cloneURL, dst)
		out, err := m.runGit(ctx, spec.RepoURL, args...)
		if err != nil {
			return fmt.Errorf("git clone: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	case StrategyWorktreeAdd:
		parent := spec.ParentRepoPath
		if parent == "" {
			return errors.New("ParentRepoPath required for StrategyWorktreeAdd")
		}
		args := []string{"-C", parent, "worktree", "add"}
		if spec.Branch != "" {
			args = append(args, "-B", spec.Branch)
		}
		args = append(args, dst)
		if spec.Branch != "" {
			// `git worktree add -B name dst origin/name` checks out
			// the remote branch when one exists; locally created
			// branches are also fine because -B resets the branch.
			args = append(args, "origin/"+spec.Branch)
		}
		out, err := m.runGit(ctx, spec.RepoURL, args...)
		if err != nil {
			return fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("unknown strategy: %d", spec.Strategy)
	}
}

// runGit runs a git invocation, applying the credential-hardening seam when a
// GitAuth callback is configured. repoURL is the remote the operation targets;
// it is passed to the GitAuth resolver and is "" for purely-local operations
// (e.g. `worktree remove`), in which case GitAuth still runs and may suppress
// the credential helper.
//
// When m.gitAuth is nil this routes straight through the env-free CommandRunner
// — byte-identical to the pre-seam path.
func (m *Manager) lockSession(sessionID string) func() {
	m.mu.Lock()
	lock := m.sessionLocks[sessionID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.sessionLocks[sessionID] = lock
	}
	m.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (m *Manager) runGit(ctx context.Context, repoURL string, args ...string) ([]byte, error) {
	if m.gitAuth == nil {
		return m.runner(ctx, "git", args...)
	}
	header, suppress, err := m.gitAuth(ctx, repoURL)
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: resolve git auth: %w", err)
	}
	env := gitexec.HardenedEnv(nil, suppress, header)
	return m.envRunner(ctx, env, "git", args...)
}

func (m *Manager) retainedPath(path string) (bool, error) {
	workareaID, err := workarea.IDForPath(path)
	if err != nil {
		return false, err
	}
	retained, err := m.leases.Retained(workareaID)
	if err != nil || retained {
		return retained, err
	}
	// Compatibility with lease records created by the unreleased path-id
	// candidate before opaque workarea identities were introduced.
	return m.leases.Retained(path)
}

func (m *Manager) requestReleasePath(ctx context.Context, path string) (bool, error) {
	workareaID, err := workarea.IDForPath(path)
	if err != nil {
		return false, err
	}
	retained, err := m.leases.RequestRelease(ctx, workareaID)
	if err != nil || retained {
		return retained, err
	}
	// Compatibility with lease records created by the unreleased path-id
	// candidate before opaque workarea identities were introduced.
	return m.leases.RequestRelease(ctx, path)
}

// cleanupConflict tries to remove a stale worktree entry left by a prior failed
// Provision. It fails closed when lease state is unreadable and never removes a
// terminal-leased exact workarea; ordinary stale cleanup remains best-effort.
func (m *Manager) cleanupConflict(ctx context.Context, dst string, spec ProvisionSpec) error {
	retained, err := m.retainedPath(dst)
	if err != nil {
		return fmt.Errorf("runtime/worktree: check terminal lease before conflict cleanup: %w", err)
	}
	if retained {
		return fmt.Errorf("%w: %s", workarea.ErrWorkareaLeased, dst)
	}
	if spec.Strategy == StrategyWorktreeAdd && spec.ParentRepoPath != "" {
		_, _ = m.runGit(ctx, "", "-C", spec.ParentRepoPath, "worktree", "remove", "--force", dst)
	}
	if _, err := os.Stat(dst); err == nil {
		_ = os.RemoveAll(dst)
	}
	return nil
}

// isRetriable returns true for errors that the legacy TS retry loop
// considers "branch in use" / "agent already running". The pattern
// list mirrors worker-runner.ts:929-933 verbatim.
func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, frag := range []string{
		"already checked out",
		"is already checked out at",
		"already exists",
		"Agent already running",
		"Agent is still running",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}
