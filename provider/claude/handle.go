package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// ErrInjectInFlight is returned by Handle.Inject when a previous Inject
// has spawned a `claude --resume` subprocess that has not yet exited.
// The provider serializes inject calls (one --resume subprocess at a
// time) to keep the event stream in causal order — the legacy TS
// provider's in-process SDK injection had the same constraint via the
// underlying message queue. Callers that need to chain multiple
// injects should consume events from Events() until they observe a
// terminal ResultEvent for the previous inject before calling again.
var ErrInjectInFlight = errors.New("provider/claude: Inject already in flight; wait for previous --resume subprocess to exit")

// ErrSessionNotReady is returned by Handle.Inject when called before
// the parent subprocess has emitted its system.init event (which
// carries the session id required for `claude --resume <id>`).
// Callers should consume events from Events() until they observe an
// agent.InitEvent before calling Inject. In typical runner usage this
// race is impossible — the runner reads events synchronously and only
// dispatches inject in response to a downstream signal — but the API
// contract documents the failure mode explicitly.
var ErrSessionNotReady = errors.New("provider/claude: session id not yet captured; wait for InitEvent before calling Inject")

// stopGracePeriod is the deadline between SIGTERM and SIGKILL when
// Stop is called or ctx is cancelled. Mirrors the legacy SDK's
// abortController behavior (5s grace).
const stopGracePeriod = 5 * time.Second

// eventBufferSize is the buffered capacity of the events channel.
// Sized to absorb a burst of fan-out events (one assistant line can
// produce many text + tool_use events) without backpressuring the
// stdout reader. The runner is expected to drain promptly.
const eventBufferSize = 64

// stderrBufferSize caps how many bytes of stderr we retain for
// post-mortem diagnostics on unexpected exits. Matches the legacy
// SDK's 2000-byte stderr-tail bound (claude-provider.ts).
const stderrBufferSize = 8 * 1024

// Handle is the agent.Handle implementation backed by a `claude`
// subprocess (the parent), with optional follow-up `claude --resume`
// subprocesses spawned by Inject() for between-turn user-message
// injection.
//
// Lifecycle:
//
//  1. spawn() creates the parent cmd, sets stdin/stdout/stderr pipes,
//     and launches the stdout-reading goroutine.
//  2. Events() returns a channel that goroutine — and any subsequent
//     Inject() resume goroutine — writes to via sendEvent.
//  3. The parent goroutine drains stdout, mapping each line to events,
//     posts them on the channel, and exits on EOF or scan error. It
//     does NOT close the channel: Inject() may follow up with another
//     subprocess whose events must reach the same consumer.
//  4. Inject(text) spawns `claude --resume <session-id> -p <text>`,
//     forwards its JSONL stream onto the same channel via sendEvent,
//     and blocks until that subprocess exits. Inject calls are
//     serialized (one --resume subprocess at a time); see
//     ErrInjectInFlight.
//  5. Stop() (or ctx cancellation) signals shutdown to all goroutines,
//     SIGTERMs the parent process group, awaits termination, removes
//     the MCP tmpfile, and finally closes the events channel exactly
//     once.
type Handle struct {
	binary        string
	mcpConfigPath string
	cmd           *exec.Cmd
	events        chan agent.Event
	logger        *slog.Logger

	stdoutPipe io.ReadCloser
	stderrPipe io.ReadCloser
	stderrBuf  *boundedBuffer

	// sessionID is captured from the first InitEvent and read
	// concurrently by SessionID(). Use atomic.Pointer to keep reads
	// lock-free.
	sessionID atomic.Pointer[string]

	// stopped guards Stop() against double-cancel.
	stopOnce sync.Once
	stopErr  error

	// injectInFlight serializes Inject() calls. CAS-flipped to true for
	// the duration of an inject — the --resume subprocess must finish
	// before another inject is allowed so events stay in causal order
	// on the shared parent channel. See ErrInjectInFlight.
	injectInFlight atomic.Bool

	// shutdown is closed by Stop() (and by ctx cancellation handling)
	// to unblock any goroutine that is currently sending to the events
	// channel. Producers select on shutdown vs the channel send so
	// they bail out cleanly when Stop races with a slow consumer.
	shutdown chan struct{}

	// eventsClosed guards closeEvents against double-close. Set to
	// true under eventsMu before close(events) runs.
	eventsClosed atomic.Bool

	// eventsMu serializes sendEvent / closeEvents access to the events
	// channel. RLock held while sending; Lock held while closing.
	// Producers are short-lived (one channel send each) so contention
	// is negligible. The matching shutdown signal in sendEvent makes a
	// stuck send observable.
	eventsMu sync.RWMutex

	// done is closed after the parent reader goroutine exits and
	// cmd.Wait returns, signalling Stop() that the parent subprocess
	// has terminated.
	done chan struct{}

	// waitErr holds the cmd.Wait error (kept for diagnostics; not
	// returned directly).
	waitErr atomic.Pointer[error]

	// parentTerminal is set to true once the parent subprocess emits a
	// terminal ResultEvent. Used by readStdout to suppress the
	// "spawn_no_result" synthetic when the parent finished cleanly.
	parentTerminal atomic.Bool
}

// sendEvent multiplexes one event onto the public events channel.
// Safe for concurrent use across the parent reader goroutine and any
// Inject-spawned resume reader goroutine. Drops the event silently
// when the channel has already been closed by Stop() — the consumer
// has signalled disinterest, so the right thing is to drop rather
// than panic on send-to-closed-channel.
func (h *Handle) sendEvent(ev agent.Event) {
	h.eventsMu.RLock()
	defer h.eventsMu.RUnlock()
	if h.eventsClosed.Load() {
		return
	}
	select {
	case h.events <- ev:
	case <-h.shutdown:
		// Stop is in progress; drop rather than block on a slow
		// consumer that will never drain.
	}
}

// closeEvents closes the events channel exactly once, setting the
// guard flag first so concurrent sendEvent callers see it. Idempotent.
func (h *Handle) closeEvents() {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	if h.eventsClosed.Load() {
		return
	}
	h.eventsClosed.Store(true)
	close(h.events)
}

// spawn is the internal Provider.Spawn implementation shared with
// (the future) Resume. It builds the CLI args, writes the MCP config
// tmpfile, starts the subprocess, and returns a fully-wired Handle.
//
// On any failure prior to the subprocess being started, spawn cleans
// up the MCP tmpfile and returns an error wrapping
// agent.ErrSpawnFailed.
func (p *Provider) spawn(ctx context.Context, spec agent.Spec, resumeSessionID string) (*Handle, error) {
	mcpPath, err := writeMCPConfig(spec.MCPServers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}

	argv, stdinPrompt := buildArgs(spec, mcpPath, resumeSessionID)

	// nolint:gosec // p.binary is resolved at provider construction
	// from exec.LookPath; argv values come from a typed agent.Spec
	// and a closed set of CLI flags. The shell-out target is the
	// trusted claude binary.
	cmd := exec.CommandContext(ctx, p.binary, argv...)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = composeEnv(os.Environ(), spec.Env)
	// Place the child in its own process group so we can signal the
	// whole group atomically (the claude binary may fork shell
	// helpers; sending SIGTERM to the leader alone leaves
	// stdout-inheriting orphans that keep the pipe open).
	configureProcessGroup(cmd)
	// Override exec.CommandContext's default leader-only kill with
	// a process-group SIGKILL on ctx cancellation. Without this,
	// the leader dies but stdout-inheriting forks keep the events
	// reader blocked.
	cmd.Cancel = func() error {
		signalProcessGroup(cmd, syscall.SIGKILL)
		return nil
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = removeMCPConfig(mcpPath)
		return nil, fmt.Errorf("%w: stdin pipe: %v", agent.ErrSpawnFailed, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = removeMCPConfig(mcpPath)
		_ = stdin.Close()
		return nil, fmt.Errorf("%w: stdout pipe: %v", agent.ErrSpawnFailed, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = removeMCPConfig(mcpPath)
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("%w: stderr pipe: %v", agent.ErrSpawnFailed, err)
	}

	if err := cmd.Start(); err != nil {
		_ = removeMCPConfig(mcpPath)
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("%w: cmd start: %v", agent.ErrSpawnFailed, err)
	}

	if spec.OnProcessSpawned != nil && cmd.Process != nil {
		spec.OnProcessSpawned(cmd.Process.Pid)
	}

	stderrBuf := newBoundedBuffer(stderrBufferSize)

	h := &Handle{
		binary:        p.binary,
		mcpConfigPath: mcpPath,
		cmd:           cmd,
		events:        make(chan agent.Event, eventBufferSize),
		logger:        slog.With("provider", "claude", "pid", cmd.Process.Pid),
		stdoutPipe:    stdout,
		stderrPipe:    stderr,
		stderrBuf:     stderrBuf,
		shutdown:      make(chan struct{}),
		done:          make(chan struct{}),
	}

	// Launch the prompt-writer in a goroutine. The CLI consumes the
	// prompt then waits for stdin EOF before producing the result
	// line, so we close stdin once the prompt is written.
	go writePromptStdin(stdin, stdinPrompt, h.logger)

	// Launch the stderr drainer.
	go drainStderr(stderr, stderrBuf, h.logger)

	// Launch the stdout reader.
	go h.readStdout()

	// Watch the spawn ctx: when it's cancelled, the subprocess is
	// already being killed by exec.CommandContext. We additionally
	// trigger a soft shutdown so the events channel closes — between-
	// turn injection (Inject) is no longer meaningful when the parent
	// session has been cancelled. The goroutine owns the Handle
	// lifetime; the spawn ctx is the right cancellation source here.
	//nolint:gosec // G118: spawn ctx is the lifecycle source we want
	go h.watchCtx(ctx)

	return h, nil
}

// watchCtx waits for the spawn ctx to fire and then runs Stop with a
// background ctx so the events channel closes and the MCP tmpfile is
// cleaned up. The parent-terminated signal (h.done) is intentionally
// NOT a watchCtx exit case: parent EOF without ctx cancellation means
// the runner may still call Inject() to spawn a --resume subprocess
// for another turn, so we must keep watching the ctx until shutdown
// is initiated by Stop or by ctx cancellation.
func (h *Handle) watchCtx(ctx context.Context) {
	select {
	case <-ctx.Done():
		// Stop is idempotent (sync.Once-guarded) so racing with a
		// caller-initiated Stop is safe. Use a background ctx with a
		// generous deadline so the MCP cleanup always runs.
		stopCtx, cancel := context.WithTimeout(context.Background(), stopGracePeriod+2*time.Second)
		defer cancel()
		_ = h.Stop(stopCtx)
	case <-h.shutdown:
		// Stop already initiated; nothing more to do.
	}
}

// SessionID returns the provider-native session id captured from the
// first InitEvent. Empty until InitEvent fires; safe for concurrent
// reads.
func (h *Handle) SessionID() string {
	if v := h.sessionID.Load(); v != nil {
		return *v
	}
	return ""
}

// Events returns the read-only event channel.
//
// Closed by Stop() after the parent subprocess and any in-flight
// Inject() resume subprocess have terminated. The channel is also
// closed when the spawn ctx is cancelled (a watcher goroutine
// triggers Stop in that case).
//
// Note: pre-F.2.3-cap-flip the parent reader goroutine closed the
// channel on EOF. That changed when SupportsMessageInjection was
// flipped to true — Inject's --resume subprocess streams onto the
// same channel, so close ownership shifted to Stop.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject sends a follow-up user message into the session by spawning
// a fresh `claude --resume <session-id> -p <text>` subprocess and
// streaming its JSONL output onto the parent Handle's events channel.
//
// This is between-turn injection — the parent subprocess (started by
// Spawn) processes its single headless turn and exits; subsequent
// Inject calls each spawn one --resume subprocess that contributes
// another turn's events. Same semantic level as the legacy TS Agent
// SDK and the future Go-native option C upgrade in REN-1451.
//
// Concurrency policy: sequential. While one Inject's --resume
// subprocess is still running, a concurrent Inject returns
// ErrInjectInFlight. Callers are expected to consume events from
// Events() until they observe a terminal ResultEvent for the prior
// inject before calling again. (Legacy TS provider's in-process SDK
// imposed the same constraint via the underlying message queue;
// streaming events out-of-order would break the consumer's
// turn-by-turn accounting.)
//
// Pre-conditions:
//
//   - The parent subprocess must have emitted its system.init event
//     (so the session id is captured). Otherwise: ErrSessionNotReady.
//     In typical runner usage this race is impossible — the runner
//     reads events synchronously and only injects in response to
//     observed downstream signals.
//
// Cancellation:
//
//   - ctx.Done() during the inject sends SIGTERM to the --resume
//     subprocess group, then SIGKILL after stopGracePeriod, then
//     returns ctx.Err.
//
// Returns:
//
//   - nil when the --resume subprocess exits cleanly.
//   - ErrInjectInFlight if a prior inject is still running.
//   - ErrSessionNotReady if no InitEvent has been observed.
//   - A wrapped error from cmd.Start / cmd.Wait on subprocess failure.
//   - ctx.Err on cancellation.
//
// The MCP config tmpfile from the parent spawn is reused for resume
// subprocesses so MCP tools remain available across turns. Other
// per-spawn state (Cwd, Env, allowedTools, etc.) is propagated via
// the resume argv constructed from a minimal Spec that carries only
// the inject prompt — the CLI's --resume restores the rest from the
// captured session.
func (h *Handle) Inject(ctx context.Context, text string) error {
	sid := h.SessionID()
	if sid == "" {
		return fmt.Errorf("provider/claude: Inject: %w", ErrSessionNotReady)
	}
	if !h.injectInFlight.CompareAndSwap(false, true) {
		return fmt.Errorf("provider/claude: Inject: %w", ErrInjectInFlight)
	}
	defer h.injectInFlight.Store(false)

	// Build a minimal spec for the resume subprocess. The CLI's
	// --resume restores model/tools/cwd/env from the captured session,
	// so we only carry the new prompt + the resume id through buildArgs.
	argv, stdinPrompt := buildArgs(agent.Spec{Prompt: text}, "", sid)

	// nolint:gosec // h.binary was resolved via exec.LookPath at
	// provider construction; argv values come from buildArgs and a
	// closed set of CLI flags. Same trust model as parent spawn.
	cmd := exec.CommandContext(ctx, h.binary, argv...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error {
		signalProcessGroup(cmd, syscall.SIGKILL)
		return nil
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("provider/claude: Inject: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("provider/claude: Inject: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("provider/claude: Inject: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return fmt.Errorf("provider/claude: Inject: cmd start: %w", err)
	}

	// Drain stdin (the prompt) and stderr asynchronously, mirroring
	// the parent spawn pattern.
	go writePromptStdin(stdin, stdinPrompt, h.logger)
	// Resume stderr is dropped — bounded-buffer diagnostics are kept
	// only for the parent spawn. If a resume subprocess fails, the
	// non-zero exit / scan error path surfaces a wrapped error.
	go func() {
		defer func() { _ = stderr.Close() }()
		_, _ = io.Copy(io.Discard, stderr)
	}()

	// Stream the resume subprocess's JSONL onto the parent events
	// channel. Reuse the same scanner config (4MiB max line) as the
	// parent reader.
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			raw := scanner.Bytes()
			if len(raw) == 0 {
				continue
			}
			line := append([]byte(nil), raw...)
			for _, ev := range mapLine(line) {
				if ev == nil {
					continue
				}
				// Resume reuses the same session id; the new
				// system.init line will carry the same id, so we
				// don't update h.sessionID here. Drop our own ctx
				// cancellation through sendEvent's shutdown signal
				// so we don't block on a slow consumer after Stop.
				h.sendEvent(ev)
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			h.sendEvent(agent.ErrorEvent{
				Message: fmt.Sprintf("provider/claude: Inject: stdout scan: %v", err),
				Code:    "inject_stdout_scan",
			})
		}
	}()

	// Wait for the resume subprocess to exit, honoring ctx.Done().
	waitErrCh := make(chan error, 1)
	go func() {
		<-scanDone
		waitErrCh <- cmd.Wait()
	}()

	select {
	case err := <-waitErrCh:
		if err != nil {
			return fmt.Errorf("provider/claude: Inject: cmd wait: %w", err)
		}
		return nil
	case <-ctx.Done():
		// Send SIGTERM, give grace period, then SIGKILL.
		if cmd.Process != nil {
			signalProcessGroup(cmd, syscall.SIGTERM)
		}
		grace := time.NewTimer(stopGracePeriod)
		defer grace.Stop()
		select {
		case <-waitErrCh:
		case <-grace.C:
			if cmd.Process != nil {
				signalProcessGroup(cmd, syscall.SIGKILL)
			}
			<-waitErrCh
		}
		return ctx.Err()
	}
}

// Stop aborts the session.
//
// Idempotent: subsequent calls return the same recorded error and do
// not re-signal the subprocess. Safe to call after the events channel
// has closed (returns nil in that case).
//
// Stop sends SIGTERM, waits up to stopGracePeriod for graceful exit,
// then sends SIGKILL. ctx cancellation overrides the grace period.
// The MCP tmpfile is removed before returning.
func (h *Handle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		h.stopErr = h.doStop(ctx)
	})
	return h.stopErr
}

func (h *Handle) doStop(ctx context.Context) error {
	// Close the shutdown signal early so any goroutine blocked on a
	// sendEvent can bail out cleanly. Channel close is the
	// terminal-state broadcast; it is idempotent because Stop is
	// guarded by stopOnce.
	close(h.shutdown)

	// Always close the events channel and remove the MCP tmpfile
	// before returning, even if subprocess teardown errored.
	defer h.closeEvents()
	defer func() { _ = removeMCPConfig(h.mcpConfigPath) }()

	// If the parent subprocess has already exited, the events
	// channel has nothing more to receive — skip signaling.
	select {
	case <-h.done:
		return nil
	default:
	}

	if h.cmd != nil && h.cmd.Process != nil {
		// SIGTERM the whole process group first so any forked
		// helpers terminate too.
		signalProcessGroup(h.cmd, syscall.SIGTERM)
	}

	// Wait for graceful exit, but no longer than grace period or
	// caller's ctx, whichever fires first.
	timer := time.NewTimer(stopGracePeriod)
	defer timer.Stop()

	select {
	case <-h.done:
		// Graceful exit observed.
	case <-timer.C:
		if h.cmd != nil && h.cmd.Process != nil {
			signalProcessGroup(h.cmd, syscall.SIGKILL)
		}
		<-h.done
	case <-ctx.Done():
		if h.cmd != nil && h.cmd.Process != nil {
			signalProcessGroup(h.cmd, syscall.SIGKILL)
		}
		<-h.done
	}
	return nil
}

// readStdout is the per-handle goroutine that drains the parent
// subprocess stdout, decodes each line via mapLine, and forwards
// events to the channel via sendEvent.
//
// It does NOT close h.events: between-turn injection (Inject()) may
// follow up with --resume subprocesses whose events flow through the
// same channel. Stop() owns the close after all subprocesses have
// terminated.
//
// On EOF without a terminal ResultEvent it emits a synthetic
// ErrorEvent (code "spawn_no_result") so the runner observes the
// failure rather than waiting silently.
func (h *Handle) readStdout() {
	defer close(h.done)
	defer func() {
		// cmd.Wait must be called to release process resources.
		err := h.cmd.Wait()
		if err != nil {
			h.waitErr.Store(&err)
		}
	}()

	// Force-close the stdout pipe when shutdown is signaled, so the
	// scanner unblocks even if SIGKILL of the subprocess group does
	// not immediately propagate a pipe close to us. Without this, a
	// process group SIGKILL can race with the bufio scanner's blocked
	// read() — observed empirically on macOS under -race load: the
	// kernel queues the signal but does not always close the inherited
	// stdout fd before our scanner is descheduled. Force-closing the
	// pipe makes the next read return immediately, breaking the loop
	// regardless of OS-level signal delivery timing.
	pipeCloseDone := make(chan struct{})
	go func() {
		defer close(pipeCloseDone)
		select {
		case <-h.done:
			// Reader exited normally; pipe was closed by cmd.Wait.
		case <-h.shutdown:
			// Shutdown initiated; force-close stdout to unblock scanner.
			_ = h.stdoutPipe.Close()
		}
	}()
	defer func() { <-pipeCloseDone }()

	scanner := bufio.NewScanner(h.stdoutPipe)
	// Each JSONL line can be large (an assistant message with
	// multiple content blocks plus the system.init line which
	// embeds the available-tools list). Bump the limit accordingly.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	terminal := false
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		// Copy: scanner reuses its buffer.
		line := append([]byte(nil), raw...)
		for _, ev := range mapLine(line) {
			if ev == nil {
				continue
			}
			if init, ok := ev.(agent.InitEvent); ok && init.SessionID != "" {
				id := init.SessionID
				h.sessionID.Store(&id)
			}
			if _, ok := ev.(agent.ResultEvent); ok {
				terminal = true
			}
			h.sendEvent(ev)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		h.sendEvent(agent.ErrorEvent{
			Message: fmt.Sprintf("provider/claude: stdout scan: %v", err),
			Code:    "stdout_scan",
		})
		return
	}
	if terminal {
		h.parentTerminal.Store(true)
		return
	}
	stderrTail := h.stderrBuf.String()
	msg := "claude exited without terminal result"
	if stderrTail != "" {
		msg = fmt.Sprintf("%s: stderr=%s", msg, stderrTail)
	}
	h.sendEvent(agent.ErrorEvent{
		Message: msg,
		Code:    "spawn_no_result",
	})
}

// writePromptStdin writes the prompt to the child's stdin and closes
// it (the CLI reads to EOF before producing the result line). Errors
// are logged but not surfaced — if the child died early the stdout
// reader will emit the spawn_no_result ErrorEvent.
func writePromptStdin(stdin io.WriteCloser, prompt string, logger *slog.Logger) {
	defer func() { _ = stdin.Close() }()
	if prompt == "" {
		return
	}
	if _, err := io.WriteString(stdin, prompt); err != nil {
		logger.Debug("write prompt to stdin", "err", err)
	}
}

// drainStderr copies subprocess stderr into the bounded buffer for
// post-mortem diagnostics. Reads to EOF; never blocks the caller.
func drainStderr(r io.ReadCloser, buf *boundedBuffer, logger *slog.Logger) {
	defer func() { _ = r.Close() }()
	if _, err := io.Copy(buf, r); err != nil && !errors.Is(err, io.EOF) {
		logger.Debug("drain stderr", "err", err)
	}
}

// boundedBuffer accumulates the last N bytes written, dropping the
// oldest data once the limit is reached. Goroutine-safe; used to keep
// a stderr tail for spawn_no_result diagnostics without unbounded
// memory growth.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit, buf: make([]byte, 0, limit)}
}

// Write implements io.Writer. Always returns len(p), nil — drops
// oldest bytes when the buffer would exceed limit.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.limit {
		// If the incoming chunk is larger than the buffer, retain
		// only its tail.
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	overflow := (len(b.buf) + len(p)) - b.limit
	if overflow > 0 {
		// Drop the oldest `overflow` bytes.
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:len(b.buf)-overflow]
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// String returns a snapshot of the current buffer contents.
func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
