package agent

// Behavioural tests for the shared quota-window types, ported from the
// upstream provider usage-limits suite (see LICENSE-t3code).

import (
	"encoding/json"
	"testing"
	"time"
)

func usageTestCheckedAt() string { return "2026-09-03T12:00:00.000Z" }

func usageTestSession() UsageWindow {
	d := 300
	return UsageWindow{
		ID:                 "five_hour",
		Kind:               UsageWindowSession,
		Label:              "Session",
		UsedPercent:        40,
		ResetsAt:           "2026-09-03T14:00:00.000Z",
		WindowDurationMins: &d,
	}
}

func usageTestWeekly() UsageWindow {
	d := 10080
	return UsageWindow{
		ID:                 "seven_day",
		Kind:               UsageWindowWeekly,
		Label:              "Weekly",
		UsedPercent:        20,
		WindowDurationMins: &d,
	}
}

func TestApplyUsageLimitsUpdate_NoChangeReturnsPrevious(t *testing.T) {
	t.Parallel()
	session, weekly := usageTestSession(), usageTestWeekly()
	published := MakeUsageLimits(usageTestCheckedAt(), []UsageWindow{session, weekly})
	next := ApplyUsageLimitsUpdate(&published, UsageLimitsUpdate{
		CheckedAt: "2026-09-03T12:00:05.000Z",
		Windows: []UsageWindow{
			weekly,
			{ID: "five_hour", Kind: UsageWindowSession, Label: "Session", UsedPercent: 40},
		},
	}, "2026-09-03T12:00:05.000Z")
	if next != &published {
		t.Fatalf("no-op update allocated a new snapshot; want the published object back")
	}
}

func TestApplyUsageLimitsUpdate_UpsertsAndKeepsOmittedReset(t *testing.T) {
	t.Parallel()
	session, weekly := usageTestSession(), usageTestWeekly()
	published := MakeUsageLimits(usageTestCheckedAt(), []UsageWindow{session, weekly})
	next := ApplyUsageLimitsUpdate(&published, UsageLimitsUpdate{
		CheckedAt: "2026-09-03T12:00:05.000Z",
		Windows:   []UsageWindow{{ID: "five_hour", Kind: UsageWindowSession, Label: "Session", UsedPercent: 55}},
	}, "2026-09-03T12:00:05.000Z")
	if next == &published {
		t.Fatal("changed update returned the published object; want a new snapshot")
	}
	if len(next.Windows) != 2 {
		t.Fatalf("got %d windows, want 2", len(next.Windows))
	}
	var got *UsageWindow
	for i := range next.Windows {
		if next.Windows[i].ID == "five_hour" {
			got = &next.Windows[i]
		}
	}
	if got == nil {
		t.Fatal("five_hour window missing after update")
	}
	if got.UsedPercent != 55 {
		t.Errorf("UsedPercent = %v, want 55", got.UsedPercent)
	}
	if got.ResetsAt != session.ResetsAt {
		t.Errorf("ResetsAt = %q, want the probe value %q kept", got.ResetsAt, session.ResetsAt)
	}
	if got.WindowDurationMins == nil || *got.WindowDurationMins != *session.WindowDurationMins {
		t.Errorf("WindowDurationMins = %v, want the probe value kept", got.WindowDurationMins)
	}
}

func TestApplyUsageLimitsUpdate_UnsupportedAndEmpty(t *testing.T) {
	t.Parallel()
	session := usageTestSession()
	unsupported := MakeUnavailableUsageLimits(usageTestCheckedAt(), UsageUnavailableUnsupported, "")
	if next := ApplyUsageLimitsUpdate(&unsupported, UsageLimitsUpdate{Windows: []UsageWindow{session}}, usageTestCheckedAt()); next != &unsupported {
		t.Error("unsupported snapshot accepted windows; want it left alone")
	}
	published := MakeUsageLimits(usageTestCheckedAt(), []UsageWindow{session})
	if next := ApplyUsageLimitsUpdate(&published, UsageLimitsUpdate{}, usageTestCheckedAt()); next != &published {
		t.Error("empty update replaced the snapshot; want it left alone")
	}
}

func TestApplyUsageLimitsUpdate_PreservesResetCredits(t *testing.T) {
	t.Parallel()
	session, weekly := usageTestSession(), usageTestWeekly()
	published := MakeUsageLimits(usageTestCheckedAt(), []UsageWindow{session, weekly})
	published.ResetCredits = &ResetCredits{AvailableCount: 2, NextExpiresAt: "2026-10-01T00:00:00.000Z"}
	next := ApplyUsageLimitsUpdate(&published, UsageLimitsUpdate{
		CheckedAt: "2026-09-03T12:00:05.000Z",
		Windows:   []UsageWindow{{ID: "five_hour", Kind: UsageWindowSession, Label: "Session", UsedPercent: 55, ResetsAt: session.ResetsAt}},
	}, "2026-09-03T12:00:05.000Z")
	if next.ResetCredits == nil || *next.ResetCredits != *published.ResetCredits {
		t.Errorf("ResetCredits = %+v, want %+v preserved", next.ResetCredits, published.ResetCredits)
	}
}

func TestResolveUsageLimitsAfterProbe(t *testing.T) {
	t.Parallel()
	session, weekly := usageTestSession(), usageTestWeekly()
	published := MakeUsageLimits(usageTestCheckedAt(), []UsageWindow{session, weekly})
	failed := MakeUnavailableUsageLimits(usageTestCheckedAt(), UsageUnavailableProbeFailed, "")
	unsupported := MakeUnavailableUsageLimits(usageTestCheckedAt(), UsageUnavailableUnsupported, "")
	if got := ResolveUsageLimitsAfterProbe(&published, &failed); got != &published {
		t.Error("failed probe wiped the last good windows; want them kept")
	}
	if got := ResolveUsageLimitsAfterProbe(&published, &unsupported); got != &unsupported {
		t.Error("unsupported reading did not clear the windows; want it authoritative")
	}
	if got := ResolveUsageLimitsAfterProbe(nil, &failed); got != &failed {
		t.Error("failed probe with nothing published should stay a failed probe")
	}
}

func TestUsageProbeAllowed_FiveMinuteInterval(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if !UsageProbeAllowed(time.Time{}, base, 5*time.Minute) {
		t.Error("first probe must be allowed")
	}
	last := base
	if !UsageProbeAllowed(last, base.Add(5*time.Minute), 5*time.Minute) {
		t.Error("probe at exactly the interval must be allowed")
	}
	if UsageProbeAllowed(last, base.Add(4*time.Minute+59*time.Second), 5*time.Minute) {
		t.Error("probe inside the interval must be refused")
	}
}

func TestClampPercent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   float64
		want float64
	}{
		{54, 54},
		{-3, 0},
		{250, 100},
	} {
		if got := ClampPercent(tc.in); got != tc.want {
			t.Errorf("ClampPercent(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPoolKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mins int
		want UsageWindowKind
	}{
		{300, UsageWindowSession},
		{10080, UsageWindowWeekly},
		{43200, UsageWindowMonthly},
	} {
		if got := PoolKind(tc.mins); got != tc.want {
			t.Errorf("PoolKind(%d) = %q, want %q", tc.mins, got, tc.want)
		}
	}
}

func TestUsageEvent_RoundTrip(t *testing.T) {
	t.Parallel()
	d := 10080
	in := UsageEvent{
		Message: "allowed_warning",
		Usage: &UsageLimitsUpdate{
			Windows: []UsageWindow{{ID: "seven_day", Kind: UsageWindowWeekly, Label: "Weekly", UsedPercent: 85, WindowDurationMins: &d}},
		},
	}
	if in.Kind() != EventUsage {
		t.Fatalf("Kind() = %q, want usage", in.Kind())
	}
	raw, err := MarshalEvent(in)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	out, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	ev, ok := out.(UsageEvent)
	if !ok {
		t.Fatalf("decoded %T, want UsageEvent", out)
	}
	if ev.Usage == nil || len(ev.Usage.Windows) != 1 || ev.Usage.Windows[0].ID != "seven_day" {
		t.Fatalf("decoded usage = %+v, want the seven_day window", ev.Usage)
	}
}

// An answered read with no windows serializes limits.windows as an empty
// array, never null: consumers read it as an array.
func TestMakeUsageLimits_EmptyWindowsSerializeAsArray(t *testing.T) {
	t.Parallel()
	for _, in := range [][]UsageWindow{nil, {}} {
		raw, err := json.Marshal(MakeUsageLimits(usageTestCheckedAt(), in))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if want := `{"checkedAt":"` + usageTestCheckedAt() + `","windows":[]}`; string(raw) != want {
			t.Fatalf("limits JSON = %s, want %s", raw, want)
		}
	}
}
