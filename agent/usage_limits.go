package agent

// Portions derived from t3code (github.com/pingdotgg/t3code)
// apps/server/src/provider/providerUsageLimits.ts and
// packages/contracts/src/providerUsageLimits.ts @3e6b45028c.
// Copyright (c) 2026 T3 Tools Inc. MIT; see LICENSE-t3code.
//
// Shared subscription quota-window types. A subscription provider reports
// one rolling quota window per allowance (for example a five-hour session
// or a weekly allowance) for the signed-in account. `id` is stable per
// provider so a sparse turn-driven update lands on the same row a full
// probe produced; `kind` only orders and labels the bar.

import (
	"math"
	"sort"
	"strings"
	"time"
)

// UsageWindowKind orders and labels one quota window.
type UsageWindowKind string

// Usage window kinds.
const (
	UsageWindowSession UsageWindowKind = "session"
	UsageWindowWeekly  UsageWindowKind = "weekly"
	UsageWindowMonthly UsageWindowKind = "monthly"
	UsageWindowOther   UsageWindowKind = "other"
)

// usageWindowKindOrder matches the upstream kind ordering: session first,
// then weekly, monthly, and anything unrecognised last.
func usageWindowKindOrder(k UsageWindowKind) int {
	switch k {
	case UsageWindowSession:
		return 0
	case UsageWindowWeekly:
		return 1
	case UsageWindowMonthly:
		return 2
	default:
		return 3
	}
}

// UsageWindow is one rolling quota window a subscription provider reports
// for the signed-in account.
type UsageWindow struct {
	// ID is stable per provider (for example "five_hour",
	// "seven_day_opus", "primary") so a sparse update lands on the same
	// row a full probe produced.
	ID string `json:"id"`
	// Kind only orders and labels the bar.
	Kind UsageWindowKind `json:"kind"`
	// Label is the display label for the bar.
	Label string `json:"label"`
	// UsedPercent is the consumed share of the window, clamped to 0–100.
	UsedPercent float64 `json:"usedPercent"`
	// ResetsAt is the ISO reset time, when the provider reports one.
	ResetsAt string `json:"resetsAt,omitempty"`
	// WindowDurationMins is the window length in minutes, when known.
	WindowDurationMins *int `json:"windowDurationMins,omitempty"`
}

// ResetCredits are the reset credits a provider banks on the account,
// granted when it has rate-limited the user unfairly. Only present when
// the provider reports them at all.
type ResetCredits struct {
	AvailableCount int    `json:"availableCount"`
	NextExpiresAt  string `json:"nextExpiresAt,omitempty"`
}

// UnavailableReason distinguishes an account that can never report
// windows from a probe that failed this time.
type UnavailableReason string

// Unavailable reasons.
const (
	// UsageUnavailableUnsupported marks an account that cannot report
	// windows (for example an API-key login). Authoritative: it clears
	// previously published windows.
	UsageUnavailableUnsupported UnavailableReason = "unsupported"
	// UsageUnavailableProbeFailed marks a probe that failed this time.
	// The last good bars stay published.
	UsageUnavailableProbeFailed UnavailableReason = "probeFailed"
)

// UsageUnavailable is the bounded, client-safe reason a probe produced
// no windows.
type UsageUnavailable struct {
	Reason  UnavailableReason `json:"reason"`
	Message string            `json:"message,omitempty"`
}

// UsageLimits is the subscription usage a provider knows about the
// signed-in account.
type UsageLimits struct {
	CheckedAt string        `json:"checkedAt"`
	Windows   []UsageWindow `json:"windows"`
	// ResetCredits rides along when the provider reports reset credits.
	ResetCredits *ResetCredits `json:"resetCredits,omitempty"`
	// Unavailable distinguishes "cannot report" from "failed this time".
	Unavailable *UsageUnavailable `json:"unavailable,omitempty"`
}

// UsageLimitsUpdate is what a harness reports when its runtime pushes a
// rate-limit update during a turn. Sparse by contract: the streamed event
// names one window at a time and the read names them all. Windows merge
// by ID onto the published snapshot; omitted windows are unchanged.
type UsageLimitsUpdate struct {
	CheckedAt string        `json:"checkedAt"`
	Windows   []UsageWindow `json:"windows"`
}

// UsageAccount is one account a quota source reports on. The account ID
// is an opaque value the platform hashes; it never carries an address.
type UsageAccount struct {
	// ID is the opaque account identity. Never an email address.
	ID string `json:"id"`
	// Provider names the harness the account belongs to (for example
	// "codex" or "claude").
	Provider string `json:"provider"`
	// Plan is the plan slug as the provider reports it (for example
	// "promax" or "team").
	Plan string `json:"plan,omitempty"`
	// LimitID names the allowance bucket the windows describe, when the
	// provider reports one (for example "codex").
	LimitID string      `json:"limitId,omitempty"`
	Limits  UsageLimits `json:"limits"`
}

// ClampPercent bounds a consumed share to the 0–100 display range.
// A non-finite reading reports as 0 rather than propagating NaN.
func ClampPercent(value float64) float64 {
	switch {
	case math.IsNaN(value) || math.IsInf(value, 0):
		return 0
	case value < 0:
		return 0
	case value > 100:
		return 100
	default:
		return value
	}
}

// SortUsageWindows orders windows by kind (session, weekly, monthly,
// other) then by ID.
func SortUsageWindows(windows []UsageWindow) []UsageWindow {
	out := append([]UsageWindow(nil), windows...)
	sort.Slice(out, func(i, j int) bool {
		if oi, oj := usageWindowKindOrder(out[i].Kind), usageWindowKindOrder(out[j].Kind); oi != oj {
			return oi < oj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// MakeUsageLimits builds a limits snapshot with sorted windows.
func MakeUsageLimits(checkedAt string, windows []UsageWindow) UsageLimits {
	return UsageLimits{CheckedAt: checkedAt, Windows: SortUsageWindows(windows)}
}

// MakeUnavailableUsageLimits builds a limits snapshot that reports why
// no windows are published.
func MakeUnavailableUsageLimits(checkedAt string, reason UnavailableReason, message string) UsageLimits {
	out := UsageLimits{CheckedAt: checkedAt, Unavailable: &UsageUnavailable{Reason: reason}}
	if message != "" {
		out.Unavailable.Message = message
	}
	return out
}

// usageWindowEquals reports whether two windows carry identical values.
func usageWindowEquals(a, b UsageWindow) bool {
	if a.ID != b.ID || a.Kind != b.Kind || a.Label != b.Label || a.UsedPercent != b.UsedPercent || a.ResetsAt != b.ResetsAt {
		return false
	}
	if (a.WindowDurationMins == nil) != (b.WindowDurationMins == nil) {
		return false
	}
	return a.WindowDurationMins == nil || *a.WindowDurationMins == *b.WindowDurationMins
}

// ApplyUsageLimitsUpdate folds a sparse runtime update into the limits a
// provider currently publishes. Windows upsert by ID; a window the update
// omits keeps its previous values, and a window that arrives without a
// reset time or duration keeps whatever the last probe resolved for it.
// An update with no windows leaves the previous snapshot untouched.
//
// An unsupported snapshot stays unsupported: an account that cannot have
// subscription windows will not start reporting them mid-turn.
func ApplyUsageLimitsUpdate(previous *UsageLimits, update UsageLimitsUpdate, checkedAt string) *UsageLimits {
	if len(update.Windows) == 0 {
		return previous
	}
	if previous != nil && previous.Unavailable != nil && previous.Unavailable.Reason == UsageUnavailableUnsupported {
		return previous
	}
	merged := make(map[string]UsageWindow)
	var order []string
	if previous != nil {
		for _, w := range previous.Windows {
			if _, ok := merged[w.ID]; !ok {
				order = append(order, w.ID)
			}
			merged[w.ID] = w
		}
	}
	changed := false
	for _, w := range update.Windows {
		existing, ok := merged[w.ID]
		next := w
		next.UsedPercent = ClampPercent(w.UsedPercent)
		if next.ResetsAt == "" && ok && existing.ResetsAt != "" {
			next.ResetsAt = existing.ResetsAt
		}
		if next.WindowDurationMins == nil && ok && existing.WindowDurationMins != nil {
			d := *existing.WindowDurationMins
			next.WindowDurationMins = &d
		}
		if _, seen := merged[w.ID]; !seen {
			order = append(order, w.ID)
		}
		if !ok || !usageWindowEquals(existing, next) {
			changed = true
		}
		merged[w.ID] = next
	}
	if !changed && previous != nil && previous.Unavailable == nil {
		return previous
	}
	ordered := make([]UsageWindow, 0, len(merged))
	for _, id := range order {
		ordered = append(ordered, merged[id])
	}
	out := MakeUsageLimits(checkedAt, ordered)
	if previous != nil && previous.ResetCredits != nil {
		rc := *previous.ResetCredits
		out.ResetCredits = &rc
	}
	return &out
}

// ResolveUsageLimitsAfterProbe chooses what to publish after a probe
// finishes. A probe that failed this time must not wipe bars a previous
// probe or a turn already established, so the last good snapshot stays;
// an unsupported reading is authoritative and replaces them.
//
// A successful probe replaces the published windows outright, including
// any runtime update that landed while it was running.
func ResolveUsageLimitsAfterProbe(published, probed *UsageLimits) *UsageLimits {
	if probed != nil && probed.Unavailable != nil && probed.Unavailable.Reason == UsageUnavailableProbeFailed {
		if published != nil && published.Unavailable == nil {
			return published
		}
	}
	return probed
}

// UsageProbeAllowed reports whether a probe may fire at now given the
// last probe time. Probes are rate-limited to once per the interval so
// quota reads stay background traffic.
func UsageProbeAllowed(lastProbe, now time.Time, interval time.Duration) bool {
	if lastProbe.IsZero() {
		return true
	}
	return !now.Before(lastProbe.Add(interval))
}

// ISOTime formats a time as the ISO instant the limits snapshots carry.
func ISOTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ISOFromEpochSeconds renders epoch seconds as the ISO instant the
// snapshots carry. Non-positive or non-finite readings carry no reset.
func ISOFromEpochSeconds(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return ""
	}
	return time.Unix(int64(value), 0).UTC().Format(time.RFC3339Nano)
}

// PoolKind classifies one window duration for pool display. Positions
// are not durations: anything below a week reads as the session bar,
// matching the upstream mapper the provider ports follow.
func PoolKind(windowDurationMins int) UsageWindowKind {
	const (
		weekMins  = 7 * 24 * 60
		monthMins = 30 * 24 * 60
	)
	switch {
	case windowDurationMins >= monthMins:
		return UsageWindowMonthly
	case windowDurationMins >= weekMins:
		return UsageWindowWeekly
	default:
		return UsageWindowSession
	}
}

// PoolLabel is the display label for one pool kind.
func PoolLabel(kind UsageWindowKind) string {
	switch kind {
	case UsageWindowSession:
		return "Session"
	case UsageWindowWeekly:
		return "Weekly"
	case UsageWindowMonthly:
		return "Monthly"
	default:
		return "Other"
	}
}

// normalizeUsageWindowKind maps an arbitrary kind string onto the closed
// kind set; unknown values land on "other".
func normalizeUsageWindowKind(kind string) UsageWindowKind {
	switch UsageWindowKind(strings.ToLower(strings.TrimSpace(kind))) {
	case UsageWindowSession:
		return UsageWindowSession
	case UsageWindowWeekly:
		return UsageWindowWeekly
	case UsageWindowMonthly:
		return UsageWindowMonthly
	case UsageWindowOther:
		return UsageWindowOther
	default:
		return UsageWindowOther
	}
}
