package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/interview"
)

// Compile-time assertion that WorkerSpawner satisfies the
// ActiveWorkareaProvider contract consumed by WorkareaArchiveRegistry.
// The runtime wires the spawner in via NewWorkareaArchiveRegistry's
// ActiveProvider option (see Daemon.workareaArchiveRegistry()), so the
// /api/daemon/workareas list and inspect endpoints return live-pool
// members rather than archives only.
var _ ActiveWorkareaProvider = (*WorkerSpawner)(nil)

// SpawnerOptions configure a WorkerSpawner.
type SpawnerOptions struct {
	Projects []ProjectConfig
	// EnabledProjectIDs is the authoritative project-admission set. When nil,
	// IDs are derived from Projects for legacy callers.
	EnabledProjectIDs     []string
	MaxConcurrentSessions int
	// WorkerCommand is the command to run for each accepted session. The
	// caller may pass arbitrary args; the session-specific environment is
	// added on top of os.Environ() at spawn time.
	//
	// When empty, a short-lived /bin/sh stub is used that prints
	// "session-started:<id>" and exits 0 — sufficient for testing the
	// daemon's accept/lifecycle path without launching real worker binaries.
	WorkerCommand []string
	// BaseEnv is the environment injected into every worker process.
	BaseEnv map[string]string

	// OnPreSpawn is an optional hook invoked once per spawn, immediately
	// before the child process is exec'd. It receives the final SessionSpec
	// and the env slice that would otherwise be exec'd, and returns the env
	// slice that will actually be exec'd plus an optional error.
	//
	// Callers may use this to layer per-session env entries (e.g.,
	// credentials resolved at spawn time) over the spawner's BaseEnv.
	// BaseEnv is set once at spawner construction and cannot express
	// per-session values; this hook is the extension point for callers
	// that need to compute env entries from the inbound SessionSpec.
	//
	// The hook runs AFTER the BaseEnv + SessionSpec.Env composition, so
	// returned entries can both add new keys and override BaseEnv keys.
	//
	// The hook MUST NOT block on I/O paths that can hang indefinitely.
	// Spawn latency budget is on the order of 250ms; if the hook needs
	// to do I/O, it should have its own timeout.
	//
	// Returning a non-nil error aborts the spawn: AcceptWork returns the
	// error to the caller without starting the child process. This is the
	// fail-closed path for callers that must deny a spawn when a required
	// credential is unavailable (e.g. byok/metered/shared sessions where
	// the credential snapshot gate returns 403). For transient failures
	// that should not block the spawn, callers should return (env, nil)
	// with a sentinel env var (e.g. DONMAI_CREDENTIAL_SNAPSHOT_FAILED=1)
	// so the agent can retry via its own fallback.
	//
	// A nil env slice return with a nil error is equivalent to returning
	// the input env unchanged (no-op, same as the nil hook case).
	OnPreSpawn func(spec SessionSpec, env []string) ([]string, error)

	// OnSpawnAborted is the optional rollback partner for OnPreSpawn. It is
	// invoked synchronously exactly once when OnPreSpawn returned nil (and
	// therefore transferred ownership of any resources it acquired) but the
	// subsequent cmd.Start failed. The error is the same wrapped
	// "start worker" error AcceptWork returns.
	//
	// It is deliberately NOT invoked when OnPreSpawn itself fails: the hook
	// never transferred ownership, so it remains responsible for releasing
	// anything acquired before returning its error. It is also never invoked
	// after a successful start; SessionEventEnded owns cleanup from that point.
	// A nil hook is a no-op.
	OnSpawnAborted func(spec SessionSpec, err error)

	// WorktreeParentDir is the directory under which the spawned worker
	// creates each per-session worktree (<WorktreeParentDir>/<sessionID>).
	// It MUST match the parent the worker resolves (the worker uses
	// statepath.Resolve("worktrees", …) when no --worktree-dir override is
	// passed), so the daemon can publish the worktree path on the
	// SessionHandle without spawning anything. When empty, the spawner
	// leaves SessionHandle.WorktreePath empty (a local reader then falls
	// back to a per-session detail call). The daemon sets this to the same
	// statepath-resolved default the worker uses.
	WorktreeParentDir string

	// Now lets tests deterministically clock acceptedAt timestamps.
	Now func() time.Time
	// Stdout is where worker stdout is forwarded with a "[worker:<id>]"
	// prefix. Defaults to os.Stdout. Set to io.Discard in tests.
	StdoutPrefixWriter PrefixedWriter
	StderrPrefixWriter PrefixedWriter
}

// PrefixedWriter is the minimal sink interface used by the spawner to emit
// child stdout/stderr. Implementations are responsible for prefixing each
// line with the worker tag.
type PrefixedWriter interface {
	WriteWorkerLine(workerID, line string)
}

// SessionEvent is emitted on the spawner's events channel.
type SessionEvent struct {
	Kind    SessionEventKind
	Handle  SessionHandle
	Spec    SessionSpec
	ExitErr error
}

// SessionEventKind identifies the kind of SessionEvent.
type SessionEventKind string

// Session event kind constants.
const (
	SessionEventStarted SessionEventKind = "started"
	SessionEventEnded   SessionEventKind = "ended"
)

// pumpDrainGrace bounds how long the session reaper waits for the stdout/
// stderr pump goroutines to reach EOF before calling cmd.Wait (which closes
// the pipes and discards anything still buffered). Normally-exiting children
// close their pipe ends on exit, so the pumps finish in microseconds and the
// grace never elapses; it only fires when a grandchild inherited a pipe end
// and outlived the worker.
const pumpDrainGrace = 10 * time.Second

// WorkerSpawner manages the lifecycle of worker child processes.
type WorkerSpawner struct {
	opts SpawnerOptions

	mu                     sync.Mutex
	sessions               map[string]*spawnedSession
	pendingSessions        map[string]*spawnAttempt
	nextAttemptID          uint64
	sessionHistory         map[string]struct{}
	sessionHistoryOrder    []string
	accepting              bool
	extraProjects          []ProjectConfig // satellite/additional org projects; never clobbered by SetProjects
	extraEnabledProjectIDs map[string]struct{}
	killProcessGroup       func(*exec.Cmd) error

	listenersMu sync.Mutex
	listeners   []func(SessionEvent)
}

const sessionHistoryLimit = 4096

// ErrSessionAlreadyActive reports a duplicate active or in-progress SessionID.
// Callers may treat it as an idempotent redelivery rather than a failed claim.
var ErrSessionAlreadyActive = errors.New("session already active")

type spawnAttempt struct {
	id   uint64
	spec SessionSpec
}

type spawnedSession struct {
	handle  SessionHandle
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	spec    SessionSpec
	attempt *spawnAttempt
}

// NewWorkerSpawner constructs a spawner. Workers will not be spawned until
// AcceptWork is called.
func NewWorkerSpawner(opts SpawnerOptions) *WorkerSpawner {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxConcurrentSessions <= 0 {
		opts.MaxConcurrentSessions = 8
	}
	opts.EnabledProjectIDs = normalizeSpawnerEnabledIDs(opts.EnabledProjectIDs, opts.Projects)
	return &WorkerSpawner{
		opts:                   opts,
		sessions:               make(map[string]*spawnedSession),
		pendingSessions:        make(map[string]*spawnAttempt),
		sessionHistory:         make(map[string]struct{}),
		accepting:              true,
		extraEnabledProjectIDs: make(map[string]struct{}),
		killProcessGroup:       killSessionProcessGroup,
	}
}

func normalizeSpawnerEnabledIDs(explicit []string, projects []ProjectConfig) []string {
	if explicit != nil {
		return normalizeProjectIDs(explicit)
	}
	return projectIDsFromRepositories(projects)
}

// On registers a session-event listener. Listeners are invoked synchronously
// from the spawner goroutine; do not block them.
func (s *WorkerSpawner) On(fn func(SessionEvent)) {
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()
	s.listeners = append(s.listeners, fn)
}

func (s *WorkerSpawner) emit(ev SessionEvent) {
	s.listenersMu.Lock()
	listeners := append([]func(SessionEvent){}, s.listeners...)
	s.listenersMu.Unlock()
	for _, fn := range listeners {
		fn(ev)
	}
}

// ActiveCount returns the number of in-flight sessions across all run modes.
func (s *WorkerSpawner) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions) + len(s.pendingSessions)
}

// ActiveInteractiveCount returns the number of in-flight interactive-occupancy
// sessions. Both the PTY "interactive" mode and legacy "interview" mode count;
// headless and unknown modes do not.
func (s *WorkerSpawner) ActiveInteractiveCount() int {
	_, interactive := s.ActiveSessionCounts()
	return interactive
}

// ActiveSessionCounts returns one coherent occupancy snapshot: active is the
// unclassed count across every run mode, while activeInteractive is the union of
// PTY "interactive" and legacy "interview" sessions. Both values are derived
// while holding the same lock, so concurrent session starts/stops cannot produce
// activeInteractive > active.
func (s *WorkerSpawner) ActiveSessionCounts() (active, activeInteractive int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active = len(s.sessions) + len(s.pendingSessions)
	for _, ss := range s.sessions {
		switch ss.spec.Mode {
		case interactiveRunMode, interview.InterviewRunMode:
			activeInteractive++
		}
	}
	for _, attempt := range s.pendingSessions {
		switch attempt.spec.Mode {
		case interactiveRunMode, interview.InterviewRunMode:
			activeInteractive++
		}
	}
	return active, activeInteractive
}

// IsAccepting reports whether the spawner is currently accepting work.
func (s *WorkerSpawner) IsAccepting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepting
}

// ActiveSessions returns a snapshot of the current session handles.
func (s *WorkerSpawner) ActiveSessions() []SessionHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionHandle, 0, len(s.sessions))
	for _, ss := range s.sessions {
		out = append(out, ss.handle)
	}
	return out
}

// ActiveWorkareas projects the spawner's in-flight sessions onto the
// canonical afclient.WorkareaSummary wire shape so the WorkareaArchiveRegistry
// can union live-pool members with on-disk archives in the GET
// /api/daemon/workareas response (Wave 11 / S5; ADR-2026-05-07-daemon-
// http-control-api.md §D4a).
//
// The projection is pull-based — the spawner holds no separate workarea
// map; each call materialises summaries from the live `sessions` map under
// the same `mu` lock that ActiveSessions uses. ProjectID is resolved via
// the project allowlist using the same matcher AcceptWork applies. The
// summary's ID is the spawner's session id so /api/daemon/workareas/<id>
// reaches the live entry.
//
// Output is sorted by SessionID for deterministic test assertions.
func (s *WorkerSpawner) ActiveWorkareas() []afclient.WorkareaSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]afclient.WorkareaSummary, 0, len(s.sessions))
	for _, ss := range s.sessions {
		summary := afclient.WorkareaSummary{
			ID:         ss.spec.SessionID,
			Kind:       afclient.WorkareaKindActive,
			Status:     afclient.WorkareaStatusReady,
			Repository: ss.spec.Repository,
			Ref:        ss.spec.Ref,
			SessionID:  ss.spec.SessionID,
		}
		project := s.findProjectForSpecLocked(ss.spec)
		if ss.spec.ProjectID == "" {
			project = s.findProjectLocked(ss.spec.Repository)
		}
		if project != nil {
			summary.ProjectID = project.ID
		} else {
			summary.ProjectID = ss.spec.ProjectID
		}
		// handle.AcceptedAt is RFC3339 today; surface it on the wire as
		// AcquiredAt (the active-only "session admitted to pool" stamp).
		// Parse failures yield nil — better than a zero-time pointer
		// that JSON-encodes as "0001-01-01T00:00:00Z".
		if ts, err := time.Parse(time.RFC3339, ss.handle.AcceptedAt); err == nil {
			t := ts
			summary.AcquiredAt = &t
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// Pause stops accepting new work but leaves running sessions alive.
func (s *WorkerSpawner) Pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepting = false
}

// Resume restores accepting state.
func (s *WorkerSpawner) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepting = true
}

// SetMaxConcurrentSessions updates the local session capacity used for future
// AcceptWork decisions. Existing sessions are never interrupted.
func (s *WorkerSpawner) SetMaxConcurrentSessions(n int) error {
	if n < 0 {
		return errors.New("max concurrent sessions must be >= 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.MaxConcurrentSessions = n
	return nil
}

// SetProjects atomically swaps the spawner's base project allowlist used by
// AcceptWork's findProjectLocked check. Existing in-flight sessions
// continue against whichever project they were dispatched under — the
// new list governs only future AcceptWork calls.
//
// Phase 2c of 2026-05-18-daemon-config-sync-DESIGN.md — wired by the
// mutation-applier so platform-driven project.add / project.remove
// proposals take effect on the very next claim without a daemon restart.
//
// SetProjects replaces ONLY the base project set (opts.Projects). Any
// satellite/additional org projects registered via AddProjects are
// preserved so a yaml-watcher reload of the primary org's config does
// NOT evict satellite entries.
//
// A defensive copy is taken so subsequent mutations to the caller's
// slice (e.g. daemon.go reusing a single buffer) don't race the spawner.
func (s *WorkerSpawner) SetProjects(projects []ProjectConfig) {
	s.SetProjectConfiguration(projects, projectIDsFromRepositories(projects))
}

// SetProjectConfiguration atomically replaces the base repository resources
// and project-admission set. Additional identities registered through
// AddProjects/AddEnabledProjectIDs remain intact.
func (s *WorkerSpawner) SetProjectConfiguration(projects []ProjectConfig, enabledProjectIDs []string) {
	projectCopy := append([]ProjectConfig(nil), projects...)
	enabledCopy := normalizeProjectIDs(enabledProjectIDs)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.Projects = projectCopy
	s.opts.EnabledProjectIDs = enabledCopy
}

// AddProjects appends additional project configurations (e.g. satellite-org
// projects in a shared-spawner topology) to the spawner's allowlist. The new
// entries are held in a separate slice from the base set managed by
// SetProjects, so a subsequent yaml-watcher reload that calls SetProjects does
// NOT evict them.
//
// Deduplication is by ProjectConfig.ID and ProjectConfig.Repository — a
// candidate entry is skipped if an entry with the same ID or the same non-empty
// Repository already exists in either the base or extra set, preventing
// double-entries when AddProjects is called repeatedly for the same org.
//
// The call is safe for concurrent use: it acquires the same mutex that
// AcceptWork and SetProjects use.
func (s *WorkerSpawner) AddProjects(extra []ProjectConfig) {
	if len(extra) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, candidate := range extra {
		if s.isDuplicateLocked(candidate) {
			continue
		}
		s.extraProjects = append(s.extraProjects, candidate)
		if candidate.ID != "" {
			s.extraEnabledProjectIDs[candidate.ID] = struct{}{}
		}
	}
}

// AddEnabledProjectIDs admits additional projects without requiring a
// repository resource. It is used by shared-spawner embedders for identities
// whose repository bindings are managed independently.
func (s *WorkerSpawner) AddEnabledProjectIDs(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range normalizeProjectIDs(ids) {
		s.extraEnabledProjectIDs[id] = struct{}{}
	}
}

// isDuplicateLocked reports whether candidate is already represented in either
// the base or extra project sets. Must be called with s.mu held.
func (s *WorkerSpawner) isDuplicateLocked(candidate ProjectConfig) bool {
	for _, existing := range s.opts.Projects {
		if candidate.ID == existing.ID && candidate.Repository == existing.Repository {
			return true
		}
		if candidate.Repository != "" && candidate.Repository == existing.Repository {
			return true
		}
	}
	for _, existing := range s.extraProjects {
		if candidate.ID == existing.ID && candidate.Repository == existing.Repository {
			return true
		}
		if candidate.Repository != "" && candidate.Repository == existing.Repository {
			return true
		}
	}
	return false
}

// AllProjects returns a snapshot of the union of the base project set and any
// additional projects registered via AddProjects. The returned slice is a
// defensive copy; callers may safely hold it across subsequent SetProjects or
// AddProjects calls.
//
// This is the correct view to pass to PollItemToSessionSpec /
// PollItemToSessionDetail in poll-loop closures so that satellite-org projects
// registered after daemon start are visible to slug→URL resolution.
func (s *WorkerSpawner) AllProjects() []ProjectConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := len(s.opts.Projects) + len(s.extraProjects)
	if total == 0 {
		return nil
	}
	out := make([]ProjectConfig, 0, total)
	out = append(out, s.opts.Projects...)
	out = append(out, s.extraProjects...)
	return out
}

// AllEnabledProjectIDs returns the sorted union of base and additional
// project-admission identities.
func (s *WorkerSpawner) AllEnabledProjectIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := append([]string(nil), s.opts.EnabledProjectIDs...)
	for id := range s.extraEnabledProjectIDs {
		ids = append(ids, id)
	}
	return normalizeProjectIDs(ids)
}

// AcceptWork validates the spec, reserves its SessionID, spawns a worker, and
// returns its handle. The reservation spans OnPreSpawn and cmd.Start so direct,
// nil-detail, and mixed callers cannot start two attempts for the same id.
func (s *WorkerSpawner) AcceptWork(spec SessionSpec) (*SessionHandle, error) {
	s.mu.Lock()
	if !s.accepting {
		s.mu.Unlock()
		return nil, errors.New("not accepting new work (paused or draining)")
	}
	if _, active := s.sessions[spec.SessionID]; active {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrSessionAlreadyActive, spec.SessionID)
	}
	if _, pending := s.pendingSessions[spec.SessionID]; pending {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrSessionAlreadyActive, spec.SessionID)
	}
	if active, capacity := len(s.sessions)+len(s.pendingSessions), s.opts.MaxConcurrentSessions; active >= capacity {
		s.mu.Unlock()
		// Snapshot the counts BEFORE unlocking — formatting them after
		// release races with the reaper's delete on s.sessions.
		return nil, fmt.Errorf("at capacity (%d/%d sessions)", active, capacity)
	}
	project, admissionErr := s.resolveProjectForSpecLocked(spec)
	if admissionErr != nil {
		s.mu.Unlock()
		return nil, admissionErr
	}
	if project == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("project admission could not be resolved")
	}
	if spec.Repository == "" && project.Repository != "" {
		spec.Repository = project.Repository
	}
	if spec.RepositoryID == "" && project.RepositoryID != "" {
		spec.RepositoryID = project.RepositoryID
	}
	attempt := &spawnAttempt{id: s.nextAttemptIDLocked(), spec: spec}
	s.pendingSessions[spec.SessionID] = attempt
	s.mu.Unlock()

	return s.spawn(spec, project, attempt)
}

func (s *WorkerSpawner) nextAttemptIDLocked() uint64 {
	s.nextAttemptID++
	if s.nextAttemptID == 0 {
		s.nextAttemptID++
	}
	return s.nextAttemptID
}

func (s *WorkerSpawner) releasePendingAttempt(attempt *spawnAttempt) {
	if attempt == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingSessions[attempt.spec.SessionID] == attempt {
		delete(s.pendingSessions, attempt.spec.SessionID)
	}
}

func (s *WorkerSpawner) resolveProjectForSpecLocked(spec SessionSpec) (*ProjectConfig, error) {
	if spec.ProjectID != "" {
		if !s.isProjectAllowedLocked(spec.ProjectID) {
			return nil, fmt.Errorf("project %q is not allowed", spec.ProjectID)
		}
		if spec.Repository == "" && spec.RepositoryID == "" {
			if spec.RequiresRepository {
				if primary := s.findPrimaryProjectRepositoryLocked(spec.ProjectID); primary != nil {
					return primary, nil
				}
				return nil, fmt.Errorf("project %q requires an explicit repository or configured primary", spec.ProjectID)
			}
			return &ProjectConfig{ID: spec.ProjectID}, nil
		}
		project := s.findProjectForSpecLocked(spec)
		if project == nil {
			if spec.RepositoryID != "" {
				return nil, fmt.Errorf("repository %q is not configured for project %q", spec.RepositoryID, spec.ProjectID)
			}
			return nil, fmt.Errorf("repository %q is not configured for project %q", spec.Repository, spec.ProjectID)
		}
		return project, nil
	}

	project := s.findProjectLocked(spec.Repository)
	if project == nil {
		return nil, fmt.Errorf("repository %q is not configured", spec.Repository)
	}
	if !s.isProjectAllowedLocked(project.ID) {
		return nil, fmt.Errorf("project %q is not allowed", project.ID)
	}
	return project, nil
}

func (s *WorkerSpawner) findPrimaryProjectRepositoryLocked(projectID string) *ProjectConfig {
	for i := range s.opts.Projects {
		if s.opts.Projects[i].ID == projectID && s.opts.Projects[i].Primary {
			return &s.opts.Projects[i]
		}
	}
	for i := range s.extraProjects {
		if s.extraProjects[i].ID == projectID && s.extraProjects[i].Primary {
			return &s.extraProjects[i]
		}
	}
	return nil
}

func (s *WorkerSpawner) isProjectAllowedLocked(id string) bool {
	for _, allowed := range s.opts.EnabledProjectIDs {
		if allowed == id {
			return true
		}
	}
	_, ok := s.extraEnabledProjectIDs[id]
	return ok
}

func (s *WorkerSpawner) findProjectForSpecLocked(spec SessionSpec) *ProjectConfig {
	find := func(projects []ProjectConfig) *ProjectConfig {
		for i := range projects {
			project := &projects[i]
			if project.ID != spec.ProjectID {
				continue
			}
			if spec.RepositoryID != "" && project.RepositoryID == spec.RepositoryID {
				return project
			}
			if spec.RepositoryID == "" && matchProject(project, spec.Repository) != nil {
				return project
			}
		}
		return nil
	}
	if project := find(s.opts.Projects); project != nil {
		return project
	}
	return find(s.extraProjects)
}

// findProjectLocked searches the union of the base project set (opts.Projects)
// and the extra project set (extraProjects) for an entry matching repository.
// Must be called with s.mu held.
func (s *WorkerSpawner) findProjectLocked(repository string) *ProjectConfig {
	for i := range s.opts.Projects {
		if p := matchProject(&s.opts.Projects[i], repository); p != nil {
			return p
		}
	}
	for i := range s.extraProjects {
		if p := matchProject(&s.extraProjects[i], repository); p != nil {
			return p
		}
	}
	return nil
}

// matchProject returns p if its ID or Repository fields match repository, or
// nil if neither matches. The platform sends spec.Repository as the Linear
// project slug (e.g. "smoke-alpha"), which doesn't match the GitHub repo name
// in p.Repository (e.g. ".../rensei-smokes-alpha"). Match by p.ID as well so
// operators can express the link via the allowlist entry's id. (REN-NEW)
func matchProject(p *ProjectConfig, repository string) *ProjectConfig {
	if p.Repository == repository ||
		p.ID == repository ||
		strings.HasSuffix(repository, "/"+p.Repository) ||
		strings.HasSuffix(p.Repository, "/"+repository) {
		return p
	}
	return nil
}

func (s *WorkerSpawner) spawn(spec SessionSpec, project *ProjectConfig, attempt *spawnAttempt) (*SessionHandle, error) {
	defer s.releasePendingAttempt(attempt)

	command := s.opts.WorkerCommand
	if len(command) == 0 {
		// Stub worker — exits 0 immediately. Production code paths
		// should always have WorkerCommand set (see daemon.go's
		// defaultWorkerCommand). Surfacing this at warn level so
		// operators notice when the daemon has fallen back to the
		// test stub.
		slog.Warn(
			"worker spawner: WorkerCommand not set; using /bin/sh test stub (sessions exit immediately — set WorkerCommand or deploy a binary that resolves via os.Executable)",
			"sessionId", spec.SessionID,
		)
		command = []string{"/bin/sh", "-c", `printf 'session-started:%s\n' "$DONMAI_SESSION_ID"; exit 0`}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec
	configureSessionProcessGroup(cmd)
	cmd.Env = composeEnv(s.opts.BaseEnv, spec.Env, map[string]string{
		"DONMAI_SESSION_ID":    spec.SessionID,
		"DONMAI_REPOSITORY":    spec.Repository,
		"DONMAI_REPOSITORY_ID": spec.RepositoryID,
		"DONMAI_REF":           spec.Ref,
		"DONMAI_PROJECT_ID":    project.ID,
	})

	// OnPreSpawn is the extension point for callers that need to compute
	// per-session env entries (e.g., credentials resolved at spawn time)
	// the static BaseEnv map cannot express. It runs after composeEnv so
	// the returned slice can add or override anything BaseEnv + spec.Env
	// produced. A nil env return with nil error is a no-op.
	//
	// A non-nil error aborts the spawn (fail-closed). The child process is
	// never started and AcceptWork surfaces the error to the caller so the
	// daemon's poll loop can NACK the session.
	preSpawnOwnsCleanup := false
	if s.opts.OnPreSpawn != nil {
		next, hookErr := s.opts.OnPreSpawn(spec, cmd.Env)
		if hookErr != nil {
			cancel()
			return nil, fmt.Errorf("pre-spawn hook: %w", hookErr)
		}
		preSpawnOwnsCleanup = true
		if next != nil {
			cmd.Env = next
		}
	}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		startErr := fmt.Errorf("start worker: %w", err)
		if preSpawnOwnsCleanup && s.opts.OnSpawnAborted != nil {
			s.opts.OnSpawnAborted(spec, startErr)
		}
		return nil, startErr
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	handle := SessionHandle{
		SessionID:   spec.SessionID,
		PID:         pid,
		AcceptedAt:  s.opts.Now().UTC().Format(time.RFC3339),
		State:       SessionRunning,
		ProjectName: project.ID,
		Repository:  spec.Repository,
	}
	// Publish the worktree path so GET /api/daemon/sessions is
	// self-sufficient for a local reader (host-watch). The worker resolves
	// the same <parent>/<sessionID> leaf; we mirror that here without
	// spawning anything. Empty parent leaves WorktreePath empty (the reader
	// falls back to a per-session detail call).
	if s.opts.WorktreeParentDir != "" {
		handle.WorktreePath = filepath.Join(s.opts.WorktreeParentDir, spec.SessionID)
	}

	ss := &spawnedSession{
		handle:  handle,
		cmd:     cmd,
		cancel:  cancel,
		spec:    spec,
		attempt: attempt,
	}

	s.mu.Lock()
	if s.pendingSessions[spec.SessionID] != attempt {
		s.mu.Unlock()
		cancel()
		_ = cmd.Wait()
		return nil, fmt.Errorf("spawn reservation lost for session %q", spec.SessionID)
	}
	delete(s.pendingSessions, spec.SessionID)
	s.sessions[spec.SessionID] = ss
	s.rememberSessionLocked(spec.SessionID)
	s.mu.Unlock()

	// Stream stdout / stderr with worker-tagged prefix. Pump completion is
	// tracked because os/exec's Wait CLOSES the pipe read ends: reaping
	// before the pumps hit EOF silently discards buffered, not-yet-read
	// child output (a loaded CI runner lost the child's only stderr record
	// that way — 2026-07-06, run 28822266352).
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		if s.opts.StdoutPrefixWriter != nil {
			pumpLines(stdout, spec.SessionID, s.opts.StdoutPrefixWriter)
		} else {
			drain(stdout)
		}
	}()
	go func() {
		defer pumps.Done()
		if s.opts.StderrPrefixWriter != nil {
			pumpLines(stderr, spec.SessionID, s.opts.StderrPrefixWriter)
		} else {
			drain(stderr)
		}
	}()

	s.emit(SessionEvent{Kind: SessionEventStarted, Handle: handle, Spec: spec})

	go func() {
		// Give the pumps a bounded grace to reach EOF before reaping. For a
		// normally exiting child the pipe write ends close on exit and both
		// pumps finish in microseconds, so this preserves every buffered
		// line. The ceiling keeps the reaper hang-resistant in the
		// degenerate case where a grandchild inherited a pipe end and
		// outlives the worker — there we accept the pre-existing tail loss
		// rather than never reaping the session.
		pumpsDone := make(chan struct{})
		go func() { pumps.Wait(); close(pumpsDone) }()
		select {
		case <-pumpsDone:
		case <-time.After(pumpDrainGrace):
		}
		err := cmd.Wait()

		s.mu.Lock()
		entry := s.sessions[spec.SessionID]
		if entry != ss {
			s.mu.Unlock()
			cancel()
			return
		}
		delete(s.sessions, spec.SessionID)
		switch {
		case err == nil:
			entry.handle.State = SessionCompleted
		case ctx.Err() != nil:
			entry.handle.State = SessionTerminated
		default:
			entry.handle.State = SessionFailed
		}
		final := entry.handle
		s.mu.Unlock()
		entry.cancel()

		s.emit(SessionEvent{Kind: SessionEventEnded, Handle: final, Spec: spec, ExitErr: err})
	}()

	return &handle, nil
}

// StopSession terminates a single in-flight session by id without
// disturbing its siblings or pausing the spawner (unlike Drain, which
// stops accepting and force-kills the whole pool). It looks up the stored
// spawnedSession, invokes its stored cancel (the same process-teardown
// machinery Drain uses for stragglers), and deletes it from s.sessions so
// the capacity slot frees immediately — even if the underlying provider is
// wedged and the cmd.Wait goroutine has not yet observed the exit.
//
// This is the hard out-of-band leg of the deterministic cancel wire
// (Guard 3): the platform's fast in-band path is the heartbeat stop signal
// (immediate LostOwnership), and this gives head-of-line-blocking
// isolation — one stuck session can be killed without a pool drain.
//
// Returns false when no session with the given id is currently active
// (already exited or never spawned); true when a session was found and its
// cancel invoked + slot freed. Deleting under the lock makes the slot-free
// race-free against AcceptWork's capacity check; cancel() is invoked after
// the lock is released so a slow process teardown never blocks AcceptWork.
//
// StopSession itself emits the SessionEventEnded lifecycle event (state
// SessionTerminated) immediately, rather than relying on the spawn
// goroutine's cmd.Wait → emit: a wedged provider may never let cmd.Wait
// return, so deferring the event would strand listeners (and the slot-free
// signal) indefinitely. The spawn goroutine's own cmd.Wait → delete/emit is
// a no-op once StopSession has removed or replaced the exact entry — it compares
// pointer ownership — so the event fires exactly once and a double-free is
// impossible.
func (s *WorkerSpawner) StopSession(id string) bool {
	s.mu.Lock()
	ss := s.sessions[id]
	if ss == nil {
		s.mu.Unlock()
		return false
	}
	delete(s.sessions, id)
	ss.handle.State = SessionTerminated
	final := ss.handle
	s.mu.Unlock()

	// Cancel outside the lock: the stored cancel tears the child process
	// down (context cancellation → process kill), which can take longer
	// than we want to hold s.mu (AcceptWork / capacity checks contend on
	// it). The slot is already freed above.
	ss.cancel()
	s.emit(SessionEvent{Kind: SessionEventEnded, Handle: final, Spec: ss.spec})
	return true
}

// ForceKillSession sends SIGKILL to the daemon-created process group for one
// owned session. It is deliberately separate from StopSession so normal local
// stop paths retain their existing context-cancellation behavior.
//
// A session seen by this daemon but already reaped is an idempotent success.
// A never-seen id is rejected: that distinction prevents a mutation routed to
// the wrong daemon from being falsely acknowledged. The bounded history is
// process-local because ownership does not survive a daemon restart.
func (s *WorkerSpawner) ForceKillSession(id string) error {
	s.mu.Lock()
	ss := s.sessions[id]
	_, owned := s.sessionHistory[id]
	s.mu.Unlock()
	if ss == nil {
		if owned {
			return nil
		}
		return fmt.Errorf("session %q is not owned by this daemon", id)
	}

	if err := s.killProcessGroup(ss.cmd); err != nil && !errors.Is(err, errSessionProcessExited) {
		return fmt.Errorf("SIGKILL session %q process group: %w", id, err)
	}

	// The wait goroutine may have reaped the process after the signal. Only the
	// goroutine that still owns this exact registry entry emits the terminal
	// event; duplicates and races are successful no-ops.
	s.mu.Lock()
	if s.sessions[id] != ss {
		s.mu.Unlock()
		return nil
	}
	delete(s.sessions, id)
	ss.handle.State = SessionTerminated
	final := ss.handle
	s.mu.Unlock()

	ss.cancel()
	s.emit(SessionEvent{Kind: SessionEventEnded, Handle: final, Spec: ss.spec})
	return nil
}

func (s *WorkerSpawner) rememberSessionLocked(id string) {
	if _, exists := s.sessionHistory[id]; exists {
		return
	}
	s.sessionHistory[id] = struct{}{}
	s.sessionHistoryOrder = append(s.sessionHistoryOrder, id)
	if len(s.sessionHistoryOrder) <= sessionHistoryLimit {
		return
	}
	oldest := s.sessionHistoryOrder[0]
	s.sessionHistoryOrder = s.sessionHistoryOrder[1:]
	delete(s.sessionHistory, oldest)
}

// Drain waits for all in-flight sessions to exit, then resolves. After
// timeout, remaining sessions receive SIGTERM via context cancellation and
// the function returns an error indicating how many were forcibly stopped.
func (s *WorkerSpawner) Drain(timeout time.Duration) error {
	s.mu.Lock()
	s.accepting = false
	if len(s.sessions)+len(s.pendingSessions) == 0 {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	deadline := time.Now().Add(timeout)
	pollInterval := 100 * time.Millisecond
	for {
		s.mu.Lock()
		n := len(s.sessions) + len(s.pendingSessions)
		s.mu.Unlock()
		if n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}

	// Force-stop remaining started sessions. Pending pre-spawn hooks retain their
	// own cancellation contract; report them explicitly rather than pretending
	// the drain completed.
	s.mu.Lock()
	stragglers := make([]*spawnedSession, 0, len(s.sessions))
	for _, ss := range s.sessions {
		stragglers = append(stragglers, ss)
	}
	pending := len(s.pendingSessions)
	s.mu.Unlock()
	for _, ss := range stragglers {
		ss.cancel()
	}
	if len(stragglers) > 0 || pending > 0 {
		return fmt.Errorf(
			"drain timeout — sigterm sent to %d session(s); %d spawn(s) still pending",
			len(stragglers),
			pending,
		)
	}
	return nil
}

// composeEnv flattens the merged env into the os.Environ() form expected by
// exec.Cmd.Env.
func composeEnv(parts ...map[string]string) []string {
	merged := map[string]string{}
	for _, p := range parts {
		for k, v := range p {
			merged[k] = v
		}
	}
	parent := os.Environ()
	out := make([]string, 0, len(parent)+len(merged))
	out = append(out, parent...)
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// PrefixWriterFunc adapts a function to PrefixedWriter.
type PrefixWriterFunc func(workerID, line string)

// WriteWorkerLine implements PrefixedWriter.
func (f PrefixWriterFunc) WriteWorkerLine(workerID, line string) { f(workerID, line) }
