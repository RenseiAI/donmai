package claude

// Portions derived from t3code (github.com/pingdotgg/t3code)
// apps/server/src/provider/Layers/claudeUsageLimits.ts @3e6b45028c.
// Copyright (c) 2026 T3 Tools Inc. MIT; see LICENSE-t3code.
//
// Claude subscription usage. Both sources produce windows with the same
// IDs so a turn-driven `rate_limit_event` lands on the row the usage
// read established:
//
//   - the usage read (on demand, during the login check) reports every
//     window at once as 0–100 percentages with ISO reset times;
//   - `rate_limit_event` (streamed during a turn) names one window at a
//     time with a 0–1 utilization fraction and an epoch-seconds reset.
//
// Only the documented SDK `get_usage` shape is read. The undocumented
// usage endpoints are never called.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Claude usage window IDs. The account-wide windows are keyed by the
// rate-limit type; model-scoped weeklies are additive on top of these.
const (
	claudeSessionWindowID = "five_hour"
	claudeWeeklyWindowID  = "seven_day"
	// claudeOverageIncludedEventType is how the streamed event names the
	// overage-included bucket, while the usage read names it by the
	// model's display name.
	claudeOverageIncludedEventType = "seven_day_overage_included"
)

// UsageProbeTimeout bounds one claude usage read: the short-lived CLI
// session it spawns must never outlive the daemon probe tick that
// started it. It stays below the shared 20s probe-attempt ceiling so
// a stuck child cannot hold the quota loop past its per-harness
// budget.
const UsageProbeTimeout = 15 * time.Second

// claudeWindowMeta is the fixed metadata for one account-wide window.
type claudeWindowMeta struct {
	kind         agent.UsageWindowKind
	label        string
	durationMins int
}

// claudeWindows is the account-wide window table.
var claudeWindows = map[string]claudeWindowMeta{
	claudeSessionWindowID: {kind: agent.UsageWindowSession, label: "Session", durationMins: 5 * 60},
	claudeWeeklyWindowID:  {kind: agent.UsageWindowWeekly, label: "Weekly", durationMins: 7 * 24 * 60},
}

// ScopedLimitNames records the model-scoped bucket name the usage read
// saw. Which model the overage-included bucket belongs to changes over
// time, so the probe records the name it saw and the event mapper
// reuses it; the mid-turn update then lands on the row the probe drew
// instead of opening a second one.
type ScopedLimitNames struct {
	// OverageIncluded is the display name of the model-scoped weekly
	// bucket the last usage read drew a row for.
	OverageIncluded string
}

// scopedWindowID derives the window ID for one model-scoped bucket.
func scopedWindowID(displayName string) string {
	var b strings.Builder
	b.WriteString("seven_day_")
	prevUnderscore := true
	for _, r := range strings.ToLower(displayName) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteRune('_')
				prevUnderscore = true
			}
		}
	}
	id := strings.TrimRight(b.String(), "_")
	if id == "seven_day_" || id == "seven_day" {
		return "seven_day_scoped"
	}
	return id
}

// scopedWindow builds one model-scoped weekly window.
func scopedWindow(displayName string, usedPercent float64, resetsAt string) agent.UsageWindow {
	duration := 7 * 24 * 60
	w := agent.UsageWindow{
		ID:                 scopedWindowID(displayName),
		Kind:               agent.UsageWindowWeekly,
		Label:              "Weekly · " + displayName,
		UsedPercent:        agent.ClampPercent(usedPercent),
		WindowDurationMins: &duration,
	}
	if resetsAt != "" {
		w.ResetsAt = resetsAt
	}
	return w
}

// makeClaudeWindow builds one account-wide window.
func makeClaudeWindow(id string, usedPercent float64, resetsAt string) agent.UsageWindow {
	meta := claudeWindows[id]
	duration := meta.durationMins
	w := agent.UsageWindow{
		ID:                 id,
		Kind:               meta.kind,
		Label:              meta.label,
		UsedPercent:        agent.ClampPercent(usedPercent),
		WindowDurationMins: &duration,
	}
	if resetsAt != "" {
		w.ResetsAt = resetsAt
	}
	return w
}

// isoFromString normalizes an ISO reset time. Empty or unparseable
// readings carry no reset.
func isoFromString(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, v); err != nil {
			return ""
		}
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// RateLimitInfo is the structural view of the streamed
// `rate_limit_event` info block.
type RateLimitInfo struct {
	Status        string   `json:"status"`
	RateLimitType string   `json:"rateLimitType"`
	Utilization   *float64 `json:"utilization"`
	ResetsAt      *float64 `json:"resetsAt"`
}

// ParseRateLimitInfo decodes the `rate_limit_info` block of a streamed
// `rate_limit_event` line.
func ParseRateLimitInfo(line []byte) (*RateLimitInfo, bool) {
	var envelope struct {
		RateLimitInfo *RateLimitInfo `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil || envelope.RateLimitInfo == nil {
		return nil, false
	}
	return envelope.RateLimitInfo, true
}

// RateLimitEventToUpdate maps one streamed `rate_limit_event` onto a
// sparse update through the same window shapes the stream driver uses:
// account-wide windows map by rate-limit type, and the overage-included
// event maps onto the model-scoped row the usage read drew. An
// overage-included event before any probe has named the bucket, or an
// unknown type, is dropped: drawing a row the probe cannot reconcile
// would orphan the stream row. checkedAt stamps the update.
func RateLimitEventToUpdate(info *RateLimitInfo, names ScopedLimitNames, checkedAt string) *agent.UsageLimitsUpdate {
	if info == nil || info.Utilization == nil {
		return nil
	}
	used := *info.Utilization * 100
	var resetsAt string
	if info.ResetsAt != nil {
		resetsAt = agent.ISOFromEpochSeconds(*info.ResetsAt)
	}
	if _, ok := claudeWindows[info.RateLimitType]; ok {
		return &agent.UsageLimitsUpdate{
			CheckedAt: checkedAt,
			Windows:   []agent.UsageWindow{makeClaudeWindow(info.RateLimitType, used, resetsAt)},
		}
	}
	if info.RateLimitType == claudeOverageIncludedEventType && names.OverageIncluded != "" {
		return &agent.UsageLimitsUpdate{
			CheckedAt: checkedAt,
			Windows:   []agent.UsageWindow{scopedWindow(names.OverageIncluded, used, resetsAt)},
		}
	}
	return nil
}

// usageWindowJSON is the structural view of one window on the usage
// read response.
type usageWindowJSON struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
	UsedTokens  *float64 `json:"used_tokens,omitempty"`
	Limit       *float64 `json:"limit,omitempty"`
}

type modelScopedWindowJSON struct {
	DisplayName string   `json:"display_name"`
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
	UsedTokens  *float64 `json:"used_tokens,omitempty"`
	Limit       *float64 `json:"limit,omitempty"`
}

// UsageResponse is the structural view of one get_usage control answer.
type UsageResponse struct {
	Available        *bool           `json:"rate_limits_available"`
	Windows          json.RawMessage `json:"rate_limits"`
	SubscriptionType *string         `json:"subscription_type,omitempty"`
	BillingMode      *string         `json:"billing_mode,omitempty"`
	CreditBalance    *float64        `json:"credit_balance,omitempty"`
	AccountID        *string         `json:"account_id,omitempty"`
	Scope            *string         `json:"scope,omitempty"`
	UserID           *string         `json:"user_id,omitempty"`
}

func claudeSeatUser(scope, userID *string) string {
	if scope == nil || userID == nil || !strings.EqualFold(strings.TrimSpace(*scope), "seat") || strings.TrimSpace(*userID) == "" {
		return ""
	}
	return *userID
}

// UsageResponseToLimits maps one usage read onto the shared limits
// snapshot. Percentages on the response are already 0–100. It also
// yields the scoped-bucket names the response carried, for the event
// mapper to reuse. An unavailable read reports the account as
// unsupported: an account with no subscription windows never reports
// them.
//
// Availability matches the pre-evidence mapper exactly: it depends
// only on rate_limits_available and whether the windows payload parses.
// Meter evidence rides on top without changing Unavailable, so a read
// with windows but no display rows stays supported with empty windows
// (as before) and a read with no windows payload stays unsupported
// (as before).
func UsageResponseToLimits(raw []byte, checkedAt string) (agent.UsageLimits, ScopedLimitNames) {
	var resp UsageResponse
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Available == nil || !*resp.Available || len(resp.Windows) == 0 {
		return agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableUnsupported, ""), ScopedLimitNames{}
	}
	var plan string
	if resp.SubscriptionType != nil {
		plan = *resp.SubscriptionType
	}
	billing := agent.UsageBillingUnknown
	if resp.BillingMode != nil {
		billing = agent.ParseUsageBillingMode(*resp.BillingMode)
	}
	var account string
	if resp.AccountID != nil {
		account = *resp.AccountID
	}
	user := claudeSeatUser(resp.Scope, resp.UserID)
	var windows map[string]json.RawMessage
	if err := json.Unmarshal(resp.Windows, &windows); err != nil {
		return agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableUnsupported, ""), ScopedLimitNames{}
	}
	if windows == nil {
		windows = map[string]json.RawMessage{}
	}
	var out []agent.UsageWindow
	var evidence []agent.UsageMeterEvidence
	for _, id := range []string{claudeSessionWindowID, claudeWeeklyWindowID} {
		rawWindow, ok := windows[id]
		if !ok {
			continue
		}
		var w usageWindowJSON
		if err := json.Unmarshal(rawWindow, &w); err != nil || (w.Utilization == nil && w.UsedTokens == nil) {
			continue
		}
		if w.Utilization != nil {
			out = append(out, makeClaudeWindow(id, *w.Utilization, isoFromString(ptrOrEmpty(w.ResetsAt))))
		}
		var unit agent.UsageObservedUnit
		var used float64
		switch {
		case w.UsedTokens != nil:
			unit, used = agent.UsageUnitTokens, *w.UsedTokens
		case w.Utilization != nil:
			unit, used = agent.UsageUnitPercent, *w.Utilization
		default:
			continue
		}
		var resets *string
		if iso := isoFromString(ptrOrEmpty(w.ResetsAt)); iso != "" {
			resets = &iso
		}
		evidence = append(evidence, agent.UsageMeterEvidence{Provider: "claude", Plan: plan, Allowance: id, Window: id, Account: account, User: user, Unit: unit, Used: used, Limit: w.Limit, ResetsAt: resets, Billing: billing, Balance: resp.CreditBalance})
	}
	var names ScopedLimitNames
	if rawScoped, ok := windows["model_scoped"]; ok && len(rawScoped) > 0 {
		var entries []modelScopedWindowJSON
		if err := json.Unmarshal(rawScoped, &entries); err == nil {
			for _, e := range entries {
				if strings.TrimSpace(e.DisplayName) == "" || e.Utilization == nil {
					continue
				}
				out = append(out, scopedWindow(e.DisplayName, *e.Utilization, isoFromString(ptrOrEmpty(e.ResetsAt))))
				if names.OverageIncluded == "" {
					names.OverageIncluded = e.DisplayName
				}
				var sunit agent.UsageObservedUnit
				var sused float64
				switch {
				case e.UsedTokens != nil:
					sunit, sused = agent.UsageUnitTokens, *e.UsedTokens
				default:
					sunit, sused = agent.UsageUnitPercent, *e.Utilization
				}
				var sresets *string
				if iso := isoFromString(ptrOrEmpty(e.ResetsAt)); iso != "" {
					sresets = &iso
				}
				evidence = append(evidence, agent.UsageMeterEvidence{Provider: "claude", Plan: plan, Allowance: "model_scoped", Window: scopedWindowID(e.DisplayName), Account: account, User: user, Unit: sunit, Used: sused, Limit: e.Limit, ResetsAt: sresets, Billing: billing, Balance: resp.CreditBalance})
			}
		}
	}
	if len(out) == 0 && len(evidence) == 0 {
		if resp.BillingMode != nil || resp.CreditBalance != nil || resp.AccountID != nil || resp.SubscriptionType != nil || user != "" {
			limits := agent.MakeUsageLimits(checkedAt, nil)
			limits.Evidence = []agent.UsageMeterEvidence{{Provider: "claude", Plan: plan, Account: account, User: user, Billing: billing, Balance: resp.CreditBalance}}
			return limits, names
		}
		return agent.MakeUsageLimits(checkedAt, out), names
	}
	limits := agent.MakeUsageLimits(checkedAt, out)
	limits.Evidence = evidence
	return limits, names
}

func ptrOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
