package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
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
	Prepare func(ctx context.Context, id Identity, current shimwire.Generation) (PreparedAdoption, error)

	// ResumeFrom returns the first sequence this daemon still needs for a
	// session — its durable last_forwarded_seq + 1. Nil resumes from the start of
	// the stream, which is always SAFE (it can only over-replay, never
	// under-replay) but produces more redundant output.
	ResumeFrom func(id Identity) uint64

	// ExpectedWorkarea returns the workarea this daemon believes a session
	// belongs to. A non-empty mismatch quarantines rather than adopts. Nil skips
	// the daemon-side half of the workarea check; the record-vs-shim half always
	// runs.
	ExpectedWorkarea func(id Identity) string

	// DialTimeout bounds one shim handshake.
	DialTimeout time.Duration

	Logger *slog.Logger
	Now    func() time.Time
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
}

// ErrAdoptionPreparation reports a composing dependency that failed before
// controller authority was proposed. Callers fail startup closed rather than
// converting it into an ordinary compatibility quarantine.
var ErrAdoptionPreparation = errors.New("sessionshim: adoption preparation failed")

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
	tombstoneByID := make(map[Identity]Tombstone, len(tombstones))
	for _, t := range tombstones {
		if t.GroupReaped {
			tombstoneByID[t.Identity()] = t
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
		id := e.Record.Identity()
		seen[id] = append(seen[id], e.Record)
	}

	for _, e := range entries {
		if e.Err != nil {
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

		if _, ok := tombstoneByID[id]; ok {
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

		ctrl, adoptErr := dialForAdoption(ctx, rec, opts)
		if adoptErr != nil {
			if errors.Is(adoptErr, ErrAdoptionPreparation) {
				result.Close()
				return result, adoptErr
			}
			reason, detail := classifyAdoptionFailure(adoptErr)
			result.Quarantined = append(result.Quarantined, NewQuarantinedSession(rec, reason, detail, now))
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

func dialForAdoption(ctx context.Context, rec Record, opts AdoptOptions) (*Controller, error) {
	id := rec.Identity()
	resume := uint64(0)
	if opts.ResumeFrom != nil {
		resume = opts.ResumeFrom(id)
	}
	expected := ""
	if opts.ExpectedWorkarea != nil {
		expected = opts.ExpectedWorkarea(id)
	}

	copts := ControllerOptions{
		ControllerID:     opts.ControllerID,
		ResumeFrom:       resume,
		ExpectedWorkarea: expected,
		DialTimeout:      opts.DialTimeout,
		Logger:           opts.Logger,
	}
	if opts.NextGeneration != nil {
		next := opts.NextGeneration
		copts.NextGeneration = func(current shimwire.Generation) shimwire.Generation {
			return next(id, current)
		}
	}
	if opts.Prepare != nil {
		copts.PrepareAdoption = func(current shimwire.Generation) (PreparedAdoption, error) {
			prepared, err := opts.Prepare(ctx, id, current)
			if err != nil {
				return PreparedAdoption{}, fmt.Errorf("%w for %s: %w", ErrAdoptionPreparation, id, err)
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
	case isDialFailure(err):
		// The record's process IS live (checked above) but the socket is not
		// reachable. §D10: quarantine — do not kill, do not recreate the socket,
		// do not release a claim. The shim's own orphan deadline is the escape.
		return QuarantineSocketUnreachable, err.Error()
	default:
		return QuarantineAdoptionFailed, err.Error()
	}
}

func isDialFailure(err error) bool {
	var opErr interface{ Timeout() bool }
	if errors.As(err, &opErr) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

func sortResult(r *AdoptionResult) {
	sort.Slice(r.Adopted, func(i, j int) bool {
		return r.Adopted[i].Identity().Key() < r.Adopted[j].Identity().Key()
	})
	SortQuarantined(r.Quarantined)
	sort.Slice(r.Tombstoned, func(i, j int) bool {
		return r.Tombstoned[i].Identity().Key() < r.Tombstoned[j].Identity().Key()
	})
	sort.Slice(r.Stale, func(i, j int) bool {
		return r.Stale[i].Identity().Key() < r.Stale[j].Identity().Key()
	})
}
