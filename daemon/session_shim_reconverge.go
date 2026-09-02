package daemon

// session_shim_reconverge.go — re-converging with a control plane that already
// moved, instead of arguing with it forever.
//
// Provenance: shim-adoption-reconvergence-2026-09-01.
//
// THE TWO STRANDS THIS UNDOES
//
// (A) AN AMBIGUOUS COMMIT THAT WAS ACTUALLY COMMITTED. An adoption-batch
// commit whose answer is lost (a client deadline, a transport reset, a 5xx)
// already schedules the bounded reconciliation in session_shim_reconcile.go.
// But measured on an installed host, that reconciliation could not finish: the
// control plane HAD committed the batch, so every reconciliation attempt's
// re-preparation answered "the expected host adoption revision changed" — the
// compare-and-swap state this daemon presented was one revision behind the one
// the control plane now held. Four attempts spent that answer, the loop
// exhausted its derived bound, and the daemon then served and beat the
// superseded revision FOREVER: every heartbeat refused as revision-stale,
// every credential receipt at the new revision refused by the composing
// plane's own retain check, and the host unusable until an operator edited
// authority state by hand.
//
// The answer that refused the daemon is itself the evidence the daemon needed.
// When the preparation reports that the expected revision advanced past this
// daemon's last-committed revision BY EXACTLY ONE, and names the operational
// digest of the batch the control plane committed to get there, and that digest
// is byte-identical to the digest of the batch this daemon is presenting right
// now, then the advance IS the outcome of this daemon's own lost commit. There
// is nothing left to publish: adopt the advanced revision and its receipt, and
// say so in the log. A digest that DISAGREES is a different fact — some other
// writer moved the scope — and the honest response is to re-present this
// daemon's complete current projection at the advanced revision, which is
// exactly what the next reconciliation attempt does once the refresher has
// relearned the revision.
//
// Nothing here fabricates authority. The advanced revision and the receipt
// retained with it are the CONTROL PLANE'S OWN, echoed back on the preparation
// answer; the digest match is what proves they belong to this daemon's batch
// rather than to somebody else's.
//
// (B) A BOOT BATCH THE CONTROL PLANE HAS ALREADY RECORDED. A planned restart
// re-presents a still-live shim at a higher controller generation. Measured on
// an installed host, the control plane answered the boot batch with a closed
// idempotency conflict for that ONE lineage — its adoption evidence was
// already on file — and the daemon treated the refusal the way it treats every
// other boot-batch failure: abort the whole composition, close every
// controller, come up with durable sessions OFF for the entire host. The
// orphaned shims then self-terminated on their own deadline and took healthy
// harnesses with them. One lineage's bookkeeping collision must never cost the
// host its durable sessions: quarantine exactly the named lineages (visible,
// capacity-honest, still holding their harnesses) and re-commit the batch
// without them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/sessionshim"
)

// SessionShimAdoptionRevisionAdvanced is the typed answer a composing
// PrepareAdoptionBatch (or OnAdoptionBatch) returns when the control plane's
// expected compare-and-swap state has already moved PAST the revision this
// daemon last committed.
//
// It is a contract, not a diagnosis: returning a bare error here says only
// "refused", and a refusal is indistinguishable from a control plane that
// simply will not take this batch. Returning this type says what the control
// plane actually reported — which revision it now holds, and which batch it
// committed to get there — and that is the whole difference between a daemon
// that re-converges and a daemon that serves a superseded revision forever.
//
// Advanced and CommittedBatchDigest are the control plane's own values.
// Committed is the receipt it issued for that batch; the daemon retains it
// unchanged, so no revision this host ever attests originates locally.
type SessionShimAdoptionRevisionAdvanced struct {
	// LastCommitted is the compare-and-swap state this daemon presented, as the
	// control plane read it back. Optional: the daemon's own retained scope
	// authority is the authority when this is empty.
	LastCommitted string

	// Advanced is the revision the control plane now holds.
	Advanced string

	// CommittedBatchDigest is the operational digest — the idempotency key — of
	// the batch the control plane committed to reach Advanced. Empty means the
	// control plane did not name one, and the daemon then refuses to treat the
	// advance as its own commit.
	CommittedBatchDigest string

	// Committed is the receipt the control plane issued for that batch. Its
	// DurableCorrelation and AdoptionRevision are retained verbatim when the
	// digest proves the batch was this daemon's own.
	Committed SessionShimAdoptionBatchReceipt

	// Err is the underlying answer, kept for the log line and for callers that
	// classify on the transport failure beneath it.
	Err error
}

func (e *SessionShimAdoptionRevisionAdvanced) Error() string {
	return fmt.Sprintf(
		"session shim: the control plane's expected host adoption revision advanced to %q (this daemon holds %q): %v",
		e.Advanced, e.LastCommitted, e.Err)
}

func (e *SessionShimAdoptionRevisionAdvanced) Unwrap() error { return e.Err }

// SessionShimAdoptionEvidenceRecorded is the typed answer a composing
// OnAdoptionBatch returns when the control plane refused the batch because it
// ALREADY HOLDS durable adoption evidence for the named lineages — the closed
// idempotency conflict a planned restart provokes by re-presenting a still-live
// shim at a higher controller generation.
//
// Lineages must name every lineage the conflict is about. A conflict that names
// none is indistinguishable from a whole-batch refusal and is handled as one:
// the daemon cannot quarantine "whichever lineage it was".
type SessionShimAdoptionEvidenceRecorded struct {
	Lineages []sessionshim.Identity
	Err      error
}

func (e *SessionShimAdoptionEvidenceRecorded) Error() string {
	ids := make([]string, 0, len(e.Lineages))
	for _, id := range e.Lineages {
		ids = append(ids, id.String())
	}
	return fmt.Sprintf(
		"session shim: the control plane already holds durable adoption evidence for %s: %v",
		strings.Join(ids, ", "), e.Err)
}

func (e *SessionShimAdoptionEvidenceRecorded) Unwrap() error { return e.Err }

// sessionShimAdoptionBatchDigestEncoding names the exact document this digest
// is taken over. It is a member of that document, so a future encoding cannot
// collide with this one even over an identical batch.
const sessionShimAdoptionBatchDigestEncoding = "session-shim-adoption-batch-v1"

// sessionShimAdoptionBatchDigest is the batch's digest: a stable fingerprint of
// WHAT this batch says, sent as the commit's idempotency key.
//
// The encoding is the corpus's, not this file's: RFC 8785 JSON canonicalization
// over a document that OMITS ITS OWN DIGEST MEMBER, hashed with SHA-256 and
// rendered as exactly 64 lowercase hexadecimal characters, with every epoch,
// cursor, and generation carried as a canonical uint64 decimal STRING rather
// than a JSON number (a JSON number cannot carry a uint64 without loss, and the
// corpus refuses a non-canonical spelling rather than accepting an equivalent
// representation).
//
// Only lifecycle-identifying facts enter it. The expected revision is excluded
// (it is the compare-and-swap state, not the content) and so is every value
// that moves on its own — a quarantine's age, an evidence's observation
// timestamp — because the digest's whole job is to be identical across two
// presentations of the same set, seconds apart, on either side of a lost
// answer.
//
// The batch must already be sorted; completeSessionShimAdoptionBatch is the one
// choke point that does both.
func sessionShimAdoptionBatchDigest(batch SessionShimAdoptionBatch) (string, error) {
	adopted := make([]any, 0, len(batch.Adopted))
	for _, outcome := range batch.Adopted {
		evidence := outcome.Evidence
		adopted = append(adopted, map[string]any{
			"orgId":                evidence.Identity.OrgID,
			"sessionId":            evidence.Identity.SessionID,
			"shimId":               evidence.ShimID,
			"processEpoch":         strconv.FormatUint(evidence.ProcessEpoch, 10),
			"controllerGeneration": strconv.FormatUint(evidence.ControllerGeneration, 10),
			"carrierCompatible":    evidence.CarrierCompatible,
		})
	}
	quarantined := make([]any, 0, len(batch.Quarantined))
	for _, entry := range batch.Quarantined {
		quarantined = append(quarantined, map[string]any{
			"orgId":                entry.OrgID,
			"sessionId":            entry.SessionID,
			"shimId":               entry.ShimID,
			"processEpoch":         strconv.FormatUint(entry.ProcessEpoch, 10),
			"controllerGeneration": strconv.FormatUint(entry.ControllerGeneration, 10),
			"reason":               string(entry.Reason),
			"consumesCapacity":     entry.ConsumesCapacity,
		})
	}
	tombstoned := make([]any, 0, len(batch.Tombstoned))
	for _, entry := range batch.Tombstoned {
		tombstoned = append(tombstoned, map[string]any{
			"orgId":        entry.Identity.OrgID,
			"sessionId":    entry.Identity.SessionID,
			"shimId":       entry.ShimID,
			"processEpoch": strconv.FormatUint(entry.ProcessEpoch, 10),
		})
	}
	cleared := make([]any, 0, len(batch.Cleared))
	for _, entry := range batch.Cleared {
		cleared = append(cleared, map[string]any{
			"orgId":                entry.OrgID,
			"sessionId":            entry.SessionID,
			"shimId":               entry.ShimID,
			"processEpoch":         strconv.FormatUint(entry.ProcessEpoch, 10),
			"controllerGeneration": strconv.FormatUint(entry.ControllerGeneration, 10),
			"disposition":          string(entry.Disposition),
			"reason":               string(entry.Reason),
		})
	}
	canonical, err := executioncell.CanonicalJSON(map[string]any{
		"encoding":    sessionShimAdoptionBatchDigestEncoding,
		"orgId":       batch.OrgID,
		"hostId":      batch.HostID,
		"adopted":     adopted,
		"quarantined": quarantined,
		"tombstoned":  tombstoned,
		"cleared":     cleared,
	})
	if err != nil {
		return "", fmt.Errorf("session shim: canonicalize adoption batch for its digest: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// sessionShimRevisionAdvancedByOne reports whether advanced is exactly one step
// past last.
//
// Both must be canonical uint64 decimal revisions — "0", or a non-zero digit
// followed by digits — which is the only spelling the control boundary defines.
// A revision in any other shape is never treated as a successor: adopting an
// advance this daemon cannot prove is exactly one step is how a host would
// silently skip a revision somebody else committed.
func sessionShimRevisionAdvancedByOne(last, advanced string) bool {
	lastCounter, lastOK := canonicalSessionShimRevision(last)
	nextCounter, nextOK := canonicalSessionShimRevision(advanced)
	if !lastOK || !nextOK || lastCounter == math.MaxUint64 {
		return false
	}
	return nextCounter == lastCounter+1
}

// canonicalSessionShimRevision decodes one canonical uint64 decimal revision. A
// leading zero, a sign, a non-digit, an empty value, or an overflow is a
// refusal rather than an equivalent representation.
func canonicalSessionShimRevision(revision string) (uint64, bool) {
	if revision == "" || (len(revision) > 1 && revision[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(revision); i++ {
		if revision[i] < '0' || revision[i] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// adoptAdvancedSessionShimAdoptionRevision decides whether a preparation or
// commit failure is really this daemon's OWN commit, reported back to it.
//
// Every condition below has to hold, and each one is load-bearing:
//
//   - the answer is the typed advance, not a bare refusal — a refusal carries
//     no revision to adopt;
//   - the advance is exactly one step past the revision this daemon last
//     committed — two steps means something else committed in between and this
//     daemon's batch is not the only thing it would be adopting;
//   - the control plane named the digest of the batch it committed, and that
//     digest equals the digest of the batch being presented right now — this is
//     what makes it OUR commit rather than somebody else's;
//   - the receipt it issued is complete, and its revision is the advanced one.
//
// Anything short of all four returns false and the caller keeps the failure,
// which re-presents the daemon's current projection at the advanced revision
// through the ordinary reconciliation path.
// It also returns the advance itself, so a caller that then REFUSES the adopted
// receipt can re-wrap its own refusal in the advance rather than degrading it
// to an untyped error — a republish that lost the type stops re-arming
// reconciliation, which is the exact failure this whole file exists to prevent.
func (d *Daemon) adoptAdvancedSessionShimAdoptionRevision(
	batch SessionShimAdoptionBatch,
	err error,
) (SessionShimAdoptionBatchReceipt, *SessionShimAdoptionRevisionAdvanced, bool) {
	var advanced *SessionShimAdoptionRevisionAdvanced
	if !errors.As(err, &advanced) || advanced.Advanced == "" {
		return SessionShimAdoptionBatchReceipt{}, advanced, false
	}
	lastCommitted := advanced.LastCommitted
	if receipt, ok := d.SessionShimScopeAuthority(batch.OrgID); ok && receipt.AdoptionRevision != "" {
		// The daemon's own retained authority wins over the echo: the echo is
		// the control plane's reading of what we presented, and if the two
		// disagree we do not know which batch the advance belongs to.
		if lastCommitted != "" && lastCommitted != receipt.AdoptionRevision {
			return SessionShimAdoptionBatchReceipt{}, advanced, false
		}
		lastCommitted = receipt.AdoptionRevision
	}
	if lastCommitted == "" || !sessionShimRevisionAdvancedByOne(lastCommitted, advanced.Advanced) {
		return SessionShimAdoptionBatchReceipt{}, advanced, false
	}
	if advanced.CommittedBatchDigest == "" || advanced.CommittedBatchDigest != batch.BatchDigest {
		return SessionShimAdoptionBatchReceipt{}, advanced, false
	}
	committed := advanced.Committed
	if len(committed.DurableCorrelation) == 0 || committed.AdoptionRevision != advanced.Advanced {
		return SessionShimAdoptionBatchReceipt{}, advanced, false
	}
	return committed, advanced, true
}

// rewrapSessionShimAdvance returns a copy of one advance carrying cause as its
// underlying answer, so the typed classification survives a refusal raised
// AFTER the advance was recognised.
func rewrapSessionShimAdvance(advanced *SessionShimAdoptionRevisionAdvanced, cause error) error {
	rewrapped := *advanced
	rewrapped.Err = cause
	return &rewrapped
}

// sessionShimBatchAfterEvidenceRecorded moves every lineage the control plane
// says it already holds evidence for out of the batch's adopted section and
// into its quarantined section, and reports the quarantine entries it made.
//
// The batch stays a COMPLETE snapshot — the lineage is presented, never
// omitted, because omitting a live lineage is itself refusable — and the shim
// keeps its harness: a bookkeeping collision is not a terminal fact about a
// running session.
//
// It reports nothing when the conflict names no lineage this batch adopted,
// which is the caller's signal that there is nothing narrower to do than fail.
func sessionShimBatchAfterEvidenceRecorded(
	batch SessionShimAdoptionBatch,
	conflicted []sessionshim.Identity,
) (SessionShimAdoptionBatch, []sessionshim.QuarantinedSession) {
	named := make(map[sessionshim.Identity]struct{}, len(conflicted))
	for _, id := range conflicted {
		named[id] = struct{}{}
	}
	kept := make([]SessionShimAdoptionOutcome, 0, len(batch.Adopted))
	quarantines := make([]sessionshim.QuarantinedSession, 0, len(conflicted))
	for _, outcome := range batch.Adopted {
		if _, ok := named[outcome.Evidence.Identity]; !ok {
			kept = append(kept, outcome)
			continue
		}
		quarantines = append(quarantines, sessionshim.QuarantinedSession{
			OrgID:                outcome.Evidence.Identity.OrgID,
			SessionID:            outcome.Evidence.Identity.SessionID,
			ShimID:               outcome.Evidence.ShimID,
			ProcessEpoch:         outcome.Evidence.ProcessEpoch,
			ControllerGeneration: outcome.Evidence.ControllerGeneration,
			Reason:               sessionshim.QuarantineAdoptionFailed,
			Detail: "the control plane already holds durable adoption evidence for this lineage; " +
				"presented quarantined so the rest of the host keeps its durable sessions",
			ConsumesCapacity: true,
		})
	}
	if len(quarantines) == 0 {
		return batch, nil
	}
	batch.Adopted = kept
	batch.Quarantined = append(append([]sessionshim.QuarantinedSession(nil), batch.Quarantined...), quarantines...)
	return batch, quarantines
}

// commitBootBatchAroundRecordedEvidence is the startup pass's narrow recovery
// from a batch the control plane refused because it already holds adoption
// evidence for a named lineage.
//
// It quarantines exactly those lineages — dropping them from the adopted set
// the caller is about to publish locally, recording them in the caller's
// failure map so the live projection, the gate resolution, and every LATER
// batch this daemon sends all agree — and commits the batch again without
// them. It repeats while each answer names another adopted lineage, bounded by
// the number of adopted entries, because every pass must remove at least one
// or it stops: a control plane that reports its conflicts one lineage at a time
// would otherwise still cost the host its durable sessions on the second name.
//
// WHAT THIS COSTS THE LINEAGE, EXACTLY. Recording it in adoptionFailures puts
// it on the caller's release path: this daemon RELEASES its control socket to
// that shim (Controller.Close), the same disposition every other failed
// per-lineage adoption gets. The harness is NOT killed and the shim is not
// stopped — the shim keeps it and starts its own bounded §D8 orphan clock, so
// the window before an unattended harness is reaped is that deadline, not
// zero. That is the honest cost of this recovery and it is deliberate: the
// alternative — keeping local controller authority over a lineage the control
// plane refused in the batch — would present the same lineage as both adopted
// and quarantined in the very next projection, which the receiver refuses as a
// duplicate, and would double-count it against capacity.
//
// A conflict that names no adopted lineage, or a re-commit that fails for any
// other reason, returns a failure and the caller aborts the composition exactly
// as before. This narrows a host-wide failure to a per-lineage one; it does not
// invent a way to succeed.
func (d *Daemon) commitBootBatchAroundRecordedEvidence(
	ctx context.Context,
	batch SessionShimAdoptionBatch,
	cause error,
	entries map[sessionshim.Identity]adoptedShim,
	adoptionFailures map[sessionshim.Identity]sessionshim.QuarantinedSession,
) (SessionShimAdoptionBatchReceipt, error) {
	// Every pass removes at least one adopted lineage, so the adopted count is
	// the loop's own bound — derived from the batch rather than chosen.
	bound := len(batch.Adopted)
	accumulated := make([]sessionshim.QuarantinedSession, 0, bound)
	for pass := 0; pass < bound; pass++ {
		var recorded *SessionShimAdoptionEvidenceRecorded
		if !errors.As(cause, &recorded) || len(recorded.Lineages) == 0 {
			break
		}
		amended, quarantines := sessionShimBatchAfterEvidenceRecorded(batch, recorded.Lineages)
		if len(quarantines) == 0 {
			// The conflict names nothing this batch adopts; there is nothing
			// narrower than the whole composition left to do.
			break
		}
		batch = amended
		accumulated = append(accumulated, quarantines...)
		receipt, err := d.completeSessionShimAdoptionBatch(ctx, batch)
		if err == nil {
			for _, quarantine := range accumulated {
				id := sessionshim.Identity{OrgID: quarantine.OrgID, SessionID: quarantine.SessionID}
				delete(entries, id)
				adoptionFailures[id] = quarantine
				slog.Warn("session shim: the control plane already holds this lineage's adoption evidence; releasing this daemon's "+
					"control socket to it, quarantining it, and composing the rest of the host — the shim keeps its harness and "+
					"starts its own bounded orphan clock (shim-adoption-reconvergence-2026-09-01)",
					"session", id.String(), "revision", receipt.AdoptionRevision,
					"orphanDeadline", d.sessionShimConfig().Orphan.Deadline)
			}
			return receipt, nil
		}
		cause = err
	}
	if len(accumulated) == 0 {
		return SessionShimAdoptionBatchReceipt{}, cause
	}
	return SessionShimAdoptionBatchReceipt{}, fmt.Errorf(
		"session shim: re-committing the boot batch without %d already-recorded lineage(s): %w",
		len(accumulated), cause)
}

// sessionShimOrphanDeadlineUnderExternalRelease returns the largest orphan
// deadline that keeps the §D8 inequality strictly satisfied against a declared
// external release threshold, never exceeding preferred.
//
// §D8 fixes deadline + grace + margin < threshold and rejects a violating
// configuration AT STARTUP, which prevents session admission. So a default
// chosen for a standalone host — where nothing external can release a claim and
// the inequality has no upper bound — cannot simply be handed to a composing
// deployment that declared one: it would refuse to admit anything, and a host
// that comes up refusing every session is a worse outcome than a shorter grace.
//
// The headroom reserved below the exclusive ceiling is one propagation margin,
// which is derived rather than chosen: the margin is exactly the unit this
// policy already uses to express how much clock and propagation slop the host
// must assume it cannot see.
//
// When no positive deadline can satisfy the bound at all, the preferred value
// is returned unchanged and Validate refuses the daemon at startup. Inventing a
// deadline there would hide a configuration that can produce double execution,
// which is the one thing this bound exists to prevent.
func sessionShimOrphanDeadlineUnderExternalRelease(
	policy sessionshim.OrphanPolicy,
	preferred time.Duration,
) time.Duration {
	if policy.ExternalReleaseThreshold <= 0 {
		return preferred
	}
	ceiling := policy.ExternalReleaseThreshold - policy.TerminationGrace - policy.PropagationMargin
	if ceiling <= 0 {
		return preferred
	}
	headroom := policy.PropagationMargin
	if headroom <= 0 || headroom >= ceiling {
		headroom = ceiling / 2
	}
	derived := ceiling - headroom
	if derived <= 0 {
		return preferred
	}
	return min(derived, preferred)
}
