package agycli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/creack/pty"
)

// stopGracePeriod is the deadline between SIGTERM and SIGKILL on Stop / ctx
// cancellation. Matches the grace period used across CLI-backed providers.
const stopGracePeriod = 5 * time.Second

// eventBufferSize is the buffered capacity of the events channel.
const eventBufferSize = 64

// maxRetainedOutput caps how much sanitized stdout we keep for envelope
// extraction and diagnostics. The result envelope is at the END of the output,
// so when the cap is exceeded we drop from the FRONT (keep the tail).
const maxRetainedOutput = 2 * 1024 * 1024

// scanBufferMax is the max single-line size from the pty (large file dumps in
// agy's narration can produce long lines).
const scanBufferMax = 4 * 1024 * 1024

// ansiRE strips terminal escape sequences a pty may interleave. agy's `-p`
// output is plain text in practice, so this is defensive normalization.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;?=]*[ -/]*[@-~]|\x1b\\][^\x07]*(\x07|\x1b\\\\)|\x1b[@-Z\\\\-_]")

// canonicalWorktree validates and canonicalizes the one filesystem root this
// provider grants to agy for a run. A blank Cwd keeps legacy no-workarea
// callers working; a non-empty Cwd must name an existing directory so an agy
// scratch fallback can never silently change the run's authority.
func canonicalWorktree(cwd string) (string, error) {
	if cwd == "" {
		return "", nil
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve worktree %q: %w", cwd, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat worktree %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("worktree %q is not a directory", abs)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize worktree %q: %w", abs, err)
	}
	return canonical, nil
}

// Handle is the agent.Handle implementation backed by an `agy` subprocess
// attached to a pty.
//
// Lifecycle:
//
//  1. spawn() starts the subprocess under a pty (creack/pty) and launches the
//     readLoop goroutine.
//  2. readLoop emits an InitEvent, streams sanitized stdout lines as
//     AssistantTextEvents while a transcript tailer (tail.go) streams
//     best-effort ToolUse/ToolResult events live off agy's on-disk
//     transcript.jsonl, then on EOF + cmd.Wait waits for the tailer's final
//     catch-up drain and emits a terminal ResultEvent, then closes the events
//     channel and signals done. It is self-contained: completion does NOT
//     require an external Stop().
//  3. Stop() (or ctx cancellation) force-closes the pty to unblock a blocked
//     reader, SIGTERM→SIGKILLs the process group, and is idempotent.
type Handle struct {
	binary    string
	cwd       string
	stateHome string
	cmd       *exec.Cmd
	ptmx      *os.File
	events    chan agent.Event
	logger    *slog.Logger

	enrichTranscript bool
	brainBefore      map[string]struct{} // conv-id snapshot taken pre-spawn

	// sessionID is the agy conversation id, captured post-hoc from transcript
	// discovery (empty until then).
	sessionID atomic.Pointer[string]

	stopOnce sync.Once
	stopErr  error

	// shutdown is closed by Stop() to unblock a blocked reader and any
	// sendEvent in flight.
	shutdown chan struct{}

	eventsClosed atomic.Bool
	eventsMu     sync.RWMutex

	// done is closed after readLoop finishes (subprocess reaped, events closed).
	done chan struct{}

	ptmxCloseOnce sync.Once
}

// sendEvent multiplexes one event onto the public channel. Drops silently once
// the channel is closed or shutdown is signalled.
func (h *Handle) sendEvent(ev agent.Event) {
	h.eventsMu.RLock()
	defer h.eventsMu.RUnlock()
	if h.eventsClosed.Load() {
		return
	}
	select {
	case h.events <- ev:
	case <-h.shutdown:
	}
}

// closeEvents closes the events channel exactly once.
func (h *Handle) closeEvents() {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	if h.eventsClosed.Load() {
		return
	}
	h.eventsClosed.Store(true)
	close(h.events)
}

func (h *Handle) closePTY() {
	h.ptmxCloseOnce.Do(func() {
		if h.ptmx != nil {
			_ = h.ptmx.Close()
		}
	})
}

// spawn is the internal Provider.Spawn implementation.
func (p *Provider) spawn(ctx context.Context, spec agent.Spec) (*Handle, error) {
	worktree, err := canonicalWorktree(spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	spec.Cwd = worktree

	stateHome := resolveStateHome(p.stateHome)

	// Opt-in workspace trust (default off; best-effort). Untrusted cwds run
	// fine under --dangerously-skip-permissions per the probe — see
	// Options.TrustWorkspace.
	if p.trustWorkspace && spec.Cwd != "" {
		_ = ensureWorkspaceTrusted(stateHome, spec.Cwd)
	}

	// Snapshot conversation ids BEFORE spawn so we can attribute the new one to
	// this run for transcript enrichment.
	var brainBefore map[string]struct{}
	if p.enrichTranscript {
		brainBefore = snapshotConvIDs(stateHome)
	}

	argv := buildArgs(spec, false)

	//nolint:gosec // binary resolved via exec.LookPath at construction; argv is
	// a closed flag set plus the prompt value.
	cmd := exec.Command(p.binary, argv...)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = composeEnv(os.Environ(), spec.Env)

	// pty.Start sets Setsid/Setctty on cmd.SysProcAttr, wires the slave as the
	// child's stdio, starts it, and returns the master. agy REQUIRES a pty
	// (CONTRACT.md §1) — a plain pipe hangs with zero output.
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("%w: pty start: %v", agent.ErrSpawnFailed, err)
	}
	// Until the Handle owns ptmx (and can close it via closePTY), guard the fd:
	// if anything below panics or returns early — e.g. a user OnProcessSpawned
	// callback panics — the pty master would otherwise leak.
	handedOff := false
	defer func() {
		if !handedOff {
			_ = ptmx.Close()
		}
	}()

	if spec.OnProcessSpawned != nil && cmd.Process != nil {
		spec.OnProcessSpawned(cmd.Process.Pid)
	}

	h := &Handle{
		binary:           p.binary,
		cwd:              spec.Cwd,
		stateHome:        stateHome,
		cmd:              cmd,
		ptmx:             ptmx,
		events:           make(chan agent.Event, eventBufferSize),
		logger:           slog.With("provider", "agy-cli", "pid", cmd.Process.Pid),
		enrichTranscript: p.enrichTranscript,
		brainBefore:      brainBefore,
		shutdown:         make(chan struct{}),
		done:             make(chan struct{}),
	}

	handedOff = true
	go h.readLoop()
	go h.watchCtx(ctx) //nolint:gosec // G118: graceful-stop ctx must outlive request ctx

	return h, nil
}

// watchCtx runs Stop with a background ctx when the spawn ctx fires, so the
// events channel closes and the subprocess is reaped regardless of state.
func (h *Handle) watchCtx(ctx context.Context) {
	select {
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), stopGracePeriod+2*time.Second)
		defer cancel()
		_ = h.Stop(stopCtx)
	case <-h.shutdown:
	}
}

// readLoop drains the pty master, emits events, and reaps the subprocess.
//
// It does NOT close the events channel — Stop() is the exclusive owner of the
// close (consistent with other CLI-backed providers). The runner returns on
// the terminal ResultEvent and then defers Stop(); watchCtx also Stops on ctx
// cancellation, so the channel always eventually closes. readLoop closes only
// h.done, which Stop() waits on.
func (h *Handle) readLoop() {
	defer close(h.done)

	// Interrupt goroutine: if Stop() fires while the read is blocked, close the
	// pty so the read unblocks. It exits on readerDone (normal) and also on
	// h.done as a panic backstop — h.done is closed by the deferred close above
	// even if readLoop panics, so the goroutine can never leak.
	readerDone := make(chan struct{})
	go func() {
		select {
		case <-h.shutdown:
			h.closePTY()
		case <-readerDone:
		case <-h.done:
		}
	}()

	h.sendEvent(agent.InitEvent{})

	// Live transcript tailing: stream agy's structured tool events DURING the
	// run (agy writes them only to its on-disk transcript; stdout is prose).
	// The tailer also discovers the conversation id mid-run and re-emits a
	// corrective InitEvent — the first InitEvent fired with an empty SessionID
	// because the conv-id is only knowable post-spawn — so the runner
	// overwrites the durable ProviderSessionID while the session is still
	// alive. Its final catch-up drain (after cmd.Wait below) replaces the old
	// after-EOF transcript replay; the tailer is the only transcript emitter,
	// so nothing is duplicated.
	var tailStop, tailDone chan struct{}
	if h.enrichTranscript && h.stateHome != "" {
		tailStop, tailDone = make(chan struct{}), make(chan struct{})
		tailer := &transcriptTailer{
			stateHome: h.stateHome,
			cwd:       h.cwd,
			before:    h.brainBefore,
			emit:      h.sendEvent,
			onConvID: func(convID string) {
				id := convID
				h.sessionID.Store(&id)
				h.sendEvent(agent.InitEvent{SessionID: convID})
			},
		}
		go func() {
			defer close(tailDone)
			tailer.run(tailStop)
		}()
	}

	retained := newCappedBuffer(maxRetainedOutput)
	buf := make([]byte, 32*1024)
	var lineCarry strings.Builder
	var envFilter envelopeLineFilter
	flushLine := func(line string) {
		clean := sanitizeLine(line)
		retained.WriteString(clean + "\n")
		// Envelope lines (markers + result JSON) are retained for
		// buildResult but never emitted — raw envelope JSON must not
		// surface as assistant "thoughts".
		if envFilter.suppress(clean) {
			return
		}
		if strings.TrimSpace(clean) != "" {
			h.sendEvent(agent.AssistantTextEvent{Text: clean})
		}
	}
	// Read raw bytes off the pty master and split on '\n' ourselves. (A
	// bufio.Scanner over a pty can mis-handle the EIO that follows slave close;
	// a manual read loop treats any read error as EOF.)
	for {
		n, err := h.ptmx.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			for {
				idx := strings.IndexByte(chunk, '\n')
				if idx < 0 {
					lineCarry.WriteString(chunk)
					if lineCarry.Len() > scanBufferMax {
						flushLine(lineCarry.String())
						lineCarry.Reset()
					}
					break
				}
				lineCarry.WriteString(chunk[:idx])
				flushLine(lineCarry.String())
				lineCarry.Reset()
				chunk = chunk[idx+1:]
			}
		}
		if err != nil {
			break // EOF / EIO on slave close — normal pty teardown
		}
	}
	if lineCarry.Len() > 0 {
		flushLine(lineCarry.String())
	}
	close(readerDone)
	h.closePTY()

	waitErr := h.cmd.Wait()

	// Stop the tailer and wait for its final catch-up drain so every
	// transcript event (and the corrective InitEvent, when discovery only
	// succeeds post-exit) is emitted before the terminal ResultEvent.
	if tailDone != nil {
		close(tailStop)
		<-tailDone
	}

	h.sendEvent(h.buildResult(retained.String(), waitErr))
}

// buildResult synthesizes the terminal ResultEvent from the exit status and the
// injected result envelope. agy emits no native terminal event, so the provider
// derives one: success = process exited 0, overridden by the envelope's status
// when present; the envelope summary becomes the result message.
func (h *Handle) buildResult(output string, waitErr error) agent.Event {
	exitOK := waitErr == nil
	env, rawJSON, ok := extractEnvelope(output)

	success := exitOK
	if ok {
		success = successFromEnvelope(env, exitOK)
	}

	if !exitOK && strings.TrimSpace(output) == "" {
		// No output AND non-zero exit: agy never produced anything. Most likely
		// not logged in, or the pty was not honored. Loud, actionable failure.
		return agent.ResultEvent{
			Success:      false,
			ErrorSubtype: "no_output",
			Errors: []string{fmt.Sprintf(
				"agy exited without output: %v (is agy logged in via OAuth? does it have a pty?)", waitErr)},
		}
	}

	res := agent.ResultEvent{Success: success}
	if ok {
		res.Message = env.Summary
		res.Raw = rawJSON
	}
	if !success {
		if waitErr != nil {
			res.ErrorSubtype = "nonzero_exit"
			res.Errors = append(res.Errors, fmt.Sprintf("agy exited: %v", waitErr))
		} else {
			res.ErrorSubtype = "envelope_failed"
			res.Errors = append(res.Errors, "agy reported a failed result envelope")
		}
	}
	return res
}

// SessionID returns the agy conversation id once discovered (post-completion),
// else "". Safe for concurrent reads.
func (h *Handle) SessionID() string {
	if v := h.sessionID.Load(); v != nil {
		return *v
	}
	return ""
}

// Events returns the read-only event channel; closed after the subprocess
// terminates and the terminal ResultEvent has been sent.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject is not supported (SupportsMessageInjection=false; single-shot -p).
func (h *Handle) Inject(_ context.Context, _ string) error {
	return fmt.Errorf("provider/agycli: Inject: %w (SupportsMessageInjection=false; agy -p is single-shot)", agent.ErrUnsupported)
}

// Stop aborts the session. Idempotent; safe after the events channel closed.
func (h *Handle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		h.stopErr = h.doStop(ctx)
	})
	return h.stopErr
}

func (h *Handle) doStop(ctx context.Context) error {
	// Unblock a blocked reader and any in-flight sendEvent.
	close(h.shutdown)
	h.closePTY()
	defer h.closeEvents()

	select {
	case <-h.done:
		return nil // already finished
	default:
	}

	if h.cmd != nil && h.cmd.Process != nil {
		signalProcessGroup(h.cmd, syscall.SIGTERM)
	}

	timer := time.NewTimer(stopGracePeriod)
	defer timer.Stop()
	select {
	case <-h.done:
	case <-timer.C:
		signalProcessGroup(h.cmd, syscall.SIGKILL)
		<-h.done
	case <-ctx.Done():
		signalProcessGroup(h.cmd, syscall.SIGKILL)
		<-h.done
	}
	return nil
}

// sanitizeLine strips a trailing CR (pty CRLF) and any ANSI escape sequences.
func sanitizeLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if strings.IndexByte(s, '\x1b') >= 0 {
		s = ansiRE.ReplaceAllString(s, "")
	}
	return s
}

// cappedBuffer retains at most cap bytes, dropping from the FRONT on overflow
// so the tail (where the result envelope lives) is always preserved.
type cappedBuffer struct {
	cap int
	buf []byte
}

func newCappedBuffer(capBytes int) *cappedBuffer {
	return &cappedBuffer{cap: capBytes, buf: make([]byte, 0, min(capBytes, 64*1024))}
}

func (b *cappedBuffer) WriteString(s string) {
	b.buf = append(b.buf, s...)
	if len(b.buf) > b.cap {
		drop := len(b.buf) - b.cap
		b.buf = b.buf[drop:]
	}
}

func (b *cappedBuffer) String() string { return string(b.buf) }
