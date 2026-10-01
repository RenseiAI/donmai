package shimwire

import "testing"

// TestPhaseHarnessLiveIsTheRunningOrOrphanedCheck pins the single source of
// truth for "does this shim phase own a live harness". Running (a controller
// is attached) and orphaned (the controller is gone and the bounded orphan
// deadline is counting down) both still hold the harness; starting owns none
// yet and exited owns only the immutable terminal observation. An unassigned
// phase owns nothing either — the quarantine rule, not a guess.
func TestPhaseHarnessLiveIsTheRunningOrOrphanedCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		phase Phase
		live  bool
	}{
		{PhaseStarting, false},
		{PhaseRunning, true},
		{PhaseOrphaned, true},
		{PhaseExited, false},
		{Phase("recovering"), false},
		{Phase(""), false},
	}
	for _, tc := range cases {
		if got := tc.phase.HarnessLive(); got != tc.live {
			t.Errorf("Phase(%q).HarnessLive() = %v, want %v", tc.phase, got, tc.live)
		}
	}
}
