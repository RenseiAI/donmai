package runner

import (
	"context"
	"math"
	"sync"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/stepheartbeat"
)

// interactiveUsageTotals accumulates an interactive session's running token
// and cost totals from the transcript tail's per-turn usage events. It is
// the interactive lane's equivalent of the headless lane's BudgetEnforcer
// as a usage METER (not as an enforcer — an interactive session honors a
// wall-clock cap only, never a token cap, so no ObserveEvent breach can
// stop it): every non-aggregate LlmCallEvent the handle surfaces adds its
// tokens to the running total, and the total rides the terminal report
// (res.Cost, via applyTo) so the existing cost-event writer records the
// real usage instead of a zero-token cost event. The step-heartbeat
// emitter reads the same total live through snapshot.
//
// A transcript is a file, not a wire: a turn whose usage no real model call
// can produce (a negative count, or one that overflows the running total)
// means the tail cannot be trusted, so the whole total reads as unreported
// from then on rather than as a wrong number. A dollar sum that leaves the
// finite range likewise drops the dollars (tokens stay reported) — a
// non-finite total would also fail to encode, losing the terminal report.
//
// The zero value is an empty total; all methods are safe for concurrent
// use (the supervisor loop and the heartbeat emitter read it from
// different goroutines).
type interactiveUsageTotals struct {
	mu     sync.Mutex
	cost   agent.CostData
	turns  int
	costWK bool
	// costLost is set once an observed price could not be summed into a
	// finite, non-negative dollar total; the dollars stay unreported.
	costLost bool
	// corrupt is set once a turn carried an impossible token count; the
	// whole total stays unreported.
	corrupt bool
}

// add meters one per-turn usage event into the running total. Aggregate or
// synthetic events are derived from a rolled-up total, so they are neither
// new usage nor evidence and are ignored — the same rule the headless
// meter applies. Token fields add whether or not the turn is priced (a
// turn that spent tokens always counts them), unless a count is impossible
// (see the type comment); the dollar total only moves when the event
// carries an observed price, so a turn with usage but no price still moves
// the token counts while leaving the dollar total untouched — and an event
// that reports a zero price is recorded as a priced zero, never confused
// with "no price".
func (u *interactiveUsageTotals) add(ev agent.LlmCallEvent) {
	if u == nil {
		return
	}
	if ev.Synthetic || ev.UsageSource == agent.LlmUsageAggregate {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.corrupt {
		return
	}
	next := u.cost
	if !addTokenCount(&next.InputTokens, ev.InputTokens) ||
		!addTokenCount(&next.OutputTokens, ev.OutputTokens) ||
		!addTokenCount(&next.CachedInputTokens, ev.CachedInputTokens) ||
		!addTokenCount(&next.CacheWriteTokens, ev.CacheWriteTokens) ||
		!addTokenCount(&next.ReasoningTokens, ev.ReasoningTokens) {
		u.corrupt = true
		return
	}
	u.cost = next
	if ev.ObservedCostUsd != nil && !u.costLost {
		price := *ev.ObservedCostUsd
		sum := u.cost.TotalCostUsd + price
		if price < 0 || math.IsNaN(sum) || math.IsInf(sum, 0) {
			u.costLost = true
		} else {
			u.cost.TotalCostUsd = sum
			u.costWK = true
		}
	}
	if ev.TurnCompleted {
		u.turns++
	}
}

// addTokenCount adds one turn's token count into a running total, refusing
// a count no model call produces (negative) and a sum that would overflow.
func addTokenCount(total *int64, n int64) bool {
	if n < 0 || *total > math.MaxInt64-n {
		return false
	}
	*total += n
	return true
}

// snapshot returns the current cumulative total. The second result reports
// whether anything has been metered yet, so callers can keep the "no
// usage" shape (nil cost, no heartbeat usage object) instead of posting
// an explicit zero. A corrupt total reports nothing metered: unreported,
// never a wrong number.
func (u *interactiveUsageTotals) snapshot() (agent.CostData, bool) {
	if u == nil {
		return agent.CostData{}, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.corrupt {
		return agent.CostData{}, false
	}
	if u.cost == (agent.CostData{}) && u.turns == 0 {
		return agent.CostData{}, false
	}
	out := u.cost
	out.NumTurns = u.turns
	if !u.costWK || u.costLost {
		out.TotalCostUsd = 0
	}
	return out, true
}

// applyTo carries the running total onto the terminal report. A session
// that metered nothing leaves res.Cost untouched (nil stays nil), so a
// usage-less session keeps today's report shape; otherwise res.Cost is
// the whole-session total the cost-event writer records. It never clears
// a cost already on the report — the transcript total is the only writer
// on this lane, but a future producer must not silently lose data.
func (u *interactiveUsageTotals) applyTo(res *Result) {
	if u == nil || res == nil {
		return
	}
	cost, ok := u.snapshot()
	if !ok {
		return
	}
	if res.Cost != nil {
		return
	}
	out := cost
	res.Cost = &out
}

// heartbeatSnapshot returns the running total in the step-heartbeat wire
// shape. A session that metered nothing returns the zero snapshot, which
// the emitter leaves off the wire body — the beat keeps today's shape
// until the first turn's usage lands.
func (u *interactiveUsageTotals) heartbeatSnapshot() stepheartbeat.UsageSnapshot {
	cost, ok := u.snapshot()
	if !ok {
		return stepheartbeat.UsageSnapshot{}
	}
	return stepheartbeat.UsageSnapshot{
		InputTokens:       cost.InputTokens,
		OutputTokens:      cost.OutputTokens,
		CachedInputTokens: cost.CachedInputTokens,
		TotalCostUsd:      cost.TotalCostUsd,
	}
}

// stepHeartbeatUsage is the step-heartbeat emitter's usage source for one
// session. Headless sessions read the budget enforcer's meter; interactive
// sessions read the transcript-tail totals their supervisor registers
// (interactiveUsageForSession) — an interactive session meters nothing
// through the enforcer, so reading it there would leave every beat
// usage-less.
func (r *Runner) stepHeartbeatUsage(qw QueuedWork, enforcer *BudgetEnforcer) stepheartbeat.UsageProvider {
	return func(context.Context) stepheartbeat.UsageSnapshot {
		if qw.isInteractive() {
			return r.interactiveUsageForSession(qw.SessionID).heartbeatSnapshot()
		}
		in, out, cached, usd := enforcer.usageSnapshot()
		return stepheartbeat.UsageSnapshot{
			InputTokens:       in,
			OutputTokens:      out,
			CachedInputTokens: cached,
			TotalCostUsd:      usd,
		}
	}
}
