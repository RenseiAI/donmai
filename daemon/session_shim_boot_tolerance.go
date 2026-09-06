package daemon

// session_shim_boot_tolerance.go — a boot that survives a dead lineage and a
// refused commit.
//
// Provenance: shim-boot-dead-lineage-tolerance-2026-09-06 — grep a build for
// this marker to prove its startup composition declares a lineage whose shim
// process is gone, releases what a quarantined lineage staged, and classifies
// an unresolvable refusal as a degraded posture rather than a dead daemon.
//
// THE THREE MEASURED STRANDS
//
// (A) A DEAD LINEAGE BRICKS THE BOOT. A shim-owned session is adopted and
// running; the daemon goes away and comes back with the shim's process gone
// and no tombstone (a crash, an OOM kill, a host reboot, an upgrade that races
// a dying child). The startup scan classifies the record exactly right — "the
// process is gone, no tombstone" — and then DROPS it: the record is retained
// as diagnostic evidence and reported nowhere. The adoption batch is a
// COMPLETE snapshot of the host, so a batch that omits a lineage the control
// plane still holds is refused, and that refusal aborted the whole
// composition. Every later start repeated it verbatim: the record was still
// stale, the batch still omitted it, and the host never came back. One dead
// seat took the org's durable sessions down permanently, with no operator
// route out.
//
// The daemon already HAD the fact it needed. So the stale record is declared
// in the batch — presented, never omitted — as a quarantined lineage carrying
// the exact shim incarnation the control plane knows it by. That is the
// strongest honest statement available here: a tombstone would forge the
// process-group reap proof a claim release depends on (§D10), and an absent
// attestation needs the registry record to be gone too, which it demonstrably
// is not. Quarantine is the third disposition and the one that fits: visible,
// capacity-charged, no controller authority, the shim not killed.
//
// (B) A QUARANTINED LINEAGE WITHDRAWS THE WHOLE HOST'S DURABILITY. One
// lineage's durable-adoption commit was refused; the pass quarantined it, and
// then failed carrier activation for the ENTIRE host, because a quarantined
// lineage keeps the mandatory Snapshot it staged during its adoption and
// nothing resolves it: "carrier activation left a staged Snapshot unresolved",
// durable sessions OFF for every seat on the host. A lineage that is
// quarantined now releases what it staged, so the composition of the rest of
// the host stays durable.
//
// NOTE ON THE REFUSAL ITSELF. Retrying a transiently-refused per-lineage
// commit is deliberately NOT done here. A composing embedder reaches its
// control plane through one HTTP choke point and spends a bounded, budgeted,
// Retry-After-honouring retry there, for every endpoint — so a retry in this
// package would stack on top of one that already exists, per lineage, inside a
// boot that must finish before the host may advertise capacity. Worse, a
// composer tears its adoption candidate down on every failure path before
// returning, so re-invoking the same callback with the same evidence is not
// the re-send it looks like. The refusal belongs to whoever owns the round
// trip; what belongs here is not letting one refused lineage cost the host.
//
// (C) A REFUSAL THAT CANNOT BE RESOLVED IS DEGRADED, NOT TERMINAL — AND
// NOTHING ELSE IS. When every bounded recovery is spent, a batch refusal that
// is genuinely unresolvable is returned as SessionShimDurabilityRefused: a
// classification, not a swallow. The daemon stands the composition down,
// records the reason where an operator can read it, and hands the typed error
// back so the embedder decides what to tell its operator. Every OTHER batch
// failure — transport, deadline, auth, an opaque status refusal, an ambiguous
// commit — keeps its old untyped error and its old consequence, because those
// are the failures a supervised restart actually recovers from, and swallowing
// them would trade one permanent outage for another.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/sessionshim"
)

// sessionShimStaleLineageDetail is the operator-facing detail carried by a
// declared stale lineage. It says what was checked and what was NOT concluded,
// because the same daemon that publishes it is the one an operator asks why a
// slot is charged for a session with no process behind it.
const sessionShimStaleLineageDetail = "the shim's recorded process identity is gone and it left no tombstone; " +
	"declared quarantined so the batch stays a complete snapshot — the harness process group is NOT proven reaped, " +
	"so this is not terminal evidence and the lineage stays capacity-charged until it is"

// sessionShimStaleLineageDeclarations turns the startup scan's stale records —
// a live discovery record whose shim process is provably gone, with no
// tombstone — into the batch entries that account for them.
//
// WHY QUARANTINE AND NOT A TOMBSTONE. The choice is forced, not preferred. A
// tombstone asserts the harness process group was reaped, and the shim
// vanishing says nothing whatsoever about the process group it was
// supervising; manufacturing one would put a reap proof in the ledger that
// nobody observed, and a claim release would follow it. An absent attestation
// is the weaker statement that fits a vanished shim — but it requires BOTH the
// process and the registry record to be absent, and a stale record is by
// definition still on disk. What remains is the disposition the corpus already
// defines for "live, accounted for, no controller authority here".
//
// WHAT THE DECLARATION COSTS, PLAINLY. The entry charges a capacity slot, and
// no automatic path in this daemon reclaims it: the stale record stays on disk
// by design, so every later boot re-declares the same lineage (an idempotent
// update at the exact incarnation, never a conflict), and the charge is
// released only by durable terminal evidence or by an explicit cleared entry —
// neither of which this pass produces, because neither can be produced
// honestly from "the shim is gone". Pairing the declaration with a producer
// for one of them is the follow-up that makes the slot recoverable.
//
// ONLY FOR A SCOPE THIS DAEMON ACTUALLY COMPOSES. A stale record is the
// longest-lived residue class on a host — nothing here removes it from disk, so
// it survives every restart — and the served-scope list comes from
// configuration, never from the registry. The two sets diverge the moment a
// host stops serving an organization it once served, and the leftover record
// outlives the credential receipt by an unbounded margin.
//
// Declaring such a record would put its organization into the batch's scope
// set, and the very first thing the per-scope loop does is resolve that scope's
// host identity — which, for a scope this daemon holds no retained credential
// receipt for, is a hard failure of the WHOLE composition, on every start,
// untyped. That is this file's own subject reintroduced through a different
// input: one dead local record permanently failing the boot. Skipping is
// correct and not a compromise: a lineage omitted from a batch that is never
// composed cannot violate a completeness rule, because no completeness rule is
// ever evaluated for it.
//
// The reason token is identity_mismatch: the closed v1 registry has no reason
// for "the record's process identity no longer resolves to a live process",
// and identity_mismatch is the one whose own definition covers a record whose
// process identity disagrees with reality — the same reason the scan already
// publishes when that identity cannot be verified at all. The precise fact
// travels in the detail, which is where an operator reads it; the receiver
// never reads a reason to decide a disposition.
func sessionShimStaleLineageDeclarations(
	stale []sessionshim.Record,
	served map[string]struct{},
	now time.Time,
) []sessionshim.QuarantinedSession {
	if len(stale) == 0 {
		return nil
	}
	out := make([]sessionshim.QuarantinedSession, 0, len(stale))
	for _, rec := range stale {
		if _, ok := served[rec.OrgID]; !ok {
			slog.Warn("session shim: a stale registry record names an organization this host no longer composes a batch for; "+
				"leaving it undeclared — its scope has no credential receipt to resolve a host identity against "+
				"(shim-boot-dead-lineage-tolerance-2026-09-06)",
				"session", rec.Identity().String(), "shim", rec.ShimID, "processEpoch", rec.ProcessEpoch)
			continue
		}
		out = append(out, sessionshim.NewQuarantinedSession(
			rec, sessionshim.QuarantineIdentityMismatch, sessionShimStaleLineageDetail, now))
	}
	sessionshim.SortQuarantined(out)
	return out
}

// sessionShimServedScopes is the set of organizations this daemon composes an
// adoption batch for, built from configuration exactly as the batch's own scope
// set seeds itself.
//
// It is the authority on "would a batch for this scope even be attempted", and
// therefore on whether omitting a lineage from one could matter. Deriving it
// here rather than reading the batch scope set is deliberate: the scope set is
// also grown FROM the discovered lineages, so asking it whether a lineage's
// scope is served would be circular.
func sessionShimServedScopes(cfg SessionShimConfig) map[string]struct{} {
	served := make(map[string]struct{}, len(cfg.AdoptionBatchOrgIDs)+1)
	for _, orgID := range cfg.AdoptionBatchOrgIDs {
		if orgID != "" {
			served[orgID] = struct{}{}
		}
	}
	served[cfg.orgID()] = struct{}{}
	return served
}

// SessionShimOmittedLineage is one lineage a composing control plane reports as
// omitted from a complete adoption batch.
//
// The identity fields are the control plane's own reading of the lineage it
// still holds. ShimID and ProcessEpoch are what make a declaration composable:
// a completeness rule matches on the exact shim incarnation, so a report that
// names only the session id can be logged but never answered.
type SessionShimOmittedLineage struct {
	Identity             sessionshim.Identity
	ShimID               string
	ProcessEpoch         uint64
	ControllerGeneration uint64
}

// SessionShimAdoptionBatchLineagesOmitted is the typed answer a composing
// OnAdoptionBatch returns when the control plane refused the batch because it
// still holds live lineages the batch did not account for.
//
// It is a contract, not a diagnosis. A bare refusal says only "refused", and
// the daemon's only remaining move is to fail the host. This type says WHICH
// lineages the control plane is still holding, which is exactly the input a
// re-composition needs: each one is declared quarantined at its exact
// incarnation and the batch is committed again.
//
// Lineages must name every lineage the refusal is about, up to whatever the
// control plane's own report limit is. TotalOmitted is that report's
// UNTRUNCATED total when the control plane sends one — the report list may be
// capped while the real set is larger, and a recovery bound derived from the
// capped list would stop before the work does.
type SessionShimAdoptionBatchLineagesOmitted struct {
	Lineages     []SessionShimOmittedLineage
	TotalOmitted int
	Err          error
}

func (e *SessionShimAdoptionBatchLineagesOmitted) Error() string {
	if len(e.Lineages) == 0 {
		return fmt.Sprintf(
			"session shim: the control plane refused this batch as incomplete and named no lineage: %v", e.Err)
	}
	ids := make([]string, 0, len(e.Lineages))
	for _, lineage := range e.Lineages {
		ids = append(ids, lineage.Identity.String())
	}
	rendered := strings.Join(ids, ", ")
	if e.TotalOmitted > len(e.Lineages) {
		rendered = fmt.Sprintf("%s (%d of %d reported)", rendered, len(e.Lineages), e.TotalOmitted)
	}
	return fmt.Sprintf(
		"session shim: the control plane still holds %s, which this batch did not account for: %v",
		rendered, e.Err)
}

func (e *SessionShimAdoptionBatchLineagesOmitted) Unwrap() error { return e.Err }

// sessionShimLineageKey is one lineage's exact incarnation: the identity plus
// the shim and process epoch that distinguish two incarnations of the same
// session from each other.
//
// Everything in this file keys on the whole tuple, because the control plane
// does. Its completeness report legitimately names an OLDER incarnation of a
// session this boot has just re-adopted at a newer one, and its commit route
// accepts `adopted@X` and `quarantined@Y` under one session id as two
// independent lineages — that is the ordinary controller-handoff shape, not a
// contradiction. A recovery that matched on session id alone would answer a
// refusal about the old incarnation by evicting the live new one.
type sessionShimLineageKey struct {
	identity     sessionshim.Identity
	shimID       string
	processEpoch uint64
}

func lineageKeyOfQuarantine(entry sessionshim.QuarantinedSession) sessionShimLineageKey {
	return sessionShimLineageKey{entry.Identity(), entry.ShimID, entry.ProcessEpoch}
}

// sessionShimBatchDeclaringOmittedLineages adds a quarantined declaration for
// every named lineage the batch does not already account for, and reports the
// declarations it made.
//
// A lineage the batch ALREADY presents at that exact incarnation — adopted,
// quarantined, tombstoned, or cleared — is skipped: re-declaring it would
// present one lineage twice, which the receiver refuses as a duplicate, and
// would double-charge it against capacity. Reporting nothing is the caller's
// signal that the refusal names nothing this daemon can answer, and that there
// is nothing narrower to do than fail.
//
// A lineage the control plane could not name an exact incarnation for is
// skipped too, for the same reason a cleared entry demands one: a declaration
// that names "some shim for this session" cannot be matched against the
// completeness rule it is trying to satisfy, and would put an unmatched entry
// in the ledger.
func sessionShimBatchDeclaringOmittedLineages(
	batch SessionShimAdoptionBatch,
	omitted []SessionShimOmittedLineage,
	detail string,
) (SessionShimAdoptionBatch, []sessionshim.QuarantinedSession) {
	present := make(map[sessionShimLineageKey]struct{})
	for _, outcome := range batch.Adopted {
		present[sessionShimLineageKey{
			outcome.Evidence.Identity, outcome.Evidence.ShimID, outcome.Evidence.ProcessEpoch,
		}] = struct{}{}
	}
	for _, entry := range batch.Quarantined {
		present[lineageKeyOfQuarantine(entry)] = struct{}{}
	}
	for _, entry := range batch.Tombstoned {
		present[sessionShimLineageKey{entry.Identity, entry.ShimID, entry.ProcessEpoch}] = struct{}{}
	}
	for _, entry := range batch.Cleared {
		present[sessionShimLineageKey{entry.Identity(), entry.ShimID, entry.ProcessEpoch}] = struct{}{}
	}
	declarations := make([]sessionshim.QuarantinedSession, 0, len(omitted))
	for _, lineage := range omitted {
		if lineage.Identity.OrgID != batch.OrgID {
			continue
		}
		if lineage.Identity.SessionID == "" || lineage.ShimID == "" ||
			lineage.ProcessEpoch == 0 || lineage.ControllerGeneration == 0 {
			// The generation is guarded for the same reason as the shim id and
			// the epoch, and it needs saying because it is the one that looks
			// optional. The control plane's report renders it NULLABLE, and a
			// null arrives here as a zero — so declaring on it would present
			// the lineage at a generation the receiver's exact-key lookup
			// cannot match, leaving the refusal unanswered while adding a row
			// that answers nothing.
			//
			// A locally-discovered stale record is the deliberate exception and
			// does not come through here: a frozen v1 record carries no
			// authenticated generation at all, so zero there is the honest
			// conservative value the quarantine projection already defines,
			// not a field the reporter dropped.
			continue
		}
		key := sessionShimLineageKey{lineage.Identity, lineage.ShimID, lineage.ProcessEpoch}
		if _, ok := present[key]; ok {
			continue
		}
		present[key] = struct{}{}
		declarations = append(declarations, sessionshim.QuarantinedSession{
			OrgID:                lineage.Identity.OrgID,
			SessionID:            lineage.Identity.SessionID,
			ShimID:               lineage.ShimID,
			ProcessEpoch:         lineage.ProcessEpoch,
			ControllerGeneration: lineage.ControllerGeneration,
			Reason:               sessionshim.QuarantineIdentityMismatch,
			Detail:               detail,
			ConsumesCapacity:     true,
		})
	}
	if len(declarations) == 0 {
		return batch, nil
	}
	batch.Quarantined = append(append([]sessionshim.QuarantinedSession(nil), batch.Quarantined...), declarations...)
	return batch, declarations
}

// sessionShimOmittedLineageDetail is the detail a re-composed declaration
// carries. It names the control plane as the source, because that is the whole
// provenance of the entry: this daemon never saw the lineage on disk.
const sessionShimOmittedLineageDetail = "the control plane reported this lineage as still live and unaccounted for at boot, " +
	"and this daemon holds no adoption of that exact incarnation; declared quarantined so the batch stays a complete " +
	"snapshot rather than refusing the whole host's durable sessions"

const (
	// sessionShimOmittedRecompositionPasses is the fixed ceiling on how many
	// times the boot pass will answer a completeness refusal and commit again.
	//
	// It is fixed rather than derived from the batch, because the quantity that
	// matters — how many lineages the RECEIVER is still holding — is not
	// bounded by what this host has on disk, and a receiver that truncates its
	// report to a page at a time would otherwise be answered with a bound
	// smaller than the work. It is a ceiling rather than an appetite: each pass
	// declares at least one lineage or the loop stops on its own, and a
	// refusal that keeps naming new lineages past this many passes is a
	// disagreement no amount of re-committing settles.
	sessionShimOmittedRecompositionPasses = 8
	// sessionShimRecompositionBackoff paces the passes. The control plane just
	// refused this batch; committing again in the same instant asks the same
	// question of a receiver that has had no time to be asked it.
	//
	// IT DOUBLES, AND THE TOTAL IS NOT SMALL — size the ceiling from this, not
	// from the first term. The seven paced passes sleep 50+100+200+400+800+
	// 1600+3200 ms = 6.35 s of daemon-side wait, and each of the eight passes
	// additionally carries the composing callback's own bounded round trip,
	// sequentially, per scope. A worst case measured in minutes is therefore
	// reachable, not seconds.
	//
	// That is affordable only because of WHERE this runs: the pass completes
	// before anything announces the shim as active, so the cost is capacity not
	// yet advertised rather than a host demoted mid-flight. And the loop can
	// only spin on completeness refusals — any other answer fails the errors.As
	// on the next pass and breaks out immediately — so the realistic figure is
	// one round trip plus a few hundred milliseconds.
	sessionShimRecompositionBackoff = 50 * time.Millisecond
)

// commitBootBatchDeclaringOmittedLineages is the startup pass's recovery from a
// batch the control plane refused for lineages it still holds and this batch
// did not account for.
//
// It declares exactly those lineages and commits again. It repeats while each
// answer names another lineage the batch can absorb, under the fixed pass
// ceiling above, because a control plane that reports its omissions a page at a
// time would otherwise still cost the host its durable sessions on the second
// page.
//
// WHAT IT DOES AND DOES NOT EVICT. A declaration whose incarnation matches a
// lineage this boot adopted is that lineage's own refusal: it is dropped from
// the adopted set and recorded in the caller's failure map, so the live
// projection, the gate resolution, and every later batch agree. A declaration
// at a DIFFERENT incarnation of the same session is a different lineage — the
// control plane's report legitimately names a predecessor of a session this
// boot has just re-adopted — and evicting on it would close this daemon's
// socket to a healthy shim and publish a projection that contradicts the batch
// just committed. Those are returned instead, for the caller to add to the live
// projection alongside the batch that already carries them.
//
// A refusal that names nothing declarable, or a re-commit that fails for any
// other reason, returns the failure and the caller degrades exactly as it
// would have. This narrows a host-wide failure to a set of per-lineage ones;
// it does not invent a way to succeed.
func (d *Daemon) commitBootBatchDeclaringOmittedLineages(
	ctx context.Context,
	batch SessionShimAdoptionBatch,
	cause error,
	entries map[sessionshim.Identity]adoptedShim,
	adoptionFailures map[sessionshim.Identity]sessionshim.QuarantinedSession,
) (SessionShimAdoptionBatchReceipt, []sessionshim.QuarantinedSession, error) {
	accumulated := make([]sessionshim.QuarantinedSession, 0, sessionShimOmittedRecompositionPasses)
	for pass := 0; pass < sessionShimOmittedRecompositionPasses; pass++ {
		var omitted *SessionShimAdoptionBatchLineagesOmitted
		if !errors.As(cause, &omitted) || len(omitted.Lineages) == 0 {
			break
		}
		amended, declarations := sessionShimBatchDeclaringOmittedLineages(
			batch, omitted.Lineages, sessionShimOmittedLineageDetail)
		if len(declarations) == 0 {
			// The refusal names nothing this batch can absorb; there is
			// nothing narrower than the whole composition left to do.
			break
		}
		batch = amended
		accumulated = append(accumulated, declarations...)
		if pass > 0 {
			if waitErr := d.waitSessionShimRecompositionBackoff(ctx, pass); waitErr != nil {
				// WAITERR CARRIES THE %w, NOT THE REFUSAL. The refusal is what
				// this pass was answering; the cancellation is what actually
				// ended it, and it is the transient half. Wrapping the refusal
				// here instead would put a completeness refusal on an errors.As
				// path — classifying a cancelled or expired boot as an
				// unresolvable durability refusal, standing the composition
				// down and stamping the status surface permanently for a
				// context that simply ran out. Every other guard in this file
				// keeps a deadline out of that classification; this verb is
				// what keeps this door shut too.
				return SessionShimAdoptionBatchReceipt{}, nil,
					fmt.Errorf("%v; re-composition pass %d abandoned: %w", cause, pass+1, waitErr)
			}
		}
		receipt, err := d.completeSessionShimAdoptionBatch(ctx, batch)
		if err == nil {
			projectionOnly := d.recordDeclaredOmittedLineages(accumulated, entries, adoptionFailures, receipt)
			return receipt, projectionOnly, nil
		}
		cause = err
	}
	if len(accumulated) == 0 {
		return SessionShimAdoptionBatchReceipt{}, nil, cause
	}
	return SessionShimAdoptionBatchReceipt{}, nil, fmt.Errorf(
		"session shim: re-committing the boot batch declaring %d omitted lineage(s): %w",
		len(accumulated), cause)
}

// recordDeclaredOmittedLineages applies a committed re-composition to this
// daemon's local state, and returns the declarations that must go into the
// live projection WITHOUT evicting anything.
//
// The incarnation test is the whole function. `entries` is keyed by lifecycle
// identity, so a session with a live adoption at one incarnation and a
// control-plane-reported predecessor at another collides in that map — and the
// collision resolves the wrong way round, evicting the live one, unless the
// shim id and process epoch are compared.
func (d *Daemon) recordDeclaredOmittedLineages(
	declarations []sessionshim.QuarantinedSession,
	entries map[sessionshim.Identity]adoptedShim,
	adoptionFailures map[sessionshim.Identity]sessionshim.QuarantinedSession,
	receipt SessionShimAdoptionBatchReceipt,
) []sessionshim.QuarantinedSession {
	projectionOnly := make([]sessionshim.QuarantinedSession, 0, len(declarations))
	for _, declaration := range declarations {
		id := declaration.Identity()
		entry, adopted := entries[id]
		sameIncarnation := adopted &&
			entry.adoption.ShimID == declaration.ShimID &&
			entry.adoption.ProcessEpoch == declaration.ProcessEpoch
		if adopted && !sameIncarnation {
			// A predecessor of a session this boot re-adopted. The batch
			// presents both, and so must the projection.
			projectionOnly = append(projectionOnly, declaration)
			slog.Warn("session shim: the control plane still held an EARLIER incarnation of a session this boot "+
				"re-adopted; declaring that incarnation quarantined and keeping the live one adopted "+
				"(shim-boot-dead-lineage-tolerance-2026-09-06)",
				"session", id.String(), "declaredShim", declaration.ShimID,
				"declaredProcessEpoch", declaration.ProcessEpoch,
				"adoptedShim", entry.adoption.ShimID, "adoptedProcessEpoch", entry.adoption.ProcessEpoch,
				"revision", receipt.AdoptionRevision)
			continue
		}
		if !adopted {
			if existing, failed := adoptionFailures[id]; failed &&
				(existing.ShimID != declaration.ShimID || existing.ProcessEpoch != declaration.ProcessEpoch) {
				// A different incarnation already failed its own adoption on
				// this boot. Overwriting it would drop that lineage from every
				// surface the failure map feeds.
				projectionOnly = append(projectionOnly, declaration)
				continue
			}
			adoptionFailures[id] = declaration
			slog.Warn("session shim: the control plane still held this lineage at boot and this daemon has no "+
				"adoption of it; declaring it quarantined and composing the rest of the host — no shim is killed "+
				"and no terminal evidence is manufactured (shim-boot-dead-lineage-tolerance-2026-09-06)",
				"session", id.String(), "shim", declaration.ShimID,
				"processEpoch", declaration.ProcessEpoch, "revision", receipt.AdoptionRevision)
			continue
		}
		delete(entries, id)
		adoptionFailures[id] = declaration
		slog.Warn("session shim: the control plane refused this boot's adoption of this exact incarnation; releasing "+
			"this daemon's control socket to it, declaring it quarantined, and composing the rest of the host — "+
			"the shim keeps its harness (shim-boot-dead-lineage-tolerance-2026-09-06)",
			"session", id.String(), "shim", declaration.ShimID,
			"processEpoch", declaration.ProcessEpoch, "revision", receipt.AdoptionRevision)
	}
	return projectionOnly
}

// waitSessionShimRecompositionBackoff sleeps the doubling delay before a
// re-composition pass, returning the context's own failure if it expires first.
func (d *Daemon) waitSessionShimRecompositionBackoff(ctx context.Context, pass int) error {
	delay := sessionShimRecompositionBackoff << (pass - 1)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SessionShimDurabilityRefused reports that one scope's boot adoption batch was
// refused for a reason no bounded recovery can settle, and that the daemon has
// stood the composition down rather than failing.
//
// IT IS A CLASSIFICATION, NOT A SWALLOW, AND THE DISTINCTION IS THE POINT.
// This type is produced ONLY for a refusal whose shape says re-asking cannot
// help: a completeness refusal this daemon has nothing left to declare for, or
// a recorded-evidence conflict its own recovery could not narrow. A transport
// failure, a deadline, an expired credential, an opaque status refusal, an
// ambiguous commit — none of those produce it, because all of them are
// recovered by the ordinary path a plain error already takes, and a host that
// silently disabled durable sessions on a relay blip would trade a transient
// outage for a permanent one.
//
// The embedder decides what a refusal means for its operator. Returning the
// type rather than nil is what lets it: a caller that treats every error as
// fatal keeps a working host by classifying this one, and a caller that does
// not classify it behaves exactly as it did before this change. Returning nil
// instead would make every embedder announce a success that did not happen.
type SessionShimDurabilityRefused struct {
	// Scope is the organization whose batch was refused.
	Scope string
	// Lineages are the lifecycle identities the refusal named, when it named
	// any. Empty is honest: a whole-batch refusal names none.
	Lineages []sessionshim.Identity
	// Err is the underlying refusal, kept for the operator line and for
	// callers that classify on what is beneath it.
	Err error
}

func (e *SessionShimDurabilityRefused) Error() string {
	return fmt.Sprintf("session shim: durable adoption batch for organization %q: %v", e.Scope, e.Err)
}

func (e *SessionShimDurabilityRefused) Unwrap() error { return e.Err }

// LineageIDs renders the named lineages for one operator line, or "none named"
// when the refusal named no lineage at all.
func (e *SessionShimDurabilityRefused) LineageIDs() string {
	if len(e.Lineages) == 0 {
		return "none named"
	}
	ids := make([]string, 0, len(e.Lineages))
	for _, id := range e.Lineages {
		ids = append(ids, id.String())
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// sessionShimBatchRefusalIsUnresolvable reports whether a spent batch failure
// is one this daemon has provably run out of moves for.
//
// Exactly two shapes qualify, and both are TYPED answers a composing callback
// chose to return — never a status code, never a string. That is deliberate:
// an embedder that has decoded its control plane's closed refusal vocabulary
// can say "this is a completeness refusal about these lineages", and only that
// statement is strong enough to justify bringing durable sessions up off. An
// undecoded failure is, by construction, one this daemon does not understand,
// and the honest response to a failure you do not understand is to fail.
//
// An ambiguous commit is excluded explicitly even when it wraps one of the two
// types: outcome-unknown means the control plane may have COMMITTED the batch,
// and standing down over a batch that landed would abandon a durable
// composition this host actually holds.
func sessionShimBatchRefusalIsUnresolvable(err error) bool {
	if err == nil || sessionShimCommitOutcomeUnknown(err) ||
		errors.Is(err, errSessionShimAmbiguousBatchCommit) {
		return false
	}
	var omitted *SessionShimAdoptionBatchLineagesOmitted
	if errors.As(err, &omitted) {
		return true
	}
	var recorded *SessionShimAdoptionEvidenceRecorded
	return errors.As(err, &recorded)
}

// newSessionShimDurabilityRefused classifies one spent batch refusal, lifting
// whatever lineages the control plane named out of the typed answers that
// carry them so the operator line can say which session is responsible.
//
// It returns nil when the failure is not one of the unresolvable shapes, which
// is the caller's signal to keep its ordinary error.
func newSessionShimDurabilityRefused(scope string, err error) *SessionShimDurabilityRefused {
	if !sessionShimBatchRefusalIsUnresolvable(err) {
		return nil
	}
	refused := &SessionShimDurabilityRefused{Scope: scope, Err: err}
	var omitted *SessionShimAdoptionBatchLineagesOmitted
	if errors.As(err, &omitted) {
		for _, lineage := range omitted.Lineages {
			refused.Lineages = append(refused.Lineages, lineage.Identity)
		}
		return refused
	}
	var recorded *SessionShimAdoptionEvidenceRecorded
	if errors.As(err, &recorded) {
		refused.Lineages = append(refused.Lineages, recorded.Lineages...)
	}
	return refused
}

// retainSessionShimDurabilityRefusal records the refusal on the daemon so an
// operator can read WHY durable sessions are off, not merely THAT they are.
//
// A boolean is not a diagnosis. The posture this sets lasts until something
// re-installs the composition, and an operator who can only see `off` has to
// go and find the process log to learn which scope refused and which lineage
// it was about — on a host whose durable sessions are already gone. The record
// is deliberately non-secret: a scope id, lifecycle identities, and the
// refusal's own text.
func (d *Daemon) retainSessionShimDurabilityRefusal(refused *SessionShimDurabilityRefused) {
	if d == nil || refused == nil {
		return
	}
	d.sessionShimDurabilityRefusal.Store(&SessionShimDurabilityRefusal{
		Scope:      refused.Scope,
		Lineages:   append([]sessionshim.Identity(nil), refused.Lineages...),
		Reason:     refused.Err.Error(),
		AtUnixNano: d.shimNow().UnixNano(),
	})
}

// clearSessionShimDurabilityRefusal forgets a retained refusal, so a later
// install that succeeds does not leave the host reporting a refusal it has
// since recovered from.
func (d *Daemon) clearSessionShimDurabilityRefusal() {
	if d == nil {
		return
	}
	d.sessionShimDurabilityRefusal.Store(nil)
}

// SessionShimDurabilityRefusal is the retained, non-secret reason this host's
// durable sessions are off. Nil when none was refused.
type SessionShimDurabilityRefusal struct {
	Scope      string
	Lineages   []sessionshim.Identity
	Reason     string
	AtUnixNano int64
}

// SessionShimDurabilityRefusal returns the retained refusal, or nil.
func (d *Daemon) SessionShimDurabilityRefusal() *SessionShimDurabilityRefusal {
	if d == nil {
		return nil
	}
	retained := d.sessionShimDurabilityRefusal.Load()
	if retained == nil {
		return nil
	}
	clone := *retained
	clone.Lineages = append([]sessionshim.Identity(nil), retained.Lineages...)
	return &clone
}

// sessionShimDurabilityRefusalReasonLimit bounds the reason text carried onto
// the status surface. The composing embedder's refusal deliberately does not
// reflect a control plane's free-text error, but this daemon does not get to
// assume that of every embedder, and status output is not the place to
// discover an unbounded string.
const sessionShimDurabilityRefusalReasonLimit = 512

// sessionShimDurabilityRefusalStatus renders the retained refusal for the
// bounded, secret-free diagnostics projection, or nil when none was recorded.
func (d *Daemon) sessionShimDurabilityRefusalStatus() *afclient.DaemonSessionShimDurabilityRefusal {
	retained := d.SessionShimDurabilityRefusal()
	if retained == nil {
		return nil
	}
	out := &afclient.DaemonSessionShimDurabilityRefusal{Scope: retained.Scope, Reason: retained.Reason}
	if len(out.Reason) > sessionShimDurabilityRefusalReasonLimit {
		out.Reason = out.Reason[:sessionShimDurabilityRefusalReasonLimit] + "…"
	}
	for _, id := range retained.Lineages {
		out.Lineages = append(out.Lineages, id.String())
	}
	sort.Strings(out.Lineages)
	if retained.AtUnixNano > 0 {
		out.RefusedAt = time.Unix(0, retained.AtUnixNano).UTC().Format(time.RFC3339)
	}
	return out
}
