package daemon

import (
	"time"

	"github.com/RenseiAI/donmai/daemon/seatstate"
)

// seatStates snapshots one three-axis report per in-flight seat. It is the
// daemon's per-seat projection for heartbeats and local readers: the union
// of the spawner's direct children and the adopted or quarantined shims,
// each mapped onto host-observed probe evidence.
//
// The mapping is deliberately conservative:
//
//   - A live spawner registry entry is present with its supervisor binding
//     intact — the daemon is the supervisor, and the entry exists only
//     while the reaper has not released it.
//   - An adopted shim consults the harness identity the controller last
//     reported: a live identity is present, a dead one exited. A nil or
//     unreadable controller is unknown, never a guess.
//   - A quarantined lineage has a live harness no controller trusts:
//     detached — the process group survives without its supervisor
//     binding, which is exactly the rebindable-not-dead shape detached
//     names.
//   - Activity evidence beyond the run-mode rung is not fabricated here:
//     an interactive or interview session votes idle_at_prompt at the
//     visible-prompt rung only when nothing stronger speaks, and headless
//     sessions offer no activity evidence at all. A seat with no evidence
//     reports unknown.
//   - Resumability is unknown unless the session detail carries a verified
//     resume key. The daemon never infers an artifact from a name, a flag,
//     or a surviving record.
func (d *Daemon) seatStates() []seatstate.SeatReport {
	now := d.shimNow()
	var out []seatstate.SeatReport
	if d.spawner != nil {
		for _, seat := range d.spawner.ActiveSeats() {
			out = append(out, seatstate.Report(seat.Handle.SessionID, seatstate.ProbeInput{
				ProcessAlive:     seatstate.BoolPtr(true),
				SupervisorBound:  seatstate.BoolPtr(true),
				RegistryReadable: true,
				SeatKnown:        true,
				Activity:         spawnerSeatActivityForMode(seat.Mode),
				NeedsInput:       spawnerSeatNeedsInput(seat.Handle),
				ObservedAt:       now,
			}))
		}
	}
	for _, rep := range d.adoptedShimSeatProbes(now) {
		out = append(out, seatstate.Report(rep.id, rep.input))
	}
	for _, rep := range d.quarantinedShimSeatProbes(now) {
		out = append(out, seatstate.Report(rep.id, rep.input))
	}
	return out
}

// spawnerSeatActivityForMode maps an admitted run mode onto arbiter evidence.
// Interactive and interview seats vote idle_at_prompt at the visible-prompt
// rung; every other mode offers no evidence and reports unknown.
func spawnerSeatActivityForMode(mode string) []seatstate.ActivityEvidence {
	switch mode {
	case interactiveRunMode, "interview":
		return []seatstate.ActivityEvidence{
			seatstate.ConclusiveVote(seatstate.RungVisiblePrompt, seatstate.ActivityIdleAtPrompt),
		}
	default:
		return nil
	}
}

// spawnerSeatNeedsInput reads the orthogonal input signal for a spawner
// seat. The spawner tracks no queued-input count today, so this is always
// the zero signal — which omits both keys on the wire rather than claiming
// a count the host never measured.
func spawnerSeatNeedsInput(SessionHandle) seatstate.NeedsInput { return seatstate.NeedsInput{} }

// shimSeatProbe is one shim seat's identity plus its probe evidence.
type shimSeatProbe struct {
	id    string
	input seatstate.ProbeInput
}

// adoptedShimSeatProbes maps adopted controllers onto probe evidence. A live
// harness identity is present; a dead one is exited; an unreadable
// controller is unknown. Resumability consults the session detail's resume
// key when one is stored.
func (d *Daemon) adoptedShimSeatProbes(now time.Time) []shimSeatProbe {
	if d.shims == nil {
		return nil
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	var out []shimSeatProbe
	for id, entry := range d.shims.adopted {
		sessionID := id.SessionID
		if sessionID == "" && entry.handle.SessionID != "" {
			sessionID = entry.handle.SessionID
		}
		in := seatstate.ProbeInput{
			RegistryReadable: true,
			SeatKnown:        true,
			Activity:         spawnerSeatActivityForMode(interactiveRunMode),
			ObservedAt:       now,
		}
		if entry.controller != nil {
			identity := entry.controller.HarnessIdentity()
			if alive, err := identity.Alive(); err == nil {
				in.ProcessAlive = seatstate.BoolPtr(alive)
				in.SupervisorBound = seatstate.BoolPtr(alive)
			}
		}
		in.ResumeArtifactReadable, in.NoResumeArtifact = d.shimResumeEvidence(sessionID)
		out = append(out, shimSeatProbe{id: sessionID, input: in})
	}
	return out
}

// quarantinedShimSeatProbes maps quarantined lineages onto probe evidence.
// A quarantined harness is live by construction (§D7 counts it toward
// capacity as running load) but has no supervisor binding this daemon
// trusts: detached.
func (d *Daemon) quarantinedShimSeatProbes(now time.Time) []shimSeatProbe {
	if d.shims == nil {
		return nil
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	var out []shimSeatProbe
	for _, q := range d.shims.quarantined {
		in := seatstate.ProbeInput{
			ProcessAlive:     seatstate.BoolPtr(true),
			SupervisorBound:  seatstate.BoolPtr(false),
			RegistryReadable: true,
			SeatKnown:        true,
			Activity:         spawnerSeatActivityForMode(interactiveRunMode),
			ObservedAt:       now,
		}
		in.ResumeArtifactReadable, in.NoResumeArtifact = d.shimResumeEvidence(q.SessionID)
		out = append(out, shimSeatProbe{id: q.SessionID, input: in})
	}
	return out
}

// shimResumeEvidence consults the stored session detail for a verified
// resume artifact. A detail carrying a valid resume key is positive
// evidence the context artifact exists; anything else is no evidence —
// never positive absence, which only the harness layer can assert.
func (d *Daemon) shimResumeEvidence(sessionID string) (*bool, bool) {
	if d.sessionDetails == nil || sessionID == "" {
		return nil, false
	}
	detail, ok := d.sessionDetails.Get(sessionID)
	if !ok || detail == nil || detail.ResumeKey == nil {
		return nil, false
	}
	if err := detail.ResumeKey.Validate(); err != nil {
		return nil, false
	}
	return seatstate.BoolPtr(true), false
}

// seatStateSummary folds the current per-seat reports into heartbeat-sized
// axis counts.
func (d *Daemon) seatStateSummary() seatstate.Summary {
	return seatstate.Summarize(d.seatStates())
}

var _ = time.Now
