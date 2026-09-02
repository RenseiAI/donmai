package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// defaultShimLaunchTimeout bounds how long the daemon waits for a freshly
// launched shim to publish its discovery record and complete a handshake.
//
// It is generous because the shim's first act is spawning a real harness under a
// PTY on a possibly-loaded host, and it is BOUNDED because a launch that never
// produces a record must fail the accept rather than hold a capacity slot for a
// session that does not exist. It bounds discovery and handshake ONLY: the
// durable adoption publication that follows runs on its own derived budget
// (adoptionPublicationTimeout), never on this clock's remainder.
const defaultShimLaunchTimeout = 30 * time.Second

type shimAdoptionGate struct {
	done      chan struct{}
	once      sync.Once
	committed bool
}

func newShimAdoptionGate() *shimAdoptionGate { return &shimAdoptionGate{done: make(chan struct{})} }

func (g *shimAdoptionGate) finish(committed bool) {
	if g == nil {
		return
	}
	g.once.Do(func() {
		g.committed = committed
		close(g.done)
	})
}

func (g *shimAdoptionGate) await() bool {
	if g == nil {
		return true
	}
	<-g.done
	return g.committed
}

// shimRecordPollInterval is how often the launch path re-reads the registry
// while waiting for the new shim's record.
const shimRecordPollInterval = 25 * time.Millisecond

// shimRecordDiscoveryGraceAttempts bounds how many extra registry reads
// awaitShimRecord makes, with exponential backoff, after the launch
// timeout's own deadline has already passed, before it truly gives up.
//
// defaultShimLaunchTimeout's budget must cover an ordinary harness cold
// start, but some harnesses' first run also does network-bound work (e.g. a
// first-time provider handshake) that can occasionally push discovery a
// little past even a generous budget. Measured live: the daemon gave up
// exactly at the deadline, and the record appeared under the exact expected
// filename for the exact expected identity moments later, with the shim
// process still alive — a session that then hung forever holding capacity
// nobody could ever release. This grace window exists to catch "a hair
// slow", never to turn the launch timeout into a suggestion: it is short,
// independently bounded, and every record it accepts must still pass
// shimDiscoveryRecordMatchesLaunch.
//
// Variables, not constants, only so a test can shrink the wall-clock cost of
// exercising both ends of the bound (a late-but-real record, and a
// never-arriving one) without changing what production ships with.
var (
	shimRecordDiscoveryGraceAttempts = 4
	// shimRecordDiscoveryGraceBaseDelay is the first backoff delay between
	// grace polls; each subsequent attempt doubles it. The FIRST check is
	// immediate (no sleep) and only the three checks after it sleep first, so
	// four attempts doubling from this base sleep 100+200+400 = 700ms of
	// additional wait in the worst case — long enough to catch a discovery
	// record landing "moments" late, short enough that a launch that
	// genuinely produced nothing still fails promptly.
	shimRecordDiscoveryGraceBaseDelay = 100 * time.Millisecond
)

// shimOwnsSession is the §D11 SELECTION rule: which sessions this daemon
// launches under per-session shim ownership.
//
// Two conditions, both required. Ownership must be enabled — §D11 sequences
// shipping the protocol (step 1) ahead of taking ownership of live terminals
// (step 2), and an operator has to be able to roll the release out before it
// changes who owns a PTY. And the session must be INTERACTIVE: the first
// delivery is interactive-only, because that is the session class whose value is
// destroyed by a daemon restart. A headless worker that dies with its daemon is
// re-dispatched; a human's terminal is not.
func (d *Daemon) shimOwnsSession(spec SessionSpec) bool {
	identity := d.shimIdentity()
	if identity.pendingComposition {
		// A composition installed after startup is live for the adoption pass
		// before it is live for ownership: until that pass has accounted for
		// what is already running on this host, a session handed to a shim is
		// a session admitted against capacity nobody has counted. Direct
		// ownership in that window is the same posture the host had a moment
		// earlier, so nothing regresses by waiting.
		return false
	}
	if !identity.config.EnableOwnership {
		return false
	}
	return spec.Mode == interactiveRunMode
}

// SessionShimOwnsSession reports whether this daemon will launch spec under
// per-session shim ownership, right now.
//
// It is exported for the one caller that must not answer this question for
// itself: an embedder whose pre-spawn chain suppresses an interactive rail
// exactly when the shim owns the session. That decision has to agree with this
// one for every session, and the only way to guarantee agreement is to ask
// rather than to mirror. A copy of the rule was correct while the composition
// was resolved before the daemon existed; it is not correct once the
// composition can land mid-flight, because a copy captured at startup answers
// for a posture the daemon has since left.
func (d *Daemon) SessionShimOwnsSession(spec SessionSpec) bool { return d.shimOwnsSession(spec) }

// launchSessionShim is the spawner's shim launch path (SpawnerOptions.ShimSpawn).
//
// It returns (nil, nil) for a session this daemon does not own through a shim,
// which is the signal the spawner reads as "use the ordinary direct-child
// spawn". Returning an error fails the accept.
//
// The sequence is: launch a DETACHED worker carrying the launch contract, let go
// of it entirely, then discover and adopt it over shimwire. The daemon never
// holds the exec.Cmd, the PTY fd, or a *ptyhost.Session (§D1) — after this
// function returns, everything it knows about the session travels over a socket
// it can drop and re-dial.
func (d *Daemon) launchSessionShim(spec SessionSpec, project ProjectConfig, env []string) (*SessionHandle, error) {
	if !d.shimOwnsSession(spec) {
		return nil, nil
	}
	cfg := d.sessionShimConfig()
	if err := cfg.validateSnapshotCarrier(); err != nil {
		return nil, err
	}
	if d.sessionShimAttestationError() != nil {
		return nil, d.sessionShimAttestationError()
	}
	id := sessionshim.Identity{OrgID: cfg.orgIDForSession(spec), SessionID: spec.SessionID}
	if err := id.Validate(); err != nil {
		return nil, fmt.Errorf("session shim: %w", err)
	}
	registry, err := d.sessionShimRegistry()
	if err != nil {
		return nil, err
	}
	if err := cfg.Orphan.Validate(); err != nil {
		return nil, fmt.Errorf("session shim: %w", err)
	}

	layout, err := sessionWorkareaLayout(d.spawner.opts.WorktreeParentDir, spec)
	if err != nil {
		return nil, err
	}
	workarea := layout.Repository.String()
	launch := sessionshim.Launch{
		Identity:     id,
		RegistryDir:  registry.Dir(),
		Orphan:       cfg.Orphan,
		ProcessEpoch: 1,
		// Zero on every ordinary launch. Non-zero only while the acceptance
		// seam is armed, so its burst can actually reach the ring's eviction
		// path instead of missing it by two orders of magnitude.
		RingBytes: acceptanceLaunchRingBytes(),
	}

	started, err := d.startShimProcess(spec, launch, env)
	if err != nil {
		return nil, err
	}
	// The guard runs for as long as the log file exists (guardShimChildLogOnce
	// self-terminates the loop once removeShimChildLog disposes of it) — no
	// cancellation plumbing needed for the ordinary adopted-and-terminated
	// case, where removeShimChildLog runs at the SAME terminal cleanup that
	// withdraws the discovery record and tombstone.
	//
	// launchAdopted covers every OTHER exit from this function: every
	// early-return failure path below this point (awaitShimRecord, Dial,
	// adoption evidence, durable publication, batch commit, revision
	// retention, …) leaves this session NEVER entering d.shims.adopted, so
	// none of finishAdoptedShim / the startup adoption pass / the
	// quarantine reconciliation pass will ever run for it — nothing else
	// would otherwise dispose of this log file or stop this goroutine, ever.
	// Once trackLaunchedShim below hands the session to the ordinary
	// adopted-session lifecycle, this defer becomes a no-op.
	logPath := shimChildLogPath(launch.RegistryDir, launch.Identity)
	launchAdopted := false
	defer func() {
		if !launchAdopted {
			removeShimChildLog(launch.RegistryDir, launch.Identity)
		}
	}()
	go runShimChildLogGuard(logPath)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.launchTimeout())
	defer cancel()

	launchProcess := d.shimLaunchProcessControl(started)
	rec, err := awaitShimRecord(ctx, shimDiscoveryWait{
		registry: registry, id: id, launch: launch, started: started,
		process: launchProcess, liveBound: cfg.liveDiscoveryTimeout(),
	})
	if err != nil {
		// The shim never announced itself — through the ordinary wait, the
		// bounded post-deadline grace poll, or the longer bound the wait holds
		// while the launched process is alive. Nothing is adopted and nothing is
		// counted.
		slog.Error("session shim: launched worker never published a discovery record",
			"session", id.String(), "pid", started.PID, "error", err)
		if errors.Is(err, errShimDiscoveryAbandonedLiveProcess) {
			// The process this daemon started is STILL RUNNING and can never be
			// adopted — the launch never reached trackLaunchedShim, and a worker
			// that published no record never armed sessionshim's own orphan clock
			// either, so §D10's escape hatch does not exist for it. Leaving it
			// alone is what let a measured launch run its entire prompt
			// un-adopted and end defunct. This is not the guess §D10 forbids: the
			// target is not inferred from a registry record of unknown
			// provenance, it is the pid+start-time this daemon pinned when it
			// exec'd the process itself.
			d.stopAbandonedShimLaunch(id, started, launchProcess, err)
		}
		return nil, fmt.Errorf("session shim: %s: %w", id, err)
	}
	if ctx.Err() != nil {
		// The record only arrived through the post-deadline grace poll: ctx's
		// own deadline has already passed. Everything downstream of this point
		// — Dial, the PrepareAdoption closure below, and the adoption-evidence
		// call — either derives its own working context from ctx directly or
		// (PrepareAdoption) closes over this exact variable, so an already-dead
		// parent would make Dial's own DialTimeout moot: context.WithTimeout
		// never outlives an expired parent, and the dial fails immediately
		// regardless of how generous DialTimeout is. Left uncorrected, this
		// would silently convert "never published a discovery record" into
		// "could not adopt the shim it just launched" — the same stranded
		// outcome the grace poll exists to undo, just one step later. Detach
		// and re-arm exactly the way pubCtx is later detached from this same
		// ctx for durable publication, so discovery's late finish gets a live
		// clock to adopt on.
		//
		// callbackTimeout(), not launchTimeout(): what remains — Dial's
		// handshake, one PrepareAdoption round trip, one adoption-evidence
		// resolution — is the same shape of single bounded round trip
		// callbackTimeout() already sizes everywhere else in this file
		// (sessionShimCallbackContext). launchTimeout() is sized for an
		// entire harness cold start, the very budget that already ran out
		// once to get here; reusing it a second time would be generous well
		// past what a live dial and two callback round trips need.
		// callbackTimeout() defaults to launchTimeout() when unset, so this
		// is never SMALLER than what the field already effectively used
		// before this fix — only more proportionate when an embedder
		// configures a tighter CallbackTimeout.
		var graceCancel context.CancelFunc
		ctx, graceCancel = context.WithTimeout(context.WithoutCancel(ctx), cfg.callbackTimeout())
		defer graceCancel()
	}

	var (
		prepared       SessionShimAdoptionPreparationResult
		preparedHostID string
	)
	controllerOpts := sessionshim.ControllerOptions{
		ControllerID:              d.controllerID(),
		EventBacklogBudget:        cfg.EventBacklogBudget,
		EventBacklogStallDeadline: cfg.EventBacklogStallDeadline,
		ExpectedWorkarea:          workarea,
		ExpectedWorkareaRoot:      layout.Root.String(),
		DialTimeout:               cfg.launchTimeout(),
		RequireFullHostFrames:     cfg.RequireAuthoritativeSnapshot && d.sessionShimEnabled(),
		Logger:                    slog.Default(),
		PrepareAdoption: func(evidence sessionshim.AdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			hostID, hostErr := d.sessionShimHostID(ctx, evidence.Identity.OrgID)
			if hostErr != nil {
				return sessionshim.PreparedAdoption{}, hostErr
			}
			resolved, prepareErr := d.prepareSessionShimAdoption(ctx, hostID, evidence)
			if prepareErr != nil {
				return sessionshim.PreparedAdoption{}, prepareErr
			}
			preparedHostID = hostID
			prepared = resolved
			return resolved.PreparedAdoption, nil
		},
	}
	ctrl, err := sessionshim.Dial(ctx, rec, controllerOpts)
	if err != nil {
		slog.Error("session shim: could not adopt the shim it just launched",
			"session", id.String(), "error", err)
		return nil, fmt.Errorf("session shim: adopt %s: %w", id, err)
	}
	evidence, err := d.sessionShimAdoptionEvidence(ctx, ctrl, prepared, preparedHostID)
	if err != nil {
		_ = ctrl.Close()
		return nil, fmt.Errorf("session shim: resolve adoption host %s: %w", id, err)
	}
	gate := newShimAdoptionGate()
	d.consumeShimEventsGated(ctrl, gate)
	// The launch clock (cfg.launchTimeout()) exists to bound discovery and
	// handshake of the freshly launched shim. Everything from here on is the
	// durable adoption publication: a fixed pipeline of composing callbacks,
	// each individually bounded by sessionShimCallbackContext. Running that
	// pipeline on the launch clock's REMAINDER made the wrong stream the
	// binding constraint — a slow discovery handed the batch prepare one or
	// two seconds while the callback's own retry policy still held a full
	// budget, and the whole claim NACKed on a deadline nobody chose for it.
	// The publication gets its own bound, derived from the per-callback bound
	// times the pipeline depth (see adoptionPublicationTimeout).
	pubCtx, cancelPublication := context.WithTimeout(
		context.WithoutCancel(ctx), cfg.adoptionPublicationTimeout())
	defer cancelPublication()
	// ringPostActivationHeartbeat is set only on the path that raised the
	// recovery heartbeat barrier for THIS launch AND completed carrier
	// activation, so the beat below can only ever carry a complete projection.
	// ringCorrectingHeartbeat is its failure twin: a failed serialized
	// publication restores the last-committed projection, and this one beat is
	// what tells the control plane so immediately — the row it cleared for the
	// in-flight attempt would otherwise sit demoted until the next tick.
	//
	// This defer is registered BEFORE the publication barrier below precisely so
	// it runs AFTER that barrier's own deferred unlock — defers are LIFO. The
	// acknowledgement this beat triggers takes the same publication barrier, so
	// ringing it while still holding the barrier would deadlock the launch
	// against its own heartbeat.
	ringPostActivationHeartbeat := false
	ringCorrectingHeartbeat := false
	defer func() {
		if ringPostActivationHeartbeat {
			// Snapshot before the beat: donmai's own acknowledgement clears the
			// scope it owns, and the hook is meant to see the set that activation
			// actually produced.
			activated := d.sessionShimActivatedScopes()
			d.ringSessionShimPostActivationHeartbeat(pubCtx)
			d.notifySessionShimAdoptionActivated(pubCtx, activated)
			return
		}
		if !ringCorrectingHeartbeat {
			return
		}
		// The publication budget may be the very thing that just expired, so
		// the correcting beat rides one fresh callback-sized bound of its own.
		beatCtx, cancelBeat := context.WithTimeout(
			context.WithoutCancel(pubCtx), cfg.callbackTimeout())
		defer cancelBeat()
		d.ringSessionShimPostActivationHeartbeat(beatCtx)
	}()
	serializedPublication := cfg.OnAdoptionPublished != nil
	publicationSucceeded := !serializedPublication
	publicationCommitted := false
	if serializedPublication {
		d.shims.publicationMu.Lock()
		if d.shims.dynamicPublicationFailed {
			d.shims.publicationMu.Unlock()
			evidence.SnapshotProxy.deactivate()
			gate.finish(false)
			_ = ctrl.Close()
			return nil, errors.New("session shim: a prior dynamic adoption publication failed")
		}
		checkpoint := d.checkpointSessionShimPublication(id.OrgID)
		defer func() {
			if !publicationSucceeded {
				if publicationCommitted {
					// The durable batch committed; only the local completion
					// after it failed. The committed revision is real and
					// retained, so admission stays latched closed exactly as
					// before — but the heartbeat lane is restored: a beat that
					// attests the committed truth is the only channel through
					// which this divergence can ever be repaired.
					d.shims.dynamicPublicationFailed = true
					d.restoreSessionShimHeartbeatLane(checkpoint)
				} else {
					// Nothing durable advanced: restore the last-committed
					// projection wholesale. The next beat re-attests the state
					// the control plane last acknowledged, admission reopens on
					// the same base this attempt started from, and the NACK
					// releases the claim for another host.
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
	receipt, err := d.completeSessionShimAdoption(pubCtx, evidence, prepared)
	evidence.SnapshotProxy.deactivate()
	if err != nil {
		d.cancelStagedSessionShimSnapshot(id)
		gate.finish(false)
		_ = ctrl.Close()
		return nil, fmt.Errorf("session shim: durable adoption %s: %w", id, err)
	}
	// SnapshotProxy exists only for the synchronous carrier takeover callback.
	// Published daemon state uses the stable lookup APIs instead.
	evidence.SnapshotProxy = nil
	batchReceipt, err := d.completeLaunchedSessionShimAdoptionBatchResilient(pubCtx, evidence, receipt)
	if err != nil && errors.Is(err, errSessionShimAmbiguousBatchCommit) {
		// OUTCOME-UNKNOWN. This function's contract is a DEFINITE disposition —
		// a live adopted handle, or an error the spawner turns into
		// OnSpawnAborted — so the ambiguity is driven to a definite outcome
		// HERE, before returning, rather than handed to an asynchronous pass
		// while an un-adopted harness keeps running.
		batchReceipt, err = d.redriveAmbiguousLaunchSessionShimBatchCommit(pubCtx, evidence, receipt, err)
		if err != nil {
			// Definitely NOT committed (or never resolvable inside the bound).
			// Release: record and publish the lineage, stop the harness, and
			// consume whatever terminal proof the stop produces, so the error
			// this returns is a true statement about the host.
			d.releaseAmbiguousLaunchSessionShim(pubCtx, ctrl, evidence, err)
		}
	}
	if err != nil {
		d.failPendingSessionShimActivations()
		gate.finish(false)
		_ = ctrl.Close()
		return nil, fmt.Errorf("session shim: durable adoption batch %s: %w", id, err)
	}
	// The batch is durably committed from here on: a later failure must keep
	// the committed revision rather than roll it back — the platform advanced,
	// and a beat re-attesting the superseded revision would argue forever.
	publicationCommitted = true
	if err := d.updateSessionShimAdoptionRevision(id.OrgID, batchReceipt.AdoptionRevision, heartbeatBarrier); err != nil {
		d.failPendingSessionShimActivations()
		gate.finish(false)
		_ = ctrl.Close()
		return nil, fmt.Errorf("session shim: retain adoption revision %s: %w", id, err)
	}
	d.shims.mu.Lock()
	if len(batchReceipt.DurableCorrelation) > 0 {
		d.shims.batchReceipts[id.OrgID] = batchReceipt
	}
	d.shims.mu.Unlock()

	handle := d.trackLaunchedShim(ctrl, spec, project, workarea, layout.Root.String(), evidence, receipt, false)
	// The session is now in d.shims.adopted: its eventual termination runs
	// through finishAdoptedShim (or, across a daemon restart, the startup
	// adoption / quarantine reconciliation passes), which disposes of the
	// log file and — by removing it — stops the guard goroutine started
	// above. Everything from here on can still return a non-nil handle with
	// a nil error even after a "degraded" publication/activation hiccup
	// (see below); none of that changes who now owns this log's lifecycle.
	launchAdopted = true
	gate.finish(true)
	if cfg.OnAdoptionPublished != nil {
		d.shims.mu.RLock()
		entry, publishedCurrent := d.shims.adopted[id]
		d.shims.mu.RUnlock()
		if !publishedCurrent || entry.controller != ctrl {
			d.failPendingSessionShimActivations()
			_ = ctrl.Close()
			return &handle, nil
		}
		published := map[sessionshim.Identity]adoptedShim{id: entry}
		if activationErr := d.activatePublishedSessionShimCarriers(pubCtx, published); activationErr != nil {
			// The harness and durable adoption are already real. Preserve the claim
			// and visible capacity while withholding further claims; returning a
			// launch failure here would invite a duplicate session.
			slog.Error("session shim: post-publication carrier activation failed",
				"session", id.String(), "error", activationErr)
			d.failPendingSessionShimActivations()
			_ = ctrl.Close()
			return &handle, nil
		}
		if !heartbeatBarrier {
			d.setState(StateRunning)
		}
	}
	publicationSucceeded = true
	ringPostActivationHeartbeat = heartbeatBarrier
	slog.Info("session shim: launched and adopted an interactive session",
		"session", id.String(), "shimId", ctrl.Hello().ShimID,
		"generation", ctrl.Generation(), "harnessPid", ctrl.HarnessIdentity().PID)
	return &handle, nil
}

// sessionShimAdoptionBatchCommitAttempts bounds how many times
// completeLaunchedSessionShimAdoptionBatchResilient retries a DEFINITE
// (decoded, non-ambiguous) adoption-batch commit refusal before giving up.
//
// Three, not sessionShimAdoptionPublicationStages: that bound sizes the
// OUTCOME-UNKNOWN reconciliation pipeline (a slower, heavier mechanism —
// pipeline depth times a verified credential refresh per attempt), which
// this function's OWN retries never deliberately invoke for a definite
// refusal — but this function does not fully control its own wall-clock
// budget. It runs under the caller's pubCtx, sized at
// adoptionPublicationTimeout() = 4×callbackTimeout(), which is SMALLER than
// this loop's own worst case (see sessionShimAdoptionBatchCommitBaseBackoff).
// A slow definite refusal can therefore have pubCtx expire mid-retry: the
// in-flight callback then returns a context-deadline-shaped error, which
// sessionShimCommitOutcomeUnknown classifies as ambiguous regardless of what
// caused it, and THAT does engage the heavier reconciliation pipeline this
// function otherwise avoids. That is not a bug — reconciliation resolves it
// correctly either way — but it means "never engages reconciliation" is true
// only while this loop finishes inside pubCtx's budget, not as an absolute
// guarantee. Three same-call, no-refresh, immediate-requery attempts are
// still enough to absorb one lost compare-and-swap race against a concurrent
// writer without turning an already-doomed refusal into a long synchronous
// stall on the accept path.
//
// EACH attempt re-runs the full completeSessionShimAdoptionBatch pipeline,
// including its own PrepareAdoptionBatch call — the SAME destructive prepare
// step whose compare-and-swap clears the host's durably-published readiness
// on every invocation, refusal or not (see this function's THE STRAND THIS
// UNDOES). Retrying is still correct: prepare's demotion is already in
// effect from the FIRST attempt, and only a successful commit (this attempt
// or the exhaustion restore below) ever clears it — but this is not a free
// retry, and the bound above exists precisely so a doomed refusal cannot
// re-trigger prepare indefinitely.
const sessionShimAdoptionBatchCommitAttempts = 3

// sessionShimAdoptionBatchCommitBaseBackoff is the first delay between
// adoption-batch commit retries; each subsequent attempt doubles it, capped
// by the callback timeout.
//
// The CAP is one callback timeout, not the whole retry's total cost: each of
// the sessionShimAdoptionBatchCommitAttempts attempts itself spends up to two
// callback timeouts (PrepareAdoptionBatch, then OnAdoptionBatch), so this
// loop's OWN worst-case total is roughly attempts×2 callback timeouts for the
// attempts themselves, plus up to (attempts-1) capped backoff sleeps between
// them — up to six callback-timeout-equivalents for the default three
// attempts, run inside a pubCtx budget of only FOUR (see
// completeLaunchedSessionShimAdoptionBatchResilient's doc comment for what
// happens when the difference matters).
const sessionShimAdoptionBatchCommitBaseBackoff = 100 * time.Millisecond

// completeLaunchedSessionShimAdoptionBatchResilient wraps
// completeLaunchedSessionShimAdoptionBatch with the retry-then-restore
// discipline a live incident proved this daemon needed.
//
// THE STRAND THIS UNDOES: the control plane's adoption-batch compare-and-swap
// clears the host's durably-published readiness the moment PrepareAdoptionBatch
// resolves a new expected revision — restoring it is the COMMIT's job, not the
// prepare's. Measured live: the commit that followed one prepare came back
// HTTP 409, this daemon surfaced that as an immediately fatal launch failure,
// and the host was left exactly as demoted as the moment prepare ran — every
// later poll refused with the control plane's durable-publication gate until
// an operator restarted the daemon. A fresh boot's own §D4 pass re-ran
// prepare+commit from scratch and committed cleanly, proving the condition
// was transient and entirely recoverable without ever leaving process memory.
//
// (a) A DEFINITE refusal — a decoded answer the control plane returned,
// classified by the same sessionShimCommitOutcomeUnknown predicate the
// ambiguous-outcome reconciliation subsystem already uses — is retried under
// bounded exponential backoff. Each retry calls
// completeLaunchedSessionShimAdoptionBatch again from scratch, which (via
// completeSessionShimAdoptionBatch's own PrepareAdoptionBatch call) re-reads
// the control plane's current expected revision rather than resending the
// stale one that just lost a race.
//
// An AMBIGUOUS outcome (the control plane may already have committed) is
// NEVER retried here: synchronously resending a guessed revision would race
// the one mechanism that can safely resolve it — the existing bounded
// reconciliation pass in session_shim_reconcile.go, which only ever learns
// the true current revision through the credential refresher before
// republishing. This function returns an ambiguous failure immediately, on
// whichever attempt first produces one, so its caller's existing
// scheduleSessionShimReconciliation call fires exactly as it always has —
// this wrapper changes nothing about that classification or that path.
//
// (b) On exhausting every retry of a definite refusal, this makes ONE
// best-effort attempt to commit the host's actual current truth — everything
// genuinely already adopted, quarantined, or tombstoned, WITHOUT the new
// session this launch could not durably publish — so a host that cannot
// publish one new arrival is not left durably-unpublished for every session
// already running on it. The attempt is unretried, and its own failure only
// widens the log line: this function still reports the ORIGINAL launch
// failure to its caller either way, because the new session's adoption
// genuinely did not durably commit.
//
// (c) Together: a single refused batch — the definite-refusal case this bug
// was measured on — can no longer be silently fatal to the rest of the
// host's ability to claim.
func (d *Daemon) completeLaunchedSessionShimAdoptionBatchResilient(
	ctx context.Context,
	evidence SessionShimAdoptionEvidence,
	receipt SessionShimAdoptionReceipt,
) (SessionShimAdoptionBatchReceipt, error) {
	var lastErr error
	backoff := sessionShimAdoptionBatchCommitBaseBackoff
	backoffCap := d.sessionShimConfig().callbackTimeout()
	for attempt := 1; attempt <= sessionShimAdoptionBatchCommitAttempts; attempt++ {
		if attempt > 1 {
			if !sleepSessionShimAdoptionBatchBackoff(ctx, backoff) {
				// ctx ended mid-backoff. lastErr already holds the real
				// refusal from the attempt just made — that string is what
				// an operator diagnoses from, so it is deliberately NOT
				// overwritten with ctx.Err() here; a cut-short backoff is not
				// itself a new failure, just a reason to stop retrying early.
				slog.Warn("session shim: adoption batch commit retry backoff was cut short; exhausting with the last refusal",
					"session", evidence.Identity.String(), "attempt", attempt, "error", lastErr)
				break
			}
			backoff *= 2
			if backoff > backoffCap {
				backoff = backoffCap
			}
		}
		batchReceipt, err := d.completeLaunchedSessionShimAdoptionBatch(ctx, evidence, receipt)
		if err == nil {
			return batchReceipt, nil
		}
		lastErr = err
		if errors.Is(err, errSessionShimAmbiguousBatchCommit) {
			return SessionShimAdoptionBatchReceipt{}, err
		}
		slog.Warn("session shim: adoption batch commit was refused; retrying with freshly re-read authority",
			"session", evidence.Identity.String(), "attempt", attempt,
			"attempts", sessionShimAdoptionBatchCommitAttempts, "error", err)
	}
	d.restoreSessionShimReadinessAfterExhaustedBatchCommit(ctx, evidence, lastErr)
	return SessionShimAdoptionBatchReceipt{}, lastErr
}

// sleepSessionShimAdoptionBatchBackoff waits d, or returns false early when
// ctx ends first.
func sleepSessionShimAdoptionBatchBackoff(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// restoreSessionShimReadinessAfterExhaustedBatchCommit makes one best-effort
// attempt to commit the host's actual current truth after every retry of a
// NEW session's batch commit has been definitively refused — see
// completeLaunchedSessionShimAdoptionBatchResilient's doc comment for why
// this is the best available repair with no separate "cancel prepare"
// primitive on offer. It never returns an error: the outcome is only logged,
// loudly, alongside the exhausted commit's own last error, so an operator can
// see both what failed and whether the host recovered on its own.
//
// This lineage is recorded into d.shims.quarantined BEFORE anything is sent
// — not merely appended to the one outgoing batch — and that ordering is the
// whole fix for a stranding bug measured in review: by the time every retry
// above has run, d.completeSessionShimAdoption (called before this
// function's caller ever reaches the batch commit) has already succeeded for
// THIS lineage, so the control plane holds a per-session adoption record for
// it — live, independent of whatever batch commit keeps failing, and
// EXPECTED IN EVERY BATCH FROM NOW ON, not just this one. A version that
// appended the quarantine only to this one outgoing batch (mirroring what
// looked like the same complete-snapshot fix fix 3 applies at startup, but
// without fix 3's upsertShimQuarantineLocked half) left d.shims.quarantined
// never knowing about it: the very next batch — a sibling launch, a
// republish, anything — went back to composing from
// d.sessionShimProjectionBatch alone, omitted this lineage again, and was
// refused again. Recording it here first means sessionShimProjectionBatch
// picks it up on its own for this attempt AND every attempt after it, self--
// healing on the next opportunity even when THIS restore also fails, and
// letting reconcileQuarantinedTombstones (which iterates exactly
// d.shims.quarantined) eventually clear it once the shim's own orphan clock
// produces a terminal tombstone — the same path every other quarantined
// lineage leaves through. Never simply omitting this lineage, in whichever
// batch attempt runs next, is the actual complete-snapshot discipline; a
// one-shot append to a single batch was not it.
//
// It runs on a FRESH callback-sized budget detached from ctx's own deadline,
// mirroring the correcting-heartbeat idiom elsewhere in this file: by the
// time every retry above has been spent, ctx may have little or nothing left,
// and this best-effort repair deserves its own full attempt rather than
// whatever remainder happens to survive it.
//
// The OUTCOME-UNKNOWN twin of the recording half is
// recordAmbiguousLaunchBatchQuarantine — same complete-snapshot obligation,
// arrived at from the other side of the same round trip.
func (d *Daemon) restoreSessionShimReadinessAfterExhaustedBatchCommit(
	ctx context.Context,
	evidence SessionShimAdoptionEvidence,
	causeErr error,
) {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.sessionShimConfig().callbackTimeout())
	defer cancel()
	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(sessionshim.QuarantinedSession{
		OrgID: evidence.Identity.OrgID, SessionID: evidence.Identity.SessionID,
		ShimID: evidence.ShimID, ProcessEpoch: evidence.ProcessEpoch,
		ControllerGeneration: evidence.ControllerGeneration,
		Reason:               sessionshim.QuarantineAdoptionFailed,
		Detail:               "adoption batch commit exhausted its retries; already durably adopted server-side, presented quarantined pending a successful batch commit",
		ConsumesCapacity:     true,
	})
	d.shims.mu.Unlock()
	// sessionShimProjectionBatch now includes the entry recorded above — the
	// outgoing batch never needs a separate manual append, and neither will
	// any later batch this daemon ever sends for this scope.
	fallback := d.sessionShimProjectionBatch(evidence.Identity.OrgID, evidence.HostID)
	receipt, err := d.completeSessionShimAdoptionBatch(restoreCtx, fallback)
	if err != nil {
		slog.Error("session shim: adoption batch commit exhausted its retries and the best-effort readiness restore also failed; "+
			"the lineage stays quarantined in this daemon's own live projection so no later batch silently omits it — nothing "+
			"here schedules a fresh publish attempt, so recovery waits on the shim's own orphan deadline producing a terminal "+
			"tombstone for reconcileQuarantinedTombstones to clear",
			"session", evidence.Identity.String(), "commitError", causeErr, "restoreError", err)
		return
	}
	// The restore batch committed durably, which means the control plane's
	// adoption revision just advanced — exactly like any other successful
	// batch commit (see republishSessionShimProjection, which retains its
	// own receipt for the identical reason). Retaining nothing here would
	// leave this daemon attesting the STALE pre-restore revision on its very
	// next beat, which the control plane answers
	// SESSION_SHIM_ADOPTION_REVISION_STALE — demoting the host all over
	// again, the same divergence session_shim_reconcile.go's own doc comment
	// warns a republish that skips this step trades one divergence for
	// another.
	if revisionErr := d.updateSessionShimAdoptionRevision(evidence.Identity.OrgID, receipt.AdoptionRevision, false); revisionErr != nil {
		slog.Error("session shim: adoption batch commit exhausted its retries; the best-effort readiness restore committed but "+
			"its revision was not retained — the next beat may present a stale revision until reconciliation relearns it",
			"session", evidence.Identity.String(), "commitError", causeErr, "revisionError", revisionErr)
		return
	}
	slog.Error("session shim: adoption batch commit exhausted its retries; restored the host's last-known-good durable "+
		"projection (with this lineage presented quarantined, pending a successful commit) so the rest of its sessions can keep claiming work",
		"session", evidence.Identity.String(), "commitError", causeErr)
}

// ambiguousLaunchBatchQuarantineDetail is the quarantine detail an
// outcome-unknown launch batch commit leaves on the lineage. It says what is
// and is not known, because it is the string an operator reads off the host's
// own projection while the ambiguity is still open.
const ambiguousLaunchBatchQuarantineDetail = "adoption batch commit outcome was never learned (transport, deadline or 5xx after the " +
	"request went out); already durably adopted server-side and no longer running here, presented quarantined until a " +
	"reconciliation republish commits a complete snapshot that includes it"

// recordAmbiguousLaunchBatchQuarantine records the launching lineage into this
// daemon's own live projection after an adoption-batch commit whose outcome was
// never learned — the OUTCOME-UNKNOWN twin of the recording half of
// restoreSessionShimReadinessAfterExhaustedBatchCommit, and the step whose
// absence was measured live.
//
// # THE STRAND THIS UNDOES
//
// By the time a launch reaches the batch commit, d.completeSessionShimAdoption
// has ALREADY succeeded for this lineage: the control plane holds a live
// per-session adoption record for it, independent of any batch, and refuses
// every later batch that omits it (adoption_batch_live_lineage_omitted). That
// is true whether the lost answer was a commit or a refusal — the obligation
// comes from the per-session adoption, not from the batch.
//
// The caller's rollback then restores the LAST-COMMITTED projection, which by
// construction cannot contain a session whose adoption never finished, and
// trackLaunchedShim has not run, so d.shims.adopted does not hold it either.
// Without this call the lineage exists nowhere in this daemon's state: every
// batch it can compose from here on omits it and is refused — INCLUDING every
// reconciliation republish, which is the one mechanism that could resolve the
// ambiguity. Measured end state: bounded reconciliation exhausting against a
// completeness rule no retry could satisfy, no later launch able to commit
// either, and a session left in its pre-running state with no process on the
// host and nothing to release it.
//
// Recording it here makes the projection composable again, and therefore makes
// the ambiguity RESOLVABLE: the scheduled reconciliation pass relearns the
// control plane's committed revision through the one credential refresher and
// republishes a complete snapshot that presents this lineage as quarantined —
// which is the truth, because the caller closes its controller on this path.
// The entry consumes capacity until a terminal tombstone for the exact
// incarnation proves the harness group was reaped, at which point
// reconcileQuarantinedTombstones clears it through the same door every other
// quarantined lineage leaves by.
//
// redriveAmbiguousLaunchSessionShimBatchCommit turns an outcome-unknown batch
// commit into a DEFINITE one, synchronously, before the launch returns.
//
// # WHY IT HAS TO BE SYNCHRONOUS
//
// By this point sessionshim.Start has already exec'd the harness. The daemon's
// only teardown on the failure path is a controller Close, which explicitly
// does NOT stop the session — the shim keeps its harness and starts its bounded
// orphan clock. So an ambiguity handed to an asynchronous pass leaves a real
// harness running, un-adopted and unreachable (the launch never reached
// trackLaunchedShim, so no adopted-set pass can find it), until that clock
// expires — while the spawner has already reported the launch aborted. The
// abort has to be TRUE when it is reported, and that means resolving here.
//
// # WHY RE-SENDING IS SAFE HERE, WHERE THE RESILIENT RETRY REFUSES TO
//
// completeLaunchedSessionShimAdoptionBatchResilient never retries an ambiguous
// outcome, because a retry inside ITS loop would resend the expected revision
// that just lost — a guess. This is a different operation: each attempt
// re-runs completeLaunchedSessionShimAdoptionBatch from scratch, which
// re-composes the COMPLETE current projection and re-reads the control plane's
// expected revision through PrepareAdoptionBatch. Nothing is guessed, the batch
// digest is the same idempotency key, and a control plane that already
// committed answers the prepare with its own advance — which
// adoptAdvancedSessionShimAdoptionRevision recognises and adopts, resolving the
// ambiguity as COMMITTED without a second commit.
//
// # ONE ATTEMPT, ONE STAGE BUDGET — AND WHY IT IS NOT MORE
//
// This runs on the sequential accept goroutine, holding publicationMu. Every
// second spent here is a second in which no sibling launch can publish, the
// worker polls nothing, and AcknowledgeSessionShimRecoveryHeartbeat blocks on
// the same mutex. An earlier draft gave this a whole adoptionPublicationTimeout
// (four stages) with four attempts and backoff between them; at production
// defaults that is two minutes of a shared goroutine to answer a question a
// bounded ASYNCHRONOUS pass already exists to answer.
//
// So the bound is ONE attempt on ONE callbackTimeout — one stage, the unit
// every other bound in this subsystem is expressed in — and it is detached from
// the caller's own budget, because the deadline that just expired may BE the
// caller's. One attempt is the honest count for one stage: an attempt is a
// prepare plus a commit, and a budget that cannot hold two of them should not
// pretend to schedule four. Everything past the first ask belongs to
// scheduleSessionShimReconciliation, which is already bounded, already derived,
// and already off this goroutine.
//
// # COST IN PREPARES
//
// PrepareAdoptionBatch's compare-and-swap clears the host's durably-published
// readiness on EVERY invocation, refusal or not, and only a successful commit
// restores it — so the number of prepares a single launch can trigger is a real
// cost, not bookkeeping. On the worst path that reaches here it is six:
//
//	3  completeLaunchedSessionShimAdoptionBatchResilient's retries — two
//	   definite refusals, then the ambiguous answer that sends us here
//	1  this re-drive
//	1  releaseAmbiguousLaunchSessionShim's quarantine publish
//	1  the detached discharge's republish
//
// plus whatever a reconciliation pass adds afterwards. A seventh — that retry
// loop's own exhaustion restore — cannot join them: it runs only when the loop
// exhausts on a DEFINITE refusal, and the definite error it then returns never
// enters this path. Each of the six is paid only after the previous one
// produced no definite answer, and every one of them ends in a commit attempt
// that would restore readiness.
//
// Outcomes, each with its own log line:
//
//   - nil error: the control plane holds this session adopted at a revision
//     this daemon now has. The caller continues the normal adoption.
//   - a DEFINITE refusal: the control plane did not commit, and said so.
//   - still ambiguous after the attempt: never resolved inside the bound.
//     Returned as a failure, because a launch that cannot prove it was adopted
//     must not be reported as adopted, and the caller's release makes that true.
func (d *Daemon) redriveAmbiguousLaunchSessionShimBatchCommit(
	ctx context.Context,
	evidence SessionShimAdoptionEvidence,
	receipt SessionShimAdoptionReceipt,
	causeErr error,
) (SessionShimAdoptionBatchReceipt, error) {
	redriveCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), d.sessionShimConfig().callbackTimeout())
	defer cancel()
	batchReceipt, err := d.completeLaunchedSessionShimAdoptionBatch(redriveCtx, evidence, receipt)
	switch {
	case err == nil:
		slog.Info("session shim: outcome-unknown adoption batch commit resolved as COMMITTED; continuing the adoption "+
			"(shim-ambiguous-launch-lineage-2026-09-02)",
			"session", evidence.Identity.String(), "revision", batchReceipt.AdoptionRevision, "commitError", causeErr)
		return batchReceipt, nil
	case !sessionShimCommitOutcomeUnknown(err):
		slog.Warn("session shim: outcome-unknown adoption batch commit resolved as NOT COMMITTED by a decoded refusal; "+
			"releasing this launch (shim-ambiguous-launch-lineage-2026-09-02)",
			"session", evidence.Identity.String(), "commitError", causeErr, "refusal", err)
	default:
		slog.Warn("session shim: outcome-unknown adoption batch commit was not resolved by its one re-drive; releasing this "+
			"launch rather than reporting an adoption this daemon cannot prove, and leaving the rest to the bounded "+
			"reconciliation pass (shim-ambiguous-launch-lineage-2026-09-02)",
			"session", evidence.Identity.String(), "commitError", causeErr, "redriveError", err)
	}
	return SessionShimAdoptionBatchReceipt{}, err
}

// releaseAmbiguousLaunchSessionShim makes the launch failure a TRUE statement
// about this host before the error is returned and the spawner reports the
// spawn aborted. It is the SYNCHRONOUS half; the waiting half is handed to
// startAmbiguousLaunchSessionShimDischarge.
//
// Three things have to be true for that report to be honest, and none of them
// happens on its own:
//
//  1. The lineage is presented in this daemon's projection and published.
//     Recording alone is not enough — an unpublished quarantine change makes
//     every later beat disagree with the last committed batch — and omission is
//     not an option either, because the control plane holds this lineage live
//     from the launch's own per-session adoption. Done here, before returning.
//  2. The harness is STOPPED. A controller Close leaves it running to its
//     orphan deadline; the generation-fenced Stop is the verb that asks the
//     shim to terminate and reap its harness group. Without it "the spawn was
//     aborted" is false for as long as that clock runs. Done here: Stop is a
//     single frame write, not a round trip, so it costs the accept goroutine
//     nothing measurable. The caller's Close that follows does not lose it —
//     a closed stream socket still delivers what was already written.
//  3. Whatever terminal proof the stop produces is CONSUMED. That is a WAIT —
//     the shim has to terminate, reap its group and publish a tombstone — and a
//     wait is precisely what must not happen here.
//
// # WHY (3) IS NOT DONE ON THIS GOROUTINE
//
// This runs on the sequential accept goroutine holding publicationMu. The
// tombstone wait is sized by acceptanceClearDeadlineFor, which at production
// defaults is over a minute; an earlier draft spent it right here. For that
// whole minute no sibling launch could publish (they block on publicationMu),
// the worker polled nothing, AcknowledgeSessionShimRecoveryHeartbeat blocked on
// the same mutex, and the local control route that asked for this session could
// not be answered. None of that is needed to make the abort true: the lineage
// is already published as quarantined and capacity-consuming, so the control
// plane's view is correct and conservative from the moment this returns. The
// discharge only makes it BETTER, and it can do that from anywhere.
//
// The stop reason is the closed registry's policy value: this is the daemon's
// own policy decision that a session it cannot durably adopt must not run here.
// Neither "operator" nor "host_shutdown" is true, and the registry has no
// closer word.
func (d *Daemon) releaseAmbiguousLaunchSessionShim(
	ctx context.Context,
	ctrl *sessionshim.Controller,
	evidence SessionShimAdoptionEvidence,
	causeErr error,
) {
	if !d.recordAmbiguousLaunchBatchQuarantine(ctx, evidence, causeErr) {
		d.scheduleSessionShimReconciliation(evidence.Identity.OrgID, sessionShimReconcileCauseAmbiguousLaunch)
	}
	if ctrl == nil {
		slog.Warn("session shim: no controller to stop after an unresolved commit; the lineage stays published quarantined "+
			"and consuming capacity until a terminal proof for it appears",
			"session", evidence.Identity.String(), "commitError", causeErr)
		return
	}
	if stopErr := ctrl.Stop(shimwire.StopPolicy); stopErr != nil {
		slog.Warn("session shim: could not ask the un-adopted shim to stop after an unresolved commit; it holds its "+
			"harness until its own orphan deadline",
			"session", evidence.Identity.String(), "error", stopErr)
		return
	}
	d.startAmbiguousLaunchSessionShimDischarge(evidence, causeErr)
}

// startAmbiguousLaunchSessionShimDischarge waits out the stopped shim's own
// terminal proof and republishes, on a bounded goroutine of its own.
//
// It consumes the proof through exactly the production reconcile —
// reconcileQuarantinedTombstones reports the group-reaped tombstone for the
// exact incarnation, drops the quarantine, and the lineage leaves through the
// same door every other quarantined lineage leaves by. Nothing is manufactured:
// a tombstone this daemon did not observe would forge the reap proof a claim
// release depends on, so a stop whose proof never lands inside the bound leaves
// the lineage exactly where the published projection already has it —
// quarantined and consuming capacity — and says so.
//
// # THE REPUBLISH IS ITS OWN OPERATION, ON ITS OWN BUDGET
//
// The reconcile deliberately does not publish (that would put a durable commit
// inside every occupancy and heartbeat surface, including the middle of a
// beat's own projection build), so the republish belongs here — and it must be
// a REAL attempt, not a leftover. An earlier draft passed the launch's own
// pubCtx down to a fire-and-forget publishSessionShimProjection: by then the
// re-drive had burned that budget, so sessionShimCallbackContext was born
// already expired, PrepareAdoptionBatch failed with a deadline, the wrapper
// discarded the error, and a plain DeadlineExceeded arms no reconciliation
// (only a revision advance or an outcome-unknown commit does). The success line
// was logged anyway. End state: the last committed batch said the lineage was
// quarantined while the beat said nothing was, and the platform demoted the
// host to draining — sticky on an otherwise idle host until a restart. So the
// budget is fresh and detached, the error is CHECKED, and a failure arms the
// reconciliation pass before anything is logged.
func (d *Daemon) startAmbiguousLaunchSessionShimDischarge(
	evidence SessionShimAdoptionEvidence,
	causeErr error,
) {
	d.shims.mu.Lock()
	if d.shims.reconcileStopped {
		d.shims.mu.Unlock()
		return
	}
	d.shims.wg.Add(1)
	d.shims.mu.Unlock()
	go func() {
		defer d.shims.wg.Done()
		discharged := d.awaitAmbiguousLaunchSessionShimDischarge(evidence, causeErr)
		d.shims.mu.RLock()
		after := d.shims.afterAmbiguousLaunchDischarge
		d.shims.mu.RUnlock()
		if after != nil {
			after(evidence.Identity, discharged)
		}
	}()
}

// awaitAmbiguousLaunchSessionShimDischarge is the discharge body. It reports
// whether the obligation was discharged inside the bound.
func (d *Daemon) awaitAmbiguousLaunchSessionShimDischarge(
	evidence SessionShimAdoptionEvidence,
	causeErr error,
) bool {
	incarnation := shimIncarnation{
		identity: evidence.Identity, shimID: evidence.ShimID, processEpoch: evidence.ProcessEpoch,
	}
	// The same bound and the same pacing the acceptance clear uses to wait out
	// this exact handoff: the shim publishes its tombstone before it withdraws
	// its record, and the reconcile that consumes it costs two callback round
	// trips.
	deadline := time.Now().Add(acceptanceClearDeadlineFor(d.sessionShimConfig().callbackTimeout()))
	for {
		d.reconcileQuarantinedTombstones()
		quarantined, tombstoned := d.sessionShimLineageDisposition(incarnation)
		if !quarantined && tombstoned {
			d.republishAfterAmbiguousLaunchDischarge(evidence, causeErr)
			return true
		}
		if !time.Now().Before(deadline) {
			slog.Warn("session shim: released the un-adopted launch, but no terminal proof landed inside the bound; the "+
				"lineage stays quarantined in the published projection and the next reconcile discharges it when its "+
				"tombstone appears",
				"session", evidence.Identity.String(), "commitError", causeErr)
			return false
		}
		if !d.sleepSessionShimReconcileBackoff(acceptanceClearPollInterval) {
			// The daemon released its shims mid-wait. The lineage is still
			// published quarantined, which is the conservative truth.
			return false
		}
	}
}

// republishAfterAmbiguousLaunchDischarge publishes the projection the discharge
// just changed, on a fresh detached budget, and says what actually happened.
func (d *Daemon) republishAfterAmbiguousLaunchDischarge(
	evidence SessionShimAdoptionEvidence,
	causeErr error,
) {
	publishCtx, cancel := context.WithTimeout(context.Background(), d.sessionShimConfig().callbackTimeout())
	defer cancel()
	if err := d.republishSessionShimProjection(publishCtx, evidence.Identity.OrgID); err != nil {
		// Arm the repair BEFORE saying anything: republishSessionShimProjection
		// arms reconciliation itself only for a revision advance or an
		// outcome-unknown commit, and the failure that stranded a host here was
		// neither.
		d.scheduleSessionShimReconciliation(evidence.Identity.OrgID, sessionShimReconcileCauseAmbiguousLaunch)
		slog.Warn("session shim: released the un-adopted launch and consumed its terminal proof, but the republish that "+
			"retires it did not land; reconciliation is armed to republish the complete snapshot, and until it does the "+
			"last committed batch still presents this lineage quarantined",
			"session", evidence.Identity.String(), "commitError", causeErr, "republishError", err)
		return
	}
	slog.Info("session shim: released the un-adopted launch and discharged its recovery obligation through the shim's "+
		"own terminal tombstone (shim-ambiguous-launch-lineage-2026-09-02)",
		"session", evidence.Identity.String(), "commitError", causeErr)
}

// recordAmbiguousLaunchBatchQuarantine records the launching lineage into this
// daemon's own live projection after an adoption-batch commit whose outcome was
// never learned, and publishes it — the OUTCOME-UNKNOWN twin of the recording
// half of restoreSessionShimReadinessAfterExhaustedBatchCommit, and the step
// whose absence was measured live.
//
// # THE STRAND THIS UNDOES
//
// By the time a launch reaches the batch commit, d.completeSessionShimAdoption
// has ALREADY succeeded for this lineage: the control plane holds a live
// per-session adoption record for it, independent of any batch, and refuses
// every later batch that omits it (adoption_batch_live_lineage_omitted). That
// is true whether the lost answer was a commit or a refusal — the obligation
// comes from the per-session adoption, not from the batch.
//
// The caller's rollback then restores the LAST-COMMITTED projection, which by
// construction cannot contain a session whose adoption never finished, and
// trackLaunchedShim has not run, so d.shims.adopted does not hold it either.
// Without this call the lineage exists nowhere in this daemon's state: every
// batch it can compose from here on omits it and is refused — INCLUDING every
// reconciliation republish, which is the one mechanism that could resolve the
// ambiguity, and including reconcileQuarantinedTombstones, which iterates
// exactly the quarantine set and so can never consume the shim's own eventual
// tombstone. Measured end state: no later launch able to commit either, and a
// session left in its pre-running state with no process on the host, its
// recovery obligation never discharged and nothing left to release it.
//
// Quarantining is correct under BOTH resolutions of the ambiguity. If the
// control plane committed the batch, it holds this lineage adopted at a
// revision this daemon never learned, and this publish corrects that adopted
// entry to the quarantine the host can actually back. If it did not commit, the
// per-session adoption record still stands and this is the first batch that
// presents it at all. Neither outcome is guessed: only server-issued revisions
// are ever retained.
//
// # THE PUBLISH IS PART OF THE RECORDING, NOT AN OPTIMIZATION
//
// The platform compares every beat's quarantine set against the snapshot the
// last committed batch stored and demotes a host whose beat disagrees, so a
// quarantine recorded and left unpublished trades this bug for a draining
// host (TestEveryQuarantineMutationPublishes is the invariant).
//
// It returns whether the publish landed. On false the caller arms the bounded
// reconciliation pass, which relearns the revision through the ONE credential
// refresher and republishes the same complete snapshot — now composable,
// because this function recorded the lineage before returning.
func (d *Daemon) recordAmbiguousLaunchBatchQuarantine(
	ctx context.Context,
	evidence SessionShimAdoptionEvidence,
	causeErr error,
) bool {
	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(sessionshim.QuarantinedSession{
		OrgID: evidence.Identity.OrgID, SessionID: evidence.Identity.SessionID,
		ShimID: evidence.ShimID, ProcessEpoch: evidence.ProcessEpoch,
		ControllerGeneration: evidence.ControllerGeneration,
		Reason:               sessionshim.QuarantineAdoptionFailed,
		Detail:               ambiguousLaunchBatchQuarantineDetail,
		ConsumesCapacity:     true,
	})
	d.shims.mu.Unlock()
	slog.Warn("session shim: adoption batch commit outcome unknown; recorded the launching lineage quarantined so every later "+
		"batch presents it and the commit can be driven to a definite outcome "+
		"(shim-ambiguous-launch-lineage-2026-09-02)",
		"session", evidence.Identity.String(), "shimId", evidence.ShimID,
		"processEpoch", evidence.ProcessEpoch, "commitError", causeErr)
	// A fresh callback-sized budget detached from ctx's deadline, mirroring
	// restoreSessionShimReadinessAfterExhaustedBatchCommit: the publication
	// budget may be the very thing that just expired, and this repair deserves
	// its own full attempt rather than whatever remainder survives.
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.sessionShimConfig().callbackTimeout())
	defer cancel()
	batch := d.sessionShimProjectionBatch(evidence.Identity.OrgID, evidence.HostID)
	receipt, err := d.completeSessionShimAdoptionBatch(publishCtx, batch)
	if err != nil {
		slog.Warn("session shim: the immediate republish after an outcome-unknown commit did not land; "+
			"bounded reconciliation now owns driving it to a definite outcome",
			"session", evidence.Identity.String(), "commitError", causeErr, "republishError", err)
		return false
	}
	if revisionErr := d.updateSessionShimAdoptionRevision(evidence.Identity.OrgID, receipt.AdoptionRevision, false); revisionErr != nil {
		// The republish committed but its revision was not retained, so the
		// next beat would present a superseded one. Reconciliation relearns it
		// through the refresher — the same repair a republish that skips this
		// step needs anywhere else in this subsystem.
		slog.Warn("session shim: the immediate republish after an outcome-unknown commit landed but its revision was not retained",
			"session", evidence.Identity.String(), "commitError", causeErr, "revisionError", revisionErr)
		return false
	}
	slog.Info("session shim: outcome-unknown adoption batch commit resolved by an immediate republish; the control plane now "+
		"holds a complete snapshot presenting this lineage quarantined, and no reconciliation pass was needed",
		"session", evidence.Identity.String(), "revision", receipt.AdoptionRevision, "commitError", causeErr)
	return true
}

// ringSessionShimPostActivationHeartbeat sends one immediate heartbeat after a
// dynamically published adoption has completed carrier activation.
//
// The barrier this launch raised clears only on an acknowledged beat. Left to
// the periodic ticker that acknowledgement arrives up to a full heartbeat
// interval late, and for that whole window a host that is completely ready
// claims no new work and does not read as adoption-complete to its control
// plane. Ringing the beat here collapses that window to one round-trip.
//
// Best-effort on purpose: a failed beat is a warning, never a launch failure.
// The session is already adopted and durable, and the periodic loop clears the
// barrier on its next tick regardless — this only shortens the wait.
func (d *Daemon) ringSessionShimPostActivationHeartbeat(ctx context.Context) {
	d.lifecycleMu.Lock()
	heartbeat := d.heartbeat
	d.lifecycleMu.Unlock()
	if heartbeat == nil {
		return
	}
	if err := heartbeat.SendNow(ctx); err != nil {
		slog.Warn("session shim: immediate post-activation heartbeat failed; recovery clears on the next tick",
			"error", err)
	}
}

// shimChildLogPath returns the per-session log file path a launched shim's
// stdout/stderr is captured to: <registryDir>/<id.LogName()> — alongside
// that same session's discovery record and adoption socket
// (sessionshim.Registry), which is already the exact directory production
// resolves via defaultShimRegistryDir/statepath.Resolve and tests override
// via SessionShimConfig.RegistryDir, so this needs no separate state-home
// seam or test-only override of its own. The filename is the SAME
// fixed-length digest convention as every other sibling artifact under this
// directory (Identity.RecordName/SocketName/TombstoneName) rather than the
// raw session id — see sessionshim/identity.go's doc comments for why.
func shimChildLogPath(registryDir string, id sessionshim.Identity) string {
	return filepath.Join(registryDir, id.LogName())
}

// startShimProcess execs the worker as a detached shim and then RELEASES it.
//
// Release is the ownership move made concrete. os/exec would otherwise leave the
// daemon as the process's parent and waiter, which is precisely the coupling
// §D1 removes: a daemon that still had to reap this process could not be
// replaced without ending it.
func (d *Daemon) startShimProcess(spec SessionSpec, launch sessionshim.Launch, env []string) (sessionshim.ProcessIdentity, error) {
	command := d.shimCommand()
	if len(command) == 0 {
		return sessionshim.ProcessIdentity{}, errors.New("session shim: no worker command is configured to launch a shim with")
	}
	cmd := exec.Command(command[0], command[1:]...) //nolint:gosec // G204: operator-configured worker command, same source as the direct-spawn path
	configureShimProcess(cmd)
	cmd.Env = append(append([]string(nil), env...), envPairs(launch.Env())...)

	// A shim outlives this daemon, so it cannot inherit this daemon's stdio
	// via a pipe THIS process reads: a closed pipe after the daemon exits
	// would hand the shim EPIPE on its own logging (the constraint a plain
	// os.Pipe()-and-forward, worker_spawner.go-style approach cannot honor
	// here). stdin still gets the null device — nothing ever writes to a
	// detached shim's stdin. stdout/stderr are instead duped into a
	// per-session log FILE: unlike a pipe, a file fd stays valid for the
	// child's entire lifetime regardless of whether this daemon process is
	// still around to hold the other end, so every runner/provider error the
	// shim child would otherwise silently swallow (including the exact
	// class of tool/lifecycle adaptation refusal
	// repository_sandbox_reconcile.go's doc comment describes) is now
	// captured rather than discarded into /dev/null.
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return sessionshim.ProcessIdentity{}, fmt.Errorf("session shim: open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()
	logPath := shimChildLogPath(launch.RegistryDir, launch.Identity)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return sessionshim.ProcessIdentity{}, fmt.Errorf("session shim: create log directory for %s: %w", logPath, err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G302: session-scoped operator diagnostics, same 0o600 convention as the rest of daemon/
	if err != nil {
		return sessionshim.ProcessIdentity{}, fmt.Errorf("session shim: open %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, logFile, logFile

	if err := cmd.Start(); err != nil {
		return sessionshim.ProcessIdentity{}, fmt.Errorf("session shim: start %s: %w", spec.SessionID, err)
	}
	pid := cmd.Process.Pid
	// Pin the OS-reported start time now, while the process is definitely
	// still running (mirrors the acceptance mutator's own harness-pinning
	// discipline): PID reuse is ordinary, and a bare PID cannot later
	// distinguish this exact incarnation from something else that reuses the
	// number. This spawn is NOT failed when pinning fails — the process is
	// genuinely running and the launch is still usable — but
	// shimDiscoveryRecordMatchesLaunch refuses to use the post-deadline grace
	// path for a launch whose start time is unpinned (StartedAt == 0) rather
	// than falling back to a bare-PID match; only the launch's own ordinary
	// (non-grace) discovery wait still applies.
	identity := sessionshim.ProcessIdentity{PID: pid}
	if pinned, identityErr := sessionshim.ProcessIdentityFor(pid); identityErr == nil {
		identity = pinned
	} else {
		slog.Warn("session shim: could not pin the launched process's start time",
			"session", spec.SessionID, "pid", pid, "error", identityErr)
	}
	if err := cmd.Process.Release(); err != nil {
		// Release failing leaves this daemon as the waiter, which contradicts the
		// ownership boundary. Report it rather than proceeding as if the shim were
		// independent — the launch is still usable, but the claim would not be true.
		slog.Warn("session shim: could not release the launched process",
			"session", spec.SessionID, "pid", pid, "error", err)
	}
	return identity, nil
}

// shimChildLogCapBytes bounds the per-session shim child log file
// (shimChildLogPath). O_APPEND alone is unbounded — a long-running
// interactive session could otherwise fill the disk.
const shimChildLogCapBytes = 4 << 20 // 4 MiB

// shimChildLogGuardInterval is how often runShimChildLogGuard re-scans a
// live session's log file to redact secret-shaped content and enforce
// shimChildLogCapBytes.
const shimChildLogGuardInterval = 2 * time.Second

// shimChildLogTruncationMarkerFormat is appended once a tick truncates the
// file back to shimChildLogCapBytes.
const shimChildLogTruncationMarkerFormat = "\n[donmai] shim child log truncated at %d bytes (cap %d bytes)\n"

// shimChildLogSecretPatterns are the credential shapes redacted from a
// launched shim's captured stdout/stderr before the guard leaves them on
// disk: an authorization header ("Bearer <token>"), well-known prefixed
// API-key/machine-token shapes (OpenAI-style sk-, and the dmk_/rsk_/rsp_
// prefixes cmd/donmai/main.go's own credential resolution already
// recognizes), and a generic catch-all for any other long opaque
// base64/hex-shaped run a provider's own stderr might carry (e.g.
// provider/harness/clijsonl's raw claude-CLI stderr passthrough). The
// catch-all is deliberately broad — it will also mask non-secret long
// identifiers (commit SHAs, session hashes) — a false positive there is the
// accepted trade-off for an OSS-safe, provider-agnostic default; this file
// never inspects or special-cases any one provider's output shape.
var shimChildLogSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\brsk_[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\brsp_[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bdmk_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`\b[A-Za-z0-9+/_-]{32,}\b`),
}

// runShimChildLogGuard periodically redacts and caps a launched shim's
// captured stdout/stderr file. It exits on its own once the file is gone —
// removeShimChildLog deletes it exactly once, at the same terminal cleanup
// that disposes of this session's discovery record and tombstone — so no
// separate cancellation signal is threaded in.
//
// This is deliberately NOT a live writer sitting in the child's stdout/
// stderr path (contrast worker_spawner.go's pipe-to-log pattern for the
// direct-child lane): startShimProcess dupes a raw *os.File straight into
// the child so a detached shim's stdio survives this daemon exiting or
// restarting (see that function's doc comment). Threading a daemon-owned
// io.Writer through cmd.Stdout/Stderr instead would make os/exec fall back
// to an internal pipe + copying goroutine — and this child is our own Go
// binary, whose stdout/stderr (fds 1/2) the Go runtime kills the process on
// SIGPIPE for by default, so a reader that vanishes on daemon restart could
// SILENTLY KILL a session a human may still be attached to, not just lose
// its logs. Scanning and rewriting the file on disk out-of-band avoids that
// risk entirely, at the cost of a short exposure window (one guard
// interval) before a secret-shaped run is masked, and a short window
// before growth past the cap is trimmed back — both bounded by
// shimChildLogGuardInterval.
func runShimChildLogGuard(logPath string) {
	ticker := time.NewTicker(shimChildLogGuardInterval)
	defer ticker.Stop()
	for range ticker.C {
		if !guardShimChildLogOnce(logPath) {
			return
		}
	}
}

// guardShimChildLogOnce runs one redact+cap pass. It reports whether logPath
// still exists — false tells the caller's loop to stop, because
// removeShimChildLog already disposed of the file as part of this session's
// terminal cleanup.
func guardShimChildLogOnce(logPath string) bool {
	f, err := os.OpenFile(logPath, os.O_RDWR, 0o600) //nolint:gosec // G304: logPath is derived from this daemon's own registry directory + a digest filename, never external input
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return true
	}
	size := info.Size()
	if err := redactShimChildLog(f, size); err != nil {
		// logPath is this daemon's own registry directory plus a
		// digest-named file (shimChildLogPath) — never external input —
		// but gosec's taint analysis (G706) cannot see that provenance
		// through the call chain, so it flags the structured field the
		// same way every other slog call in this package that logs a
		// daemon-owned path already does; see e.g.
		// kit_registry.go's "kit registry: read scan path" for the same
		// precedent.
		slog.Warn("session shim: redact child log", //nolint:gosec // structured slog handler escapes values
			"path", logPath,
			"error", err,
		)
	}
	if err := capShimChildLog(f, size); err != nil {
		slog.Warn("session shim: cap child log", //nolint:gosec // structured slog handler escapes values
			"path", logPath,
			"error", err,
		)
	}
	return true
}

// redactShimChildLog masks every shimChildLogSecretPatterns match found in
// f's first size bytes with the ASCII byte 'x', repeated for the exact
// matched span. Same-length, same-offset in-place substitution is
// deliberate: it is race-safe against the shim child's own concurrent
// O_APPEND writes past size (offsets [0,size) were already durably on disk
// before this snapshot was taken, and nothing here ever touches offset
// size or beyond), whereas a rewrite that could shift or shorten content
// would not be.
func redactShimChildLog(f *os.File, size int64) error {
	if size <= 0 {
		return nil
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	changed := false
	for _, pattern := range shimChildLogSecretPatterns {
		for _, loc := range pattern.FindAllIndex(buf, -1) {
			for i := loc[0]; i < loc[1]; i++ {
				if buf[i] != 'x' {
					changed = true
				}
				buf[i] = 'x'
			}
		}
	}
	if !changed {
		return nil
	}
	_, err := f.WriteAt(buf, 0)
	return err
}

// capShimChildLog truncates f back to shimChildLogCapBytes and appends one
// truncation marker line when size exceeds the cap.
//
// Truncate is a metadata-only size change — it never rewrites bytes
// [0,shimChildLogCapBytes) — so, like redactShimChildLog, it never races the
// child's own concurrent O_APPEND writes.
//
// The marker write is a SEPARATE fd opened O_APPEND specifically for this
// write, not f.Seek(END)+f.Write on f itself: f is opened O_RDWR without
// O_APPEND (guardShimChildLogOnce), so a plain Seek+Write is two syscalls —
// a concurrent append from the child's own O_APPEND fd landing between them
// would move the true end-of-file out from under the Seek's result, and
// this function's Write would then land at that now-stale offset and
// OVERWRITE the child's just-appended bytes instead of following them. A
// fresh O_APPEND-opened fd makes the marker write one atomic
// seek-to-current-EOF-then-write kernel operation, exactly the same
// guarantee the child's own writes already rely on.
func capShimChildLog(f *os.File, size int64) error {
	if size <= shimChildLogCapBytes {
		return nil
	}
	if err := f.Truncate(shimChildLogCapBytes); err != nil {
		return err
	}
	appendFile, err := os.OpenFile(f.Name(), os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: f.Name() is this daemon's own already-open handle's path, never external input
	if err != nil {
		return err
	}
	defer func() { _ = appendFile.Close() }()
	_, err = appendFile.Write([]byte(fmt.Sprintf(shimChildLogTruncationMarkerFormat, size, shimChildLogCapBytes)))
	return err
}

// removeShimChildLog disposes of the per-session stdout/stderr capture file
// (shimChildLogPath) as part of the SAME terminal cleanup that withdraws
// this session's discovery record and disposes its tombstone (see this
// file's finishAdoptedShim and the quarantine/startup-adoption terminal
// disposal call sites) — never left behind indefinitely the way an
// unmanaged log file would be. Best-effort and idempotent: a missing file
// is not an error, and a removal failure only leaks one log file, never a
// reason to fail a terminal cleanup pass whose durable evidence is already
// committed.
func removeShimChildLog(registryDir string, id sessionshim.Identity) {
	if err := os.Remove(shimChildLogPath(registryDir, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("session shim: remove per-session log file", "session", id.String(), "error", err)
	}
}

// shimCommand is the argv used to launch a shim. It is the SAME worker command
// the direct-spawn path uses: the shim is the worker process, told by its
// environment to own its PTY instead of being owned (§D1 — "the shim process
// contains the runner-side interactive driver and ptyhost.Session").
func (d *Daemon) shimCommand() []string {
	if d.spawner == nil {
		return nil
	}
	return d.spawner.opts.WorkerCommand
}

// awaitShimRecord polls the registry until the launched shim publishes a valid
// discovery record, or ctx expires.
//
// A plain expiry is not the last word: sessionshim.Start deliberately spawns
// the harness under a PTY BEFORE publishing the record (sessionshim/shim.go),
// so the launch timeout's budget covers the harness's entire cold start —
// and some harnesses occasionally need a hair longer than even a generous
// budget. Rather than fail a launch whose record was seconds from landing,
// ctx expiring here hands off to a short, independently bounded grace poll
// (awaitShimRecordPostDeadlineGrace) that only ever turns a false "never
// arrived" into a true "arrived late" — it can never turn a genuine non-event
// into a fabricated success, because it still requires the record to
// identity-match this exact launch.
//
// Past that grace the question stops being about clocks and becomes about the
// PROCESS: while the worker this launch started is still alive there is
// something to wait for, so the wait continues to the longer live bound
// (awaitWhileProcessLives); the moment it is not, the wait ends with a definite
// failure, on whichever budget was running. Neither branch changes the happy
// path: every iteration reads the registry BEFORE it consults the process, so a
// record that lands in five seconds still adopts in five seconds.
func awaitShimRecord(ctx context.Context, wait shimDiscoveryWait) (sessionshim.Record, error) {
	// One addressable wait for the whole call: the probe-error log below is
	// once-per-launch state, and a value copy per phase would reset it.
	w := &wait
	start := time.Now()
	ticker := time.NewTicker(shimRecordPollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		rec, err := w.registry.Get(w.id)
		if err == nil {
			return rec, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if rec, ok := awaitShimRecordPostDeadlineGrace(w.registry, w.id, w.launch, w.started); ok {
				return rec, nil
			}
			return w.awaitWhileProcessLives(ctx, start, lastErr)
		case <-ticker.C:
			// A worker that has DIED cannot still be about to publish. Ending
			// here rather than serving out the launch clock is the same rule the
			// extended wait below applies, applied to the budget that runs first;
			// it never delays a record, because the registry read above always
			// runs before this probe.
			if w.processIsGone() {
				return sessionshim.Record{}, w.exitedError(lastErr)
			}
		}
	}
}

// shimDiscoveryWait is one launch's discovery wait: where to look for the
// record, which incarnation would count as this launch's own, and — new with
// shim-discovery-deadline-2026-09-02 — how to ask whether the launched process
// is still alive, plus the longer bound that liveness buys.
//
// process and liveBound are BOTH optional, and a wait missing either keeps
// exactly the pre-extension behaviour: no liveness knowledge means no basis for
// waiting longer than the launch clock, and none for claiming a live process was
// abandoned.
type shimDiscoveryWait struct {
	registry *sessionshim.Registry
	id       sessionshim.Identity
	launch   sessionshim.Launch
	started  sessionshim.ProcessIdentity
	process  shimLaunchProcess
	// liveBound is the TOTAL discovery budget, measured from the start of the
	// wait, for a launch whose process stays alive — not an extra budget added
	// on top of the launch clock. See SessionShimConfig.liveDiscoveryTimeout.
	liveBound time.Duration
	// probeErrorLogged makes the "could not probe the launched process" warning
	// once-per-launch rather than once-per-poll. Owned by the single goroutine
	// running the wait; never read after it returns.
	probeErrorLogged bool
}

// errShimDiscoveryAbandonedLiveProcess classifies the one discovery failure that
// leaves something running: the bound expired (or the only record on offer
// belongs to another incarnation) while the process this launch started was
// still alive.
//
// The caller stops that process. Every OTHER discovery failure — a dead worker, a
// wait with no liveness knowledge — has nothing left to stop, and must not be
// reported as if it did.
var errShimDiscoveryAbandonedLiveProcess = errors.New("the launched process was still alive at the discovery bound")

// awaitWhileProcessLives is the extended wait: the launch clock has expired and
// the bounded post-deadline grace poll found nothing, so the only question left
// is whether there is still a worker to wait FOR.
//
// Measured live under concurrent launch load: a worker whose harness bootstraps
// slowly had published nothing 31s after spawn, the daemon gave up at its launch
// bound — and the worker went on to run the whole prompt un-adopted. It was never
// a launch that produced nothing; it was a launch given a budget sized for an
// unloaded host. While the pid lives there is something to wait for, so this
// keeps waiting to liveBound; when it dies there is not, so this ends at once.
func (w *shimDiscoveryWait) awaitWhileProcessLives(
	ctx context.Context,
	start time.Time,
	lastErr error,
) (sessionshim.Record, error) {
	deadline := start.Add(w.liveBound)
	if w.process == nil || w.liveBound <= 0 || !time.Now().Before(deadline) {
		return sessionshim.Record{}, fmt.Errorf("waiting for discovery record: %w (last read: %v)", ctx.Err(), lastErr)
	}
	if w.processIsGone() {
		return sessionshim.Record{}, w.exitedError(lastErr)
	}
	// ONE line at the old bound, not one per poll: a slow bootstrap is visible in
	// the log exactly where an operator used to see the give-up, and the launch
	// that eventually adopts says so on its own.
	slog.Warn("session shim: still waiting for the discovery record; pid alive "+
		"(shim-discovery-deadline-2026-09-02)",
		"session", w.id.String(), "pid", w.started.PID,
		"launchBoundElapsed", time.Since(start).String(), "liveBound", w.liveBound.String())
	ticker := time.NewTicker(shimRecordPollInterval)
	defer ticker.Stop()
	for {
		rec, err := w.registry.Get(w.id)
		switch {
		case err == nil && shimDiscoveryRecordMatchesLaunch(rec, w.id, w.launch, w.started):
			slog.Warn("session shim: discovery record appeared past the launch timeout while the launched process stayed "+
				"alive; adopting it rather than failing the accept (shim-discovery-deadline-2026-09-02)",
				"session", w.id.String(), "pid", rec.PID, "processEpoch", rec.ProcessEpoch,
				"elapsed", time.Since(start).String())
			return rec, nil
		case err == nil:
			// A record exists but is not this launch's. Waiting longer cannot
			// change whose record it is — the same reasoning the grace poll
			// applies — and THIS launch's process is still running, so the caller
			// still has something to stop.
			slog.Warn("session shim: a discovery record for this session belongs to a different incarnation while this "+
				"launch's own process is still alive; abandoning and stopping it",
				"session", w.id.String(), "wantPid", w.started.PID, "wantProcessStartedAt", w.started.StartedAt,
				"gotPid", rec.PID, "gotProcessStartedAt", rec.ProcessStartedAt, "gotProcessEpoch", rec.ProcessEpoch)
			return sessionshim.Record{}, w.abandonedLiveError(start, "a discovery record for a different incarnation")
		default:
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			return sessionshim.Record{}, w.abandonedLiveError(start, fmt.Sprintf("last read: %v", lastErr))
		}
		<-ticker.C
		if w.processIsGone() {
			return sessionshim.Record{}, w.exitedError(lastErr)
		}
	}
}

// processIsGone reports whether the launched process has exited — and, because
// shimLaunchProcess.Alive reaps what it observes, guarantees a process reported
// gone left no defunct entry behind.
//
// A probe that ERRORS is not a death: an unprobeable process is treated as still
// running, so the wait keeps its bound and the caller still stops it at the end.
// Guessing "gone" from an unreadable probe is how a live harness gets abandoned.
//
// The error is logged ONCE per launch, not once per probe: this runs at
// shimRecordPollInterval, so a probe that fails for a persistent reason would
// otherwise emit thousands of identical lines across one live bound and bury the
// two lines that carry the actual disposition. The condition is a property of
// the launch, and one line reports it.
func (w *shimDiscoveryWait) processIsGone() bool {
	if w.process == nil {
		return false
	}
	alive, err := w.process.Alive()
	if err != nil {
		if !w.probeErrorLogged {
			w.probeErrorLogged = true
			slog.Warn("session shim: could not probe the launched process while waiting for its discovery record; "+
				"treating it as alive (logged once per launch)",
				"session", w.id.String(), "pid", w.started.PID, "error", err)
		}
		return false
	}
	return !alive
}

// exitedError is the DEFINITE failure for a launch whose process died before
// publishing anything. It deliberately does not wrap
// errShimDiscoveryAbandonedLiveProcess: there is nothing left to stop.
//
// It has exactly one shape because processIsGone reports "gone" from exactly one
// observation — a successful probe that answered "not alive". An errored probe
// never reports gone, so there is no such thing as a launch that exited AND
// could not be probed reaching here.
func (w *shimDiscoveryWait) exitedError(lastErr error) error {
	return fmt.Errorf("waiting for discovery record: the launched process %s exited without publishing one "+
		"(last read: %v)", w.started, lastErr)
}

// abandonedLiveError is the give-up that leaves a live worker behind — the one
// the caller answers with a stop.
func (w *shimDiscoveryWait) abandonedLiveError(start time.Time, detail string) error {
	return fmt.Errorf("waiting for discovery record: %w after %s (%s)",
		errShimDiscoveryAbandonedLiveProcess, time.Since(start).Round(time.Millisecond), detail)
}

// awaitShimRecordPostDeadlineGrace polls a short, independently bounded
// number of times AFTER the launch timeout's own deadline has already
// passed, on the chance the worker's discovery record lands moments late —
// the exact condition measured live: the daemon gave up at the deadline, and
// the record appeared under the expected filename for the expected identity
// moments afterward, with the shim process still alive.
//
// It returns the record only when it is plausibly THIS launch's own — see
// shimDiscoveryRecordMatchesLaunch. A record that never appears, or that
// appears but belongs to a different incarnation, still fails the launch
// exactly as before: §D10's prohibition on guessing at an unidentifiable
// launch is preserved, not relaxed. This grace window only ever converts a
// false negative into a true positive, never the reverse.
func awaitShimRecordPostDeadlineGrace(
	registry *sessionshim.Registry,
	id sessionshim.Identity,
	launch sessionshim.Launch,
	started sessionshim.ProcessIdentity,
) (sessionshim.Record, bool) {
	delay := shimRecordDiscoveryGraceBaseDelay
	for attempt := 0; attempt < shimRecordDiscoveryGraceAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			delay *= 2
		}
		rec, err := registry.Get(id)
		if err != nil {
			continue
		}
		if !shimDiscoveryRecordMatchesLaunch(rec, id, launch, started) {
			// A record exists but is not this launch's — something else's
			// record, not a slow arrival of this one. Waiting longer cannot
			// change whose record this is, so stop rather than keep polling
			// toward a match that will never happen.
			slog.Warn("session shim: a discovery record appeared after the launch timeout but did not match this launch; still failing the accept",
				"session", id.String(), "wantProcessEpoch", launch.ProcessEpoch, "wantPid", started.PID,
				"wantProcessStartedAt", started.StartedAt,
				"gotProcessEpoch", rec.ProcessEpoch, "gotPid", rec.PID, "gotProcessStartedAt", rec.ProcessStartedAt)
			return sessionshim.Record{}, false
		}
		slog.Warn("session shim: discovery record appeared after the launch timeout; adopting it rather than failing the accept",
			"session", id.String(), "attempt", attempt+1, "processEpoch", rec.ProcessEpoch, "pid", rec.PID)
		return rec, true
	}
	return sessionshim.Record{}, false
}

// shimDiscoveryRecordMatchesLaunch reports whether rec is plausibly the exact
// discovery record this launch's own worker eventually published: the same
// identity, at the process epoch this launch requested, the PID this launch
// actually started, AND the OS-reported start time this daemon pinned for
// that PID at spawn time (see startShimProcess). It is deliberately NOT a
// liveness check — §D10 forbids inferring anything from process state —
// only a check against the record's own declared identity.
//
// A missing pinned start time (started.StartedAt == 0, meaning
// startShimProcess could not read it) REFUSES the match rather than
// degrading to PID alone. launch.ProcessEpoch is a hardcoded constant in
// every production launch, so PID+StartedAt are the only two discriminators
// this check actually has; PID alone is exactly the bare-PID comparison
// sessionshim.ProcessIdentity's own doc comment calls unsafe, because PID
// reuse is ordinary. Guessing a match here would be the exact inference
// §D10 forbids, so an unpinned launch simply cannot use the grace path —
// its own orphan deadline remains the escape hatch, same as a record that
// never appears at all.
func shimDiscoveryRecordMatchesLaunch(rec sessionshim.Record, id sessionshim.Identity, launch sessionshim.Launch, started sessionshim.ProcessIdentity) bool {
	if started.StartedAt == 0 {
		return false
	}
	return rec.OrgID == id.OrgID && rec.SessionID == id.SessionID &&
		rec.ProcessEpoch == launch.ProcessEpoch && rec.PID == started.PID &&
		rec.ProcessStartedAt == started.StartedAt
}

// trackLaunchedShim records a newly adopted controller and starts consuming its
// event stream.
func (d *Daemon) trackLaunchedShim(
	ctrl *sessionshim.Controller,
	spec SessionSpec,
	project ProjectConfig,
	workarea string,
	workareaRoot string,
	evidence SessionShimAdoptionEvidence,
	receipt SessionShimAdoptionReceipt,
	startConsumer bool,
) SessionHandle {
	handle := SessionHandle{
		SessionID:  spec.SessionID,
		PID:        ctrl.HarnessIdentity().PID,
		AcceptedAt: d.shimNow().UTC().Format(time.RFC3339),
		State:      SessionRunning,
		// The workarea doubles as the worktree path a local reader joins with
		// .agent/…; it is the same <parent>/<sessionID> leaf the direct path
		// publishes, so a reader cannot tell shim-backed sessions apart by shape.
		WorktreePath: workarea,
		WorkareaRoot: workareaRoot,
		ProjectName:  project.ID,
		Repository:   spec.Repository,
	}
	entry := adoptedShim{
		controller:      ctrl,
		shimID:          ctrl.Hello().ShimID,
		handle:          handle,
		spec:            spec,
		launched:        true,
		adoption:        evidence,
		adoptionReceipt: cloneSessionShimAdoptionReceipt(receipt),
	}
	d.shims.mu.Lock()
	d.shims.adopted[ctrl.Identity()] = entry
	d.shims.correlations[shimIncarnationFor(evidence)] = sessionShimAdoptionCorrelation{
		evidence: evidence,
		receipt:  cloneSessionShimAdoptionReceipt(receipt),
	}
	d.shims.mu.Unlock()
	// A shim-backed session is external to the direct-child registry, but it is
	// still one accepted WorkerSpawner lifecycle. Emit Started only after the
	// controller and handle are published, and before starting the consumer, so
	// an immediately available immutable Exit can never overtake it.
	if d.spawner != nil {
		d.spawner.emit(SessionEvent{Kind: SessionEventStarted, Handle: handle, Spec: spec})
	}
	if startConsumer {
		d.consumeShimEvents(ctrl)
	}
	return handle
}

// consumeShimEvents drains one adopted session's stream in the background.
//
// This is the daemon's half of stop/input/OUTPUT after adoption. It exists as a
// real consumer rather than a drop-on-the-floor because the shim's ring is
// bounded: a controller that never reads and never acknowledges makes every
// later adoption resume further back, and eventually forces the honest-but-
// avoidable Gap that §D5 makes the shim declare.
func (d *Daemon) consumeShimEvents(ctrl *sessionshim.Controller) {
	d.consumeShimEventsGated(ctrl, nil)
}

func (d *Daemon) consumeShimEventsGated(ctrl *sessionshim.Controller, gate *shimAdoptionGate) {
	d.shims.wg.Add(1)
	go func() {
		defer d.shims.wg.Done()
		id := ctrl.Identity()
		cfg := d.sessionShimConfig()
		observe := cfg.OnSessionEvent
		durable := cfg.OnSessionEventDurable
		fullHostFrames := ctrl.SupportsFullHostFrames()
		legacyDurability := !cfg.RequireAuthoritativeSnapshot
		cursor := d.startShimCursorAcknowledger(id, ctrl)
		defer cursor.stop()
		var lastSeq uint64
		// terminalStreamClosed records that this daemon closed the stream while
		// handling the terminal frame. The Exit is still reported — it is durable —
		// and the loop ends immediately afterwards rather than reading a socket
		// nobody owns.
		terminalStreamClosed := false
		// cause records WHY the stream ended. A stream this daemon closed
		// because its durable carrier refused is a carrier loss — the shim and
		// its harness are untouched — and is the one ending the release path
		// re-adopts before it quarantines. Every other ending (the shim closed,
		// the socket went away, the shim broke the sequence contract) keeps the
		// pre-existing disposition.
		cause := shimStreamEnded
	consume:
		for ev := range ctrl.Events() {
			if observe != nil {
				// Forward first, bookkeep second: a carrier must see output in the
				// order the shim produced it, and acknowledging a sequence this
				// daemon has not actually handed on would make the resume point a
				// claim rather than a record.
				observe(id, ev)
			}
			switch ev.Kind {
			case sessionshim.EventOutput:
				if ev.Seq > lastSeq {
					lastSeq = ev.Seq
					if durable != nil && legacyDurability {
						if err := durable(id, ev); err != nil {
							slog.Warn("session shim: durable carrier rejected output",
								"session", id.String(), "seq", ev.Seq, "error", err)
							// A later frame must never advance the cursor past this
							// unacknowledged one. Close the controller and let the
							// normal disconnect/quarantine path retain ownership.
							cause = shimStreamCarrierLost
							_ = ctrl.Close()
							break consume
						}
						if err := d.recordShimForwardedSeqForController(id, ctrl, ev.Seq); err != nil {
							slog.Warn("session shim: durable output acknowledgement was not persisted",
								"session", id.String(), "seq", ev.Seq, "error", err)
							cause = shimStreamCarrierLost
							_ = ctrl.Close()
							break consume
						}
					}
				}
			case sessionshim.EventGap:
				// Surfaced, never smoothed over (§D5). A daemon that logged nothing
				// here would be claiming contiguous output it does not possess.
				slog.Warn("session shim: output gap declared by the shim",
					"session", id.String(), "fromSeq", ev.Gap.FromSeq,
					"toSeq", ev.Gap.ToSeq, "reason", ev.Gap.Reason)
				if fullHostFrames && durable != nil {
					if err := durable(id, ev); err != nil {
						slog.Warn("session shim: durable carrier rejected output gap",
							"session", id.String(), "fromSeq", ev.Gap.FromSeq,
							"toSeq", ev.Gap.ToSeq, "error", err)
						cause = shimStreamCarrierLost
						_ = ctrl.Close()
						break consume
					}
				}
			case sessionshim.EventHostFrame:
				if ev.Seq == 0 || ev.Seq <= lastSeq {
					slog.Warn("session shim: HostFrame sequence did not advance",
						"session", id.String(), "seq", ev.Seq, "lastSeq", lastSeq)
					_ = ctrl.Close()
					break consume
				}
				lastSeq = ev.Seq
				if d.isStagedSessionShimSnapshot(id, ev) {
					if durable == nil {
						slog.Warn("session shim: staged Snapshot has no durable carrier",
							"session", id.String(), "seq", ev.Seq)
						_ = ctrl.Close()
						break consume
					}
					if err := durable(id, ev); err != nil {
						slog.Warn("session shim: durable carrier rejected staged Snapshot",
							"session", id.String(), "seq", ev.Seq, "error", err)
						cause = shimStreamCarrierLost
						_ = ctrl.Close()
						break consume
					}
					activationGate, retained := d.retainStagedSessionShimSnapshot(id, ev)
					if !retained || !activationGate.await() {
						return
					}
					continue
				}
				if durable != nil {
					if err := durable(id, ev); err != nil {
						slog.Warn("session shim: durable carrier rejected host frame",
							"session", id.String(), "seq", ev.Seq, "type", ev.FrameType, "error", err)
						cause = shimStreamCarrierLost
						_ = ctrl.Close()
						break consume
					}
					if ev.FrameType == attachwire.TypeExit {
						// The terminal cursor must be durable BEFORE a terminal
						// outcome is reported, so this one frame still pays for a
						// synchronous receipt.
						//
						// A FAILED acknowledgement is not a reason to drop the
						// observation. The carrier already accepted this exact
						// Exit durably one line above; the acknowledgement is the
						// SHIM's replay cursor, and a shim that published Exit and
						// then closed has nothing left to replay. Dropping the
						// terminal fact here — measured — left the session
						// quarantined `socket_unreachable` with its harness
						// already reaped, which §D10 calls unresolved rather than
						// ended. Close the dead stream and report what is durable.
						if err := cursor.persist(ev.Seq); err != nil {
							slog.Warn("session shim: terminal cursor acknowledgement was not delivered; the carrier already holds this Exit",
								"session", id.String(), "seq", ev.Seq, "error", err)
							_ = ctrl.Close()
							terminalStreamClosed = true
						}
					} else {
						cursor.record(ev.Seq)
					}
				}
				if ev.FrameType == attachwire.TypeExit {
					slog.Info("session shim: terminal observation received",
						"session", id.String(), "exitCode", ev.Exit.ExitCode, "signal", ev.Exit.Signal)
					if !gate.await() {
						return
					}
					d.finishAdoptedShim(id, ev.Exit)
					if terminalStreamClosed {
						break consume
					}
				}
			case sessionshim.EventExit:
				slog.Info("session shim: terminal observation received",
					"session", id.String(), "exitCode", ev.Exit.ExitCode, "signal", ev.Exit.Signal)
				if !gate.await() {
					return
				}
				d.finishAdoptedShim(id, ev.Exit)
			case sessionshim.EventError:
				slog.Warn("session shim: error frame",
					"session", id.String(), "code", ev.Err.Code, "detail", ev.Err.Detail)
			case sessionshim.EventSnapshot:
				// State after a gap or on request; the sequence it carries is the
				// resume point a later adoption starts from.
				if ev.Snapshot.AtSeq > lastSeq {
					lastSeq = ev.Snapshot.AtSeq
					if durable != nil && legacyDurability {
						if err := durable(id, ev); err != nil {
							slog.Warn("session shim: durable carrier rejected snapshot",
								"session", id.String(), "seq", ev.Snapshot.AtSeq, "error", err)
							// A later frame must never advance the cursor past this
							// unacknowledged snapshot. Close the controller and let the
							// normal disconnect/quarantine path retain ownership.
							cause = shimStreamCarrierLost
							_ = ctrl.Close()
							break consume
						}
						if err := d.recordShimForwardedSeqForController(id, ctrl, ev.Snapshot.AtSeq); err != nil {
							slog.Warn("session shim: durable Snapshot acknowledgement was not persisted",
								"session", id.String(), "seq", ev.Snapshot.AtSeq, "error", err)
							cause = shimStreamCarrierLost
							_ = ctrl.Close()
							break consume
						}
					}
				}
			case sessionshim.EventSnapshotFrame:
				// Selected-v2 only. Hosted full-frame composition never stages this
				// semantic event; selected v3 stages its one exact EventHostFrame.
				if ev.Seq > lastSeq {
					lastSeq = ev.Seq
					if durable != nil && legacyDurability {
						if err := durable(id, ev); err != nil {
							slog.Warn("session shim: durable carrier rejected emitted snapshot",
								"session", id.String(), "seq", ev.Seq, "error", err)
							cause = shimStreamCarrierLost
							_ = ctrl.Close()
							break consume
						}
						if err := d.recordShimForwardedSeqForController(id, ctrl, ev.Seq); err != nil {
							slog.Warn("session shim: durable emitted Snapshot acknowledgement was not persisted",
								"session", id.String(), "seq", ev.Seq, "error", err)
							cause = shimStreamCarrierLost
							_ = ctrl.Close()
							break consume
						}
					}
				}
			}
		}
		// The stream ended, or this consumer dropped it. Either way the shim keeps
		// the harness and starts its orphan clock, so this is NOT a terminal
		// outcome and must not be reported as one — but ownership must be released
		// rather than left published against a socket nobody can write to, or
		// (after a carrier loss) re-established through a fresh adoption.
		if gate.await() {
			d.releaseShimIfLive(id, ctrl, cause)
		}
	}()
}

// shimStreamEndCause says why an adopted session's controller stream ended.
type shimStreamEndCause uint8

const (
	// shimStreamEnded: the shim, or its socket, ended the stream — or this
	// daemon closed it because the shim broke the sequence contract.
	shimStreamEnded shimStreamEndCause = iota
	// shimStreamCarrierLost: this daemon closed the stream because its
	// durable carrier refused an append or an acknowledgement. The shim and
	// its harness are alive and untouched.
	shimStreamCarrierLost
)

// shimCursorAcknowledger persists one adopted session's durable forwarded
// cursor OFF the event path.
//
// Selected v3 acknowledges through an fsync-backed round trip to the shim, and
// paying for one per frame caps the consumer at whatever a single fsync costs —
// tens of frames a second. The controller's priority event queue is bounded and
// fail-closed by design, so a consumer that falls behind does not slow the
// stream down: the reader drops the connection. One ordinary terminal redraw
// therefore used to be enough to kill a live session's control channel and leave
// the daemon holding an adopted entry it could no longer write to.
//
// Acknowledging from a separate goroutine keeps the cursor exactly as durable
// while taking the round trip off the frame path. The cursor still advances only
// on the shim's exact receipt, and a coalesced acknowledgement makes a later
// adoption replay MORE, never less — which is the direction the resume contract
// already allows.
type shimCursorAcknowledger struct {
	daemon  *Daemon
	id      sessionshim.Identity
	ctrl    shimCursorController
	pending atomic.Uint64
	wake    chan struct{}
	quit    chan struct{}
	quitOne sync.Once

	// mu serializes acknowledgement round trips so a coalesced background beat
	// and a synchronous terminal one can never interleave, and never regress the
	// sequence the shim has already stored.
	mu    sync.Mutex
	acked uint64

	// backoffMu guards lastBackoff, the delay the loop most recently waited for
	// a pending persistence receipt. It exists so the reset-on-success rule is
	// observable: a backoff that keeps its earned cap after the stall cleared
	// makes the next unrelated hiccup wait a full callback timeout for nothing,
	// and nothing else about the loop distinguishes the two.
	backoffMu   sync.Mutex
	lastBackoff time.Duration
}

// noteReceiptBackoff records the delay the loop is about to wait.
func (a *shimCursorAcknowledger) noteReceiptBackoff(d time.Duration) {
	a.backoffMu.Lock()
	a.lastBackoff = d
	a.backoffMu.Unlock()
}

// receiptBackoffUsed reports the delay most recently waited for a pending
// persistence receipt.
func (a *shimCursorAcknowledger) receiptBackoffUsed() time.Duration {
	a.backoffMu.Lock()
	defer a.backoffMu.Unlock()
	return a.lastBackoff
}

// shimCursorController is the controller surface the acknowledger uses. It is
// an interface so a test can drive the REAL goroutine against a shim that
// refuses, and observe whether the connection is dropped — the one decision
// this loop makes that a caller can never see from the outside.
type shimCursorController interface {
	sessionShimCursorAcknowledger
	Done() <-chan struct{}
	Close() error
}

func (d *Daemon) startShimCursorAcknowledger(
	id sessionshim.Identity,
	ctrl shimCursorController,
) *shimCursorAcknowledger {
	a := &shimCursorAcknowledger{
		daemon: d, id: id, ctrl: ctrl,
		wake: make(chan struct{}, 1), quit: make(chan struct{}),
	}
	d.shims.wg.Add(1)
	// The pending-receipt retry reuses the batch commit's derived units rather
	// than inventing a second pair: one base delay, doubling, capped by the
	// composing callback bound this daemon already waits a round trip for.
	receiptBackoff := sessionShimAdoptionBatchCommitBaseBackoff
	receiptBackoffCap := d.sessionShimConfig().callbackTimeout()
	go func() {
		defer d.shims.wg.Done()
		for {
			select {
			case <-a.wake:
			case <-a.quit:
				return
			case <-ctrl.Done():
				return
			}
			if err := a.persist(a.pending.Load()); err != nil {
				// A receipt that is merely SLOW is a condition of the durable
				// side, not of this connection. Measured on an installed host:
				// a persistence stall of tens of seconds dropped two healthy
				// shims, and both reaped their own harnesses once their orphan
				// clocks ran out — two working seats lost to a slow write. Keep
				// the connection, leave the acknowledgement outstanding (the
				// cursor never advances past what the shim confirmed), and come
				// back to it after one bounded backoff.
				if errors.Is(err, sessionshim.ErrHeartbeatReceiptPending) {
					slog.Warn("session shim: durable HostFrame acknowledgement is still pending; keeping the shim connection and retrying "+
						"(shim-adoption-reconvergence-2026-09-01)",
						"session", id.String(), "seq", a.pending.Load(), "backoff", receiptBackoff, "error", err)
					a.noteReceiptBackoff(receiptBackoff)
					if !a.sleepPendingReceiptBackoff(receiptBackoff) {
						return
					}
					if receiptBackoff *= 2; receiptBackoff > receiptBackoffCap {
						receiptBackoff = receiptBackoffCap
					}
					a.signal()
					continue
				}
				// A shim that refuses because its TERMINAL PROOF IS PUBLISHED is
				// not a broken socket — it is telling this daemon that the
				// tombstone is already on disk. Measured on an installed host:
				// treating that as an ordinary transport failure published a
				// quarantine at one adoption revision, drew a heartbeat 409
				// SESSION_SHIM_ADOPTION_REVISION_STALE, armed commit-outcome
				// reconciliation and republished at the next revision — 26
				// seconds of churn to reach a terminal outcome the shim had
				// already handed over. Consume the proof instead.
				if errors.Is(err, sessionshim.ErrShimExited) {
					// STOP ACKNOWLEDGING, DO NOT DROP THE CONNECTION. This
					// refusal says the tombstone is on disk — and the shim is
					// still flushing the one Exit HostFrame that ends the
					// session on this very stream. Closing here took that frame
					// away from the consume loop: the lineage then left through
					// releaseShimIfLive instead of finishAdoptedShim, so
					// SessionEventEnded — the only cleanup of the per-session
					// detail cache — was never emitted, and the fallback
					// republished a whole adoption batch that flipped the host
					// out of adoption_complete until its next beat. Measured on
					// an installed host, in that order.
					slog.Info("session shim: the shim refused the cursor acknowledgement because its terminal proof is published",
						"session", id.String(), "seq", a.pending.Load())
					return
				}
				slog.Warn("session shim: durable HostFrame acknowledgement was not persisted",
					"session", id.String(), "seq", a.pending.Load(), "error", err)
				// A genuine persistence failure is different: the cursor must
				// never claim a sequence the shim did not durably store. Drop the
				// connection and let the ordinary disconnect path release
				// ownership; the shim keeps the harness.
				// The reconcile itself runs in releaseShimIfLive, which is where
				// the quarantine this would otherwise publish is created —
				// calling it here would race that publication instead of
				// preventing it.
				_ = ctrl.Close()
				return
			}
			// A confirmed acknowledgement ends the stall: the next one that has
			// to wait starts from the base delay rather than inheriting a cap
			// earned by a condition that has since cleared.
			receiptBackoff = sessionShimAdoptionBatchCommitBaseBackoff
			if a.pending.Load() > a.highWater() {
				a.signal()
			}
		}
	}()
	return a
}

// record notes the highest sequence this daemon has durably forwarded and wakes
// the acknowledger. It never blocks on the shim.
func (a *shimCursorAcknowledger) record(seq uint64) {
	for {
		current := a.pending.Load()
		if seq <= current {
			return
		}
		if a.pending.CompareAndSwap(current, seq) {
			break
		}
	}
	a.signal()
}

// persist acknowledges seq synchronously. It is a no-op for a sequence the shim
// has already stored, which is what keeps the coalesced beat and the terminal
// one from ever regressing each other.
func (a *shimCursorAcknowledger) persist(seq uint64) error {
	a.record(seq)
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq <= a.acked {
		return nil
	}
	if err := a.daemon.recordShimForwardedSeqForController(a.id, a.ctrl, seq); err != nil {
		return err
	}
	a.acked = seq
	return nil
}

func (a *shimCursorAcknowledger) highWater() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acked
}

func (a *shimCursorAcknowledger) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *shimCursorAcknowledger) stop() { a.quitOne.Do(func() { close(a.quit) }) }

// sleepPendingReceiptBackoff waits d before the next acknowledgement attempt,
// or returns false when the acknowledger was stopped or the controller ended
// first — the two reasons there is nothing left to retry for.
func (a *shimCursorAcknowledger) sleepPendingReceiptBackoff(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-a.quit:
		return false
	case <-a.ctrl.Done():
		return false
	}
}

type sessionShimCursorAcknowledger interface {
	SupportsFullHostFrames() bool
	Heartbeat(uint64) error
}

func (d *Daemon) recordShimForwardedSeqForController(
	id sessionshim.Identity,
	ctrl sessionShimCursorAcknowledger,
	seq uint64,
) error {
	fullHostFrames := ctrl != nil && ctrl.SupportsFullHostFrames()
	if fullHostFrames {
		if err := ctrl.Heartbeat(seq); err != nil {
			return err
		}
	}
	d.shims.mu.Lock()
	if d.shims.forwarded == nil {
		d.shims.forwarded = make(map[sessionshim.Identity]uint64)
	}
	if seq > d.shims.forwarded[id] {
		d.shims.forwarded[id] = seq
	}
	d.shims.mu.Unlock()
	if ctrl != nil && !fullHostFrames {
		_ = ctrl.Heartbeat(seq)
	}
	return nil
}

// SessionShimForwardedSeq reports the highest output sequence this daemon has
// durably forwarded for a session — its resume position for a later adoption.
func (d *Daemon) SessionShimForwardedSeq(orgID, sessionID string) uint64 {
	if d.shims == nil {
		return 0
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return d.shims.forwarded[sessionshim.Identity{OrgID: orgID, SessionID: sessionID}]
}

// finishAdoptedShim is terminal cleanup after adoption (§D8/§D10).
//
// The order matters and is the conservative one: drop the live entry so capacity
// is released, record the tombstone as the durable proof of what happened, and
// only THEN dispose of it. Disposing first would destroy the one artifact that
// can prove the harness group was reaped, turning a proven death back into an
// unresolved one for anything that asks later.
func (d *Daemon) finishAdoptedShim(id sessionshim.Identity, exit shimwire.ExitMsg) {
	d.shims.mu.Lock()
	entry, ok := d.shims.adopted[id]
	if !ok || entry.terminal {
		d.shims.mu.Unlock()
		return
	}
	entry.terminal = true
	d.shims.adopted[id] = entry
	registry := d.shims.registry
	d.shims.mu.Unlock()

	// OnPreSpawn transferred cleanup ownership when this daemon launched the
	// shim. Deliver the same ordinary lifecycle listeners as a direct child, but
	// only for the immutable Exit frame. A controller disconnect never reaches
	// this function and therefore never fabricates a terminal outcome.
	if entry.launched && d.spawner != nil {
		handle := entry.handle
		var exitErr error
		switch {
		case exit.Signal != "":
			handle.State = SessionTerminated
			exitErr = fmt.Errorf("session shim exited after signal %s", exit.Signal)
		case exit.ExitCode != 0:
			handle.State = SessionFailed
			exitErr = fmt.Errorf("session shim exited with code %d", exit.ExitCode)
		default:
			handle.State = SessionCompleted
		}
		d.spawner.emit(SessionEvent{
			Kind:    SessionEventEnded,
			Handle:  handle,
			Spec:    entry.spec,
			ExitErr: exitErr,
		})
	}

	// Retain the exact generation through synchronous listener delivery. A
	// replacement must never be deleted by a stale terminal consumer.
	d.shims.mu.Lock()
	current, stillOwned := d.shims.adopted[id]
	if stillOwned && current.controller == entry.controller && current.terminal {
		delete(d.shims.adopted, id)
		delete(d.shims.forwarded, id)
	} else {
		stillOwned = false
	}
	d.shims.mu.Unlock()
	if !stillOwned {
		return
	}
	if entry.controller != nil {
		_ = entry.controller.Close()
	}
	if registry == nil {
		return
	}
	// The shim emits Exit and THEN durably publishes the tombstone, so an
	// immediate read races the write. Poll briefly rather than concluding the
	// proof is missing: treating an in-flight tombstone as absent would leave a
	// session that provably ended parked in reconciliation forever.
	tombstone, err := awaitTombstone(registry, id, entry.shimID, entry.adoption.ProcessEpoch, tombstoneSettleWindow)
	if err != nil {
		slog.Warn("session shim: terminal observation without a durable tombstone",
			"session", id.String(), "error", err)
		return
	}
	d.shims.mu.Lock()
	d.shims.tombstoned = append(d.shims.tombstoned, tombstone)
	d.shims.mu.Unlock()

	hostID, hostErr := d.sessionShimHostID(context.Background(), id.OrgID)
	if hostErr != nil {
		slog.Warn("session shim: retain terminal tombstone after host identity resolution failed",
			"session", id.String(), "error", hostErr)
		return
	}
	adoption := entry.adoption
	terminalEvidence := SessionShimTerminalEvidence{
		Identity:                   id,
		HostID:                     hostID,
		ShimID:                     tombstone.ShimID,
		ProcessEpoch:               tombstone.ProcessEpoch,
		Adoption:                   &adoption,
		DurableAdoptionCorrelation: append([]byte(nil), entry.adoptionReceipt.DurableCorrelation...),
		Tombstone:                  tombstone,
	}
	if err := d.reportSessionShimTerminalEvidence(context.Background(), terminalEvidence); err != nil {
		// The harness group is already gone, so capacity may be released, but the
		// proof is retained on disk. A later startup retries the exact durable
		// handoff before readiness; disposing it here would strand the external
		// claim in reconciliation with no evidence left to replay.
		slog.Warn("session shim: retain terminal tombstone after durable evidence refusal",
			"session", id.String(), "error", err)
		return
	}
	d.shims.mu.Lock()
	delete(d.shims.correlations, shimIncarnationFor(entry.adoption))
	d.shims.mu.Unlock()

	verdict := d.SessionShimReleaseDecision(id.OrgID, id.SessionID, d.SessionShimTerminalProof(id.OrgID, id.SessionID))
	if verdict != sessionshim.ReleaseAllowed {
		slog.Info("session shim: terminal outcome recorded but the claim is not releasable yet",
			"session", id.String(), "verdict", verdict)
		return
	}
	// Withdraw the liveness claim BEFORE disposing the proof, and never the other
	// way round. A shim publishes its tombstone and then removes its discovery
	// record, so those two writes can be observed apart — and a crash between them
	// leaves both on disk by design. Disposing the tombstone first would collapse
	// "terminal, proven" into "a record whose process is gone", which §D10
	// classifies as stale and leaves unresolved. Remove is idempotent, so the
	// ordinary case where the shim already withdrew its own record costs nothing.
	if err := registry.RemoveIncarnation(id, tombstone.ShimID, tombstone.ProcessEpoch); err != nil {
		slog.Warn("session shim: withdraw discovery record", "session", id.String(), "error", err)
		return
	}
	// Audited disposal: the outcome is durably recorded above and no liveness
	// claim survives it, so the tombstone has done its job and may go.
	if err := registry.RemoveTombstoneIncarnation(tombstone); err != nil {
		slog.Warn("session shim: dispose tombstone", "session", id.String(), "error", err)
	}
	// The captured stdout/stderr file is disposed alongside the record/
	// tombstone above — see removeShimChildLog's doc comment.
	removeShimChildLog(registry.Dir(), id)
}

// tombstoneSettleWindow bounds the wait for a shim's tombstone after its Exit
// frame arrives. The shim writes it immediately afterwards (one Alive() probe
// and one atomic publish), so this is a settle window, not a retry budget.
const tombstoneSettleWindow = 5 * time.Second

// awaitTombstone polls for the durable terminal proof a shim leaves behind.
func awaitTombstone(
	registry *sessionshim.Registry,
	id sessionshim.Identity,
	shimID string,
	processEpoch uint64,
	within time.Duration,
) (sessionshim.Tombstone, error) {
	deadline := time.Now().Add(within)
	var lastErr error
	for {
		t, err := registry.GetTombstoneIncarnation(id, shimID, processEpoch)
		if err == nil {
			live, liveErr := registry.HasIncarnation(id, shimID, processEpoch)
			if liveErr == nil && !live {
				return t, nil
			}
			if liveErr != nil {
				lastErr = liveErr
			} else {
				lastErr = errors.New("matching live discovery record has not been withdrawn")
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			return sessionshim.Tombstone{}, lastErr
		}
		time.Sleep(shimRecordPollInterval)
	}
}

// releaseShimIfLive handles a controller whose stream ended without a terminal
// observation. The session is NOT reported as ended: only a terminal receipt or
// matching tombstone closes the loop (§D7/§D10).
//
// A CARRIER loss — this daemon closed the stream because its durable carrier
// refused — is re-adopted first, through the same pipeline the startup pass
// runs, and the lineage stays adopted under a strictly newer generation when
// that succeeds. Every other ending, and a carrier loss whose re-adoption
// fails inside its bound, withdraws authority and moves the exact live shim
// into visible, capacity-consuming quarantine.
func (d *Daemon) releaseShimIfLive(id sessionshim.Identity, ctrl *sessionshim.Controller, cause shimStreamEndCause) {
	d.shims.mu.RLock()
	entry, ok := d.shims.adopted[id]
	if ok && ctrl != nil && entry.controller != ctrl {
		// A replacement controller already owns this identity. A consumer whose
		// own connection ended must never evict the live one.
		ok = false
	}
	d.shims.mu.RUnlock()
	if !ok {
		return
	}
	if entry.controller != nil {
		_ = entry.controller.Close()
		slog.Info("session shim: controller connection ended without a terminal observation; the shim retains its harness",
			"session", id.String(), "carrierLost", cause == shimStreamCarrierLost)
	}
	// The adopted entry stays in place while re-adoption runs: the receiver
	// holds this lineage adopted at exactly this generation, and every
	// projection built meanwhile must keep saying so. Removing it first would
	// have a concurrent republish omit a live lineage, which the receiver
	// refuses as an incomplete snapshot.
	if cause == shimStreamCarrierLost && d.readoptSessionShimAfterControllerLoss(id, entry) {
		return
	}
	d.quarantineLostSessionShim(id, entry)
}

// quarantineLostSessionShim withdraws authority from the lost controller and
// projects the exact live shim as quarantined `socket_unreachable`, then
// publishes. It is a no-op when the entry has already left by another route —
// a shutdown release, or a replacement controller.
func (d *Daemon) quarantineLostSessionShim(id sessionshim.Identity, entry adoptedShim) {
	now := d.shimNow()
	d.shims.mu.Lock()
	current, ok := d.shims.adopted[id]
	if ok && current.controller != entry.controller {
		ok = false
	}
	if ok {
		delete(d.shims.adopted, id)
		hello := entry.controller.Hello()
		q := sessionshim.NewQuarantinedSession(sessionshim.Record{
			OrgID:             id.OrgID,
			SessionID:         id.SessionID,
			ShimID:            entry.shimID,
			ProcessEpoch:      hello.ProcessEpoch,
			ProtocolMin:       hello.Min,
			ProtocolMax:       hello.Max,
			Phase:             hello.Phase,
			CreatedAtUnixNano: now.UnixNano(),
		}, sessionshim.QuarantineSocketUnreachable,
			"controller stream ended before a terminal observation", now)
		q.ControllerGeneration = uint64(entry.controller.Generation())
		d.upsertShimQuarantineLocked(q)
	}
	d.shims.mu.Unlock()
	if ok {
		d.publishQuarantineAfterConsumingTerminalProof(id.OrgID)
	}
}

// publishQuarantineAfterConsumingTerminalProof is the disconnect path's
// publication, and the order in its name is the point.
//
// A shim can finalize between its last frame and the disconnect — it answers a
// late acknowledgement with "terminal proof is published" (ErrShimExited) and
// then closes — so the lineage about to be projected as quarantined may already
// have its tombstone on disk. Publishing it anyway costs an adoption revision,
// a heartbeat 409 SESSION_SHIM_ADOPTION_REVISION_STALE, commit-outcome
// reconciliation and a second publication to undo, all to reach an outcome the
// shim had already handed over. Consume the proof first; the publish then
// carries the terminal fact instead of a quarantine nothing holds — and when
// the handoff itself already published this scope's complete projection, that
// publication IS the one, and a second would cost a revision for nothing.
func (d *Daemon) publishQuarantineAfterConsumingTerminalProof(orgID string) {
	if published := d.reconcileQuarantinedTombstones(); published[orgID] {
		return
	}
	d.publishSessionShimProjection(context.Background(), orgID)
}

// upsertShimQuarantineLocked adds one exact shim projection without allowing a
// repeated disconnect observer to double-charge capacity. Lifecycle identity is
// authoritative, while shim id distinguishes the D7 duplicate-identity case in
// which two real survivors under one identity must both remain visible.
// d.shims.mu must be held.
func (d *Daemon) upsertShimQuarantineLocked(q sessionshim.QuarantinedSession) {
	for i := range d.shims.quarantined {
		current := d.shims.quarantined[i]
		if current.Identity() == q.Identity() && current.ShimID == q.ShimID && current.ProcessEpoch == q.ProcessEpoch {
			d.shims.quarantined[i] = q
			sessionshim.SortQuarantined(d.shims.quarantined)
			return
		}
	}
	d.shims.quarantined = append(d.shims.quarantined, q)
	sessionshim.SortQuarantined(d.shims.quarantined)
}

// reconcileQuarantinedTombstones withdraws capacity only when a durable
// tombstone for the exact quarantined shim proves its harness group was reaped.
// It is called by the occupancy/reporting surfaces that already run on every
// admission and heartbeat, so reconciliation needs no unbounded background
// goroutine. The pass that OWNS a lineage's handoff reports it synchronously,
// bounded by the two callback timeouts that round trip costs (host identity,
// then the terminal evidence); every other pass skips immediately.
//
// It returns the scopes whose complete projection a handoff republished, so a
// caller about to publish the same scope for its own reason can see that the
// receiver already holds the current set.
func (d *Daemon) reconcileQuarantinedTombstones() map[string]bool {
	published := make(map[string]bool)
	if d.shims == nil {
		return published
	}
	d.shims.mu.RLock()
	registry := d.shims.registry
	quarantined := append([]sessionshim.QuarantinedSession(nil), d.shims.quarantined...)
	afterTombstoneFetch := d.shims.afterTombstoneFetch
	d.shims.mu.RUnlock()
	if registry == nil || len(quarantined) == 0 {
		return published
	}

	for _, q := range quarantined {
		id := q.Identity()
		tombstone, err := registry.GetTombstoneIncarnation(id, q.ShimID, q.ProcessEpoch)
		if err != nil || tombstone.ShimID != q.ShimID || tombstone.ProcessEpoch != q.ProcessEpoch {
			continue
		}
		// The question here is per-INCARNATION — "is this lineage's terminal
		// outcome durable enough to hand over?" — not the session-wide
		// claim-release question. §D10 names a group-reaped tombstone for that
		// exact harness process group as admissible proof, and reporting it is
		// not releasing anything: the composer resolves one obligation and
		// still decides release on its own complete set. Asking the
		// session-wide predicate here would refuse forever whenever the
		// identity also holds a live sibling, which is precisely the case that
		// produces a quarantined lineage beside a running session.
		if !tombstone.GroupReaped {
			continue
		}
		key := shimIncarnation{identity: id, shimID: tombstone.ShimID, processEpoch: tombstone.ProcessEpoch}
		if afterTombstoneFetch != nil {
			afterTombstoneFetch(key)
		}
		// One report per incarnation, even with several reconcile passes in
		// flight: every occupancy and heartbeat surface calls this, and two
		// passes reading the same tombstone would both commit it.
		//
		// A pass that does NOT own the handoff returns immediately. It used to
		// wait out the owner — but the owner's bound is a platform round trip
		// (the callback timeout), not the settle window, so the waiter stalled
		// an occupancy or heartbeat surface on a remote call it could not
		// shorten. Skipping is not merely cheaper, it is the CORRECT reading:
		// while the report is in flight the composer's obligation for this
		// lineage is still `active`, and its completeness cover-set is the
		// quarantined and cleared sections — so this lineage MUST keep being
		// projected as quarantined until the evidence lands. A skipped pass
		// leaves the row exactly where the composer still expects it.
		//
		// Owning the mark is NECESSARY but not sufficient: the mark is disposed
		// with the tombstone, so a pass descheduled since its fetch can claim a
		// lineage that is already gone. The handoff re-reads the quarantine
		// projection under the lock before it reports anything.
		own, _ := d.claimSessionShimTerminalReport(key, time.Now())
		if !own {
			continue
		}
		if d.handOffQuarantinedTerminalProof(registry, key, tombstone) {
			published[id.OrgID] = true
		}
	}
	return published
}

// handOffQuarantinedTerminalProof performs the durable terminal handoff for the
// ONE incarnation whose in-flight mark this caller already owns, and releases
// that mark however it leaves. It reports whether the lineage left the
// quarantine projection AND that scope's complete projection was republished
// to carry the change.
//
// The release is a defer keyed on a flag, not three explicit calls at the exits.
// This runs on the daemon's control-API handler goroutines, where net/http
// RECOVERS a panic raised inside a downstream callback — so an explicit release
// below a panicking HostIDForOrg or OnTerminalEvidence is simply never reached,
// the mark stays in flight for the rest of the daemon's life, every later pass
// answers "not mine" and skips, and the lineage stays projected quarantined and
// charged against capacity forever. The ordinary paths behave exactly as they
// did: the flag carries the same committed/refused answer they used to pass by
// hand, and a refusal (a panic included) still cools off before a retry.
func (d *Daemon) handOffQuarantinedTerminalProof(
	registry *sessionshim.Registry,
	key shimIncarnation,
	tombstone sessionshim.Tombstone,
) (republished bool) {
	id := key.identity
	committed := false
	forget := false
	defer func() {
		d.releaseSessionShimTerminalReport(key, committed, time.Now())
		if forget {
			d.forgetSessionShimTerminalReport(key)
		}
	}()

	// The claim proves no OTHER pass is mid-handoff; it does NOT prove this
	// lineage is still there to hand over. The tombstone is read BEFORE the
	// claim on purpose — a registry round trip must not run under d.shims.mu —
	// so a pass descheduled between the two can wake after the owner reported,
	// withdrew the row and, its proof now off disk, FORGOT the mark. The claim
	// then succeeds against an empty map and the stale tombstone this pass is
	// still holding gets committed a second time; measured as two terminal
	// observations for one incarnation on a loaded CI runner. d.shims.quarantined
	// is the source of truth for what may still be reported: if the exact
	// incarnation has left it, there is nothing to report and nothing to retry,
	// so the mark goes with the refusal rather than cooling off for a pass that
	// can never reach this lineage again.
	if !d.quarantineHoldsIncarnation(key) {
		forget = true
		return false
	}

	hostID, hostErr := d.sessionShimHostID(context.Background(), id.OrgID)
	if hostErr != nil {
		slog.Warn("session shim: retain quarantined terminal proof after host identity resolution failed",
			"session", id.String(), "error", hostErr)
		return false
	}
	// NO adoption correlation, even when this daemon still retains one.
	//
	// This lineage is being reported out of the QUARANTINE set, and the
	// obligation the composer holds for it is quarantined-kind: it resolves on
	// lifecycle identity plus shim id and process epoch. Attaching an adoption
	// receipt asks the receiver for the ADOPTED-kind predicate instead, which
	// matches nothing once the lineage was reported quarantined — measured on an
	// installed host as a terminal observation that committed while the
	// obligation stayed `active`, after which every complete batch was refused
	// `adoption_batch_live_lineage_omitted` and the host could not recover.
	terminalEvidence := SessionShimTerminalEvidence{
		Identity:     id,
		HostID:       hostID,
		ShimID:       tombstone.ShimID,
		ProcessEpoch: tombstone.ProcessEpoch,
		Tombstone:    tombstone,
	}
	if err := d.reportSessionShimTerminalEvidence(context.Background(), terminalEvidence); err != nil {
		slog.Warn("session shim: retain quarantined terminal proof after durable evidence refusal",
			"session", id.String(), "error", err)
		return false
	}
	committed = true

	// ONLY NOW. The evidence is durably accepted, so the composer has resolved
	// this lineage's quarantined obligation and the row may leave the quarantine
	// projection for the terminal set.
	if !d.withdrawQuarantinedLineageAfterDurableHandoff(key, tombstone) {
		// The row left by another route while the report was in flight. The
		// evidence committed either way, but THIS pass withdrew nothing and
		// recorded nothing in the terminal set — so the on-disk proof stays
		// where it is. Disposing it here would delete the only artifact
		// SessionShimTerminalProof can still read for this incarnation and turn
		// a proven death back into an unresolved one; a later pass, or the next
		// startup, re-proves it from disk. The mark may go: the reconcile only
		// ever reaches a lineage through the quarantine set, and this one is no
		// longer in it.
		forget = true
		return false
	}

	// The daemon's quarantine set just changed, and the receiver's did not:
	// accepting the terminal evidence resolved this lineage's obligation but
	// nothing on that side prunes the host row's quarantine snapshot — only a
	// batch moves it. Without this publish the next beat carries
	// `quarantined=[]` against a row still holding `[X]` at the same revision,
	// is refused stale, and demotes the host to draining on every beat until
	// something else republishes. Measured on an installed host as ten minutes
	// of 409s that ended only with a restart. The publish is fire-and-forget
	// for the same reason the disconnect path's is: a refusal is logged and
	// the next adoption, tombstone, or reconciliation republishes the same
	// projection.
	d.publishSessionShimProjection(context.Background(), id.OrgID)
	republished = true

	// Withdraw the liveness claim BEFORE disposing the proof, exactly as
	// finishAdoptedShim does. Disposing first would collapse "terminal, proven"
	// into "a record whose process is gone", which §D10 classifies as stale and
	// leaves unresolved. Remove is idempotent, so the ordinary case where the
	// shim already withdrew its own record costs nothing.
	if err := registry.RemoveIncarnation(id, tombstone.ShimID, tombstone.ProcessEpoch); err != nil {
		slog.Warn("session shim: withdraw discovery record after durable terminal handoff",
			"session", id.String(), "error", err)
		return republished
	}
	if err := registry.RemoveTombstoneIncarnation(tombstone); err != nil {
		slog.Warn("session shim: dispose quarantined tombstone after durable terminal handoff",
			"session", id.String(), "error", err)
		return republished
	}
	// The captured stdout/stderr file is disposed alongside the record/
	// tombstone above — see removeShimChildLog's doc comment.
	removeShimChildLog(registry.Dir(), id)
	// The proof is off disk, so no later pass can rediscover this incarnation
	// and no mark is needed to stop it. Keeping one would make this map grow for
	// the daemon's whole life.
	forget = true
	return republished
}

// quarantineHoldsIncarnation answers whether the EXACT incarnation is still in
// the quarantine projection. That projection is the only route a reconcile pass
// has to a lineage, so it is also the answer to "may this pass still report it?"
// — a pass holding a tombstone the projection no longer names is holding a fact
// another pass has already handed over.
func (d *Daemon) quarantineHoldsIncarnation(key shimIncarnation) bool {
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	for _, current := range d.shims.quarantined {
		if current.Identity() == key.identity && current.ShimID == key.shimID &&
			current.ProcessEpoch == key.processEpoch {
			return true
		}
	}
	return false
}

// withdrawQuarantinedLineageAfterDurableHandoff moves one exact incarnation out
// of the quarantine projection and into the terminal set, as ONE locked
// transition, AFTER its terminal evidence is durably accepted. It reports false
// when the row had already left by another route; the adoption correlation for
// the incarnation is dropped unconditionally either way, but nothing is
// appended to the terminal set in that case.
//
// The ORDER is the contract, not an implementation detail. Doing this before
// the report — to spare the other reconcile passes a wait — makes every
// projection built during the round trip report the lineage as tombstoned while
// the composer's obligation for it is still `active`; the composer's
// completeness cover-set for an active quarantined obligation is the batch's
// quarantined and cleared sections, so every concurrent publish is refused as a
// batch that omitted a live lineage, and the acceptance clear's own
// "not quarantined AND tombstoned" break fires mid-handoff and then commits
// exactly that illegal batch. A lineage whose report is in flight is still
// quarantined, and every surface must keep saying so.
func (d *Daemon) withdrawQuarantinedLineageAfterDurableHandoff(
	key shimIncarnation,
	tombstone sessionshim.Tombstone,
) bool {
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
	// The correlation goes whether or not this daemon still held the row. The
	// caller only reaches here AFTER the terminal evidence was durably accepted,
	// and a retained adoption correlation for a lineage the composer has already
	// resolved is what makes a later batch attach an ADOPTED-kind receipt to a
	// lineage the receiver only knows as quarantined.
	delete(d.shims.correlations, key)
	if !removed {
		return false
	}
	// forwarded is keyed by LIFECYCLE IDENTITY, not by incarnation, and one
	// identity can hold a quarantined lineage and a live adopted one at the
	// same time (§D7's duplicate-identity case). Dropping the durable
	// high-water because a SIBLING incarnation terminalized would regress the
	// surviving session's fence correlation to zero.
	if _, stillAdopted := d.shims.adopted[key.identity]; !stillAdopted && !remainingForIdentity {
		delete(d.shims.forwarded, key.identity)
	}
	for _, existing := range d.shims.tombstoned {
		if existing.Identity() == key.identity && existing.ShimID == key.shimID &&
			existing.ProcessEpoch == key.processEpoch {
			return true
		}
	}
	d.shims.tombstoned = append(d.shims.tombstoned, tombstone)
	return true
}

// sessionShimTerminalReportBackoff is the cool-down after a refused durable
// handoff. It is DERIVED from the acceptance clear's own settle window so one
// clear can spend at most a handful of commit attempts on a lineage the
// composer is refusing: an unthrottled poller turned a single refusal into
// hundreds of POSTs of the same evidence.
const sessionShimTerminalReportBackoff = tombstoneSettleWindow / 5

// forgetSessionShimTerminalReport drops one incarnation's handoff mark once its
// tombstone is off disk. Nothing can rediscover the lineage after that, so the
// mark has no reader left — and this map is consulted from every occupancy and
// heartbeat surface, so an entry per lineage that never leaves is a leak for
// the daemon's whole life.
func (d *Daemon) forgetSessionShimTerminalReport(key shimIncarnation) {
	d.shims.mu.Lock()
	delete(d.shims.reportingTerminal, key)
	d.shims.mu.Unlock()
}

// claimSessionShimTerminalReport takes ownership of one incarnation's durable
// terminal handoff. It returns (true, nil) to the one pass that owns it; to any
// other pass it returns false plus the channel a WAITING caller would use when
// a handoff is in flight, or a nil channel when there is nothing to wait for
// (the handoff already committed, or a refusal is cooling off). No production
// caller waits — see reconcileQuarantinedTombstones — but the channel is still
// closed on release so a future one could without a second mechanism.
func (d *Daemon) claimSessionShimTerminalReport(key shimIncarnation, now time.Time) (bool, <-chan struct{}) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	state := d.shims.reportingTerminal[key]
	switch {
	case state.committed:
		return false, nil
	case state.inFlight != nil:
		return false, state.inFlight
	case now.Before(state.retryAt):
		return false, nil
	}
	state.inFlight = make(chan struct{})
	d.shims.reportingTerminal[key] = state
	return true, nil
}

// releaseSessionShimTerminalReport clears the in-flight mark and wakes anyone
// waiting on it. A handoff that COMMITTED keeps a permanent mark: the evidence
// is durable, and a later pass reading a not-yet-disposed tombstone must not
// commit it a second time. A refused one is retried, but only after the backoff.
func (d *Daemon) releaseSessionShimTerminalReport(key shimIncarnation, committed bool, now time.Time) {
	d.shims.mu.Lock()
	state := d.shims.reportingTerminal[key]
	done := state.inFlight
	state.inFlight = nil
	if committed {
		state.committed = true
		state.retryAt = time.Time{}
	} else {
		state.retryAt = now.Add(sessionShimTerminalReportBackoff)
	}
	d.shims.reportingTerminal[key] = state
	d.shims.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// WriteAdoptedSessionShimInput sends attributed input bytes to one adopted
// session under this daemon's generation.
//
// Refusing a session this daemon has not adopted is the same rule Stop follows:
// quarantine grants no input authority (§D7), and reaching a quarantined shim by
// another route is exactly the "kill instead of quarantine" behaviour the ADR
// rejects, in a quieter form.
func (d *Daemon) WriteAdoptedSessionShimInput(orgID, sessionID string, data []byte) error {
	entry, err := d.adoptedShimEntry(orgID, sessionID)
	if err != nil {
		return err
	}
	return entry.controller.WriteInput(data)
}

// WriteAdoptedSessionShimInputFor sends input only when the complete captured
// adoption authority is still current. The controller's wire request carries
// the same generation, so a replacement racing after the local comparison also
// rejects the old controller rather than applying the input to new authority.
func (d *Daemon) WriteAdoptedSessionShimInputFor(ref SessionShimControlRef, data []byte) error {
	controller, err := d.adoptedSessionShimControllerFor(ref)
	if err != nil {
		return err
	}
	return controller.WriteInput(data)
}

// WriteAdoptedSessionShimInputAttributed is WriteAdoptedSessionShimInput's
// attributed twin: it additionally carries userID — a composing carrier's
// relay-stamped sender identity — through to Controller.WriteAttributedInput
// (sessionshim/controller.go), which the shim uses to apply last-hop
// pacing/paste-guard to SYSTEM-authority input (ptyhost/systeminput.go,
// attachwire.SystemNudgeUserID) that a stalled leg let queue and arrive
// back-to-back. A controller negotiated below selected v4 degrades to the
// exact byte-identical WriteInput send WriteAdoptedSessionShimInput has
// always made — the write still lands, verbatim; only the last-hop guarantee
// is unavailable there. WriteAdoptedSessionShimInput itself is unchanged.
func (d *Daemon) WriteAdoptedSessionShimInputAttributed(orgID, sessionID, userID string, data []byte) error {
	entry, err := d.adoptedShimEntry(orgID, sessionID)
	if err != nil {
		return err
	}
	return entry.controller.WriteAttributedInput([]byte(userID), data)
}

// WriteAdoptedSessionShimInputAttributedFor is
// WriteAdoptedSessionShimInputFor's attributed twin — see
// WriteAdoptedSessionShimInputAttributed for what userID does and the
// selected-v4 degrade rule. WriteAdoptedSessionShimInputFor itself is
// unchanged.
func (d *Daemon) WriteAdoptedSessionShimInputAttributedFor(ref SessionShimControlRef, userID string, data []byte) error {
	controller, err := d.adoptedSessionShimControllerFor(ref)
	if err != nil {
		return err
	}
	return controller.WriteAttributedInput([]byte(userID), data)
}

// ResizeAdoptedSessionShim sends authoritative geometry under this daemon's
// generation.
func (d *Daemon) ResizeAdoptedSessionShim(orgID, sessionID string, cols, rows, pxWidth, pxHeight uint32) error {
	entry, err := d.adoptedShimEntry(orgID, sessionID)
	if err != nil {
		return err
	}
	return entry.controller.Resize(cols, rows, pxWidth, pxHeight)
}

// ResizeAdoptedSessionShimFor applies geometry only under the complete current
// adoption authority captured by the composing carrier callback.
func (d *Daemon) ResizeAdoptedSessionShimFor(
	ref SessionShimControlRef,
	cols, rows, pxWidth, pxHeight uint32,
) error {
	controller, err := d.adoptedSessionShimControllerFor(ref)
	if err != nil {
		return err
	}
	return controller.Resize(cols, rows, pxWidth, pxHeight)
}

// StopAdoptedSessionShimFor stops only the exact current adopted incarnation.
// The identity-only StopAdoptedSessionShim remains for compatibility; composed
// carrier callbacks should retain and use this stronger authority.
func (d *Daemon) StopAdoptedSessionShimFor(ref SessionShimControlRef, reason shimwire.StopReason) error {
	controller, err := d.adoptedSessionShimControllerFor(ref)
	if err != nil {
		return err
	}
	return controller.Stop(reason)
}

func (d *Daemon) adoptedSessionShimControllerFor(ref SessionShimControlRef) (*sessionshim.Controller, error) {
	if err := ref.Identity.Validate(); err != nil || ref.ShimID == "" || ref.ProcessEpoch == 0 || ref.ControllerGeneration == 0 {
		return nil, errors.New("session shim: control reference is incomplete")
	}
	if d.shims == nil {
		return nil, errors.New("session shim: adoption is not configured")
	}
	d.shims.mu.RLock()
	entry, ok := d.shims.adopted[ref.Identity]
	d.shims.mu.RUnlock()
	if !ok || entry.controller == nil {
		return nil, fmt.Errorf("session shim: control reference is not current for %s", ref.Identity)
	}
	hello := entry.controller.Hello()
	if entry.adoption.Identity != ref.Identity || entry.controller.Identity() != ref.Identity ||
		entry.shimID != ref.ShimID || hello.ShimID != ref.ShimID ||
		entry.adoption.ProcessEpoch != ref.ProcessEpoch || hello.ProcessEpoch != ref.ProcessEpoch ||
		entry.adoption.ControllerGeneration != ref.ControllerGeneration ||
		uint64(entry.controller.Generation()) != ref.ControllerGeneration {
		return nil, fmt.Errorf("session shim: control reference is stale for %s", ref.Identity)
	}
	return entry.controller, nil
}

// adoptedShimEntry resolves one adopted session with a live controller.
func (d *Daemon) adoptedShimEntry(orgID, sessionID string) (adoptedShim, error) {
	if d.shims == nil {
		return adoptedShim{}, errors.New("session shim: adoption is not configured")
	}
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	d.shims.mu.RLock()
	entry, ok := d.shims.adopted[id]
	d.shims.mu.RUnlock()
	if !ok {
		return adoptedShim{}, fmt.Errorf("session shim: %s is not adopted by this daemon", id)
	}
	if entry.controller == nil {
		return adoptedShim{}, fmt.Errorf("session shim: %s has no live controller connection", id)
	}
	return entry, nil
}

type sessionShimStopResult uint8

const (
	sessionShimStopNotFound sessionShimStopResult = iota
	sessionShimStopHandled
	sessionShimStopRefused
)

// stopSessionShimByID routes a plain session id — what the control API and the
// spawner speak — to the generation-fenced Stop on an adopted shim.
//
// It reports whether the session was shim-owned at all, so the caller can fall
// through to the direct-child path for everything else rather than having to
// know which ownership model a session uses.
func (d *Daemon) stopSessionShimByID(sessionID string) sessionShimStopResult {
	if d.shims == nil {
		return sessionShimStopNotFound
	}
	d.shims.mu.RLock()
	matches := make([]sessionshim.Identity, 0, 1)
	for id := range d.shims.adopted {
		if id.SessionID == sessionID {
			matches = append(matches, id)
		}
	}
	d.shims.mu.RUnlock()
	if len(matches) == 0 {
		return sessionShimStopNotFound
	}
	if len(matches) != 1 {
		// The localhost API supplies only a session id. Do not collapse two
		// tenant-scoped lifecycle identities or pick one by map order.
		slog.Error("session shim: bare session id is ambiguous across organizations",
			"matches", len(matches))
		return sessionShimStopRefused
	}
	id := matches[0]
	if err := d.StopAdoptedSessionShim(id.OrgID, id.SessionID, shimwire.StopOperator); err != nil {
		slog.Error("session shim: stop", "session", id.String(), "error", err)
		return sessionShimStopRefused
	}
	return sessionShimStopHandled
}

// sessionShimHandles projects every shim-backed session onto the same
// SessionHandle shape the direct-spawn path publishes.
//
// Quarantined shims are included, and that is §D7 held to at the surface an
// operator actually looks at: a quarantined shim is a running harness this
// daemon cannot control, and a session list that omitted it would show an
// occupied host as idle. Their state is "running" — because it is — with a PID
// of 0, since this daemon never negotiated with them and does not know one.
func (d *Daemon) sessionShimHandles() []SessionHandle {
	if d.shims == nil {
		return nil
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	out := make([]SessionHandle, 0, len(d.shims.adopted)+len(d.shims.quarantined))
	for id, entry := range d.shims.adopted {
		handle := entry.handle
		if handle.SessionID == "" {
			// Adopted at startup rather than launched here: this daemon has the
			// identity and the shim's report, not the original spec.
			handle = SessionHandle{SessionID: id.SessionID, State: SessionRunning}
			if entry.controller != nil {
				handle.PID = entry.controller.HarnessIdentity().PID
				handle.WorktreePath = entry.controller.Hello().WorkareaPath
				handle.WorkareaRoot = entry.controller.WorkareaRoot()
				if handle.WorkareaRoot == "" {
					handle.WorkareaRoot = handle.WorktreePath
				}
				if started := entry.controller.Hello().ProcessStartedAt; started > 0 {
					handle.AcceptedAt = time.Unix(0, started).UTC().Format(time.RFC3339)
				}
			}
		}
		out = append(out, handle)
	}
	for _, q := range d.shims.quarantined {
		out = append(out, SessionHandle{
			SessionID: q.SessionID,
			State:     SessionRunning,
		})
	}
	return out
}

// shimNow is the daemon's clock for shim bookkeeping, routed through the
// spawner's injectable clock so shim-backed and direct handles are stamped from
// the same source in tests.
func (d *Daemon) shimNow() time.Time {
	if d.spawner != nil && d.spawner.opts.Now != nil {
		return d.spawner.opts.Now()
	}
	return time.Now()
}

// sessionShimRegistry opens the registry once and reuses it for the daemon's
// lifetime, so the launch path and the startup adoption pass cannot end up
// pointed at two different directories.
func (d *Daemon) sessionShimRegistry() (*sessionshim.Registry, error) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	if d.shims.registry != nil {
		return d.shims.registry, nil
	}
	registry, err := sessionshim.NewRegistry(d.sessionShimConfig().RegistryDir)
	if err != nil {
		return nil, fmt.Errorf("session shim: open registry: %w", err)
	}
	d.shims.registry = registry
	return registry, nil
}

// envPairs renders an overlay map as KEY=VALUE entries, sorted so a spawn
// environment is byte-stable across runs.
func envPairs(overlay map[string]string) []string {
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+overlay[k])
	}
	return out
}

// waitShimConsumers joins every background event consumer. Tests use it to
// observe a fully settled daemon; Stop uses it so a shutdown cannot race a
// consumer that is still writing bookkeeping.
func (d *Daemon) waitShimConsumers() {
	if d.shims == nil {
		return
	}
	d.shims.wg.Wait()
}
