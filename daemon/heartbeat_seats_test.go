package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon/seatstate"
)

// TestHeartbeatService_SeatsOmittedWhenUnconfigured pins the additive
// contract: a heartbeat without GetSeatStates carries no seat keys —
// byte-identical to the pre-seat-state beat.
func TestHeartbeatService_SeatsOmittedWhenUnconfigured(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var (
		mu  sync.Mutex
		raw map[string]json.RawMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(buf, &raw)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr_seat_omit", Hostname: "h",
		OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value",
		IntervalSeconds: 60,
		GetActiveCount:  func() int { return 1 },
		GetMaxCount:     func() int { return 4 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.sendOneResult(ctx); err != nil {
		t.Fatalf("sendOneResult: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if _, present := raw["seats"]; present {
		t.Errorf("body.seats present without GetSeatStates: %s", raw["seats"])
	}
	if _, present := raw["seatSummary"]; present {
		t.Errorf("body.seatSummary present without GetSeatStates: %s", raw["seatSummary"])
	}
	if got := hs.LastPayload(); got.Seats != nil || got.SeatSummary != nil {
		t.Errorf("LastPayload carries seats without GetSeatStates: %+v", got.Seats)
	}
}

// TestHeartbeatService_SeatsRoundTrip pins the per-seat projection: the
// outbound body carries every seat's three axes with its decided-by rung,
// plus the axis-count fold.
func TestHeartbeatService_SeatsRoundTrip(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	now := time.Date(2026, 10, 12, 7, 0, 0, 0, time.UTC)
	reports := []seatstate.SeatReport{
		seatstate.Report("sess-a", seatstate.ProbeInput{
			ProcessAlive: seatstate.BoolPtr(true), SupervisorBound: seatstate.BoolPtr(true),
			RegistryReadable: true, SeatKnown: true,
			Activity:   []seatstate.ActivityEvidence{seatstate.ConclusiveVote(seatstate.RungHarnessReport, seatstate.ActivityWorking)},
			NeedsInput: seatstate.NeedsInput{Count: 1, Reason: "approval"},
			ObservedAt: now,
		}),
		seatstate.Report("sess-b", seatstate.ProbeInput{
			ProcessAlive: seatstate.BoolPtr(true), SupervisorBound: seatstate.BoolPtr(false),
			RegistryReadable: true, SeatKnown: true,
			Activity:   []seatstate.ActivityEvidence{seatstate.ConclusiveVote(seatstate.RungVisiblePrompt, seatstate.ActivityIdleAtPrompt)},
			ObservedAt: now,
		}),
	}

	var (
		mu  sync.Mutex
		raw map[string]json.RawMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(buf, &raw)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr_seat", Hostname: "h",
		OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value",
		IntervalSeconds: 60,
		GetActiveCount:  func() int { return 2 },
		GetMaxCount:     func() int { return 4 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		GetSeatStates:   func() []seatstate.SeatReport { return reports },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.sendOneResult(ctx); err != nil {
		t.Fatalf("sendOneResult: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	rawSeats, present := raw["seats"]
	if !present {
		t.Fatal("body.seats missing with GetSeatStates configured")
	}
	var seats []seatstate.SeatReport
	if err := json.Unmarshal(rawSeats, &seats); err != nil {
		t.Fatalf("body.seats not decodable: %v (raw %s)", err, rawSeats)
	}
	if len(seats) != 2 {
		t.Fatalf("body.seats has %d entries, want 2", len(seats))
	}
	if seats[0].SessionID != "sess-a" || seats[0].State.Session != seatstate.SessionPresent ||
		seats[0].State.Activity != seatstate.ActivityWorking ||
		seats[0].State.DecidedBy != string(seatstate.RungHarnessReport) {
		t.Errorf("seats[0] = %+v, want present/working/harness_report", seats[0].State)
	}
	if seats[0].State.NeedsInput != (seatstate.NeedsInput{Count: 1, Reason: "approval"}) {
		t.Errorf("seats[0].needsInput = %+v, want count 1 reason approval", seats[0].State.NeedsInput)
	}
	if seats[1].SessionID != "sess-b" || seats[1].State.Session != seatstate.SessionDetached ||
		seats[1].State.Activity != seatstate.ActivityIdleAtPrompt ||
		seats[1].State.DecidedBy != string(seatstate.RungVisiblePrompt) {
		t.Errorf("seats[1] = %+v, want detached/idle_at_prompt/visible_prompt", seats[1].State)
	}

	rawSummary, present := raw["seatSummary"]
	if !present {
		t.Fatal("body.seatSummary missing with GetSeatStates configured")
	}
	var summary seatstate.Summary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("body.seatSummary not decodable: %v (raw %s)", err, rawSummary)
	}
	if summary.Total != 2 || summary.Session.Present != 1 || summary.Session.Detached != 1 {
		t.Errorf("seatSummary.session = %+v, want present 1 detached 1 total 2", summary.Session)
	}
	if summary.Activity.Working != 1 || summary.Activity.IdleAtPrompt != 1 {
		t.Errorf("seatSummary.activity = %+v, want working 1 idle 1", summary.Activity)
	}
	if summary.NeedsInput != 1 {
		t.Errorf("seatSummary.needsInput = %d, want 1", summary.NeedsInput)
	}

	got := hs.LastPayload()
	if len(got.Seats) != 2 || got.SeatSummary == nil || got.SeatSummary.Total != 2 {
		t.Errorf("LastPayload seats = %d summary = %+v, want 2 seats and total 2", len(got.Seats), got.SeatSummary)
	}
}

// TestHeartbeatService_SeatsEmptyReportsZero pins the explicit zero-seat
// beat: an empty return omits the seats slice per the omitempty contract
// but still reports the zero summary — distinguishable from the omitted
// keys above.
func TestHeartbeatService_SeatsEmptyReportsZero(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var (
		mu  sync.Mutex
		raw map[string]json.RawMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(buf, &raw)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr_seat_empty", Hostname: "h",
		OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value",
		IntervalSeconds: 60,
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     func() int { return 4 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		GetSeatStates:   func() []seatstate.SeatReport { return []seatstate.SeatReport{} },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.sendOneResult(ctx); err != nil {
		t.Fatalf("sendOneResult: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if rawSeats, present := raw["seats"]; present {
		t.Errorf("body.seats present for empty return: %s (omitempty omits empty slices)", rawSeats)
	}
	rawSummary, present := raw["seatSummary"]
	if !present {
		t.Fatal("body.seatSummary missing for explicit empty return")
	}
	var summary seatstate.Summary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("body.seatSummary not decodable: %v (raw %s)", err, rawSummary)
	}
	if summary.Total != 0 {
		t.Errorf("seatSummary.total = %d, want 0", summary.Total)
	}
}
