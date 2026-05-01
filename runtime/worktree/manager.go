package worktree

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
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

// defaultRunner is the production CommandRunner; tests inject a stub.
func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	// nolint:gosec // G204: name is a hard-coded "git" binary; args are
	// constructed from validated ProvisionSpec fields at this layer.
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// ProvisionResult is the per-session bookkeeping the manager records.
// Returned alongside the worktree path for callers that need both.
type ProvisionResult struct {
	// Path is the absolute worktree path on disk.
	Path string
	// Strategy is the strategy that succeeded.
	Strategy CloneStrategy
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
	delay     time.Duration

	mu       sync.Mutex
	sessions map[string]*ProvisionResult
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
	// CommandRunner overrides the default exec.CommandContext runner.
	CommandRunner CommandRunner
	// RetryDelay overrides SpawnRetryDelay. Useful for tests.
	RetryDelay time.Duration
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
	return &Manager{
		parentDir: abs,
		logger:    logger,
		prober:    opts.OwnershipProber,
		runner:    runner,
		delay:     delay,
		sessions:  make(map[string]*ProvisionResult),
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

	leaf := spec.LeafName
	if leaf == "" {
		leaf = spec.SessionID
	}
	dst := filepath.Join(m.parentDir, leaf)

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
			res := &ProvisionResult{Path: dst, Strategy: spec.Strategy, Attempts: attempts}
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
		// Best-effort cleanup before retry — mirrors legacy
		// tryCleanupConflictingWorktree.
		_ = m.cleanupConflict(ctx, dst, spec)

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

// Teardown removes the session's worktree. Idempotent when the
// session is unknown — returns nil.
func (m *Manager) Teardown(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	res, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if res.Strategy == StrategyWorktreeAdd {
		// Best-effort: ignore errors so a partial worktree from a
		// prior crash does not block cleanup.
		_, _ = m.runner(ctx, "git", "-C", m.parentDir, "worktree", "remove", "--force", res.Path)
	}
	if err := os.RemoveAll(res.Path); err != nil {
		return fmt.Errorf("runtime/worktree: remove %q: %w", res.Path, err)
	}
	return nil
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
		args := []string{"clone"}
		if spec.Branch != "" {
			args = append(args, "--branch", spec.Branch)
		}
		args = append(args, spec.RepoURL, dst)
		out, err := m.runner(ctx, "git", args...)
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
		out, err := m.runner(ctx, "git", args...)
		if err != nil {
			return fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("unknown strategy: %d", spec.Strategy)
	}
}

// cleanupConflict tries to remove a stale worktree entry left by a
// prior failed Provision. Best-effort; never returns an error.
func (m *Manager) cleanupConflict(ctx context.Context, dst string, spec ProvisionSpec) error {
	if spec.Strategy == StrategyWorktreeAdd && spec.ParentRepoPath != "" {
		_, _ = m.runner(ctx, "git", "-C", spec.ParentRepoPath, "worktree", "remove", "--force", dst)
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
