package ptycli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// stopGrace bounds how long watchCtx waits for a graceful exit after ctx is
// cancelled before falling back to ptyhost.Session.Stop's own SIGTERM→grace→
// SIGKILL escalation. Matches the grace period used across the other
// CLI-backed harnesses (clijsonl, agycli).
const stopGrace = 5 * time.Second

// Handle is the agent.Handle + agent.InteractiveCapable implementation
// shared by every harness's interactive PTY spawn mode. See doc.go for the
// event-semantics and suspend/resume contract.
type Handle struct {
	sess   *ptyhost.Session
	events chan agent.Event

	closeOnce   sync.Once
	cleanupOnce sync.Once
	cleanupFn   func() error
	cleanupErr  error
}

// Spawn starts binary+argv under a PTY via ptyhost.Spawn and returns a
// Handle. spec.Interactive supplies the geometry/ring/record knobs
// (agent.InteractiveSpec); a nil spec.Interactive falls back to ptyhost's own
// defaults (80×24, 8 MiB ring, no recording) — callers should not invoke
// Spawn with a nil spec.Interactive (the capability-gated Spec contract means
// callers only reach this package when Spec.Interactive != nil), but the
// fallback keeps the driver defensive rather than panicking.
//
// spec.Env is passed through to ptyhost.Spec.Env, which layers it onto the
// parent process environment, applies TERM=xterm-256color /
// COLORTERM=truecolor as interactive defaults, and then honors explicit
// per-request overrides
// (ptyhost/spec.go composeEnv) — the harness-layer callers of this package
// never need to set those themselves. spec.Cwd is passed through verbatim.
//
// spec.OnProcessSpawned is NOT invoked: ptyhost.Session does not expose the
// child PID (it is a deliberately narrow, transport-free host surface), so
// there is no PID to report. Callers that need PID-based metrics for an
// interactive session are not served by this driver today — a documented,
// known gap rather than a silent no-op.
// manifest is the harness's own live declaration. Only one field is read
// today — Caps.NoticeDelivery, which decides whether this PTY may accept a
// runner-authored notice — but the whole manifest is taken rather than that one
// value so the driver keeps reading the harness's declaration instead of
// growing a parameter (and a call-site decision) per capability. Passing a zero
// manifest declares nothing, which refuses notices.
func Spawn(ctx context.Context, binary string, argv []string, spec agent.Spec, manifest agent.HarnessManifest) (*Handle, error) {
	return SpawnWithCleanup(ctx, binary, argv, spec, manifest, nil)
}

// SpawnWithCleanup starts an interactive PTY and transfers ownership of an
// optional per-session resource cleanup function to the returned handle. The
// cleanup runs exactly once on spawn failure, child exit, context cancellation,
// or Stop, whichever happens first.
func SpawnWithCleanup(ctx context.Context, binary string, argv []string, spec agent.Spec, manifest agent.HarnessManifest, cleanup func() error) (*Handle, error) {
	ispec := spec.Interactive
	if ispec == nil {
		ispec = &agent.InteractiveSpec{}
	}

	command := []string{binary}
	command = append(command, argv...)

	pspec := ptyhost.Spec{
		Command:    command,
		Env:        envSlice(spec.Env),
		Cwd:        spec.Cwd,
		Cols:       uint16(ispec.Cols), //nolint:gosec // terminal geometry never exceeds uint16
		Rows:       uint16(ispec.Rows), //nolint:gosec // terminal geometry never exceeds uint16
		RingBytes:  ispec.RingBytes,
		RecordPath: ispec.RecordPath,
		// Read from the LIVE manifest, never from the harness's name: the
		// permission to write a notice into this terminal belongs to whichever
		// harnesses declare it, and that set must stay derivable from the
		// registry rather than restated here as a literal.
		NoticeDelivery: manifest.Caps.NoticeDelivery,
	}

	sess, err := ptyhost.Spawn(pspec)
	if err != nil {
		spawnErr := fmt.Errorf("%w: interactive pty spawn: %v", agent.ErrSpawnFailed, err)
		if cleanup != nil {
			if cleanupErr := cleanup(); cleanupErr != nil {
				return nil, errors.Join(spawnErr, fmt.Errorf("interactive resource cleanup: %w", cleanupErr))
			}
		}
		return nil, spawnErr
	}

	h := &Handle{
		sess: sess,
		// Buffered for exactly the two events this driver ever emits
		// (InitEvent, terminal ResultEvent) so sendEvent never blocks on a
		// slow/absent consumer.
		events:    make(chan agent.Event, 2),
		cleanupFn: cleanup,
	}
	// The session is up the instant ptyhost.Spawn returns (pty.StartWithSize
	// blocks until fork+exec completes) — emit InitEvent synchronously,
	// before run()/watchCtx() start, so it is queued before the Handle is
	// even returned to the caller.
	h.events <- agent.InitEvent{}

	go h.run()
	//nolint:gosec // G118: spawn ctx is the lifecycle source we want (matches clijsonl's watchCtx)
	go h.watchCtx(ctx)

	return h, nil
}

// run waits for the PTY child to exit, maps its terminal Exit payload onto a
// single ResultEvent, and closes the events channel. It is the sole owner of
// both the terminal event and the channel close.
func (h *Handle) run() {
	<-h.sess.Done()
	exit, _ := h.sess.Exit()
	result := buildResult(exit)
	if err := h.cleanup(); err != nil {
		result = cleanupFailureResult(err)
	}
	h.events <- result
	h.closeOnce.Do(func() { close(h.events) })
}

func cleanupFailureResult(err error) agent.Event {
	return agent.ResultEvent{
		Success:      false,
		ErrorSubtype: "cleanup_failed",
		Errors:       []string{"interactive session resource cleanup: " + err.Error()},
	}
}

func (h *Handle) cleanup() error {
	h.cleanupOnce.Do(func() {
		if h.cleanupFn != nil {
			h.cleanupErr = h.cleanupFn()
		}
	})
	return h.cleanupErr
}

// watchCtx Stops the session when ctx is cancelled, so a caller that only
// cancels its context (rather than calling Stop explicitly) still tears the
// PTY child down. Mirrors the watchCtx pattern in clijsonl/agycli.
func (h *Handle) watchCtx(ctx context.Context) {
	select {
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), stopGrace+2*time.Second)
		defer cancel()
		_ = h.Stop(stopCtx)
	case <-h.sess.Done():
	}
}

// buildResult maps a ptyhost Exit payload onto the terminal ResultEvent per
// doc.go's coarse event-semantics contract: exit code 0 (no signal) is
// success, anything else is a failure carrying the exit/signal detail.
// Mirrors the ErrorSubtype convention agycli's buildResult uses
// ("nonzero_exit").
func buildResult(exit attachwire.ExitPayload) agent.Event {
	if exit.ExitCode == 0 && !exit.BySignal() {
		return agent.ResultEvent{Success: true}
	}
	msg := fmt.Sprintf("interactive pty child exited with code %d", exit.ExitCode)
	if exit.BySignal() {
		msg = fmt.Sprintf("interactive pty child killed by signal %s (exit code %d)", exit.Signal, exit.ExitCode)
	}
	return agent.ResultEvent{
		Success:      false,
		ErrorSubtype: "nonzero_exit",
		Errors:       []string{msg},
	}
}

// SessionID always returns "" — no harness's interactive TUI exposes a
// provider-native session id on its own stdout (see doc.go). Distinct from
// the interactive-attach-v1 session identity, which the composing layer owns.
func (h *Handle) SessionID() string { return "" }

// Events returns the read-only event channel; see doc.go for the exact
// two-event sequence (InitEvent, terminal ResultEvent).
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject always returns agent.ErrUnsupported: in interactive mode the
// terminal IS the input surface (write via
// InteractiveSession().WriteInput), not Handle.Inject.
func (h *Handle) Inject(context.Context, string) error {
	return fmt.Errorf(
		"provider/harness/ptycli: Inject: %w (interactive mode — the terminal is the input surface; use InteractiveSession().WriteInput)",
		agent.ErrUnsupported,
	)
}

// Stop routes to the underlying ptyhost.Session's Stop (SIGTERM→grace→
// SIGKILL escalation it already implements). Idempotent; safe after the
// events channel has closed.
func (h *Handle) Stop(ctx context.Context) error {
	return errors.Join(h.sess.Stop(ctx), h.cleanup())
}

// InteractiveSession returns the live PTY surface. Never nil for a Handle
// returned by Spawn — satisfies agent.InteractiveCapable.
func (h *Handle) InteractiveSession() agent.InteractiveSession { return h.sess }

// EmitMarker is a passthrough convenience to the underlying
// ptyhost.Session.EmitMarker so the P5-WS6 suspend/resume seam does not need
// a type assertion to agent.InteractiveCapable before calling it. Suspend
// transitions call h.EmitMarker(agent.MarkerApprovalPending); resume calls
// h.EmitMarker(agent.MarkerApprovalResolved) (agent/interactive.go documents
// the markers). This package never calls it itself — it only wires the hook.
func (h *Handle) EmitMarker(label string) error { return h.sess.EmitMarker(label) }

// Compile-time assertions: Handle satisfies both agent.Handle and the
// additive agent.InteractiveCapable seam.
var (
	_ agent.Handle             = (*Handle)(nil)
	_ agent.InteractiveCapable = (*Handle)(nil)
)

// envSlice converts a Spec.Env map into the "KEY=VALUE" slice
// ptyhost.Spec.Env expects. Keys are sorted for deterministic child argv/env
// across runs (matches the composeEnv convention in clijsonl and agycli).
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if runtimeenv.IsRunnerOnly(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
