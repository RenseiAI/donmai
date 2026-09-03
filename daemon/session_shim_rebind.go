package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/RenseiAI/donmai/sessionshim"
)

// Typed refusals from the adopted-lineage lookup and the rebind seam. They
// exist so an embedder can switch on the CAUSE rather than match substrings:
// before them, "unknown session", "no live controller" and "adoption is not
// configured" were three bare strings from one *errors.errorString.
//
// Their wording composes into the sentence each call site produced before the
// sentinel existed — `session shim: <id> is not adopted by this daemon` — so
// adding errors.Is support changed no message a caller or a pin reads.
var (
	// ErrSessionShimAdoptionNotConfigured reports a daemon with no session-shim
	// state at all: nothing was ever adopted because adoption is not composed.
	ErrSessionShimAdoptionNotConfigured = errors.New("adoption is not configured")
	// ErrSessionShimNotAdopted reports a lifecycle identity this daemon does not
	// hold.
	ErrSessionShimNotAdopted = errors.New("not adopted by this daemon")
	// ErrSessionShimNoController reports an adopted lineage whose controller
	// connection is gone, so nothing can be asked of the shim through it.
	ErrSessionShimNoController = errors.New("has no live controller connection")
	// ErrSessionShimRebindInProgress reports a rebind refused because another
	// caller's re-adoption of the same lineage is still in flight. It is
	// returned alongside SessionShimRebindInProgress so a caller that only
	// checks the error still learns the refusal was benign.
	ErrSessionShimRebindInProgress = errors.New("a rebind of this lineage is already in flight")
)

// SessionShimRebindResult is what RebindAdoptedSessionShim did, as a value
// rather than as the absence of an error. "Rebound" and "already bound" are
// both successes and an embedder driving a repair loop has to tell them apart;
// so are "not adopted" and "someone else is doing it", which are refusals a
// caller should not retry the same way.
type SessionShimRebindResult uint8

const (
	// SessionShimRebindUnknown is the zero value, returned only alongside an
	// error that classifies nothing else.
	SessionShimRebindUnknown SessionShimRebindResult = iota
	// SessionShimRebound reports a completed daemon-side re-adoption: a fresh
	// dial, a strictly newer controller generation, durable adoption, a complete
	// batch, and carrier activation. The receiver has been told.
	SessionShimRebound
	// SessionShimAlreadyBound reports a lineage whose carrier binding this
	// daemon already believes holds. Nothing was dialled and no hook ran.
	SessionShimAlreadyBound
	// SessionShimNotAdopted reports a lifecycle identity this daemon does not
	// hold. Returned with an error wrapping ErrSessionShimNotAdopted.
	SessionShimNotAdopted
	// SessionShimRebindInProgress reports a concurrent caller's re-adoption of
	// the same lineage. Exactly one of them performs the operation.
	SessionShimRebindInProgress
)

// String names the result for logs and diagnostics.
func (r SessionShimRebindResult) String() string {
	switch r {
	case SessionShimRebound:
		return "rebound"
	case SessionShimAlreadyBound:
		return "already_bound"
	case SessionShimNotAdopted:
		return "not_adopted"
	case SessionShimRebindInProgress:
		return "rebind_in_progress"
	default:
		return "unknown"
	}
}

// RebindAdoptedSessionShim re-adopts ONE lineage this daemon still holds whose
// carrier binding did not survive the carrier's return.
//
// The state it repairs is specific and otherwise invisible: the lineage is
// adopted, its shim and harness are healthy, its controller socket is open —
// and the composing carrier's side of the binding never completed after the
// carrier came back, so nothing reaches the session. Every membership
// projection reports it as fine. AdoptedSessionShimBindings and
// SessionShimDiagnostics carry CarrierBound so the state is detectable, and
// SessionShimConfig.OnSessionShimCarrierBindLost raises it the moment the
// daemon learns of it; this is the repair those two point at.
//
// It performs a REAL daemon-side operation, not a callback trampoline: the same
// re-adoption pipeline a carrier fault runs — a fresh dial, a strictly newer
// generation, durable adoption, a complete batch, carrier activation — filtered
// to this identity. The receiver therefore learns the lineage is bound again
// from the batch, which is what makes the result observable from outside this
// process. SessionShimConfig.OnSessionShimRebind, when set, runs afterwards so
// a composition can re-establish whatever it binds alongside the adoption; a
// hook error is returned, and the daemon-side re-adoption it follows has
// already committed by then.
//
// It is idempotent and concurrency-safe. A lineage this daemon believes is
// bound answers SessionShimAlreadyBound with nothing dialled and no hook run. A
// second caller arriving while a re-adoption is in flight answers
// SessionShimRebindInProgress rather than driving a second one — the claim and
// the bind check are one critical section under d.shims.mu, and that lock is
// released before any network work begins.
func (d *Daemon) RebindAdoptedSessionShim(ctx context.Context, orgID, sessionID string) (SessionShimRebindResult, error) {
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	if err := id.Validate(); err != nil {
		return SessionShimRebindUnknown, fmt.Errorf("session shim: rebind: %w", err)
	}
	entry, err := d.adoptedShimEntry(orgID, sessionID)
	if err != nil {
		result := SessionShimRebindUnknown
		if errors.Is(err, ErrSessionShimNotAdopted) || errors.Is(err, ErrSessionShimAdoptionNotConfigured) {
			result = SessionShimNotAdopted
		}
		return result, fmt.Errorf("session shim: rebind %s: %w", id, err)
	}
	claimed, refusal := d.claimSessionShimRebind(id, entry)
	if !claimed {
		switch refusal {
		case SessionShimAlreadyBound:
			return SessionShimAlreadyBound, nil
		case SessionShimRebindInProgress:
			return SessionShimRebindInProgress, fmt.Errorf("session shim: rebind %s: %w", id, ErrSessionShimRebindInProgress)
		default:
			return SessionShimNotAdopted, fmt.Errorf("session shim: rebind %s: %w", id, ErrSessionShimNotAdopted)
		}
	}
	defer d.releaseSessionShimRebindClaim(id)

	registry, err := d.sessionShimRegistry()
	if err != nil {
		return SessionShimRebindUnknown, fmt.Errorf("session shim: rebind %s: %w", id, err)
	}
	cfg := d.sessionShimConfig()
	// The lost entry's own re-adoption instant is carried through UNCHANGED.
	// Stamping "now" here would spend the automatic re-adoption budget on an
	// audited human action: the re-entry guard refuses a second automatic
	// re-adoption inside one window, so a carrier fault a minute after an
	// operator repaired the binding would be quarantined instead of re-adopted,
	// and the shim would reap a healthy harness. The guard exists to stop a
	// flapping carrier from spending adoption revisions in a retry loop, which
	// is not what a rebind is.
	if err := d.readoptSessionShimOnce(
		ctx, registry, cfg, id, entry, entry.controller.Hello(), entry.readoptedAtUnixNano,
	); err != nil {
		return SessionShimRebindUnknown, fmt.Errorf("session shim: rebind %s: %w", id, err)
	}
	// The shim closed the previous connection the moment the new generation
	// committed; closing this end too releases the fd rather than leaving a
	// half-dead socket for the consumer goroutine to discover later.
	_ = entry.controller.Close()
	slog.Info("session shim: re-adopted a lineage on request to repair its carrier binding", "session", id.String())
	if cfg.OnSessionShimRebind != nil {
		if err := cfg.OnSessionShimRebind(ctx, id); err != nil {
			return SessionShimRebindUnknown, fmt.Errorf("session shim: rebind %s: on-rebind hook: %w", id, err)
		}
	}
	return SessionShimRebound, nil
}

// claimSessionShimRebind takes the one in-flight rebind slot for a lineage, or
// reports why it did not.
//
// The bind check and the claim are ONE critical section deliberately: checking
// "already bound" outside the lock and taking the claim inside it is how two
// concurrent callers each passed the check and then each drove a re-adoption,
// serialized only by the lock they took afterwards.
func (d *Daemon) claimSessionShimRebind(id sessionshim.Identity, expected adoptedShim) (bool, SessionShimRebindResult) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	current, ok := d.shims.adopted[id]
	if !ok || current.controller != expected.controller {
		return false, SessionShimNotAdopted
	}
	if current.rebinding {
		return false, SessionShimRebindInProgress
	}
	if current.carrierBound {
		return false, SessionShimAlreadyBound
	}
	current.rebinding = true
	d.shims.adopted[id] = current
	return true, SessionShimRebindUnknown
}

// releaseSessionShimRebindClaim clears the in-flight mark, whatever the
// re-adoption did. It is deliberately tolerant of the entry having been
// replaced meanwhile — the re-adoption itself swaps the entry — and clears the
// mark on whatever is there now, because a mark left behind would refuse every
// later repair of this lineage.
func (d *Daemon) releaseSessionShimRebindClaim(id sessionshim.Identity) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	if current, ok := d.shims.adopted[id]; ok && current.rebinding {
		current.rebinding = false
		d.shims.adopted[id] = current
	}
}

// noteSessionShimCarrierBindLost clears a lineage's bind state and stamps the
// loss instant, reporting whether this call is the one that observed the
// transition. Only that caller raises OnSessionShimCarrierBindLost, so an
// embedder is told once per loss rather than once per code path that notices.
func (d *Daemon) noteSessionShimCarrierBindLost(id sessionshim.Identity, ctrl *sessionshim.Controller) bool {
	d.shims.mu.Lock()
	current, ok := d.shims.adopted[id]
	if !ok || (ctrl != nil && current.controller != ctrl) || !current.carrierBound {
		d.shims.mu.Unlock()
		return false
	}
	current.carrierBound = false
	current.carrierLostAtUnixNano = d.shimNow().UnixNano()
	d.shims.adopted[id] = current
	d.shims.mu.Unlock()
	return true
}

// raiseSessionShimCarrierBindLost delivers the bind-lost notification under the
// configured callback bound, off the daemon's lock.
func (d *Daemon) raiseSessionShimCarrierBindLost(cfg SessionShimConfig, id sessionshim.Identity) {
	if cfg.OnSessionShimCarrierBindLost == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.callbackTimeout())
	defer cancel()
	cfg.OnSessionShimCarrierBindLost(ctx, id)
}
