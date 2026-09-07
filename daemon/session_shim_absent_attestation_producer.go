package daemon

// session_shim_absent_attestation_producer.go — composing and submitting the
// shim-absent attestation for a lineage this daemon PROVED it cannot observe.
//
// THE HOLE THIS FILLS
//
// SessionShimAbsentAttestation, its completeness rule, and its mutual exclusion
// with a tombstone have been in this package since the recovery seams landed —
// and nothing in production ever constructed one. The type was reachable only
// from its own unit tests, so the failure it was designed for had no code path
// at all.
//
// That failure is ordinary. A shim killed by SIGKILL, an OOM kill, or a power
// cut never runs its finalizer, so it writes no tombstone. What it leaves is a
// discovery record naming a process identity that is no longer running. §D10
// gives that state exactly one honest reading: the daemon cannot report the
// lineage terminal (no tombstone exists, and manufacturing one would forge the
// reap proof a claim release depends on) and it cannot drop the lineage either
// (a complete batch that omits a correlation the composer still holds is
// refused). Before this file, the daemon did neither: the lineage sat in the
// quarantine projection forever, its recovery obligation stayed `active`, and
// the host's batch composition was wedged behind it.
//
// WHAT THE ATTESTATION IS, AND IS NOT
//
// It proves UNOBSERVABILITY, never death. A vanished supervisor says nothing
// about the process group it was supervising, so the attestation closes what
// the daemon owes the composer and never what the session owes the fence. The
// receiver converts the lineage's recovery obligation from active to ABANDONED
// — never resolved — and it may never be read as reap proof. That is why the
// two facts travel as separate fields, why Complete() demands both, and why
// reportSessionShimTerminalEvidence refuses a report that also carries a
// tombstone. It does NOT terminalize the session row or release its seat; the
// control plane owns that and an abandoned obligation never satisfies it.
//
// THE PROOF IS DESTRUCTIVE, SO EVERY STEP IS BUILT AROUND THAT
//
// Composing the attestation requires making its second fact true: the exact
// incarnation's discovery record must be gone. Three properties follow, and
// each one is a correctness requirement rather than a refinement.
//
//  1. NOTHING IRREVERSIBLE BEFORE ACCEPTANCE. The record is RENAMED to a
//     non-discovery sidecar, not unlinked (sessionshim/absence.go). Adopt
//     cannot see a sidecar, so the discovery record is genuinely gone; but the
//     recorded (pid, start time) survives, so a daemon whose report was refused
//     and which then restarts re-reads the sidecar and re-submits the same
//     attestation instead of losing the lineage entirely. Unlinking made the
//     restart case strictly worse than never having tried: the composer keeps
//     an active obligation for a correlation nothing on the host can name any
//     more, and every later complete batch is refused for omitting it.
//
//  2. TWO OBSERVATIONS, NOT ONE. Before this producer, a false "not running"
//     answer was inert — the lineage was classified stale, logged, and its
//     record kept as diagnostic evidence. Now the same misread withdraws the
//     record, abandons the obligation and drops the row. ProcessIdentity.Alive
//     cannot discriminate: on darwin the sysctl reports an unknown pid through
//     four errnos including EIO, and on linux a daemon in another pid namespace
//     or under hidepid reads ENOENT for a perfectly live process. So absence is
//     concluded only from two separated readings that agree AND a socket dial
//     that nothing answers. The dial catches the persistent misread (a live
//     shim answers its socket whatever procfs says); the separation catches the
//     transient one. Either observation erroring is UNKNOWN — retain, never
//     discharge.
//
//  3. THE TOMBSTONE IS RE-READ IMMEDIATELY BEFORE THE WITHDRAWAL. A shim
//     publishes its tombstone and only then spends its courtesy windows before
//     exiting, so "the process is gone" implies "the tombstone is already
//     written, if the finalizer ran at all". Reading the tombstone once at the
//     top leaves a window in which a proven reap is downgraded to mere
//     unobservability — which holds the release predicate forever and strands
//     the tombstone on disk, because the reconcile only reaches a lineage
//     through the quarantine set this discharge removes it from.
//
// ORDERING AGAINST THE PROJECTION
//
// The lineage stays in the quarantine projection for the whole round trip and
// leaves it only after the composer durably accepts — the same order, and for
// the same reason, as handOffQuarantinedTerminalProof. While the report is in
// flight the composer's obligation is still `active` and its completeness
// cover-set still includes this lineage; a projection that dropped it early
// would be refused as a batch that omitted a live lineage. The republish
// afterwards is load-bearing rather than belt-and-braces: the receiver prunes
// its own quarantine snapshot for ordinary terminal evidence but NOT for an
// absent attestation delivered through the standalone terminal-evidence seam,
// so nothing else reconciles the two sets.
//
// THE CLAIM IS TAKEN BEFORE THE PROBE
//
// The sweep this producer hangs off is called from every occupancy and
// heartbeat surface, and its contract is that only the owning pass does work.
// Probing first would make every other pass pay a full registry scan per
// quarantined lineage per call — and the ordinary steady state of the
// quarantine set is lineages whose shims are ALIVE, which never reach the
// claim at all. Claiming first also means the refusal cool-down throttles the
// disk work, not just the round trip, and no losing pass can have already
// withdrawn a record.

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

const (
	// absenceObservationSeparation is how far apart the two readings of a
	// recorded process identity must be.
	//
	// It is short enough that a genuinely absent lineage is discharged inside
	// one sweep, and it is paid ONCE per lineage by the one pass that owns the
	// handoff — a live shim fails the first reading or answers its socket and
	// never reaches the wait at all. It is real time, deliberately: the wait
	// sleeps in real time, so measuring it against an injected clock would make
	// the separation mean different things in a test and on a host.
	absenceObservationSeparation = 250 * time.Millisecond

	// absenceSocketProbeTimeout bounds the second-opinion dial. A shim that
	// answers at all is observable, so the interesting answer arrives
	// immediately; this only bounds the pathological case of a socket that
	// accepts the connection slowly.
	absenceSocketProbeTimeout = 250 * time.Millisecond
)

// sessionShimAbsenceOrigin names the seam that proved a lineage unobservable.
// It is diagnostic only — the attestation's content is identical whichever seam
// produced it — but "which seam noticed" is the first question asked of a host
// that discharged a lineage, and deriving it from a stack trace after the fact
// is not an answer.
type sessionShimAbsenceOrigin string

const (
	// absenceOriginStartupAdoption is the §D4 pass: a registry record whose
	// process is gone with no tombstone, classified stale by sessionshim.Adopt.
	absenceOriginStartupAdoption sessionShimAbsenceOrigin = "startup-adoption"
	// absenceOriginWithdrawnResubmit is the startup pass re-submitting a
	// discharge a previous daemon incarnation withdrew but never got accepted.
	absenceOriginWithdrawnResubmit sessionShimAbsenceOrigin = "withdrawn-resubmit"
	// absenceOriginQuarantineSweep is the periodic reconcile that already walks
	// the quarantine projection looking for tombstones.
	absenceOriginQuarantineSweep sessionShimAbsenceOrigin = "quarantine-sweep"
)

// sessionShimAbsenceProof is the retained unobservability observation for ONE
// exact incarnation. It short-circuits a re-probe on the retry path; the
// durable copy of the same fact is the registry sidecar, which is what makes
// the retry survive a restart.
type sessionShimAbsenceProof struct {
	process    sessionshim.ProcessIdentity
	observedAt int64
}

// attestation renders the retained observation as the wire fact. Both booleans
// are unconditional here on purpose: nothing may reach this constructor that
// has not already proved both halves.
func (p sessionShimAbsenceProof) attestation() SessionShimAbsentAttestation {
	return SessionShimAbsentAttestation{
		ProcessIdentityAbsent: true,
		RegistryRecordAbsent:  true,
		ObservedAtUnixNano:    p.observedAt,
	}
}

// retainedSessionShimAbsence returns a proof this daemon already made for the
// exact incarnation.
func (d *Daemon) retainedSessionShimAbsence(key shimIncarnation) (sessionShimAbsenceProof, bool) {
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	proof, ok := d.shims.absenceProofs[key]
	return proof, ok
}

// retainSessionShimAbsence records one proof so a refused report can be retried
// within this daemon's life without re-probing.
func (d *Daemon) retainSessionShimAbsence(key shimIncarnation, proof sessionShimAbsenceProof) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	if d.shims.absenceProofs == nil {
		d.shims.absenceProofs = make(map[shimIncarnation]sessionShimAbsenceProof)
	}
	d.shims.absenceProofs[key] = proof
}

// forgetSessionShimAbsence drops a retained proof once its lineage is durably
// discharged and its sidecar disposed. Nothing can rediscover the incarnation
// after that, so a retained entry would have no reader for the daemon's life.
func (d *Daemon) forgetSessionShimAbsence(key shimIncarnation) {
	d.shims.mu.Lock()
	delete(d.shims.absenceProofs, key)
	d.shims.mu.Unlock()
}

// sessionShimLiveRecord reads the exact incarnation's live discovery record. A
// missing record is not an error: it means this daemon has no observation to
// prove absence FROM, which is a different answer from "the process is gone".
func sessionShimLiveRecord(
	registry *sessionshim.Registry,
	key shimIncarnation,
) (sessionshim.Record, bool) {
	entries, err := registry.Scan()
	if err != nil {
		return sessionshim.Record{}, false
	}
	for _, entry := range entries {
		if entry.Err != nil || entry.Record.Identity() != key.identity ||
			entry.Record.ShimID != key.shimID || entry.Record.ProcessEpoch != key.processEpoch {
			continue
		}
		return entry.Record, true
	}
	return sessionshim.Record{}, false
}

// sessionShimSocketAnswers reports whether anything is listening on the
// record's socket.
//
// This is the second opinion that procfs cannot give. A shim whose socket
// accepts a connection is observable by definition, whatever the process table
// says — and the process table is exactly what is unreliable here: darwin maps
// four errnos including EIO onto "no such process", and a daemon in another pid
// namespace or under hidepid reads ENOENT for a live one. A connect that
// succeeds is therefore a veto on the whole attestation.
//
// A refused or unreachable socket answers nothing on its own; it is only ever
// read as "this did not veto", never as proof of absence.
func sessionShimSocketAnswers(path string) bool {
	if path == "" {
		return false
	}
	conn, err := net.DialTimeout("unix", path, absenceSocketProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// observeSessionShimAbsence is the two-reading probe.
//
// first is the record this pass started from. second re-derives the record for
// the same incarnation after the separation, so the pass sees a process that
// came back, a record that was rewritten, and a record that vanished
// underneath it — each of which means this daemon may NOT say the lineage is
// unobservable.
//
// Every negative answer here is "unknown, retain", never "gone". That
// asymmetry is the whole point: the cost of retaining a dead lineage for one
// more sweep is a sweep, and the cost of discharging a live one is a running
// harness whose obligation has been abandoned.
func (d *Daemon) observeSessionShimAbsence(
	key shimIncarnation,
	first sessionshim.Record,
	second func() (sessionshim.Record, bool),
) bool {
	if first.SocketPath == "" {
		// With no socket to dial there is only one kind of evidence available,
		// and one kind is what this function exists not to trust.
		slog.Warn("session shim: recorded incarnation has no socket to second-guess the process table with; not attesting absence",
			"session", key.identity.String(), "shim", key.shimID)
		return false
	}
	if !d.sessionShimProcessGone(key, first, "first") {
		return false
	}
	if sessionShimSocketAnswers(first.SocketPath) {
		slog.Info("session shim: the recorded process reads as gone but its socket still answers; retaining the lineage",
			"session", key.identity.String(), "shim", key.shimID)
		return false
	}
	if hook := d.afterFirstAbsenceObservation(); hook != nil {
		hook(key)
	}
	time.Sleep(absenceObservationSeparation)
	confirm, ok := second()
	if !ok {
		slog.Warn("session shim: the recorded incarnation changed between absence observations; retaining the lineage",
			"session", key.identity.String(), "shim", key.shimID)
		return false
	}
	if confirm.PID != first.PID || confirm.ProcessStartedAt != first.ProcessStartedAt {
		slog.Warn("session shim: the recorded process identity changed between absence observations; retaining the lineage",
			"session", key.identity.String(), "shim", key.shimID)
		return false
	}
	// The SECOND reading of the same identity. It is not redundant with the
	// comparison above: the case it exists for is an identity that never
	// changed and a reading that did — a transient EIO/EINVAL from the darwin
	// sysctl, or a momentary ENOENT, which makes a live process read as gone
	// exactly once. Nothing about the record moves in that failure, so only
	// re-reading catches it.
	return d.sessionShimProcessGone(key, confirm, "second")
}

// sessionShimProcessGone is one reading. An error is UNKNOWN and never gone —
// §D7 classifies an identity this daemon cannot check as quarantine, and
// quarantine is where it stays, and a probe that read an error as death would
// discharge a lineage on the strength of the one answer it did not get.
func (d *Daemon) sessionShimProcessGone(key shimIncarnation, rec sessionshim.Record, reading string) bool {
	identity := sessionshim.ProcessIdentity{PID: rec.PID, StartedAt: rec.ProcessStartedAt}
	alive, err := d.readSessionShimLiveness(identity)
	if err != nil {
		slog.Warn("session shim: process identity unverifiable; not attesting absence",
			"session", key.identity.String(), "shim", key.shimID, "reading", reading, "error", err)
		return false
	}
	return !alive
}

// readSessionShimLiveness reads one process identity through the probe's test
// seam when one is installed, and the OS otherwise.
func (d *Daemon) readSessionShimLiveness(identity sessionshim.ProcessIdentity) (bool, error) {
	d.shims.mu.RLock()
	scripted := d.shims.absenceLiveness
	d.shims.mu.RUnlock()
	if scripted != nil {
		return scripted(identity)
	}
	return identity.Alive()
}

// afterFirstAbsenceObservation returns the test seam that runs between the two
// readings, or nil in every production daemon.
func (d *Daemon) afterFirstAbsenceObservation() func(shimIncarnation) {
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return d.shims.afterFirstAbsenceObservation
}

// proveSessionShimAbsence runs the bounded probe §D10 requires and returns the
// completed attestation only when both of its facts hold.
//
// Every "no" is a real answer rather than a deferral: a live process, a socket
// that answers, an unverifiable identity, a tombstone, a record that changed
// underneath the probe, or a withdrawal that would not take all mean this
// daemon may not say the lineage is unobservable.
func (d *Daemon) proveSessionShimAbsence(
	registry *sessionshim.Registry,
	key shimIncarnation,
) (SessionShimAbsentAttestation, bool) {
	if registry == nil || key.shimID == "" || key.processEpoch == 0 {
		// reportSessionShimTerminalEvidence refuses an attestation without the
		// exact incarnation, and it is right to: an attestation that cannot
		// name which shim it is about discharges nothing the composer holds.
		return SessionShimAbsentAttestation{}, false
	}
	if d.sessionShimIncarnationHasTombstone(registry, key) {
		return SessionShimAbsentAttestation{}, false
	}
	if retained, ok := d.retainedSessionShimAbsence(key); ok {
		// Already proved in this daemon's life; the sidecar holds the durable
		// copy. Absence does not become presence.
		return retained.attestation(), true
	}
	if sidecar, ok, err := registry.GetWithdrawnAbsence(key.identity, key.shimID, key.processEpoch); err == nil && ok {
		// A previous incarnation of this daemon withdrew the record and never
		// got its report accepted. Re-prove from the sidecar rather than
		// trusting a file: the process may have been replaced since.
		return d.proveWithdrawnSessionShimAbsence(registry, key, sidecar)
	}
	record, ok := sessionShimLiveRecord(registry, key)
	if !ok {
		return SessionShimAbsentAttestation{}, false
	}
	if !d.observeSessionShimAbsence(key, record, func() (sessionshim.Record, bool) {
		return sessionShimLiveRecord(registry, key)
	}) {
		return SessionShimAbsentAttestation{}, false
	}
	// LAST, and immediately before the withdrawal. A shim publishes its
	// tombstone before it spends its courtesy waits, so a process that is gone
	// has already written one if its finalizer ran at all — which makes this
	// read the load-bearing one and the read at the top merely an optimisation.
	if d.sessionShimIncarnationHasTombstone(registry, key) {
		return SessionShimAbsentAttestation{}, false
	}
	withdrawn, err := registry.WithdrawIncarnationForAbsence(key.identity, key.shimID, key.processEpoch)
	if err != nil {
		slog.Warn("session shim: could not withdraw the discovery record of a vanished shim",
			"session", key.identity.String(), "shim", key.shimID, "error", err)
		return SessionShimAbsentAttestation{}, false
	}
	if !withdrawn {
		return SessionShimAbsentAttestation{}, false
	}
	return d.retainProvenSessionShimAbsence(registry, key, record)
}

// proveWithdrawnSessionShimAbsence re-derives the attestation for an
// incarnation whose record a previous daemon already withdrew.
//
// The sidecar is evidence, not authority: this re-runs the same two-reading
// probe against the identity it records, because a daemon that has been down
// long enough for a pid to be reused must not discharge on the strength of a
// file it wrote before the restart.
func (d *Daemon) proveWithdrawnSessionShimAbsence(
	registry *sessionshim.Registry,
	key shimIncarnation,
	sidecar sessionshim.Record,
) (SessionShimAbsentAttestation, bool) {
	if !d.observeSessionShimAbsence(key, sidecar, func() (sessionshim.Record, bool) {
		rec, ok, err := registry.GetWithdrawnAbsence(key.identity, key.shimID, key.processEpoch)
		return rec, err == nil && ok
	}) {
		return SessionShimAbsentAttestation{}, false
	}
	if d.sessionShimIncarnationHasTombstone(registry, key) {
		return SessionShimAbsentAttestation{}, false
	}
	// Finish an interrupted withdrawal. The rename is publish-then-unlink, so a
	// crash between the two leaves the sidecar BESIDE its live record — the
	// recoverable direction, and the reason that order was chosen. But the
	// record still being there means the attestation's second fact is not true
	// yet, and retainProvenSessionShimAbsence would refuse forever on a lineage
	// nothing else can resolve. Re-running the withdrawal is idempotent and
	// completes exactly what the crash interrupted.
	if _, err := registry.WithdrawIncarnationForAbsence(key.identity, key.shimID, key.processEpoch); err != nil {
		slog.Warn("session shim: could not complete an interrupted record withdrawal",
			"session", key.identity.String(), "shim", key.shimID, "error", err)
		return SessionShimAbsentAttestation{}, false
	}
	return d.retainProvenSessionShimAbsence(registry, key, sidecar)
}

// retainProvenSessionShimAbsence confirms the record really is gone and mints
// the observation instant.
func (d *Daemon) retainProvenSessionShimAbsence(
	registry *sessionshim.Registry,
	key shimIncarnation,
	record sessionshim.Record,
) (SessionShimAbsentAttestation, bool) {
	present, err := registry.HasIncarnation(key.identity, key.shimID, key.processEpoch)
	if err != nil || present {
		// Say only what was actually checked. A withdrawal that did not take is
		// a record still on disk, and an attestation claiming otherwise is the
		// one thing this type must never be.
		slog.Warn("session shim: discovery record survived its withdrawal; not attesting absence",
			"session", key.identity.String(), "shim", key.shimID, "error", err)
		return SessionShimAbsentAttestation{}, false
	}
	observedAt := d.shimNow().UnixNano()
	if observedAt <= 0 {
		// Complete() requires a positive observation instant. An injected clock
		// answering with the zero time would otherwise compose an attestation
		// the report refuses, turning a clock seam into a silent discharge hole.
		observedAt = time.Now().UnixNano()
	}
	proof := sessionShimAbsenceProof{
		process:    sessionshim.ProcessIdentity{PID: record.PID, StartedAt: record.ProcessStartedAt},
		observedAt: observedAt,
	}
	d.retainSessionShimAbsence(key, proof)
	return proof.attestation(), true
}

// sessionShimIncarnationHasTombstone is the mutual-exclusion read.
//
// ANY tombstone blocks the attestation, not just a group-reaped one. A shim
// that observed its own ending is not a shim this daemon cannot observe, and a
// weak tombstone is the reconcile's business (it stays quarantined until the
// reap is proven) rather than something to downgrade into an abandoned
// obligation that holds the release predicate forever.
func (d *Daemon) sessionShimIncarnationHasTombstone(
	registry *sessionshim.Registry,
	key shimIncarnation,
) bool {
	_, err := registry.GetTombstoneIncarnation(key.identity, key.shimID, key.processEpoch)
	return err == nil
}

// attestAbsentSessionShim composes and submits the attestation for ONE exact
// incarnation and records the outcome. It reports whether the composer durably
// accepted the discharge, and separately whether that scope's complete
// projection was republished — a caller about to publish the same scope for its
// own reason must not pay for a second revision.
//
// The claim is taken FIRST, before any disk work: see the file header.
func (d *Daemon) attestAbsentSessionShim(
	ctx context.Context,
	registry *sessionshim.Registry,
	key shimIncarnation,
	origin sessionShimAbsenceOrigin,
) (accepted, republished bool) {
	if d.shims == nil || registry == nil {
		return false, false
	}
	own, _ := d.claimSessionShimTerminalReport(key, time.Now())
	if !own {
		return false, false
	}
	committed := false
	forget := false
	// The release runs BEFORE the forget, and the order is not cosmetic:
	// releaseSessionShimTerminalReport re-reads the mark to close its in-flight
	// channel, so forgetting first would hand it a zero value — leaving any
	// waiter parked on a channel nobody closes, and re-inserting the entry the
	// forget was there to remove. It is a defer keyed on flags, not calls at
	// the exits, for the reason handOffQuarantinedTerminalProof spells out:
	// this runs on handler goroutines where net/http RECOVERS a panic raised
	// inside a downstream callback, and an explicit release below a panicking
	// hook is simply never reached.
	defer func() {
		d.releaseSessionShimTerminalReport(key, committed, time.Now())
		if forget {
			d.forgetSessionShimTerminalReport(key)
			d.forgetSessionShimAbsence(key)
		}
	}()

	attestation, provable := d.proveSessionShimAbsence(registry, key)
	if !provable {
		return false, false
	}
	hostID, err := d.sessionShimHostID(ctx, key.identity.OrgID)
	if err != nil {
		slog.Warn("session shim: retain an unobservable lineage after host identity resolution failed",
			"session", key.identity.String(), "origin", string(origin), "error", err)
		return false, false
	}
	// NO adoption correlation and NO tombstone. The obligation this discharges
	// is quarantined-kind — lifecycle identity plus shim id and process epoch —
	// and an adoption receipt would ask the receiver for the adopted-kind
	// predicate instead, which matches nothing once the lineage was reported
	// quarantined. The same reasoning is spelled out at handOffQuarantined-
	// TerminalProof, where attaching one cost an installed host its recovery.
	if err := d.reportSessionShimTerminalEvidence(ctx, SessionShimTerminalEvidence{
		Identity:     key.identity,
		HostID:       hostID,
		ShimID:       key.shimID,
		ProcessEpoch: key.processEpoch,
		Absent:       &attestation,
	}); err != nil {
		slog.Warn("session shim: retain an unobservable lineage after its absent attestation was refused",
			"session", key.identity.String(), "origin", string(origin), "error", err)
		return false, false
	}
	committed = true
	slog.Info("session shim: attested a lineage this host can no longer observe; its recovery obligation is abandoned, not resolved",
		"session", key.identity.String(), "shim", key.shimID,
		"processEpoch", key.processEpoch, "origin", string(origin))

	// ONLY NOW. The composer has converted the obligation to abandoned and
	// removed the lineage from its completeness set, so the row may leave this
	// daemon's quarantine projection — and the two sets must change in that
	// order, or a projection built mid-flight omits a lineage the receiver
	// still holds active and the whole batch is refused.
	republished = d.withdrawAttestedSessionShimLineage(key)
	if republished {
		// The daemon's quarantine set just changed and the receiver's did not.
		// The receiver prunes its own snapshot for ordinary terminal evidence
		// but NOT for an absent attestation on this seam, so this republish is
		// the only thing that reconciles the two: without it the next beat
		// presents a set the last committed batch disagrees with, is refused
		// revision-stale, and drains the host on every beat.
		d.publishSessionShimProjection(ctx, key.identity.OrgID)
	}
	// The sidecar is the last artifact that can re-derive this fact, so it goes
	// only here — after a durable acceptance — exactly where the tombstone path
	// disposes its own proof. With it gone nothing can rediscover the
	// incarnation, so neither the retained proof nor the handoff mark has a
	// reader left.
	if err := registry.DisposeWithdrawnAbsence(key.identity, key.shimID, key.processEpoch); err != nil {
		slog.Warn("session shim: dispose the withdrawn record after a durable absent attestation",
			"session", key.identity.String(), "error", err)
		return true, republished
	}
	forget = true
	return true, republished
}

// withdrawAttestedSessionShimLineage moves one exact incarnation out of the
// quarantine projection as ONE locked transition, AFTER its attestation is
// durably accepted. It reports false when the row was not there to remove.
//
// Nothing is appended to the terminal set. That set is the daemon's record of
// lineages with positive reap proof, and an attestation is explicitly not one:
// SessionShimTerminalProof reads it to answer the release predicate, and an
// entry there would turn "unobservable" into "proven dead" at exactly the seam
// §D10 forbids it.
func (d *Daemon) withdrawAttestedSessionShimLineage(key shimIncarnation) bool {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	removed := false
	remainingForIdentity := false
	kept := d.shims.quarantined[:0]
	for _, current := range d.shims.quarantined {
		if current.Identity() == key.identity && current.ShimID == key.shimID &&
			current.ProcessEpoch == key.processEpoch {
			removed = true
			continue
		}
		if current.Identity() == key.identity {
			remainingForIdentity = true
		}
		kept = append(kept, current)
	}
	d.shims.quarantined = kept
	// The correlation goes whether or not the row was there. A retained
	// adoption correlation for a lineage the composer has already discharged is
	// what makes a later batch attach an adopted-kind receipt to a lineage the
	// receiver knows only as quarantined.
	delete(d.shims.correlations, key)
	if !removed {
		return false
	}
	// forwarded is keyed by LIFECYCLE IDENTITY, and one identity can hold a
	// discharged lineage beside a live adopted one (§D7's duplicate-identity
	// case). Dropping the durable high-water because a SIBLING incarnation was
	// discharged would regress the surviving session's fence correlation.
	if _, stillAdopted := d.shims.adopted[key.identity]; !stillAdopted && !remainingForIdentity {
		delete(d.shims.forwarded, key.identity)
	}
	return true
}

// attestAbsentQuarantinedIncarnation is the periodic sweep's per-lineage entry
// point: the reconcile already walks the quarantine projection and has already
// established that this incarnation has no group-reaped tombstone to hand over.
// It reports whether the scope's complete projection was republished, so the
// caller can avoid publishing the same scope twice.
func (d *Daemon) attestAbsentQuarantinedIncarnation(
	registry *sessionshim.Registry,
	key shimIncarnation,
) bool {
	_, republished := d.attestAbsentSessionShim(
		context.Background(), registry, key, absenceOriginQuarantineSweep)
	return republished
}

// attestAbsentStaleSessionShims discharges the startup pass's stale records and
// returns, in the input's order, the ones it could not.
//
// A stale record IS the §D10 case by construction — sessionshim.Adopt files a
// record here only after proving its process identity is not running and
// finding no tombstone — so this is the seam where the attestation is owed
// first. Discharging before the batch is composed is what lets that batch
// legitimately omit the lineage: the composer has already converted the
// obligation to abandoned and dropped it from its completeness set.
//
// A record that cannot be discharged is HANDED BACK rather than dropped. The
// caller still owes the composer a complete snapshot, and "this daemon could
// not reach the control plane" is not a reason to omit a lineage; it is a
// reason to keep declaring it.
//
// Nothing here fails the boot. Every refusal is one lineage's, the rest of the
// host composes normally, and the sweep retries on the next occupancy or
// heartbeat surface.
func (d *Daemon) attestAbsentStaleSessionShims(
	ctx context.Context,
	registry *sessionshim.Registry,
	stale []sessionshim.Record,
) []sessionshim.Record {
	if len(stale) == 0 || d.shims == nil {
		return stale
	}
	remaining := make([]sessionshim.Record, 0, len(stale))
	for _, rec := range stale {
		key := shimIncarnation{
			identity: rec.Identity(), shimID: rec.ShimID, processEpoch: rec.ProcessEpoch,
		}
		if accepted, _ := d.attestAbsentSessionShim(
			ctx, registry, key, absenceOriginStartupAdoption); !accepted {
			remaining = append(remaining, rec)
		}
	}
	return remaining
}

// resubmitWithdrawnSessionShimAbsences re-reports every discharge a previous
// incarnation of this daemon withdrew but never got accepted.
//
// This is the half of the mechanism that makes the withdrawal survivable. A
// composer that was unreachable for the whole of the last daemon's life leaves
// one sidecar per lineage and no memory of any of them; without this pass the
// composer keeps an active obligation for a correlation nothing on the host can
// name any more, and every later complete batch is refused for omitting it.
//
// It runs on the startup path beside the stale set, before the first batch is
// composed, so a lineage discharged here is legitimately absent from that
// batch. Nothing here fails the boot.
func (d *Daemon) resubmitWithdrawnSessionShimAbsences(ctx context.Context, registry *sessionshim.Registry) int {
	if d.shims == nil || registry == nil {
		return 0
	}
	withdrawn, err := registry.ScanWithdrawnAbsences()
	if err != nil {
		slog.Warn("session shim: could not scan withdrawn absence records", "error", err)
		return 0
	}
	resubmitted := 0
	for _, rec := range withdrawn {
		key := shimIncarnation{
			identity: rec.Identity(), shimID: rec.ShimID, processEpoch: rec.ProcessEpoch,
		}
		if accepted, _ := d.attestAbsentSessionShim(
			ctx, registry, key, absenceOriginWithdrawnResubmit); accepted {
			resubmitted++
		}
	}
	return resubmitted
}
