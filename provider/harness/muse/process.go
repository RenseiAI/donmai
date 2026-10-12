package muse

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// stopGracePeriod is the deadline between SIGTERM and SIGKILL when
// Stop is called or ctx is cancelled. Borrowed from clijsonl (5s
// grace); the serve host owns live model calls that deserve a drain
// window before the kill.
const stopGracePeriod = 5 * time.Second

// stderrBufferSize caps how many bytes of serve stderr are retained
// for post-mortem diagnostics on unexpected exits. Borrowed from
// clijsonl's 8 KiB stderr-tail bound.
const stderrBufferSize = 8 * 1024

// hostProc owns one `muse serve` child and its JSON-RPC client. One
// hostProc per Handle: it is created in Spawn/Resume, handshaked,
// attached to the handle, and reaped when the handle terminates or
// the host exits.
type hostProc struct {
	cmd    *exec.Cmd
	client *Client
	stderr *boundedBuffer

	handleMu sync.Mutex
	handle   *Handle

	stopOnce sync.Once
}

// startHost spawns `muse serve`, wires the JSON-RPC client, and runs
// the initialize handshake. On any failure the child is reaped and the
// error wraps ErrSpawnFailed (or ErrProviderUnavailable for binary
// problems, already classified by New).
func (p *Provider) startHost(ctx context.Context, spec agent.Spec, readOnly bool, workspaceRoot string) (*hostProc, error) {
	needMCP := len(spec.MCPServers) > 0

	if p.opts.skipProcess {
		// Test path: caller wired stdin/stdout via overrides.
		client := NewClient(p.opts.stdinOverride, p.opts.stdoutOverride)
		proc := &hostProc{client: client, stderr: newBoundedBuffer(stderrBufferSize)}
		if err := handshake(ctx, client, p.opts.HandshakeTimeout, needMCP); err != nil {
			client.Stop(err)
			return nil, err
		}
		return proc, nil
	}

	argv := []string{"serve"}
	if readOnly {
		// The read-only lane: no shell execution, no workspace
		// writes. Sandbox posture is fixed for the host lifetime;
		// approvalMode denyUnmatched is selected on the wire.
		argv = append(argv, "--disable-shell", "--disable-write")
	}
	//nolint:gosec // binary resolved from Options/env; flags are a fixed set.
	cmd := exec.Command(p.museBin, argv...)
	configureProcessGroup(cmd)
	cmd.Dir = workspaceRoot
	// Inherit the parent environment (nil parent would leave the host
	// without PATH/HOME and it exits before answering initialize).
	cmd.Env = serveEnv(os.Environ(), p.opts.Env, spec.Env, specSetsKey(spec, apiKeyEnv))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, wrapSpawn(err, "stdin pipe")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, wrapSpawn(err, "stdout pipe")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, wrapSpawn(err, "stderr pipe")
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, wrapSpawn(err, "spawn")
	}
	proc := &hostProc{cmd: cmd, stderr: captureStderr(stderr)}
	proc.client = NewClient(stdin, stdout)
	go proc.watchExit()

	if err := handshake(ctx, proc.client, p.opts.HandshakeTimeout, needMCP); err != nil {
		proc.close()
		return nil, wrapSpawn(err, "handshake")
	}
	return proc, nil
}

func wrapSpawn(err error, what string) error {
	return &spawnError{what: what, err: err}
}

// spawnError marks a host-start failure. Unwraps to the cause.
type spawnError struct {
	what string
	err  error
}

func (e *spawnError) Error() string { return "muse " + e.what + ": " + e.err.Error() }
func (e *spawnError) Unwrap() error { return e.err }

// attachHandle binds the handle whose session this host serves. The
// client's on-close hook fails exactly that handle.
func (proc *hostProc) attachHandle(h *Handle) {
	proc.handleMu.Lock()
	proc.handle = h
	proc.handleMu.Unlock()
	proc.client.SetOnClose(func(cause error) {
		proc.handleMu.Lock()
		target := proc.handle
		proc.handleMu.Unlock()
		if target != nil {
			if excerpt := proc.stderr.Excerpt(); excerpt != "" {
				cause = &stderrEnrichedError{cause: cause, excerpt: excerpt}
			}
			target.failNow(cause)
		}
	})
}

// stderrEnrichedError carries the host's stderr tail alongside the
// close cause.
type stderrEnrichedError struct {
	cause   error
	excerpt string
}

func (e *stderrEnrichedError) Error() string {
	if e.cause == nil {
		return "muse serve host exited (stderr: " + e.excerpt + ")"
	}
	return e.cause.Error() + " (serve stderr: " + e.excerpt + ")"
}
func (e *stderrEnrichedError) Unwrap() error { return e.cause }

// pid reports the host child pid, or 0 when there is no real child
// (skipProcess tests).
func (proc *hostProc) pid() int {
	if proc.cmd == nil || proc.cmd.Process == nil {
		return 0
	}
	return proc.cmd.Process.Pid
}

// watchExit observes the host child exit and stops the client so the
// attached Handle is failed via onHostClose.
func (proc *hostProc) watchExit() {
	if proc.cmd == nil {
		return
	}
	err := proc.cmd.Wait()
	cause := err
	if cause == nil {
		cause = errHostExited
	}
	if proc.client != nil {
		proc.client.Stop(cause)
	}
}

var errHostExited = &hostExitedError{}

type hostExitedError struct{}

func (e *hostExitedError) Error() string { return "muse serve host exited" }

// close stops the client and reaps the child (SIGTERM → grace →
// SIGKILL on the process group). Idempotent.
func (proc *hostProc) close() {
	proc.stopOnce.Do(func() {
		if proc.client != nil {
			proc.client.Stop(errHostExited)
		}
		if proc.cmd == nil || proc.cmd.Process == nil {
			return
		}
		signalProcessGroup(proc.cmd, syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = proc.cmd.Process.Wait()
		}()
		select {
		case <-done:
		case <-time.After(stopGracePeriod):
			signalProcessGroup(proc.cmd, syscall.SIGKILL)
			<-done
		}
	})
}

// checkAlive reports ErrProviderUnavailable when the host has already
// terminated. Kept as a method for the handle's use; the provider
// itself never shares a host across handles.
var _ = agent.ErrProviderUnavailable

// captureStderr drains r into a bounded tail buffer. Borrowed from
// clijsonl's stderr ring: the child never blocks on a full pipe, and
// a crash leaves a forensic excerpt instead of nothing.
func captureStderr(r io.ReadCloser) *boundedBuffer {
	buf := newBoundedBuffer(stderrBufferSize)
	go func() {
		defer func() { _ = r.Close() }()
		tmp := make([]byte, 4096)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				_, _ = buf.Write(tmp[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return buf
}
