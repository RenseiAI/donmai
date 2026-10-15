package seatstate

import "testing"

// TestDecideActivity_RankOrder pins the ladder: the highest-rank conclusive
// rung decides, and lower rungs never override it. Each row's winner is the
// first conclusive vote in rank order, whatever the input order.
func TestDecideActivity_RankOrder(t *testing.T) {
	cases := []struct {
		name     string
		evidence []ActivityEvidence
		want     Verdict
	}{
		{
			name:     "no evidence decides nothing",
			evidence: nil,
			want:     Verdict{Activity: ActivityUnknown, DecidedBy: RungNone},
		},
		{
			name: "harness self-report decides working alone",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungHarnessReport, ActivityWorking),
			},
			want: Verdict{Activity: ActivityWorking, DecidedBy: RungHarnessReport},
		},
		{
			name: "hooks decide idle at prompt alone",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungHooks, ActivityIdleAtPrompt),
			},
			want: Verdict{Activity: ActivityIdleAtPrompt, DecidedBy: RungHooks},
		},
		{
			name: "visible prompt decides idle at prompt alone",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungVisiblePrompt, ActivityIdleAtPrompt),
			},
			want: Verdict{Activity: ActivityIdleAtPrompt, DecidedBy: RungVisiblePrompt},
		},
		{
			name: "pty activity decides working alone",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungPTYActivity, ActivityWorking),
			},
			want: Verdict{Activity: ActivityWorking, DecidedBy: RungPTYActivity},
		},
		{
			name: "harness report beats hooks",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungHooks, ActivityIdleAtPrompt),
				ConclusiveVote(RungHarnessReport, ActivityWorking),
			},
			want: Verdict{Activity: ActivityWorking, DecidedBy: RungHarnessReport},
		},
		{
			name: "harness report beats visible prompt",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungVisiblePrompt, ActivityIdleAtPrompt),
				ConclusiveVote(RungHarnessReport, ActivityWorking),
			},
			want: Verdict{Activity: ActivityWorking, DecidedBy: RungHarnessReport},
		},
		{
			name: "harness report beats pty activity",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungPTYActivity, ActivityIdleAtPrompt),
				ConclusiveVote(RungHarnessReport, ActivityWorking),
			},
			want: Verdict{Activity: ActivityWorking, DecidedBy: RungHarnessReport},
		},
		{
			name: "hooks beat visible prompt",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungVisiblePrompt, ActivityWorking),
				ConclusiveVote(RungHooks, ActivityIdleAtPrompt),
			},
			want: Verdict{Activity: ActivityIdleAtPrompt, DecidedBy: RungHooks},
		},
		{
			name: "hooks beat pty activity",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungPTYActivity, ActivityWorking),
				ConclusiveVote(RungHooks, ActivityIdleAtPrompt),
			},
			want: Verdict{Activity: ActivityIdleAtPrompt, DecidedBy: RungHooks},
		},
		{
			name: "visible prompt beats pty activity",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungPTYActivity, ActivityWorking),
				ConclusiveVote(RungVisiblePrompt, ActivityIdleAtPrompt),
			},
			want: Verdict{Activity: ActivityIdleAtPrompt, DecidedBy: RungVisiblePrompt},
		},
		{
			name: "full ladder decides at the top",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungPTYActivity, ActivityIdleAtPrompt),
				ConclusiveVote(RungVisiblePrompt, ActivityIdleAtPrompt),
				ConclusiveVote(RungHooks, ActivityIdleAtPrompt),
				ConclusiveVote(RungHarnessReport, ActivityWorking),
			},
			want: Verdict{Activity: ActivityWorking, DecidedBy: RungHarnessReport},
		},
		{
			name: "input order never decides",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungPTYActivity, ActivityWorking),
				ConclusiveVote(RungHarnessReport, ActivityIdleAtPrompt),
			},
			want: Verdict{Activity: ActivityIdleAtPrompt, DecidedBy: RungHarnessReport},
		},
		{
			name: "first vote per rung wins on repeat rungs",
			evidence: []ActivityEvidence{
				ConclusiveVote(RungHooks, ActivityWorking),
				ConclusiveVote(RungHooks, ActivityIdleAtPrompt),
			},
			want: Verdict{Activity: ActivityWorking, DecidedBy: RungHooks},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideActivity(tc.evidence)
			if got != tc.want {
				t.Fatalf("DecideActivity() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDecideActivity_NeverGuesses pins the unknown-first-class rule: a probe
// that never ran, errored, or read ambiguously is inconclusive, and an
// inconclusive ladder reports unknown — naming the lowest rung that ran, not
// a guessed state.
func TestDecideActivity_NeverGuesses(t *testing.T) {
	cases := []struct {
		name     string
		evidence []ActivityEvidence
		want     Verdict
	}{
		{
			name: "probe that never ran is inconclusive",
			evidence: []ActivityEvidence{
				InconclusiveProbe(RungHarnessReport, false),
			},
			want: Verdict{Activity: ActivityUnknown, DecidedBy: RungHarnessReport},
		},
		{
			name:     "ambiguous reading is inconclusive",
			evidence: []ActivityEvidence{{}},
			want:     Verdict{Activity: ActivityUnknown, DecidedBy: RungNone},
		},
		{
			name: "observed but ambiguous reading falls through",
			evidence: []ActivityEvidence{
				{Rung: RungHarnessReport, Observed: true, Conclusive: false, Activity: ActivityWorking},
				ConclusiveVote(RungHooks, ActivityIdleAtPrompt),
			},
			want: Verdict{Activity: ActivityIdleAtPrompt, DecidedBy: RungHooks},
		},
		{
			name: "full ladder inconclusive reports unknown at the lowest rung",
			evidence: []ActivityEvidence{
				InconclusiveProbe(RungHarnessReport, true),
				InconclusiveProbe(RungHooks, true),
				InconclusiveProbe(RungVisiblePrompt, true),
				InconclusiveProbe(RungPTYActivity, true),
			},
			want: Verdict{Activity: ActivityUnknown, DecidedBy: RungPTYActivity},
		},
		{
			name: "partial ladder inconclusive names the lowest rung that ran",
			evidence: []ActivityEvidence{
				InconclusiveProbe(RungHarnessReport, true),
				InconclusiveProbe(RungHooks, false),
			},
			want: Verdict{Activity: ActivityUnknown, DecidedBy: RungHooks},
		},
		{
			name: "invalid rung name is not evidence",
			evidence: []ActivityEvidence{
				{Rung: "telemetry", Observed: true, Conclusive: true, Activity: ActivityWorking},
			},
			want: Verdict{Activity: ActivityUnknown, DecidedBy: RungNone},
		},
		{
			name: "none rung is not evidence",
			evidence: []ActivityEvidence{
				{Rung: RungNone, Observed: true, Conclusive: true, Activity: ActivityWorking},
			},
			want: Verdict{Activity: ActivityUnknown, DecidedBy: RungNone},
		},
		{
			name: "invalid activity vote is inconclusive",
			evidence: []ActivityEvidence{
				{Rung: RungHooks, Observed: true, Conclusive: true, Activity: "busy"},
			},
			want: Verdict{Activity: ActivityUnknown, DecidedBy: RungHooks},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideActivity(tc.evidence)
			if got != tc.want {
				t.Fatalf("DecideActivity() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSeatState_Valid pins the closed vocabularies: every axis must carry a
// known value, the rung must name a real rung, needs-input must be a
// consistent count plus reason, and the zero value is invalid on every axis.
func TestSeatState_Valid(t *testing.T) {
	valid := SeatState{
		Session:      SessionPresent,
		Activity:     ActivityWorking,
		DecidedBy:    string(RungHarnessReport),
		Resumability: ResumabilityResumable,
		NeedsInput:   NeedsInput{Count: 2, Reason: "approval"},
	}
	if !valid.Valid() {
		t.Fatalf("Valid() = false for %+v, want true", valid)
	}
	if !UnknownSeatState().Valid() {
		t.Fatalf("Valid() = false for UnknownSeatState(), want true")
	}

	cases := []struct {
		name   string
		mutate func(*SeatState)
	}{
		{"zero value", func(s *SeatState) { *s = SeatState{} }},
		{"bad session", func(s *SeatState) { s.Session = "running" }},
		{"empty session", func(s *SeatState) { s.Session = "" }},
		{"needs-input as state", func(s *SeatState) { s.Activity = "needs-input" }},
		{"empty activity", func(s *SeatState) { s.Activity = "" }},
		{"empty decided-by", func(s *SeatState) { s.DecidedBy = "" }},
		{"unknown rung", func(s *SeatState) { s.DecidedBy = "telemetry" }},
		{"bad resumability", func(s *SeatState) { s.Resumability = "resumable-later" }},
		{"empty resumability", func(s *SeatState) { s.Resumability = "" }},
		{"count without reason", func(s *SeatState) { s.NeedsInput = NeedsInput{Count: 1} }},
		{"reason without count", func(s *SeatState) { s.NeedsInput = NeedsInput{Reason: "approval"} }},
		{"negative count", func(s *SeatState) { s.NeedsInput = NeedsInput{Count: -1, Reason: "approval"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.mutate(&s)
			if s.Valid() {
				t.Fatalf("Valid() = true for %+v, want false", s)
			}
		})
	}
}

// TestSeatState_AxisVocabularies pins each axis's closed set, including the
// first-class unknown member.
func TestSeatState_AxisVocabularies(t *testing.T) {
	for _, s := range []SessionState{SessionPresent, SessionDetached, SessionExited, SessionAbsent, SessionUnknown} {
		if !s.Valid() {
			t.Errorf("SessionState(%q).Valid() = false, want true", s)
		}
	}
	for _, a := range []ActivityState{ActivityWorking, ActivityIdleAtPrompt, ActivityUnknown} {
		if !a.Valid() {
			t.Errorf("ActivityState(%q).Valid() = false, want true", a)
		}
	}
	for _, r := range []ResumabilityState{ResumabilityResumable, ResumabilityFreshOnly, ResumabilityUnknown} {
		if !r.Valid() {
			t.Errorf("ResumabilityState(%q).Valid() = false, want true", r)
		}
	}
	for _, bad := range []string{"", "running", "idle", "paused", "needs-input", "needs_input", "stalled", "fresh", "none"} {
		if SessionState(bad).Valid() {
			t.Errorf("SessionState(%q).Valid() = true, want false", bad)
		}
		if ActivityState(bad).Valid() && bad != "none" {
			t.Errorf("ActivityState(%q).Valid() = true, want false", bad)
		}
		if ResumabilityState(bad).Valid() {
			t.Errorf("ResumabilityState(%q).Valid() = true, want false", bad)
		}
	}
}
