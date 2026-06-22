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
	Projects              []ProjectConfig
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

// WorkerSpawner manages the lifecycle of worker child processes.
type WorkerSpawner struct {
	opts SpawnerOptions

	mu            sync.Mutex
	sessions      map[string]*spawnedSession
	accepting     bool
	extraProjects []ProjectConfig // satellite/additional org projects; never clobbered by SetProjects

	listenersMu sync.Mutex
	listeners   []func(SessionEvent)
}

type spawnedSession struct {
	handle SessionHandle
	cmd    *exec.Cmd
	cancel context.CancelFunc
	spec   SessionSpec
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
	return &WorkerSpawner{
		opts:      opts,
		sessions:  make(map[string]*spawnedSession),
		accepting: true,
	}
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

// ActiveCount returns the number of in-flight sessions.
func (s *WorkerSpawner) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
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
		if project := s.findProjectLocked(ss.spec.Repository); project != nil {
			summary.ProjectID = project.ID
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
	cp := make([]ProjectConfig, len(projects))
	copy(cp, projects)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.Projects = cp
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
	}
}

// isDuplicateLocked reports whether candidate is already represented in either
// the base or extra project sets. Must be called with s.mu held.
func (s *WorkerSpawner) isDuplicateLocked(candidate ProjectConfig) bool {
	for _, existing := range s.opts.Projects {
		if candidate.ID != "" && candidate.ID == existing.ID {
			return true
		}
		if candidate.Repository != "" && candidate.Repository == existing.Repository {
			return true
		}
	}
	for _, existing := range s.extraProjects {
		if candidate.ID != "" && candidate.ID == existing.ID {
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

// AcceptWork validates the spec, spawns a worker, and returns its handle.
func (s *WorkerSpawner) AcceptWork(spec SessionSpec) (*SessionHandle, error) {
	s.mu.Lock()
	if !s.accepting {
		s.mu.Unlock()
		return nil, errors.New("not accepting new work (paused or draining)")
	}
	if active, capacity := len(s.sessions), s.opts.MaxConcurrentSessions; active >= capacity {
		s.mu.Unlock()
		// Snapshot the counts BEFORE unlocking — formatting them after
		// release races with spawn.func1's delete on s.sessions when an
		// in-flight session exits. (Caught under -race during Wave 11
		// S5 work; pre-existing.)
		return nil, fmt.Errorf("at capacity (%d/%d sessions)", active, capacity)
	}
	project := s.findProjectLocked(spec.Repository)
	if project == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("repository %q is not in the project allowlist", spec.Repository)
	}
	s.mu.Unlock()

	return s.spawn(spec, project)
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
	command := s.opts.WorkerCommand
	if len(command) == 0 {
		// Stub worker — exits 0 immediately. Production code paths
		// should always have WorkerCommand set (see daemon.go's
		// defaultWorkerCommand). Surfacing this at warn level so
		// operators notice when the daemon has fallen back to the
		// test stub.
		slog.Warn("worker spawner: WorkerCommand not set; using /bin/sh test stub (sessions exit immediately — set WorkerCommand or deploy a binary that resolves via os.Executable)",
			"sessionId", spec.SessionID,
		)
		command = []string{"/bin/sh", "-c", `printf 'session-started:%s\n' "$DONMAI_SESSION_ID"; exit 0`}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec
	cmd.Env = composeEnv(s.opts.BaseEnv, spec.Env, map[string]string{
		"DONMAI_SESSION_ID": spec.SessionID,
		"DONMAI_REPOSITORY": spec.Repository,
		"DONMAI_REF":        spec.Ref,
		"DONMAI_PROJECT_ID": project.ID,
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
	if s.opts.OnPreSpawn != nil {
		next, hookErr := s.opts.OnPreSpawn(spec, cmd.Env)
		if hookErr != nil {
			cancel()
			return nil, fmt.Errorf("pre-spawn hook: %w", hookErr)
		}
		if next != nil {
			cmd.Env = next
		}
	}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start worker: %w", err)
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
		handle: handle,
		cmd:    cmd,
		cancel: cancel,
		spec:   spec,
	}

	s.mu.Lock()
	s.sessions[spec.SessionID] = ss
	s.mu.Unlock()

	// Stream stdout / stderr with worker-tagged prefix.
	if s.opts.StdoutPrefixWriter != nil {
		go pumpLines(stdout, spec.SessionID, s.opts.StdoutPrefixWriter)
	} else {
		go drain(stdout)
	}
	if s.opts.StderrPrefixWriter != nil {
		go pumpLines(stderr, spec.SessionID, s.opts.StderrPrefixWriter)
	} else {
		go drain(stderr)
	}

	s.emit(SessionEvent{Kind: SessionEventStarted, Handle: handle, Spec: spec})

	go func() {
		err := cmd.Wait()

		s.mu.Lock()
		entry := s.sessions[spec.SessionID]
		if entry == nil {
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
// a no-op once StopSession has removed the entry — it guards on a nil lookup
// — so the event fires exactly once and a double-free is impossible.
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

// Drain waits for all in-flight sessions to exit, then resolves. After
// timeout, remaining sessions receive SIGTERM via context cancellation and
// the function returns an error indicating how many were forcibly stopped.
func (s *WorkerSpawner) Drain(timeout time.Duration) error {
	s.mu.Lock()
	s.accepting = false
	if len(s.sessions) == 0 {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	deadline := time.Now().Add(timeout)
	pollInterval := 100 * time.Millisecond
	for {
		s.mu.Lock()
		n := len(s.sessions)
		s.mu.Unlock()
		if n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}

	// Force-stop remaining sessions.
	s.mu.Lock()
	stragglers := make([]*spawnedSession, 0, len(s.sessions))
	for _, ss := range s.sessions {
		stragglers = append(stragglers, ss)
	}
	s.mu.Unlock()
	for _, ss := range stragglers {
		ss.cancel()
	}
	if len(stragglers) > 0 {
		return fmt.Errorf("drain timeout — sigterm sent to %d session(s)", len(stragglers))
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
