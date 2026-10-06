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
// sparse update. Utilization is a 0–1 fraction on the streamed event.
// An overage-included event before any probe has named the bucket is
// dropped: guessing a name would draw a row the next probe cannot
// reconcile.
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
}

// modelScopedWindowJSON is the structural view of one model-scoped
// entry. It shipped in the CLI after the pinned SDK typings, so it is
// read structurally.
type modelScopedWindowJSON struct {
	DisplayName string   `json:"display_name"`
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// UsageResponse is the structural view of the usage read response.
type UsageResponse struct {
	Available *bool           `json:"rate_limits_available"`
	Windows   json.RawMessage `json:"rate_limits"`
}

// UsageResponseToLimits maps one usage read onto the shared limits
// snapshot. Percentages on the response are already 0–100. It also
// yields the scoped-bucket names the response carried, for the event
// mapper to reuse. An unavailable read reports the account as
// unsupported: an account with no subscription windows never reports
// them.
func UsageResponseToLimits(raw []byte, checkedAt string) (agent.UsageLimits, ScopedLimitNames) {
	var resp UsageResponse
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Available == nil || !*resp.Available || len(resp.Windows) == 0 {
		return agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableUnsupported, ""), ScopedLimitNames{}
	}
	var windows map[string]json.RawMessage
	if err := json.Unmarshal(resp.Windows, &windows); err != nil {
		return agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableUnsupported, ""), ScopedLimitNames{}
	}
	var out []agent.UsageWindow
	for _, id := range []string{claudeSessionWindowID, claudeWeeklyWindowID} {
		rawWindow, ok := windows[id]
		if !ok {
			continue
		}
		var w usageWindowJSON
		if err := json.Unmarshal(rawWindow, &w); err != nil || w.Utilization == nil {
			continue
		}
		var resetsAt string
		if w.ResetsAt != nil {
			resetsAt = isoFromString(*w.ResetsAt)
		}
		out = append(out, makeClaudeWindow(id, *w.Utilization, resetsAt))
	}
	// The CLI filters model-scoped entries to the overage-included
	// allowlist; the first entry that draws a row is the one the
	// streamed event refers to.
	var names ScopedLimitNames
	if rawScoped, ok := windows["model_scoped"]; ok && len(rawScoped) > 0 {
		var entries []modelScopedWindowJSON
		if err := json.Unmarshal(rawScoped, &entries); err == nil {
			for _, e := range entries {
				if strings.TrimSpace(e.DisplayName) == "" || e.Utilization == nil {
					continue
				}
				var resetsAt string
				if e.ResetsAt != nil {
					resetsAt = isoFromString(*e.ResetsAt)
				}
				out = append(out, scopedWindow(e.DisplayName, *e.Utilization, resetsAt))
				// Only a bucket that drew a row may receive events;
				// naming one that was skipped would let a mid-turn
				// event open a row the probe never showed.
				if names.OverageIncluded == "" {
					names.OverageIncluded = e.DisplayName
				}
			}
		}
	}
	return agent.MakeUsageLimits(checkedAt, out), names
}
