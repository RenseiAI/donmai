package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultSessionShimReadinessStaleAfter bounds how long readiness may stay
// unknown before it is treated as a definite not-ready with reason "stale".
// SessionShimConfig.ReadinessStaleAfter overrides it.
const DefaultSessionShimReadinessStaleAfter = 10 * time.Minute

const (
	// sessionShimReadinessCadence is the maximum age of a cached readiness
	// sample served to a consumer that is not the heartbeat. It is a CONSTANT,
	// deliberately not derived from the heartbeat interval the control plane
	// negotiates: the beat resolves live on every beat, so the interval between
	// resolutions is min(negotiated interval, this constant), and a definite
	// not-ready is therefore always visible to every other seam within one
	// heartbeat interval whatever interval was negotiated. A composer that
	// negotiates something other than the 30s default gets two cadences rather
	// than one — its own on the beat, this one everywhere else.
	sessionShimReadinessCadence = 30 * time.Second

	// sessionShimReadinessResolveNow asks for a resolution rather than a cached
	// sample. The heartbeat seam uses it: the beat IS this host's readiness
	// cadence, which is what keeps every other consumer off the resolver.
	sessionShimReadinessResolveNow = time.Duration(0)

	// The retry backoff after a failed resolution: 5s doubling to a 30s cap. It
	// SHORTENS the cadence rather than lengthening it — while readiness is
	// unknown an ordinary consumer may retry after the backoff instead of
	// waiting out the full healthy cadence, so recovery is not held back.
	sessionShimReadinessRetryBase = 5 * time.Second
	sessionShimReadinessRetryMax  = 30 * time.Second

	// sessionShimReadinessReasonLimit bounds the published reason so a verbose
	// embedder error cannot inflate every beat.
	sessionShimReadinessReasonLimit = 256
)

// sessionShimReadinessSample is one readiness answer as every seam sees it: the
// resolved facts (populated only while ready), the tri-state it projects, and
// the blocking error, if any, that a new-work seam must fail on.
type sessionShimReadinessSample struct {
	readiness SessionShimCarrierProofV2Readiness
	// state is "" while readiness is established, and otherwise
	// SessionShimReadinessNotReady or SessionShimReadinessUnknown. Empty rather
	// than "ready" so a ready projection serializes exactly as it did before
	// the tri-state existed.
	state  string
	reason string
	// stateSince is when the current non-ready state was first observed. Zero
	// while ready. A continuing unknown carries the onset forward across
	// retries, which is the instant the staleness bound measures from.
	stateSince time.Time
	// blocking is non-nil for a state that refuses new work: a definite
	// not-ready, a permanent resolver misconfiguration, or an unknown on a host
	// that has never established readiness at all. It is nil while readiness is
	// established and while an ESTABLISHED readiness is merely unknown — that
	// distinction is the whole rule this type exists to carry. "Must not
	// withdraw" is a statement about a readiness this host once proved; a host
	// that has never proved one has nothing to keep serving on.
	blocking error
}

// observedAt renders stateSince for the wire. Ready samples carry no timestamp.
func (s sessionShimReadinessSample) observedAt() string {
	if s.stateSince.IsZero() {
		return ""
	}
	return s.stateSince.UTC().Format(time.RFC3339Nano)
}

// sessionShimReadinessCache is the single resolved readiness answer this host
// shares with every consumer. Guarded by Daemon.readinessMu; the resolver
// itself is never called while that lock is held.
type sessionShimReadinessCache struct {
	sample     sessionShimReadinessSample
	valid      bool
	resolvedAt time.Time
	// failures counts consecutive unavailable resolutions. It selects the retry
	// backoff and resets on any resolution that produced an answer.
	failures int
	// unknownSince is when the current run of unavailable resolutions began,
	// and staleSince when that run first crossed the staleness bound. Both are
	// cleared by any resolution that produced an answer, and both are tracked
	// outside the sample so a further failed retry cannot restart the clock the
	// bound measures — which would let a resolver that is down forever keep a
	// host permanently in unknown.
	unknownSince time.Time
	staleSince   time.Time
}

// sessionShimReadinessRetryBackoff is the delay before retrying a resolver that
// has failed n consecutive times: 5s, 10s, 20s, then 30s for every further
// failure. n counts from 1.
func sessionShimReadinessRetryBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	delay := sessionShimReadinessRetryBase
	for i := 1; i < n && delay < sessionShimReadinessRetryMax; i++ {
		delay *= 2
	}
	if delay > sessionShimReadinessRetryMax {
		delay = sessionShimReadinessRetryMax
	}
	return delay
}

// boundedSessionShimReadinessReason renders one resolver failure as the reason
// a beat publishes, bounded so a verbose embedder error cannot inflate every
// beat.
//
// The cut is on a RUNE boundary, never a byte boundary. A byte cut through a
// multi-byte rune produces invalid UTF-8; json.Marshal silently substitutes
// U+FFFD for it, so the reason the consumer receives is not the reason still
// held in memory, a perfect echo can never satisfy exactEqual, and every beat
// for the duration of the outage loses its whole response — host status and the
// pending-mutation drain included. That is a second failure stacked on the one
// this degradation exists to survive.
func boundedSessionShimReadinessReason(err error) string {
	reason := strings.TrimSpace(err.Error())
	if len(reason) <= sessionShimReadinessReasonLimit {
		return reason
	}
	cut := sessionShimReadinessReasonLimit
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

func (d *Daemon) sessionShimReadinessStaleAfter() time.Duration {
	if bound := d.sessionShimConfig().ReadinessStaleAfter; bound > 0 {
		return bound
	}
	return DefaultSessionShimReadinessStaleAfter
}

// sessionShimReadinessWithin returns the host's readiness, resolving it only
// when no cached sample is fresher than maxAge and the failure backoff has
// elapsed. Every other consumer reads the sample the last resolution stored, so
// a host resolves readiness on one cadence rather than once per session, per
// poll tick, per admission and per credential refresh.
func (d *Daemon) sessionShimReadinessWithin(maxAge time.Duration) sessionShimReadinessSample {
	if !d.sessionShimEnabled() {
		return sessionShimReadinessSample{}
	}
	if sample, ok := d.cachedSessionShimReadinessWithin(maxAge); ok {
		return sample
	}
	// Single-flight. The resolver is embedder code and is never called under
	// readinessMu: a slow resolver must not serialize readers of the previous
	// value, and a callback that touched the daemon under that lock would
	// deadlock it. GetCarrierProofV2Readiness documents the reciprocal
	// obligation — return promptly, do not re-enter.
	d.readinessResolveMu.Lock()
	defer d.readinessResolveMu.Unlock()
	if sample, ok := d.cachedSessionShimReadinessWithin(maxAge); ok {
		return sample
	}
	readiness, err := d.resolveSessionShimCarrierProofV2Readiness()
	return d.storeSessionShimReadiness(readiness, err)
}

// sessionShimReadinessGate is the new-work admission predicate for every seam
// that is not the heartbeat. It returns nil while readiness is established and
// while an ESTABLISHED readiness is unknown: a resolver outage must not drain a
// host that was serving before the outage started. A definite not-ready, a
// permanent misconfiguration, and an unknown on a host that has never resolved
// readiness once all return an error — the last because a host with nothing
// established has no readiness to keep serving on, and admitting work there
// would be a loosening rather than a survival.
func (d *Daemon) sessionShimReadinessGate(maxAge time.Duration) error {
	return d.sessionShimReadinessWithin(maxAge).blocking
}

func (d *Daemon) cachedSessionShimReadinessWithin(maxAge time.Duration) (sessionShimReadinessSample, bool) {
	d.readinessMu.Lock()
	defer d.readinessMu.Unlock()
	if !d.readinessCache.valid {
		return sessionShimReadinessSample{}, false
	}
	if failures := d.readinessCache.failures; failures > 0 {
		if backoff := sessionShimReadinessRetryBackoff(failures); backoff < maxAge {
			maxAge = backoff
		}
	}
	now := time.Now()
	if now.Sub(d.readinessCache.resolvedAt) >= maxAge {
		return sessionShimReadinessSample{}, false
	}
	return d.boundStaleReadinessLocked(now), true
}

func (d *Daemon) storeSessionShimReadiness(
	readiness SessionShimCarrierProofV2Readiness,
	err error,
) sessionShimReadinessSample {
	now := time.Now()
	d.readinessMu.Lock()
	defer d.readinessMu.Unlock()
	switch {
	case err == nil:
		d.readinessEstablished = true
		d.readinessCache = sessionShimReadinessCache{
			sample:     sessionShimReadinessSample{readiness: readiness},
			valid:      true,
			resolvedAt: now,
		}
	case errors.Is(err, ErrSessionShimReadinessMisconfigured):
		// Permanent, and never degraded to unknown: a daemon with no resolver
		// configured is a programming fault, not an outage. Nothing is cached,
		// so it also cannot age into "stale" and look like a carrier problem.
		d.readinessCache = sessionShimReadinessCache{}
		return sessionShimReadinessSample{
			state:      SessionShimReadinessNotReady,
			reason:     boundedSessionShimReadinessReason(err),
			stateSince: now,
			blocking:   err,
		}
	case errors.Is(err, ErrSessionShimReadinessRejected):
		// A refusal is still an ANSWER: the resolver was reachable and said no.
		// That establishes readiness for the purpose of the non-withdrawal rule,
		// so a later outage on this host degrades rather than fails closed.
		d.readinessEstablished = true
		d.readinessCache = sessionShimReadinessCache{
			sample: sessionShimReadinessSample{
				state:      SessionShimReadinessNotReady,
				reason:     boundedSessionShimReadinessReason(err),
				stateSince: d.readinessStateSinceLocked(SessionShimReadinessNotReady, now),
				blocking:   err,
			},
			valid:      true,
			resolvedAt: now,
		}
	default:
		unknownSince := d.readinessCache.unknownSince
		if !d.readinessCache.valid || unknownSince.IsZero() {
			unknownSince = now
		}
		// blocking stays nil once readiness has been established — that is the
		// rule. Before then it is the resolver's own error, so a host that comes
		// up into the outage refuses new work at every seam instead of serving
		// on a readiness it has never proved.
		var blocking error
		if !d.readinessEstablished {
			blocking = err
		}
		d.readinessCache = sessionShimReadinessCache{
			sample: sessionShimReadinessSample{
				state:      SessionShimReadinessUnknown,
				reason:     boundedSessionShimReadinessReason(err),
				stateSince: unknownSince,
				blocking:   blocking,
			},
			valid:        true,
			resolvedAt:   now,
			failures:     d.readinessCache.failures + 1,
			unknownSince: unknownSince,
			staleSince:   d.readinessCache.staleSince,
		}
	}
	return d.boundStaleReadinessLocked(now)
}

// readinessStateSinceLocked carries the onset of an unchanged non-ready state
// forward, so a retry does not reset the clock a reader measures against.
func (d *Daemon) readinessStateSinceLocked(state string, now time.Time) time.Time {
	if d.readinessCache.valid && d.readinessCache.sample.state == state &&
		!d.readinessCache.sample.stateSince.IsZero() {
		return d.readinessCache.sample.stateSince
	}
	return now
}

// boundStaleReadinessLocked converts an unknown that has outlived the staleness
// bound into a definite not-ready. Applied on every read, so the conversion
// needs no timer and happens whether or not the resolver is being retried.
//
// The bound applies only AFTER readiness has been established. It exists to
// stop a host riding out an outage forever on a readiness it once had; a host
// that never had one is already refusing new work at every seam, and converting
// it to not-ready would only withdraw a readiness that was never granted.
func (d *Daemon) boundStaleReadinessLocked(now time.Time) sessionShimReadinessSample {
	sample := d.readinessCache.sample
	if sample.state != SessionShimReadinessUnknown || !d.readinessEstablished {
		return sample
	}
	bound := d.sessionShimReadinessStaleAfter()
	if now.Sub(d.readinessCache.unknownSince) < bound {
		return sample
	}
	if d.readinessCache.staleSince.IsZero() {
		d.readinessCache.staleSince = now
	}
	d.readinessCache.sample = sessionShimReadinessSample{
		state:      SessionShimReadinessNotReady,
		reason:     SessionShimReadinessStaleReason,
		stateSince: d.readinessCache.staleSince,
		blocking: fmt.Errorf("%w: readiness has been unknown for longer than %s",
			ErrSessionShimReadinessRejected, bound),
	}
	return d.readinessCache.sample
}
