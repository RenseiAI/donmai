package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// AdoptOptions configure a startup adoption pass.
type AdoptOptions struct {
	// Registry is the discovery surface to scan. Required.
	Registry *Registry

	// ControllerID identifies this daemon in diagnostics.
	ControllerID string

	// NextGeneration proposes the generation for one adoption. It receives the
	// generation the shim currently holds and MUST return a strictly greater
	// value. Nil uses current+1, which is the ordinary path.
	NextGeneration func(id Identity, current shimwire.Generation) shimwire.Generation

	// Prepare is the optional per-identity composing hook run before the daemon
	// sends Welcome and therefore before it acquires controller authority. It is
	// where an embedder resolves a fresh carrier generation or any other generic
	// Welcome extension that must be fixed for this adoption. A failure aborts
	// the WHOLE startup pass: silently quarantining one otherwise-compatible shim
	// would let the daemon advertise ready without rehydrating that session's
	// external carrier (ADR-2026-08-17 §D4).
	Prepare func(ctx context.Context, evidence AdoptionPreparation) (PreparedAdoption, error)

	// ResumeFrom returns the first sequence this daemon still needs for a
	// session — its durable last_forwarded_seq + 1. Nil uses this shim
	// incarnation's fsync-backed ACK sidecar + 1 when present, otherwise the
	// start of the stream. A supplied callback may advance beyond the sidecar but
	// cannot regress it silently.
	ResumeFrom func(id Identity) uint64

	// ExpectedWorkarea returns the workarea this daemon believes a session
	// belongs to. A non-empty mismatch quarantines rather than adopts. Nil skips
	// the daemon-side half of the workarea check; the record-vs-shim half always
	// runs.
	ExpectedWorkarea func(id Identity) string
	// ExpectedWorkareaRoot optionally cross-checks the additive secret-free
	// discovery record field. An old record without it remains adoptable.
	ExpectedWorkareaRoot func(id Identity) string
	// ExpectedWorkareaLayout is the fail-closed resolver used by root-aware
	// adoption. When set it supersedes the two legacy string-only callbacks.
	ExpectedWorkareaLayout func(id Identity) (workareaPath, workareaRoot string, err error)

	// Filter, when set, restricts the pass to the identities it accepts. The
	// scan, the tombstone read, and duplicate detection still run over the
	// WHOLE registry — "two live records claim one identity" is only visible
	// in aggregate — but no other record is dialled, classified, or reported.
	// It is how a daemon re-adopts ONE live shim through exactly the pipeline
	// the startup pass runs, rather than through a second adoption path.
	Filter func(id Identity) bool

	// EventBacklogBudget overrides the per-controller event backlog budget, in
	// payload bytes. Zero uses EventBacklogBudget.
	EventBacklogBudget int
	// EventBacklogStallDeadline overrides how long a controller's consumer may go
	// without taking a whole budget's worth of bytes before the controller fails
	// closed. Zero uses the sessionshim default. Values below the package floor
	// (which sits above the durable-heartbeat receipt wait bound) are raised to
	// it rather than honoured — see ControllerOptions.EventBacklogStallDeadline.
	EventBacklogStallDeadline time.Duration
	// DurableAckAmbiguityBound overrides how long a stalled reader is held open
	// while a durable acknowledgement is outstanding. Zero uses the sessionshim
	// default, which is the WRONG answer for any composition that configures a
	// re-adoption window: ADR-2026-09-03 makes this bound and the lineage-live
	// re-adoption window ONE configured value, so a composing daemon must set
	// this from its resolved policy rather than letting two defaults agree by
	// coincidence. See ControllerOptions.DurableAckAmbiguityBound.
	DurableAckAmbiguityBound time.Duration
	// EventBacklogDropBound overrides how long a stalled reader is held after
	// the stall deadline before the carrier is dropped. Zero uses
	// EventBacklogDropBound; a value under the resolved stall deadline is
	// clamped up rather than honoured. See ControllerOptions.EventBacklogDropBound.
	EventBacklogDropBound time.Duration

	// DialTimeout bounds one shim handshake.
	DialTimeout time.Duration
	// ProtocolMin/ProtocolMax optionally narrow the adopting controller range.
	ProtocolMin uint32
	ProtocolMax uint32
	// RequireFullHostFrames explicitly opts every newly adopted controller into
	// selected-v3 raw HostFrame consumption when the peer also supports v3.
	RequireFullHostFrames bool

	Logger *slog.Logger
	Now    func() time.Time
}

// AdoptionPreparation is the exact authenticated Hello correlation available
// before Welcome proposes authority. A composing callback must use this shape
// rather than looking up a shim by session identity and guessing which
// incarnation it is preparing.
type AdoptionPreparation struct {
	Identity                    Identity
	ControllerID                string
	ShimID                      string
	ProcessEpoch                uint64
	CurrentControllerGeneration shimwire.Generation
	// LocalResumeFrom is the normalized successor of the selected-v3 shim ACK
	// sidecar, or 1 when no sidecar exists. It is local comparison evidence,
	// never an external carrier cursor.
	LocalResumeFrom uint64
	// LastHostSeq is the authenticated Hello.LastSeq frozen by selected-v3
	// adoption preparation.
	LastHostSeq uint64
	// LastForwardedSeq is retained as a deprecated source-compatible alias for
	// LocalResumeFrom-1. It is not durable-carrier proof authority.
	LastForwardedSeq uint64
	SelectedVersion  uint32
}

// PreparedAdoption is the per-session portion of Welcome supplied by a
// composing daemon. Both fields are optional; the zero value preserves the
// standalone controller's current+1 generation and extension-free handshake.
//
// ControllerGeneration is resolved only after the verified Hello exposes the
// shim's authoritative current generation. Zero preserves current+1. This
// deliberately provides no scalar host generation: session-shim-v1 owns one
// controller generation per shim.
type PreparedAdoption struct {
	ControllerGeneration shimwire.Generation
	Extensions           shimwire.Extensions
	// Correlation is opaque composing state prepared against the exact Hello
	// above (for example a fence revision and expected adoption revision). Donmai
	// never parses it; the daemon hands the bytes unchanged to OnAdoption.
	Correlation []byte
	// ResumeFrom is the proof-resolved first requested sequence. Nil preserves
	// the local/standalone floor. A non-nil value may raise but never regress
	// LocalResumeFrom and may not exceed authenticated LastHostSeq+1.
	ResumeFrom *uint64
}

// ErrAdoptionPreparation reports a composing dependency that failed before
// controller authority was proposed. Callers fail startup closed rather than
// converting it into an ordinary compatibility quarantine.
var ErrAdoptionPreparation = errors.New("sessionshim: adoption preparation failed")

// PreparedAdoptionBounds is the exact local evidence one preparation answer is
// validated against: what the caller has already configured statically, the
// local acknowledgement floor, and the authenticated Hello cursor.
type PreparedAdoptionBounds struct {
	// StaticGenerationConfigured reports that the caller already fixed the
	// proposed generation, so a prepared one would be a second, conflicting
	// authority rather than the answer.
	StaticGenerationConfigured bool
	// StaticResumeConfigured reports the same for the resume cursor.
	StaticResumeConfigured bool
	// LocalResumeFrom is the normalized local floor. A prepared cursor may raise
	// it and may never regress it.
	LocalResumeFrom uint64
	// HelloLastSeq is the authenticated Hello cursor. A prepared cursor may not
	// exceed its successor, and an unset (all-ones) Hello cursor cannot bound
	// anything at all.
	HelloLastSeq uint64
}

// ResolvedPreparedAdoption is what a validated preparation answer resolves to.
// ResumeFrom is meaningful only when ResumeProvided is set: zero is a real
// cursor, not an absence.
type ResolvedPreparedAdoption struct {
	ControllerGeneration shimwire.Generation
	Extensions           shimwire.Extensions
	ResumeFrom           uint64
	ResumeProvided       bool
}

// ResolvePreparedAdoption validates one preparation answer against the bounds
// it must hold inside, and returns the generation, extensions, and cursor the
// adoption will actually use.
//
// It exists as a function rather than a stretch of handshake because a
// preparation answer can now arrive AFTER the handshake — a composing daemon
// that re-prepares a drifted carrier proof gets a second answer with no Welcome
// left to spend it on. An answer that reached the wire through one set of checks
// and an answer that reached a durable receipt through none is how a raised
// floor gets silently dropped, or a regressed one silently honoured. Both paths
// call this, so neither can take a route the other does not.
func ResolvePreparedAdoption(prepared PreparedAdoption, bounds PreparedAdoptionBounds) (ResolvedPreparedAdoption, error) {
	resolved := ResolvedPreparedAdoption{Extensions: prepared.Extensions}
	if prepared.ControllerGeneration != 0 {
		if bounds.StaticGenerationConfigured {
			return ResolvedPreparedAdoption{},
				fmt.Errorf("%w: prepared and static controller generations are both configured", ErrAdoptionPreparation)
		}
		resolved.ControllerGeneration = prepared.ControllerGeneration
	}
	if prepared.ResumeFrom == nil {
		return resolved, nil
	}
	if bounds.StaticResumeConfigured {
		return ResolvedPreparedAdoption{},
			fmt.Errorf("%w: static and proof-resolved resume cursors are both configured", ErrAdoptionPreparation)
	}
	cursor := *prepared.ResumeFrom
	if cursor < bounds.LocalResumeFrom {
		return ResolvedPreparedAdoption{}, fmt.Errorf("%w: prepared resume %d regresses local floor %d",
			ErrAdoptionPreparation, cursor, bounds.LocalResumeFrom)
	}
	if bounds.HelloLastSeq == ^uint64(0) || cursor > bounds.HelloLastSeq+1 {
		return ResolvedPreparedAdoption{}, fmt.Errorf("%w: prepared resume %d is ahead of Hello LastSeq %d",
			ErrAdoptionPreparation, cursor, bounds.HelloLastSeq)
	}
	resolved.ResumeFrom, resolved.ResumeProvided = cursor, true
	return resolved, nil
}

func (o AdoptOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o AdoptOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// AdoptionResult is the complete outcome of one startup pass.
//
// Every survivor lands in EXACTLY one bucket. That totality is the point: §D4
// requires a daemon to account for every registry entry before advertising, and
// a result shape that let an entry fall through none of these would make
// "accounted for" unverifiable.
type AdoptionResult struct {
	// Adopted are live, compatible shims now under this daemon's control.
	Adopted []*Controller
	// Quarantined are survivors refused authority but NOT killed, and counted
	// against capacity.
	Quarantined []QuarantinedSession
	// Tombstoned are sessions whose shim already reaped its harness and left
	// durable proof. These do not consume capacity — the workload is over — but
	// they still require a terminal report before any claim is released.
	Tombstoned []Tombstone
	// Stale are records whose process is gone with no tombstone. Nothing is
	// signalled for these: a PID whose start identity no longer matches is a
	// DIFFERENT process, and §D10 forbids signalling a reused pid.
	Stale []Record
}

type terminalIncarnation struct {
	identity     Identity
	shimID       string
	processEpoch uint64
}

func terminalIncarnationForTombstone(t Tombstone) terminalIncarnation {
	return terminalIncarnation{identity: t.Identity(), shimID: t.ShimID, processEpoch: t.ProcessEpoch}
}

func terminalIncarnationForRecord(r Record) terminalIncarnation {
	return terminalIncarnation{identity: r.Identity(), shimID: r.ShimID, processEpoch: r.ProcessEpoch}
}

// OccupiedSlots is the capacity a host must subtract before advertising.
//
// Quarantined shims count. That is the whole of §D7's second half: their
// harnesses are still running, so treating them as free capacity would advertise
// slots that are physically occupied and let the host claim work it cannot run.
// Tombstoned and stale entries do not count — their workloads are over.
func (r AdoptionResult) OccupiedSlots() int { return len(r.Adopted) + len(r.Quarantined) }

// QuarantinedProjection returns the bounded, deterministically ordered
// projection for host status and heartbeat payloads (§D7).
func (r AdoptionResult) QuarantinedProjection() []QuarantinedSession {
	out := append([]QuarantinedSession(nil), r.Quarantined...)
	SortQuarantined(out)
	return out
}

// Close drops every adopted controller without stopping any session.
func (r AdoptionResult) Close() {
	for _, c := range r.Adopted {
		_ = c.Close()
	}
}

// Adopt performs the §D4 startup pass: scan, classify, adopt, quarantine.
//
// The caller MUST run this to completion before advertising ready capacity or
// claiming new work. The ordering is not an optimisation — a daemon that
// advertised first would claim against slots already occupied by shims it has
// not yet discovered.
//
// Adopt never kills anything. Refusal is always quarantine (§D7), which is what
// makes an incompatible-protocol upgrade non-destructive.
func Adopt(ctx context.Context, opts AdoptOptions) (AdoptionResult, error) {
	var result AdoptionResult
	if opts.Registry == nil {
		return result, errors.New("sessionshim: Adopt requires a Registry")
	}
	if !peerCredSupported() {
		// §D3: adoption stays disabled rather than running unauthenticated. An
		// empty result with no error means "nothing adopted, nothing occupied",
		// which is the honest answer on a platform that cannot verify a peer.
		return result, ErrShimUnsupported
	}
	log := opts.logger()
	now := opts.now()

	entries, err := opts.Registry.Scan()
	if err != nil {
		return result, err
	}

	tombstones, err := opts.Registry.ScanTombstones()
	if err != nil {
		return result, err
	}
	tombstoneByIncarnation := make(map[terminalIncarnation]Tombstone, len(tombstones))
	for _, t := range tombstones {
		if opts.Filter != nil && !opts.Filter(t.Identity()) {
			continue
		}
		if t.GroupReaped {
			tombstoneByIncarnation[terminalIncarnationForTombstone(t)] = t
			result.Tombstoned = append(result.Tombstoned, t)
			continue
		}
		// A tombstone is terminal observation, but only GroupReaped is positive
		// proof that capacity disappeared. Keep the exact shim/process incarnation
		// visible and charged rather than treating a failed OS probe as free space.
		result.Quarantined = append(result.Quarantined, QuarantinedSession{
			OrgID:            t.OrgID,
			SessionID:        t.SessionID,
			ShimID:           t.ShimID,
			ProcessEpoch:     t.ProcessEpoch,
			Reason:           QuarantineGroupReapUnproven,
			Detail:           "terminal tombstone did not prove harness process-group reap",
			ConsumesCapacity: true,
		})
	}

	// Duplicate detection runs over the WHOLE scan before any adoption, because
	// "two live records claim one identity" is only visible in aggregate — and
	// adopting the first one seen would be exactly the guess §D7 forbids.
	seen := make(map[Identity][]Record)
	for _, e := range entries {
		if e.Err != nil {
			continue
		}
		if _, terminal := tombstoneByIncarnation[terminalIncarnationForRecord(e.Record)]; terminal {
			// A crash may leave the exact live record beside its already-published
			// positive tombstone. It is not a live duplicate and must not make a
			// different surviving incarnation under the same lifecycle identity look
			// ambiguous.
			continue
		}
		id := e.Record.Identity()
		seen[id] = append(seen[id], e.Record)
	}

	for _, e := range entries {
		if e.Err != nil {
			if opts.Filter != nil {
				// A filtered pass is asking about ONE identity. An undecodable
				// entry has none to match, and reporting it here would let a
				// single-lineage re-adoption re-quarantine every malformed
				// entry on the host as a side effect.
				continue
			}
			// A record we cannot even decode still occupies a slot: something is
			// running out there. Quarantine with whatever identity we have.
			result.Quarantined = append(result.Quarantined, QuarantinedSession{
				Reason:           QuarantineRecordMalformed,
				Detail:           fmt.Sprintf("registry entry %s: %v", e.Name, e.Err),
				ConsumesCapacity: true,
			})
			log.Warn("sessionshim: quarantined malformed registry entry", "entry", e.Name, "error", e.Err)
			continue
		}
		rec := e.Record
		id := rec.Identity()
		if opts.Filter != nil && !opts.Filter(id) {
			continue
		}

		if _, ok := tombstoneByIncarnation[terminalIncarnationForRecord(rec)]; ok {
			// The shim already proved its outcome. The live record is a crash
			// artifact from between tombstone publication and record removal.
			continue
		}

		if len(seen[id]) > 1 {
			result.Quarantined = append(result.Quarantined,
				NewQuarantinedSession(rec, QuarantineDuplicateIdentity,
					fmt.Sprintf("%d live records claim this identity", len(seen[id])), now))
			log.Warn("sessionshim: quarantined duplicate session identity",
				"session", id.String(), "records", len(seen[id]))
			continue
		}

		alive, aliveErr := ProcessIdentity{PID: rec.PID, StartedAt: rec.ProcessStartedAt}.Alive()
		if aliveErr != nil {
			result.Quarantined = append(result.Quarantined,
				NewQuarantinedSession(rec, QuarantineIdentityMismatch,
					fmt.Sprintf("process identity unverifiable: %v", aliveErr), now))
			continue
		}
		if !alive {
			// Gone, with no tombstone: STALE. Retain the record as diagnostic
			// evidence and signal nothing — the pid may belong to someone else now.
			result.Stale = append(result.Stale, rec)
			log.Info("sessionshim: registry record is stale (process gone, no tombstone)",
				"session", id.String(), "pid", rec.PID)
			continue
		}

		if !rec.Phase.Known() {
			result.Quarantined = append(result.Quarantined,
				NewQuarantinedSession(rec, QuarantinePhaseUnknown,
					fmt.Sprintf("record reports phase %q", rec.Phase), now))
			continue
		}

		// Range overlap is checked from the RECORD before dialling, so a plainly
		// incompatible shim is classified without a connection attempt. The live
		// handshake re-checks against what the shim actually says.
		if _, negErr := shimwire.Negotiate(rec.ProtocolMin, rec.ProtocolMax, shimwire.ProtocolMin, shimwire.ProtocolMax); negErr != nil {
			result.Quarantined = append(result.Quarantined,
				NewQuarantinedSession(rec, QuarantineProtocolMismatch,
					fmt.Sprintf("shim speaks [%d,%d], this daemon speaks [%d,%d]",
						rec.ProtocolMin, rec.ProtocolMax, shimwire.ProtocolMin, shimwire.ProtocolMax), now))
			log.Warn("sessionshim: quarantined protocol-incompatible shim (not killed)",
				"session", id.String(), "shimRange", fmt.Sprintf("[%d,%d]", rec.ProtocolMin, rec.ProtocolMax))
			continue
		}

		ctrl, adoptErr := dialForAdoptionWithRetry(ctx, rec, opts, log)
		if adoptErr != nil {
			if errors.Is(adoptErr, ErrAdoptionPreparation) {
				result.Close()
				return result, adoptErr
			}
			reason, detail := classifyAdoptionFailure(adoptErr)
			quarantined := NewQuarantinedSession(rec, reason, detail, now)
			if generation, ok := authenticatedHelloGeneration(adoptErr); ok {
				quarantined.ControllerGeneration = uint64(generation)
			}
			result.Quarantined = append(result.Quarantined, quarantined)
			log.Warn("sessionshim: quarantined shim after failed adoption (not killed)",
				"session", id.String(), "reason", reason, "error", adoptErr)
			continue
		}
		result.Adopted = append(result.Adopted, ctrl)
		log.Info("sessionshim: adopted live shim",
			"session", id.String(), "shim", rec.ShimID,
			"generation", uint64(ctrl.Generation()), "contiguous", ctrl.Adoption().Contiguous)
	}

	sortResult(&result)
	return result, nil
}

const (
	// adoptionDialAttempts is the TOTAL number of dials one live record gets
	// when every failure is transient. It bounds ONE record, not the pass —
	// see adoptionRetryDialTimeout for what that leaves unbounded.
	adoptionDialAttempts = 3
	// adoptionDialBackoff is the base delay between transient attempts; it
	// doubles. The delay is short on purpose — the dial timeout is the real
	// spacing, and this only avoids hammering a peer that just refused.
	adoptionDialBackoff = 100 * time.Millisecond
	// adoptionRetryDialTimeout caps attempts 2 and 3, because Adopt walks
	// records in ONE sequential loop and every later lineage waits behind the
	// current one. At the 5s default a retried hang would cost 15s of the pass
	// instead of 5s, and hung records ahead of a healthy one push that healthy
	// lineage's adoption toward its own orphan deadline — turning stalled shims
	// into other sessions' self-teardown, which is the exact harm per-lineage
	// quarantine exists to prevent. One record's worst case is bounded at
	// first + 2s + 2s (5+2+2 = 9s at the default dial timeout), and a caller
	// that configures a shorter DialTimeout keeps it: this only ever lowers.
	//
	// Be honest about what that does NOT bound. Only the RECORD is bounded; the
	// pass is not. N hung records still cost 9N seconds serially — against a 90s
	// shim orphan deadline that is roughly ten of them, where before this cap it
	// was roughly six. The exposure predates this change (5s × N crossed the
	// same deadline at around eighteen records) and the cap reduces it, but
	// nothing here enforces an aggregate budget. A pass-level budget, or dialing
	// independent lineages concurrently, is a real change with its own ordering
	// and capacity consequences and belongs in its own ADR — not in a constant.
	adoptionRetryDialTimeout = 2 * time.Second
)

// adoptionAttemptDialTimeout is the dial timeout for one attempt: the caller's
// own for the first, and the retry cap for every attempt after it. Attempt 1 is
// deliberately unchanged — a first dial is not evidence of anything yet, and
// shortening it would convert slow-but-healthy shims into retries.
func adoptionAttemptDialTimeout(configured time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return configured
	}
	if configured > 0 && configured < adoptionRetryDialTimeout {
		return configured
	}
	return adoptionRetryDialTimeout
}

// dialForAdoptionWithRetry dials one record, retrying while the failure says
// only "not this time".
//
// A preparation failure is never retried: it aborts the whole pass, and asking
// a composing authority again for something it already refused is not recovery.
// Everything else non-transient returns on the first answer, exactly as before.
//
// A retry re-dials the ALREADY-PREPARED candidate. Preparation runs inside the
// handshake, after Hello authentication and before the Welcome write, so the
// measured failure shape this retry exists for — a write timeout on an
// established socket — happens AFTER preparation has already succeeded. Asking
// again would mint a second control-plane reservation for a lineage whose first
// one is admitted and undisposed, on a path that has no abandonment verb and no
// drift to repair. The first answer is retained and replayed instead, so a hung
// lineage costs extra dials and never extra reservations.
func dialForAdoptionWithRetry(
	ctx context.Context,
	rec Record,
	opts AdoptOptions,
	log *slog.Logger,
) (*Controller, error) {
	configuredTimeout := opts.DialTimeout
	attemptOpts := opts
	if opts.Prepare != nil {
		prepare := opts.Prepare
		var retained PreparedAdoption
		var retainedOK bool
		attemptOpts.Prepare = func(prepareCtx context.Context, evidence AdoptionPreparation) (PreparedAdoption, error) {
			if retainedOK {
				return clonePreparedAdoption(retained), nil
			}
			prepared, err := prepare(prepareCtx, evidence)
			if err != nil {
				return PreparedAdoption{}, err
			}
			retained, retainedOK = clonePreparedAdoption(prepared), true
			return prepared, nil
		}
	}
	attemptOpts.DialTimeout = adoptionAttemptDialTimeout(configuredTimeout, 1)
	ctrl, err := dialForAdoption(ctx, rec, attemptOpts)
	for attempt := 2; attempt <= adoptionDialAttempts; attempt++ {
		if !isTransientDialFailure(err) || errors.Is(err, ErrAdoptionPreparation) {
			return ctrl, err
		}
		log.Warn("sessionshim: adoption dial failed transiently; retrying the prepared candidate before classifying",
			"session", rec.Identity().String(), "attempt", attempt, "of", adoptionDialAttempts, "error", err)
		delay := adoptionDialBackoff
		for i := 2; i < attempt; i++ {
			delay *= 2
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		}
		timer.Stop()
		attemptOpts.DialTimeout = adoptionAttemptDialTimeout(configuredTimeout, attempt)
		ctrl, err = dialForAdoption(ctx, rec, attemptOpts)
	}
	return ctrl, err
}

// clonePreparedAdoption deep-copies a retained preparation answer so a replay
// cannot hand a later attempt an aliased map or slice the earlier one still
// holds.
func clonePreparedAdoption(in PreparedAdoption) PreparedAdoption {
	extensions := shimwire.Extensions{Required: append([]string(nil), in.Extensions.Required...)}
	if in.Extensions.Values != nil {
		extensions.Values = make(map[string]string, len(in.Extensions.Values))
		for name, value := range in.Extensions.Values {
			extensions.Values[name] = value
		}
	}
	in.Extensions = extensions
	in.Correlation = append([]byte(nil), in.Correlation...)
	if in.ResumeFrom != nil {
		cursor := *in.ResumeFrom
		in.ResumeFrom = &cursor
	}
	return in
}

// ResolvedDurableAckAmbiguityBound reports the bound a controller dialled by
// this adoption pass would actually hold for, after the default and the floor.
//
// It exists for the same reason ControllerOptions has one: a composing daemon
// must be able to assert that its configured re-adoption window REACHES the
// controllers it dials, on the struct its own production code built, rather
// than on a struct a test rebuilt by hand — the latter passes with the
// assignment deleted.
func (o AdoptOptions) ResolvedDurableAckAmbiguityBound() time.Duration {
	return ControllerOptions{
		DurableAckAmbiguityBound:  o.DurableAckAmbiguityBound,
		EventBacklogStallDeadline: o.EventBacklogStallDeadline,
	}.ResolvedDurableAckAmbiguityBound()
}

// ResolvedEventBacklogDropBound reports the drop bound a controller dialled by
// this adoption pass would actually hold for, for the same reason.
func (o AdoptOptions) ResolvedEventBacklogDropBound() time.Duration {
	return ControllerOptions{
		EventBacklogDropBound:     o.EventBacklogDropBound,
		EventBacklogStallDeadline: o.EventBacklogStallDeadline,
	}.ResolvedEventBacklogDropBound()
}

func dialForAdoption(ctx context.Context, rec Record, opts AdoptOptions) (*Controller, error) {
	id := rec.Identity()
	probe := ControllerOptions{
		ProtocolMin: opts.ProtocolMin, ProtocolMax: opts.ProtocolMax,
		RequireFullHostFrames: opts.RequireFullHostFrames,
	}
	localMin, localMax, rangeErr := probe.protocolRange()
	if rangeErr != nil {
		return nil, rangeErr
	}
	selected, selectErr := shimwire.Negotiate(rec.ProtocolMin, rec.ProtocolMax, localMin, localMax)
	if selectErr != nil {
		return nil, selectErr
	}
	localResumeFrom := uint64(1)
	var durableAckGeneration shimwire.Generation
	if selected >= shimwire.V3 {
		durableAck, ackErr := opts.Registry.getDurableAck(rec)
		hasDurableAck := ackErr == nil
		if ackErr != nil && !errors.Is(ackErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("sessionshim: load durable acknowledgement for %s: %w", id, ackErr)
		}
		if hasDurableAck {
			if durableAck.AckedSeq == ^uint64(0) {
				return nil, fmt.Errorf("sessionshim: durable acknowledgement for %s cannot advance past uint64 max", id)
			}
			localResumeFrom = durableAck.AckedSeq + 1
			durableAckGeneration = durableAck.ControllerGeneration
		}
	}
	resume := uint64(0)
	if opts.ResumeFrom != nil {
		resume = opts.ResumeFrom(id)
		effectiveResume := resume
		if effectiveResume == 0 {
			effectiveResume = 1
		}
		if selected >= shimwire.V3 && effectiveResume < localResumeFrom {
			return nil, fmt.Errorf("%w for %s: external resume %d regresses shim local floor %d",
				ErrAdoptionPreparation, id, effectiveResume, localResumeFrom)
		}
	} else if selected >= shimwire.V3 {
		resume = localResumeFrom
	}
	expected, expectedRoot := "", ""
	if opts.ExpectedWorkareaLayout != nil {
		var resolveErr error
		expected, expectedRoot, resolveErr = opts.ExpectedWorkareaLayout(id)
		if resolveErr != nil {
			return nil, fmt.Errorf("%w for %s: resolve expected workarea: %w", ErrAdoptionRefused, id, resolveErr)
		}
	} else {
		if opts.ExpectedWorkarea != nil {
			expected = opts.ExpectedWorkarea(id)
		}
		if opts.ExpectedWorkareaRoot != nil {
			expectedRoot = opts.ExpectedWorkareaRoot(id)
		}
	}

	copts := ControllerOptions{
		ControllerID:               opts.ControllerID,
		ResumeFrom:                 resume,
		LocalResumeFrom:            localResumeFrom,
		ResumeExternallyConfigured: opts.ResumeFrom != nil,
		DurableAckGeneration:       durableAckGeneration,
		EventBacklogBudget:         opts.EventBacklogBudget,
		EventBacklogStallDeadline:  opts.EventBacklogStallDeadline,
		DurableAckAmbiguityBound:   opts.DurableAckAmbiguityBound,
		EventBacklogDropBound:      opts.EventBacklogDropBound,
		ExpectedWorkarea:           expected,
		ExpectedWorkareaRoot:       expectedRoot,
		DialTimeout:                opts.DialTimeout,
		Logger:                     opts.Logger,
		ProtocolMin:                opts.ProtocolMin,
		ProtocolMax:                opts.ProtocolMax,
		RequireFullHostFrames:      opts.RequireFullHostFrames,
	}
	if opts.NextGeneration != nil {
		next := opts.NextGeneration
		copts.NextGeneration = func(current shimwire.Generation) shimwire.Generation {
			return next(id, current)
		}
	}
	if opts.Prepare != nil {
		copts.PrepareAdoption = func(evidence AdoptionPreparation) (PreparedAdoption, error) {
			prepared, err := opts.Prepare(ctx, evidence)
			if err != nil {
				return PreparedAdoption{}, fmt.Errorf("%w for %s: %w", ErrAdoptionPreparation, evidence.Identity, err)
			}
			return prepared, nil
		}
	}
	return Dial(ctx, rec, copts)
}

// Adopt classification helpers.

func classifyAdoptionFailure(err error) (QuarantineReason, string) {
	switch {
	case errors.Is(err, shimwire.ErrVersionMismatch):
		return QuarantineProtocolMismatch, err.Error()
	case errors.Is(err, shimwire.ErrExtensionUnsupported):
		return QuarantineProtocolMismatch, err.Error()
	case errors.Is(err, shimwire.ErrStaleGeneration):
		return QuarantineGenerationNotAdvanced, err.Error()
	case errors.Is(err, ErrRegistryUnsafe):
		return QuarantineUnauthenticated, err.Error()
	case errors.Is(err, ErrAdoptionRefused):
		return QuarantineIdentityMismatch, err.Error()
	case errors.Is(err, ErrRecordInvalid):
		return QuarantineRecordMalformed, err.Error()
	case isSocketUnreachable(err):
		// The record's process IS live (checked above) but the endpoint itself
		// answered: refused, or gone. §D10: quarantine — do not kill, do not
		// recreate the socket, do not release a claim. The shim's own orphan
		// deadline is the escape.
		return QuarantineSocketUnreachable, err.Error()
	default:
		return QuarantineAdoptionFailed, err.Error()
	}
}

// isSocketUnreachable is POSITIVE evidence about the endpoint: the connect was
// refused, or nothing is bound at the path any more. Nothing else qualifies.
//
// The predicate this replaced asked only whether the error had a Timeout method
// and never called it — which every net.OpError and os.PathError has — so any
// error the network stack produced was reported as an unreachable socket.
// Measured live: a write timeout on an ALREADY-ESTABLISHED unix socket, to a
// shim whose pid had just been proved alive, was classified socket_unreachable
// on the first re-adoption attempt. A stalled peer is not an absent one, and
// the two disagree about whether anything is still out there to talk to.
func isSocketUnreachable(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, fs.ErrNotExist)
}

// isTransientDialFailure reports the shapes that say only "not this time":
// a real timeout, or a peer that hung up mid-handshake. The socket existed and
// accepted, and the record's process is live, so the honest reading is that the
// shim was busy — a shim mid-snapshot, or one whose accept loop had not yet
// come back around. Retrying is what distinguishes a busy peer from an absent
// one; classifying without retrying just guesses.
func isTransientDialFailure(err error) bool {
	if err == nil || isSocketUnreachable(err) {
		return false
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

func sortResult(r *AdoptionResult) {
	sort.Slice(r.Adopted, func(i, j int) bool {
		return r.Adopted[i].Identity().Key() < r.Adopted[j].Identity().Key()
	})
	SortQuarantined(r.Quarantined)
	sort.Slice(r.Tombstoned, func(i, j int) bool {
		left, right := r.Tombstoned[i], r.Tombstoned[j]
		if left.Identity().Key() != right.Identity().Key() {
			return left.Identity().Key() < right.Identity().Key()
		}
		if left.ShimID != right.ShimID {
			return left.ShimID < right.ShimID
		}
		return left.ProcessEpoch < right.ProcessEpoch
	})
	sort.Slice(r.Stale, func(i, j int) bool {
		return r.Stale[i].Identity().Key() < r.Stale[j].Identity().Key()
	})
}
