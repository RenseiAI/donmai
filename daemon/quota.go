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
	muse       *agent.UsageLimits
	codexAt    time.Time
	claudeAt   time.Time
	museAt     time.Time
	codexPlan  string
	codexID    string
	claudeID   string
	museID     string
	codexAuth  *agent.UsageAuthCheck
	claudeAuth *agent.UsageAuthCheck
	museAuth   *agent.UsageAuthCheck
}

// QuotaSnapshot composes the heartbeat quota field: one entry per
// account with quota state. Nil-safe: a daemon that has never produced
// a snapshot reports nothing, and the beat omits the key. An embedding
// binary that runs its own heartbeat lane may call this to publish the
// same host login checks the daemon's own lane sends.
//
// Account IDs ride as opaque values the platform hashes; the snapshot
// never carries an address. Each entry carries the harness's latest
// login-check outcome as authCheck, or omits it when no probe has run.
// The returned accounts are copies: mutating them never disturbs the
// cached readings a later probe replaces.
func (d *Daemon) QuotaSnapshot() []agent.UsageAccount {
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
			Limits:    copyUsageLimits(q.codex),
			AuthCheck: copyAuthCheck(q.codexAuth),
		})
	}
	if q.claude != nil && q.claude.Unavailable == nil {
		out = append(out, agent.UsageAccount{
			ID:        q.claudeID,
			Provider:  agent.UsageHarnessClaude,
			Limits:    copyUsageLimits(q.claude),
			AuthCheck: copyAuthCheck(q.claudeAuth),
		})
	}
	if q.muse != nil && q.muse.Unavailable == nil {
		out = append(out, agent.UsageAccount{
			ID:        q.museID,
			Provider:  agent.UsageHarnessMuse,
			Limits:    copyUsageLimits(q.muse),
			AuthCheck: copyAuthCheck(q.museAuth),
		})
	}
	return out
}

// copyUsageLimits returns a private copy so a published snapshot never
// aliases the cached readings a later probe replaces. Windows copy with
// make plus copy (guarded by a nil check) so an empty non-nil set stays
// an empty non-nil set on the wire, and each window duration pointer,
// the reset credits and the unavailability reason copy by value.
func copyUsageLimits(src *agent.UsageLimits) agent.UsageLimits {
	if src == nil {
		return agent.UsageLimits{}
	}
	out := *src
	if src.Windows != nil {
		out.Windows = make([]agent.UsageWindow, len(src.Windows))
		copy(out.Windows, src.Windows)
		for i := range src.Windows {
			if src.Windows[i].WindowDurationMins != nil {
				d := *src.Windows[i].WindowDurationMins
				out.Windows[i].WindowDurationMins = &d
			}
		}
	}
	if src.ResetCredits != nil {
		rc := *src.ResetCredits
		out.ResetCredits = &rc
	}
	if src.Unavailable != nil {
		u := *src.Unavailable
		out.Unavailable = &u
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
// way the probe is a login check, stamped with the probe time. Every
// attempt advances the probe clock, so retries stay inside the
// 5-minute cadence even when the read fails.
func (q *quotaState) noteCodexProbe(accountID, plan string, probed agent.UsageLimits, at time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.codex = agent.ResolveUsageLimitsAfterProbe(q.codex, &probed)
	q.codexAt = at
	if check := agent.UsageAuthCheckAfterProbe(agent.UsageHarnessCodex, &probed, at); check != nil {
		q.codexAuth = check
	}
	// Resolve keeps the previous pointer on a failed probe; only stamp
	// identity when the probe produced a live snapshot.
	if q.codex != nil && q.codex.Unavailable == nil {
		q.codexID = accountID
		q.codexPlan = plan
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
	q.claudeAt = at
	if check := agent.UsageAuthCheckAfterProbe(agent.UsageHarnessClaude, &probed, at); check != nil {
		q.claudeAuth = check
	}
	if q.claude != nil && q.claude.Unavailable == nil {
		q.claudeID = accountID
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

// noteMuseProbe records one muse login-check outcome: the check
// proves a supported CLI is installed, never usage windows, so the
// muse entry always carries an empty window set beside its verdict.
// A verdict — ok or refused — publishes the entry outright; only a
// read that never reached the CLI leaves the previous entry
// untouched. Every attempt advances the probe clock, so retries stay
// inside the 5-minute cadence even when the read fails.
func (q *quotaState) noteMuseProbe(accountID string, probed agent.UsageLimits, at time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.museAt = at
	if check := agent.UsageAuthCheckAfterProbe(agent.UsageHarnessMuse, &probed, at); check != nil {
		q.museAuth = check
		q.museID = accountID
		q.muse = &agent.UsageLimits{CheckedAt: agent.ISOTime(at), Windows: []agent.UsageWindow{}}
	}
}

// Subscription-window exhaustion claim admission refuses a poll-claimed
// host-session whose cached subscription allowance is exhausted, before
// AcceptWorkWithDetail can reserve or spawn. The decision is structural only:
// it never parses prose and never infers exhaustion from agent.UpstreamError
// or a provider 429.
const (
	subscriptionWindowExhaustedContractVersion = "subscription-window-exhausted/v1"
	subscriptionWindowExhaustedCode            = "subscription_window_exhausted"
)

// subscriptionWindowExhaustedError refuses an exhausted host-session claim.
// resetAt is the canonical RFC3339 effective reset; usable reports whether
// every blocking window supplied a valid future instant — the only case that
// may publish the typed NACK member. An unusable reset still refuses (fail
// closed) when the snapshot is fresh; the prose stays fixed so identical
// text never gains authority. A stale snapshot or a reset at or before now
// admits, so the refusal never wedges on a cache the probe has not refreshed.
type subscriptionWindowExhaustedError struct {
	resetAt string
	usable  bool
}

func (e *subscriptionWindowExhaustedError) Error() string {
	return "subscription window exhausted"
}

// quotaClaimAdmission decides whether a poll-claimed session may proceed
// past claim admission given this host's cached quota snapshot. It runs over
// the locked snapshot copy; a nil daemon admits.
func (d *Daemon) quotaClaimAdmission(spec SessionSpec) error {
	if d == nil {
		return nil
	}
	return decideQuotaClaimAdmission(d.QuotaSnapshot(), spec.AuthMode, spec.Harness, time.Now())
}

// parseQuotaInstant parses a snapshot instant in either RFC3339 form the
// snapshots carry (ISOTime renders RFC3339Nano, which also reads plain
// RFC3339). It reports false on a missing or malformed value.
func parseQuotaInstant(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, true
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// quotaSnapshotFresh reports whether a snapshot CheckedAt still verifies the
// host: a missing, malformed, or older-than-TTL reading is unknown and
// admits. The probe interval stays below the TTL, so a live harness keeps
// its snapshot fresh; a stale one means the cache has not been refreshed
// since the window may have reset.
func quotaSnapshotFresh(checkedAt string, now time.Time) bool {
	parsed, ok := parseQuotaInstant(checkedAt)
	if !ok {
		return false
	}
	return now.Sub(parsed) <= quotaConsumerFreshnessTTL
}

// decideQuotaClaimAdmission is the clock-parameterized admission decision over
// one copied snapshot. It refuses only host-session claims whose harness
// matches a cached account with a fresh, still-blocking exhausted window
// (UsedPercent >= 100 with a valid future reset); anything else preserves
// current admission (nil). A reset at or before now means the window has
// reset and admits; a stale snapshot admits as unknown. Only a fresh
// snapshot with a missing or malformed reset fails closed without the typed
// member, bounded by the freshness TTL. The effective reset is the latest
// future RFC3339 instant across all still-blocking windows.
func decideQuotaClaimAdmission(accounts []agent.UsageAccount, authMode, harness string, now time.Time) error {
	if authMode != string(agent.AuthHostSession) || harness == "" {
		return nil
	}
	var matched *agent.UsageAccount
	for i := range accounts {
		if accounts[i].Provider == harness {
			matched = &accounts[i]
			break
		}
	}
	if matched == nil {
		return nil
	}
	if !quotaSnapshotFresh(matched.Limits.CheckedAt, now) {
		return nil
	}
	latest := time.Time{}
	sawBlocking := false
	for _, window := range matched.Limits.Windows {
		if window.UsedPercent < 100 {
			continue
		}
		reset, ok := parseQuotaInstant(window.ResetsAt)
		if !ok {
			return &subscriptionWindowExhaustedError{}
		}
		if !reset.After(now) {
			// The window has reset since the snapshot was taken: it no
			// longer blocks this harness.
			continue
		}
		sawBlocking = true
		if reset.After(latest) {
			latest = reset
		}
	}
	if !sawBlocking {
		return nil
	}
	return &subscriptionWindowExhaustedError{resetAt: latest.UTC().Format(time.RFC3339), usable: true}
}

// quotaExhaustionSuspendsClaiming is the host-wide, pre-claim counterpart of
// the per-item admission decision. Poll requests cannot select a harness, so
// any fresh, still-blocking host-session exhaustion suspends the whole tick:
// the poll request never goes out and no claim is taken, which keeps a typed
// exhaustion NACK (unread by older platforms and counted toward their
// terminal nack-loop) from terminalizing sessions the window would have
// requeued. Stale snapshots and past resets admit, so the gate reopens on
// reset or probe refresh without a restart. It never parses prose and never
// infers exhaustion from endpoint errors or 429s.
func quotaExhaustionSuspendsClaiming(accounts []agent.UsageAccount, now time.Time) (bool, string) {
	latest := time.Time{}
	sawUnusable := false
	for i := range accounts {
		if !quotaSnapshotFresh(accounts[i].Limits.CheckedAt, now) {
			continue
		}
		for _, window := range accounts[i].Limits.Windows {
			if window.UsedPercent < 100 {
				continue
			}
			reset, ok := parseQuotaInstant(window.ResetsAt)
			if !ok {
				sawUnusable = true
				continue
			}
			if !reset.After(now) {
				continue
			}
			if reset.After(latest) {
				latest = reset
			}
		}
	}
	if !latest.IsZero() {
		return true, "subscription window exhausted until " + latest.UTC().Format(time.RFC3339)
	}
	if sawUnusable {
		return true, "subscription window exhausted (awaiting fresh quota probe)"
	}
	return false, ""
}

// quotaSuspendsClaiming reports whether this host's cached quota snapshot
// must stop the poll loop from claiming new work. Nil-safe: no snapshot
// never suspends.
func (d *Daemon) quotaSuspendsClaiming(now time.Time) (bool, string) {
	if d == nil {
		return false, ""
	}
	return quotaExhaustionSuspendsClaiming(d.QuotaSnapshot(), now)
}

// museProbeDue reports whether the muse login check may fire now.
func (q *quotaState) museProbeDue(now time.Time) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return agent.UsageProbeAllowed(q.museAt, now, providercodex.RateLimitsProbeInterval)
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
