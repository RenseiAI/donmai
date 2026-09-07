package daemon

// session_shim_composition.go — installing a durable-session composition that
// an embedder finished AFTER the daemon was already serving.
//
// WHY THIS EXISTS
//
// Composing durable sessions is not free and not local: an embedder that offers
// them has to talk to its control plane before it can hand this package a
// SessionShimConfig at all. Doing that before New makes daemon readiness
// hostage to a round trip the host does not control — the control port binds
// only after Start returns, and Start cannot return before the configuration it
// was constructed with has been adopted. Measured on one machine minutes apart,
// the same binary reported ready in 3.2s when the composition failed fast and
// had not reported ready at 10s when it actually ran. The variable was never
// the host.
//
// So the composition moves off the readiness path. While it is pending the
// daemon declares an explicit stand-down (Options.SessionShimStandDown): its
// registration says "not running one", its heartbeat carries no shim
// projection, and the control plane expects none. Sessions launched in that
// window are direct-owned, which is exactly what they would be on a host whose
// composition failed — not a new state.
//
// When the composition resolves, InstallSessionShimComposition installs it,
// declares it, runs the startup adoption pass, and only then announces it
// through the heartbeat projection.
//
// THE DECLARATION FOUNDS HOST AUTHORITY
//
// The declaring refresh is the only round trip that may resolve the primary
// scope's stable host id and adoption revision, and it happens inside the
// credential refresher's lock like every other presentation. An embedder that
// learned the host id any other way — by presenting the composed attestation
// itself, ahead of the install — raced the running lanes: the refresher kept
// presenting the stand-down while the embedder presented the composition, and
// the control plane answered the flip-flop with an attestation conflict and a
// revoked credential. So the receipt the declaration answers with is handed to
// the embedder (AcquireRecoveryScopes' primary argument) before anything asks
// the embedder a question that needs it. The proof-v2 readiness check is that
// question: for the founding refresh alone it runs AFTER the receipt is
// retained and delivered, not before — deferred, never dropped.
//
// THE ORDER IS THE WHOLE POINT
//
// Nothing may announce the shim as ACTIVE before the adoption pass completes.
// A control plane's heartbeat preflight demands exact agreement on controller
// credential, attestation currency, adoption revision, and canonical quarantine
// JSON, and demotes a host that disagrees. An install that published its
// projection early would trip precisely that. Two things gate the announcement
// and nothing else does:
//
//   - the credential refresher's own copy of the registration attestation,
//     which stays stood-down until declareSessionShimComposition swaps it; and
//   - the heartbeat's projection hook, which stays nil until the adoption pass
//     has completed and been declared.
//
// Installing the configuration itself announces nothing. It is read by this
// process only.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// sessionShimCompositionRefreshReason is the classified refresh trigger for the
// round trip that moves this daemon from stand-down to its composed
// attestation. It is distinct from every expiry/rejection reason so an operator
// reading the [runtime-token] line can tell a declaration from a recovery.
const sessionShimCompositionRefreshReason = "session-shim-composition"

// sessionShimIdentity is one coherent generation of the durable-session facts a
// daemon presents: the configuration it was given and the controller id and
// host attestation derived from it. Resolution errors are carried in the tuple
// rather than returned, because New has no error return and the failure has to
// survive until Start (or a launch) can refuse on it.
//
// It is immutable once published. Installing a composition publishes a NEW
// tuple; it never edits the one readers may already hold.
type sessionShimIdentity struct {
	// config is held by pointer, not by value, and the pointee is never
	// written after the generation is published. The generation New publishes
	// points AT Options.SessionShim, so a caller that authored its options in
	// place still reaches the daemon it configured; a generation an install
	// publishes points at that installer's own copy. Swapping the pointer is
	// what makes the change visible atomically, and never writing through it is
	// what makes the change safe.
	config         *SessionShimConfig
	controllerID   string
	controllerErr  error
	attestation    SessionShimHostAttestation
	attestationErr error

	// pendingComposition marks the window between a composed configuration
	// being installed and its adoption pass completing. The configuration has
	// to be live during that window — the adoption pass reads it, and so does
	// every callback the adoption pass drives — but ownership of NEW sessions
	// must not be, because a session handed to a shim before the startup pass
	// has accounted for what is already running is a session admitted against
	// capacity nobody has counted yet.
	pendingComposition bool
}

// newSessionShimIdentity resolves one generation from a configuration.
// standDown turns the absence of a composed attestation from silence into an
// explicit declaration; it contradicts a composed attestation and says so.
func newSessionShimIdentity(cfg *SessionShimConfig, standDown bool) *sessionShimIdentity {
	if cfg == nil {
		cfg = &SessionShimConfig{}
	}
	id := &sessionShimIdentity{config: cfg}
	id.controllerID, id.controllerErr = resolveControllerID(*cfg)
	id.attestation, id.attestationErr = resolveSessionShimHostAttestation(*cfg, id.controllerID)
	if standDown && id.attestationErr == nil {
		if id.attestation.enabled() {
			id.attestationErr = errors.New(
				"session shim: stand-down contradicts the composed host attestation")
		} else {
			id.attestation = SessionShimStandDownAttestation()
		}
	}
	return id
}

// shimIdentity returns the generation currently in effect. It never returns
// nil: New always publishes one, and a zero-value tuple is the correct reading
// for a Daemon literal a test constructed without going through New.
func (d *Daemon) shimIdentity() *sessionShimIdentity {
	if id := d.shimIdentityRef.Load(); id != nil {
		return id
	}
	return &sessionShimIdentity{config: &SessionShimConfig{}}
}

// refreshSessionShimIdentity re-resolves the controller id and host attestation
// from the configuration currently held in Options. It exists for the caller
// that authored its options in place after New and needs the derived tuple to
// catch up; an installed composition publishes its own generation instead.
func (d *Daemon) refreshSessionShimIdentity() *sessionShimIdentity {
	id := newSessionShimIdentity(&d.opts.SessionShim, d.opts.SessionShimStandDown)
	d.shimIdentityRef.Store(id)
	return id
}

// sessionShimEnabled reports whether this daemon presents a composed host
// attestation — the exported reading is SessionShimHostAttestation().Supports().
func (d *Daemon) sessionShimEnabled() bool { return d.shimIdentity().attestation.enabled() }

// sessionShimAttestationError returns the failure that made the configured
// attestation unresolvable, if any.
func (d *Daemon) sessionShimAttestationError() error { return d.shimIdentity().attestationErr }

// sessionShimControllerIDError returns the failure that made the controller id
// unresolvable, if any.
func (d *Daemon) sessionShimControllerIDError() error { return d.shimIdentity().controllerErr }

// SessionShimCompositionPending reports whether a composed durable-session
// configuration is installed but its startup adoption pass has not finished.
// Operators reach it through the local control API's diagnostics; it is the
// difference between "this host does not do durable sessions" and "this host is
// still bringing them up".
func (d *Daemon) SessionShimCompositionPending() bool {
	return d.shimIdentity().pendingComposition
}

// InstallSessionShimComposition installs a durable-session composition that an
// embedder finished after New, brings it fully online, and declares it.
//
// The daemon must already be running and standing down. On success the
// composition is live: new interactive sessions are shim-owned, the control
// plane has been re-presented this host's full attestation on the identity its
// lanes already hold, and the heartbeat carries the adoption projection.
//
// On ANY failure the daemon is returned to the stand-down posture it was
// serving in — including a re-declaration of the stand-down when the failure
// happened after the attestation had already been presented. A host that could
// not bring durable sessions up keeps serving direct-owned sessions; it does
// not die, and it does not leave the control plane believing in a composition
// that is not there.
//
// ONE failure is CLASSIFIED for the caller rather than left opaque: a boot
// adoption batch the control plane refused for a reason no bounded recovery
// can settle comes back as *SessionShimDurabilityRefused, wrapped, so
// errors.As reaches it. It is still an error and it still means nothing was
// installed — but it is the one failure whose honest handling is "warn, do not
// announce durable sessions, keep serving direct-owned ones" rather than
// "exit". An embedder that exits on any error from this call keeps a working
// host by classifying it; an embedder that does not classify it behaves
// exactly as it did before. Every other failure is untyped exactly as it was,
// because a transport blip or an expired credential is recovered by the
// supervised restart that an error triggers, and disabling durable sessions
// for the life of the process over one would trade a transient outage for a
// permanent one.
//
// The refusal's scope, lineages, and reason are retained and readable from
// SessionShimDurabilityRefusal() and the diagnostics surface, so an operator
// who sees `off` can see why without the process log.
//
// It is safe to call from a goroutine started after Start returned. It admits
// one install at a time and refuses a second composition outright.
func (d *Daemon) InstallSessionShimComposition(ctx context.Context, cfg SessionShimConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !d.installingComposition.CompareAndSwap(false, true) {
		return errors.New("session shim: a composition install is already in flight")
	}
	defer d.installingComposition.Store(false)

	if state := d.State(); state != StateRunning {
		return fmt.Errorf("session shim: composition install requires a running daemon, not %q", state)
	}
	prior := d.shimIdentity()
	if prior.attestation.enabled() {
		return errors.New("session shim: this daemon has already declared a composition")
	}

	pending := newSessionShimIdentity(&cfg, false)
	pending.pendingComposition = true
	if pending.controllerErr != nil {
		return pending.controllerErr
	}
	if pending.attestationErr != nil {
		return pending.attestationErr
	}
	if !pending.attestation.enabled() {
		return errors.New("session shim: composition install requires a composed host attestation")
	}

	// Everything past this point is reversible, and the deferred rollback is
	// what makes it safe to install first and validate against the control
	// plane second. `declared` distinguishes the two rollbacks: before the
	// declaration the control plane never heard the attestation, after it the
	// stand-down has to be said again. It flips the moment the control plane
	// ACCEPTED the attestation — not when everything the declaration leads to
	// has also succeeded — because a refresher left presenting a composition
	// its daemon has abandoned is the flip-flop this file exists to prevent,
	// approached from the other side.
	installed := false
	declared := false
	defer func() {
		if installed {
			return
		}
		d.shimIdentityRef.Store(prior)
		d.discardSessionShimCompositionState()
		// Before the stand-down goes back on the wire: admission is local and
		// must not wait on a network round trip that is itself best-effort.
		d.restoreSessionShimAdmissionAfterFailedInstall()
		if declared {
			d.redeclareSessionShimStandDown(ctx, prior.attestation)
		}
	}()
	d.shimIdentityRef.Store(pending)

	var err error
	if declared, err = d.declareSessionShimComposition(ctx); err != nil {
		return err
	}

	if err := d.adoptSessionShims(ctx); err != nil {
		var refused *SessionShimDurabilityRefused
		if errors.As(err, &refused) {
			// DEGRADED, NOT TERMINAL — and said so in a way a caller can act
			// on. The control plane refused this scope's boot batch and every
			// bounded recovery for it is spent. The deferred rollback above has
			// already put this daemon back in the stand-down posture it was
			// serving in, which is a posture it serves correctly: direct-owned
			// sessions, no shim projection on the beat, a control plane that
			// expects none.
			//
			// The typed error is RETURNED rather than swallowed. An embedder
			// that exits on any error from this call keeps a working host by
			// classifying this one — `errors.As(err, &daemon.SessionShimDurabilityRefused{})`
			// — and an embedder that does not classify it behaves exactly as it
			// did before this change. Returning nil would instead make every
			// embedder announce a durable-session composition that is not
			// there, which is worse than the failure it was meant to fix.
			//
			// ONE loud line, because it is the line an operator greps for, and
			// the reason is retained on the diagnostics surface so `off` comes
			// with a why.
			slog.Error("session shim: DURABLE SESSIONS ARE OFF for this host — the control plane refused this scope's "+
				"boot adoption batch and every bounded recovery is spent; the daemon keeps serving direct-owned "+
				"sessions (shim-boot-dead-lineage-tolerance-2026-09-06)",
				"scope", refused.Scope, "lineages", refused.LineageIDs(), "refusal", refused.Err)
			d.retainSessionShimDurabilityRefusal(refused)
			return fmt.Errorf("session shim: composition adoption: %w", err)
		}
		return fmt.Errorf("session shim: composition adoption: %w", err)
	}
	d.clearSessionShimDurabilityRefusal()

	// Adoption is complete and durable. Publishing the configuration without
	// the pending flag is what opens ownership of new sessions.
	live := *pending
	live.pendingComposition = false
	d.shimIdentityRef.Store(&live)

	if err := d.publishSessionShimHeartbeatProjection(ctx); err != nil {
		return err
	}
	installed = true
	slog.Info("session shim: durable-session composition installed",
		"controller", live.controllerID, "scope", d.sessionShimConfig().orgID())
	return nil
}

// declareSessionShimComposition presents the composed attestation to the
// control plane on the registration this daemon already holds, retains the
// scoped receipts the adoption pass needs to resolve host authority, and then
// runs the readiness check the founding refresh deferred.
//
// It goes through the credential refresher rather than Register: a full
// re-registration mints a NEW worker identity and retires the current one,
// which is exactly the mutual-eviction shape RefreshRuntimeToken exists to
// avoid. The refresh re-presents this worker id with the new attestation and
// answers with the receipt.
//
// declared reports whether the control plane accepted the attestation, which
// is true for every failure past the round trip itself. The caller's rollback
// reads it: an accepted declaration has to be withdrawn, a refused one does
// not.
func (d *Daemon) declareSessionShimComposition(ctx context.Context) (declared bool, err error) {
	d.mu.RLock()
	credentials := d.credentials
	d.mu.RUnlock()
	if credentials == nil {
		return false, errors.New("session shim: composition install requires a registered daemon")
	}
	// The retention this refresh's receipt validation would otherwise demand is
	// the very next step: this round trip IS what establishes the scope's host
	// authority, so there is nothing retained yet to compare it against, and no
	// host id yet for the readiness resolver to answer about.
	d.shims.setDeclaringComposition(true)
	defer d.shims.setDeclaringComposition(false)

	result, err := credentials.DeclareSessionShim(
		ctx, d.SessionShimHostAttestation(), sessionShimCompositionRefreshReason,
	)
	if err != nil {
		return false, fmt.Errorf("session shim: declare composition: %w", err)
	}
	// From here on the control plane holds the composed attestation and every
	// lane presents the credential it minted for it. Whatever fails below
	// fails an install the control plane has already been told about.
	if err := d.acquireSessionShimRecoveryReceipts(ctx, result.SessionShim); err != nil {
		return true, err
	}
	// The primary receipt is retained and the embedder has been handed it. Only
	// now can a readiness resolver that answers for the primary host answer at
	// all. Not held under d.shims.mu: the resolver is embedder code.
	// Resolved rather than read from the cache: this is the first moment the
	// resolver can answer for this scope at all, so any earlier sample answered
	// a question about a host authority that did not yet exist.
	if err := d.sessionShimReadinessGate(sessionShimReadinessResolveNow); err != nil {
		return true, fmt.Errorf("session shim: founding declaration readiness: %w", err)
	}
	slog.Info("session shim: founding declaration resolved host authority",
		"scope", d.sessionShimConfig().orgID(),
		"workerHostId", result.SessionShim.WorkerHostID,
		"adoptionRevision", result.SessionShim.AdoptionRevision)
	return true, nil
}

// redeclareSessionShimStandDown puts the stand-down back on the wire after a
// failed install that had already presented the composed attestation. Best
// effort by construction: the install is already failing, and the alternative
// to a logged warning is taking the whole daemon down over a feature that was
// meant to be optional. The next ordinary refresh re-presents the stand-down
// anyway, because the refresher's attestation is restored here regardless of
// whether the round trip lands.
func (d *Daemon) redeclareSessionShimStandDown(ctx context.Context, attestation SessionShimHostAttestation) {
	d.mu.RLock()
	credentials := d.credentials
	d.mu.RUnlock()
	if credentials == nil {
		return
	}
	if _, err := credentials.DeclareSessionShim(
		ctx, attestation, sessionShimCompositionRefreshReason+"-withdrawn",
	); err != nil {
		slog.Warn("session shim: could not re-declare stand-down after a failed composition install",
			"err", err.Error())
	}
}

// restoreSessionShimAdmissionAfterFailedInstall clears a proof-v2 readiness
// withdrawal that the install's own window provoked. While a composition is
// pending, this daemon already presents the composed attestation, so a poll
// tick's claim gate or a heartbeat projection resolve can consult the
// embedder's readiness resolver at the one moment it cannot answer yet —
// before the founding receipt exists — and withdraw admission: latch raised,
// spawner paused, lifecycle moved to recovering. A SUCCESSFUL install
// self-heals through the acknowledged projected heartbeat; a failed one used
// to roll back the identity and leave the withdrawal standing — a daemon
// claiming to serve while refusing every admission, forever. A withdrawal the
// install provoked must not outlive the install it belongs to.
//
// Mirrors the reopen in AcknowledgeSessionShimRecoveryHeartbeat: same lock,
// same stopGen guard so a daemon mid-shutdown is not resurrected, and only the
// recovering state is reopened — an operator pause or drain stays an operator
// pause or drain.
func (d *Daemon) restoreSessionShimAdmissionAfterFailedInstall() {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.stopGen != nil || !d.sessionShimReadinessWithdrawn.CompareAndSwap(true, false) {
		return
	}
	if d.State() == StateRecovering {
		if d.spawner != nil {
			d.spawner.Resume()
		}
		d.setState(StateRunning)
	}
}

// discardSessionShimCompositionState drops the per-scope authority a failed
// install had already retained. Nothing reads it while the daemon is standing
// down, but leaving a receipt for an attestation this host no longer presents
// is state that can only ever be wrong.
func (d *Daemon) discardSessionShimCompositionState() {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	for scope := range d.shims.credentialReceipts {
		delete(d.shims.credentialReceipts, scope)
	}
}

// publishSessionShimHeartbeatProjection is the announcement. It installs the
// projection on the running heartbeat and rings one beat immediately, so the
// window in which this host presents an attestation the control plane has no
// projection for is one round trip rather than one heartbeat interval.
//
// A beat the control plane does not acknowledge fails the install: the caller's
// rollback withdraws the projection along with everything else, which is the
// only honest response to authority refusing the state we just published.
func (d *Daemon) publishSessionShimHeartbeatProjection(ctx context.Context) error {
	heartbeat := d.heartbeat
	if heartbeat == nil {
		return errors.New("session shim: composition install requires a running heartbeat")
	}
	primaryScope := d.sessionShimConfig().orgID()
	heartbeat.SetSessionShimProjection(
		func() (SessionShimHeartbeatProjection, error) {
			return d.SessionShimHeartbeatProjection(primaryScope)
		},
		func(projection SessionShimHeartbeatProjection) {
			d.AcknowledgeSessionShimRecoveryHeartbeat(primaryScope, projection)
		},
	)
	if err := heartbeat.SendNow(ctx); err != nil {
		heartbeat.SetSessionShimProjection(nil, nil)
		return fmt.Errorf("session shim: first projected heartbeat: %w", err)
	}
	// Startup's own beat is what Daemon.Start rings for the scope it owns. A
	// composition that lands later rings it here instead, and announces the
	// activated set so an embedder holding a lane this package does not own can
	// ring that one too.
	d.notifySessionShimAdoptionActivated(ctx, d.sessionShimActivatedScopes())
	return nil
}
