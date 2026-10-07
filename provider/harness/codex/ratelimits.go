package codex

// Portions derived from t3code (github.com/pingdotgg/t3code)
// apps/server/src/provider/Layers/codexUsageLimits.ts @3e6b45028c.
// Copyright (c) 2026 T3 Tools Inc. MIT; see LICENSE-t3code.
//
// Codex subscription usage. The `account/rateLimits/read` response and
// the `account/rateLimits/updated` notification carry the same snapshot
// shape, so one mapper serves the status probe and the turn-driven
// update; both emit windows with the same IDs so they merge onto the
// same rows.
//
// Only the documented `account/rateLimits/read` call is used. The
// undocumented usage endpoints are never called.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// RateLimitsProbeInterval bounds how often the daemon reads quota
// windows: at most every 5 minutes.
const RateLimitsProbeInterval = 5 * time.Minute

// RateLimitsProbeTimeout bounds one quota read.
const RateLimitsProbeTimeout = 3 * time.Second

// CodexMainLimitID is the allowance bucket that counts as the main
// subscription allowance. Model-specific buckets (for example a
// model-scoped notification) must never replace its rows.
const CodexMainLimitID = "codex"

// RateLimitWindow is one position of the app-server rate-limit
// snapshot. `primary` and `secondary` are positions, not durations:
// the mapper classifies each by its reported duration, falling back
// to the plan default when the snapshot omits it.
type RateLimitWindow struct {
	UsedPercent        float64  `json:"usedPercent"`
	ResetsAt           *float64 `json:"resetsAt,omitempty"`
	WindowDurationMins *int     `json:"windowDurationMins,omitempty"`
}

// RateLimitSnapshot is the structural view of the app-server
// rate-limit snapshot. Both the read response and the updated
// notification satisfy it.
type RateLimitSnapshot struct {
	LimitID              *string          `json:"limitId,omitempty"`
	PlanType             *string          `json:"planType,omitempty"`
	RateLimitReachedType *string          `json:"rateLimitReachedType,omitempty"`
	Primary              *RateLimitWindow `json:"primary,omitempty"`
	Secondary            *RateLimitWindow `json:"secondary,omitempty"`
}

// ResetCreditEntry is one banked reset credit on the read response.
type ResetCreditEntry struct {
	Status    string   `json:"status"`
	ExpiresAt *float64 `json:"expiresAt,omitempty"`
}

// ResetCreditsSummary is the structural view of the read response's
// reset-credit summary.
type ResetCreditsSummary struct {
	AvailableCount int                `json:"availableCount"`
	Credits        []ResetCreditEntry `json:"credits,omitempty"`
}

// rateLimitsReadResponse is the structural view of the
// `account/rateLimits/read` response body.
type rateLimitsReadResponse struct {
	RateLimits          *RateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID map[string]*RateLimitSnapshot `json:"rateLimitsByLimitId"`
	ResetCredits        *ResetCreditsSummary          `json:"rateLimitResetCredits"`
}

// rateLimitsUpdatedParams is the structural view of the
// `account/rateLimits/updated` notification params.
type rateLimitsUpdatedParams struct {
	RateLimits *RateLimitSnapshot `json:"rateLimits"`
}

// isMonthlyPlan reports whether the plan exposes one monthly allowance
// instead of the session/weekly pair.
func isMonthlyPlan(planType *string) bool {
	if planType == nil {
		return false
	}
	return *planType == "free" || *planType == "go"
}

// rateLimitsToWindows maps the snapshot's positions onto quota windows.
// It shows the main allowance only: a snapshot naming another limit
// yields no rows.
func rateLimitsToWindows(snapshot *RateLimitSnapshot) []agent.UsageWindow {
	if snapshot == nil {
		return nil
	}
	if snapshot.LimitID != nil && *snapshot.LimitID != "" && *snapshot.LimitID != CodexMainLimitID {
		return nil
	}
	monthly := isMonthlyPlan(snapshot.PlanType)
	type position struct {
		id       string
		window   *RateLimitWindow
		fallback int
	}
	positions := []position{
		{id: "primary", window: snapshot.Primary, fallback: monthMins(monthly)},
		{id: "secondary", window: snapshot.Secondary, fallback: weekMins},
	}
	var out []agent.UsageWindow
	for _, p := range positions {
		if p.window == nil {
			continue
		}
		used := p.window.UsedPercent
		if math.IsNaN(used) || math.IsInf(used, 0) {
			continue
		}
		duration := p.fallback
		if p.window.WindowDurationMins != nil {
			duration = *p.window.WindowDurationMins
		}
		kind := agent.PoolKind(duration)
		w := agent.UsageWindow{
			ID:                 p.id,
			Kind:               kind,
			Label:              agent.PoolLabel(kind),
			UsedPercent:        agent.ClampPercent(used),
			WindowDurationMins: &duration,
		}
		if p.window.ResetsAt != nil {
			if iso := agent.ISOFromEpochSeconds(*p.window.ResetsAt); iso != "" {
				w.ResetsAt = iso
			}
		}
		out = append(out, w)
	}
	return out
}

const (
	sessionMinsFallback = 5 * 60
	weekMins            = 7 * 24 * 60
	monthMinsFallback   = 30 * 24 * 60
)

func monthMins(monthly bool) int {
	if monthly {
		return monthMinsFallback
	}
	return sessionMinsFallback
}

// ResetCreditsToContract maps the read response's reset-credit summary
// onto the shared contract: the available count and the soonest expiry
// of an available credit.
func ResetCreditsToContract(summary *ResetCreditsSummary) *agent.ResetCredits {
	if summary == nil {
		return nil
	}
	count := summary.AvailableCount
	if count < 0 {
		count = 0
	}
	out := &agent.ResetCredits{AvailableCount: count}
	var earliest float64
	found := false
	for _, c := range summary.Credits {
		if c.Status != "available" || c.ExpiresAt == nil {
			continue
		}
		if !found || *c.ExpiresAt < earliest {
			earliest = *c.ExpiresAt
			found = true
		}
	}
	if found {
		if iso := agent.ISOFromEpochSeconds(earliest); iso != "" {
			out.NextExpiresAt = iso
		}
	}
	return out
}

// RateLimitsToLimits maps one quota read onto the shared limits
// snapshot. The bucket map selects the main allowance explicitly; a
// legacy snapshot can name another limit, which never counts.
func RateLimitsToLimits(snapshot *RateLimitSnapshot, byLimitID map[string]*RateLimitSnapshot, resetCredits *ResetCreditsSummary, checkedAt string) agent.UsageLimits {
	selected := snapshot
	if byLimitID != nil {
		if main, ok := byLimitID[CodexMainLimitID]; ok {
			selected = main
		}
	}
	out := agent.MakeUsageLimits(checkedAt, rateLimitsToWindows(selected))
	if rc := ResetCreditsToContract(resetCredits); rc != nil {
		out.ResetCredits = rc
	}
	return out
}

// RateLimitsToUpdate maps one notification snapshot onto a sparse
// update. It returns nil when the notification names no main-allowance
// window, so a model-specific notification never overwrites the rows.
func RateLimitsToUpdate(snapshot *RateLimitSnapshot, checkedAt string) *agent.UsageLimitsUpdate {
	windows := rateLimitsToWindows(snapshot)
	if len(windows) == 0 {
		return nil
	}
	return &agent.UsageLimitsUpdate{CheckedAt: checkedAt, Windows: windows}
}

// MergeRateLimits folds a partial notification snapshot into the
// previous one field by field: a field the update omits keeps the
// value observed earlier, so a notification that only names the limit
// it reached must not drop the windows an earlier one carried.
// Model-specific snapshots describe a different allowance and never
// overwrite the main one.
func MergeRateLimits(previous, update *RateLimitSnapshot) *RateLimitSnapshot {
	if update != nil && update.LimitID != nil && *update.LimitID != "" && *update.LimitID != CodexMainLimitID {
		return previous
	}
	if previous == nil {
		return update
	}
	if update == nil {
		return previous
	}
	out := *previous
	if update.LimitID != nil {
		out.LimitID = update.LimitID
	}
	if update.PlanType != nil {
		out.PlanType = update.PlanType
	}
	if update.RateLimitReachedType != nil {
		out.RateLimitReachedType = update.RateLimitReachedType
	}
	if update.Primary != nil {
		out.Primary = update.Primary
	}
	if update.Secondary != nil {
		out.Secondary = update.Secondary
	}
	return &out
}

// RateLimitsFailureMessage renders a bounded, client-safe reason for a
// failed quota read. The raw error is for the log; only the category
// reaches the limits view.
func RateLimitsFailureMessage(err error) string {
	if err == nil {
		return "Codex did not answer the usage request."
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return "Codex could not read usage (JSON-RPC " + itoa(rpcErr.Code) + ")."
	}
	if isProcessExitedError(err) {
		return "Codex exited before it could report usage."
	}
	if isSpawnError(err) {
		return "Codex could not be started to read usage."
	}
	return "Codex did not answer the usage request."
}

// isProcessExitedError reports whether err describes the app-server
// process exiting before it could answer.
func isProcessExitedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "exited") || strings.Contains(msg, "exit")
}

// isSpawnError reports whether err describes the app-server failing
// to start.
func isSpawnError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "spawn") || strings.Contains(msg, "start")
}

func itoa(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// RateLimitsProbe reads quota windows through the documented
// `account/rateLimits/read` call. It is a probe, not session traffic:
// a failure or a slow answer yields a probe-failed snapshot rather
// than an error.
type RateLimitsProbe struct {
	mu       sync.Mutex
	lastFire time.Time
	now      func() time.Time
}

// NewRateLimitsProbe builds a probe using the wall clock.
func NewRateLimitsProbe() *RateLimitsProbe {
	return &RateLimitsProbe{now: time.Now}
}

// Allow reports whether a probe may fire now. Probes are rate-limited
// to once per RateLimitsProbeInterval.
func (p *RateLimitsProbe) Allow(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return agent.UsageProbeAllowed(p.lastFire, now, RateLimitsProbeInterval)
}

// mark records one probe attempt.
func (p *RateLimitsProbe) mark(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastFire = now
}

// LastFire reports the last probe attempt time.
func (p *RateLimitsProbe) LastFire() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastFire
}

// ReadProbe performs one quota read against the app-server client. A
// nil client (the harness is not running) is a failed probe, never an
// error: the last good windows stay published. Only a JSON-RPC error
// (the app-server answered and refused the read) marks the failure as
// answered; a nil client, a timeout, an exit or an unreadable reply
// never reached the account and carries no login verdict.
func (p *RateLimitsProbe) ReadProbe(ctx context.Context, client *Client) agent.UsageLimits {
	now := time.Now()
	if p != nil && p.now != nil {
		now = p.now()
	}
	checkedAt := agent.ISOTime(now)
	if p != nil {
		p.mark(now)
	}
	if client == nil {
		return agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	reqCtx, cancel := context.WithTimeout(ctx, RateLimitsProbeTimeout)
	defer cancel()
	raw, err := client.Request(reqCtx, "account/rateLimits/read", nil, RateLimitsProbeTimeout)
	if err != nil {
		out := agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, RateLimitsFailureMessage(err))
		out.Unavailable.Answered = answeredByAppServer(client, err)
		return out
	}
	var resp rateLimitsReadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	return RateLimitsToLimits(resp.RateLimits, resp.RateLimitsByLimitID, resp.ResetCredits, checkedAt)
}

// answeredByAppServer reports whether a failed read was the app-server's
// own JSON-RPC refusal. A stopped client fails its pending requests with
// a synthesized JSON-RPC error, so a closed client never counts as an
// answer.
func answeredByAppServer(client *Client, err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	select {
	case <-client.Done():
		return false
	default:
		return true
	}
}

// SubscribeRateLimits routes `account/rateLimits/updated` notifications
// into the shared snapshot: the notification merges field by field onto
// the last snapshot, then folds onto the published limits as a sparse
// update. onUpdate receives the merged limits whenever the notification
// carries a main-allowance window.
//
// The subscription chains onto the client's fall-through handler under
// the client's subscription lock, so concurrent subscriptions are safe.
// unsubscribe stops this subscription's delivery without dropping a
// subscription installed after it.
func SubscribeRateLimits(client *Client, onUpdate func(agent.UsageLimits)) (unsubscribe func()) {
	if client == nil {
		return func() {}
	}
	var mu sync.Mutex
	var last *RateLimitSnapshot
	var published *agent.UsageLimits
	var active atomic.Bool
	active.Store(true)
	restore := client.wrapGlobal(func(prev notificationHandler) notificationHandler {
		return func(n notification) {
			if prev != nil {
				prev(n)
			}
			if !active.Load() || n.Method != "account/rateLimits/updated" {
				return
			}
			var params rateLimitsUpdatedParams
			if err := json.Unmarshal(n.Params, &params); err != nil || params.RateLimits == nil {
				return
			}
			mu.Lock()
			last = MergeRateLimits(last, params.RateLimits)
			// Classify the windows this notification names against the
			// merged plan: a partial notification that omits planType must
			// not turn a monthly allowance into a five-hour session.
			named := *params.RateLimits
			if named.PlanType == nil && last != nil {
				named.PlanType = last.PlanType
			}
			checkedAt := agent.ISOTime(time.Now())
			update := RateLimitsToUpdate(&named, checkedAt)
			if update == nil {
				mu.Unlock()
				return
			}
			next := agent.ApplyUsageLimitsUpdate(published, *update, checkedAt)
			published = next
			mu.Unlock()
			if onUpdate != nil && next != nil {
				onUpdate(*next)
			}
		}
	})
	return func() {
		active.Store(false)
		restore()
	}
}
