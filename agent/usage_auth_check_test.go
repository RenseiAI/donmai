package agent

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestUsageAuthCheckAfterProbe(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	live := MakeUsageLimits(ISOTime(at), []UsageWindow{usageTestWeekly()})
	empty := MakeUsageLimits(ISOTime(at), nil)
	refused := MakeUnavailableUsageLimits(ISOTime(at), UsageUnavailableProbeFailed, "Codex could not read usage (JSON-RPC -32600).")
	refused.Unavailable.Answered = true
	unreached := MakeUnavailableUsageLimits(ISOTime(at), UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	unsupported := MakeUnavailableUsageLimits(ISOTime(at), UsageUnavailableUnsupported, "")

	tests := []struct {
		name    string
		harness string
		probed  *UsageLimits
		at      time.Time
		want    *UsageAuthCheck
	}{
		{
			name:    "windows read proves the login",
			harness: UsageHarnessCodex,
			probed:  &live,
			at:      at,
			want:    &UsageAuthCheck{Harness: "codex", OK: true, CheckedAt: "2026-10-06T12:00:00Z"},
		},
		{
			name:    "answered read without windows still proves the login",
			harness: UsageHarnessClaude,
			probed:  &empty,
			at:      at,
			want:    &UsageAuthCheck{Harness: "claude", OK: true, CheckedAt: "2026-10-06T12:00:00Z"},
		},
		{
			name:    "read the provider answered with a refusal is a failed check",
			harness: UsageHarnessCodex,
			probed:  &refused,
			at:      at,
			want:    &UsageAuthCheck{Harness: "codex", OK: false, CheckedAt: "2026-10-06T12:00:00Z"},
		},
		{
			name:    "read that never reached the provider reports no check",
			harness: UsageHarnessCodex,
			probed:  &unreached,
			at:      at,
			want:    nil,
		},
		{
			name:    "account with no subscription windows is not a verified login",
			harness: UsageHarnessClaude,
			probed:  &unsupported,
			at:      at,
			want:    &UsageAuthCheck{Harness: "claude", OK: false, CheckedAt: "2026-10-06T12:00:00Z"},
		},
		{
			name:    "checkedAt is the probe time, not the limits stamp",
			harness: UsageHarnessCodex,
			probed:  &live,
			at:      at.Add(90 * time.Second),
			want:    &UsageAuthCheck{Harness: "codex", OK: true, CheckedAt: "2026-10-06T12:01:30Z"},
		},
		{
			name:    "non-UTC probe time is reported in UTC",
			harness: UsageHarnessCodex,
			probed:  &live,
			at:      at.In(time.FixedZone("offset", 2*60*60)),
			want:    &UsageAuthCheck{Harness: "codex", OK: true, CheckedAt: "2026-10-06T12:00:00Z"},
		},
		{name: "no probe outcome reports no check", harness: UsageHarnessCodex, probed: nil, at: at, want: nil},
		{name: "no probe time reports no check", harness: UsageHarnessCodex, probed: &live, at: time.Time{}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := UsageAuthCheckAfterProbe(tt.harness, tt.probed, tt.at)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("UsageAuthCheckAfterProbe = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestUsageAccount_AuthCheckGoldenJSON pins the serialized account shape.
// The quota-window fields stay exactly as they were before authCheck
// existed, and the verdict adds only harness, ok and checkedAt.
func TestUsageAccount_AuthCheckGoldenJSON(t *testing.T) {
	t.Parallel()
	d := 10080
	limits := MakeUsageLimits("2026-10-06T12:00:00Z", []UsageWindow{
		{ID: "primary", Kind: UsageWindowWeekly, Label: "Weekly", UsedPercent: 1, WindowDurationMins: &d},
	})
	account := func(check *UsageAuthCheck) UsageAccount {
		return UsageAccount{ID: "opaque-account-1", Provider: "codex", Plan: "promax", LimitID: "codex", Limits: limits, AuthCheck: check}
	}
	const base = `"id":"opaque-account-1","provider":"codex","plan":"promax","limitId":"codex",` +
		`"limits":{"checkedAt":"2026-10-06T12:00:00Z","windows":[{"id":"primary","kind":"weekly","label":"Weekly","usedPercent":1,"windowDurationMins":10080}]}`

	tests := []struct {
		name string
		in   UsageAccount
		want string
	}{
		{
			name: "ok verdict",
			in:   account(&UsageAuthCheck{Harness: UsageHarnessCodex, OK: true, CheckedAt: "2026-10-06T12:00:00Z"}),
			want: `{` + base + `,"authCheck":{"harness":"codex","ok":true,"checkedAt":"2026-10-06T12:00:00Z"}}`,
		},
		{
			name: "failed verdict",
			in:   account(&UsageAuthCheck{Harness: UsageHarnessCodex, OK: false, CheckedAt: "2026-10-06T12:05:00Z"}),
			want: `{` + base + `,"authCheck":{"harness":"codex","ok":false,"checkedAt":"2026-10-06T12:05:00Z"}}`,
		},
		{
			name: "no check attempted omits the key",
			in:   account(nil),
			want: `{` + base + `}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tt.want {
				t.Fatalf("account JSON =\n%s\nwant\n%s", raw, tt.want)
			}
		})
	}
}

// TestUsageAuthCheck_CarriesOnlyThreeFields guards against a field that
// could carry an address or credential joining the verdict.
func TestUsageAuthCheck_CarriesOnlyThreeFields(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(UsageAuthCheck{Harness: UsageHarnessClaude, OK: true, CheckedAt: "2026-10-06T12:00:00Z"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"checkedAt", "harness", "ok"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("authCheck keys = %v, want %v", keys, want)
	}
}
