package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
)

// Remediation for a WEDGED seat: a live harness process at 0% CPU whose
// terminal has gone byte-frozen while its message cursor keeps advancing and
// every delivered nudge is ignored.
//
// WHY THE ORDINARY RAILS DO NOT REACH IT. The process is alive, so nothing
// times out. The control plane's own liveness reads delivery state, which the
// still-fetching runtime keeps warm. And the standard nudge is BYTES: it
// arrives at a PTY whose reader has stopped reading, so it queues and nothing
// happens. That is the whole class — the seat is not gone, it is not idle, and
// it cannot be talked to.
//
// The two verbs here are the bounded, escalating automatic action the control
// plane drives, mirroring how session.kill already rides the heartbeat's
// pending-mutation channel (see mutation_apply.go):
//
//	session.wake             rung 1 — one content-free wake keystroke.
//	session.restart-harness  rung 2 — an interrupt first, to unwind whatever
//	                         turn the harness is stalled inside, then a wake.
//
// BOTH CARRY ZERO CONTENT. They are keystrokes, not messages: a bare Enter and
// an interrupt byte. Nothing here can inject text into a terminal, which keeps
// the remediation rail incapable by construction of the thing the input rail
// is carefully restricted from doing.
//
// BOTH RETAIN THE SHIM. Neither verb stops, terminates or respawns anything.
// They write through the adopted shim's existing controller, so the session
// keeps its identity, its PTY, its adoption generation and its worktree. A
// stop remains a separate, later decision made by the control plane, never an
// escalation this file performs on its own.
//
// WHAT IS DELIBERATELY NOT HERE. Rung 2 does not re-exec the harness child
// under the retained shim. That primitive does not exist: the shim owns its
// child through a single terminal teardown path, and giving it a
// "replace the child, keep the shim" operation needs a new message in the
// closed wire vocabulary — a governed protocol change, not a patch. Rung 2
// therefore performs the strongest retained-shim action that IS available and
// reports success for having performed it; whether the seat actually recovered
// is judged where it must be, at the consumer, by watching terminal output
// resume. If it does not, the control plane's ladder escalates on its own.

// wakeKeystroke is a bare Enter — one carriage return, no content.
//
// The shim's last hop recognises a SYSTEM-authority bare Enter and gives it
// the treatment it needs: a dangling bracketed-paste region is closed first so
// the byte cannot be swallowed as literal pasted text, and the write is paced
// away from any immediately preceding input so a terminal UI's paste-detection
// heuristic does not coalesce and eat it. Sending a bare Enter is therefore
// strictly better than sending our own richer sequence — it is the one shape
// that path is built to protect.
var wakeKeystroke = []byte{'\r'}

// interruptKeystroke is the terminal interrupt character (ETX, ^C).
//
// It is rung 2's whole reason to exist: it is a DIFFERENT MECHANISM from rung
// 1 rather than a louder version of it. A wake asks the harness to notice
// input it has stopped reading; an interrupt asks it to abandon the turn it is
// stuck in. A seat that ignores the first can still answer the second.
//
// Honest limit: a terminal UI that puts its tty in raw mode has signal
// generation disabled, and there the interrupt arrives as an ordinary byte
// that the stalled reader will not read either. Rung 2 is a real escalation,
// not a guarantee — which is exactly why the ladder has a rung after it.
var interruptKeystroke = []byte{0x03}

// interruptSettleGap is the pause between the interrupt and the wake that
// follows it, so the harness has a moment to unwind before being asked to
// redraw. Short enough that the whole verb stays well inside one mutation
// application; long enough to be a real ordering, not a race.
//
// A package var, not a const, so tests exercise the real two-write path
// without spending wall-clock time on it.
var interruptSettleGap = 250 * time.Millisecond

// errShimAmbiguous reports a bare session id that names shim-backed sessions
// in more than one organization. Mutations carry an org id precisely so this
// cannot happen; refusing loudly is still correct, because silently picking
// one would write keystrokes into an unrelated tenant's terminal.
var errShimAmbiguous = errors.New("session id is ambiguous across organizations")

// sessionWakeParams is the wire shape both remediation verbs decode.
//
// OrgID is optional for source compatibility with the session-scoped mutation
// shape already on the wire; when present it removes the ambiguity lookup
// entirely, and it SHOULD always be sent.
type sessionWakeParams struct {
	SessionID string `json:"sessionId"`
	OrgID     string `json:"orgId"`
	Reason    string `json:"reason"`
}

func decodeSessionWakeParams(op string, raw json.RawMessage) (sessionWakeParams, error) {
	var params sessionWakeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return params, fmt.Errorf("%s decode params: %w", op, err)
	}
	params.SessionID = strings.TrimSpace(params.SessionID)
	params.OrgID = strings.TrimSpace(params.OrgID)
	params.Reason = strings.TrimSpace(params.Reason)
	if params.SessionID == "" {
		return params, fmt.Errorf("%s requires sessionId", op)
	}
	return params, nil
}

// resolveWakeController finds the adopted shim controller a remediation verb
// must write through.
//
// With an org id this is the exact tenant-scoped lookup. Without one it falls
// back to a session-id scan that refuses rather than guesses when the id names
// more than one tenant's session — the same rule the stop path applies to the
// bare session ids the local control API speaks.
func (d *Daemon) resolveWakeController(params sessionWakeParams) (*sessionshim.Controller, error) {
	if params.OrgID != "" {
		return d.adoptedShimController(params.OrgID, params.SessionID)
	}
	if d.shims == nil {
		return nil, fmt.Errorf("session shim: %s is not adopted by this daemon", params.SessionID)
	}
	d.shims.mu.RLock()
	matches := make([]sessionshim.Identity, 0, 1)
	for id := range d.shims.adopted {
		if id.SessionID == params.SessionID {
			matches = append(matches, id)
		}
	}
	d.shims.mu.RUnlock()
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("session shim: %s is not adopted by this daemon", params.SessionID)
	case 1:
		return d.adoptedShimController(matches[0].OrgID, matches[0].SessionID)
	default:
		return nil, fmt.Errorf("session shim: %s: %w", params.SessionID, errShimAmbiguous)
	}
}

// writeWakeKeystroke sends one content-free keystroke under SYSTEM authority.
//
// The attribution is what earns the last-hop paste guard and pacing; a shim on
// an older wire that cannot carry attribution still receives the identical
// bytes, just without that guarantee.
func writeWakeKeystroke(ctrl *sessionshim.Controller, data []byte) error {
	return ctrl.WriteAttributedInput([]byte(attachwire.SystemNudgeUserID), data)
}

// applySessionWake is rung 1: deliver exactly one wake keystroke.
//
// Bounded by construction — one keystroke per mutation, no retry loop, no
// escalation. Re-poking a seat that stayed frozen is the control plane's
// decision to make by enqueueing the next rung, not this handler's to take.
func (d *Daemon) applySessionWake(m PendingMutation) error {
	params, err := decodeSessionWakeParams("session.wake", m.Params)
	if err != nil {
		return err
	}
	ctrl, err := d.resolveWakeController(params)
	if err != nil {
		return fmt.Errorf("session.wake: %w", err)
	}
	if err := writeWakeKeystroke(ctrl, wakeKeystroke); err != nil {
		return fmt.Errorf("session.wake: deliver wake keystroke: %w", err)
	}
	slog.Info("[daemon-sync] session wake delivered",
		"session", params.SessionID, "reason", params.Reason)
	return nil
}

// applySessionRestartHarness is rung 2: interrupt the stalled turn, then wake.
//
// It reports applied when both keystrokes reached the shim. That is a receipt
// for DELIVERY, never a claim that the seat recovered — recovery is only ever
// provable by terminal output resuming, which the control plane observes
// directly and which is what advances or escalates the ladder.
func (d *Daemon) applySessionRestartHarness(m PendingMutation) error {
	params, err := decodeSessionWakeParams("session.restart-harness", m.Params)
	if err != nil {
		return err
	}
	ctrl, err := d.resolveWakeController(params)
	if err != nil {
		return fmt.Errorf("session.restart-harness: %w", err)
	}
	if err := writeWakeKeystroke(ctrl, interruptKeystroke); err != nil {
		return fmt.Errorf("session.restart-harness: deliver interrupt: %w", err)
	}
	time.Sleep(interruptSettleGap)
	if err := writeWakeKeystroke(ctrl, wakeKeystroke); err != nil {
		return fmt.Errorf("session.restart-harness: deliver wake keystroke: %w", err)
	}
	slog.Info("[daemon-sync] session harness interrupt+wake delivered",
		"session", params.SessionID, "reason", params.Reason)
	return nil
}
