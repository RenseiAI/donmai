package codex

// Behavioural tests for the codex quota-window mappers, ported from the
// upstream codex usage-limits suite (see LICENSE-t3code).

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const rateLimitsTestCheckedAt = "2026-07-18T10:00:00.000Z"

func strPtr(s string) *string { return &s }

func floatPtr(f float64) *float64 { return &f }

func intPtr(n int) *int { return &n }

func TestRateLimitsToLimits_SessionAndWeekly(t *testing.T) {
	t.Parallel()
	got := RateLimitsToLimits(&RateLimitSnapshot{
		PlanType:  strPtr("plus"),
		Primary:   &RateLimitWindow{UsedPercent: 12, ResetsAt: floatPtr(1784000000), WindowDurationMins: intPtr(300)},
		Secondary: &RateLimitWindow{UsedPercent: 47, ResetsAt: floatPtr(1784500000), WindowDurationMins: intPtr(10080)},
	}, nil, nil, rateLimitsTestCheckedAt)
	if len(got.Windows) != 2 {
		t.Fatalf("got %d windows, want 2: %+v", len(got.Windows), got.Windows)
	}
	primary := got.Windows[0]
	if primary.ID != "primary" || primary.Kind != "session" || primary.Label != "Session" || primary.UsedPercent != 12 {
		t.Errorf("primary = %+v, want session/12", primary)
	}
	if primary.WindowDurationMins == nil || *primary.WindowDurationMins != 300 {
		t.Errorf("primary duration = %v, want 300", primary.WindowDurationMins)
	}
	if primary.ResetsAt == "" {
		t.Error("primary carries no reset time")
	}
	secondary := got.Windows[1]
	if secondary.ID != "secondary" || secondary.Kind != "weekly" || secondary.UsedPercent != 47 {
		t.Errorf("secondary = %+v, want weekly/47", secondary)
	}
	if secondary.WindowDurationMins == nil || *secondary.WindowDurationMins != 10080 {
		t.Errorf("secondary duration = %v, want 10080", secondary.WindowDurationMins)
	}
}

// A duration-less primary on a monthly plan must map to monthly — this
// goes RED if the 5-hour session fallback is hardcoded.
func TestRateLimitsToLimits_MonthlyPlanDurationlessPrimary(t *testing.T) {
	t.Parallel()
	for _, plan := range []string{"free", "go"} {
		got := RateLimitsToLimits(&RateLimitSnapshot{
			PlanType: strPtr(plan),
			Primary:  &RateLimitWindow{UsedPercent: 80},
		}, nil, nil, rateLimitsTestCheckedAt)
		if len(got.Windows) != 1 {
			t.Fatalf("plan %s: got %d windows, want 1", plan, len(got.Windows))
		}
		w := got.Windows[0]
		if w.Kind != "monthly" || w.Label != "Monthly" {
			t.Errorf("plan %s: kind = %q/%q, want monthly/Monthly", plan, w.Kind, w.Label)
		}
		if w.WindowDurationMins == nil || *w.WindowDurationMins != 43200 {
			t.Errorf("plan %s: duration = %v, want 43200", plan, w.WindowDurationMins)
		}
	}
}

// The recorded studio read: a promax primary with a 10,080-minute
// window classifies as weekly at about 1% used.
func TestRateLimitsToLimits_PromaxWeeklyPrimary(t *testing.T) {
	t.Parallel()
	got := RateLimitsToLimits(&RateLimitSnapshot{
		LimitID:   strPtr("codex"),
		PlanType:  strPtr("promax"),
		Primary:   &RateLimitWindow{UsedPercent: 1, ResetsAt: floatPtr(1791646860), WindowDurationMins: intPtr(10080)},
		Secondary: &RateLimitWindow{UsedPercent: 1, ResetsAt: floatPtr(1791646860), WindowDurationMins: intPtr(10080)},
	}, nil, nil, "2026-10-06T12:00:00.000Z")
	if len(got.Windows) != 2 {
		t.Fatalf("got %d windows, want 2", len(got.Windows))
	}
	primary := got.Windows[0]
	if primary.ID != "primary" || primary.Kind != "weekly" {
		t.Errorf("primary = %+v, want id primary kind weekly", primary)
	}
	if primary.WindowDurationMins == nil || *primary.WindowDurationMins != 10080 {
		t.Errorf("primary duration = %v, want 10080", primary.WindowDurationMins)
	}
	if primary.UsedPercent != 1 {
		t.Errorf("UsedPercent = %v, want 1", primary.UsedPercent)
	}
	if !strings.HasPrefix(primary.ResetsAt, "2026-10-10") {
		t.Errorf("ResetsAt = %q, want the 2026-10-10 reset", primary.ResetsAt)
	}
}

func TestRateLimitsToLimits_SelectsMainAllowance(t *testing.T) {
	t.Parallel()
	spark := &RateLimitSnapshot{
		LimitID:   strPtr("spark"),
		Primary:   &RateLimitWindow{UsedPercent: 0, WindowDurationMins: intPtr(300)},
		Secondary: &RateLimitWindow{UsedPercent: 90, WindowDurationMins: intPtr(10080)},
	}
	got := RateLimitsToLimits(spark, map[string]*RateLimitSnapshot{
		"spark": spark,
		"codex": {Secondary: &RateLimitWindow{UsedPercent: 42, WindowDurationMins: intPtr(10080)}},
	}, nil, rateLimitsTestCheckedAt)
	if len(got.Windows) != 1 || got.Windows[0].ID != "secondary" || got.Windows[0].UsedPercent != 42 {
		t.Errorf("windows = %+v, want the main secondary at 42", got.Windows)
	}
	legacy := RateLimitsToLimits(&RateLimitSnapshot{
		LimitID:   strPtr("spark"),
		Secondary: &RateLimitWindow{UsedPercent: 90},
	}, nil, nil, rateLimitsTestCheckedAt)
	if len(legacy.Windows) != 0 {
		t.Errorf("model-specific legacy snapshot produced %+v, want no rows", legacy.Windows)
	}
}

func TestRateLimitsToUpdate_SparkNeverOverwrites(t *testing.T) {
	t.Parallel()
	spark := &RateLimitSnapshot{
		LimitID:   strPtr("spark"),
		Primary:   &RateLimitWindow{UsedPercent: 0, WindowDurationMins: intPtr(300)},
		Secondary: &RateLimitWindow{UsedPercent: 90, WindowDurationMins: intPtr(10080)},
	}
	if got := RateLimitsToUpdate(spark, rateLimitsTestCheckedAt); got != nil {
		t.Errorf("Spark update produced %+v, want nil", got)
	}
	main := RateLimitsToUpdate(&RateLimitSnapshot{
		LimitID:   strPtr("codex"),
		Secondary: &RateLimitWindow{UsedPercent: 42, WindowDurationMins: intPtr(10080)},
	}, rateLimitsTestCheckedAt)
	if main == nil || len(main.Windows) != 1 || main.Windows[0].UsedPercent != 42 {
		t.Errorf("main update = %+v, want the secondary at 42", main)
	}
	if got := RateLimitsToUpdate(&RateLimitSnapshot{PlanType: strPtr("plus")}, rateLimitsTestCheckedAt); got != nil {
		t.Errorf("windowless update produced %+v, want nil", got)
	}
}

func TestMergeRateLimits_PartialUpdateKeepsWindows(t *testing.T) {
	t.Parallel()
	merged := MergeRateLimits(&RateLimitSnapshot{
		LimitID:  strPtr("codex"),
		PlanType: strPtr("business"),
		Primary:  &RateLimitWindow{UsedPercent: 100, ResetsAt: floatPtr(1800000000), WindowDurationMins: intPtr(300)},
	}, &RateLimitSnapshot{RateLimitReachedType: strPtr("rate_limit_reached")})
	if merged == nil || merged.Primary == nil || merged.Primary.UsedPercent != 100 {
		t.Fatalf("merged = %+v, want the primary window kept", merged)
	}
	if merged.RateLimitReachedType == nil || *merged.RateLimitReachedType != "rate_limit_reached" {
		t.Errorf("merged reached type = %v, want the update applied", merged.RateLimitReachedType)
	}
	main := &RateLimitSnapshot{
		LimitID: strPtr("codex"),
		Primary: &RateLimitWindow{UsedPercent: 100, ResetsAt: floatPtr(1800000000), WindowDurationMins: intPtr(300)},
	}
	if got := MergeRateLimits(main, &RateLimitSnapshot{
		LimitID: strPtr("spark"),
		Primary: &RateLimitWindow{UsedPercent: 3, WindowDurationMins: intPtr(300)},
	}); got != main {
		t.Error("model-specific snapshot replaced the main allowance; want it kept")
	}
}

func TestResetCreditsToContract(t *testing.T) {
	t.Parallel()
	got := ResetCreditsToContract(&ResetCreditsSummary{
		AvailableCount: 2,
		Credits: []ResetCreditEntry{
			{Status: "available", ExpiresAt: floatPtr(1784500000)},
			{Status: "redeemed", ExpiresAt: floatPtr(1700000000)},
			{Status: "available", ExpiresAt: floatPtr(1784000000)},
		},
	})
	if got == nil || got.AvailableCount != 2 {
		t.Fatalf("credits = %+v, want count 2", got)
	}
	if got.NextExpiresAt == "" {
		t.Error("credits carry no expiry; want the soonest available credit")
	}
	if plain := ResetCreditsToContract(&ResetCreditsSummary{AvailableCount: 0}); plain == nil || plain.AvailableCount != 0 {
		t.Errorf("empty credits = %+v, want count 0", plain)
	}
	if ResetCreditsToContract(nil) != nil {
		t.Error("nil summary should map to nil")
	}
	limits := RateLimitsToLimits(
		&RateLimitSnapshot{Primary: &RateLimitWindow{UsedPercent: 5, WindowDurationMins: intPtr(300)}},
		nil, &ResetCreditsSummary{AvailableCount: 1}, rateLimitsTestCheckedAt)
	if limits.ResetCredits == nil || limits.ResetCredits.AvailableCount != 1 {
		t.Errorf("limits credits = %+v, want count 1 riding along", limits.ResetCredits)
	}
}

func TestRateLimitsFailureMessage(t *testing.T) {
	t.Parallel()
	rpc := RateLimitsFailureMessage(&RPCError{Method: "account/rateLimits/read", Code: -32603, Message: "boom"})
	if !strings.Contains(rpc, "-32603") || strings.Contains(rpc, "boom") {
		t.Errorf("RPC failure message = %q, want the code and nothing else", rpc)
	}
	if got := RateLimitsFailureMessage(nil); got == "" {
		t.Error("nil error should still render a message")
	}
}

func TestRateLimitsProbe_AllowInterval(t *testing.T) {
	t.Parallel()
	p := NewRateLimitsProbe()
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if !p.Allow(base) {
		t.Fatal("first probe must be allowed")
	}
	p.mark(base)
	if p.Allow(base.Add(4 * time.Minute)) {
		t.Error("probe inside the 5-minute interval must be refused")
	}
	if !p.Allow(base.Add(5 * time.Minute)) {
		t.Error("probe at the 5-minute mark must be allowed")
	}
}

// A failed probe keeps the last good windows: the probe itself
// reports probeFailed, and the shared resolve rule keeps publishing.
func TestRateLimitsProbe_FailedProbeKeepsLastWindows(t *testing.T) {
	t.Parallel()
	p := NewRateLimitsProbe()
	probed := p.ReadProbe(context.Background(), nil)
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableProbeFailed {
		t.Fatalf("nil-client probe = %+v, want probeFailed", probed.Unavailable)
	}
	published := RateLimitsToLimits(&RateLimitSnapshot{
		Primary: &RateLimitWindow{UsedPercent: 12, WindowDurationMins: intPtr(300)},
	}, nil, nil, rateLimitsTestCheckedAt)
	if got := agent.ResolveUsageLimitsAfterProbe(&published, &probed); got != &published {
		t.Errorf("resolved = %+v, want the last good windows kept", got)
	}
}

func TestRateLimitsProbe_ReadAgainstFakeServer(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	client := NewClient(fake.clientWriter, fake.serverReader)
	defer client.Stop(nil)

	go func() {
		req := fake.readClientLine(t)
		if req == nil {
			return
		}
		if req["method"] != "account/rateLimits/read" {
			return
		}
		id, _ := req["id"].(float64)
		fake.writeServerLine(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      int(id),
			"result": map[string]any{
				"rateLimits": map[string]any{
					"limitId":  "codex",
					"planType": "promax",
					"primary": map[string]any{
						"usedPercent":        1,
						"resetsAt":           1791562860,
						"windowDurationMins": 10080,
					},
				},
			},
		})
	}()

	p := NewRateLimitsProbe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	probed := p.ReadProbe(ctx, client)
	if probed.Unavailable != nil {
		t.Fatalf("probe = %+v, want windows", probed.Unavailable)
	}
	if len(probed.Windows) != 1 || probed.Windows[0].Kind != agent.UsageWindowWeekly {
		t.Fatalf("windows = %+v, want one weekly row", probed.Windows)
	}
}

func TestSubscribeRateLimits_PartialUpdateMerges(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	client := NewClient(fake.clientWriter, fake.serverReader)
	defer client.Stop(nil)

	updated := make(chan struct{}, 4)
	var mu sync.Mutex
	var last agent.UsageLimits
	unsub := SubscribeRateLimits(client, func(limits agent.UsageLimits) {
		mu.Lock()
		last = limits
		mu.Unlock()
		updated <- struct{}{}
	})
	defer unsub()

	emitRateLimits := func(snapshot *RateLimitSnapshot) {
		t.Helper()
		params, err := json.Marshal(rateLimitsUpdatedParams{RateLimits: snapshot})
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		fake.writeServerLine(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "account/rateLimits/updated",
			"params":  json.RawMessage(params),
		})
	}

	// Full snapshot first: both rows publish.
	emitRateLimits(&RateLimitSnapshot{
		PlanType:  strPtr("plus"),
		Primary:   &RateLimitWindow{UsedPercent: 12, WindowDurationMins: intPtr(300)},
		Secondary: &RateLimitWindow{UsedPercent: 47, WindowDurationMins: intPtr(10080)},
	})
	select {
	case <-updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first quota update")
	}

	// Partial update naming only the secondary: the primary survives.
	emitRateLimits(&RateLimitSnapshot{
		Secondary: &RateLimitWindow{UsedPercent: 51, WindowDurationMins: intPtr(10080)},
	})
	select {
	case <-updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the partial quota update")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(last.Windows) != 2 {
		t.Fatalf("windows = %+v, want both rows after a partial update", last.Windows)
	}
	byID := map[string]agent.UsageWindow{}
	for _, w := range last.Windows {
		byID[w.ID] = w
	}
	if byID["primary"].UsedPercent != 12 {
		t.Errorf("primary = %+v, want the earlier 12 kept", byID["primary"])
	}
	if byID["secondary"].UsedPercent != 51 {
		t.Errorf("secondary = %+v, want the updated 51", byID["secondary"])
	}

	// A model-specific notification never touches the rows.
	emitRateLimits(&RateLimitSnapshot{
		LimitID:   strPtr("spark"),
		Secondary: &RateLimitWindow{UsedPercent: 90, WindowDurationMins: intPtr(10080)},
	})
	select {
	case <-updated:
		t.Fatal("model-specific notification must not publish")
	case <-time.After(200 * time.Millisecond):
	}
}
