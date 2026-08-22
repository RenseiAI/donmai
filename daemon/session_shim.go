package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// SessionShimConfig configures per-session shim ownership and daemon adoption
// (ADR-2026-08-17).
//
// EnableAdoption defaults to FALSE, and that default is the ADR's own migration
// law rather than caution for its own sake: §D11 step 1 ships the protocol,
// the registry, and registry INSPECTION with adoption off, so a release can be
// rolled out and observed before it starts taking ownership of live terminals.
// A daemon that leaves this unset behaves exactly as it did before this package
// existed.
type SessionShimConfig struct {
	// EnableAdoption turns on the startup adoption pass (§D11 step 3).
	EnableAdoption bool

	// EnableOwnership makes this daemon LAUNCH new interactive sessions under
	// per-session shim ownership (§D11 step 2). It is separate from
	// EnableAdoption on purpose, and the separation is the migration order
	// itself: a fleet turns adoption on first so a daemon can take over shims it
	// finds, and only then starts creating them. Reversing that would produce
	// shims no daemon in the fleet knows how to adopt.
	EnableOwnership bool

	// OrgID is the organization half of the lifecycle identity (§D2). A
	// standalone OSS daemon has no organization boundary, so it defaults to
	// "local" — a real value rather than an empty one, because the identity is
	// hashed into a filename and an empty half would make every session on the
	// host key off its session id alone with no room to ever add a second tenant.
	OrgID string

	// LaunchTimeout bounds how long a launch waits for the new shim to publish
	// its discovery record and complete a handshake. Zero uses
	// defaultShimLaunchTimeout.
	LaunchTimeout time.Duration

	// RegistryDir is where discovery records, sockets, and tombstones live.
	// Empty resolves through the injected state-directory seam, so no
	// install-specific path is compiled in.
	RegistryDir string

	// Orphan bounds the shim-owned controller-loss rule (§D8). A zero policy
	// uses sessionshim.DefaultOrphanPolicy.
	//
	// ExternalReleaseThreshold is how a composing deployment declares the
	// smallest interval after which something OUTSIDE this host would consider a
	// session abandoned. Setting it makes the §D8 inequality checkable; leaving
	// it zero means "nothing external can release a claim", which is true of a
	// standalone daemon.
	Orphan sessionshim.OrphanPolicy

	// RestartBudget is how long a planned restart is expected to take. It sizes
	// the restart fence's hold window together with the orphan bound.
	RestartBudget time.Duration

	// FenceStore is the OPTIONAL composing-plane restart-fence persister (§D9).
	// Nil is fully supported: a standalone daemon has no remote reaper to fence
	// against and still gets the local bounded-orphan rule. This field retains the
	// v0.67 source contract; hosted activation uses ExactFenceStore below.
	FenceStore sessionshim.FenceStore

	// ExactFenceStore is the additive hosted restart-fence persister. When set,
	// RequestSessionShimRestartFence uses the exact request-byte and durable
	// revision contract. It is separate so the v0.67 FenceStore field remains
	// source-compatible for OSS embedders.
	ExactFenceStore sessionshim.ExactFenceStore

	// ExpectedWorkarea returns the workarea this daemon believes a session
	// belongs to, for the adoption-time workarea identity check. Nil skips only
	// the daemon-side half; the record-versus-live-shim half always runs.
	ExpectedWorkarea func(orgID, sessionID string) string

	// OnSessionEvent, when set, receives EVERY event from every adopted session:
	// output frames carrying the shim-allocated sequence, declared gaps,
	// snapshots, error frames, and the terminal Exit.
	//
	// It is the attachment point for a composing carrier. The daemon deliberately
	// does not interpret output — §D5 makes the shim the sole allocator of host
	// sequence, and a daemon that re-framed or renumbered what it forwards would
	// be fabricating the continuity the ADR forbids. What arrives here is what
	// the shim produced, in order, gaps included.
	//
	// It is called from the per-session consumer goroutine and MUST NOT block
	// indefinitely: a stalled consumer stops acknowledgements, which costs the
	// next adoption an avoidable replay gap.
	OnSessionEvent func(sessionshim.Identity, sessionshim.ControllerEvent)

	// OnSessionEventDurable is the optional durable carrier handoff. Unlike
	// OnSessionEvent, this callback is not an observer: a nil callback means no
	// carrier durability is available, and a non-nil callback must return nil
	// only after it has durably accepted the event. Output and snapshot sequence
	// state advances, and the shim heartbeat acknowledgement is sent, only after
	// that successful return. It MUST be bounded for the same reason as
	// OnSessionEvent.
	OnSessionEventDurable func(sessionshim.Identity, sessionshim.ControllerEvent) error

	// ResumeFrom returns the first output sequence this daemon still needs for a
	// session (its durable last_forwarded_seq + 1). Nil resumes from the start of
	// the stream, which can only over-replay, never under-replay.
	ResumeFrom func(orgID, sessionID string) uint64
}

// defaultShimRegistryDir resolves the registry location through the injected
// state-directory seam.
func defaultShimRegistryDir() string {
	return statepath.Resolve("session-shims", "")
}

// adoptedShim is one session under this daemon's control.
//
// ShimID is recorded here at adoption time rather than read back off the
// controller on demand. That keeps the reporting paths — capacity, the restart
// fence, host diagnostics — independent of whether a live connection is still
// held, which is exactly when those paths matter most.
type adoptedShim struct {
	controller *sessionshim.Controller
	shimID     string
	// handle is the published SessionHandle for a shim this daemon LAUNCHED. A
	// shim adopted at startup has none: the spec that created it belonged to a
	// daemon generation that is gone, and inventing project/repository fields
	// from nothing would put guesses on an operator-facing surface.
	handle SessionHandle
	// spec is retained only for a shim this daemon launched. Its exact value is
	// the lifecycle payload delivered to ordinary WorkerSpawner listeners; an
	// adopted shim deliberately has no fabricated spec.
	spec SessionSpec
	// launched distinguishes "this daemon created it" from "this daemon adopted
	// it after a restart" — a diagnostic distinction only (§D11: ownership mode
	// is a diagnostic field, never a second lifecycle authority).
	launched bool
	// terminal serializes immutable Exit handling. The entry stays present while
	// synchronous Ended listeners run, preserving capacity ownership until their
	// generation-scoped cleanup is complete.
	terminal bool
}

// sessionShimState is the daemon's live view of per-session shim ownership.
type sessionShimState struct {
	mu          sync.RWMutex
	registry    *sessionshim.Registry
	adopted     map[sessionshim.Identity]adoptedShim
	quarantined []sessionshim.QuarantinedSession
	tombstoned  []sessionshim.Tombstone
	fence       *sessionshim.Fence
	// forwarded is the highest output sequence this daemon durably forwarded per
	// session — the resume point a LATER adoption asks the shim to replay from
	// (§D5). The daemon records only this; it never allocates sequence.
	forwarded map[sessionshim.Identity]uint64
	// wg joins the per-session event consumers so shutdown cannot race one that
	// is still writing bookkeeping.
	wg sync.WaitGroup
	// adoptionComplete records that the §D4 pass ran to completion. Capacity and
	// readiness read it: a daemon that has NOT finished adopting must not
	// advertise, because it does not yet know what is occupied.
	adoptionComplete bool
}

func newSessionShimState() *sessionShimState {
	return &sessionShimState{
		adopted:   make(map[sessionshim.Identity]adoptedShim),
		forwarded: make(map[sessionshim.Identity]uint64),
	}
}

// sessionShimConfig returns the effective configuration.
func (d *Daemon) sessionShimConfig() SessionShimConfig {
	cfg := d.opts.SessionShim
	if cfg.RegistryDir == "" {
		cfg.RegistryDir = defaultShimRegistryDir()
	}
	if cfg.Orphan.Deadline == 0 {
		policy := sessionshim.DefaultOrphanPolicy()
		policy.ExternalReleaseThreshold = cfg.Orphan.ExternalReleaseThreshold
		cfg.Orphan = policy
	}
	return cfg
}

// defaultShimOrgID is the organization identity a standalone OSS daemon uses.
const defaultShimOrgID = "local"

// orgID returns the effective organization half of every lifecycle identity.
func (c SessionShimConfig) orgID() string {
	if c.OrgID == "" {
		return defaultShimOrgID
	}
	return c.OrgID
}

// launchTimeout returns the effective bound on one shim launch.
func (c SessionShimConfig) launchTimeout() time.Duration {
	if c.LaunchTimeout <= 0 {
		return defaultShimLaunchTimeout
	}
	return c.LaunchTimeout
}

// adoptSessionShims runs the §D4 startup pass: discover, classify, adopt every
// compatible live shim, and account for every quarantined one.
//
// It MUST complete before the daemon registers, advertises ready capacity, or
// claims work. Start calls it in exactly that position. Returning an error is a
// startup failure rather than a warning: a daemon that could not determine what
// is already running on this host cannot honestly advertise how much it can take.
func (d *Daemon) adoptSessionShims(ctx context.Context) error {
	cfg := d.sessionShimConfig()

	// The §D8 inequality is validated at STARTUP, before any session is admitted.
	// A configuration whose orphan bound can outlast an external release
	// threshold is capable of double execution, and discovering that at deadline
	// time means discovering it from the damage.
	if err := cfg.Orphan.Validate(); err != nil {
		return fmt.Errorf("session shim: %w", err)
	}

	if !cfg.EnableAdoption {
		// §D11 step 1: inspection-only. The registry is still opened and scanned
		// so `host status` can SHOW what is out there, but nothing is adopted and
		// nothing is quarantined — this daemon claims no authority it has not
		// negotiated.
		return nil
	}

	registry, err := d.sessionShimRegistry()
	if err != nil {
		return err
	}

	opts := sessionshim.AdoptOptions{
		Registry:     registry,
		ControllerID: d.controllerID(),
		Logger:       slog.Default(),
	}
	if cfg.ResumeFrom != nil {
		resume := cfg.ResumeFrom
		opts.ResumeFrom = func(id sessionshim.Identity) uint64 {
			return resume(id.OrgID, id.SessionID)
		}
	} else {
		// With no durable store configured, resume from what THIS process has
		// forwarded — zero on a cold start, which replays from the beginning of
		// the ring. Over-replay is always safe; under-replay is not (§D5).
		opts.ResumeFrom = func(id sessionshim.Identity) uint64 {
			return d.SessionShimForwardedSeq(id.OrgID, id.SessionID)
		}
	}
	if cfg.ExpectedWorkarea != nil {
		expected := cfg.ExpectedWorkarea
		opts.ExpectedWorkarea = func(id sessionshim.Identity) string {
			return expected(id.OrgID, id.SessionID)
		}
	}

	result, err := sessionshim.Adopt(ctx, opts)
	if err != nil {
		if errors.Is(err, sessionshim.ErrShimUnsupported) {
			// §D3: a platform without a trustworthy peer-credential primitive
			// keeps adoption disabled rather than running unauthenticated. Nothing
			// was adopted and nothing is claimed to be occupied.
			slog.Warn("session shim: adoption unsupported on this platform; continuing without it")
			return nil
		}
		return fmt.Errorf("session shim: adopt: %w", err)
	}

	d.shims.mu.Lock()
	d.shims.registry = registry
	for _, c := range result.Adopted {
		id := c.Identity()
		d.shims.adopted[id] = adoptedShim{controller: c, shimID: c.Hello().ShimID}
		if resumeFrom := c.ResumeFrom(); resumeFrom > 0 {
			// ResumeFrom is exactly last_forwarded_seq + 1. Seed the replacement
			// daemon's snapshot before its event consumer starts so an immediate
			// second planned restart cannot regress the durable correlation to zero.
			d.shims.forwarded[id] = resumeFrom - 1
		}
	}
	d.shims.quarantined = result.QuarantinedProjection()
	d.shims.tombstoned = append(d.shims.tombstoned, result.Tombstoned...)
	d.shims.adoptionComplete = true
	d.shims.mu.Unlock()

	// Adoption without a consumer would be authority this daemon never exercises:
	// the shim's ring is bounded, so an unread stream costs the next adoption an
	// avoidable Gap, and a terminal Exit would never reach the cleanup path.
	for _, c := range result.Adopted {
		d.consumeShimEvents(c)
	}

	slog.Info("session shim: startup adoption complete",
		"adopted", len(result.Adopted),
		"quarantined", len(result.Quarantined),
		"tombstoned", len(result.Tombstoned),
		"stale", len(result.Stale),
		"occupiedSlots", result.OccupiedSlots())
	return nil
}

// controllerID identifies this daemon process in shim diagnostics.
func (d *Daemon) controllerID() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.workerID != "" {
		return d.workerID
	}
	return "daemon"
}

// SessionShimOccupancy returns how many capacity slots per-session shims hold.
//
// Adopted AND quarantined shims count. §D7 is explicit that a quarantined shim
// is still occupied capacity: its harness is running, this daemon simply has no
// authority over it. Excluding it would advertise slots that are physically in
// use and let the host claim work it cannot actually run.
func (d *Daemon) SessionShimOccupancy() int {
	if d.shims == nil {
		return 0
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return len(d.shims.adopted) + len(d.shims.quarantined)
}

// AdoptedSessionShims returns the identities currently under this daemon's
// control, for host diagnostics.
func (d *Daemon) AdoptedSessionShims() []sessionshim.Identity {
	if d.shims == nil {
		return nil
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	out := make([]sessionshim.Identity, 0, len(d.shims.adopted))
	for id := range d.shims.adopted {
		out = append(out, id)
	}
	return out
}

// QuarantinedSessions returns the bounded quarantine projection for host status
// and heartbeat payloads (§D7). The same projection reaches both surfaces, so an
// operator reading `host status` and a consumer reading a beat cannot disagree
// about what is quarantined.
func (d *Daemon) QuarantinedSessions() []sessionshim.QuarantinedSession {
	if d.shims == nil {
		return nil
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	if len(d.shims.quarantined) == 0 {
		return nil
	}
	out := make([]sessionshim.QuarantinedSession, len(d.shims.quarantined))
	copy(out, d.shims.quarantined)
	return out
}

// SessionShimAdoptionComplete reports whether the §D4 pass finished.
//
// Readiness reads this. Adoption disabled reads as complete, because a daemon
// that never adopts has nothing left to discover.
func (d *Daemon) SessionShimAdoptionComplete() bool {
	if d.shims == nil {
		return true
	}
	if !d.sessionShimConfig().EnableAdoption {
		return true
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return d.shims.adoptionComplete
}

// RequestSessionShimRestartFence obtains the durable, acknowledged restart fence
// a PLANNED restart requires (§D9).
//
// The fence enumerates every adopted AND quarantined session, because both kinds
// of harness are still running across the restart. If no acknowledgement
// arrives, this returns an error and the caller MUST refuse the restart and keep
// serving — an unfenced restart is exactly the split-brain the fence prevents.
func (d *Daemon) RequestSessionShimRestartFence(ctx context.Context, fenceID string) (sessionshim.Fence, error) {
	cfg := d.sessionShimConfig()
	var covered []sessionshim.FencedSession

	if d.shims != nil {
		d.reconcileQuarantinedTombstones()
		d.shims.mu.RLock()
		for id, entry := range d.shims.adopted {
			coveredSession := sessionshim.FencedSession{
				OrgID: id.OrgID, SessionID: id.SessionID, ShimID: entry.shimID,
				LastForwardedSeq: d.shims.forwarded[id],
			}
			if entry.controller != nil {
				coveredSession.ProcessEpoch = entry.controller.Hello().ProcessEpoch
			}
			covered = append(covered, coveredSession)
		}
		for _, q := range d.shims.quarantined {
			id := q.Identity()
			covered = append(covered, sessionshim.FencedSession{
				OrgID: q.OrgID, SessionID: q.SessionID, ShimID: q.ShimID, ProcessEpoch: q.ProcessEpoch,
				LastForwardedSeq: d.shims.forwarded[id],
			})
		}
		d.shims.mu.RUnlock()
	}
	// RequestFence preserves order byte-for-byte because the composing store's
	// durable acknowledgement must echo the exact request. The daemon owns the
	// snapshot order, so make it deterministic instead of leaking Go map order.
	sort.Slice(covered, func(i, j int) bool {
		if covered[i].OrgID != covered[j].OrgID {
			return covered[i].OrgID < covered[j].OrgID
		}
		if covered[i].SessionID != covered[j].SessionID {
			return covered[i].SessionID < covered[j].SessionID
		}
		if covered[i].ShimID != covered[j].ShimID {
			return covered[i].ShimID < covered[j].ShimID
		}
		return covered[i].ProcessEpoch < covered[j].ProcessEpoch
	})

	policy := sessionshim.FencePolicy{RestartBudget: cfg.RestartBudget, Orphan: cfg.Orphan}
	var (
		fence sessionshim.Fence
		err   error
	)
	if cfg.ExactFenceStore != nil {
		fence, err = sessionshim.RequestFenceExact(ctx, cfg.ExactFenceStore, fenceID, d.controllerID(), covered, policy, time.Now())
	} else {
		fence, err = sessionshim.RequestFence(ctx, cfg.FenceStore, fenceID, d.controllerID(), covered, policy, time.Now())
	}
	if err != nil {
		return sessionshim.Fence{}, err
	}
	if d.shims != nil {
		d.shims.mu.Lock()
		d.shims.fence = &fence
		d.shims.mu.Unlock()
	}
	return fence, nil
}

// SessionShimReleaseDecision is the SINGLE predicate every claim-release and
// terminalization path must consult (§D9/§D10).
//
// It exists as one method rather than a check sprinkled through each reaper for
// the reason the ADR names directly: a per-reaper check recreates split-brain
// through whichever path forgets it. Routing every caller here means a new
// release path either uses the contract or is visibly not using it.
//
// The rule it enforces: fence EXPIRY never releases a claim. Release requires
// either a terminal receipt from an adopted live owner, or a durable shim
// tombstone proving the harness process group was reaped. Without either, the
// session stays visible in reconciliation.
func (d *Daemon) SessionShimReleaseDecision(orgID, sessionID string, proof sessionshim.TerminalProof) sessionshim.ReleaseVerdict {
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	var fence *sessionshim.Fence
	if d.shims != nil {
		d.shims.mu.RLock()
		fence = d.shims.fence
		d.shims.mu.RUnlock()
	}
	return sessionshim.ReleaseDecision(fence, id, proof, time.Now())
}

// SessionShimTerminalProof gathers whatever durable evidence exists that a
// session's workload actually ended.
//
// It looks ONLY for positive observations: an adopted live owner reporting a
// terminal receipt, or a tombstone whose GroupReaped flag records a verified
// reap. Absence of a record, an unreachable socket, and a dead PID are all
// deliberately absent from this function — none of them observes a harness
// stopping, and treating them as proof is the exact inference §D10 forbids.
func (d *Daemon) SessionShimTerminalProof(orgID, sessionID string) sessionshim.TerminalProof {
	if d.shims == nil {
		return sessionshim.TerminalProof{}
	}
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	d.shims.mu.RLock()
	registry := d.shims.registry
	tombstones := d.shims.tombstoned
	d.shims.mu.RUnlock()

	for i := range tombstones {
		if tombstones[i].Identity() == id {
			t := tombstones[i]
			return sessionshim.TerminalProof{Tombstone: &t}
		}
	}
	if registry != nil {
		if t, err := registry.GetTombstone(id); err == nil {
			return sessionshim.TerminalProof{Tombstone: &t}
		}
	}
	return sessionshim.TerminalProof{}
}

// ReleaseAdoptedSessionShims drops every adopted controller WITHOUT stopping any
// session.
//
// This is what an ordinary daemon shutdown does, and the asymmetry is the whole
// design: the daemon lets go of the socket, the shim keeps the harness and
// starts its bounded orphan clock, and the next daemon adopts. Stopping the
// sessions here would make a restart destructive again.
func (d *Daemon) ReleaseAdoptedSessionShims() {
	if d.shims == nil {
		return
	}
	d.shims.mu.Lock()
	adopted := d.shims.adopted
	d.shims.adopted = make(map[sessionshim.Identity]adoptedShim)
	d.shims.adoptionComplete = false
	d.shims.mu.Unlock()
	for _, entry := range adopted {
		if entry.controller != nil {
			_ = entry.controller.Close()
		}
	}
	// Join the event consumers: closing a controller ends its stream, and a
	// consumer still recording bookkeeping while Stop returns would make
	// "shut down" mean less than it says.
	d.waitShimConsumers()
}

// StopAdoptedSessionShim sends a generation-fenced Stop to one adopted session.
// A session this daemon has NOT adopted (including a quarantined one) is refused
// rather than reached for by another means — quarantine means no stop authority
// (§D7), and honouring it here is what keeps "quarantine, not kill" true.
func (d *Daemon) StopAdoptedSessionShim(orgID, sessionID string, reason shimwire.StopReason) error {
	if d.shims == nil {
		return errors.New("session shim: adoption is not configured")
	}
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	d.shims.mu.RLock()
	entry, ok := d.shims.adopted[id]
	d.shims.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session shim: %s is not adopted by this daemon", id)
	}
	if entry.controller == nil {
		// An entry without a live connection cannot carry a generation-fenced
		// Stop, and reaching the shim by any other means would bypass the fence.
		return fmt.Errorf("session shim: %s has no live controller connection", id)
	}
	return entry.controller.Stop(reason)
}
