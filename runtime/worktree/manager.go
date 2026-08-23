package worktree

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

const (
	// ModeExclusive creates and owns one session root.
	ModeExclusive = "exclusive"
	// ModeShared joins a durable parent root without acquiring ownership.
	ModeShared = "shared"
)

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

	// ErrWorkareaRootOccupied refuses an undeclared or retained root rather
	// than deleting filesystem state the current acquisition does not own.
	ErrWorkareaRootOccupied = errors.New("runtime/worktree: workarea root already exists")
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
// invocation that touches a remote applies gitexec.HardenedEnv, and a clone of
// a URL carrying embedded userinfo clones the userinfo-stripped URL so the
// token never persists in .git/config (auth flows through the injected
// http.extraHeader instead).
//
// The returned header is scoped to the repoURL it was resolved for —
// `http.<repoURL>.extraHeader` — so it is offered ONLY to that remote. A
// resolver that ignores repoURL and returns one blanket token still gets that
// scoping; it does not get a credential that follows the process into every
// other git remote it or its children touch. Correspondingly, a header
// returned for a repoURL that is empty or not http(s) is dropped (and logged),
// because there is no request it could correctly authenticate.
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
	// Path is the selected repository worktree path and harness CWD.
	Path string
	// WorkareaRoot is the session-owned lifecycle root. It equals Path only for
	// a retained legacy flat workarea.
	WorkareaRoot string
	// WorkareaID is generated for this acquisition and is never reused merely
	// because a later acquisition occupies the same filesystem path.
	WorkareaID string
	// AcquisitionID is the durable nested-generation cleanup authority.
	AcquisitionID string
	// Mode is exclusive for the owner and shared for a participant.
	Mode string
	// OwnerSessionID names the root lifecycle owner.
	OwnerSessionID string
	// CacheSeedID is correlation only; it never becomes session identity.
	CacheSeedID string
	// Repositories maps normalized declared names to their repository paths.
	// Legacy flat workareas contain only the selected repository when known.
	Repositories map[string]string
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
	parentDir        string
	logger           *slog.Logger
	prober           OwnershipProber
	runner           CommandRunner
	envRunner        EnvCommandRunner
	gitAuth          GitAuth
	delay            time.Duration
	leases           *workarea.LeaseStore
	acquisitions     *workarea.AcquisitionStore
	seeds            *workarea.SeedStore
	provisionHook    ProvisionHook
	archiveRoot      ArchiveRootFunc
	restoreSessionID string
	authorityMu      sync.Mutex
	now              func() time.Time

	mu           sync.Mutex
	sessions     map[string]*ProvisionResult
	sessionLocks map[string]*sync.Mutex
}

// ProvisionHook is a test/fault-injection seam at durable crash boundaries.
type ProvisionHook func(stage string, acquisition workarea.AcquisitionRecord) error

// ArchiveRootFunc captures one complete session-owned root before disposal.
type ArchiveRootFunc func(context.Context, ArchiveRootSpec) error

// ArchiveRootSpec is the provider-neutral whole-root archive input.
type ArchiveRootSpec struct {
	AcquisitionID string
	WorkareaID    string
	SessionID     string
	WorkareaRoot  string
	SelectedPath  string
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
	// AcquisitionStore overrides the durable nested-generation authority.
	AcquisitionStore *workarea.AcquisitionStore
	// SeedStore overrides the reusable cache-seed authority.
	SeedStore *workarea.SeedStore
	// ProvisionHook injects deterministic crash boundaries in tests.
	ProvisionHook ProvisionHook
	// ArchiveRoot must succeed before an archive disposition may release a root.
	ArchiveRoot ArchiveRootFunc
	// RestoreSessionID limits restart bookkeeping to one owner or participant.
	// Empty is the daemon-wide recovery view.
	RestoreSessionID string
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
	acquisitions := opts.AcquisitionStore
	if acquisitions == nil {
		acquisitions, _, err = workarea.OpenExistingAcquisitionStore(abs, opts.Now)
		if err != nil {
			return nil, fmt.Errorf("runtime/worktree: acquisition store: %w", err)
		}
	}
	seeds := opts.SeedStore
	if seeds == nil {
		seeds, _, err = workarea.OpenExistingSeedStore(abs, opts.Now)
		if err != nil {
			return nil, fmt.Errorf("runtime/worktree: cache seed store: %w", err)
		}
	}
	manager := &Manager{
		parentDir:        abs,
		logger:           logger,
		prober:           opts.OwnershipProber,
		runner:           runner,
		envRunner:        envRunner,
		gitAuth:          opts.GitAuth,
		delay:            delay,
		leases:           leases,
		acquisitions:     acquisitions,
		seeds:            seeds,
		provisionHook:    opts.ProvisionHook,
		archiveRoot:      opts.ArchiveRoot,
		restoreSessionID: opts.RestoreSessionID,
		now:              opts.Now,
		sessions:         make(map[string]*ProvisionResult),
		sessionLocks:     make(map[string]*sync.Mutex),
	}
	if acquisitions != nil {
		if err := manager.restoreReadyAcquisitions(); err != nil {
			return nil, err
		}
	}
	return manager, nil
}

func (m *Manager) restoreReadyAcquisitions() error {
	records, err := m.acquisitions.ReadyRecords()
	if err != nil {
		return fmt.Errorf("runtime/worktree: restore acquisitions: %w", err)
	}
	for _, record := range records {
		declaration, err := workarea.ReadDeclaration(workarea.RootPath(record.FinalRoot))
		if err != nil {
			return err
		}
		paths := make(map[string]string, len(declaration.Repositories))
		selectedPath := ""
		for _, repository := range declaration.Repositories {
			path := filepath.Join(record.FinalRoot, repository.Leaf)
			paths[repository.Name] = path
			if repository.Name == declaration.SelectedRepository {
				selectedPath = path
			}
		}
		owner := &ProvisionResult{
			Path: selectedPath, WorkareaRoot: record.FinalRoot, WorkareaID: record.WorkareaID,
			AcquisitionID: record.AcquisitionID, Mode: ModeExclusive, OwnerSessionID: record.SessionID,
			CacheSeedID: record.CacheSeedID, Repositories: paths, Strategy: StrategyClone,
		}
		if !record.OwnerReleased && (m.restoreSessionID == "" || m.restoreSessionID == record.SessionID) {
			m.sessions[record.SessionID] = owner
		}
		for _, participant := range record.Participants {
			if m.restoreSessionID != "" && m.restoreSessionID != participant.SessionID {
				continue
			}
			participantPath := paths[participant.SelectedRepository]
			if participantPath == "" {
				return fmt.Errorf("runtime/worktree: restore participant %q selected an undeclared repository", participant.SessionID)
			}
			m.sessions[participant.SessionID] = &ProvisionResult{
				Path: participantPath, WorkareaRoot: record.FinalRoot, WorkareaID: record.WorkareaID,
				AcquisitionID: record.AcquisitionID, Mode: ModeShared, OwnerSessionID: record.SessionID,
				CacheSeedID: record.CacheSeedID, Repositories: paths, Strategy: StrategyClone,
			}
		}
	}
	return nil
}

// ParentDir returns the absolute path of the manager's parent
// directory.
func (m *Manager) ParentDir() string {
	return m.parentDir
}

// CacheSeedRecord returns the separate seed identity and physical charge.
func (m *Manager) CacheSeedRecord(seedID string) (workarea.SeedRecord, error) {
	seeds, err := m.seedStore()
	if err != nil {
		return workarea.SeedRecord{}, err
	}
	return seeds.Record(seedID)
}

// AcquisitionRecord returns the durable generation identities for one workarea.
func (m *Manager) AcquisitionRecord(workareaID string) (workarea.AcquisitionRecord, error) {
	acquisitions, err := m.acquisitionStore()
	if err != nil {
		return workarea.AcquisitionRecord{}, err
	}
	return acquisitions.RecordForWorkareaID(workareaID)
}

func (m *Manager) acquisitionStore() (*workarea.AcquisitionStore, error) {
	m.authorityMu.Lock()
	defer m.authorityMu.Unlock()
	if m.acquisitions != nil {
		return m.acquisitions, nil
	}
	store, err := workarea.NewAcquisitionStore(m.parentDir, m.now)
	if err != nil {
		return nil, err
	}
	m.acquisitions = store
	return store, nil
}

func (m *Manager) seedStore() (*workarea.SeedStore, error) {
	m.authorityMu.Lock()
	defer m.authorityMu.Unlock()
	if m.seeds != nil {
		return m.seeds, nil
	}
	store, err := workarea.NewSeedStore(m.parentDir, m.now)
	if err != nil {
		return nil, err
	}
	m.seeds = store
	return store, nil
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
	// SourceRef is the requested legacy singular source ref. It is compared
	// byte-for-byte with the declaration's primary source when a versioned
	// declaration is present.
	SourceRef string
	// RepositoryDeclaration activates session-root-v1 only when the exact
	// bound executor capabilities below positively attest it. Nil preserves
	// the shipped flat layout byte-for-byte.
	RepositoryDeclaration *workarea.RepositoryDeclarationV1
	// ExecutorCapabilities is the exact claim-time attestation for this
	// provision. Absence is []/none and cannot activate session-root-v1.
	ExecutorCapabilities workarea.ExecutorWorkareaCapabilities
	// SparsePaths are relative repository paths materialized by git sparse
	// checkout. Empty keeps the existing full clone.
	SparsePaths []string
	// Mode is exclusive (default) or shared. Shared requires ParentWorkareaID.
	Mode string
	// ParentWorkareaID durably joins the exact owner root in shared mode.
	ParentWorkareaID string
	// RepositoryFilter lets a shared participant select another declared leaf.
	RepositoryFilter *workarea.RepositoryFilter
	// CacheSeedID records reusable seed provenance without reusing session identity.
	CacheSeedID string
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
	mode := spec.Mode
	if mode == "" {
		mode = ModeExclusive
	}
	if mode == ModeShared {
		return m.provisionShared(ctx, spec)
	}
	if mode != ModeExclusive {
		return "", fmt.Errorf("runtime/worktree: unsupported provision mode %q", mode)
	}
	if spec.ParentWorkareaID != "" || spec.RepositoryFilter != nil {
		return "", errors.New("runtime/worktree: parent workarea selection is valid only in shared mode")
	}
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
	layout, declaration, err := m.provisionLayout(leaf, spec)
	if err != nil {
		return "", err
	}
	root := layout.Root.String()
	dst := layout.Repository.String()
	if filepath.Clean(root) == filepath.Clean(m.leases.Dir()) {
		return "", errors.New("runtime/worktree: destination conflicts with terminal lease state directory")
	}
	retained, err := m.retainedPath(root)
	if err != nil {
		return "", fmt.Errorf("runtime/worktree: check terminal lease before provision: %w", err)
	}
	if retained {
		return "", fmt.Errorf("%w: %s", workarea.ErrWorkareaLeased, root)
	}
	workareaID, err := workarea.NewWorkareaID()
	if err != nil {
		return "", err
	}

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

		repositories, acquisition, err := m.provisionLayoutOnce(ctx, layout, declaration, workareaID, spec)
		if err == nil {
			res := &ProvisionResult{
				Path: dst, WorkareaRoot: root, WorkareaID: workareaID,
				Mode: ModeExclusive, OwnerSessionID: spec.SessionID, CacheSeedID: spec.CacheSeedID,
				Repositories: repositories, Strategy: spec.Strategy,
				ParentRepoPath: parentRepoPath, Attempts: attempts,
			}
			if acquisition.AcquisitionID != "" {
				res.AcquisitionID = acquisition.AcquisitionID
				res.WorkareaRoot = acquisition.FinalRoot
				res.WorkareaID = acquisition.WorkareaID
				res.CacheSeedID = acquisition.CacheSeedID
				res.Path = filepath.Join(acquisition.FinalRoot, acquisition.SelectedLeaf)
			}
			m.mu.Lock()
			m.sessions[spec.SessionID] = res
			m.mu.Unlock()
			m.logger.Debug("worktree provisioned",
				"sessionId", spec.SessionID, "path", res.Path, "workareaRoot", res.WorkareaRoot,
				"nested", layout.IsNested(),
				"strategy", spec.Strategy.String(), "attempts", attempt)
			return res.Path, nil
		}
		if errors.Is(err, ErrWorkareaRootOccupied) {
			return "", err
		}
		if !isRetriable(err) {
			return "", err
		}
		// Cleanup may proceed only when no durable terminal lease retains the
		// exact destination. Lease-state read failures fail closed.
		if !layout.IsNested() {
			if cleanupErr := m.cleanupConflict(ctx, layout, spec); cleanupErr != nil {
				return "", cleanupErr
			}
		}
		if attempt == MaxSpawnRetries {
			return "", fmt.Errorf("runtime/worktree: provisioning failed after %d attempts: %w", attempt, err)
		}

		m.logger.Warn("worktree provision failed; retrying",
			"sessionId", spec.SessionID, "attempt", attempt,
			"max", MaxSpawnRetries, "err", err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(m.delay):
		}
	}
	return "", fmt.Errorf("runtime/worktree: provisioning failed after %d attempts", attempts)
}

func (m *Manager) provisionShared(_ context.Context, spec ProvisionSpec) (string, error) {
	if spec.ParentWorkareaID == "" {
		return "", errors.New("runtime/worktree: shared mode requires ParentWorkareaID")
	}
	if spec.RepositoryDeclaration == nil {
		return "", errors.New("runtime/worktree: shared mode requires the versioned parent declaration")
	}
	normalized, err := spec.RepositoryDeclaration.Normalize()
	if err != nil {
		return "", err
	}
	if err := normalized.ValidatePrimarySource(workarea.RepositorySource{Repository: spec.RepoURL, Ref: spec.SourceRef}); err != nil {
		return "", err
	}
	if err := spec.ExecutorCapabilities.ValidateFor(normalized); err != nil {
		return "", err
	}
	acquisitions, err := m.acquisitionStore()
	if err != nil {
		return "", err
	}
	record, err := acquisitions.RecordForWorkareaID(spec.ParentWorkareaID)
	if err != nil {
		return "", err
	}
	if record.State != workarea.AcquisitionReady || record.OwnerReleased {
		return "", errors.New("runtime/worktree: shared parent is not available")
	}
	retained, err := m.retainedPath(record.FinalRoot)
	if err != nil {
		return "", fmt.Errorf("runtime/worktree: check shared parent lease: %w", err)
	}
	if retained {
		return "", fmt.Errorf("%w: %s", workarea.ErrWorkareaLeased, record.FinalRoot)
	}
	declaration, err := workarea.ReadDeclaration(workarea.RootPath(record.FinalRoot))
	if err != nil {
		return "", err
	}
	if err := declarationMatchesNormalized(declaration, normalized); err != nil {
		return "", err
	}
	selected, err := declaration.ResolveOne(spec.RepositoryFilter)
	if err != nil {
		return "", err
	}
	record, err = acquisitions.JoinShared(spec.ParentWorkareaID, spec.SessionID, selected.Name)
	if err != nil {
		return "", err
	}
	paths := declarationRepositoryPaths(record.FinalRoot, declaration)
	selectedPath := paths[selected.Name]
	if selectedPath == "" {
		return "", errors.New("runtime/worktree: shared selection did not resolve inside parent root")
	}
	m.mu.Lock()
	m.sessions[spec.SessionID] = &ProvisionResult{
		Path: selectedPath, WorkareaRoot: record.FinalRoot, WorkareaID: record.WorkareaID,
		AcquisitionID: record.AcquisitionID, Mode: ModeShared, OwnerSessionID: record.SessionID,
		CacheSeedID: record.CacheSeedID, Repositories: paths, Strategy: StrategyClone, Attempts: 1,
	}
	m.mu.Unlock()
	m.logger.Debug("shared workarea participant joined", "sessionId", spec.SessionID, "ownerSessionId", record.SessionID, "workareaRoot", record.FinalRoot)
	return selectedPath, nil
}

func declarationMatchesNormalized(record workarea.DeclarationRecord, normalized workarea.NormalizedDeclaration) error {
	if record.Protocol != normalized.Protocol || len(record.Repositories) != len(normalized.Repositories) {
		return errors.New("runtime/worktree: shared parent declaration does not match the bound carrier")
	}
	byName := make(map[string]workarea.NormalizedRepository, len(normalized.Repositories))
	for _, repository := range normalized.Repositories {
		byName[repository.Name] = repository
	}
	for _, repository := range record.Repositories {
		bound, ok := byName[repository.Name]
		if !ok || bound.Leaf != repository.Leaf || bound.Role != repository.Role || bound.Authority != repository.Authority {
			return errors.New("runtime/worktree: shared parent declaration does not match the bound carrier")
		}
	}
	return nil
}

func declarationRepositoryPaths(root string, declaration workarea.DeclarationRecord) map[string]string {
	paths := make(map[string]string, len(declaration.Repositories))
	for _, repository := range declaration.Repositories {
		paths[repository.Name] = filepath.Join(root, repository.Leaf)
	}
	return paths
}

// Teardown removes the session-owned root and every declared leaf. An active or
// release-pending terminal lease retains that exact root. Unknown sessions are
// idempotent no-ops.
func (m *Manager) Teardown(ctx context.Context, sessionID string) error {
	unlock := m.lockSession(sessionID)
	defer unlock()

	m.mu.Lock()
	res, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	retained, err := m.requestReleasePath(ctx, res.workareaRootOrPath())
	if err != nil {
		return fmt.Errorf("runtime/worktree: check terminal lease before teardown: %w", err)
	}
	if retained {
		// The lease store durably recorded ReleaseRequested. The exact leaf stays
		// owned by this session until acknowledgement or expiry drives the same
		// teardown through the release callback.
		return nil
	}
	if res.AcquisitionID != "" {
		return m.releaseNestedSession(*res, sessionID)
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

func (m *Manager) releaseNestedSession(res ProvisionResult, sessionID string) error {
	acquisitions, err := m.acquisitionStore()
	if err != nil {
		return err
	}
	releaseRoot := false
	if res.Mode == ModeShared {
		releaseRoot, err = acquisitions.LeaveShared(res.WorkareaID, sessionID)
	} else {
		releaseRoot, err = acquisitions.RequestOwnerRelease(res.WorkareaID)
	}
	if err != nil {
		return err
	}
	if releaseRoot {
		if err := acquisitions.RemovePublishedRoot(res.AcquisitionID); err != nil {
			return err
		}
		m.forgetSessionsAtRoot(res.WorkareaRoot)
		return nil
	}
	m.mu.Lock()
	if current := m.sessions[sessionID]; current != nil && current.AcquisitionID == res.AcquisitionID {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	return nil
}

// AcquireTerminalLease persists a provider-neutral lease for a workarea already
// owned by the session. WorkareaID is an opaque acquisition-scoped identity; the
// exact local path remains only in durable local state.
func (m *Manager) AcquireTerminalLease(ctx context.Context, spec workarea.AcquireSpec) (*workarea.TerminalLease, error) {
	unlock := m.lockSession(spec.SessionID)
	defer unlock()

	m.mu.Lock()
	res := m.sessions[spec.SessionID]
	m.mu.Unlock()
	if res == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownSession, spec.SessionID)
	}
	if spec.WorkareaPath == "" {
		spec.WorkareaPath = res.workareaRootOrPath()
	}
	if spec.WorkareaID == "" {
		spec.WorkareaID = res.WorkareaID
	}
	if spec.WorkareaPath != res.workareaRootOrPath() || spec.WorkareaID != res.WorkareaID {
		return nil, fmt.Errorf("%w: terminal lease must retain the manager-owned workarea", workarea.ErrLeaseConflict)
	}
	if res.AcquisitionID != "" {
		acquisitions, err := m.acquisitionStore()
		if err != nil {
			return nil, err
		}
		record, err := acquisitions.RecordForWorkareaID(res.WorkareaID)
		if err != nil {
			return nil, err
		}
		if res.Mode != ModeExclusive || record.OwnerReleased || len(record.Participants) != 0 {
			return nil, fmt.Errorf("%w: a shared root cannot enter a terminal lease", workarea.ErrLeaseConflict)
		}
	}
	metadata := make(map[string]string, len(spec.ReleaseMetadata)+3)
	for key, value := range spec.ReleaseMetadata {
		metadata[key] = value
	}
	metadata["strategy"] = strconv.Itoa(int(res.Strategy))
	if res.AcquisitionID != "" {
		metadata["acquisitionID"] = res.AcquisitionID
		metadata["ownerSessionID"] = res.OwnerSessionID
		metadata["cacheSeedID"] = res.CacheSeedID
	}
	if res.Path != res.workareaRootOrPath() {
		metadata["repositoryWorktreePath"] = res.Path
	}
	if spec.ReleaseDisposition == "" {
		spec.ReleaseDisposition = "destroy"
	}
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
// exclusive workarea-backed verifier for a lease and returns the committed
// transaction clock sample as claimNowMs operation metadata.
func (m *Manager) ClaimTerminalLeaseExecution(ctx context.Context, claim workarea.ExecutionClaimSpec) (*workarea.ExecutionClaimResult, error) {
	return m.leases.ClaimExecution(ctx, claim)
}

// SaveTerminalStatus persists the immutable complete terminal-status body using
// a compare-and-set against the authoritative lease expiry.
func (m *Manager) SaveTerminalStatus(ctx context.Context, spec workarea.TerminalStatusSaveSpec) (*workarea.TerminalLease, error) {
	return m.leases.SaveTerminalStatus(ctx, spec)
}

// RegisterTerminalReceiver implements the documented terminal-workarea contract.
func (m *Manager) RegisterTerminalReceiver(receiverKey, endpoint string) error {
	return m.leases.RegisterReceiver(receiverKey, endpoint)
}

// TerminalStatusHTTPSender implements the documented terminal-workarea contract.
func (m *Manager) TerminalStatusHTTPSender(client *http.Client, auth workarea.ReceiverAuthorizationResolver) workarea.TerminalStatusSender {
	return m.leases.TerminalStatusHTTPSender(client, auth)
}

// MarkTerminalStatusDelivered records transport acceptance without granting
// acknowledgement-path release authority.
func (m *Manager) MarkTerminalStatusDelivered(ctx context.Context, leaseID, sessionID, terminalResultID, workareaID string) (*workarea.TerminalLease, error) {
	return m.leases.MarkTerminalStatusDelivered(ctx, leaseID, sessionID, terminalResultID, workareaID)
}

// ReplayTerminalResults performs one bounded durable outbox recovery pass.
func (m *Manager) ReplayTerminalResults(ctx context.Context, batch int, attemptTimeout time.Duration, sender workarea.TerminalStatusSender) (int, error) {
	return m.leases.ReplayTerminalResults(ctx, batch, attemptTimeout, sender)
}

// RunTerminalResultReplayer runs durable terminal status recovery until ctx is
// cancelled. The sender must resolve the stored receiver key on every attempt.
func (m *Manager) RunTerminalResultReplayer(ctx context.Context, opts workarea.TerminalResultReplayOptions, sender workarea.TerminalStatusSender) {
	m.leases.RunTerminalResultReplayer(ctx, opts, sender)
}

// AcknowledgeTerminalResult atomically stores the exact acknowledgement outcome
// and active -> release-pending transition. Provider disposition remains a
// separate at-least-once reaper operation.
func (m *Manager) AcknowledgeTerminalResult(ctx context.Context, ack workarea.TerminalResultAcknowledgement) (*workarea.TerminalAcknowledgementOutcome, error) {
	unlock := m.lockSession(ack.SessionID)
	defer unlock()
	return m.leases.Acknowledge(ctx, ack)
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

// CleanupTerminalQuarantines destroys quarantined leaves only; it never applies
// an ordinary return-to-pool or archive disposition.
func (m *Manager) CleanupTerminalQuarantines(ctx context.Context, opts workarea.SchedulerOptions) (int, error) {
	return m.leases.CleanupQuarantines(ctx, opts, func(_ context.Context, item workarea.TerminalWorkareaQuarantine) error {
		declaration, declarationErr := workarea.ReadDeclaration(workarea.RootPath(item.WorkareaPath))
		if declarationErr == nil {
			if declaration.AcquisitionID == "" {
				return errors.New("runtime/worktree: declared quarantine root has no acquisition cleanup authority")
			}
			acquisitions, err := m.acquisitionStore()
			if err != nil {
				return err
			}
			return acquisitions.RemovePublishedRoot(declaration.AcquisitionID)
		}
		if _, metadataErr := os.Lstat(filepath.Join(item.WorkareaPath, workarea.DeclarationDirName)); metadataErr == nil || !errors.Is(metadataErr, os.ErrNotExist) {
			return fmt.Errorf("runtime/worktree: quarantine root declaration is unsafe; deletion refused: %w", declarationErr)
		}
		return os.RemoveAll(item.WorkareaPath)
	})
}

func (m *Manager) releaseLeasedWorkarea(ctx context.Context, lease workarea.TerminalLease) error {
	// The lease store persists release-pending before invoking this callback and
	// does not hold its per-result/workarea locks during provider disposition.
	// Avoid the manager's session lock so independent teardown can proceed while
	// durable lease state keeps this exact path unavailable.
	return m.releaseLeasedWorkareaUnlocked(ctx, lease)
}

func (m *Manager) releaseLeasedWorkareaUnlocked(ctx context.Context, lease workarea.TerminalLease) error {
	repositoryPath := lease.ReleaseMetadata["repositoryWorktreePath"]
	if repositoryPath == "" {
		repositoryPath = lease.WorkareaPath
	}
	res := ProvisionResult{
		Path: repositoryPath, WorkareaRoot: lease.WorkareaPath, WorkareaID: lease.WorkareaID,
		AcquisitionID: lease.ReleaseMetadata["acquisitionID"], OwnerSessionID: lease.ReleaseMetadata["ownerSessionID"],
		CacheSeedID: lease.ReleaseMetadata["cacheSeedID"], Mode: ModeExclusive,
	}
	if lease.ReleaseDisposition == "archive" {
		if m.archiveRoot == nil {
			return errors.New("runtime/worktree: archive disposition has no whole-root archive provider")
		}
		if err := m.archiveRoot(ctx, ArchiveRootSpec{
			AcquisitionID: res.AcquisitionID, WorkareaID: lease.WorkareaID, SessionID: lease.SessionID,
			WorkareaRoot: lease.WorkareaPath, SelectedPath: repositoryPath,
		}); err != nil {
			return fmt.Errorf("runtime/worktree: archive whole workarea root: %w", err)
		}
	}
	if lease.ReleaseDisposition != "destroy" && lease.ReleaseDisposition != "archive" && lease.ReleaseDisposition != "return-to-pool" {
		return fmt.Errorf("runtime/worktree: unsupported leased release disposition %q", lease.ReleaseDisposition)
	}
	strategyValue, err := strconv.Atoi(lease.ReleaseMetadata["strategy"])
	if err != nil {
		return fmt.Errorf("runtime/worktree: decode leased release strategy: %w", err)
	}
	res.Strategy = CloneStrategy(strategyValue)
	res.ParentRepoPath = lease.ReleaseMetadata["parentRepoPath"]
	if err := m.teardownResult(ctx, res); err != nil {
		return err
	}
	m.forgetSessionAtRoot(lease.SessionID, lease.WorkareaPath)
	return nil
}

func (m *Manager) forgetSessionAtRoot(sessionID, root string) {
	m.mu.Lock()
	if current := m.sessions[sessionID]; current != nil && current.workareaRootOrPath() == root {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
}

func (m *Manager) forgetSessionsAtRoot(root string) {
	m.mu.Lock()
	for sessionID, current := range m.sessions {
		if current != nil && current.workareaRootOrPath() == root {
			delete(m.sessions, sessionID)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) teardownResult(ctx context.Context, res ProvisionResult) error {
	if res.AcquisitionID != "" {
		acquisitions, err := m.acquisitionStore()
		if err != nil {
			return err
		}
		return acquisitions.RemovePublishedRoot(res.AcquisitionID)
	}
	if filepath.Clean(res.workareaRootOrPath()) != filepath.Clean(res.Path) {
		return errors.New("runtime/worktree: nested root has no acquisition cleanup authority")
	}
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
	root := res.workareaRootOrPath()
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("runtime/worktree: remove %q: %w", root, err)
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

// Layout returns the distinct lifecycle root and selected repository CWD.
func (m *Manager) Layout(sessionID string) (workarea.Layout, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res, ok := m.sessions[sessionID]
	if !ok {
		return workarea.Layout{}, fmt.Errorf("%w: %s", ErrUnknownSession, sessionID)
	}
	return workarea.Layout{Root: workarea.RootPath(res.workareaRootOrPath()), Repository: workarea.RepositoryPath(res.Path)}, nil
}

// RepositoryPaths returns a copy of the declaration name -> path map.
func (m *Manager) RepositoryPaths(sessionID string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownSession, sessionID)
	}
	paths := make(map[string]string, len(res.Repositories))
	for name, repositoryPath := range res.Repositories {
		paths[name] = repositoryPath
	}
	return paths, nil
}

func (r ProvisionResult) workareaRootOrPath() string {
	if r.WorkareaRoot != "" {
		return r.WorkareaRoot
	}
	return r.Path
}

func (m *Manager) provisionLayout(sessionLeaf string, spec ProvisionSpec) (workarea.Layout, *workarea.NormalizedDeclaration, error) {
	if spec.RepositoryDeclaration == nil {
		if filepath.Clean(filepath.Join(m.parentDir, sessionLeaf)) == filepath.Clean(m.leases.Dir()) {
			return workarea.Layout{}, nil, errors.New("runtime/worktree: destination conflicts with terminal lease state directory")
		}
		layout, err := workarea.FlatLayout(m.parentDir, sessionLeaf)
		if err != nil {
			return workarea.Layout{}, nil, fmt.Errorf("runtime/worktree: resolve legacy layout: %w", err)
		}
		return layout, nil, nil
	}
	if spec.Strategy != StrategyClone {
		return workarea.Layout{}, nil, errors.New("runtime/worktree: session-root-v1 currently requires clone strategy")
	}
	normalized, err := spec.RepositoryDeclaration.Normalize()
	if err != nil {
		return workarea.Layout{}, nil, err
	}
	if err := normalized.ValidatePrimarySource(workarea.RepositorySource{Repository: spec.RepoURL, Ref: spec.SourceRef}); err != nil {
		return workarea.Layout{}, nil, err
	}
	if err := spec.ExecutorCapabilities.ValidateFor(normalized); err != nil {
		return workarea.Layout{}, nil, err
	}
	layout, err := workarea.NewLayout(m.parentDir, sessionLeaf, normalized.Selected.Leaf)
	if err != nil {
		return workarea.Layout{}, nil, err
	}
	return layout, &normalized, nil
}

func (m *Manager) provisionLayoutOnce(
	ctx context.Context,
	layout workarea.Layout,
	declaration *workarea.NormalizedDeclaration,
	workareaID string,
	spec ProvisionSpec,
) (paths map[string]string, acquisition workarea.AcquisitionRecord, resultErr error) {
	if declaration == nil {
		if err := m.provisionOnce(ctx, layout.Repository.String(), spec); err != nil {
			return nil, workarea.AcquisitionRecord{}, err
		}
		paths = make(map[string]string, 1)
		if leaf, err := workarea.RepositoryLeaf(spec.RepoURL); err == nil {
			paths[leaf] = layout.Repository.String()
		}
		return paths, workarea.AcquisitionRecord{}, nil
	}
	seedPaths := map[string]string(nil)
	if spec.CacheSeedID != "" {
		seeds, seedErr := m.seedStore()
		if seedErr != nil {
			return nil, workarea.AcquisitionRecord{}, seedErr
		}
		_, resolvedSeedPaths, seedErr := seeds.Ensure(ctx, spec.CacheSeedID, *declaration, func(ctx context.Context, repository workarea.NormalizedRepository, destination string) error {
			return m.provisionOnce(ctx, destination, ProvisionSpec{
				SessionID: spec.SessionID, RepoURL: repository.Source.Repository,
				Branch: repository.Source.Ref, Strategy: StrategyClone,
				SparsePaths: append([]string(nil), repository.Source.Paths...),
			})
		})
		if seedErr != nil {
			return nil, workarea.AcquisitionRecord{}, seedErr
		}
		seedPaths = resolvedSeedPaths
	}
	acquisitions, err := m.acquisitionStore()
	if err != nil {
		return nil, workarea.AcquisitionRecord{}, err
	}
	claim, err := acquisitions.Begin(
		spec.SessionID, workareaID, layout.Root, declaration.Selected.Leaf, spec.CacheSeedID,
	)
	if err != nil {
		if errors.Is(err, workarea.ErrAcquisitionRootOccupied) {
			return nil, workarea.AcquisitionRecord{}, fmt.Errorf("%w: %s", ErrWorkareaRootOccupied, layout.Root.String())
		}
		return nil, workarea.AcquisitionRecord{}, err
	}
	committed := false
	preserveForRecovery := false
	defer func() {
		if committed || preserveForRecovery {
			return
		}
		if abortErr := acquisitions.Abort(claim.Record.AcquisitionID); abortErr != nil && resultErr == nil {
			resultErr = abortErr
		}
	}()
	crashBoundary := func(stage string) error {
		if m.provisionHook == nil {
			return nil
		}
		if err := m.provisionHook(stage, claim.Record); err != nil {
			preserveForRecovery = true
			if abandonErr := acquisitions.Abandon(claim.Record.AcquisitionID); abandonErr != nil {
				return errors.Join(err, abandonErr)
			}
			return err
		}
		return nil
	}
	if err := crashBoundary("root-created"); err != nil {
		return nil, workarea.AcquisitionRecord{}, err
	}
	stagingLayout := workarea.Layout{Root: claim.StagingRoot}
	selectedPath, err := stagingLayout.RepositoryPathFor(declaration.Selected.Leaf)
	if err != nil {
		return nil, workarea.AcquisitionRecord{}, err
	}
	stagingLayout.Repository = selectedPath

	paths = make(map[string]string, len(declaration.Repositories))
	resolvedRefs := make(map[string]string, len(declaration.Repositories))
	for index, repository := range declaration.Repositories {
		repositoryPath, err := stagingLayout.RepositoryPathFor(repository.Leaf)
		if err != nil {
			return nil, workarea.AcquisitionRecord{}, err
		}
		repositorySpec := ProvisionSpec{
			SessionID:   spec.SessionID,
			RepoURL:     repository.Source.Repository,
			Branch:      repository.Source.Ref,
			Strategy:    StrategyClone,
			SparsePaths: append([]string(nil), repository.Source.Paths...),
		}
		if err := m.provisionOnceWithReference(ctx, repositoryPath.String(), repositorySpec, seedPaths[repository.Name]); err != nil {
			return nil, workarea.AcquisitionRecord{}, fmt.Errorf("runtime/worktree: provision declared repository %q: %w", repository.Name, err)
		}
		paths[repository.Name] = filepath.Join(layout.Root.String(), repository.Leaf)
		out, err := m.runGit(ctx, "", "-C", repositoryPath.String(), "rev-parse", "HEAD")
		if err != nil {
			return nil, workarea.AcquisitionRecord{}, fmt.Errorf("runtime/worktree: resolve declared repository %q ref: %w (%s)", repository.Name, err, strings.TrimSpace(string(out)))
		}
		resolvedRefs[repository.Name] = strings.TrimSpace(string(out))
		if index == 0 {
			if err := crashBoundary("mid-clone"); err != nil {
				return nil, workarea.AcquisitionRecord{}, err
			}
		}
	}
	if err := crashBoundary("pre-record"); err != nil {
		return nil, workarea.AcquisitionRecord{}, err
	}
	record := workarea.NewDeclarationRecord(spec.SessionID, workareaID, *declaration, resolvedRefs, claim.Record.AcquisitionID)
	if err := workarea.WriteDeclaration(ctx, claim.StagingRoot, record); err != nil {
		return nil, workarea.AcquisitionRecord{}, err
	}
	acquisition, err = acquisitions.Commit(claim.Record.AcquisitionID)
	if err != nil {
		if errors.Is(err, workarea.ErrAcquisitionRootOccupied) {
			return nil, workarea.AcquisitionRecord{}, fmt.Errorf("%w: %s", ErrWorkareaRootOccupied, layout.Root.String())
		}
		return nil, workarea.AcquisitionRecord{}, err
	}
	committed = true
	return paths, acquisition, nil
}

// provisionOnce runs one git invocation per spec.Strategy.
func (m *Manager) provisionOnce(ctx context.Context, dst string, spec ProvisionSpec) error {
	return m.provisionOnceWithReference(ctx, dst, spec, "")
}

func (m *Manager) provisionOnceWithReference(ctx context.Context, dst string, spec ProvisionSpec, referencePath string) error {
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
		if referencePath != "" {
			args = append(args, "--reference-if-able", referencePath, "--dissociate")
		}
		if len(spec.SparsePaths) > 0 {
			args = append(args, "--filter=blob:none", "--sparse")
		}
		if spec.Branch != "" {
			args = append(args, "--branch", spec.Branch)
		}
		args = append(args, cloneURL, dst)
		out, err := m.runGit(ctx, spec.RepoURL, args...)
		if err != nil {
			return fmt.Errorf("git clone: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		if len(spec.SparsePaths) > 0 {
			sparseArgs := []string{"-C", dst, "sparse-checkout", "set", "--"}
			sparseArgs = append(sparseArgs, spec.SparsePaths...)
			out, err := m.runGit(ctx, "", sparseArgs...)
			if err != nil {
				return fmt.Errorf("git sparse-checkout: %w (%s)", err, strings.TrimSpace(string(out)))
			}
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

// runGit runs a git invocation, applying the credential-hardening seam when a
// GitAuth callback is configured. repoURL is the remote the operation targets;
// it is passed to the GitAuth resolver and is "" for purely-local operations
// (`worktree list`, `worktree remove`), in which case GitAuth still runs and
// may suppress the credential helper.
//
// repoURL is also what any resolved auth header is SCOPED to: gitexec emits
// `http.<repoURL>.extraHeader`, never the bare key, so the credential is not
// offered to unrelated remotes by the git process or anything it spawns. A
// purely-local operation therefore gets no header — correct, since it issues no
// HTTP request — and a repoURL that is not an http(s) remote gets none either,
// which is logged rather than silently swallowed.
//
// When m.gitAuth is nil this routes straight through the env-free CommandRunner
// — byte-identical to the pre-seam path.
func (m *Manager) runGit(ctx context.Context, repoURL string, args ...string) ([]byte, error) {
	if m.gitAuth == nil {
		return m.runner(ctx, "git", args...)
	}
	header, suppress, err := m.gitAuth(ctx, repoURL)
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: resolve git auth: %w", err)
	}
	if header != "" && repoURL != "" {
		if _, scoped := gitexec.ExtraHeaderConfigKey(repoURL); !scoped {
			// The resolver produced a credential for a remote that cannot
			// carry an HTTP header (SSH/scp-style, or a local path), so it is
			// dropped. Say so here: the alternative is an unauthenticated
			// operation that fails later at the remote with a message that
			// does not name the cause. repoURL is not logged — it may embed
			// the credential itself (CleanURL exists because of that).
			m.logger.Warn("runtime/worktree: git auth header dropped, remote is not an http(s) URL so the header cannot be scoped to it")
		}
	}
	env := gitexec.HardenedEnv(nil, suppress, gitexec.Auth{Header: header, RemoteURL: repoURL})
	return m.envRunner(ctx, env, "git", args...)
}

func (m *Manager) retainedPath(path string) (bool, error) {
	return m.leases.RetainedPath(path)
}

func (m *Manager) requestReleasePath(ctx context.Context, path string) (bool, error) {
	return m.leases.RequestReleasePath(ctx, path)
}

// cleanupConflict removes only the root the current failed acquisition created.
// Callers never invoke it for ErrWorkareaRootOccupied, so pre-existing state is
// refused rather than reclaimed by inference.
func (m *Manager) cleanupConflict(ctx context.Context, layout workarea.Layout, spec ProvisionSpec) error {
	root := layout.Root.String()
	retained, err := m.retainedPath(root)
	if err != nil {
		return fmt.Errorf("runtime/worktree: check terminal lease before conflict cleanup: %w", err)
	}
	if retained {
		return fmt.Errorf("%w: %s", workarea.ErrWorkareaLeased, root)
	}
	if spec.Strategy == StrategyWorktreeAdd && spec.ParentRepoPath != "" {
		_, _ = m.runGit(ctx, "", "-C", spec.ParentRepoPath, "worktree", "remove", "--force", layout.Repository.String())
	}
	if _, err := os.Stat(root); err == nil {
		_ = os.RemoveAll(root)
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
