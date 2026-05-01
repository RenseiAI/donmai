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

// Handle is the agent.Handle implementation backed by a single
// `claude` subprocess. The subprocess writes JSONL on stdout which
// the Handle decodes into agent.Event values.
//
// Lifecycle:
//
//  1. spawn() creates the cmd, sets stdin/stdout/stderr pipes, and
//     launches the cmd-reading goroutine.
//  2. Events() returns a channel the goroutine writes to.
//  3. The goroutine drains stdout, mapping each line to events,
//     posts them on the channel, and closes the channel on EOF or
//     after emitting a synthetic ErrorEvent on subprocess failure.
//  4. Stop() (or ctx cancellation) sends SIGTERM, then SIGKILL after
//     stopGracePeriod, then awaits the goroutine via cmd.Wait. The
//     MCP tmpfile is removed in cleanup.
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

	// done is closed when the reader goroutine exits and cmd.Wait
	// returns, signalling Stop() that the subprocess has terminated.
	done chan struct{}

	// waitErr holds the cmd.Wait error (kept for diagnostics; not
	// returned directly).
	waitErr atomic.Pointer[error]
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
		done:          make(chan struct{}),
	}

	// Launch the prompt-writer in a goroutine. The CLI consumes the
	// prompt then waits for stdin EOF before producing the result
	// line, so we close stdin once the prompt is written.
	go writePromptStdin(stdin, stdinPrompt, h.logger)

	// Launch the stderr drainer.
	go drainStderr(stderr, stderrBuf, h.logger)

	// Launch the stdout reader; it owns the events channel close.
	go h.readStdout()

	return h, nil
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

// Events returns the read-only event channel. Closed by the reader
// goroutine after the subprocess terminates.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject returns agent.ErrUnsupported on v0.5.0. The Claude CLI
// stream-json mode does not expose mid-session user-message
// injection. Re-enable when a wrapper sidecar lands in F.5.
func (h *Handle) Inject(_ context.Context, _ string) error {
	return fmt.Errorf("provider/claude: Inject: %w (SupportsMessageInjection=false in v0.5.0; tracked in REN-1451)", agent.ErrUnsupported)
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
	// If the subprocess has already exited, just clean up.
	select {
	case <-h.done:
		return removeMCPConfig(h.mcpConfigPath)
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

	if err := removeMCPConfig(h.mcpConfigPath); err != nil {
		return err
	}
	return nil
}

// readStdout is the per-handle goroutine that drains the subprocess
// stdout, decodes each line via mapLine, and forwards events to the
// channel. It owns the channel close.
//
// On EOF without a terminal ResultEvent it emits a synthetic
// ErrorEvent (code "spawn_no_result") so the runner observes the
// failure rather than blocking forever.
func (h *Handle) readStdout() {
	defer close(h.events)
	defer close(h.done)
	defer func() {
		// cmd.Wait must be called to release process resources.
		err := h.cmd.Wait()
		if err != nil {
			h.waitErr.Store(&err)
		}
	}()

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
			h.events <- ev
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		h.events <- agent.ErrorEvent{
			Message: fmt.Sprintf("provider/claude: stdout scan: %v", err),
			Code:    "stdout_scan",
		}
		return
	}
	if !terminal {
		stderrTail := h.stderrBuf.String()
		msg := "claude exited without terminal result"
		if stderrTail != "" {
			msg = fmt.Sprintf("%s: stderr=%s", msg, stderrTail)
		}
		h.events <- agent.ErrorEvent{
			Message: msg,
			Code:    "spawn_no_result",
		}
	}
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
