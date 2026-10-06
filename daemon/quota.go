package daemon

import (
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
)

// quotaState is the daemon's quota-snapshot cache. Each provider probe
// fires at most every 5 minutes; a failed probe keeps the last good
// windows while an unsupported reading clears them.
type quotaState struct {
	mu        sync.Mutex
	codex     *agent.UsageLimits
	claude    *agent.UsageLimits
	codexAt   time.Time
	claudeAt  time.Time
	codexPlan string
	codexID   string
	claudeID  string
}

// quotaSnapshot composes the heartbeat quota field: one entry per
// account with quota state. Nil-safe: a daemon that has never produced
// a snapshot reports nothing, and the beat omits the key.
//
// Account IDs ride as opaque values the platform hashes; the snapshot
// never carries an address.
func (d *Daemon) quotaSnapshot() []agent.UsageAccount {
	if d == nil {
		return nil
	}
	return d.quota.snapshot()
}

func (q *quotaState) snapshot() []agent.UsageAccount {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []agent.UsageAccount
	if q.codex != nil && q.codex.Unavailable == nil {
		out = append(out, agent.UsageAccount{
			ID:       q.codexID,
			Provider: "codex",
			Plan:     q.codexPlan,
			LimitID:  providercodex.CodexMainLimitID,
			Limits:   *q.codex,
		})
	}
	if q.claude != nil && q.claude.Unavailable == nil {
		out = append(out, agent.UsageAccount{
			ID:       q.claudeID,
			Provider: "claude",
			Limits:   *q.claude,
		})
	}
	return out
}

// noteCodexProbe records one codex quota probe outcome: a failed probe
// keeps the last good windows, an unsupported reading clears them, and
// a successful probe replaces the published windows outright.
func (q *quotaState) noteCodexProbe(accountID, plan string, probed agent.UsageLimits, at time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.codex = agent.ResolveUsageLimitsAfterProbe(q.codex, &probed)
	// Resolve keeps the previous pointer on a failed probe; only stamp
	// identity and time when the probe produced a live snapshot.
	if q.codex != nil && q.codex.Unavailable == nil {
		q.codexID = accountID
		q.codexPlan = plan
		q.codexAt = at
	}
}

// noteClaudeProbe records one claude usage-read outcome under the same
// rule.
func (q *quotaState) noteClaudeProbe(accountID string, probed agent.UsageLimits, at time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claude = agent.ResolveUsageLimitsAfterProbe(q.claude, &probed)
	if q.claude != nil && q.claude.Unavailable == nil {
		q.claudeID = accountID
		q.claudeAt = at
	}
}

// noteCodexUpdate folds one turn-driven codex update into the published
// snapshot. Windows merge by ID; omitted windows are unchanged.
func (q *quotaState) noteCodexUpdate(update agent.UsageLimitsUpdate, at time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.codex = agent.ApplyUsageLimitsUpdate(q.codex, update, agent.ISOTime(at))
}

// noteClaudeUpdate folds one streamed claude update into the published
// snapshot.
func (q *quotaState) noteClaudeUpdate(update agent.UsageLimitsUpdate, at time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claude = agent.ApplyUsageLimitsUpdate(q.claude, update, agent.ISOTime(at))
}

// codexProbeDue reports whether the codex quota probe may fire now.
func (q *quotaState) codexProbeDue(now time.Time) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return agent.UsageProbeAllowed(q.codexAt, now, providercodex.RateLimitsProbeInterval)
}

// claudeProbeDue reports whether the claude usage read may fire now.
func (q *quotaState) claudeProbeDue(now time.Time) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return agent.UsageProbeAllowed(q.claudeAt, now, providercodex.RateLimitsProbeInterval)
}
