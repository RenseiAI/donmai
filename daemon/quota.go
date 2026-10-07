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
//
// Each probe also records the login-check outcome it implies. The
// outcome rides on the account entry as its authCheck: it is stamped
// with the probe time and only a probe replaces it, so a turn-driven
// update never refreshes the verdict.
type quotaState struct {
	mu         sync.Mutex
	codex      *agent.UsageLimits
	claude     *agent.UsageLimits
	codexAt    time.Time
	claudeAt   time.Time
	codexPlan  string
	codexID    string
	claudeID   string
	codexAuth  *agent.UsageAuthCheck
	claudeAuth *agent.UsageAuthCheck
}

// quotaSnapshot composes the heartbeat quota field: one entry per
// account with quota state. Nil-safe: a daemon that has never produced
// a snapshot reports nothing, and the beat omits the key.
//
// Account IDs ride as opaque values the platform hashes; the snapshot
// never carries an address. Each entry carries the harness's latest
// login-check outcome as authCheck, or omits it when no probe has run.
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
			ID:        q.codexID,
			Provider:  agent.UsageHarnessCodex,
			Plan:      q.codexPlan,
			LimitID:   providercodex.CodexMainLimitID,
			Limits:    *q.codex,
			AuthCheck: copyAuthCheck(q.codexAuth),
		})
	}
	if q.claude != nil && q.claude.Unavailable == nil {
		out = append(out, agent.UsageAccount{
			ID:        q.claudeID,
			Provider:  agent.UsageHarnessClaude,
			Limits:    *q.claude,
			AuthCheck: copyAuthCheck(q.claudeAuth),
		})
	}
	return out
}

// copyAuthCheck returns a private copy so a published snapshot never
// aliases the cached verdict a later probe replaces.
func copyAuthCheck(check *agent.UsageAuthCheck) *agent.UsageAuthCheck {
	if check == nil {
		return nil
	}
	c := *check
	return &c
}

// noteCodexProbe records one codex quota probe outcome: a failed probe
// keeps the last good windows, an unsupported reading clears them, and
// a successful probe replaces the published windows outright. Either
// way the probe is a login check, stamped with the probe time.
func (q *quotaState) noteCodexProbe(accountID, plan string, probed agent.UsageLimits, at time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.codex = agent.ResolveUsageLimitsAfterProbe(q.codex, &probed)
	if check := agent.UsageAuthCheckAfterProbe(agent.UsageHarnessCodex, &probed, at); check != nil {
		q.codexAuth = check
	}
	// Resolve keeps the previous pointer on a failed probe; only stamp
	// identity and time when the probe produced a live snapshot.
	if q.codex != nil && q.codex.Unavailable == nil {
		q.codexID = accountID
		q.codexPlan = plan
		q.codexAt = at
	}
}

// noteClaudeProbe records one claude usage-read outcome under the same
// rules, login check included.
func (q *quotaState) noteClaudeProbe(accountID string, probed agent.UsageLimits, at time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claude = agent.ResolveUsageLimitsAfterProbe(q.claude, &probed)
	if check := agent.UsageAuthCheckAfterProbe(agent.UsageHarnessClaude, &probed, at); check != nil {
		q.claudeAuth = check
	}
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
