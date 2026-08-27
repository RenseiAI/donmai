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
// runs the startup adoption pass, and only then re-declares the full
// attestation through the credential refresh and the heartbeat projection.
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
	// stand-down has to be said again.
	installed := false
	declared := false
	defer func() {
		if installed {
			return
		}
		d.shimIdentityRef.Store(prior)
		d.discardSessionShimCompositionState()
		if declared {
			d.redeclareSessionShimStandDown(ctx, prior.attestation)
		}
	}()
	d.shimIdentityRef.Store(pending)

	if err := d.declareSessionShimComposition(ctx); err != nil {
		return err
	}
	declared = true

	if err := d.adoptSessionShims(ctx); err != nil {
		return fmt.Errorf("session shim: composition adoption: %w", err)
	}

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
// control plane on the registration this daemon already holds, and retains the
// scoped receipts the adoption pass needs to resolve host authority.
//
// It goes through the credential refresher rather than Register: a full
// re-registration mints a NEW worker identity and retires the current one,
// which is exactly the mutual-eviction shape RefreshRuntimeToken exists to
// avoid. The refresh re-presents this worker id with the new attestation and
// answers with the receipt.
func (d *Daemon) declareSessionShimComposition(ctx context.Context) error {
	d.mu.RLock()
	credentials := d.credentials
	d.mu.RUnlock()
	if credentials == nil {
		return errors.New("session shim: composition install requires a registered daemon")
	}
	// The retention this refresh's receipt validation would otherwise demand is
	// the caller's very next step: this round trip IS what establishes the
	// scope's host authority, so there is nothing retained yet to compare it
	// against.
	d.shims.setDeclaringComposition(true)
	defer d.shims.setDeclaringComposition(false)

	result, err := credentials.DeclareSessionShim(
		ctx, d.SessionShimHostAttestation(), sessionShimCompositionRefreshReason,
	)
	if err != nil {
		return fmt.Errorf("session shim: declare composition: %w", err)
	}
	return d.acquireSessionShimRecoveryReceipts(ctx, result.SessionShim)
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
