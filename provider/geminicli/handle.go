package geminicli

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

	"github.com/RenseiAI/donmai/agent"
)

// stopGracePeriod is the deadline between SIGTERM and SIGKILL when
// Stop is called or ctx is cancelled. Mirrors the claude provider's
// 5s grace period.
const stopGracePeriod = 5 * time.Second

// eventBufferSize is the buffered capacity of the events channel.
// Sized to absorb a burst of fan-out events without backpressuring
// the stdout reader. The runner is expected to drain promptly.
const eventBufferSize = 64

// stderrBufferSize caps how many bytes of stderr we retain for
// post-mortem diagnostics on unexpected exits.
const stderrBufferSize = 8 * 1024

// Handle is the agent.Handle implementation backed by a `gemini`
// subprocess.
//
// Lifecycle:
//
//  1. spawn() creates the subprocess, wires stdin/stdout/stderr pipes,
//     and launches the stdout-reading goroutine.
//  2. Events() returns a channel that goroutine writes to via sendEvent.
//  3. The goroutine drains stdout, mapping each line to events, posts
//     them on the channel, and exits on EOF or scan error.
//  4. Inject() is not supported (SupportsMessageInjection=false); it
//     returns agent.ErrUnsupported.
//  5. Stop() (or ctx cancellation) signals shutdown to all goroutines,
//     SIGTERMs the process group, awaits termination, removes the
//     settings.json file, and closes the events channel exactly once.
type Handle struct {
	binary           string
	settingsFilePath string // absolute path to .gemini/settings.json (or "")
	cwd              string
	cmd              *exec.Cmd
	events           chan agent.Event
	logger           *slog.Logger

	stdoutPipe io.ReadCloser
	stderrPipe io.ReadCloser
	stderrBuf  *boundedBuffer

	// sessionID is captured from the first InitEvent.
	sessionID atomic.Pointer[string]

	// stopped guards Stop() against double-cancel.
	stopOnce sync.Once
	stopErr  error

	// shutdown is closed by Stop() to unblock any goroutine that is
	// currently sending to the events channel.
	shutdown chan struct{}

	// eventsClosed guards closeEvents against double-close.
	eventsClosed atomic.Bool

	// eventsMu serializes sendEvent / closeEvents access to the events
	// channel. RLock held while sending; Lock held while closing.
	eventsMu sync.RWMutex

	// done is closed after the stdout reader goroutine exits and
	// cmd.Wait returns, signalling Stop() that the subprocess has terminated.
	done chan struct{}

	// waitErr holds the cmd.Wait error (kept for diagnostics).
	waitErr atomic.Pointer[error]

	// parentTerminal is set to true once the subprocess emits a terminal
	// ResultEvent. Used by readStdout to suppress the "spawn_no_result"
	// synthetic ErrorEvent when the session ended cleanly.
	parentTerminal atomic.Bool
}

// sendEvent multiplexes one event onto the public events channel.
// Safe for concurrent use. Drops the event silently when the channel
// has already been closed by Stop().
func (h *Handle) sendEvent(ev agent.Event) {
	h.eventsMu.RLock()
	defer h.eventsMu.RUnlock()
	if h.eventsClosed.Load() {
		return
	}
	select {
	case h.events <- ev:
	case <-h.shutdown:
		// Stop is in progress; drop rather than block on a slow consumer.
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

// spawn is the internal Provider.Spawn implementation.
func (p *Provider) spawn(ctx context.Context, spec agent.Spec) (*Handle, error) {
	// Write .gemini/settings.json with MCP server config (if any).
	settingsPath, err := writeGeminiSettings(spec.Cwd, spec.MCPServers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}

	argv, stdinPrompt := buildArgs(spec)
	return spawnRaw(ctx, p.binary, argv, stdinPrompt, settingsPath, spec.Cwd, spec.Env, spec.OnProcessSpawned)
}

// spawnRaw is the low-level subprocess spawn. It creates the exec.Command,
// wires up stdin/stdout/stderr pipes, starts the subprocess, and returns
// a fully-wired Handle whose stdout reader goroutine decodes gemini CLI JSONL.
//
// On any failure prior to the subprocess being started, spawnRaw removes
// the settingsFilePath and returns an error wrapping agent.ErrSpawnFailed.
func spawnRaw(
	ctx context.Context,
	binary string,
	argv []string,
	stdinPrompt string,
	settingsFilePath string,
	cwd string,
	env map[string]string,
	onProcessSpawned func(pid int),
) (*Handle, error) {
	//nolint:gosec // binary is resolved via exec.LookPath at provider construction;
	// argv values come from buildArgs (a closed set of CLI flags) and agent.Spec.
	cmd := exec.CommandContext(ctx, binary, argv...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = composeEnv(os.Environ(), env)
	// Place the child in its own process group so we can signal the whole
	// group atomically — the gemini Node process may fork MCP server children
	// and shell helpers that inherit stdout.
	configureProcessGroup(cmd)
	// Override exec.CommandContext's default leader-only kill with a
	// process-group SIGKILL on ctx cancellation.
	cmd.Cancel = func() error {
		signalProcessGroup(cmd, syscall.SIGKILL)
		return nil
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = removeGeminiSettings(settingsFilePath)
		return nil, fmt.Errorf("%w: stdin pipe: %v", agent.ErrSpawnFailed, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = removeGeminiSettings(settingsFilePath)
		_ = stdin.Close()
		return nil, fmt.Errorf("%w: stdout pipe: %v", agent.ErrSpawnFailed, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = removeGeminiSettings(settingsFilePath)
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("%w: stderr pipe: %v", agent.ErrSpawnFailed, err)
	}

	if err := cmd.Start(); err != nil {
		_ = removeGeminiSettings(settingsFilePath)
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("%w: cmd start: %v", agent.ErrSpawnFailed, err)
	}

	if onProcessSpawned != nil && cmd.Process != nil {
		onProcessSpawned(cmd.Process.Pid)
	}

	stderrBuf := newBoundedBuffer(stderrBufferSize)

	h := &Handle{
		binary:           binary,
		settingsFilePath: settingsFilePath,
		cwd:              cwd,
		cmd:              cmd,
		events:           make(chan agent.Event, eventBufferSize),
		logger:           slog.With("provider", "gemini-cli", "pid", cmd.Process.Pid),
		stdoutPipe:       stdout,
		stderrPipe:       stderr,
		stderrBuf:        stderrBuf,
		shutdown:         make(chan struct{}),
		done:             make(chan struct{}),
	}

	// Write the prompt to stdin then close it. The CLI reads stdin until
	// EOF before producing the first result line, so closing is mandatory.
	go writePromptStdin(stdin, stdinPrompt, h.logger)

	// Drain stderr asynchronously into the bounded buffer for post-mortem.
	go drainStderr(stderr, stderrBuf, h.logger)

	// Launch the stdout reader goroutine.
	go h.readStdout()

	// Watch the spawn ctx: when cancelled, trigger a soft shutdown so
	// the events channel closes regardless of subprocess state.
	go h.watchCtx(ctx) //nolint:gosec // G118: graceful-stop ctx must outlive request ctx

	return h, nil
}

// watchCtx waits for the spawn ctx to fire and then runs Stop with a
// background ctx so the events channel closes and the settings.json
// is cleaned up.
func (h *Handle) watchCtx(ctx context.Context) {
	select {
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), stopGracePeriod+2*time.Second)
		defer cancel()
		_ = h.Stop(stopCtx)
	case <-h.shutdown:
		// Stop already initiated; nothing more to do.
	}
}

// SessionID returns the provider-native session id captured from the
// first InitEvent. Empty until InitEvent fires; safe for concurrent reads.
func (h *Handle) SessionID() string {
	if v := h.sessionID.Load(); v != nil {
		return *v
	}
	return ""
}

// Events returns the read-only event channel.
// Closed by Stop() after the subprocess has terminated.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject is not supported by the gemini-cli provider.
// SupportsMessageInjection=false in Capabilities; the gemini CLI's
// --resume uses session indexes rather than UUIDs.
func (h *Handle) Inject(_ context.Context, _ string) error {
	return fmt.Errorf("provider/geminicli: Inject: %w (SupportsMessageInjection=false; gemini CLI does not support between-turn injection)", agent.ErrUnsupported)
}

// Stop aborts the session.
//
// Idempotent: subsequent calls return the same recorded error.
// Safe to call after the events channel has closed.
//
// Stop sends SIGTERM, waits up to stopGracePeriod for graceful exit,
// then sends SIGKILL. The settings.json is removed before returning.
func (h *Handle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		h.stopErr = h.doStop(ctx)
	})
	return h.stopErr
}

func (h *Handle) doStop(ctx context.Context) error {
	// Close the shutdown signal early so any goroutine blocked on a
	// sendEvent can bail out cleanly.
	close(h.shutdown)

	// Always close the events channel and remove settings.json before
	// returning, even if subprocess teardown errored.
	defer h.closeEvents()
	defer func() { _ = removeGeminiSettings(h.settingsFilePath) }()

	// If the subprocess has already exited, nothing more to signal.
	select {
	case <-h.done:
		return nil
	default:
	}

	if h.cmd != nil && h.cmd.Process != nil {
		// SIGTERM the whole process group first so MCP server children
		// and shell helpers terminate too.
		signalProcessGroup(h.cmd, syscall.SIGTERM)
	}

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

// readStdout is the per-handle goroutine that drains the subprocess
// stdout, decodes each line via mapLine, and forwards events to the
// channel via sendEvent.
//
// It does NOT close h.events; Stop() owns the close after the subprocess
// has terminated.
//
// On EOF without a terminal ResultEvent it emits a synthetic ErrorEvent
// (code "spawn_no_result") so the runner observes the failure rather
// than waiting silently.
func (h *Handle) readStdout() {
	defer close(h.done)
	defer func() {
		err := h.cmd.Wait()
		if err != nil {
			h.waitErr.Store(&err)
		}
	}()

	// Force-close the stdout pipe when shutdown is signalled so the
	// scanner unblocks even if SIGKILL of the subprocess group does not
	// immediately propagate a pipe close (observed race on macOS under
	// -race load; mirrors claude provider's pipe-close guard).
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
	// Each JSONL line can be large (assistant output, MCP tool payloads).
	// Use the same 4 MiB limit as the claude provider.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	terminal := false
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		// Copy: scanner reuses its buffer across calls.
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
			Message: fmt.Sprintf("provider/geminicli: stdout scan: %v", err),
			Code:    "stdout_scan",
		})
		return
	}
	if terminal {
		h.parentTerminal.Store(true)
		return
	}

	// EOF without a terminal result event — subprocess exited without
	// emitting a "result" line. Build a diagnostic message from stderr.
	stderrTail := h.stderrBuf.String()
	msg := "gemini CLI exited without terminal result"
	if stderrTail != "" {
		msg = fmt.Sprintf("%s: stderr=%s", msg, stderrTail)
	}
	h.sendEvent(agent.ErrorEvent{
		Message: msg,
		Code:    "spawn_no_result",
	})
}

// writePromptStdin writes the prompt to the child's stdin and closes it
// (the CLI reads stdin to EOF before producing the first line of output).
// Errors are logged but not surfaced — if the child died early the stdout
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
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	overflow := (len(b.buf) + len(p)) - b.limit
	if overflow > 0 {
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
