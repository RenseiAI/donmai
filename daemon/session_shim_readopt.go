package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// readoptSessionShimAfterControllerLoss re-adopts ONE live shim whose
// controller stream this daemon closed because its durable carrier refused.
//
// The loss the composing carrier suffered — a relay redeploy, a transport
// reset — leaves the shim, its harness, and its discovery record exactly where
// they were. Quarantining that lineage at once hands it to the shim's orphan
// deadline, which then reaps a healthy harness for a fault nobody at either
// end had (§D8 reserves that outcome for a daemon that never returned). This
// runs the pipeline the startup pass runs — dial, prepare, durable adoption,
// complete batch, carrier activation — for exactly this identity, bounded by
// the configured policy, and reports true once the lineage is adopted again
// under a strictly newer generation. The shim disarms its own orphan clock on
// that adoption. A lineage that cannot be re-adopted inside the bound is left
// for the caller's quarantine path, exactly as before.
//
// The lost entry stays in d.shims.adopted throughout: the receiver holds the
// lineage adopted at that generation, and every projection built meanwhile
// must keep saying so. The swap to the new controller happens under the lock,
// only when the lost controller is still the adopted one, and is undone when
// the batch that would have told the receiver about it does not commit.
func (d *Daemon) readoptSessionShimAfterControllerLoss(id sessionshim.Identity, lost adoptedShim) bool {
	cfg := d.sessionShimConfig()
	policy := cfg.readoption()
	if policy.Disabled || lost.controller == nil {
		return false
	}
	if lost.readoptedAtUnixNano != 0 {
		since := d.shimNow().Sub(time.Unix(0, lost.readoptedAtUnixNano))
		if since < policy.window() {
			// Re-adopted inside the window and lost again: this carrier is not
			// one a bounded retry can restore, and every further cycle costs an
			// adoption revision the receiver has to re-attest.
			slog.Warn("session shim: controller lost again inside the re-adoption window; quarantining rather than re-adopting",
				"session", id.String(), "sinceReadoption", since, "window", policy.window())
			return false
		}
	}
	registry, err := d.sessionShimRegistry()
	if err != nil {
		slog.Warn("session shim: re-adoption after controller loss has no registry", "session", id.String(), "error", err)
		return false
	}
	hello := lost.controller.Hello()
	backoff := policy.Backoff
	for attempt := 1; attempt <= policy.Attempts; attempt++ {
		if attempt > 1 {
			if !d.sleepSessionShimReconcileBackoff(backoff) {
				return false
			}
			backoff *= 2
		}
		if d.sessionShimReconcileStopped() {
			return false
		}
		if !sessionShimIncarnationStillLive(registry, id, hello.ShimID, hello.ProcessEpoch) {
			// The record is gone or the shim already left its proof on disk:
			// there is nothing to re-adopt, and the caller's path consumes the
			// tombstone before it publishes anything.
			return false
		}
		err := d.readoptSessionShimOnce(registry, cfg, id, lost, hello)
		if err == nil {
			slog.Info("session shim: re-adopted a live shim after controller loss",
				"session", id.String(), "attempt", attempt)
			return true
		}
		var recorded *SessionShimAdoptionEvidenceRecorded
		if errors.As(err, &recorded) {
			// The control plane already holds adoption evidence this batch
			// conflicts with; presenting the same lineage again cannot change
			// that answer, so every further attempt would only spend prepares.
			slog.Warn("session shim: re-adoption refused as already-recorded evidence; quarantining",
				"session", id.String(), "attempt", attempt, "error", err)
			return false
		}
		slog.Warn("session shim: re-adoption attempt after controller loss failed",
			"session", id.String(), "attempt", attempt, "attempts", policy.Attempts, "error", err)
	}
	return false
}

// sessionShimIncarnationStillLive answers whether the exact incarnation still
// has a live discovery record and no terminal tombstone.
func sessionShimIncarnationStillLive(registry *sessionshim.Registry, id sessionshim.Identity, shimID string, processEpoch uint64) bool {
	if _, err := registry.GetTombstoneIncarnation(id, shimID, processEpoch); err == nil {
		return false
	}
	live, err := registry.HasIncarnation(id, shimID, processEpoch)
	return err == nil && live
}

// readoptSessionShimOnce is one bounded attempt: the startup adoption pass
// filtered to this identity, then the dynamic publication the launch path runs
// after its own dial.
func (d *Daemon) readoptSessionShimOnce(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	id sessionshim.Identity,
	lost adoptedShim,
	lostHello shimwire.Hello,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.adoptionPublicationTimeout())
	defer cancel()
	opts, preparations, err := d.sessionShimAdoptOptions(registry, cfg)
	if err != nil {
		return err
	}
	opts.Filter = func(candidate sessionshim.Identity) bool { return candidate == id }
	result, err := sessionshim.Adopt(ctx, opts)
	if err != nil {
		return fmt.Errorf("session shim: re-adopt: %w", err)
	}
	if len(result.Adopted) != 1 {
		result.Close()
		if len(result.Quarantined) > 0 {
			q := result.Quarantined[0]
			return fmt.Errorf("session shim: re-adoption classified the shim %s: %s", q.Reason, q.Detail)
		}
		return errors.New("session shim: re-adoption found no adoptable record")
	}
	ctrl := result.Adopted[0]
	if got := ctrl.Hello(); got.ShimID != lostHello.ShimID || got.ProcessEpoch != lostHello.ProcessEpoch {
		_ = ctrl.Close()
		return fmt.Errorf("session shim: a different incarnation answered the re-adoption (shim %q epoch %d, lost %q epoch %d)",
			got.ShimID, got.ProcessEpoch, lostHello.ShimID, lostHello.ProcessEpoch)
	}
	preparation := preparations.prepared[id]
	evidence, err := d.sessionShimAdoptionEvidence(ctx, ctrl, preparation, preparations.hosts[id])
	if err != nil {
		_ = ctrl.Close()
		return fmt.Errorf("session shim: resolve re-adoption host: %w", err)
	}
	gate := newShimAdoptionGate()
	d.consumeShimEventsGated(ctrl, gate)
	// Everything below is the launch path's publication, on the same barrier,
	// checkpoint, and heartbeat-barrier discipline — see launchSessionShim for
	// why each step is where it is.
	serializedPublication := cfg.OnAdoptionPublished != nil
	publicationSucceeded := !serializedPublication
	publicationCommitted := false
	ringPostActivationHeartbeat := false
	ringCorrectingHeartbeat := false
	defer func() {
		if ringPostActivationHeartbeat {
			activated := d.sessionShimActivatedScopes()
			d.ringSessionShimPostActivationHeartbeat(ctx)
			d.notifySessionShimAdoptionActivated(ctx, activated)
			return
		}
		if ringCorrectingHeartbeat {
			d.ringSessionShimHeartbeatDetached()
		}
	}()
	if serializedPublication {
		d.shims.publicationMu.Lock()
		if d.shims.dynamicPublicationFailed {
			d.shims.publicationMu.Unlock()
			evidence.SnapshotProxy.deactivate()
			gate.finish(false)
			_ = ctrl.Close()
			return errors.New("session shim: a prior dynamic adoption publication failed")
		}
		checkpoint := d.checkpointSessionShimPublication(id.OrgID)
		defer func() {
			if !publicationSucceeded {
				if publicationCommitted {
					d.shims.dynamicPublicationFailed = true
					d.restoreSessionShimHeartbeatLane(checkpoint)
				} else {
					d.rollbackSessionShimPublication(checkpoint)
				}
				ringCorrectingHeartbeat = true
			}
			d.shims.publicationMu.Unlock()
		}()
	}
	heartbeatBarrier := cfg.OnAdoptionPublished != nil && cfg.OnCarrierActivationAcknowledged != nil &&
		d.sessionShimEnabled()
	if cfg.OnAdoptionPublished != nil {
		if heartbeatBarrier {
			d.beginSessionShimRecoveryHeartbeatBarrier()
		} else {
			d.setState(StateRecovering)
		}
		d.shims.mu.Lock()
		d.shims.carrierActivationComplete = false
		d.shims.mu.Unlock()
	}
	receipt, err := d.completeSessionShimAdoption(ctx, evidence, preparation)
	evidence.SnapshotProxy.deactivate()
	if err != nil {
		d.cancelStagedSessionShimSnapshot(id)
		gate.finish(false)
		_ = ctrl.Close()
		return fmt.Errorf("session shim: durable re-adoption: %w", err)
	}
	evidence.SnapshotProxy = nil
	readopted := adoptedShim{
		controller:          ctrl,
		shimID:              ctrl.Hello().ShimID,
		handle:              lost.handle,
		spec:                lost.spec,
		launched:            lost.launched,
		adoption:            evidence,
		adoptionReceipt:     cloneSessionShimAdoptionReceipt(receipt),
		consumedRecovery:    newSessionShimConsumedRecovery(preparation, receipt),
		readoptedAtUnixNano: d.shimNow().UnixNano(),
	}
	if !d.installReadoptedSessionShim(id, lost.controller, readopted) {
		d.cancelStagedSessionShimSnapshot(id)
		gate.finish(false)
		_ = ctrl.Close()
		return errors.New("session shim: the lost controller is no longer the adopted entry")
	}
	batchReceipt, err := d.completeSessionShimAdoptionBatch(ctx, d.sessionShimProjectionBatch(id.OrgID, evidence.HostID))
	if err != nil {
		// Nothing durable advanced that this daemon can prove. Put the lost
		// entry back so the projection presents exactly what the receiver
		// holds — the caller's quarantine path builds its row from it — and
		// let an OUTCOME-UNKNOWN answer reach the reconciliation pass that
		// learns the committed revision through the refresher.
		d.restoreLostSessionShim(id, ctrl, lost)
		d.failPendingSessionShimActivations()
		gate.finish(false)
		_ = ctrl.Close()
		if errors.Is(err, errSessionShimAmbiguousBatchCommit) {
			d.scheduleSessionShimReconciliation(id.OrgID, sessionShimReconcileCauseAmbiguous)
		}
		return fmt.Errorf("session shim: re-adoption batch: %w", err)
	}
	publicationCommitted = true
	if err := d.updateSessionShimAdoptionRevision(id.OrgID, batchReceipt.AdoptionRevision, heartbeatBarrier); err != nil {
		d.failPendingSessionShimActivations()
		gate.finish(false)
		_ = ctrl.Close()
		return fmt.Errorf("session shim: retain re-adoption revision: %w", err)
	}
	d.shims.mu.Lock()
	if len(batchReceipt.DurableCorrelation) > 0 {
		d.shims.batchReceipts[id.OrgID] = batchReceipt
	}
	d.shims.mu.Unlock()
	gate.finish(true)
	if cfg.OnAdoptionPublished != nil {
		published := map[sessionshim.Identity]adoptedShim{id: readopted}
		if activationErr := d.activatePublishedSessionShimCarriers(ctx, published); activationErr != nil {
			// The batch is committed and the shim is adopted; only the carrier
			// did not activate. Same disposition as the launch path: keep the
			// claim and the visible capacity, withhold further claims.
			slog.Error("session shim: post-publication carrier activation failed after re-adoption",
				"session", id.String(), "error", activationErr)
			d.failPendingSessionShimActivations()
			_ = ctrl.Close()
			return nil
		}
		if !heartbeatBarrier {
			d.setState(StateRunning)
		}
	}
	publicationSucceeded = true
	ringPostActivationHeartbeat = heartbeatBarrier
	if !heartbeatBarrier {
		// No barrier was raised, but the batch still advanced the receiver's
		// revision and demoted its readiness until a beat re-attests it.
		d.ringSessionShimHeartbeatDetached()
	}
	return nil
}

// installReadoptedSessionShim swaps the re-adopted controller in for the lost
// one, under the lock, only while the lost one is still the adopted entry and
// the daemon has not released its shims. The correlation for the incarnation
// is replaced — same shim id and process epoch, new generation and receipt —
// and the durable forwarded cursor is seeded from the new controller's resume
// point exactly as the startup pass seeds it.
func (d *Daemon) installReadoptedSessionShim(id sessionshim.Identity, lost *sessionshim.Controller, entry adoptedShim) bool {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	if d.shims.reconcileStopped {
		return false
	}
	current, ok := d.shims.adopted[id]
	if !ok || current.controller != lost {
		return false
	}
	d.shims.adopted[id] = entry
	d.shims.correlations[shimIncarnationFor(entry.adoption)] = sessionShimAdoptionCorrelation{
		evidence: entry.adoption,
		receipt:  cloneSessionShimAdoptionReceipt(entry.adoptionReceipt),
	}
	if resumeFrom := entry.controller.ResumeFrom(); resumeFrom > 0 {
		if durableBeforeAdoption := resumeFrom - 1; durableBeforeAdoption > d.shims.forwarded[id] {
			d.shims.forwarded[id] = durableBeforeAdoption
		}
	}
	return true
}

// restoreLostSessionShim puts the lost entry back after a re-adoption whose
// batch did not commit, so the projection presents what the receiver holds.
func (d *Daemon) restoreLostSessionShim(id sessionshim.Identity, readopted *sessionshim.Controller, lost adoptedShim) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	current, ok := d.shims.adopted[id]
	if !ok || current.controller != readopted {
		return
	}
	d.shims.adopted[id] = lost
	d.shims.correlations[shimIncarnationFor(lost.adoption)] = sessionShimAdoptionCorrelation{
		evidence: lost.adoption,
		receipt:  cloneSessionShimAdoptionReceipt(lost.adoptionReceipt),
	}
}
