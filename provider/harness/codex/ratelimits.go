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
	"os"
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
	UsedTokens         *float64 `json:"usedTokens,omitempty"`
	Limit              *float64 `json:"limit,omitempty"`
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
	BillingMode          *string          `json:"billingMode,omitempty"`
	CreditBalance        *float64         `json:"creditBalance,omitempty"`
	Scope                *string          `json:"scope,omitempty"`
	UserID               *string          `json:"userId,omitempty"`
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
// `account/rateLimits/read` response body. accountId rides the
// response only on newer app-server builds; older ones omit it,
// so callers must treat an empty id as "the login did not name an
// account" rather than as a distinct account.
type rateLimitsReadResponse struct {
	RateLimits          *RateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID map[string]*RateLimitSnapshot `json:"rateLimitsByLimitId"`
	ResetCredits        *ResetCreditsSummary          `json:"rateLimitResetCredits"`
	// AccountID is the opaque account identity the read names, when
	// the app-server build reports one. It never carries an address.
	AccountID string `json:"accountId"`
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

// selectMainRateLimits prefers the main bucket; another limit never counts.
func selectMainRateLimits(snapshot *RateLimitSnapshot, byLimitID map[string]*RateLimitSnapshot) *RateLimitSnapshot {
	if byLimitID != nil {
		if main, ok := byLimitID[CodexMainLimitID]; ok {
			return main
		}
	}
	return snapshot
}

func isSeatScope(scope *string) bool {
	return scope != nil && strings.EqualFold(strings.TrimSpace(*scope), "seat")
}

// buildRateLimitsEvidence projects the selected allowance; raw reset and
// duration stay absent when omitted, tokens need explicit usedTokens.
func buildRateLimitsEvidence(account string, selected *RateLimitSnapshot) []agent.UsageMeterEvidence {
	if selected == nil {
		return nil
	}
	if selected.LimitID != nil && *selected.LimitID != "" && *selected.LimitID != CodexMainLimitID {
		return nil
	}
	var plan, allowance string
	if selected.PlanType != nil {
		plan = *selected.PlanType
	}
	allowance = CodexMainLimitID
	if selected.LimitID != nil && *selected.LimitID != "" {
		allowance = *selected.LimitID
	}
	billing := agent.UsageBillingUnknown
	if selected.BillingMode != nil {
		billing = agent.ParseUsageBillingMode(*selected.BillingMode)
	}
	var user string
	if isSeatScope(selected.Scope) && selected.UserID != nil {
		user = *selected.UserID
	}
	var out []agent.UsageMeterEvidence
	for _, p := range []struct {
		id string
		w  *RateLimitWindow
	}{{"primary", selected.Primary}, {"secondary", selected.Secondary}} {
		if p.w == nil {
			continue
		}
		hasTokens := p.w.UsedTokens != nil
		if hasTokens && (math.IsNaN(*p.w.UsedTokens) || math.IsInf(*p.w.UsedTokens, 0)) {
			continue
		}
		if !hasTokens && (math.IsNaN(p.w.UsedPercent) || math.IsInf(p.w.UsedPercent, 0)) {
			continue
		}
		unit, used := agent.UsageUnitPercent, p.w.UsedPercent
		if hasTokens {
			unit, used = agent.UsageUnitTokens, *p.w.UsedTokens
		}
		var resets *string
		if p.w.ResetsAt != nil {
			if iso := agent.ISOFromEpochSeconds(*p.w.ResetsAt); iso != "" {
				resets = &iso
			}
		}
		out = append(out, agent.UsageMeterEvidence{Provider: "codex", Plan: plan, Allowance: allowance, Window: p.id, Account: account, User: user, Unit: unit, Used: used, Limit: p.w.Limit, ResetsAt: resets, DurationMins: p.w.WindowDurationMins, Billing: billing, Balance: selected.CreditBalance})
	}
	return out
}

// RateLimitsToLimits maps one quota read onto the shared limits
// snapshot. The bucket map selects the main allowance explicitly; a
// legacy snapshot can name another limit, which never counts.
func RateLimitsToLimits(snapshot *RateLimitSnapshot, byLimitID map[string]*RateLimitSnapshot, resetCredits *ResetCreditsSummary, checkedAt string) agent.UsageLimits {
	return rateLimitsToLimitsWithAccount(snapshot, byLimitID, resetCredits, "", checkedAt)
}

func rateLimitsToLimitsWithAccount(snapshot *RateLimitSnapshot, byLimitID map[string]*RateLimitSnapshot, resetCredits *ResetCreditsSummary, account, checkedAt string) agent.UsageLimits {
	selected := selectMainRateLimits(snapshot, byLimitID)
	out := agent.MakeUsageLimits(checkedAt, rateLimitsToWindows(selected))
	out.Evidence = buildRateLimitsEvidence(account, selected)
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
// error: the last good windows stay published. Only the app-server's
// login refusal (rateLimitsRefusalCode) marks the failure as answered;
// a nil client, a timeout, an exit, an unreadable reply or a backend
// fetch the app-server could not complete says nothing about the login
// and carries no verdict.
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
	_, _, limits := decodeRateLimitsRead(resp, checkedAt)
	return limits
}

// decodeRateLimitsRead projects one decoded read onto the account
// identity, the selected plan and the mapped snapshot. Identity, plan
// and usage come from the same reply so the account and the windows
// can never describe different reads; the plan always rides the
// selected bucket, never another bucket's plan.
func decodeRateLimitsRead(resp rateLimitsReadResponse, checkedAt string) (string, string, agent.UsageLimits) {
	selected := selectMainRateLimits(resp.RateLimits, resp.RateLimitsByLimitID)
	var plan string
	if selected != nil && selected.PlanType != nil {
		plan = *selected.PlanType
	}
	limits := rateLimitsToLimitsWithAccount(resp.RateLimits, resp.RateLimitsByLimitID, resp.ResetCredits, resp.AccountID, checkedAt)
	return resp.AccountID, plan, limits
}

// ProbeQuota performs one quota read against the provider's shared
// app-server and reports the opaque account identity, the plan slug
// and the mapped limits snapshot. It starts the app-server on first
// use; a provider with no codex binary or no host login fails closed
// with agent.ErrProviderUnavailable, and a read the app-server never
// answered yields a probe-failed snapshot rather than an error, so
// the caller can feed it straight into the daemon's quota cache.
//
// The account identity is the read's own accountId: the value the
// platform hashes, never an address. Older app-server builds omit it;
// the empty string then means "the login did not name an account",
// and the caller must not treat it as a distinct account.
func (p *Provider) ProbeQuota(ctx context.Context) (accountID, plan string, probed agent.UsageLimits) {
	if p == nil {
		return "", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(time.Now()), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// The shared app-server outlives one probe attempt, so its startup
	// runs under a detached context: a slow but healthy initialize that
	// outlasts this attempt's deadline must not poison the shared
	// Provider with a permanent shutdown. The read below stays bound to
	// the caller, and a cancelled attempt still yields unknown before or
	// after the read without touching shared startup state. The
	// probe-owned ProbeQuotaForLogin below keeps the caller's deadline
	// for startup because it owns its process from construction onward.
	if err := p.ensureProbeStarted(context.Background()); err != nil {
		return "", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(time.Now()), agent.UsageUnavailableProbeFailed, RateLimitsFailureMessage(err))
	}
	p.startMu.Lock()
	client := p.client
	p.startMu.Unlock()
	if client == nil {
		return "", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(time.Now()), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	if ctx.Err() != nil {
		return "", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(time.Now()), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	checkedAt := agent.ISOTime(time.Now())
	reqCtx, cancel := context.WithTimeout(ctx, RateLimitsProbeTimeout)
	defer cancel()
	raw, err := client.Request(reqCtx, "account/rateLimits/read", nil, RateLimitsProbeTimeout)
	if err != nil {
		out := agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, RateLimitsFailureMessage(err))
		out.Unavailable.Answered = answeredByAppServer(client, err)
		return "", "", out
	}
	var resp rateLimitsReadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	if ctx.Err() != nil {
		return "", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(time.Now()), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	return decodeRateLimitsRead(resp, checkedAt)
}

// ProbeQuotaForLogin reads quota for exactly one login through a probe-owned
// app-server, bound by the caller's deadline from construction through
// shutdown. sourceHome selects the login: empty preserves the default host
// resolution, while any non-empty value is an explicit selector that must be
// an absolute path whose auth.json is projected through the checked private
// hard link. A blank or otherwise invalid explicit selector never falls back
// to the default: it yields unknown. The child always starts with the private
// CODEX_HOME and must confirm it; the source home is never passed to the
// child and its files are never written. One quota read serves identity,
// plan and windows together, so a login that answers once is published even
// when a second read would fail. Cancellation at any point — including
// during shutdown grace — yields an unknown snapshot with empty identity
// and plan, superseding even a live login refusal; only an uncancelled
// refusal stays answered. The private home is
// joined before removal and retained when writer quiescence cannot be proved.
func ProbeQuotaForLogin(ctx context.Context, opts Options, sourceHome string) (accountID, plan string, probed agent.UsageLimits) {
	fail := func() (string, string, agent.UsageLimits) {
		return "", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(time.Now()), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	if ctx == nil || ctx.Err() != nil {
		return fail()
	}
	// Any non-empty selector is explicit: a blank selector is an invalid
	// explicit login, not an omitted one, so it must not resolve the default.
	explicit := sourceHome != ""
	var hostAuthFile string
	fileAuth := opts.HostSessionAuth
	if explicit {
		if !opts.HostSessionAuth {
			return fail()
		}
		if strings.TrimSpace(sourceHome) == "" {
			return fail()
		}
		// Resolve before constructing the private home and never consult
		// the default: a valid explicit login must not depend on it.
		resolved, err := explicitHostSessionAuthFile(sourceHome)
		if err != nil {
			return fail()
		}
		if _, err := os.Lstat(resolved); err != nil {
			return fail()
		}
		hostAuthFile = resolved
		fileAuth = true
	} else if opts.HostSessionAuth {
		resolved, err := resolveHostSessionAuthFile()
		if err != nil {
			return fail()
		}
		hostAuthFile = resolved
	}
	provider, err := newProviderWithHostAuth(opts, hostAuthFile, fileAuth)
	if err != nil {
		return fail()
	}
	// Own shutdown from construction onward under the attempt budget: the
	// deferred Shutdown joins the owned group before the private home is
	// removed, caps grace at the time remaining, and requests zero grace
	// once the attempt is cancelled or expired. A cancellation that lands
	// at any point — including during shutdown after a successful read or
	// a live login refusal — supersedes every outcome with unknown: the
	// daemon must not record a fresh login verdict for an attempt it
	// cancelled. Only an uncancelled refusal stays answered.
	defer func() {
		_ = provider.Shutdown(ctx)
		if ctx.Err() != nil {
			accountID, plan, probed = fail()
		}
	}()
	if err := provider.ensureProbeStarted(ctx); err != nil {
		accountID, plan, probed = fail()
		return accountID, plan, probed
	}
	provider.startMu.Lock()
	client := provider.client
	provider.startMu.Unlock()
	if client == nil {
		accountID, plan, probed = fail()
		return accountID, plan, probed
	}
	if ctx.Err() != nil {
		accountID, plan, probed = fail()
		return accountID, plan, probed
	}
	// A single read serves identity, plan and windows from the same reply:
	// discarding it for a second read would publish nothing on a cold cache
	// when the backend fails between the two calls.
	checkedAt := agent.ISOTime(time.Now())
	reqCtx, cancel := context.WithTimeout(ctx, RateLimitsProbeTimeout)
	defer cancel()
	raw, err := client.Request(reqCtx, "account/rateLimits/read", nil, RateLimitsProbeTimeout)
	if err != nil {
		out := agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, RateLimitsFailureMessage(err))
		out.Unavailable.Answered = answeredByAppServer(client, err)
		accountID, plan, probed = "", "", out
		return accountID, plan, probed
	}
	var resp rateLimitsReadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		accountID, plan, probed = fail()
		return accountID, plan, probed
	}
	if ctx.Err() != nil {
		accountID, plan, probed = fail()
		return accountID, plan, probed
	}
	accountID, plan, probed = decodeRateLimitsRead(resp, checkedAt)
	if ctx.Err() != nil {
		accountID, plan, probed = fail()
		return accountID, plan, probed
	}
	return accountID, plan, probed
}

// rateLimitsRefusalCode is the JSON-RPC code the app-server answers a
// rate-limit read with when no usable login backs it ("codex account
// authentication required to read rate limits", "chatgpt
// authentication required to read rate limits"). Its other errors say
// nothing about the login: -32603 is a backend fetch it could not
// complete ("failed to fetch codex rate limits: ..." when the backend
// is unreachable) and -32601 is a build that does not know the method.
const rateLimitsRefusalCode = -32600

// answeredByAppServer reports whether a failed read was the app-server's
// own login refusal. A stopped client fails its pending requests with a
// synthesized JSON-RPC error, so a closed client never counts as an
// answer.
func answeredByAppServer(client *Client, err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != rateLimitsRefusalCode {
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
