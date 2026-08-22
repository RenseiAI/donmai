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
	EnabledProjectIDs []string
	// ProjectAdmissionMode is the machine owner's standing consent decision:
	// ProjectAdmissionModeEnumerated (default) admits only EnabledProjectIDs;
	// ProjectAdmissionModeAllRouted admits any project the orchestrator routes
	// here. Empty reads as enumerated, so an embedder that never sets it keeps
	// today's behaviour exactly.
	ProjectAdmissionMode  string
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

	// ExternalOccupancy reports capacity slots held on this host by sessions the
	// spawner does not parent — per-session shims it launched, adopted, or
	// quarantined (ADR-2026-08-17 §D7).
	//
	// Admission has to see them, not just the heartbeat. A shim-owned session
	// never enters this spawner's own registry by design, so a host that advertised
	// its occupancy honestly and then admitted against its direct-child count
	// alone would still double-book itself — accepting work it has no core to run.
	// Nil reads as zero, which is correct for an embedder with no shims.
	ExternalOccupancy func() int

	// ShimOwns, when non-nil, is the stable ownership selector consulted before
	// ShimSpawn or OnPreSpawn. False goes directly to the ordinary child path;
	// true requires ShimSpawn to return a handle or error and may never silently
	// fall through. The selector must depend only on immutable daemon config and
	// the supplied spec so ownership cannot change between selection and launch.
	//
	// Nil preserves the original ShimSpawn contract for external embedders whose
	// callback makes the combined decision. The built-in daemon always supplies
	// this selector. Keeping selection ahead of OnPreSpawn is load-bearing:
	// otherwise a direct fallback invokes credential/resource acquisition twice.
	ShimOwns func(spec SessionSpec) bool

	// ShimSpawn, when non-nil, is consulted for selected sessions BEFORE the
	// ordinary direct-child spawn (ADR-2026-08-17 §D1/§D11).
	//
	// With ShimOwns nil it returns a handle when the session was launched under
	// per-session shim ownership, and (nil, nil) when it was not. With ShimOwns
	// non-nil, a selected launch returning (nil, nil) is an invariant violation
	// and fails closed instead of downgrading to direct ownership.
	//
	// A non-nil error fails the accept, fail-closed. A session the daemon
	// intended to launch under a shim must not silently become a
	// daemon-parented child that dies with the next upgrade.
	ShimSpawn func(spec SessionSpec, project ProjectConfig, env []string) (*SessionHandle, error)

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
// line with the worker tag and must return promptly. After a terminal pipe-close
// deadline, a blocked writer is detached from session ownership so it cannot
// retain the worker slot indefinitely.
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

// pumpDrainGrace bounds the post-exit interval during which the session reaper
// lets stdout/stderr pumps drain naturally. It is armed only after cmd.Wait has
// observed the direct child exit and a descendant may still hold an inherited
// write end; an ordinary running worker never owns this deadline.
const pumpDrainGrace = 10 * time.Second

// pumpCloseJoinGrace bounds the final join after the terminal reaper closes its
// pipe readers. A PrefixedWriter is external code and can block after a reader
// has been closed; such a writer must not retain the terminal session or its
// capacity forever. This interval is reachable only after direct-child exit and
// the output-drain deadline, never while an ordinary worker is running.
const pumpCloseJoinGrace = 250 * time.Millisecond

// pumpDrainTimer is the minimal stoppable timer the terminal pump-drain path
// needs. Keeping it narrow lets lifecycle tests advance the deadline without
// waiting for wall-clock time.
type pumpDrainTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type realPumpDrainTimer struct {
	timer *time.Timer
}

func (t *realPumpDrainTimer) C() <-chan time.Time { return t.timer.C }
func (t *realPumpDrainTimer) Stop() bool          { return t.timer.Stop() }

// WorkerSpawner manages the lifecycle of worker child processes.
type WorkerSpawner struct {
	opts SpawnerOptions

	mu                     sync.Mutex
	sessions               map[string]*spawnedSession
	sessionHistory         map[string]struct{}
	sessionHistoryOrder    []string
	accepting              bool
	spawnReservations      map[string]struct{}
	extraProjects          []ProjectConfig // satellite/additional org projects; never clobbered by SetProjects
	extraEnabledProjectIDs map[string]struct{}
	terminateProcessGroup  func(*exec.Cmd) error
	killProcessGroup       func(*exec.Cmd) error
	waitProcessGroup       func(*exec.Cmd, time.Duration) processGroupWaitResult
	startCommand           func(*exec.Cmd) error

	// These per-spawner lifecycle seams are nil in production. Tests inject a
	// controllable drain timer and synchronize at the running and fully-joined
	// pump boundaries without waiting for a wall-clock deadline.
	pumpDrainTimerFactory func(time.Duration) pumpDrainTimer
	reaperRunning         func()
	// runningPhaseContextDone may replace the cancellation arm immediately
	// before the direct-child running select. It is nil in production.
	runningPhaseContextDone func(context.Context, <-chan error, <-chan struct{}) <-chan struct{}
	afterPumpsJoined        func()
	// afterPumpDrainDeadlineSelected blocks the narrow terminal-drain edge after
	// its timer wins but before the reaper claims process-group termination. It
	// is nil in production.
	afterPumpDrainDeadlineSelected func()

	// drainBeforeContextSnapshot is a deterministic test seam for the narrow
	// deadline edge where the final owner clears the last entry while DrainContext
	// is selecting ctx.Done. It is nil in production.
	drainBeforeContextSnapshot func()

	listenersMu sync.Mutex
	listeners   []func(SessionEvent)
}

const sessionHistoryLimit = 4096

// groupTerminationOwner atomically resolves whether operator cancellation or
// natural reaping owns process-group termination for one live generation.
type groupTerminationOwner uint8

const (
	groupTerminationOpen groupTerminationOwner = iota
	groupTerminationOperator
	groupTerminationNatural
)

type spawnedSession struct {
	handle                SessionHandle
	cmd                   *exec.Cmd
	cancel                context.CancelFunc
	released              chan struct{}
	spec                  SessionSpec
	stopRequested         bool                  // guarded by WorkerSpawner.mu
	forceKillRequested    bool                  // guarded by WorkerSpawner.mu
	groupTerminationOwner groupTerminationOwner // guarded by WorkerSpawner.mu
	terminal              bool                  // guarded by WorkerSpawner.mu; Ended delivery owns this generation
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
	opts.ProjectAdmissionMode = normalizeProjectAdmissionMode(opts.ProjectAdmissionMode)
	return &WorkerSpawner{
		opts:                   opts,
		sessions:               make(map[string]*spawnedSession),
		spawnReservations:      make(map[string]struct{}),
		sessionHistory:         make(map[string]struct{}),
		accepting:              true,
		extraEnabledProjectIDs: make(map[string]struct{}),
		terminateProcessGroup:  terminateSessionProcessGroup,
		killProcessGroup:       killSessionProcessGroup,
		waitProcessGroup:       waitSessionProcessGroup,
		startCommand:           func(cmd *exec.Cmd) error { return cmd.Start() },
		pumpDrainTimerFactory: func(d time.Duration) pumpDrainTimer {
			return &realPumpDrainTimer{timer: time.NewTimer(d)}
		},
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
	return len(s.sessions)
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
	active = len(s.sessions)
	for _, ss := range s.sessions {
		switch ss.spec.Mode {
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

// SetProjectAdmissionMode replaces the standing consent mode. Called by the
// yaml watcher so an operator who flips projectAdmissionMode gets the new
// semantics on the next reload — no daemon restart.
func (s *WorkerSpawner) SetProjectAdmissionMode(mode string) {
	normalized := normalizeProjectAdmissionMode(mode)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.ProjectAdmissionMode = normalized
}

// ProjectAdmissionMode returns the spawner's current normalized consent mode.
// Reported to the orchestrator so it can tell "this host has not enabled your
// project" apart from "this host admits anything routed to it".
func (s *WorkerSpawner) ProjectAdmissionMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return normalizeProjectAdmissionMode(s.opts.ProjectAdmissionMode)
}

// AddProjects appends additional project configurations (e.g. satellite-org
// projects in a shared-spawner topology) to the spawner's allowlist. The new
// entries are held in a separate slice from the base set managed by
// SetProjects, so a subsequent yaml-watcher reload that calls SetProjects does
// NOT evict them.
//
// Deduplication is by ProjectConfig.ID and ProjectConfig.Repository: a
// candidate is skipped when it matches an existing entry's ID and Repository
// exactly, or when the two share a non-empty Repository AND at least one of
// them carries no ID (an unattributed repository-only entry always collapses
// into whatever else already claims that repository). This still prevents
// double-entries when AddProjects is called repeatedly for the same org's
// project list.
//
// Two entries that share a non-empty Repository but carry DIFFERENT non-empty
// IDs are NOT deduplicated — both are admitted. A monorepo can host more than
// one project, and each must load independently so AcceptWork can resolve it
// by (projectID, repository).
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
//
// A repository-only match (same non-empty Repository, different ID pair) is a
// duplicate ONLY when the candidate carries no ID, the existing entry carries
// no ID, or the two IDs are equal. Two entries with distinct non-empty IDs
// that happen to share a repository — a monorepo hosting more than one
// project — are NOT duplicates of each other; both must load.
func (s *WorkerSpawner) isDuplicateLocked(candidate ProjectConfig) bool {
	matches := func(existing ProjectConfig) bool {
		if candidate.ID == existing.ID && candidate.Repository == existing.Repository {
			return true
		}
		if candidate.Repository == "" || candidate.Repository != existing.Repository {
			return false
		}
		return candidate.ID == "" || existing.ID == "" || candidate.ID == existing.ID
	}
	for _, existing := range s.opts.Projects {
		if matches(existing) {
			return true
		}
	}
	for _, existing := range s.extraProjects {
		if matches(existing) {
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

// AcceptWork validates the spec, spawns a worker, and returns its handle.
func (s *WorkerSpawner) AcceptWork(spec SessionSpec) (*SessionHandle, error) {
	s.mu.Lock()
	if !s.accepting {
		s.mu.Unlock()
		return nil, errors.New("not accepting new work (paused or draining)")
	}
	if _, active := s.sessions[spec.SessionID]; active {
		s.mu.Unlock()
		return nil, fmt.Errorf("session %q is already active", spec.SessionID)
	}
	if _, reserved := s.spawnReservations[spec.SessionID]; reserved {
		s.mu.Unlock()
		return nil, fmt.Errorf("session %q is already being started", spec.SessionID)
	}
	if active, capacity := len(s.sessions)+len(s.spawnReservations)+s.externalOccupancy(), s.opts.MaxConcurrentSessions; active >= capacity {
		s.mu.Unlock()
		// Snapshot the counts BEFORE unlocking — formatting them after
		// release races with spawn.func1's delete on s.sessions when an
		// in-flight session exits. (Caught under -race during Wave 11
		// S5 work; pre-existing.)
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
	s.spawnReservations[spec.SessionID] = struct{}{}
	s.mu.Unlock()

	return s.spawn(spec, project)
}

// externalOccupancy is the slot count held by sessions this spawner does not
// parent. Zero when no reporter is wired.
func (s *WorkerSpawner) externalOccupancy() int {
	if s.opts.ExternalOccupancy == nil {
		return 0
	}
	if n := s.opts.ExternalOccupancy(); n > 0 {
		return n
	}
	return 0
}

// spawnThroughShim offers the session to the shim launch path.
//
// handled=false means this session is not shim-owned and the caller continues
// with the direct spawn. handled=true means ownership moved to a shim and the
// spawner keeps NO process bookkeeping for it — no exec.Cmd, no pipes, no
// reaper entry — because every one of those would be a second owner of a session
// whose whole point is having exactly one (§D1).
func (s *WorkerSpawner) spawnThroughShim(spec SessionSpec, project *ProjectConfig) (*SessionHandle, bool, error) {
	if s.opts.ShimSpawn == nil {
		return nil, false, nil
	}
	env := composeEnv(s.opts.BaseEnv, spec.Env, map[string]string{
		"DONMAI_SESSION_ID":    spec.SessionID,
		"DONMAI_REPOSITORY":    spec.Repository,
		"DONMAI_REPOSITORY_ID": spec.RepositoryID,
		"DONMAI_REF":           spec.Ref,
		"DONMAI_PROJECT_ID":    project.ID,
	})
	// OnPreSpawn is the credential rail, and a shim-backed session needs it for
	// exactly the same reason a direct one does: the harness cannot start without
	// the credentials the hook resolves. Skipping it for shim sessions would make
	// shim ownership silently downgrade the security posture of the spawn.
	if s.opts.OnPreSpawn != nil {
		next, hookErr := s.opts.OnPreSpawn(spec, env)
		if hookErr != nil {
			return nil, true, fmt.Errorf("pre-spawn hook: %w", hookErr)
		}
		if next != nil {
			env = next
		}
	}
	handle, err := s.opts.ShimSpawn(spec, *project, env)
	if err != nil {
		if s.opts.OnSpawnAborted != nil && s.opts.OnPreSpawn != nil {
			s.opts.OnSpawnAborted(spec, err)
		}
		return nil, true, err
	}
	if handle == nil {
		return nil, false, nil
	}
	return handle, true, nil
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
			// Distinguish "this project was never loaded by this spawner" from
			// "it was loaded, but not for this repository" — conflating the two
			// under one repository-flavored message sends whoever is debugging
			// straight at the wrong layer (the repository config) when the real
			// gap is the project's admission/config entry itself.
			if !s.hasProjectEntryLocked(spec.ProjectID) {
				return nil, fmt.Errorf("project %q is not admitted or loaded by this spawner", spec.ProjectID)
			}
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
	// all-routed: the machine owner consented once to "whatever my org routes
	// here", so there is no per-project list to consult. A blank id is still
	// refused — it is a malformed spec, not a routing decision.
	if id != "" && normalizeProjectAdmissionMode(s.opts.ProjectAdmissionMode) == ProjectAdmissionModeAllRouted {
		return true
	}
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

// hasProjectEntryLocked reports whether any entry with the given project ID
// exists in either the base or extra project sets, independent of whether its
// repository matches any particular spec. Must be called with s.mu held.
func (s *WorkerSpawner) hasProjectEntryLocked(id string) bool {
	for i := range s.opts.Projects {
		if s.opts.Projects[i].ID == id {
			return true
		}
	}
	for i := range s.extraProjects {
		if s.extraProjects[i].ID == id {
			return true
		}
	}
	return false
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

func (s *WorkerSpawner) spawn(spec SessionSpec, project *ProjectConfig) (*SessionHandle, error) {
	reservationTransferred := false
	defer func() {
		if reservationTransferred {
			return
		}
		s.mu.Lock()
		delete(s.spawnReservations, spec.SessionID)
		s.mu.Unlock()
	}()

	// §D1: shim ownership is decided before any daemon-owned process exists. Once
	// the direct path has created a pipe or an exec.Cmd, the daemon is already
	// the owner this design exists to stop it from being.
	shimSelected := s.opts.ShimSpawn != nil && (s.opts.ShimOwns == nil || s.opts.ShimOwns(spec))
	if shimSelected {
		handle, handled, err := s.spawnThroughShim(spec, project)
		if s.opts.ShimOwns != nil && !handled && err == nil {
			err = errors.New("session shim selector chose ownership but launcher returned no handle")
			if s.opts.OnSpawnAborted != nil && s.opts.OnPreSpawn != nil {
				s.opts.OnSpawnAborted(spec, err)
			}
		}
		if err != nil {
			return nil, err
		}
		if handled {
			return handle, nil
		}
		// Legacy combined-decision callback declined ownership. Continue to
		// the direct path exactly as before.
	}

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

	// Keep cancellation as a lifecycle signal only. CommandContext's default
	// cancellation hard-kills the direct child before the process group receives
	// its cooperative SIGTERM window, so use Command and let the reaper own the
	// TERM -> bounded grace -> KILL escalation explicitly.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command(command[0], command[1:]...) //nolint:gosec
	configureSessionProcessGroup(cmd)
	cmd.Env = composeEnv(s.opts.BaseEnv, spec.Env, map[string]string{
		"DONMAI_SESSION_ID":    spec.SessionID,
		"DONMAI_REPOSITORY":    spec.Repository,
		"DONMAI_REPOSITORY_ID": spec.RepositoryID,
		"DONMAI_REF":           spec.Ref,
		"DONMAI_PROJECT_ID":    project.ID,
	})

	// The daemon, rather than os/exec, owns these read ends. That lets a waiter
	// observe direct-child exit without closing a pump mid-buffer, while still
	// allowing the terminal reaper to close inherited-pipe readers on deadline.
	// Create them before OnPreSpawn so a setup failure occurs before the hook can
	// acquire resources whose rollback ownership it would transfer.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create worker stdout pipe: %w", err)
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		cancel()
		return nil, fmt.Errorf("create worker stderr pipe: %w", err)
	}
	closeWorkerPipes := func() {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

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
			closeWorkerPipes()
			cancel()
			return nil, fmt.Errorf("pre-spawn hook: %w", hookErr)
		}
		preSpawnOwnsCleanup = true
		if next != nil {
			cmd.Env = next
		}
	}

	if err := s.startCommand(cmd); err != nil {
		closeWorkerPipes()
		cancel()
		startErr := fmt.Errorf("start worker: %w", err)
		if preSpawnOwnsCleanup && s.opts.OnSpawnAborted != nil {
			s.opts.OnSpawnAborted(spec, startErr)
		}
		return nil, startErr
	}
	// Child-side duplicates are now installed. Keeping either daemon write end
	// open would prevent EOF after the worker exits, so release both immediately.
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()

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
		handle:   handle,
		cmd:      cmd,
		cancel:   cancel,
		released: make(chan struct{}),
		spec:     spec,
	}

	s.mu.Lock()
	// Transfer this exact admission from its pre-spawn reservation to the live
	// registry under one lock. AcceptWork reserves ids before leaving the lock,
	// so a duplicate cannot overwrite this session while cmd.Start is in flight.
	delete(s.spawnReservations, spec.SessionID)
	s.sessions[spec.SessionID] = ss
	s.rememberSessionLocked(spec.SessionID)
	reservationTransferred = true
	s.mu.Unlock()

	// Stream stdout / stderr with worker-tagged prefix. The daemon owns the read
	// ends, so cmd.Wait can observe direct-child exit without closing a pump and
	// discarding buffered output. Each completion fits in this buffered channel
	// while the direct child is still running; no bridge goroutine is needed.
	pumpDone := make(chan struct{}, 2)
	pump := func(reader *os.File, writer PrefixedWriter) {
		go func() {
			defer func() {
				_ = reader.Close()
				pumpDone <- struct{}{}
			}()
			if writer != nil {
				pumpLines(reader, spec.SessionID, writer)
				return
			}
			drain(reader)
		}()
	}
	pump(stdout, s.opts.StdoutPrefixWriter)
	pump(stderr, s.opts.StderrPrefixWriter)

	// Wait observes only direct-process termination. Terminal classification,
	// process-group cleanup, event delivery, and SessionID release remain owned
	// by the reaper below.
	waitResult := make(chan error, 1)
	go func() { waitResult <- cmd.Wait() }()

	s.emit(SessionEvent{Kind: SessionEventStarted, Handle: handle, Spec: spec})

	go func() {
		// This seam marks the running phase only after both pumps and the direct
		// waiter are installed. Production leaves it nil.
		if s.reaperRunning != nil {
			s.reaperRunning()
		}

		pumpsRemaining := 2
		cancelled := false
		ctxDone := ctx.Done()
		handleCancellation := func() {
			// Cancellation has already sent SIGTERM under the exact session lock.
			// Give the complete group its cooperative window before a TERM-ignoring
			// tree is escalated to SIGKILL, whether the leader is still running or
			// is in its terminal output-drain phase.
			cancelled = true
			ctxDone = nil
			if outcome := s.waitProcessGroup(cmd, sessionTerminationGrace); outcome != processGroupGone {
				s.forceKillGeneration(ss)
			}
		}
		reconcileCancellation := func() {
			// A select gives no priority to a ready cancellation arm. Reconcile the
			// exact session context after every running-phase result so child exit
			// or pump EOF cannot bypass the TERM grace already requested by Stop or
			// Drain.
			if !cancelled && ctx.Err() != nil {
				handleCancellation()
			}
		}
		if s.runningPhaseContextDone != nil {
			ctxDone = s.runningPhaseContextDone(ctx, waitResult, pumpDone)
		}

		// A running worker has no output-drain deadline. Pump completion merely
		// records that a stream reached EOF; direct-child termination is the only
		// transition that can begin terminal cleanup.
		var err error
		for directChildRunning := true; directChildRunning; {
			select {
			case err = <-waitResult:
				directChildRunning = false
			case <-ctxDone:
				handleCancellation()
			case <-pumpDone:
				pumpsRemaining--
			}
			reconcileCancellation()
		}

		// Account for EOF notifications that arrived concurrently with cmd.Wait
		// before deciding whether a terminal drain timer is needed.
	drainCompletedPumps:
		for pumpsRemaining > 0 {
			select {
			case <-pumpDone:
				pumpsRemaining--
			default:
				break drainCompletedPumps
			}
		}

		pumpsJoined := true
		if pumpsRemaining > 0 {
			timerFactory := s.pumpDrainTimerFactory
			if timerFactory == nil {
				timerFactory = func(d time.Duration) pumpDrainTimer {
					return &realPumpDrainTimer{timer: time.NewTimer(d)}
				}
			}
			timer := timerFactory(pumpDrainGrace)
			for pumpsRemaining > 0 {
				select {
				case <-pumpDone:
					pumpsRemaining--
				case <-ctxDone:
					handleCancellation()
				case <-timer.C():
					if s.afterPumpDrainDeadlineSelected != nil {
						s.afterPumpDrainDeadlineSelected()
					}
					switch s.claimReaperGroupTermination(ss) {
					case reaperGroupTerminationOperatorGrace:
						if !cancelled {
							handleCancellation()
						}
					case reaperGroupTerminationNatural:
						// The leader has already exited, so reclaim a surviving group
						// without rewriting an exit-0 session as an operator termination.
						if killErr := s.reapRemainingProcessGroup(ss); killErr != nil && !errors.Is(killErr, errSessionProcessExited) {
							slog.Warn("worker process group cleanup signal failed", "sessionId", spec.SessionID, "err", killErr)
						}
					}
					// Descendants can retain inherited write ends after the group
					// signal races with their exit. Closing daemon-owned reads makes
					// pipe-read pumps finish. A PrefixedWriter can itself be blocked,
					// though, so only retain terminal ownership for a bounded final join.
					_ = stdout.Close()
					_ = stderr.Close()
					closeTimer := timerFactory(pumpCloseJoinGrace)
					for pumpsRemaining > 0 {
						select {
						case <-pumpDone:
							pumpsRemaining--
						case <-ctxDone:
							handleCancellation()
						case <-closeTimer.C():
							// Consume notifications already ready at the deadline before
							// allowing a non-cooperative external writer to outlive this
							// terminal generation. pumpDone is sized for both later sends.
							for pumpsRemaining > 0 {
								select {
								case <-pumpDone:
									pumpsRemaining--
								default:
									pumpsJoined = false
									slog.Warn("worker output pump did not stop after pipe close", "sessionId", spec.SessionID, "remainingPumps", pumpsRemaining)
									pumpsRemaining = 0
								}
							}
						}
					}
					_ = closeTimer.Stop()
				}
			}
			_ = timer.Stop()
		}

		// Responsive pumps are joined before terminal ownership moves to
		// process-group cleanup and synchronous Ended delivery. A blocked external
		// writer may outlive the bounded post-close join, but it cannot retain the
		// session registry entry or capacity.
		if pumpsJoined && s.afterPumpsJoined != nil {
			s.afterPumpsJoined()
		}

		// cmd.Wait proves only that the direct child exited. Claim process-group
		// termination before the zero-duration probe so a late StopSession cannot
		// race natural cleanup into an immediate KILL.
		groupTermination := s.claimReaperGroupTermination(ss)
		if groupTermination == reaperGroupTerminationOperatorGrace && !cancelled {
			handleCancellation()
		}
		// A normal leader exit must not turn a surviving, pipe-silent child into a
		// capacity leak. The zero-duration probe performs one classification without
		// adding a grace period; operator-owned generations have already received
		// their cooperative window and stale generations issue no group signal.
		if groupTermination != reaperGroupTerminationStale && s.waitProcessGroup(cmd, 0) != processGroupGone && groupTermination == reaperGroupTerminationNatural {
			if killErr := s.reapRemainingProcessGroup(ss); killErr != nil && !errors.Is(killErr, errSessionProcessExited) {
				slog.Warn("worker process group cleanup signal failed", "sessionId", spec.SessionID, "err", killErr)
			}
		}
		// A killed group can still be reported as EPERM or zombie-only. Bound that
		// observation and release the terminal generation after logging the precise
		// outcome; an unobservable group must not retain a SessionID forever.
		if outcome := s.waitProcessGroup(cmd, processGroupPostWaitGrace); outcome != processGroupGone {
			slog.Warn("worker process group did not disappear after direct-child reap", "sessionId", spec.SessionID, "outcome", outcome)
		}

		event, reaped := s.reapSession(spec.SessionID, ss, err, cancelled)
		if !reaped {
			cancel()
			return
		}
		ss.cancel()
		s.emitAndReleaseSession(spec.SessionID, ss, event)
	}()

	return &handle, nil
}

// StopSession requests cooperative termination for one session without releasing
// its admission key. It returns true when this spawner still owns the exact
// generation, including terminal listener delivery and naturally owned cleanup.
// Those acknowledged states do not send a signal, cancel the session, change its
// terminal classification, or transfer termination ownership. It returns false
// only when the ID was never registered or has already been released. The reaper
// remains the sole terminal owner: it waits for the complete dedicated process
// group, synchronously notifies listeners, then releases the exact registry
// entry. Keeping that order prevents same-ID replacement and stale Ended cleanup
// from crossing generations.
func (s *WorkerSpawner) StopSession(id string) bool {
	s.mu.Lock()
	ss := s.sessions[id]
	if ss == nil {
		s.mu.Unlock()
		return false
	}
	if ss.terminal || ss.groupTerminationOwner == groupTerminationNatural {
		// These owners still retain the registry entry and capacity, but a late
		// Stop must acknowledge them without stealing ownership or mutating state.
		s.mu.Unlock()
		return true
	}
	if ss.groupTerminationOwner == groupTerminationOperator {
		s.mu.Unlock()
		return true
	}
	// Signal initiation is intentionally serialized under the exact generation:
	// the reaper cannot claim natural process-group cleanup between ownership
	// validation and SIGTERM. A repeated Stop remains a successful request
	// without a second signal.
	ss.groupTerminationOwner = groupTerminationOperator
	ss.stopRequested = true
	_ = s.terminateProcessGroup(ss.cmd)
	s.mu.Unlock()
	ss.cancel()
	return true
}

type reaperGroupTermination uint8

const (
	reaperGroupTerminationStale reaperGroupTermination = iota
	reaperGroupTerminationOperatorGrace
	reaperGroupTerminationOperatorForced
	reaperGroupTerminationNatural
)

// claimReaperGroupTermination resolves process-group cleanup for the exact
// live generation. Natural cleanup becomes visible under the same lock that
// accepts operator cancellation, so only one path can own the first KILL.
func (s *WorkerSpawner) claimReaperGroupTermination(expected *spawnedSession) reaperGroupTermination {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected == nil || expected.terminal || s.sessions[expected.spec.SessionID] != expected {
		return reaperGroupTerminationStale
	}
	switch expected.groupTerminationOwner {
	case groupTerminationOperator:
		if expected.forceKillRequested {
			return reaperGroupTerminationOperatorForced
		}
		return reaperGroupTerminationOperatorGrace
	case groupTerminationNatural:
		return reaperGroupTerminationNatural
	case groupTerminationOpen:
		expected.groupTerminationOwner = groupTerminationNatural
		return reaperGroupTerminationNatural
	default:
		return reaperGroupTerminationStale
	}
}

// forceKillGeneration escalates an operator-owned exact live generation while
// holding the same lock that serializes terminal classification and registry
// release.
func (s *WorkerSpawner) forceKillGeneration(expected *spawnedSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected == nil || expected.terminal || s.sessions[expected.spec.SessionID] != expected || expected.groupTerminationOwner != groupTerminationOperator || expected.forceKillRequested {
		return
	}
	expected.stopRequested = true
	if err := s.killProcessGroup(expected.cmd); err == nil || errors.Is(err, errSessionProcessExited) {
		expected.forceKillRequested = true
	}
}

// reapRemainingProcessGroup reclaims descendants left after cmd.Wait without
// rewriting the direct child's completed outcome as an operator stop. It holds
// the exact-generation lock across the signal so a released ID cannot receive a
// stale signal from its predecessor.
func (s *WorkerSpawner) reapRemainingProcessGroup(expected *spawnedSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected == nil || expected.terminal || s.sessions[expected.spec.SessionID] != expected || expected.groupTerminationOwner != groupTerminationNatural || expected.forceKillRequested {
		return nil
	}
	if err := s.killProcessGroup(expected.cmd); err != nil {
		return err
	}
	expected.forceKillRequested = true
	return nil
}

// ForceKillSession handles a replayable SIGKILL mutation for one daemon-owned
// session. For a live open or operator-owned generation it attempts SIGKILL and
// returns an error only for an actual signal failure. Naturally owned, terminal,
// and already force-killed generations return nil without another signal or
// cancellation. Released IDs retained in bounded session history likewise return
// nil to acknowledge a mutation replay; a never-owned or history-evicted ID
// returns "not owned". Thus nil means this daemon recognizes the mutation as
// successfully handled, not that this invocation sent SIGKILL. Terminal
// ownership remains with the reaper throughout.
func (s *WorkerSpawner) ForceKillSession(id string) error {
	s.mu.Lock()
	ss := s.sessions[id]
	_, owned := s.sessionHistory[id]
	if ss == nil || ss.terminal {
		s.mu.Unlock()
		if owned {
			return nil
		}
		return fmt.Errorf("session %q is not owned by this daemon", id)
	}
	if ss.groupTerminationOwner == groupTerminationNatural || ss.forceKillRequested {
		s.mu.Unlock()
		return nil
	}

	// Serialize a successful kill acknowledgement with the session generation.
	// A duplicate mutation racing the reaper must be idempotent rather than
	// observing a transient leader/process-group state as a failed command.
	claimedOpen := ss.groupTerminationOwner == groupTerminationOpen
	if claimedOpen {
		ss.groupTerminationOwner = groupTerminationOperator
	}
	ss.stopRequested = true
	if err := s.killProcessGroup(ss.cmd); err != nil && !errors.Is(err, errSessionProcessExited) {
		if claimedOpen {
			ss.groupTerminationOwner = groupTerminationOpen
			ss.stopRequested = false
		}
		s.mu.Unlock()
		return fmt.Errorf("SIGKILL session %q process group: %w", id, err)
	}
	ss.forceKillRequested = true
	s.mu.Unlock()
	ss.cancel()
	return nil
}

// emitAndReleaseSession retains exact ownership through synchronous listener
// delivery. Listener cleanup is therefore generation-safe: it cannot observe a
// replacement under the same SessionID until the terminal event is complete.
func (s *WorkerSpawner) emitAndReleaseSession(id string, expected *spawnedSession, event SessionEvent) {
	s.emit(event)
	s.mu.Lock()
	if s.sessions[id] == expected {
		delete(s.sessions, id)
		close(expected.released)
	}
	s.mu.Unlock()
}

// sessionRelease returns a signal for the exact currently owned generation.
// It closes only after terminal listeners return and the generation is removed
// from the active registry, so lifecycle tests can synchronize with release
// without inferring it from the earlier Ended event.
func (s *WorkerSpawner) sessionRelease(id string) (<-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[id]
	if ss == nil {
		return nil, false
	}
	return ss.released, true
}

// reapSession removes expected from the live registry and builds its terminal
// event. A reaper must own the exact session it removes: even though admission
// rejects duplicate IDs, a stale waiter must never be able to delete a newer
// entry if a caller or future maintenance path replaces an ID.
func (s *WorkerSpawner) reapSession(id string, expected *spawnedSession, err error, cancelled bool) (SessionEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[id] != expected {
		return SessionEvent{}, false
	}
	switch {
	case err == nil && !expected.stopRequested:
		expected.handle.State = SessionCompleted
	case expected.stopRequested || cancelled:
		expected.handle.State = SessionTerminated
	default:
		expected.handle.State = SessionFailed
	}
	// Retain the exact map entry through synchronous Ended delivery, but make
	// this generation terminal before listeners run. Stop/ForceKill can therefore
	// never signal a process after classification while release is pending.
	expected.terminal = true
	return SessionEvent{Kind: SessionEventEnded, Handle: expected.handle, Spec: expected.spec, ExitErr: err}, true
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

// DrainIncompleteError reports a drain that could not prove every admitted
// spawn has either registered or completed its synchronous abort cleanup.
// Callers should use errors.As and inspect the counts rather than parsing Error.
type DrainIncompleteError struct {
	Cause             error
	ActiveSessions    int
	SpawnReservations int
}

func (e *DrainIncompleteError) Error() string {
	return fmt.Sprintf("drain incomplete: %v; %d active session(s) observed; %d spawn reservation(s) still pending", e.Cause, e.ActiveSessions, e.SpawnReservations)
}

// Unwrap exposes the cancellation reason that ended this drain attempt.
func (e *DrainIncompleteError) Unwrap() error { return e.Cause }

// Drain waits for every admitted spawn to either register or abort and for all
// in-flight sessions to exit. It is retained for compatibility; callers that
// need an independently bounded retry should use DrainContext.
func (s *WorkerSpawner) Drain(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.DrainContext(ctx)
}

// DrainContext waits for every admitted spawn to either register or abort and
// for all in-flight sessions to exit. When ctx ends, registered sessions are
// cancelled outside the spawner lock, while pre-start reservations remain owned
// by their spawn path until hooks and synchronous abort cleanup finish.
func (s *WorkerSpawner) DrainContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	s.accepting = false
	if len(s.sessions) == 0 && len(s.spawnReservations) == 0 {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// A ticker avoids holding the mutex while waiting, and context cancellation
	// makes an explicit cancellation deterministic instead of polling a clock.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		pending := len(s.sessions) + len(s.spawnReservations)
		s.mu.Unlock()
		if pending == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			// A final locked snapshot is required here: the last owner may have
			// completed after the optimistic pending check but before this select
			// chose ctx.Done. Returning an incomplete error in that case would
			// poison Daemon.Stop's cached result despite a fully drained pool.
			if s.drainBeforeContextSnapshot != nil {
				s.drainBeforeContextSnapshot()
			}
			s.mu.Lock()
			stragglers := make([]*spawnedSession, 0, len(s.sessions))
			for _, ss := range s.sessions {
				stragglers = append(stragglers, ss)
			}
			reservations := len(s.spawnReservations)
			if len(stragglers) == 0 && reservations == 0 {
				s.mu.Unlock()
				return nil
			}
			incomplete := &DrainIncompleteError{
				Cause:             ctx.Err(),
				ActiveSessions:    len(stragglers),
				SpawnReservations: reservations,
			}
			s.mu.Unlock()

			// Request TERM outside the snapshot lock. StopSession serializes each
			// signal with the exact live generation, then the reaper grants the
			// bounded cooperative window before SIGKILL escalation.
			for _, ss := range stragglers {
				s.StopSession(ss.spec.SessionID)
			}
			return incomplete
		case <-ticker.C:
		}
	}
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
