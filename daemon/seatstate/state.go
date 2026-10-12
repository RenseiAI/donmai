package seatstate

// SessionState names which of the seat's process facts the host can prove.
//
//   - SessionPresent: the harness process identity (pid + start time) is
//     live under supervision.
//   - SessionDetached: the seat's process group survives but its supervisor
//     binding is gone — rebindable, not dead.
//   - SessionExited: the process identity is proved gone (reaped, or a
//     terminal proof the harness itself published).
//   - SessionAbsent: no record of the seat exists on this host at all.
//   - SessionUnknown: the probe was inconclusive. Unknown is first-class:
//     an inconclusive probe reports unknown, never a guess.
type SessionState string

// Session-axis values.
const (
	SessionPresent  SessionState = "present"
	SessionDetached SessionState = "detached"
	SessionExited   SessionState = "exited"
	SessionAbsent   SessionState = "absent"
	SessionUnknown  SessionState = "unknown"
)

// Valid reports whether s is a known session-axis value.
func (s SessionState) Valid() bool {
	switch s {
	case SessionPresent, SessionDetached, SessionExited, SessionAbsent, SessionUnknown:
		return true
	default:
		return false
	}
}

// ActivityState names what the harness is doing.
//
//   - ActivityWorking: the harness is making progress (turns, tool calls,
//     output).
//   - ActivityIdleAtPrompt: the harness is parked at a prompt waiting for
//     input — the interactive/interview idle state.
//   - ActivityUnknown: the probe was inconclusive. Unknown is first-class:
//     an inconclusive probe reports unknown, never a guess.
//
// Needs-input is NOT a member: it is a count plus a reason (see NeedsInput),
// orthogonal to activity. A working harness can hold queued input, and an
// idle harness can hold none.
type ActivityState string

// Activity-axis values.
const (
	ActivityWorking      ActivityState = "working"
	ActivityIdleAtPrompt ActivityState = "idle_at_prompt"
	ActivityUnknown      ActivityState = "unknown"
)

// Valid reports whether a is a known activity-axis value.
func (a ActivityState) Valid() bool {
	switch a {
	case ActivityWorking, ActivityIdleAtPrompt, ActivityUnknown:
		return true
	default:
		return false
	}
}

// ResumabilityState names whether a new incarnation can continue the seat's
// work.
//
//   - ResumabilityResumable: positive evidence shows the context artifact
//     exists and is readable — a new incarnation can be seeded from it.
//   - ResumabilityFreshOnly: positive evidence shows no resumable artifact
//     exists — only a fresh session is honest.
//   - ResumabilityUnknown: the probe was inconclusive. Unknown is
//     first-class: absent evidence reports unknown, never a guess.
type ResumabilityState string

// Resumability-axis values.
const (
	ResumabilityResumable ResumabilityState = "resumable"
	ResumabilityFreshOnly ResumabilityState = "fresh_only"
	ResumabilityUnknown   ResumabilityState = "unknown"
)

// Valid reports whether r is a known resumability-axis value.
func (r ResumabilityState) Valid() bool {
	switch r {
	case ResumabilityResumable, ResumabilityFreshOnly, ResumabilityUnknown:
		return true
	default:
		return false
	}
}

// NeedsInput is the orthogonal needs-input signal: a count plus a reason,
// never a state. Zero with an empty reason is the zero value and means no
// input is waiting; omitempty drops both keys on the wire.
type NeedsInput struct {
	// Count is the number of input items waiting (queued prompts, pending
	// approvals, unanswered questions).
	Count int `json:"count,omitempty"`
	// Reason names why input is needed (e.g. "approval", "question",
	// "inject"). Empty when Count is zero.
	Reason string `json:"reason,omitempty"`
}

// Valid reports whether n is internally consistent: a positive count needs
// a reason, and a reason needs a positive count.
func (n NeedsInput) Valid() bool {
	if n.Count < 0 {
		return false
	}
	if n.Count == 0 {
		return n.Reason == ""
	}
	return n.Reason != ""
}

// SeatState is one seat's report on all three axes plus the orthogonal
// needs-input signal.
//
// Every axis value is a closed vocabulary; the zero value ("") is never
// valid — use the explicit Unknown member when a probe is inconclusive.
// The zero SeatState is therefore invalid on every axis, which keeps an
// uninitialized report from parsing as a claim.
type SeatState struct {
	// Session is the presence axis: present | detached | exited | absent |
	// unknown.
	Session SessionState `json:"session"`
	// Activity is the liveness axis: working | idle_at_prompt | unknown.
	Activity ActivityState `json:"activity"`
	// DecidedBy names the evidence rung that decided Activity (see
	// EvidenceRung). Empty only when Activity itself is unset; an unknown
	// activity decided by "no evidence reached any rung" still records the
	// rung that ran last (RungPTYActivity) — the record says what ran, not
	// what decided.
	DecidedBy string `json:"decidedBy,omitempty"`
	// Resumability is the continuation axis: resumable | fresh_only |
	// unknown.
	Resumability ResumabilityState `json:"resumability"`
	// NeedsInput is the orthogonal input signal: a count plus a reason,
	// never a state.
	NeedsInput NeedsInput `json:"needsInput,omitempty"`
}

// Valid reports whether s carries a known value on every axis, a rung name
// consistent with its activity, and a consistent needs-input signal.
func (s SeatState) Valid() bool {
	if !s.Session.Valid() || !s.Activity.Valid() || !s.Resumability.Valid() {
		return false
	}
	if !s.NeedsInput.Valid() {
		return false
	}
	if s.DecidedBy == "" {
		return false
	}
	return validRung(s.DecidedBy)
}

// UnknownSeatState is the honest report when no probe ran: unknown on every
// axis, decided by no rung, no input claimed.
func UnknownSeatState() SeatState {
	return SeatState{
		Session:      SessionUnknown,
		Activity:     ActivityUnknown,
		DecidedBy:    string(RungNone),
		Resumability: ResumabilityUnknown,
	}
}
