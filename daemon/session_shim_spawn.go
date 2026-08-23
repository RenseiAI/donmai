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
	"sync"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// defaultShimLaunchTimeout bounds how long the daemon waits for a freshly
// launched shim to publish its discovery record and complete a handshake.
//
// It is generous because the shim's first act is spawning a real harness under a
// PTY on a possibly-loaded host, and it is BOUNDED because a launch that never
// produces a record must fail the accept rather than hold a capacity slot for a
// session that does not exist.
const defaultShimLaunchTimeout = 30 * time.Second

type shimAdoptionGate struct {
	done      chan struct{}
	once      sync.Once
	committed bool
}

func newShimAdoptionGate() *shimAdoptionGate { return &shimAdoptionGate{done: make(chan struct{})} }

func (g *shimAdoptionGate) finish(committed bool) {
	if g == nil {
		return
	}
	g.once.Do(func() {
		g.committed = committed
		close(g.done)
	})
}

func (g *shimAdoptionGate) await() bool {
	if g == nil {
		return true
	}
	<-g.done
	return g.committed
}

// shimRecordPollInterval is how often the launch path re-reads the registry
// while waiting for the new shim's record.
const shimRecordPollInterval = 25 * time.Millisecond

// shimOwnsSession is the §D11 SELECTION rule: which sessions this daemon
// launches under per-session shim ownership.
//
// Two conditions, both required. Ownership must be enabled — §D11 sequences
// shipping the protocol (step 1) ahead of taking ownership of live terminals
// (step 2), and an operator has to be able to roll the release out before it
// changes who owns a PTY. And the session must be INTERACTIVE: the first
// delivery is interactive-only, because that is the session class whose value is
// destroyed by a daemon restart. A headless worker that dies with its daemon is
// re-dispatched; a human's terminal is not.
func (d *Daemon) shimOwnsSession(spec SessionSpec) bool {
	if !d.sessionShimConfig().EnableOwnership {
		return false
	}
	return spec.Mode == interactiveRunMode
}

// launchSessionShim is the spawner's shim launch path (SpawnerOptions.ShimSpawn).
//
// It returns (nil, nil) for a session this daemon does not own through a shim,
// which is the signal the spawner reads as "use the ordinary direct-child
// spawn". Returning an error fails the accept.
//
// The sequence is: launch a DETACHED worker carrying the launch contract, let go
// of it entirely, then discover and adopt it over shimwire. The daemon never
// holds the exec.Cmd, the PTY fd, or a *ptyhost.Session (§D1) — after this
// function returns, everything it knows about the session travels over a socket
// it can drop and re-dial.
func (d *Daemon) launchSessionShim(spec SessionSpec, project ProjectConfig, env []string) (*SessionHandle, error) {
	if !d.shimOwnsSession(spec) {
		return nil, nil
	}
	cfg := d.sessionShimConfig()
	if err := cfg.validateSnapshotCarrier(); err != nil {
		return nil, err
	}
	if d.sessionShimAttestationErr != nil {
		return nil, d.sessionShimAttestationErr
	}
	id := sessionshim.Identity{OrgID: cfg.orgIDForSession(spec), SessionID: spec.SessionID}
	if err := id.Validate(); err != nil {
		return nil, fmt.Errorf("session shim: %w", err)
	}
	registry, err := d.sessionShimRegistry()
	if err != nil {
		return nil, err
	}
	if err := cfg.Orphan.Validate(); err != nil {
		return nil, fmt.Errorf("session shim: %w", err)
	}

	workarea := d.sessionWorkareaPath(spec)
	launch := sessionshim.Launch{
		Identity:     id,
		RegistryDir:  registry.Dir(),
		Orphan:       cfg.Orphan,
		ProcessEpoch: 1,
	}

	started, err := d.startShimProcess(spec, launch, env)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.launchTimeout())
	defer cancel()

	rec, err := awaitShimRecord(ctx, registry, id)
	if err != nil {
		// The shim never announced itself. Nothing is adopted and nothing is
		// counted; the process is left alone rather than signalled, because a
		// launch this daemon cannot identify is exactly the target §D10 forbids
		// guessing at. Its own orphan deadline is the escape hatch.
		slog.Error("session shim: launched worker never published a discovery record",
			"session", id.String(), "pid", started, "error", err)
		return nil, fmt.Errorf("session shim: %s: %w", id, err)
	}

	var (
		prepared       sessionshim.PreparedAdoption
		preparedHostID string
	)
	controllerOpts := sessionshim.ControllerOptions{
		ControllerID:          d.controllerID(),
		ExpectedWorkarea:      workarea,
		DialTimeout:           cfg.launchTimeout(),
		RequireFullHostFrames: cfg.RequireAuthoritativeSnapshot && d.sessionShimAttestationValue.enabled(),
		Logger:                slog.Default(),
		PrepareAdoption: func(evidence sessionshim.AdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			hostID, hostErr := d.sessionShimHostID(ctx, evidence.Identity.OrgID)
			if hostErr != nil {
				return sessionshim.PreparedAdoption{}, hostErr
			}
			resolved, prepareErr := d.prepareSessionShimAdoption(ctx, hostID, evidence)
			if prepareErr != nil {
				return sessionshim.PreparedAdoption{}, prepareErr
			}
			preparedHostID = hostID
			prepared = resolved
			return resolved, nil
		},
	}
	ctrl, err := sessionshim.Dial(ctx, rec, controllerOpts)
	if err != nil {
		slog.Error("session shim: could not adopt the shim it just launched",
			"session", id.String(), "error", err)
		return nil, fmt.Errorf("session shim: adopt %s: %w", id, err)
	}
	evidence, err := d.sessionShimAdoptionEvidence(ctx, ctrl, prepared, preparedHostID)
	if err != nil {
		_ = ctrl.Close()
		return nil, fmt.Errorf("session shim: resolve adoption host %s: %w", id, err)
	}
	gate := newShimAdoptionGate()
	d.consumeShimEventsGated(ctrl, gate)
	receipt, err := d.completeSessionShimAdoption(ctx, evidence)
	evidence.SnapshotProxy.deactivate()
	if err != nil {
		d.cancelStagedSessionShimSnapshot(id)
		gate.finish(false)
		_ = ctrl.Close()
		return nil, fmt.Errorf("session shim: durable adoption %s: %w", id, err)
	}
	// SnapshotProxy exists only for the synchronous carrier takeover callback.
	// Published daemon state uses the stable lookup APIs instead.
	evidence.SnapshotProxy = nil
	if cfg.OnAdoptionPublished != nil {
		d.setState(StateRecovering)
		d.shims.mu.Lock()
		d.shims.carrierActivationComplete = false
		d.shims.mu.Unlock()
	}
	batchReceipt, err := d.completeLaunchedSessionShimAdoptionBatch(ctx, evidence, receipt)
	if err != nil {
		d.failPendingSessionShimActivations()
		gate.finish(false)
		_ = ctrl.Close()
		return nil, fmt.Errorf("session shim: durable adoption batch %s: %w", id, err)
	}
	if err := d.updateSessionShimAdoptionRevision(id.OrgID, batchReceipt.AdoptionRevision); err != nil {
		d.failPendingSessionShimActivations()
		gate.finish(false)
		_ = ctrl.Close()
		return nil, fmt.Errorf("session shim: retain adoption revision %s: %w", id, err)
	}
	d.shims.mu.Lock()
	if len(batchReceipt.DurableCorrelation) > 0 {
		d.shims.batchReceipts[id.OrgID] = batchReceipt
	}
	d.shims.mu.Unlock()

	handle := d.trackLaunchedShim(ctrl, spec, project, workarea, evidence, receipt, false)
	gate.finish(true)
	if cfg.OnAdoptionPublished != nil {
		d.shims.mu.RLock()
		published := make(map[sessionshim.Identity]adoptedShim, len(d.shims.adopted))
		for identity, entry := range d.shims.adopted {
			published[identity] = entry
		}
		d.shims.mu.RUnlock()
		if activationErr := d.activatePublishedSessionShimCarriers(ctx, published); activationErr != nil {
			// The harness and durable adoption are already real. Preserve the claim
			// and visible capacity while withholding further claims; returning a
			// launch failure here would invite a duplicate session.
			slog.Error("session shim: post-publication carrier activation failed",
				"session", id.String(), "error", activationErr)
			d.failPendingSessionShimActivations()
			_ = ctrl.Close()
			return &handle, nil
		}
		d.setState(StateRunning)
	}
	slog.Info("session shim: launched and adopted an interactive session",
		"session", id.String(), "shimId", ctrl.Hello().ShimID,
		"generation", ctrl.Generation(), "harnessPid", ctrl.HarnessIdentity().PID)
	return &handle, nil
}

// startShimProcess execs the worker as a detached shim and then RELEASES it.
//
// Release is the ownership move made concrete. os/exec would otherwise leave the
// daemon as the process's parent and waiter, which is precisely the coupling
// §D1 removes: a daemon that still had to reap this process could not be
// replaced without ending it.
func (d *Daemon) startShimProcess(spec SessionSpec, launch sessionshim.Launch, env []string) (int, error) {
	command := d.shimCommand()
	if len(command) == 0 {
		return 0, errors.New("session shim: no worker command is configured to launch a shim with")
	}
	cmd := exec.Command(command[0], command[1:]...) //nolint:gosec // G204: operator-configured worker command, same source as the direct-spawn path
	configureShimProcess(cmd)
	cmd.Env = append(append([]string(nil), env...), envPairs(launch.Env())...)

	// A shim outlives this daemon, so it cannot inherit this daemon's stdio: a
	// closed pipe after the daemon exits would hand the shim EPIPE on its own
	// logging. It gets the null device and speaks over its socket instead.
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, fmt.Errorf("session shim: open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("session shim: start %s: %w", spec.SessionID, err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		// Release failing leaves this daemon as the waiter, which contradicts the
		// ownership boundary. Report it rather than proceeding as if the shim were
		// independent — the launch is still usable, but the claim would not be true.
		slog.Warn("session shim: could not release the launched process",
			"session", spec.SessionID, "pid", pid, "error", err)
	}
	return pid, nil
}

// shimCommand is the argv used to launch a shim. It is the SAME worker command
// the direct-spawn path uses: the shim is the worker process, told by its
// environment to own its PTY instead of being owned (§D1 — "the shim process
// contains the runner-side interactive driver and ptyhost.Session").
func (d *Daemon) shimCommand() []string {
	if d.spawner == nil {
		return nil
	}
	return d.spawner.opts.WorkerCommand
}

// sessionWorkareaPath is the workarea this daemon believes a session runs in.
// Adoption compares it against the shim's own self-report, so a shim running
// somewhere else quarantines instead of being taken over (§D7).
func (d *Daemon) sessionWorkareaPath(spec SessionSpec) string {
	if d.spawner == nil || d.spawner.opts.WorktreeParentDir == "" {
		return ""
	}
	return filepath.Join(d.spawner.opts.WorktreeParentDir, spec.SessionID)
}

// awaitShimRecord polls the registry until the launched shim publishes a valid
// discovery record, or ctx expires.
func awaitShimRecord(ctx context.Context, registry *sessionshim.Registry, id sessionshim.Identity) (sessionshim.Record, error) {
	ticker := time.NewTicker(shimRecordPollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		rec, err := registry.Get(id)
		if err == nil {
			return rec, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return sessionshim.Record{}, fmt.Errorf("waiting for discovery record: %w (last read: %v)", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

// trackLaunchedShim records a newly adopted controller and starts consuming its
// event stream.
func (d *Daemon) trackLaunchedShim(
	ctrl *sessionshim.Controller,
	spec SessionSpec,
	project ProjectConfig,
	workarea string,
	evidence SessionShimAdoptionEvidence,
	receipt SessionShimAdoptionReceipt,
	startConsumer bool,
) SessionHandle {
	handle := SessionHandle{
		SessionID:  spec.SessionID,
		PID:        ctrl.HarnessIdentity().PID,
		AcceptedAt: d.shimNow().UTC().Format(time.RFC3339),
		State:      SessionRunning,
		// The workarea doubles as the worktree path a local reader joins with
		// .agent/…; it is the same <parent>/<sessionID> leaf the direct path
		// publishes, so a reader cannot tell shim-backed sessions apart by shape.
		WorktreePath: workarea,
		ProjectName:  project.ID,
		Repository:   spec.Repository,
	}
	entry := adoptedShim{
		controller:      ctrl,
		shimID:          ctrl.Hello().ShimID,
		handle:          handle,
		spec:            spec,
		launched:        true,
		adoption:        evidence,
		adoptionReceipt: cloneSessionShimAdoptionReceipt(receipt),
	}
	d.shims.mu.Lock()
	d.shims.adopted[ctrl.Identity()] = entry
	d.shims.correlations[shimIncarnationFor(evidence)] = sessionShimAdoptionCorrelation{
		evidence: evidence,
		receipt:  cloneSessionShimAdoptionReceipt(receipt),
	}
	d.shims.mu.Unlock()
	// A shim-backed session is external to the direct-child registry, but it is
	// still one accepted WorkerSpawner lifecycle. Emit Started only after the
	// controller and handle are published, and before starting the consumer, so
	// an immediately available immutable Exit can never overtake it.
	if d.spawner != nil {
		d.spawner.emit(SessionEvent{Kind: SessionEventStarted, Handle: handle, Spec: spec})
	}
	if startConsumer {
		d.consumeShimEvents(ctrl)
	}
	return handle
}

// consumeShimEvents drains one adopted session's stream in the background.
//
// This is the daemon's half of stop/input/OUTPUT after adoption. It exists as a
// real consumer rather than a drop-on-the-floor because the shim's ring is
// bounded: a controller that never reads and never acknowledges makes every
// later adoption resume further back, and eventually forces the honest-but-
// avoidable Gap that §D5 makes the shim declare.
func (d *Daemon) consumeShimEvents(ctrl *sessionshim.Controller) {
	d.consumeShimEventsGated(ctrl, nil)
}

func (d *Daemon) consumeShimEventsGated(ctrl *sessionshim.Controller, gate *shimAdoptionGate) {
	d.shims.wg.Add(1)
	go func() {
		defer d.shims.wg.Done()
		id := ctrl.Identity()
		cfg := d.sessionShimConfig()
		observe := cfg.OnSessionEvent
		durable := cfg.OnSessionEventDurable
		fullHostFrames := ctrl.SupportsFullHostFrames()
		legacyDurability := !cfg.RequireAuthoritativeSnapshot
		var lastSeq uint64
		for ev := range ctrl.Events() {
			if observe != nil {
				// Forward first, bookkeep second: a carrier must see output in the
				// order the shim produced it, and acknowledging a sequence this
				// daemon has not actually handed on would make the resume point a
				// claim rather than a record.
				observe(id, ev)
			}
			switch ev.Kind {
			case sessionshim.EventOutput:
				if ev.Seq > lastSeq {
					lastSeq = ev.Seq
					if durable != nil && legacyDurability {
						if err := durable(id, ev); err != nil {
							slog.Warn("session shim: durable carrier rejected output",
								"session", id.String(), "seq", ev.Seq, "error", err)
							// A later frame must never advance the cursor past this
							// unacknowledged one. Close the controller and let the
							// normal disconnect/quarantine path retain ownership.
							_ = ctrl.Close()
							return
						}
						if err := d.recordShimForwardedSeqForController(id, ctrl, ev.Seq); err != nil {
							slog.Warn("session shim: durable output acknowledgement was not persisted",
								"session", id.String(), "seq", ev.Seq, "error", err)
							_ = ctrl.Close()
							return
						}
					}
				}
			case sessionshim.EventGap:
				// Surfaced, never smoothed over (§D5). A daemon that logged nothing
				// here would be claiming contiguous output it does not possess.
				slog.Warn("session shim: output gap declared by the shim",
					"session", id.String(), "fromSeq", ev.Gap.FromSeq,
					"toSeq", ev.Gap.ToSeq, "reason", ev.Gap.Reason)
				if fullHostFrames && durable != nil {
					if err := durable(id, ev); err != nil {
						slog.Warn("session shim: durable carrier rejected output gap",
							"session", id.String(), "fromSeq", ev.Gap.FromSeq,
							"toSeq", ev.Gap.ToSeq, "error", err)
						_ = ctrl.Close()
						return
					}
				}
			case sessionshim.EventHostFrame:
				if ev.Seq == 0 || ev.Seq <= lastSeq {
					_ = ctrl.Close()
					return
				}
				lastSeq = ev.Seq
				if d.isStagedSessionShimSnapshot(id, ev) {
					if durable == nil {
						_ = ctrl.Close()
						return
					}
					if err := durable(id, ev); err != nil {
						slog.Warn("session shim: durable carrier rejected staged Snapshot",
							"session", id.String(), "seq", ev.Seq, "error", err)
						_ = ctrl.Close()
						return
					}
					activationGate, retained := d.retainStagedSessionShimSnapshot(id, ev)
					if !retained || !activationGate.await() {
						return
					}
					continue
				}
				if durable != nil {
					if err := durable(id, ev); err != nil {
						slog.Warn("session shim: durable carrier rejected host frame",
							"session", id.String(), "seq", ev.Seq, "type", ev.FrameType, "error", err)
						_ = ctrl.Close()
						return
					}
					if err := d.recordShimForwardedSeqForController(id, ctrl, ev.Seq); err != nil {
						slog.Warn("session shim: durable HostFrame acknowledgement was not persisted",
							"session", id.String(), "seq", ev.Seq, "error", err)
						_ = ctrl.Close()
						return
					}
				}
				if ev.FrameType == attachwire.TypeExit {
					slog.Info("session shim: terminal observation received",
						"session", id.String(), "exitCode", ev.Exit.ExitCode, "signal", ev.Exit.Signal)
					if !gate.await() {
						return
					}
					d.finishAdoptedShim(id, ev.Exit)
				}
			case sessionshim.EventExit:
				slog.Info("session shim: terminal observation received",
					"session", id.String(), "exitCode", ev.Exit.ExitCode, "signal", ev.Exit.Signal)
				if !gate.await() {
					return
				}
				d.finishAdoptedShim(id, ev.Exit)
			case sessionshim.EventError:
				slog.Warn("session shim: error frame",
					"session", id.String(), "code", ev.Err.Code, "detail", ev.Err.Detail)
			case sessionshim.EventSnapshot:
				// State after a gap or on request; the sequence it carries is the
				// resume point a later adoption starts from.
				if ev.Snapshot.AtSeq > lastSeq {
					lastSeq = ev.Snapshot.AtSeq
					if durable != nil && legacyDurability {
						if err := durable(id, ev); err != nil {
							slog.Warn("session shim: durable carrier rejected snapshot",
								"session", id.String(), "seq", ev.Snapshot.AtSeq, "error", err)
							// A later frame must never advance the cursor past this
							// unacknowledged snapshot. Close the controller and let the
							// normal disconnect/quarantine path retain ownership.
							_ = ctrl.Close()
							return
						}
						if err := d.recordShimForwardedSeqForController(id, ctrl, ev.Snapshot.AtSeq); err != nil {
							slog.Warn("session shim: durable Snapshot acknowledgement was not persisted",
								"session", id.String(), "seq", ev.Snapshot.AtSeq, "error", err)
							_ = ctrl.Close()
							return
						}
					}
				}
			case sessionshim.EventSnapshotFrame:
				// Selected-v2 only. Hosted full-frame composition never stages this
				// semantic event; selected v3 stages its one exact EventHostFrame.
				if ev.Seq > lastSeq {
					lastSeq = ev.Seq
					if durable != nil && legacyDurability {
						if err := durable(id, ev); err != nil {
							slog.Warn("session shim: durable carrier rejected emitted snapshot",
								"session", id.String(), "seq", ev.Seq, "error", err)
							_ = ctrl.Close()
							return
						}
						if err := d.recordShimForwardedSeqForController(id, ctrl, ev.Seq); err != nil {
							slog.Warn("session shim: durable emitted Snapshot acknowledgement was not persisted",
								"session", id.String(), "seq", ev.Seq, "error", err)
							_ = ctrl.Close()
							return
						}
					}
				}
			}
		}
		// The stream ended. If no Exit arrived, this daemon simply lost its
		// connection — the shim keeps the harness and starts its orphan clock. That
		// is NOT a terminal outcome and must not be reported as one.
		if gate.await() {
			d.releaseShimIfLive(id)
		}
	}()
}

type sessionShimCursorAcknowledger interface {
	SupportsFullHostFrames() bool
	Heartbeat(uint64) error
}

func (d *Daemon) recordShimForwardedSeqForController(
	id sessionshim.Identity,
	ctrl sessionShimCursorAcknowledger,
	seq uint64,
) error {
	fullHostFrames := ctrl != nil && ctrl.SupportsFullHostFrames()
	if fullHostFrames {
		if err := ctrl.Heartbeat(seq); err != nil {
			return err
		}
	}
	d.shims.mu.Lock()
	if d.shims.forwarded == nil {
		d.shims.forwarded = make(map[sessionshim.Identity]uint64)
	}
	if seq > d.shims.forwarded[id] {
		d.shims.forwarded[id] = seq
	}
	d.shims.mu.Unlock()
	if ctrl != nil && !fullHostFrames {
		_ = ctrl.Heartbeat(seq)
	}
	return nil
}

// SessionShimForwardedSeq reports the highest output sequence this daemon has
// durably forwarded for a session — its resume position for a later adoption.
func (d *Daemon) SessionShimForwardedSeq(orgID, sessionID string) uint64 {
	if d.shims == nil {
		return 0
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return d.shims.forwarded[sessionshim.Identity{OrgID: orgID, SessionID: sessionID}]
}

// finishAdoptedShim is terminal cleanup after adoption (§D8/§D10).
//
// The order matters and is the conservative one: drop the live entry so capacity
// is released, record the tombstone as the durable proof of what happened, and
// only THEN dispose of it. Disposing first would destroy the one artifact that
// can prove the harness group was reaped, turning a proven death back into an
// unresolved one for anything that asks later.
func (d *Daemon) finishAdoptedShim(id sessionshim.Identity, exit shimwire.ExitMsg) {
	d.shims.mu.Lock()
	entry, ok := d.shims.adopted[id]
	if !ok || entry.terminal {
		d.shims.mu.Unlock()
		return
	}
	entry.terminal = true
	d.shims.adopted[id] = entry
	registry := d.shims.registry
	d.shims.mu.Unlock()

	// OnPreSpawn transferred cleanup ownership when this daemon launched the
	// shim. Deliver the same ordinary lifecycle listeners as a direct child, but
	// only for the immutable Exit frame. A controller disconnect never reaches
	// this function and therefore never fabricates a terminal outcome.
	if entry.launched && d.spawner != nil {
		handle := entry.handle
		var exitErr error
		switch {
		case exit.Signal != "":
			handle.State = SessionTerminated
			exitErr = fmt.Errorf("session shim exited after signal %s", exit.Signal)
		case exit.ExitCode != 0:
			handle.State = SessionFailed
			exitErr = fmt.Errorf("session shim exited with code %d", exit.ExitCode)
		default:
			handle.State = SessionCompleted
		}
		d.spawner.emit(SessionEvent{
			Kind:    SessionEventEnded,
			Handle:  handle,
			Spec:    entry.spec,
			ExitErr: exitErr,
		})
	}

	// Retain the exact generation through synchronous listener delivery. A
	// replacement must never be deleted by a stale terminal consumer.
	d.shims.mu.Lock()
	current, stillOwned := d.shims.adopted[id]
	if stillOwned && current.controller == entry.controller && current.terminal {
		delete(d.shims.adopted, id)
		delete(d.shims.forwarded, id)
	} else {
		stillOwned = false
	}
	d.shims.mu.Unlock()
	if !stillOwned {
		return
	}
	if entry.controller != nil {
		_ = entry.controller.Close()
	}
	if registry == nil {
		return
	}
	// The shim emits Exit and THEN durably publishes the tombstone, so an
	// immediate read races the write. Poll briefly rather than concluding the
	// proof is missing: treating an in-flight tombstone as absent would leave a
	// session that provably ended parked in reconciliation forever.
	tombstone, err := awaitTombstone(registry, id, entry.shimID, entry.adoption.ProcessEpoch, tombstoneSettleWindow)
	if err != nil {
		slog.Warn("session shim: terminal observation without a durable tombstone",
			"session", id.String(), "error", err)
		return
	}
	d.shims.mu.Lock()
	d.shims.tombstoned = append(d.shims.tombstoned, tombstone)
	d.shims.mu.Unlock()

	hostID, hostErr := d.sessionShimHostID(context.Background(), id.OrgID)
	if hostErr != nil {
		slog.Warn("session shim: retain terminal tombstone after host identity resolution failed",
			"session", id.String(), "error", hostErr)
		return
	}
	adoption := entry.adoption
	terminalEvidence := SessionShimTerminalEvidence{
		Identity:                   id,
		HostID:                     hostID,
		ShimID:                     tombstone.ShimID,
		ProcessEpoch:               tombstone.ProcessEpoch,
		Adoption:                   &adoption,
		DurableAdoptionCorrelation: append([]byte(nil), entry.adoptionReceipt.DurableCorrelation...),
		Tombstone:                  tombstone,
	}
	if err := d.reportSessionShimTerminalEvidence(context.Background(), terminalEvidence); err != nil {
		// The harness group is already gone, so capacity may be released, but the
		// proof is retained on disk. A later startup retries the exact durable
		// handoff before readiness; disposing it here would strand the external
		// claim in reconciliation with no evidence left to replay.
		slog.Warn("session shim: retain terminal tombstone after durable evidence refusal",
			"session", id.String(), "error", err)
		return
	}
	d.shims.mu.Lock()
	delete(d.shims.correlations, shimIncarnationFor(entry.adoption))
	d.shims.mu.Unlock()

	verdict := d.SessionShimReleaseDecision(id.OrgID, id.SessionID, d.SessionShimTerminalProof(id.OrgID, id.SessionID))
	if verdict != sessionshim.ReleaseAllowed {
		slog.Info("session shim: terminal outcome recorded but the claim is not releasable yet",
			"session", id.String(), "verdict", verdict)
		return
	}
	// Withdraw the liveness claim BEFORE disposing the proof, and never the other
	// way round. A shim publishes its tombstone and then removes its discovery
	// record, so those two writes can be observed apart — and a crash between them
	// leaves both on disk by design. Disposing the tombstone first would collapse
	// "terminal, proven" into "a record whose process is gone", which §D10
	// classifies as stale and leaves unresolved. Remove is idempotent, so the
	// ordinary case where the shim already withdrew its own record costs nothing.
	if err := registry.RemoveIncarnation(id, tombstone.ShimID, tombstone.ProcessEpoch); err != nil {
		slog.Warn("session shim: withdraw discovery record", "session", id.String(), "error", err)
		return
	}
	// Audited disposal: the outcome is durably recorded above and no liveness
	// claim survives it, so the tombstone has done its job and may go.
	if err := registry.RemoveTombstoneIncarnation(tombstone); err != nil {
		slog.Warn("session shim: dispose tombstone", "session", id.String(), "error", err)
	}
}

// tombstoneSettleWindow bounds the wait for a shim's tombstone after its Exit
// frame arrives. The shim writes it immediately afterwards (one Alive() probe
// and one atomic publish), so this is a settle window, not a retry budget.
const tombstoneSettleWindow = 5 * time.Second

// awaitTombstone polls for the durable terminal proof a shim leaves behind.
func awaitTombstone(
	registry *sessionshim.Registry,
	id sessionshim.Identity,
	shimID string,
	processEpoch uint64,
	within time.Duration,
) (sessionshim.Tombstone, error) {
	deadline := time.Now().Add(within)
	var lastErr error
	for {
		t, err := registry.GetTombstoneIncarnation(id, shimID, processEpoch)
		if err == nil {
			live, liveErr := registry.HasIncarnation(id, shimID, processEpoch)
			if liveErr == nil && !live {
				return t, nil
			}
			if liveErr != nil {
				lastErr = liveErr
			} else {
				lastErr = errors.New("matching live discovery record has not been withdrawn")
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			return sessionshim.Tombstone{}, lastErr
		}
		time.Sleep(shimRecordPollInterval)
	}
}

// releaseShimIfLive withdraws authority from a controller whose stream ended
// without a terminal observation and moves the exact live shim into visible,
// capacity-consuming quarantine. The session is NOT reported as ended: only a
// terminal receipt or matching tombstone closes the loop (§D7/§D10).
func (d *Daemon) releaseShimIfLive(id sessionshim.Identity) {
	now := d.shimNow()
	d.shims.mu.Lock()
	entry, ok := d.shims.adopted[id]
	if ok {
		delete(d.shims.adopted, id)
		hello := entry.controller.Hello()
		q := sessionshim.NewQuarantinedSession(sessionshim.Record{
			OrgID:             id.OrgID,
			SessionID:         id.SessionID,
			ShimID:            entry.shimID,
			ProcessEpoch:      hello.ProcessEpoch,
			ProtocolMin:       hello.Min,
			ProtocolMax:       hello.Max,
			Phase:             hello.Phase,
			CreatedAtUnixNano: now.UnixNano(),
		}, sessionshim.QuarantineSocketUnreachable,
			"controller stream ended before a terminal observation", now)
		q.ControllerGeneration = uint64(entry.controller.Generation())
		d.upsertShimQuarantineLocked(q)
	}
	d.shims.mu.Unlock()
	if ok && entry.controller != nil {
		_ = entry.controller.Close()
		slog.Info("session shim: controller connection ended without a terminal observation; the shim retains its harness",
			"session", id.String())
	}
}

// upsertShimQuarantineLocked adds one exact shim projection without allowing a
// repeated disconnect observer to double-charge capacity. Lifecycle identity is
// authoritative, while shim id distinguishes the D7 duplicate-identity case in
// which two real survivors under one identity must both remain visible.
// d.shims.mu must be held.
func (d *Daemon) upsertShimQuarantineLocked(q sessionshim.QuarantinedSession) {
	for i := range d.shims.quarantined {
		current := d.shims.quarantined[i]
		if current.Identity() == q.Identity() && current.ShimID == q.ShimID && current.ProcessEpoch == q.ProcessEpoch {
			d.shims.quarantined[i] = q
			sessionshim.SortQuarantined(d.shims.quarantined)
			return
		}
	}
	d.shims.quarantined = append(d.shims.quarantined, q)
	sessionshim.SortQuarantined(d.shims.quarantined)
}

// reconcileQuarantinedTombstones withdraws capacity only when a durable
// tombstone for the exact quarantined shim proves its harness group was reaped.
// It is called by the occupancy/reporting surfaces that already run on every
// admission and heartbeat, so reconciliation needs no unbounded background
// goroutine and cannot delay intentional daemon shutdown.
func (d *Daemon) reconcileQuarantinedTombstones() {
	if d.shims == nil {
		return
	}
	d.shims.mu.RLock()
	registry := d.shims.registry
	quarantined := append([]sessionshim.QuarantinedSession(nil), d.shims.quarantined...)
	d.shims.mu.RUnlock()
	if registry == nil || len(quarantined) == 0 {
		return
	}

	for _, q := range quarantined {
		id := q.Identity()
		tombstone, err := registry.GetTombstoneIncarnation(id, q.ShimID, q.ProcessEpoch)
		if err != nil || tombstone.ShimID != q.ShimID || tombstone.ProcessEpoch != q.ProcessEpoch {
			continue
		}
		proof := d.SessionShimTerminalProof(id.OrgID, id.SessionID)
		if d.SessionShimReleaseDecision(id.OrgID, id.SessionID, proof) != sessionshim.ReleaseAllowed {
			continue
		}
		key := shimIncarnation{identity: id, shimID: tombstone.ShimID, processEpoch: tombstone.ProcessEpoch}
		d.shims.mu.RLock()
		correlation, hasCorrelation := d.shims.correlations[key]
		d.shims.mu.RUnlock()
		hostID, hostErr := d.sessionShimHostID(context.Background(), id.OrgID)
		if hostErr != nil {
			slog.Warn("session shim: retain quarantined terminal proof after host identity resolution failed",
				"session", id.String(), "error", hostErr)
			continue
		}
		terminalEvidence := SessionShimTerminalEvidence{
			Identity:     id,
			HostID:       hostID,
			ShimID:       tombstone.ShimID,
			ProcessEpoch: tombstone.ProcessEpoch,
			Tombstone:    tombstone,
		}
		if hasCorrelation {
			adoption := correlation.evidence
			terminalEvidence.Adoption = &adoption
			terminalEvidence.DurableAdoptionCorrelation = append(
				[]byte(nil), correlation.receipt.DurableCorrelation...)
		}
		if err := d.reportSessionShimTerminalEvidence(context.Background(), terminalEvidence); err != nil {
			slog.Warn("session shim: retain quarantined terminal proof after durable evidence refusal",
				"session", id.String(), "error", err)
			continue
		}

		d.shims.mu.Lock()
		removed := false
		kept := d.shims.quarantined[:0]
		for _, current := range d.shims.quarantined {
			if current.Identity() == id && current.ShimID == tombstone.ShimID && current.ProcessEpoch == tombstone.ProcessEpoch {
				removed = true
				continue
			}
			kept = append(kept, current)
		}
		d.shims.quarantined = kept
		if removed {
			delete(d.shims.forwarded, id)
			delete(d.shims.correlations, key)
			alreadyRecorded := false
			for _, existing := range d.shims.tombstoned {
				if existing.Identity() == id && existing.ShimID == tombstone.ShimID && existing.ProcessEpoch == tombstone.ProcessEpoch {
					alreadyRecorded = true
					break
				}
			}
			if !alreadyRecorded {
				d.shims.tombstoned = append(d.shims.tombstoned, tombstone)
			}
		}
		d.shims.mu.Unlock()
		if removed {
			if err := registry.RemoveTombstoneIncarnation(tombstone); err != nil {
				slog.Warn("session shim: dispose quarantined tombstone after durable terminal handoff",
					"session", id.String(), "error", err)
			}
		}
	}
}

// WriteAdoptedSessionShimInput sends attributed input bytes to one adopted
// session under this daemon's generation.
//
// Refusing a session this daemon has not adopted is the same rule Stop follows:
// quarantine grants no input authority (§D7), and reaching a quarantined shim by
// another route is exactly the "kill instead of quarantine" behaviour the ADR
// rejects, in a quieter form.
func (d *Daemon) WriteAdoptedSessionShimInput(orgID, sessionID string, data []byte) error {
	entry, err := d.adoptedShimEntry(orgID, sessionID)
	if err != nil {
		return err
	}
	return entry.controller.WriteInput(data)
}

// ResizeAdoptedSessionShim sends authoritative geometry under this daemon's
// generation.
func (d *Daemon) ResizeAdoptedSessionShim(orgID, sessionID string, cols, rows, pxWidth, pxHeight uint32) error {
	entry, err := d.adoptedShimEntry(orgID, sessionID)
	if err != nil {
		return err
	}
	return entry.controller.Resize(cols, rows, pxWidth, pxHeight)
}

// adoptedShimEntry resolves one adopted session with a live controller.
func (d *Daemon) adoptedShimEntry(orgID, sessionID string) (adoptedShim, error) {
	if d.shims == nil {
		return adoptedShim{}, errors.New("session shim: adoption is not configured")
	}
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	d.shims.mu.RLock()
	entry, ok := d.shims.adopted[id]
	d.shims.mu.RUnlock()
	if !ok {
		return adoptedShim{}, fmt.Errorf("session shim: %s is not adopted by this daemon", id)
	}
	if entry.controller == nil {
		return adoptedShim{}, fmt.Errorf("session shim: %s has no live controller connection", id)
	}
	return entry, nil
}

type sessionShimStopResult uint8

const (
	sessionShimStopNotFound sessionShimStopResult = iota
	sessionShimStopHandled
	sessionShimStopRefused
)

// stopSessionShimByID routes a plain session id — what the control API and the
// spawner speak — to the generation-fenced Stop on an adopted shim.
//
// It reports whether the session was shim-owned at all, so the caller can fall
// through to the direct-child path for everything else rather than having to
// know which ownership model a session uses.
func (d *Daemon) stopSessionShimByID(sessionID string) sessionShimStopResult {
	if d.shims == nil {
		return sessionShimStopNotFound
	}
	d.shims.mu.RLock()
	matches := make([]sessionshim.Identity, 0, 1)
	for id := range d.shims.adopted {
		if id.SessionID == sessionID {
			matches = append(matches, id)
		}
	}
	d.shims.mu.RUnlock()
	if len(matches) == 0 {
		return sessionShimStopNotFound
	}
	if len(matches) != 1 {
		// The localhost API supplies only a session id. Do not collapse two
		// tenant-scoped lifecycle identities or pick one by map order.
		slog.Error("session shim: bare session id is ambiguous across organizations",
			"matches", len(matches))
		return sessionShimStopRefused
	}
	id := matches[0]
	if err := d.StopAdoptedSessionShim(id.OrgID, id.SessionID, shimwire.StopOperator); err != nil {
		slog.Error("session shim: stop", "session", id.String(), "error", err)
		return sessionShimStopRefused
	}
	return sessionShimStopHandled
}

// sessionShimHandles projects every shim-backed session onto the same
// SessionHandle shape the direct-spawn path publishes.
//
// Quarantined shims are included, and that is §D7 held to at the surface an
// operator actually looks at: a quarantined shim is a running harness this
// daemon cannot control, and a session list that omitted it would show an
// occupied host as idle. Their state is "running" — because it is — with a PID
// of 0, since this daemon never negotiated with them and does not know one.
func (d *Daemon) sessionShimHandles() []SessionHandle {
	if d.shims == nil {
		return nil
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	out := make([]SessionHandle, 0, len(d.shims.adopted)+len(d.shims.quarantined))
	for id, entry := range d.shims.adopted {
		handle := entry.handle
		if handle.SessionID == "" {
			// Adopted at startup rather than launched here: this daemon has the
			// identity and the shim's report, not the original spec.
			handle = SessionHandle{SessionID: id.SessionID, State: SessionRunning}
			if entry.controller != nil {
				handle.PID = entry.controller.HarnessIdentity().PID
				if started := entry.controller.Hello().ProcessStartedAt; started > 0 {
					handle.AcceptedAt = time.Unix(0, started).UTC().Format(time.RFC3339)
				}
			}
		}
		out = append(out, handle)
	}
	for _, q := range d.shims.quarantined {
		out = append(out, SessionHandle{
			SessionID: q.SessionID,
			State:     SessionRunning,
		})
	}
	return out
}

// shimNow is the daemon's clock for shim bookkeeping, routed through the
// spawner's injectable clock so shim-backed and direct handles are stamped from
// the same source in tests.
func (d *Daemon) shimNow() time.Time {
	if d.spawner != nil && d.spawner.opts.Now != nil {
		return d.spawner.opts.Now()
	}
	return time.Now()
}

// sessionShimRegistry opens the registry once and reuses it for the daemon's
// lifetime, so the launch path and the startup adoption pass cannot end up
// pointed at two different directories.
func (d *Daemon) sessionShimRegistry() (*sessionshim.Registry, error) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	if d.shims.registry != nil {
		return d.shims.registry, nil
	}
	registry, err := sessionshim.NewRegistry(d.sessionShimConfig().RegistryDir)
	if err != nil {
		return nil, fmt.Errorf("session shim: open registry: %w", err)
	}
	d.shims.registry = registry
	return registry, nil
}

// envPairs renders an overlay map as KEY=VALUE entries, sorted so a spawn
// environment is byte-stable across runs.
func envPairs(overlay map[string]string) []string {
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+overlay[k])
	}
	return out
}

// waitShimConsumers joins every background event consumer. Tests use it to
// observe a fully settled daemon; Stop uses it so a shutdown cannot race a
// consumer that is still writing bookkeeping.
func (d *Daemon) waitShimConsumers() {
	if d.shims == nil {
		return
	}
	d.shims.wg.Wait()
}
