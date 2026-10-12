package seatstate

// EvidenceRung names one rung of the activity arbiter's ranked evidence
// ladder. Lower rank wins: the first rung whose evidence is conclusive
// decides, and DecidedBy records it.
type EvidenceRung string

// Activity-arbitration rungs, in rank order.
const (
	// RungNone decided nothing: no evidence was offered at all. Only
	// UnknownSeatState carries it.
	RungNone EvidenceRung = "none"
	// RungHarnessReport is the harness's own self-report (e.g. an activity
	// event it emitted). The actor closest to the work speaks first.
	RungHarnessReport EvidenceRung = "harness_report"
	// RungHooks is a lifecycle hook observation (e.g. a stop/turn hook
	// fired, a hook feed went quiet).
	RungHooks EvidenceRung = "hooks"
	// RungVisiblePrompt is the visible prompt surface: a prompt is on
	// screen waiting for input.
	RungVisiblePrompt EvidenceRung = "visible_prompt"
	// RungPTYActivity is the PTY byte stream: recent output frames mean
	// working, sustained silence at a prompt means idle. It runs last
	// because it is the weakest signal — silence is ambiguous until every
	// stronger source has declined to answer.
	RungPTYActivity EvidenceRung = "pty_activity"
)

// rungRank orders the rungs: index 0 decides first.
var rungRank = []EvidenceRung{
	RungHarnessReport,
	RungHooks,
	RungVisiblePrompt,
	RungPTYActivity,
}

func validRung(name string) bool {
	switch EvidenceRung(name) {
	case RungNone, RungHarnessReport, RungHooks, RungVisiblePrompt, RungPTYActivity:
		return true
	default:
		return false
	}
}

// ActivityEvidence is one rung's input to the arbiter. A rung is
// conclusive only when its probe actually ran (Observed) and reached a
// verdict (Conclusive): a probe that never ran, errored, or timed out is
// inconclusive by construction, and the arbiter moves to the next rung.
type ActivityEvidence struct {
	// Rung is the ladder position this evidence speaks for.
	Rung EvidenceRung
	// Observed is true when the probe ran to a reading. False means the
	// probe never ran, errored, or timed out — inconclusive, never a
	// guess.
	Observed bool
	// Conclusive is true when the reading settles the question. An
	// observed-but-ambiguous reading (e.g. PTY silence with no prompt on
	// screen) is inconclusive.
	Conclusive bool
	// Activity is the verdict this rung votes for when Observed and
	// Conclusive are both true.
	Activity ActivityState
}

// Verdict is the arbiter's output: the decided activity plus the rung
// that decided it.
type Verdict struct {
	// Activity is the decided state, or ActivityUnknown when no rung was
	// conclusive.
	Activity ActivityState
	// DecidedBy is the rung that decided. When no rung was conclusive it
	// is the lowest rung that ran (RungPTYActivity when the full ladder
	// ran) — the record says what ran, not what decided. When no
	// evidence was offered at all it is RungNone.
	DecidedBy EvidenceRung
}

// DecideActivity runs the ranked ladder over evidence and returns the
// first conclusive rung's verdict. Evidence may arrive in any order and
// may repeat rungs; the first conclusive vote per rung in rank order
// wins. Later (lower-rank) rungs never override an earlier decision.
//
// When no rung is conclusive the verdict is ActivityUnknown with
// DecidedBy naming the lowest rung that ran — or RungNone when nothing
// ran. Unknown is the honest answer: an inconclusive probe reports
// unknown, never a guess.
func DecideActivity(evidence []ActivityEvidence) Verdict {
	byRung := make(map[EvidenceRung]ActivityEvidence, len(evidence))
	ran := make(map[EvidenceRung]bool, len(evidence))
	for _, e := range evidence {
		if !validRung(string(e.Rung)) || e.Rung == RungNone {
			continue
		}
		ran[e.Rung] = true
		if _, seen := byRung[e.Rung]; seen {
			continue
		}
		byRung[e.Rung] = e
	}
	lowestRan := RungNone
	for _, rung := range rungRank {
		if ran[rung] {
			lowestRan = rung
		}
		if e, ok := byRung[rung]; ok && e.Observed && e.Conclusive && e.Activity.Valid() {
			return Verdict{Activity: e.Activity, DecidedBy: rung}
		}
	}
	return Verdict{Activity: ActivityUnknown, DecidedBy: lowestRan}
}

// ConclusiveVote is a test-and-call-site helper: one observed, conclusive
// rung vote.
func ConclusiveVote(rung EvidenceRung, activity ActivityState) ActivityEvidence {
	return ActivityEvidence{Rung: rung, Observed: true, Conclusive: true, Activity: activity}
}

// BoolPtr boxes a probe boolean so call sites can distinguish "probed
// true/false" from "probe never ran" (nil).
func BoolPtr(b bool) *bool { return &b }

// InconclusiveProbe is a test-and-call-site helper: a probe that ran but
// reached no verdict (ambiguous reading), or never ran at all. Observed
// distinguishes the two in the record; both are inconclusive to the
// arbiter.
func InconclusiveProbe(rung EvidenceRung, observed bool) ActivityEvidence {
	return ActivityEvidence{Rung: rung, Observed: observed}
}
