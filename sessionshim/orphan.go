package sessionshim

import (
	"errors"
	"fmt"
	"time"
)

// Default bounds for the shim-owned orphan rule (ADR-2026-08-17 §D8).
const (
	// DefaultOrphanDeadline is how long a shim keeps running after losing its
	// controller before it terminates and reaps its own harness process group.
	//
	// §D8 fixes an INEQUALITY, not a number, and the number is a policy choice
	// inside it (Validate enforces the inequality; the first implementation
	// chose 90 seconds). Ninety seconds turned out to be a choice that answers
	// the wrong question: it is far shorter than the time a control plane can
	// take to come back, so every transient stall that cost a shim its
	// controller also cost a live harness its work. Measured on an installed
	// host: two healthy sessions were dropped by a slow durable write and both
	// self-terminated ninety seconds later, with nothing wrong on either end.
	//
	// The deadline exists to bound DOUBLE EXECUTION, and it bounds that exactly
	// as well at fifteen minutes as at ninety seconds — as long as the
	// inequality still holds against whatever external threshold a composing
	// deployment declares, which Validate checks at startup and refuses to
	// start without. The window is long enough for a daemon restart, a control
	// plane outage, or an operator to intervene; a session that is genuinely
	// abandoned is still reaped, just not eagerly. Deployments that need a
	// tighter bound set OrphanPolicy.Deadline (the daemon also reads
	// DONMAI_SESSION_SHIM_ORPHAN_DEADLINE_MS).
	DefaultOrphanDeadline = 15 * time.Minute

	// DefaultTerminationGrace is the SIGTERM→SIGKILL window the shim uses when
	// the deadline fires. It matches the PTY host's own stop grace.
	DefaultTerminationGrace = 5 * time.Second

	// DefaultPropagationMargin covers clock skew between this host and whatever
	// external component holds a claim, plus the time for a terminal observation
	// to propagate.
	DefaultPropagationMargin = 30 * time.Second
)

// ErrOrphanPolicyUnsafe reports a configuration that violates the §D8
// inequality. It is returned at STARTUP, not at deadline time, because a
// configuration that can produce double execution must prevent session
// admission rather than be discovered when it already has.
var ErrOrphanPolicyUnsafe = errors.New("sessionshim: orphan policy violates the double-execution bound")

// OrphanPolicy is the shim-owned bounded-orphan configuration.
//
// The contract §D8 states is an INEQUALITY, not a number:
//
//	Deadline + TerminationGrace + PropagationMargin < ExternalReleaseThreshold
//
// Read plainly: by the time anything outside this host is willing to consider a
// session abandoned and release its claim, the shim must already have finished
// killing and reaping the harness. If that ordering inverts, an external
// component can hand the same work to another host while the original harness is
// still running — double execution, from a configuration change alone.
//
// ExternalReleaseThreshold is zero for an OSS-only daemon: nothing external can
// release a claim, so the inequality has no upper bound to violate and Validate
// only checks internal sanity.
type OrphanPolicy struct {
	Deadline                 time.Duration
	TerminationGrace         time.Duration
	PropagationMargin        time.Duration
	ExternalReleaseThreshold time.Duration
}

// DefaultOrphanPolicy returns the OSS-only defaults (no external threshold).
func DefaultOrphanPolicy() OrphanPolicy {
	return OrphanPolicy{
		Deadline:          DefaultOrphanDeadline,
		TerminationGrace:  DefaultTerminationGrace,
		PropagationMargin: DefaultPropagationMargin,
	}
}

// TotalBound is the left-hand side of the §D8 inequality: the longest time
// between controller loss and a proven-reaped harness.
func (p OrphanPolicy) TotalBound() time.Duration {
	return p.Deadline + p.TerminationGrace + p.PropagationMargin
}

// Validate enforces the §D8 inequality.
//
// This is called from daemon startup and from shim construction. A daemon whose
// policy fails here must not admit sessions — refusing to start is the correct
// response to a configuration that can silently produce double execution.
func (p OrphanPolicy) Validate() error {
	if p.Deadline <= 0 {
		return fmt.Errorf("%w: deadline must be positive, got %s", ErrOrphanPolicyUnsafe, p.Deadline)
	}
	if p.TerminationGrace < 0 || p.PropagationMargin < 0 {
		return fmt.Errorf("%w: negative grace or margin", ErrOrphanPolicyUnsafe)
	}
	if p.ExternalReleaseThreshold <= 0 {
		// No external releaser: the local rule stands alone and cannot race one.
		return nil
	}
	if total := p.TotalBound(); total >= p.ExternalReleaseThreshold {
		return fmt.Errorf(
			"%w: orphan deadline %s + termination grace %s + propagation margin %s = %s, "+
				"which is not strictly less than the smallest external release threshold %s — "+
				"an external component could release the claim while the harness is still running",
			ErrOrphanPolicyUnsafe, p.Deadline, p.TerminationGrace, p.PropagationMargin, total, p.ExternalReleaseThreshold,
		)
	}
	return nil
}

// WithExternalThreshold returns a copy carrying a newly-learned external release
// threshold.
//
// A LATER reduction of an external threshold must fail this check before
// rollout, not silently invalidate the inequality (§D8) — so this returns a copy
// for the caller to Validate rather than mutating in place and hoping someone
// re-checks.
func (p OrphanPolicy) WithExternalThreshold(d time.Duration) OrphanPolicy {
	p.ExternalReleaseThreshold = d
	return p
}
