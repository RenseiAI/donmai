package daemon

import (
	"testing"

	"github.com/RenseiAI/donmai/daemon/seatstate"
)

// TestSeatStates_SpawnerSeatsByMode pins the collector over live spawner
// seats: every accepted seat is present with its binding intact, and the
// activity vote follows the admitted run mode — interactive and interview
// seats vote idle_at_prompt at the visible-prompt rung, headless seats
// offer no evidence and report unknown.
func TestSeatStates_SpawnerSeatsByMode(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 4,
		WorkerCommand:         []string{"sleep", "30"},
	})
	specs := []SessionSpec{
		{SessionID: "headless", Repository: "github.com/a/b"},
		{SessionID: "interactive", Repository: "github.com/a/b", Mode: interactiveRunMode},
		{SessionID: "interview", Repository: "github.com/a/b", Mode: "interview"},
	}
	for _, spec := range specs {
		if _, err := s.AcceptWork(spec); err != nil {
			t.Fatalf("accept %s: %v", spec.SessionID, err)
		}
	}
	t.Cleanup(func() {
		for _, spec := range specs {
			_ = s.ForceKillSession(spec.SessionID)
		}
	})

	d := &Daemon{spawner: s}
	reports := d.seatStates()
	byID := make(map[string]seatstate.SeatReport, len(reports))
	for _, r := range reports {
		byID[r.SessionID] = r
	}
	if len(byID) != 3 {
		t.Fatalf("seatStates() = %d reports, want 3", len(byID))
	}

	headless := byID["headless"]
	if headless.State.Session != seatstate.SessionPresent {
		t.Errorf("headless session = %q, want present", headless.State.Session)
	}
	if headless.State.Activity != seatstate.ActivityUnknown {
		t.Errorf("headless activity = %q, want unknown (no prompt surface to read)", headless.State.Activity)
	}
	if headless.State.DecidedBy != string(seatstate.RungNone) {
		t.Errorf("headless decidedBy = %q, want none", headless.State.DecidedBy)
	}
	if headless.State.Resumability != seatstate.ResumabilityUnknown {
		t.Errorf("headless resumability = %q, want unknown", headless.State.Resumability)
	}

	for _, id := range []string{"interactive", "interview"} {
		got := byID[id]
		if got.State.Session != seatstate.SessionPresent {
			t.Errorf("%s session = %q, want present", id, got.State.Session)
		}
		if got.State.Activity != seatstate.ActivityIdleAtPrompt {
			t.Errorf("%s activity = %q, want idle_at_prompt", id, got.State.Activity)
		}
		if got.State.DecidedBy != string(seatstate.RungVisiblePrompt) {
			t.Errorf("%s decidedBy = %q, want visible_prompt", id, got.State.DecidedBy)
		}
	}

	for _, r := range reports {
		if !r.State.Valid() {
			t.Errorf("report %+v is not valid", r.State)
		}
		if r.ObservedAt == "" {
			t.Errorf("report %s carries no observed-at stamp", r.SessionID)
		}
	}
}

// TestSeatStates_EmptyDaemon pins the empty host: no spawner, no shims,
// no reports — and a zero summary, not a nil one.
func TestSeatStates_EmptyDaemon(t *testing.T) {
	d := &Daemon{}
	if reports := d.seatStates(); len(reports) != 0 {
		t.Fatalf("seatStates() = %d reports, want 0", len(reports))
	}
	sum := d.seatStateSummary()
	if sum.Total != 0 || sum.NeedsInput != 0 {
		t.Fatalf("summary = %+v, want zero", sum)
	}
}

// TestSpawnerSeatActivityForMode pins the mode-to-evidence map directly:
// interactive and interview vote at the visible-prompt rung, everything
// else (including empty and unknown modes) offers nothing.
func TestSpawnerSeatActivityForMode(t *testing.T) {
	for _, mode := range []string{interactiveRunMode, "interview"} {
		ev := spawnerSeatActivityForMode(mode)
		if len(ev) != 1 {
			t.Fatalf("mode %q: %d evidence votes, want 1", mode, len(ev))
		}
		if ev[0].Rung != seatstate.RungVisiblePrompt || ev[0].Activity != seatstate.ActivityIdleAtPrompt {
			t.Fatalf("mode %q: vote = %+v, want visible_prompt/idle_at_prompt", mode, ev[0])
		}
	}
	for _, mode := range []string{"", "headless", "interactive-preview", "INTERACTIVE"} {
		if ev := spawnerSeatActivityForMode(mode); len(ev) != 0 {
			t.Fatalf("mode %q: %d evidence votes, want 0 (never guess)", mode, len(ev))
		}
	}
}

// TestActiveSeats_CarriesAdmittedMode pins the spawner snapshot: the mode
// beside each handle is the admitted spec mode, sampled under the same
// lock as the handle.
func TestActiveSeats_CarriesAdmittedMode(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"sleep", "30"},
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: "plain", Repository: "github.com/a/b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptWork(SessionSpec{SessionID: "pty", Repository: "github.com/a/b", Mode: interactiveRunMode}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.ForceKillSession("plain")
		_ = s.ForceKillSession("pty")
	})

	seats := s.ActiveSeats()
	byID := make(map[string]ActiveSeat, len(seats))
	for _, seat := range seats {
		byID[seat.Handle.SessionID] = seat
	}
	if byID["plain"].Mode != "" {
		t.Errorf("plain mode = %q, want empty", byID["plain"].Mode)
	}
	if byID["pty"].Mode != interactiveRunMode {
		t.Errorf("pty mode = %q, want %q", byID["pty"].Mode, interactiveRunMode)
	}
}
