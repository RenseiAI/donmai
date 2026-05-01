package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

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

	mu        sync.Mutex
	sessions  map[string]*spawnedSession
	accepting bool

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

// AcceptWork validates the spec, spawns a worker, and returns its handle.
func (s *WorkerSpawner) AcceptWork(spec SessionSpec) (*SessionHandle, error) {
	s.mu.Lock()
	if !s.accepting {
		s.mu.Unlock()
		return nil, errors.New("not accepting new work (paused or draining)")
	}
	if len(s.sessions) >= s.opts.MaxConcurrentSessions {
		s.mu.Unlock()
		return nil, fmt.Errorf("at capacity (%d/%d sessions)", len(s.sessions), s.opts.MaxConcurrentSessions)
	}
	project := s.findProjectLocked(spec.Repository)
	if project == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("repository %q is not in the project allowlist", spec.Repository)
	}
	s.mu.Unlock()

	return s.spawn(spec, project)
}

func (s *WorkerSpawner) findProjectLocked(repository string) *ProjectConfig {
	for i := range s.opts.Projects {
		p := &s.opts.Projects[i]
		// The platform sends spec.Repository as the Linear project slug
		// (e.g. "smoke-alpha"), which doesn't match the GitHub repo name
		// in p.Repository (e.g. ".../rensei-smokes-alpha"). Match by p.ID
		// as well so operators can express the link via the allowlist
		// entry's id. (REN-NEW)
		if p.Repository == repository ||
			p.ID == repository ||
			strings.HasSuffix(repository, "/"+p.Repository) ||
			strings.HasSuffix(p.Repository, "/"+repository) {
			return p
		}
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
		// test stub. (REN-1461.)
		slog.Warn("worker spawner: WorkerCommand not set; using /bin/sh test stub (sessions exit immediately — set WorkerCommand or deploy a binary that resolves via os.Executable)",
			"sessionId", spec.SessionID,
		)
		command = []string{"/bin/sh", "-c", `printf 'session-started:%s\n' "$RENSEI_SESSION_ID"; exit 0`}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec
	cmd.Env = composeEnv(s.opts.BaseEnv, spec.Env, map[string]string{
		"RENSEI_SESSION_ID": spec.SessionID,
		"RENSEI_REPOSITORY": spec.Repository,
		"RENSEI_REF":        spec.Ref,
		"RENSEI_PROJECT_ID": project.ID,
	})

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
		SessionID:  spec.SessionID,
		PID:        pid,
		AcceptedAt: s.opts.Now().UTC().Format(time.RFC3339),
		State:      SessionRunning,
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
