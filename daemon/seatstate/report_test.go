package seatstate

import (
	"encoding/json"
	"testing"
	"time"
)

// TestReport_SessionAxis pins the presence ladder: terminal proof wins, then
// the live identity plus binding, then the registry record — and every
// inconclusive shape reports unknown.
func TestReport_SessionAxis(t *testing.T) {
	alive, dead := true, false
	bound, unbound := true, false
	cases := []struct {
		name  string
		input ProbeInput
		want  SessionState
	}{
		{"live bound identity is present", ProbeInput{ProcessAlive: &alive, SupervisorBound: &bound}, SessionPresent},
		{"live unbound identity is detached", ProbeInput{ProcessAlive: &alive, SupervisorBound: &unbound}, SessionDetached},
		{"dead identity is exited", ProbeInput{ProcessAlive: &dead}, SessionExited},
		{"dead identity with binding read is still exited", ProbeInput{ProcessAlive: &dead, SupervisorBound: &bound}, SessionExited},
		{"terminal proof is exited without a liveness probe", ProbeInput{TerminalProof: true}, SessionExited},
		{"terminal proof wins over a live read", ProbeInput{ProcessAlive: &alive, SupervisorBound: &bound, TerminalProof: true}, SessionExited},
		{"readable registry without a record is absent", ProbeInput{RegistryReadable: true}, SessionAbsent},
		{"live identity without a binding read is unknown", ProbeInput{ProcessAlive: &alive}, SessionUnknown},
		{"unreadable registry with nothing else is unknown", ProbeInput{}, SessionUnknown},
		{"known seat with no probe detail is unknown", ProbeInput{RegistryReadable: true, SeatKnown: true}, SessionUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Report("sess", tc.input)
			if got.SessionID != "sess" {
				t.Fatalf("SessionID = %q, want sess", got.SessionID)
			}
			if got.State.Session != tc.want {
				t.Fatalf("Session = %q, want %q", got.State.Session, tc.want)
			}
			if !got.State.Valid() {
				t.Fatalf("report %+v is not valid", got.State)
			}
		})
	}
}

// TestReport_ResumabilityAxis pins the continuation ladder: a verified
// artifact is resumable, verified absence is fresh_only, and anything else
// is unknown — absence of evidence never selects a member.
func TestReport_ResumabilityAxis(t *testing.T) {
	readable, unreadable := true, false
	cases := []struct {
		name  string
		input ProbeInput
		want  ResumabilityState
	}{
		{"verified artifact is resumable", ProbeInput{ResumeArtifactReadable: &readable}, ResumabilityResumable},
		{"artifact wins over absence claim", ProbeInput{ResumeArtifactReadable: &readable, NoResumeArtifact: true}, ResumabilityResumable},
		{"unreadable artifact with positive absence is fresh_only", ProbeInput{ResumeArtifactReadable: &unreadable, NoResumeArtifact: true}, ResumabilityFreshOnly},
		{"positive absence alone is fresh_only", ProbeInput{NoResumeArtifact: true}, ResumabilityFreshOnly},
		{"unreadable artifact without absence proof is unknown", ProbeInput{ResumeArtifactReadable: &unreadable}, ResumabilityUnknown},
		{"no probe at all is unknown", ProbeInput{}, ResumabilityUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Report("sess", tc.input)
			if got.State.Resumability != tc.want {
				t.Fatalf("Resumability = %q, want %q", got.State.Resumability, tc.want)
			}
		})
	}
}

// TestReport_ActivityAxisRunsTheArbiter pins that the report carries the
// arbiter's verdict verbatim, including the decided-by rung.
func TestReport_ActivityAxisRunsTheArbiter(t *testing.T) {
	got := Report("sess", ProbeInput{
		Activity: []ActivityEvidence{
			ConclusiveVote(RungPTYActivity, ActivityWorking),
			ConclusiveVote(RungHooks, ActivityIdleAtPrompt),
		},
	})
	if got.State.Activity != ActivityIdleAtPrompt {
		t.Fatalf("Activity = %q, want idle_at_prompt", got.State.Activity)
	}
	if got.State.DecidedBy != string(RungHooks) {
		t.Fatalf("DecidedBy = %q, want hooks", got.State.DecidedBy)
	}

	unknown := Report("sess", ProbeInput{})
	if unknown.State.Activity != ActivityUnknown {
		t.Fatalf("Activity = %q, want unknown", unknown.State.Activity)
	}
	if unknown.State.DecidedBy != string(RungNone) {
		t.Fatalf("DecidedBy = %q, want none", unknown.State.DecidedBy)
	}
}

// TestReport_NeedsInputIsCountPlusReason pins that needs-input rides the
// report orthogonally: it never changes an axis, and the zero value stays
// zero.
func TestReport_NeedsInputIsCountPlusReason(t *testing.T) {
	alive, bound := true, true
	got := Report("sess", ProbeInput{
		ProcessAlive: &alive, SupervisorBound: &bound,
		Activity:   []ActivityEvidence{ConclusiveVote(RungHarnessReport, ActivityWorking)},
		NeedsInput: NeedsInput{Count: 3, Reason: "approval"},
	})
	if got.State.Activity != ActivityWorking {
		t.Fatalf("Activity = %q, want working (input must not move the axis)", got.State.Activity)
	}
	if got.State.NeedsInput != (NeedsInput{Count: 3, Reason: "approval"}) {
		t.Fatalf("NeedsInput = %+v, want count 3 reason approval", got.State.NeedsInput)
	}

	zero := Report("sess", ProbeInput{})
	if zero.State.NeedsInput != (NeedsInput{}) {
		t.Fatalf("NeedsInput = %+v, want zero", zero.State.NeedsInput)
	}
}

// TestReport_ObservedAtStampsRFC3339 pins the probe instant shape.
func TestReport_ObservedAtStampsRFC3339(t *testing.T) {
	when := time.Date(2026, 10, 12, 7, 0, 0, 0, time.UTC)
	got := Report("sess", ProbeInput{ObservedAt: when})
	if got.ObservedAt != "2026-10-12T07:00:00Z" {
		t.Fatalf("ObservedAt = %q, want 2026-10-12T07:00:00Z", got.ObservedAt)
	}
	if Report("sess", ProbeInput{}).ObservedAt != "" {
		t.Fatal("unstamped report carries an observed-at")
	}
}

// TestSummarize_CountsEverySeat pins the heartbeat fold: every seat lands
// in exactly one bucket per axis, unknowns are counted, and needs-input
// sums.
func TestSummarize_CountsEverySeat(t *testing.T) {
	reports := []SeatReport{
		{SessionID: "a", State: SeatState{Session: SessionPresent, Activity: ActivityWorking, DecidedBy: "harness_report", Resumability: ResumabilityResumable, NeedsInput: NeedsInput{Count: 2, Reason: "approval"}}},
		{SessionID: "b", State: SeatState{Session: SessionDetached, Activity: ActivityIdleAtPrompt, DecidedBy: "hooks", Resumability: ResumabilityFreshOnly}},
		{SessionID: "c", State: SeatState{Session: SessionExited, Activity: ActivityUnknown, DecidedBy: "pty_activity", Resumability: ResumabilityUnknown}},
		{SessionID: "d", State: SeatState{Session: SessionAbsent, Activity: ActivityUnknown, DecidedBy: "none", Resumability: ResumabilityUnknown, NeedsInput: NeedsInput{Count: 1, Reason: "question"}}},
		{SessionID: "e", State: UnknownSeatState()},
	}
	got := Summarize(reports)
	if got.Total != 5 {
		t.Fatalf("Total = %d, want 5", got.Total)
	}
	if got.Session != (SessionSummary{Present: 1, Detached: 1, Exited: 1, Absent: 1, Unknown: 1}) {
		t.Fatalf("Session = %+v", got.Session)
	}
	if got.Activity != (ActivitySummary{Working: 1, IdleAtPrompt: 1, Unknown: 3}) {
		t.Fatalf("Activity = %+v", got.Activity)
	}
	if got.Resumability != (ResumeSummary{Resumable: 1, FreshOnly: 1, Unknown: 3}) {
		t.Fatalf("Resumability = %+v", got.Resumability)
	}
	if got.NeedsInput != 3 {
		t.Fatalf("NeedsInput = %d, want 3", got.NeedsInput)
	}

	empty := Summarize(nil)
	if empty.Total != 0 || empty.NeedsInput != 0 {
		t.Fatalf("empty summarize = %+v, want zero", empty)
	}
	if empty.Session != (SessionSummary{}) || empty.Activity != (ActivitySummary{}) || empty.Resumability != (ResumeSummary{}) {
		t.Fatalf("empty summarize buckets = %+v %+v %+v, want zero", empty.Session, empty.Activity, empty.Resumability)
	}
}

// TestReport_DeadOrAbsentSeatReportsNoActivity pins the staleness rule: a
// seat whose process is proved gone (dead identity or terminal proof) or
// that was never on this host carries no current activity, whatever the
// evidence says — the vote describes an earlier incarnation. A live seat,
// or one whose liveness is merely unknown, keeps its arbitrated vote.
func TestReport_DeadOrAbsentSeatReportsNoActivity(t *testing.T) {
	alive, dead := true, false
	bound, unbound := true, false
	idle := []ActivityEvidence{ConclusiveVote(RungVisiblePrompt, ActivityIdleAtPrompt)}
	working := []ActivityEvidence{ConclusiveVote(RungHarnessReport, ActivityWorking)}
	cases := []struct {
		name         string
		input        ProbeInput
		wantActivity ActivityState
		wantRung     EvidenceRung
	}{
		{"dead identity drops a prompt vote", ProbeInput{ProcessAlive: &dead, SupervisorBound: &dead, Activity: idle}, ActivityUnknown, RungNone},
		{"terminal proof drops a self-report", ProbeInput{TerminalProof: true, Activity: working}, ActivityUnknown, RungNone},
		{"terminal proof over a live read drops the vote", ProbeInput{ProcessAlive: &alive, SupervisorBound: &bound, TerminalProof: true, Activity: working}, ActivityUnknown, RungNone},
		{"absent seat drops the vote", ProbeInput{RegistryReadable: true, Activity: idle}, ActivityUnknown, RungNone},
		{"present seat keeps its vote", ProbeInput{ProcessAlive: &alive, SupervisorBound: &bound, Activity: working}, ActivityWorking, RungHarnessReport},
		{"detached seat keeps its vote", ProbeInput{ProcessAlive: &alive, SupervisorBound: &unbound, Activity: idle}, ActivityIdleAtPrompt, RungVisiblePrompt},
		{"unknown liveness keeps its vote", ProbeInput{RegistryReadable: true, SeatKnown: true, Activity: idle}, ActivityIdleAtPrompt, RungVisiblePrompt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Report("sess", tc.input)
			if got.State.Activity != tc.wantActivity || got.State.DecidedBy != string(tc.wantRung) {
				t.Fatalf("activity = %q decidedBy = %q, want %q / %q", got.State.Activity, got.State.DecidedBy, tc.wantActivity, tc.wantRung)
			}
			if !got.State.Valid() {
				t.Fatalf("report %+v is not valid", got.State)
			}
		})
	}
}

// TestSeatReport_WireGolden pins the heartbeat wire bytes for one seat and
// its summary fold. Round-tripping through the same Go type cannot catch a
// renamed tag; these literals can.
func TestSeatReport_WireGolden(t *testing.T) {
	alive, bound := true, true
	when := time.Date(2026, 10, 12, 7, 0, 0, 0, time.UTC)
	full := Report("sess-a", ProbeInput{
		ProcessAlive: &alive, SupervisorBound: &bound, RegistryReadable: true, SeatKnown: true,
		Activity:               []ActivityEvidence{ConclusiveVote(RungHarnessReport, ActivityWorking)},
		NeedsInput:             NeedsInput{Count: 1, Reason: "approval"},
		ResumeArtifactReadable: &alive,
		ObservedAt:             when,
	})
	bare := Report("sess-b", ProbeInput{})
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"full seat", full, `{"sessionId":"sess-a","state":{"session":"present","activity":"working","decidedBy":"harness_report","resumability":"resumable","needsInput":{"count":1,"reason":"approval"}},"observedAt":"2026-10-12T07:00:00Z"}`},
		{"bare seat", bare, `{"sessionId":"sess-b","state":{"session":"unknown","activity":"unknown","decidedBy":"none","resumability":"unknown","needsInput":{}}}`},
		{"summary", Summarize([]SeatReport{full, bare}), `{"total":2,"session":{"present":1,"detached":0,"exited":0,"absent":0,"unknown":1},"activity":{"working":1,"idleAtPrompt":0,"unknown":1},"resumability":{"resumable":1,"freshOnly":0,"unknown":1},"needsInput":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("wire bytes drifted\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}
