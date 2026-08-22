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

// SessionShimAdoptionEvidence is the exact local fact handed to a composing
// carrier after controller generation commits and before the daemon advertises
// ready. The correlation fields are deliberately per session: session-shim-v1
// defines no host-wide controller generation.
type SessionShimAdoptionEvidence struct {
	Identity             sessionshim.Identity
	HostID               string
	ControllerID         string
	ShimID               string
	ProcessEpoch         uint64
	ControllerGeneration uint64
	LastForwardedSeq     uint64
	Extensions           shimwire.Extensions
	ObservedAtUnixNano   int64
}

// SessionShimAdoptionReceipt is opaque durable correlation state returned by a
// composing carrier. Donmai never parses or rewrites it. The exact bytes are
// retained in memory and handed back with terminal evidence so a downstream
// implementation can carry its own fence and adoption revisions without
// importing that control-plane schema into OSS.
type SessionShimAdoptionReceipt struct {
	DurableCorrelation []byte
}

// SessionShimTerminalEvidence is emitted only after a tombstone positively
// proves process-group reap. It carries every local correlation plus the exact
// opaque adoption receipt returned above. When a daemon discovers a tombstone
// after an unplanned gap, Adoption is nil: the callback receives the positive
// tombstone as such and must not manufacture a live-adoption fact the daemon
// never observed.
type SessionShimTerminalEvidence struct {
	Identity     sessionshim.Identity
	HostID       string
	ShimID       string
	ProcessEpoch uint64
	// Adoption is present when this daemon observed the live controller
	// generation that preceded the terminal fact. It is nil for an orphan
	// tombstone discovered after restart; D9 permits that authenticated positive
	// proof without fabricating a live-adoption receipt.
	Adoption                   *SessionShimAdoptionEvidence
	DurableAdoptionCorrelation []byte
	Tombstone                  sessionshim.Tombstone
}

// SessionShimConfig configures per-session shim ownership and daemon adoption
// (ADR-2026-08-17). EnableAdoption defaults to false, preserving the accepted
// migration law and standalone behavior.
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

	// HostID is the durable host authority named by restart fences. It is NOT
	// the controller id or worker-registration id. Empty preserves the legacy
	// fallback to the daemon controller id for existing embedders; a hosted
	// multi-organization composition must supply its real stable host identity.
	HostID string

	// HostIDForOrg resolves the durable host authority inside one organization.
	// A multi-organization hosted composition uses this because worker-host row
	// ids may be tenant-scoped. It supersedes HostID for non-empty org ids. Error
	// fails adoption/fencing closed; no worker/controller id is substituted.
	HostIDForOrg func(context.Context, string) (string, error)

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

	// PrepareAdoption runs after the live shim's authenticated Hello exposes its
	// current per-shim generation and before Welcome proposes new authority. It
	// atomically resolves the exact next generation and generic carrier_epoch
	// extension, allowing a durable carrier reservation to bind the two. An error
	// aborts startup rather than producing a ready-but-unreachable session.
	PrepareAdoption func(context.Context, sessionshim.Identity, shimwire.Generation) (sessionshim.PreparedAdoption, error)

	// OnAdoption runs after the shim commits the new controller generation and
	// before readiness/claim advertisement. It returns only after the composing
	// layer has durably rehydrated its external carrier and, when applicable,
	// posted adoption evidence. Its opaque receipt is retained for terminal
	// correlation. An error aborts the launch/startup pass fail-closed.
	OnAdoption func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error)

	// OnTerminalEvidence runs after exact process-group reap proof exists and
	// before Donmai disposes the tombstone. It must durably post or retain the
	// terminal fact before returning nil. Error retains the tombstone for exact,
	// idempotent replay on a later reconciliation/startup pass.
	OnTerminalEvidence func(context.Context, SessionShimTerminalEvidence) error

	// CallbackTimeout bounds PrepareAdoption, OnAdoption, and
	// OnTerminalEvidence. Zero uses the launch timeout/default.
	CallbackTimeout time.Duration

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
	// adoption and its opaque receipt remain attached to this exact controller
	// generation until terminal proof is handed downstream.
	adoption        SessionShimAdoptionEvidence
	adoptionReceipt SessionShimAdoptionReceipt
}

type shimIncarnation struct {
	identity     sessionshim.Identity
	shimID       string
	processEpoch uint64
}

type sessionShimAdoptionCorrelation struct {
	evidence SessionShimAdoptionEvidence
	receipt  SessionShimAdoptionReceipt
}

// sessionShimState is the daemon's live view of per-session shim ownership.
type sessionShimState struct {
	mu          sync.RWMutex
	registry    *sessionshim.Registry
	adopted     map[sessionshim.Identity]adoptedShim
	quarantined []sessionshim.QuarantinedSession
	tombstoned  []sessionshim.Tombstone
	fence       *sessionshim.Fence
	fences      map[string]sessionshim.Fence
	// forwarded is the highest output sequence this daemon durably forwarded per
	// session — the resume point a LATER adoption asks the shim to replay from
	// (§D5). The daemon records only this; it never allocates sequence.
	forwarded map[sessionshim.Identity]uint64
	// correlations survive a controller disconnect so the later exact tombstone
	// callback still receives the same opaque durable adoption receipt.
	correlations map[shimIncarnation]sessionShimAdoptionCorrelation
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
		adopted:      make(map[sessionshim.Identity]adoptedShim),
		forwarded:    make(map[sessionshim.Identity]uint64),
		correlations: make(map[shimIncarnation]sessionShimAdoptionCorrelation),
		fences:       make(map[string]sessionshim.Fence),
	}
}

func shimIncarnationFor(evidence SessionShimAdoptionEvidence) shimIncarnation {
	return shimIncarnation{
		identity:     evidence.Identity,
		shimID:       evidence.ShimID,
		processEpoch: evidence.ProcessEpoch,
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

// orgIDForSession resolves the organization half of one lifecycle identity.
// The per-session value is authoritative when present; OrgID remains the
// source-compatible standalone/legacy fallback.
func (c SessionShimConfig) orgIDForSession(spec SessionSpec) string {
	if spec.OrganizationID != "" {
		return spec.OrganizationID
	}
	return c.orgID()
}

// launchTimeout returns the effective bound on one shim launch.
func (c SessionShimConfig) launchTimeout() time.Duration {
	if c.LaunchTimeout <= 0 {
		return defaultShimLaunchTimeout
	}
	return c.LaunchTimeout
}

func (c SessionShimConfig) callbackTimeout() time.Duration {
	if c.CallbackTimeout > 0 {
		return c.CallbackTimeout
	}
	return c.launchTimeout()
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
	preparedByID := make(map[sessionshim.Identity]sessionshim.PreparedAdoption)
	hostByID := make(map[sessionshim.Identity]string)
	if cfg.PrepareAdoption != nil || cfg.HostIDForOrg != nil {
		opts.Prepare = func(prepareCtx context.Context, id sessionshim.Identity, current shimwire.Generation) (sessionshim.PreparedAdoption, error) {
			hostID, hostErr := d.sessionShimHostID(prepareCtx, id.OrgID)
			if hostErr != nil {
				return sessionshim.PreparedAdoption{}, hostErr
			}
			prepared, err := d.prepareSessionShimAdoption(prepareCtx, id, current)
			if err != nil {
				return sessionshim.PreparedAdoption{}, err
			}
			hostByID[id] = hostID
			preparedByID[id] = prepared
			return prepared, nil
		}
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

	// The local generation is committed, but startup is still NOT ready. Give
	// the composing carrier each exact fact and require its durable handoff
	// before publishing adoptionComplete or starting registration.
	entries := make(map[sessionshim.Identity]adoptedShim, len(result.Adopted))
	for _, c := range result.Adopted {
		id := c.Identity()
		evidence, evidenceErr := d.sessionShimAdoptionEvidence(ctx, c, preparedByID[id].Extensions, hostByID[id])
		if evidenceErr != nil {
			result.Close()
			return fmt.Errorf("session shim: resolve adoption host for %s: %w", id, evidenceErr)
		}
		receipt, callbackErr := d.completeSessionShimAdoption(ctx, evidence)
		if callbackErr != nil {
			result.Close()
			return fmt.Errorf("session shim: durable adoption for %s: %w", id, callbackErr)
		}
		entries[id] = adoptedShim{
			controller:      c,
			shimID:          c.Hello().ShimID,
			adoption:        evidence,
			adoptionReceipt: receipt,
		}
	}

	// A startup tombstone is the orphan path's retained positive evidence. Post
	// it before readiness too: otherwise a controller could become ready while a
	// fenced claim remained needlessly unreconciled. Unproven tombstones were
	// classified as capacity-consuming quarantine by sessionshim.Adopt and never
	// enter this loop.
	for _, tombstone := range result.Tombstoned {
		hostID, hostErr := d.sessionShimHostID(ctx, tombstone.OrgID)
		if hostErr != nil {
			result.Close()
			return fmt.Errorf("session shim: resolve terminal host for %s: %w", tombstone.Identity(), hostErr)
		}
		evidence := SessionShimTerminalEvidence{
			Identity:     tombstone.Identity(),
			HostID:       hostID,
			ShimID:       tombstone.ShimID,
			ProcessEpoch: tombstone.ProcessEpoch,
			Tombstone:    tombstone,
		}
		if callbackErr := d.reportSessionShimTerminalEvidence(ctx, evidence); callbackErr != nil {
			result.Close()
			return fmt.Errorf("session shim: durable terminal evidence for %s: %w", tombstone.Identity(), callbackErr)
		}
	}

	d.shims.mu.Lock()
	d.shims.registry = registry
	for id, entry := range entries {
		d.shims.adopted[id] = entry
		d.shims.correlations[shimIncarnationFor(entry.adoption)] = sessionShimAdoptionCorrelation{
			evidence: entry.adoption,
			receipt:  cloneSessionShimAdoptionReceipt(entry.adoptionReceipt),
		}
		c := entry.controller
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
	for _, tombstone := range result.Tombstoned {
		if removeErr := registry.RemoveTombstone(tombstone.Identity()); removeErr != nil {
			slog.Warn("session shim: dispose startup tombstone after durable terminal handoff",
				"session", tombstone.Identity().String(), "error", removeErr)
		}
	}

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

// sessionShimHostID returns the host authority named by restart/adoption
// evidence for one organization. The resolver wins, then the explicit static
// config. Falling back to controllerID preserves the old standalone API only;
// hosted composition supplies one of the first two and never relabels a worker.
func (d *Daemon) sessionShimHostID(ctx context.Context, orgID string) (string, error) {
	cfg := d.sessionShimConfig()
	if cfg.HostIDForOrg != nil && orgID != "" {
		callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
		defer cancel()
		hostID, err := cfg.HostIDForOrg(callbackCtx, orgID)
		if err != nil {
			return "", err
		}
		if hostID == "" {
			return "", fmt.Errorf("session shim: host identity resolver returned empty for organization %q", orgID)
		}
		return hostID, nil
	}
	if cfg.HostID != "" {
		return cfg.HostID, nil
	}
	return d.controllerID(), nil
}

func cloneShimExtensions(in shimwire.Extensions) shimwire.Extensions {
	out := shimwire.Extensions{Required: append([]string(nil), in.Required...)}
	if in.Values != nil {
		out.Values = make(map[string]string, len(in.Values))
		for key, value := range in.Values {
			out.Values[key] = value
		}
	}
	return out
}

func cloneSessionShimAdoptionReceipt(in SessionShimAdoptionReceipt) SessionShimAdoptionReceipt {
	return SessionShimAdoptionReceipt{DurableCorrelation: append([]byte(nil), in.DurableCorrelation...)}
}

func (d *Daemon) sessionShimCallbackContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, d.sessionShimConfig().callbackTimeout())
}

func (d *Daemon) prepareSessionShimAdoption(ctx context.Context, id sessionshim.Identity, current shimwire.Generation) (sessionshim.PreparedAdoption, error) {
	hook := d.sessionShimConfig().PrepareAdoption
	if hook == nil {
		return sessionshim.PreparedAdoption{}, nil
	}
	callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
	defer cancel()
	prepared, err := hook(callbackCtx, id, current)
	if err != nil {
		return sessionshim.PreparedAdoption{}, err
	}
	prepared.Extensions = cloneShimExtensions(prepared.Extensions)
	return prepared, nil
}

func (d *Daemon) sessionShimAdoptionEvidence(
	ctx context.Context,
	ctrl *sessionshim.Controller,
	extensions shimwire.Extensions,
	preparedHostID string,
) (SessionShimAdoptionEvidence, error) {
	lastForwarded := uint64(0)
	if resumeFrom := ctrl.ResumeFrom(); resumeFrom > 0 {
		lastForwarded = resumeFrom - 1
	}
	hostID := preparedHostID
	if hostID == "" {
		var err error
		hostID, err = d.sessionShimHostID(ctx, ctrl.Identity().OrgID)
		if err != nil {
			return SessionShimAdoptionEvidence{}, err
		}
	}
	return SessionShimAdoptionEvidence{
		Identity:             ctrl.Identity(),
		HostID:               hostID,
		ControllerID:         ctrl.ControllerID(),
		ShimID:               ctrl.Hello().ShimID,
		ProcessEpoch:         ctrl.Hello().ProcessEpoch,
		ControllerGeneration: uint64(ctrl.Generation()),
		LastForwardedSeq:     lastForwarded,
		Extensions:           cloneShimExtensions(extensions),
		ObservedAtUnixNano:   d.shimNow().UnixNano(),
	}, nil
}

func (d *Daemon) completeSessionShimAdoption(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
	hook := d.sessionShimConfig().OnAdoption
	if hook == nil {
		return SessionShimAdoptionReceipt{}, nil
	}
	callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
	defer cancel()
	receipt, err := hook(callbackCtx, evidence)
	if err != nil {
		return SessionShimAdoptionReceipt{}, err
	}
	return cloneSessionShimAdoptionReceipt(receipt), nil
}

func (d *Daemon) reportSessionShimTerminalEvidence(ctx context.Context, evidence SessionShimTerminalEvidence) error {
	if !evidence.Tombstone.GroupReaped {
		return errors.New("session shim: terminal tombstone does not prove process-group reap")
	}
	hook := d.sessionShimConfig().OnTerminalEvidence
	if hook == nil {
		return nil
	}
	if evidence.Adoption != nil {
		adoption := *evidence.Adoption
		adoption.Extensions = cloneShimExtensions(adoption.Extensions)
		evidence.Adoption = &adoption
	}
	evidence.DurableAdoptionCorrelation = append([]byte(nil), evidence.DurableAdoptionCorrelation...)
	callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
	defer cancel()
	return hook(callbackCtx, evidence)
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
	covered := d.sessionShimFenceSnapshot()
	scopeOrg := ""
	for _, session := range covered {
		if scopeOrg == "" {
			scopeOrg = session.OrgID
			continue
		}
		if session.OrgID != scopeOrg && d.sessionShimConfig().HostIDForOrg != nil {
			return sessionshim.Fence{}, fmt.Errorf(
				"%w: multi-organization host identity requires RequestSessionShimRestartFences",
				sessionshim.ErrFenceRequired,
			)
		}
	}
	fence, err := d.requestSessionShimRestartFence(ctx, fenceID, scopeOrg, covered)
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

// RequestSessionShimRestartFences obtains one exact fence per organization.
//
// Hosted runtime credentials and lifecycle release predicates are tenant
// scoped, while one physical host may serve several organizations. A single
// cross-organization request cannot be authenticated without widening that
// boundary. This additive plural method snapshots once, partitions without
// collapsing identities, and requires every per-org acknowledgement before the
// caller may restart. The same fence id is safe across organizations because
// lifecycle scope is part of every covered identity and store key.
func (d *Daemon) RequestSessionShimRestartFences(ctx context.Context, fenceID string) ([]sessionshim.Fence, error) {
	if fenceID == "" {
		return nil, fmt.Errorf("%w: fence id is required", sessionshim.ErrFenceRequired)
	}
	covered := d.sessionShimFenceSnapshot()
	if len(covered) == 0 {
		return nil, nil
	}
	byOrg := make(map[string][]sessionshim.FencedSession)
	for _, session := range covered {
		byOrg[session.OrgID] = append(byOrg[session.OrgID], session)
	}
	orgIDs := make([]string, 0, len(byOrg))
	for orgID := range byOrg {
		orgIDs = append(orgIDs, orgID)
	}
	sort.Strings(orgIDs)

	fences := make([]sessionshim.Fence, 0, len(orgIDs))
	for _, orgID := range orgIDs {
		fence, err := d.requestSessionShimRestartFence(ctx, fenceID, orgID, byOrg[orgID])
		if err != nil {
			return fences, fmt.Errorf("session shim: restart fence for organization %q: %w", orgID, err)
		}
		fences = append(fences, fence)
		if d.shims != nil {
			d.shims.mu.Lock()
			d.shims.fences[orgID] = fence
			d.shims.mu.Unlock()
		}
	}
	return fences, nil
}

func (d *Daemon) sessionShimFenceSnapshot() []sessionshim.FencedSession {
	var covered []sessionshim.FencedSession
	if d.shims == nil {
		return covered
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	for id, entry := range d.shims.adopted {
		coveredSession := sessionshim.FencedSession{
			OrgID: id.OrgID, SessionID: id.SessionID, ShimID: entry.shimID,
			LastForwardedSeq: d.shims.forwarded[id],
		}
		if entry.controller != nil {
			coveredSession.ProcessEpoch = entry.controller.Hello().ProcessEpoch
			coveredSession.ControllerGeneration = uint64(entry.controller.Generation())
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
		if covered[i].ProcessEpoch != covered[j].ProcessEpoch {
			return covered[i].ProcessEpoch < covered[j].ProcessEpoch
		}
		if covered[i].ControllerGeneration != covered[j].ControllerGeneration {
			return covered[i].ControllerGeneration < covered[j].ControllerGeneration
		}
		return covered[i].LastForwardedSeq < covered[j].LastForwardedSeq
	})
	return covered
}

func (d *Daemon) requestSessionShimRestartFence(ctx context.Context, fenceID, orgID string, covered []sessionshim.FencedSession) (sessionshim.Fence, error) {
	cfg := d.sessionShimConfig()
	hostID, err := d.sessionShimHostID(ctx, orgID)
	if err != nil {
		return sessionshim.Fence{}, fmt.Errorf("%w: resolve host identity: %w", sessionshim.ErrFenceRequired, err)
	}
	policy := sessionshim.FencePolicy{RestartBudget: cfg.RestartBudget, Orphan: cfg.Orphan}
	var (
		fence    sessionshim.Fence
		fenceErr error
	)
	if cfg.ExactFenceStore != nil {
		fence, fenceErr = sessionshim.RequestFenceExact(ctx, cfg.ExactFenceStore, fenceID, hostID, covered, policy, time.Now())
	} else {
		fence, fenceErr = sessionshim.RequestFence(ctx, cfg.FenceStore, fenceID, hostID, covered, policy, time.Now())
	}
	if fenceErr != nil {
		return sessionshim.Fence{}, fenceErr
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
		if scoped, ok := d.shims.fences[id.OrgID]; ok && scoped.Covers(id) {
			fenceCopy := scoped
			fence = &fenceCopy
		} else {
			fence = d.shims.fence
		}
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
