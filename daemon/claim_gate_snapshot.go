// Package daemon claim_gate_snapshot.go — the daemon's claim path evaluates
// the cached, verified ruleset snapshot (fail-static) instead of depending
// on a live control-plane round trip that a platform outage would otherwise
// take down with it.
//
// FailStaticClaimGateProvider wraps the existing ClaimGateProvider seam
// (daemon.go's evaluateNarrowOnlyClaim, unchanged since
// ADR-2026-08-05-versioned-execution-cell-and-session-reference.md D4/D5)
// with two additions, both sourced from a *rulesetsnapshot.Client:
//
//  1. A permission re-check (rulesetsnapshot.EvaluatePermission) that a
//     live-only ClaimGateProvider has no reason to perform on its own: is
//     this claim's target pool STILL granted, per the freshest cached
//     snapshot? This runs whenever a snapshot source is configured, in
//     front of whatever a Live provider answers — an org revoking a pool
//     grant must take effect for a fail-static claim exactly as it would
//     for a live one, and the check being "is this still true" rather than
//     "is this true right now" is exactly what caching costs.
//  2. When no Live provider is wired at all, an OSS-shipped DEFAULT answer
//     to ResolveClaimLocalReality — see rulesetsnapshot.BuildClaimLocalReality
//     for precisely how bounded that default is.
//
// Both additions are TTL-bounded: past the snapshot's configured
// RefuseAfter, ResolveClaimLocalReality returns a loud, typed
// *rulesetsnapshot.ExpiredError instead of extending either check past the
// point this package considers the cached data trustworthy — never a
// silent stall, never fail-open on permission.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/rulesetsnapshot"
)

// FailStaticClaimGateProvider implements ClaimGateProvider, layering a
// cached-ruleset-snapshot evaluation over an optional Live provider. See
// the package doc comment above for exactly what it adds and when.
type FailStaticClaimGateProvider struct {
	// Live is the daemon's normal (typically host-local) ClaimGateProvider,
	// if any. May be nil — see BuildClaimLocalReality's doc comment for how
	// conservative the fallback default is when it is.
	Live ClaimGateProvider
	// Snapshot is the cached ruleset-snapshot source. Must be non-nil and
	// Configured() for this provider to do anything beyond delegating to
	// Live unchanged — see daemon.claimGateProvider, which only ever
	// constructs a FailStaticClaimGateProvider when at least one of Live or
	// a configured Snapshot is present.
	Snapshot *rulesetsnapshot.Client
	// HostID is this daemon's own placement identity, reported as
	// ClaimLocalReality.PlacementID by the OSS default evaluator (used only
	// when Live is nil — Live is expected to report its own PlacementID).
	HostID string
}

// ResolveClaimLocalReality implements ClaimGateProvider.
func (p *FailStaticClaimGateProvider) ResolveClaimLocalReality(cellJSON json.RawMessage) (json.RawMessage, error) {
	cell, err := executioncell.DecodeResolvedExecutionCell(cellJSON)
	if err != nil {
		return nil, fmt.Errorf("rulesetsnapshot claim gate: decode admitted cell: %w", err)
	}

	if p.Snapshot != nil && p.Snapshot.Configured() {
		snap, status, ok := p.Snapshot.Current()
		if !ok {
			return nil, fmt.Errorf("rulesetsnapshot claim gate: %w", rulesetsnapshot.ErrNoCachedSnapshot)
		}
		if status.Age > p.Snapshot.RefuseAfter() {
			return nil, fmt.Errorf("rulesetsnapshot claim gate: %w", &rulesetsnapshot.ExpiredError{
				Rev: status.Rev, Age: status.Age, RefuseAfter: p.Snapshot.RefuseAfter(),
			})
		}
		// Permission re-check (addition 1, see package doc): runs against
		// whatever the freshest cached snapshot says, in front of Live —
		// a revoked grant must deny even a Live provider's "yes, this host
		// can serve it" answer, because Live only ever speaks to VIABILITY
		// (can this host deliver it), never to whether the grant itself
		// still stands.
		if err := rulesetsnapshot.EvaluatePermission(cell, snap); err != nil {
			return nil, fmt.Errorf("rulesetsnapshot claim gate: %w", err)
		}
		// status.Degraded (age between DegradedAfter and RefuseAfter) is
		// deliberately allowed through past this point — fail-STATIC, not
		// fail-open and not fail-closed early. It is surfaced separately on
		// the routing/claim-gate decision record (daemon.go's
		// recordClaimGateSnapshotDecision), never silently.

		if p.Live == nil {
			reality, err := rulesetsnapshot.BuildClaimLocalReality(cell, snap, p.HostID)
			if err != nil {
				return nil, fmt.Errorf("rulesetsnapshot claim gate: %w", err)
			}
			return json.Marshal(reality)
		}
	}

	if p.Live != nil {
		return p.Live.ResolveClaimLocalReality(cellJSON)
	}

	// Neither a configured Snapshot nor a Live provider — should be
	// unreachable given how daemon.claimGateProvider constructs this type,
	// but fails closed (a typed error, never a silent "no opinion" that a
	// caller might mistake for permission) rather than assume.
	return nil, fmt.Errorf("rulesetsnapshot claim gate: %w", errNoClaimGateSource)
}

var errNoClaimGateSource = errors.New("no live provider and no configured ruleset-snapshot source")

// claimGateProvider resolves the ClaimGateProvider evaluateNarrowOnlyClaim
// should use. When Options.RulesetSnapshot is nil this returns exactly what
// a direct type assertion on ProviderRegistry always did, so a daemon that
// never configures a snapshot source is byte-identical to every deployment
// before this file existed. Otherwise it wraps whatever Live provider is
// available (possibly none) in a FailStaticClaimGateProvider.
func (d *Daemon) claimGateProvider() (ClaimGateProvider, bool) {
	live, liveOK := d.opts.ProviderRegistry.(ClaimGateProvider)
	if d.opts.RulesetSnapshot == nil {
		return live, liveOK
	}
	if !liveOK && !d.opts.RulesetSnapshot.Configured() {
		return nil, false
	}
	var liveProvider ClaimGateProvider
	if liveOK {
		liveProvider = live
	}
	return &FailStaticClaimGateProvider{
		Live:     liveProvider,
		Snapshot: d.opts.RulesetSnapshot,
		HostID:   d.WorkerID(),
	}, true
}

// recordClaimGateSnapshotDecision records the daemon's cached
// ruleset-snapshot status into RoutingTraces, keyed by sessionID, so
// `routing explain <sessionId>` shows the rev/age/degraded state a claim
// decision was evaluated against. A nil-safe no-op when no snapshot source
// is configured or none has ever verified.
func (d *Daemon) recordClaimGateSnapshotDecision(sessionID string) {
	if d.opts.RulesetSnapshot == nil || d.routingTraces == nil {
		return
	}
	status, ok := rulesetSnapshotWireStatus(d.opts.RulesetSnapshot)
	if !ok {
		return
	}
	decision := afclient.RoutingDecision{SessionID: sessionID, DecidedAt: time.Now().UTC()}
	d.routingTraces.RecordDecisionWithSnapshot(decision, nil, &status)
}

// rulesetSnapshotWireStatus adapts a *rulesetsnapshot.Client's Current()
// status to the afclient wire shape. ok is false when the client has never
// verified any snapshot (nothing to report).
func rulesetSnapshotWireStatus(client *rulesetsnapshot.Client) (afclient.RulesetSnapshotStatus, bool) {
	if client == nil {
		return afclient.RulesetSnapshotStatus{}, false
	}
	_, status, ok := client.Current()
	if !ok {
		return afclient.RulesetSnapshotStatus{}, false
	}
	return afclient.RulesetSnapshotStatus{
		Rev:        status.Rev,
		AgeMs:      status.Age.Milliseconds(),
		Degraded:   status.Degraded,
		CompiledAt: status.CompiledAt,
	}, true
}
