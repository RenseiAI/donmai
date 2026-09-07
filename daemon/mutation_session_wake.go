package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
// BOTH CARRY ZERO CONTENT. They are keystrokes, not messages: a line clear, a
// bare Enter, and an interrupt byte. Nothing here can inject text into a
// terminal, which keeps the remediation rail incapable by construction of the
// thing the input rail is carefully restricted from doing.
//
// And zero content is not by itself enough, because an Enter SUBMITS what is
// already there. Both rungs therefore clear the line editor before they submit
// (clearKeystroke), so neither can send a draft the seat never finished. That
// is a property of the sequence, not of the bytes, and it is tested directly.
//
// Scoped precisely, since this file argues the point harder than any other:
// the rungs write only CONTROL bytes and never text. On a seat whose editor
// interprets them the submitted line is empty. On a seat whose editor does NOT
// — a canonical-mode reader, where Ctrl-U is eaten by the line discipline and
// Ctrl-A/Ctrl-K arrive as data — the submitted line is those two control bytes
// rather than nothing. Still not text, still not the seat's draft, but not
// literally empty either.
//
// BOTH RETAIN THE SHIM. Neither verb stops, terminates or respawns anything.
// They write through the adopted shim's existing controller, so the session
// keeps its identity, its PTY, its adoption generation and its worktree. A
// stop remains a separate, later decision made by the control plane, never an
// escalation this file performs on its own.
//
// THE ORDER IS ENFORCED HERE, NOT ONLY DECIDED UPSTREAM. The control plane
// chooses the rung, but the mutation channel is at-least-once and can reorder
// or replay a delivery. A per-session ledger (wakeLedger) refuses rung 2 on a
// seat that was never woken, and dedupes a replayed mutation id so a second
// interrupt is never written into a terminal that may since have recovered.
//
// THE LEDGER IS PROCESS-LOCAL and is forgotten on a daemon restart, which
// moves the two properties in opposite directions. Ordering gets SAFER: the
// woken flag is gone, so a redelivered rung 2 hard-refuses. Dedupe is LOST: a
// rung whose ack was in flight across the restart is written again — harmless
// for a wake, and for a restart it is the second interrupt the ledger exists
// to prevent. So the ordering guarantee ultimately still lives in the control
// plane; this is a second line of defence, not the only one.
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

// clearKeystroke empties whatever holds the pending line, before anything is
// submitted: Ctrl-U, then Ctrl-A, then Ctrl-K.
//
// THIS IS NOT OPTIONAL, AND IT IS THE WHOLE POINT OF THE RUNG. A bare Enter is
// a SUBMIT. It carries no content of its own, but its EFFECT is to send
// whatever the terminal is already holding — and the seat this verb is aimed
// at is exactly the seat most likely to be holding a half-composed line or an
// abandoned paste. Enter alone would take content this rail never wrote and
// cause it to be sent, which is the harm the input rail is carefully
// restricted from doing, reached sideways.
//
// WHY THREE BYTES AND NOT THE OPERATOR RAIL'S TWO. The established nudge rail
// sends Ctrl-A + Ctrl-K, which are LINE-EDITOR commands: they are interpreted
// by a full-screen application's own editor, and that is the right choice for
// the terminal UIs this class occurs in. But when the pending line is held by
// the KERNEL line discipline instead — a seat in canonical mode — Ctrl-A and
// Ctrl-K are not commands at all, they are ordinary data. A test with a real
// draft planted in a canonical-mode fixture proved the consequence: the two
// bytes were APPENDED to the draft and the whole thing was then submitted,
// which is strictly worse than sending nothing.
//
// Ctrl-U is the line discipline's own kill character (VKILL), so it clears the
// case the editor commands cannot reach.
//
// THE SAFETY ARGUMENT IS THE ORDER, not that Ctrl-U is universally a line
// kill — it is not. A full-screen application binds it to whatever it likes;
// this repo's own terminal UI binds ctrl+u to page-up. What makes the sequence
// safe anyway is that Ctrl-U goes FIRST: wherever it scrolls, is swallowed, or
// inserts a stray byte, the Ctrl-A + Ctrl-K that follow still move to the start
// of the line and kill to the end. The line is cleared either way, and the
// worst case is a scroll nobody sees.
var clearKeystroke = []byte{0x15, 0x01, 0x0b}

// submitKeystroke is a bare Enter — one carriage return, no content.
//
// The shim's last hop recognises a SYSTEM-authority bare Enter and gives it
// the treatment it needs: a dangling bracketed-paste region is closed first so
// the byte cannot be swallowed as literal pasted text, and the write is paced
// away from any immediately preceding input so a terminal UI's paste-detection
// heuristic does not coalesce and eat it. Keeping the submit a BARE Enter — a
// separate write from the clear that precedes it — is what earns that pacing:
// the last hop only paces a write that is exactly one CR.
var submitKeystroke = []byte{'\r'}

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
//
// It sleeps inside the mutation apply loop. That is safe today — the session
// branch returns before the config lock is taken — but it is per-mutation
// latency in the heartbeat handler, so a batch carrying several rungs pays it
// serially. The ladder makes that unlikely (one rung per freeze per beat)
// rather than impossible.
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

// writeClearThenSubmit empties the line editor and only then submits.
//
// Two writes, never one concatenated buffer: the last hop paces a write that
// is exactly one CR, and a clear glued onto the front of it would forfeit that
// pacing for the byte that needs it most.
func writeClearThenSubmit(ctrl *sessionshim.Controller) error {
	if err := writeWakeKeystroke(ctrl, clearKeystroke); err != nil {
		return fmt.Errorf("deliver line clear: %w", err)
	}
	if err := writeWakeKeystroke(ctrl, submitKeystroke); err != nil {
		return fmt.Errorf("deliver submit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The per-session rung ledger.
//
// The mutation channel is AT-LEAST-ONCE: an ack lost between the heartbeat
// response and the next poll re-presents the same mutation. That is harmless
// for a kill, which is idempotent, and NOT harmless for an interrupt — a second
// ^C lands on a seat that may by then have recovered and started a real turn,
// abandoning it. So redelivery is deduped here, by mutation id, rather than
// assumed away.
//
// The ledger also enforces the one ordering property the ladder depends on: a
// seat must have been WOKEN before it is interrupted. The control plane decides
// which rung to send; this refuses to let a lost or reordered delivery turn
// rung 2 into the first thing a seat ever receives.
// ---------------------------------------------------------------------------

// wakeLedgerMaxSessions bounds the ledger. It is remediation state for seats
// currently believed wedged, which is a handful at most; the cap exists so a
// pathological mutation stream cannot grow it without limit.
const wakeLedgerMaxSessions = 256

// wakeLedgerMaxMutationIDs is how many recent mutation ids are remembered per
// session for dedupe. Redelivery arrives within a beat or two of the original,
// so a short memory is sufficient and a long one only delays reclamation.
const wakeLedgerMaxMutationIDs = 8

// wakeLedger is the per-daemon rung ledger. Its zero value is ready to use;
// the map is created on first write.
type wakeLedger struct {
	mu      sync.Mutex
	entries map[sessionshim.Identity]*wakeLedgerEntry
}

type wakeLedgerEntry struct {
	woken       bool
	appliedIDs  []string
	lastTouched time.Time
}

// errRestartBeforeWake refuses rung 2 on a seat that has not been woken.
var errRestartBeforeWake = errors.New("no wake has been delivered to this session; rung 1 must precede rung 2")

// noteWakeMutation records a rung against a session and reports whether this
// exact mutation was already applied.
//
// CHECK AND COMMIT ARE SEPARATE, and the split is the point. Recording a rung
// on the way IN would mean a rung 1 whose write FAILED still satisfied the
// ordering guard — and the seats this rail targets are precisely the ones
// whose transport is least healthy, so a failed write is not the exotic case.
// The next interrupt would then genuinely be the first byte the seat ever
// received, which is the one thing the guard exists to prevent. The same
// mistake would ACK a redelivered failed mutation as "already applied": a
// false receipt on a rail whose entire value is that delivered and answered
// are different facts.
//
// So: check, write, and only then commit.

// checkWakeMutation reports whether this mutation was already applied, and
// refuses a rung that is out of order. It MUTATES NOTHING.
//
// Returns (alreadyApplied, error). An ordering violation is an error; a
// redelivery is not — it is a successful no-op, because the mutation DID take
// effect, just on an earlier beat.
func (d *Daemon) checkWakeMutation(id sessionshim.Identity, mutationID, op string) (bool, error) {
	d.wakeLedger.mu.Lock()
	defer d.wakeLedger.mu.Unlock()
	entry := d.wakeLedger.entries[id]
	if entry == nil {
		// Nothing recorded for this seat. Only rung 1 may open the ladder.
		if op == "session.restart-harness" {
			return false, errRestartBeforeWake
		}
		return false, nil
	}
	if mutationID != "" {
		for _, applied := range entry.appliedIDs {
			if applied == mutationID {
				return true, nil
			}
		}
	}
	if op == "session.restart-harness" && !entry.woken {
		return false, errRestartBeforeWake
	}
	return false, nil
}

// commitWakeMutation records a rung that has ACTUALLY been written.
//
// Called only after every keystroke of the verb reached the shim, so a failed
// write leaves the ledger untouched: the ordering guard still refuses rung 2,
// and a redelivery re-applies rather than being deduped into a false receipt.
func (d *Daemon) commitWakeMutation(id sessionshim.Identity, mutationID, op string) {
	d.wakeLedger.mu.Lock()
	defer d.wakeLedger.mu.Unlock()
	if d.wakeLedger.entries == nil {
		d.wakeLedger.entries = make(map[sessionshim.Identity]*wakeLedgerEntry)
	}
	entry := d.wakeLedger.entries[id]
	if entry == nil {
		entry = &wakeLedgerEntry{}
		d.wakeLedger.evictLocked()
		d.wakeLedger.entries[id] = entry
	}
	if op == "session.wake" {
		entry.woken = true
	}
	if mutationID != "" {
		entry.appliedIDs = append(entry.appliedIDs, mutationID)
		if len(entry.appliedIDs) > wakeLedgerMaxMutationIDs {
			entry.appliedIDs = entry.appliedIDs[len(entry.appliedIDs)-wakeLedgerMaxMutationIDs:]
		}
	}
	entry.lastTouched = time.Now()
}

// evictLocked drops the least recently touched entry once the cap is reached.
func (l *wakeLedger) evictLocked() {
	if len(l.entries) < wakeLedgerMaxSessions {
		return
	}
	var oldestID sessionshim.Identity
	var oldest time.Time
	first := true
	for id, entry := range l.entries {
		if first || entry.lastTouched.Before(oldest) {
			oldestID, oldest, first = id, entry.lastTouched, false
		}
	}
	if !first {
		delete(l.entries, oldestID)
	}
}

// applySessionWake is rung 1: clear the line editor, then submit.
//
// Bounded PER MUTATION — one clear and one submit, no retry loop, no
// escalation to another rung. The ladder itself is the control plane's to
// climb; this handler only ever performs the rung it was handed.
func (d *Daemon) applySessionWake(m PendingMutation) error {
	params, err := decodeSessionWakeParams("session.wake", m.Params)
	if err != nil {
		return err
	}
	ctrl, err := d.resolveWakeController(params)
	if err != nil {
		return fmt.Errorf("session.wake: %w", err)
	}
	already, err := d.checkWakeMutation(ctrl.Identity(), m.ID, "session.wake")
	if err != nil {
		return fmt.Errorf("session.wake: %w", err)
	}
	if already {
		slog.Info("[daemon-sync] session wake already applied (redelivery)",
			"session", params.SessionID, "mutation", m.ID)
		return nil
	}
	if err := writeClearThenSubmit(ctrl); err != nil {
		return fmt.Errorf("session.wake: %w", err)
	}
	d.commitWakeMutation(ctrl.Identity(), m.ID, "session.wake")
	slog.Info("[daemon-sync] session wake delivered",
		"session", params.SessionID, "reason", params.Reason)
	return nil
}

// applySessionRestartHarness is rung 2: interrupt the stalled turn, then wake.
//
// It reports applied when every keystroke reached the shim. That is a receipt
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
	already, err := d.checkWakeMutation(ctrl.Identity(), m.ID, "session.restart-harness")
	if err != nil {
		return fmt.Errorf("session.restart-harness: %w", err)
	}
	if already {
		slog.Info("[daemon-sync] session harness restart already applied (redelivery)",
			"session", params.SessionID, "mutation", m.ID)
		return nil
	}
	if err := writeWakeKeystroke(ctrl, interruptKeystroke); err != nil {
		return fmt.Errorf("session.restart-harness: deliver interrupt: %w", err)
	}
	time.Sleep(interruptSettleGap)
	if err := writeClearThenSubmit(ctrl); err != nil {
		// Deliberately NOT committed: the interrupt landed but the verb did
		// not complete, and a redelivery re-writing the interrupt is exactly
		// what the dedupe is for — a half-applied rung 2 is the case where
		// retrying is safer than recording a delivery that did not finish.
		return fmt.Errorf("session.restart-harness: %w", err)
	}
	d.commitWakeMutation(ctrl.Identity(), m.ID, "session.restart-harness")
	slog.Info("[daemon-sync] session harness interrupt+wake delivered",
		"session", params.SessionID, "reason", params.Reason)
	return nil
}
