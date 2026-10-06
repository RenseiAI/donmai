package claude

// Behavioural tests for the claude quota-window mappers, ported from the
// upstream claude usage-limits suite (see LICENSE-t3code).

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

const claudeTestCheckedAt = "2026-07-18T10:00:00.000Z"

func truePtr() *bool { v := true; return &v }

func TestUsageResponseToLimits_SessionWeeklyAndScoped(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"rate_limits_available": true,
		"rate_limits": {
			"five_hour": {"utilization": 54, "resets_at": "2026-07-18T14:39:00Z"},
			"seven_day": {"utilization": 18.4, "resets_at": "2026-07-24T08:59:00+00:00"},
			"seven_day_opus": {"utilization": 3, "resets_at": null},
			"model_scoped": [
				{"display_name": "Fable", "utilization": 73, "resets_at": "2026-07-24T08:59:00Z"},
				{"display_name": "Ghost", "utilization": null, "resets_at": null}
			]
		}
	}`)
	limits, names := UsageResponseToLimits(raw, claudeTestCheckedAt)
	if names.OverageIncluded != "Fable" {
		t.Errorf("scoped names = %+v, want Fable recorded", names)
	}
	if len(limits.Windows) != 3 {
		t.Fatalf("windows = %+v, want 3", limits.Windows)
	}
	byID := map[string]agent.UsageWindow{}
	for _, w := range limits.Windows {
		byID[w.ID] = w
	}
	session := byID["five_hour"]
	if session.Kind != agent.UsageWindowSession || session.UsedPercent != 54 {
		t.Errorf("session = %+v, want session/54", session)
	}
	if session.WindowDurationMins == nil || *session.WindowDurationMins != 300 {
		t.Errorf("session duration = %v, want 300", session.WindowDurationMins)
	}
	weekly := byID["seven_day"]
	if weekly.Kind != agent.UsageWindowWeekly || weekly.UsedPercent != 18.4 {
		t.Errorf("weekly = %+v, want weekly/18.4", weekly)
	}
	scoped := byID["seven_day_fable"]
	if scoped.Kind != agent.UsageWindowWeekly || scoped.Label != "Weekly · Fable" || scoped.UsedPercent != 73 {
		t.Errorf("scoped = %+v, want Weekly · Fable at 73", scoped)
	}
}

func TestUsageResponseToLimits_Unsupported(t *testing.T) {
	t.Parallel()
	limits, _ := UsageResponseToLimits([]byte(`{"rate_limits_available": false, "rate_limits": null}`), claudeTestCheckedAt)
	if limits.Unavailable == nil || limits.Unavailable.Reason != agent.UsageUnavailableUnsupported {
		t.Errorf("limits = %+v, want unsupported", limits.Unavailable)
	}
	if len(limits.Windows) != 0 {
		t.Errorf("windows = %+v, want none", limits.Windows)
	}
}

func TestUsageResponseToLimits_SkipsWindowWithoutUtilization(t *testing.T) {
	t.Parallel()
	limits, _ := UsageResponseToLimits([]byte(`{
		"rate_limits_available": true,
		"rate_limits": {
			"five_hour": {"utilization": null, "resets_at": null},
			"seven_day": {"utilization": 250, "resets_at": null}
		}
	}`), claudeTestCheckedAt)
	if len(limits.Windows) != 1 || limits.Windows[0].ID != "seven_day" {
		t.Fatalf("windows = %+v, want only seven_day", limits.Windows)
	}
	if limits.Windows[0].UsedPercent != 100 {
		t.Errorf("UsedPercent = %v, want 250 clamped to 100", limits.Windows[0].UsedPercent)
	}
}

func float64Ptr(f float64) *float64 { return &f }

func TestRateLimitEventToUpdate_ScalesOntoProbeID(t *testing.T) {
	t.Parallel()
	info := &RateLimitInfo{Status: "allowed_warning", RateLimitType: "seven_day", Utilization: float64Ptr(0.85), ResetsAt: float64Ptr(1784000000)}
	update := RateLimitEventToUpdate(info, ScopedLimitNames{}, claudeTestCheckedAt)
	if update == nil || len(update.Windows) != 1 {
		t.Fatalf("update = %+v, want one window", update)
	}
	w := update.Windows[0]
	if w.ID != "seven_day" || w.Kind != agent.UsageWindowWeekly || w.UsedPercent != 85 {
		t.Errorf("window = %+v, want seven_day weekly at 85", w)
	}
	if w.ResetsAt == "" || w.WindowDurationMins == nil || *w.WindowDurationMins != 10080 {
		t.Errorf("window = %+v, want reset and 10080 duration", w)
	}
}

func TestRateLimitEventToUpdate_ScopedBucketNeedsProbeName(t *testing.T) {
	t.Parallel()
	info := &RateLimitInfo{Status: "allowed", RateLimitType: "seven_day_overage_included", Utilization: float64Ptr(0.4)}
	if got := RateLimitEventToUpdate(info, ScopedLimitNames{}, claudeTestCheckedAt); got != nil {
		t.Errorf("unnamed bucket produced %+v, want nil", got)
	}
	got := RateLimitEventToUpdate(info, ScopedLimitNames{OverageIncluded: "Fable"}, claudeTestCheckedAt)
	if got == nil || len(got.Windows) != 1 || got.Windows[0].ID != "seven_day_fable" {
		t.Fatalf("update = %+v, want the seven_day_fable row", got)
	}
	if got.Windows[0].UsedPercent != 40 {
		t.Errorf("UsedPercent = %v, want 40", got.Windows[0].UsedPercent)
	}
}

func TestRateLimitEventToUpdate_IgnoresUnknown(t *testing.T) {
	t.Parallel()
	if got := RateLimitEventToUpdate(&RateLimitInfo{Status: "allowed", RateLimitType: "seven_day_opus", Utilization: float64Ptr(0.1)}, ScopedLimitNames{}, claudeTestCheckedAt); got != nil {
		t.Errorf("unrendered window produced %+v, want nil", got)
	}
	if got := RateLimitEventToUpdate(&RateLimitInfo{Status: "rejected", RateLimitType: "five_hour"}, ScopedLimitNames{}, claudeTestCheckedAt); got != nil {
		t.Errorf("utilization-less event produced %+v, want nil", got)
	}
}

func TestParseRateLimitInfo_RecordedLine(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1777673400}}`)
	info, ok := ParseRateLimitInfo(line)
	if !ok || info == nil {
		t.Fatal("recorded line did not parse")
	}
	if info.Status != "allowed" {
		t.Errorf("Status = %q, want allowed", info.Status)
	}
	if info.Utilization != nil {
		t.Errorf("Utilization = %v, want nil (line carries none)", *info.Utilization)
	}
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
}
