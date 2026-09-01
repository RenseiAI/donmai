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
	"strconv"
	"strings"

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

// sessionShimAdoptionBatchDigest is the batch's operational digest: a stable
// fingerprint of WHAT this batch says, sent as the commit's idempotency key.
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
func sessionShimAdoptionBatchDigest(batch SessionShimAdoptionBatch) string {
	h := sha256.New()
	// hash.Hash.Write never returns an error, which is what makes the discard
	// here a fact about the interface rather than an unchecked failure.
	write := func(format string, args ...any) { _, _ = fmt.Fprintf(h, format, args...) }
	write("session-shim-adoption-batch-v1\norg=%s\nhost=%s\n", batch.OrgID, batch.HostID)
	write("adopted=%d\n", len(batch.Adopted))
	for _, outcome := range batch.Adopted {
		e := outcome.Evidence
		write("a\x1f%s\x1f%s\x1f%s\x1f%d\x1f%d\x1f%t\n",
			e.Identity.OrgID, e.Identity.SessionID, e.ShimID, e.ProcessEpoch,
			e.ControllerGeneration, e.CarrierCompatible)
	}
	write("quarantined=%d\n", len(batch.Quarantined))
	for _, q := range batch.Quarantined {
		write("q\x1f%s\x1f%s\x1f%s\x1f%d\x1f%d\x1f%s\x1f%t\n",
			q.OrgID, q.SessionID, q.ShimID, q.ProcessEpoch, q.ControllerGeneration,
			q.Reason, q.ConsumesCapacity)
	}
	write("tombstoned=%d\n", len(batch.Tombstoned))
	for _, tombstone := range batch.Tombstoned {
		write("t\x1f%s\x1f%s\x1f%s\x1f%d\n",
			tombstone.Identity.OrgID, tombstone.Identity.SessionID,
			tombstone.ShimID, tombstone.ProcessEpoch)
	}
	write("cleared=%d\n", len(batch.Cleared))
	for _, cleared := range batch.Cleared {
		write("c\x1f%s\x1f%s\x1f%s\x1f%d\x1f%d\x1f%s\x1f%s\n",
			cleared.OrgID, cleared.SessionID, cleared.ShimID, cleared.ProcessEpoch,
			cleared.ControllerGeneration, cleared.Disposition, cleared.Reason)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sessionShimRevisionAdvancedByOne reports whether advanced is exactly one step
// past last.
//
// Adoption revisions are opaque strings on this wire, but every composition
// that issues them counts: the value is a decimal counter, optionally behind a
// stable prefix. So the comparison is "same prefix, successor counter", and a
// revision whose shape this cannot read is never treated as a successor —
// adopting an advance this daemon cannot prove is exactly one step is how a
// host would silently skip a revision somebody else committed.
func sessionShimRevisionAdvancedByOne(last, advanced string) bool {
	lastPrefix, lastCounter, lastOK := splitSessionShimRevisionCounter(last)
	nextPrefix, nextCounter, nextOK := splitSessionShimRevisionCounter(advanced)
	if !lastOK || !nextOK || lastPrefix != nextPrefix {
		return false
	}
	return nextCounter == lastCounter+1
}

// splitSessionShimRevisionCounter splits a revision into its stable prefix and
// its trailing decimal counter. A value with no trailing digits, with a counter
// too long to be a counter, or with a leading zero (two spellings of one
// number would make "exactly one step" ambiguous) is not readable as one.
func splitSessionShimRevisionCounter(revision string) (prefix string, counter uint64, ok bool) {
	i := len(revision)
	for i > 0 && revision[i-1] >= '0' && revision[i-1] <= '9' {
		i--
	}
	digits := revision[i:]
	if digits == "" || len(digits) > 19 || (len(digits) > 1 && digits[0] == '0') {
		return "", 0, false
	}
	value, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return revision[:i], value, true
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
func (d *Daemon) adoptAdvancedSessionShimAdoptionRevision(
	batch SessionShimAdoptionBatch,
	err error,
) (SessionShimAdoptionBatchReceipt, bool) {
	var advanced *SessionShimAdoptionRevisionAdvanced
	if !errors.As(err, &advanced) || advanced.Advanced == "" {
		return SessionShimAdoptionBatchReceipt{}, false
	}
	lastCommitted := advanced.LastCommitted
	if receipt, ok := d.SessionShimScopeAuthority(batch.OrgID); ok && receipt.AdoptionRevision != "" {
		// The daemon's own retained authority wins over the echo: the echo is
		// the control plane's reading of what we presented, and if the two
		// disagree we do not know which batch the advance belongs to.
		if lastCommitted != "" && lastCommitted != receipt.AdoptionRevision {
			return SessionShimAdoptionBatchReceipt{}, false
		}
		lastCommitted = receipt.AdoptionRevision
	}
	if lastCommitted == "" || !sessionShimRevisionAdvancedByOne(lastCommitted, advanced.Advanced) {
		return SessionShimAdoptionBatchReceipt{}, false
	}
	if advanced.CommittedBatchDigest == "" || advanced.CommittedBatchDigest != batch.OperationalDigest {
		return SessionShimAdoptionBatchReceipt{}, false
	}
	committed := advanced.Committed
	if len(committed.DurableCorrelation) == 0 || committed.AdoptionRevision != advanced.Advanced {
		return SessionShimAdoptionBatchReceipt{}, false
	}
	return committed, true
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
// batch this daemon sends all agree — and commits the batch once more without
// them. The shims themselves are untouched: a lineage whose evidence the
// control plane already holds is not a lineage that died.
//
// A conflict that names no adopted lineage, or a re-commit that fails on its
// own, returns a failure and the caller aborts the composition exactly as
// before. This narrows a host-wide failure to a per-lineage one; it does not
// invent a way to succeed.
func (d *Daemon) commitBootBatchAroundRecordedEvidence(
	ctx context.Context,
	batch SessionShimAdoptionBatch,
	cause error,
	entries map[sessionshim.Identity]adoptedShim,
	adoptionFailures map[sessionshim.Identity]sessionshim.QuarantinedSession,
) (SessionShimAdoptionBatchReceipt, error) {
	var recorded *SessionShimAdoptionEvidenceRecorded
	if !errors.As(cause, &recorded) || len(recorded.Lineages) == 0 {
		return SessionShimAdoptionBatchReceipt{}, cause
	}
	amended, quarantines := sessionShimBatchAfterEvidenceRecorded(batch, recorded.Lineages)
	if len(quarantines) == 0 {
		return SessionShimAdoptionBatchReceipt{}, cause
	}
	receipt, err := d.completeSessionShimAdoptionBatch(ctx, amended)
	if err != nil {
		return SessionShimAdoptionBatchReceipt{}, fmt.Errorf(
			"session shim: re-committing the boot batch without %d already-recorded lineage(s): %w (original refusal: %v)",
			len(quarantines), err, cause)
	}
	for _, quarantine := range quarantines {
		id := sessionshim.Identity{OrgID: quarantine.OrgID, SessionID: quarantine.SessionID}
		delete(entries, id)
		adoptionFailures[id] = quarantine
		slog.Warn("session shim: the control plane already holds this lineage's adoption evidence; quarantining it and composing "+
			"the rest of the host (shim-adoption-reconvergence-2026-09-01)",
			"session", id.String(), "revision", receipt.AdoptionRevision, "error", cause)
	}
	return receipt, nil
}
