package seatstate

import (
	"strings"
	"time"
)

// ProbeInput is the host-observed evidence one seat-state report is built
// from. Every field is optional: a probe that could not run leaves its
// field unset, and the report answers unknown rather than guessing.
//
// The zero ProbeInput is the "nothing observed" fixture: the report is
// unknown on every axis with no rung run and no input claimed.
type ProbeInput struct {
	// ProcessAlive is true when the seat's process identity (pid + start
	// time) proves live. Nil when the liveness probe never ran or errored.
	ProcessAlive *bool
	// SupervisorBound is true when the seat's supervisor binding is intact.
	// Nil when unobserved. Only read when ProcessAlive is true: a dead
	// process has no binding to evaluate.
	SupervisorBound *bool
	// TerminalProof is true when the harness published its own terminal
	// proof (exit event, tombstone). Terminal proof classifies the seat
	// exited even when the liveness probe never ran: it is the harness's
	// own last word, not an inference.
	TerminalProof bool
	// SeatKnown is true when the host holds any record of the seat (live
	// registry entry, discovery record, launch record). False with no
	// other evidence reports absent; unknown when the registry itself
	// could not be read.
	RegistryReadable bool
	SeatKnown        bool
	// Activity is the pre-arbitrated activity evidence for the seat,
	// ordered however the collectors produced it. DecideActivity ranks it.
	Activity []ActivityEvidence
	// NeedsInput is the orthogonal input signal for the seat.
	NeedsInput NeedsInput
	// ResumeArtifactReadable is true when positive evidence shows the
	// seat's context artifact exists and is readable (verified resume
	// key, readable checkpoint branch). Nil when the probe never ran.
	ResumeArtifactReadable *bool
	// NoResumeArtifact is true when positive evidence shows no resumable
	// artifact exists (nothing to preserve, verified absent). Only read
	// when ResumeArtifactReadable is not true.
	NoResumeArtifact bool
	// ObservedAt stamps the report. Zero leaves the stamp empty.
	ObservedAt time.Time
}

// SeatReport is one seat's three-axis report with its evidence record.
type SeatReport struct {
	// SessionID identifies the seat.
	SessionID string `json:"sessionId"`
	// State is the three-axis report plus needs-input.
	State SeatState `json:"state"`
	// ObservedAt is the probe instant (RFC3339), empty when unstamped.
	ObservedAt string `json:"observedAt,omitempty"`
}

// Report builds one seat's three-axis report from host-observed evidence.
//
//   - Session axis: terminal proof classifies exited; a live process
//     identity with an intact binding is present, without one detached;
//     a dead identity without terminal proof is still exited (the identity
//     proof is positive: pid + start time gone); a readable registry with
//     no record is absent. Anything else is unknown.
//   - Activity axis: DecideActivity over the ranked evidence.
//   - Resumability axis: a readable artifact is resumable; positive
//     absence is fresh_only; anything else is unknown.
func Report(sessionID string, in ProbeInput) SeatReport {
	state := SeatState{
		Session:      decideSession(in),
		Resumability: decideResumability(in),
		NeedsInput:   in.NeedsInput,
	}
	verdict := DecideActivity(in.Activity)
	state.Activity = verdict.Activity
	state.DecidedBy = string(verdict.DecidedBy)
	out := SeatReport{SessionID: strings.TrimSpace(sessionID), State: state}
	if !in.ObservedAt.IsZero() {
		out.ObservedAt = in.ObservedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func decideSession(in ProbeInput) SessionState {
	if in.TerminalProof {
		return SessionExited
	}
	if in.ProcessAlive != nil {
		if !*in.ProcessAlive {
			return SessionExited
		}
		if in.SupervisorBound != nil {
			if *in.SupervisorBound {
				return SessionPresent
			}
			return SessionDetached
		}
		return SessionUnknown
	}
	if in.RegistryReadable && !in.SeatKnown {
		return SessionAbsent
	}
	return SessionUnknown
}

func decideResumability(in ProbeInput) ResumabilityState {
	if in.ResumeArtifactReadable != nil {
		if *in.ResumeArtifactReadable {
			return ResumabilityResumable
		}
		if in.NoResumeArtifact {
			return ResumabilityFreshOnly
		}
		return ResumabilityUnknown
	}
	if in.NoResumeArtifact {
		return ResumabilityFreshOnly
	}
	return ResumabilityUnknown
}

// Summarize folds per-seat reports into axis counts for a heartbeat. Unknown
// seats are counted, never dropped: a consumer that cannot see the unknown
// population reads a healthy host where the host is actually blind.
func Summarize(reports []SeatReport) Summary {
	var out Summary
	out.Total = len(reports)
	for _, r := range reports {
		switch r.State.Session {
		case SessionPresent:
			out.Session.Present++
		case SessionDetached:
			out.Session.Detached++
		case SessionExited:
			out.Session.Exited++
		case SessionAbsent:
			out.Session.Absent++
		default:
			out.Session.Unknown++
		}
		switch r.State.Activity {
		case ActivityWorking:
			out.Activity.Working++
		case ActivityIdleAtPrompt:
			out.Activity.IdleAtPrompt++
		default:
			out.Activity.Unknown++
		}
		switch r.State.Resumability {
		case ResumabilityResumable:
			out.Resumability.Resumable++
		case ResumabilityFreshOnly:
			out.Resumability.FreshOnly++
		default:
			out.Resumability.Unknown++
		}
		out.NeedsInput += r.State.NeedsInput.Count
	}
	return out
}

// Summary is the heartbeat-sized fold of per-seat reports: axis counts plus
// the total needs-input count. Every seat lands in exactly one bucket per
// axis, so the buckets always sum to Total.
type Summary struct {
	Total        int             `json:"total"`
	Session      SessionSummary  `json:"session"`
	Activity     ActivitySummary `json:"activity"`
	Resumability ResumeSummary   `json:"resumability"`
	NeedsInput   int             `json:"needsInput"`
}

// SessionSummary counts seats per session-axis value.
type SessionSummary struct {
	Present  int `json:"present"`
	Detached int `json:"detached"`
	Exited   int `json:"exited"`
	Absent   int `json:"absent"`
	Unknown  int `json:"unknown"`
}

// ActivitySummary counts seats per activity-axis value.
type ActivitySummary struct {
	Working      int `json:"working"`
	IdleAtPrompt int `json:"idleAtPrompt"`
	Unknown      int `json:"unknown"`
}

// ResumeSummary counts seats per resumability-axis value.
type ResumeSummary struct {
	Resumable int `json:"resumable"`
	FreshOnly int `json:"freshOnly"`
	Unknown   int `json:"unknown"`
}
