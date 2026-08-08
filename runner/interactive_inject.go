package runner

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
)

// This file is the interactive half of the runtime-inject rail.
//
// The headless path folds a buffered inject into the model as a follow-up turn
// at the post-terminal seam (drainMemoryInjects); the interview path parks on
// injectCh and resumes the session per inject. An interactive PTY session has
// neither seam — there is no turn boundary to wait for and no Handle.Inject to
// call (the PTY handle answers agent.ErrUnsupported; the terminal is the input
// surface).
//
// # What this leg may and may not do
//
// Delivering a message into a live session is a DECLARED per-harness
// capability (agent.NoticeDelivery), never an assumed one, and never a
// decision keyed off a harness's NAME. This leg implements TWO of the declared
// mechanisms, which are opposite in shape:
//
//   - agent.NoticeDeliveryPTYNotice — PUSH. Correct for exactly one harness,
//     `shell`, where there is no agent behind the terminal to route around: a
//     line written at an idle shell prompt is a command the shell runs.
//   - agent.NoticeDeliveryHook — PULL. The harness itself calls out at a
//     lifecycle point and the message is delivered by answering that call
//     (Claude Code's Stop hook). Nothing is typed at the terminal.
//
// For every OTHER interactive harness a PTY write is a KEYSTROKE into whatever
// that harness's UI is currently drawing — a permission prompt, a plan
// confirmation, a trust dialog — and a submit byte there selects the
// highlighted option. So this leg REFUSES those outright and says why. It does
// not best-effort them, and a refusal never acks: the message stays with its
// producer, which is where the durable record lives.
//
// # Nothing here acks what it did not deliver
//
// The acceptor takes custody without claiming delivery (newInjectAcceptor's
// ackOnBuffer=false for this mode). A payload this session cannot place is
// either held for retry, or DEAD-LETTERED — reported back to the producer with
// a reason so a sender that was told "queued" can learn it died — never
// silently dropped and never acked.
//
// Where the ack falls differs by shape, and the difference is the whole point
// of separating them:
//
//   - PUSH acks when the bytes reach the PTY. For a shell there is nothing
//     behind the terminal to consult; the write IS the arrival.
//   - PULL acks only when the RECIPIENT'S OWN record shows the message entered
//     its conversation (agent.NoticeChannel.Consumed). Placing a message where
//     a harness will look is not delivery, and the interval between the two is
//     unbounded: it ends when the current turn ends. A pull channel that acked
//     on placement would report every silently-discarded message as delivered,
//     which is the failure this axis exists to prevent.
const (
	// interactiveNoticeRetry is how long the supervisor waits before
	// re-attempting a notice that was refused (human mid-composition, child on
	// the alternate screen) or whose write failed. Short enough that a notice
	// lands within a beat of the human pressing Enter; long enough to cost
	// nothing while they type.
	interactiveNoticeRetry = 2 * time.Second

	// interactiveNoticeMaxAttempts bounds how many times ONE notice may be
	// attempted before it is dead-lettered.
	//
	// This bound is the head-of-line fix. The rail allows one notice in flight
	// per session; without a cap, a single payload that can never be placed
	// (the human walked away mid-line, the child is parked in a full-screen
	// pager) holds the slot forever and starves every later payload behind it,
	// including ones the session could have taken. With the cap the slot always
	// frees, and the starved payload becomes an observable dead letter instead
	// of an invisible stall.
	//
	// 30 × interactiveNoticeRetry ≈ one minute of trying, which is generous for
	// "finish typing your line" and short enough that a queue behind it is not
	// meaningfully delayed.
	interactiveNoticeMaxAttempts = 30

	// interactiveNoticePullMaxPolls bounds how long ONE notice may sit on a
	// PULL channel, offered but not yet consumed, before it is dead-lettered.
	//
	// It is a SEPARATE bound from interactiveNoticeMaxAttempts, and separating
	// them is not tidiness. An attempt is a refusal — the session was asked and
	// said no — and thirty of those is decisive. A poll is not a refusal: it is
	// the runner asking "has the turn ended yet?", and the honest answer can be
	// "no" for as long as the agent keeps working. Spending the refusal budget
	// on waiting would dead-letter every message sent to a session doing a
	// minute of real work, which is most of them.
	//
	// It is still bounded, because the rail allows one notice in flight per
	// session: a channel nobody ever collects from would otherwise hold the
	// slot for the life of the session and starve everything behind it. 450 ×
	// interactiveNoticeRetry = 15 minutes, which is longer than an agent turn
	// and short enough that the producer gets its payload back while the fact
	// still matters.
	interactiveNoticePullMaxPolls = 450

	// interactiveNoticeTruncated marks a notice clipped to the canonical-mode
	// input bound. The full text remains wherever it was durably queued.
	interactiveNoticeTruncated = " …[truncated]"

	// interactiveNoticeSubmit is the byte that makes a notice arrive as a
	// SUBMITTED TURN rather than as text sitting in an input box. It is CR
	// (0x0D), not LF, and the distinction is load-bearing:
	//
	//   - A terminal emulator sends CR for the Return key. LF is what the
	//     Ctrl-J chord sends. They are different keys on the wire.
	//   - A raw-mode TUI reads the PTY slave with ICRNL off, so it sees the
	//     byte verbatim and its keypress parser maps CR and LF to DIFFERENT
	//     key names. Sending LF there types a distinct, usually inert key:
	//     the text lands in the input box and no turn is ever taken, while
	//     every layer above reports success.
	//   - A line-oriented child in the default (cooked) discipline gets CR
	//     translated to NL by ICRNL, so its read still terminates.
	//
	// CR is therefore correct in BOTH modes; LF is correct only in cooked
	// mode. See TestInteractiveNotice_SubmitByteDistinguishesReturnFromCtrlJ,
	// which drives a raw-mode fixture that tells the two apart.
	interactiveNoticeSubmit = '\r'
)

// Dead-letter reasons. Short, stable tokens: they ride back to the producer, so
// they are part of the wire vocabulary, not prose.
const (
	// noticeDeadChannelNotDriven — the harness declares a notice-delivery
	// channel this build does not implement. Structural: retrying cannot help,
	// so it is dead-lettered on the first attempt.
	noticeDeadChannelNotDriven = "channel-not-driven"

	// noticeDeadSurfaceNotNotifier — the harness declares pty-notice but the
	// live PTY surface does not implement agent.InteractiveNotifier. Also
	// structural.
	noticeDeadSurfaceNotNotifier = "surface-cannot-accept-notices"

	// noticeDeadAttemptCap — the notice was legitimately deliverable but the
	// session refused it interactiveNoticeMaxAttempts times running.
	noticeDeadAttemptCap = "attempt-cap-exceeded"

	// noticeDeadChannelAbsent — the harness declares a PULL channel this build
	// drives, but THIS session exposed none (the handle is not
	// agent.NoticeChannelCapable, or its channel is nil). Structural for the
	// life of the session: a channel is established at spawn or not at all.
	//
	// This is the fallback seam. A Claude session whose Stop-hook drop could
	// not be created still runs — it just has no live-turn delivery — and every
	// message aimed at it is reported here rather than accepted into a channel
	// that does not exist. The durable mailbox remains the delivery path.
	noticeDeadChannelAbsent = "harness-exposed-no-channel"

	// noticeDeadNotConsumed — the notice was offered on a pull channel and the
	// recipient never took it within interactiveNoticePullMaxPolls. The three
	// measured ways this happens on a Stop hook are all silent from the
	// session's side: the hook overran its timeout and its output was
	// discarded, the session was parked on a modal so no turn ever ended, or
	// the session went idle at an empty prompt where Stop has already fired.
	noticeDeadNotConsumed = "not-consumed"
)

// noticeChannelDrivenByRunner reports whether THIS build can actually push a
// message over the named channel.
//
// It is deliberately separate from agent.NoticeDelivery.CanDeliver, which
// answers a different question: what the HARNESS exposes. Conflating "the
// harness has a door" with "we drive that door" is precisely how a message gets
// accepted, acked and never delivered, so the two facts are kept apart and both
// are checked.
//
// This is the SEAM the per-channel lanes land on. Adding a channel means
// implementing its delivery and adding it here — in the same change, so the
// declaration and the capability move together:
//
//   - agent.NoticeDeliveryPTYNotice (shell) is driven as a PUSH, straight at
//     the terminal, because for that harness the terminal is the recipient.
//   - agent.NoticeDeliveryHook (claude-code, interactive) is driven as a PULL
//     through agent.NoticeChannel: the runner offers, the harness's Stop hook
//     collects, and the ack waits on the harness's own transcript. Nothing is
//     typed at the terminal on this path.
//   - agent.NoticeDeliveryMCPRPC (codex app-server), NoticeDeliveryHTTPSession
//     (opencode serve), NoticeDeliveryRPCSteer (pi), NoticeDeliveryACP,
//     NoticeDeliveryResumeInject and NoticeDeliveryInBoxLoop are not driven by
//     THIS leg — the headless/interview legs reach some of them through
//     Handle.Inject, which is a different rail with a different seam.
func noticeChannelDrivenByRunner(nd agent.NoticeDelivery) bool {
	return nd == agent.NoticeDeliveryPTYNotice || nd == agent.NoticeDeliveryHook
}

// noticeChannelIsPull reports whether the declared mechanism delivers by being
// COLLECTED FROM rather than written to — i.e. whether this leg drives it
// through agent.NoticeChannel instead of agent.InteractiveNotifier.
//
// Keyed off the declared mechanism, never off the harness's name. A hardcoded
// provider-name exemption on this rail has already caused one live regression
// (three harnesses could not spawn at all in a connected session), so the only
// input allowed here is the manifest's own answer.
func noticeChannelIsPull(nd agent.NoticeDelivery) bool {
	return nd == agent.NoticeDeliveryHook
}

// formatInteractiveNotice renders one inject payload as the exact byte slice
// to hand to agent.InteractiveNotifier.TryWriteNotice, or nil when the
// payload carries nothing worth showing.
//
// Three rules, each load-bearing:
//
//  1. FLATTENED. Every CR/LF (and VT/FF) becomes a single space. An
//     interactive REPL submits on a line break, so an unflattened multi-line
//     payload would submit its first line as one turn and type the rest into
//     the next prompt as separate turns. Bracketed paste is the only correct
//     multi-line framing and this layer emits none, so flattening is the
//     honest option.
//  2. BOUNDED. The complete write stays within the conservative 1,024-byte
//     canonical-mode boundary shared by the supported host environments
//     (maxInitialPromptBytes leaves the byte for the submit key), and the clip
//     lands on a UTF-8 boundary so no partial rune reaches the terminal.
//  3. SELF-SUBMITTING. Exactly one trailing interactiveNoticeSubmit (CR), so
//     the notice arrives as its own submitted TURN — not as text parked in an
//     input box — and never merges with anything else. Read that constant's
//     doc before changing this byte; CR vs LF is the difference between the
//     peer taking a turn and the peer never seeing the message.
//
// It is the PUSH path's formatter only. A pull channel carries the message as
// a structured field to the harness's own API, where none of these three rules
// applies: flattening would destroy a multi-line message the harness can carry
// perfectly well, and a trailing CR would be a stray character in a JSON
// string, not a submit key.
func formatInteractiveNotice(text string) []byte {
	text = strings.TrimSpace(flattenNoticeText(text))
	if text == "" {
		return nil
	}
	if len(text) > maxInitialPromptBytes {
		keep := maxInitialPromptBytes - len(interactiveNoticeTruncated)
		text = truncateOnRuneBoundary(text, keep) + interactiveNoticeTruncated
	}
	notice := make([]byte, 0, len(text)+1)
	notice = append(notice, text...)
	return append(notice, interactiveNoticeSubmit)
}

// flattenNoticeText maps every line-breaking byte to a space (see rule 1).
func flattenNoticeText(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\v', '\f':
			return ' '
		default:
			return r
		}
	}, s)
}

// truncateOnRuneBoundary clips s to at most limit bytes without splitting a
// rune, then trims the trailing space the clip may have exposed.
func truncateOnRuneBoundary(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimRight(cut, " ")
}

// deliverInteractiveNotice makes exactly ONE TryWriteNotice call.
//
// It deliberately does NOT route through Runner.injectDirective: that helper
// swallows agent.ErrUnsupported and returns nil, and the PTY harness answers
// exactly that for every handle — reusing it would drain the buffer, discard
// every message, and log a healthy session. Here an unsupported surface is an
// error the caller must see, and the payload is dead-lettered rather than
// dropped.
func deliverInteractiveNotice(isess agent.InteractiveSession, notice []byte) (bool, error) {
	notifier, ok := isess.(agent.InteractiveNotifier)
	if !ok {
		return false, fmt.Errorf(
			"interactive notice: %w: PTY surface does not implement TryWriteNotice",
			agent.ErrUnsupported,
		)
	}
	written, err := notifier.TryWriteNotice(notice)
	if err != nil {
		return written, fmt.Errorf("interactive notice: %w", err)
	}
	return written, nil
}

// noticeSink is the producer-facing half of the ack-or-requeue contract. The
// interactive consumer takes custody of a payload WITHOUT acking it (see
// newInjectAcceptor's ackOnBuffer) and then reports exactly one terminal fact
// about it:
//
//   - AckInject once the bytes have actually reached the PTY, or
//   - DeadLetterInject when this session establishes it will never place them.
//
// A payload that gets neither is left unacked, which is the correct outcome for
// a session that simply ended first: the producer re-offers it elsewhere.
//
// *heartbeat.Pulser implements both, nil-safely, so a supervisor running
// without a pulser (some tests) needs no special case.
type noticeSink interface {
	AckInject(deliveryID string)
	DeadLetterInject(deliveryID, reason string)
}

// interactivePulser is the narrow slice of *heartbeat.Pulser the interactive
// supervisor consumes: the two ownership signals plus delivery reporting.
// Taking the interface rather than the struct is what lets the
// ack-on-delivery contract be driven by a double in tests instead of only
// through a live HTTP heartbeat; a nil value disables all of it, which is the
// pre-existing "no pulser wired" case.
type interactivePulser interface {
	noticeSink
	LostOwnership() <-chan struct{}
	StopRequested() bool
}

// noticeOutcome is what one delivery attempt settled.
type noticeOutcome int

const (
	// noticeDelivered — the bytes reached the PTY; the slot is free and the
	// payload has been acked.
	noticeDelivered noticeOutcome = iota
	// noticeHeld — a transient refusal. The payload keeps the slot and the
	// caller re-arms the retry timer.
	noticeHeld
	// noticeDeadLettered — this session established it will never place the
	// payload. It was reported to the producer with a reason and the slot is
	// free, so nothing behind it is starved.
	noticeDeadLettered
)

// interactiveNoticeQueue holds AT MOST ONE formatted notice awaiting
// delivery. One slot, not a queue of many, on purpose:
//
//   - ORDERING. While a notice is held the supervisor stops reading injectCh,
//     so a later payload can never overtake an earlier one.
//   - BACK-PRESSURE. Unread payloads stay buffered on injectCh; once that
//     fills, the heartbeat rejects further deliveries instead of acking them,
//     which is the "leave it unacked and re-offer it" contract the producer
//     already implements. Nothing is accepted-then-lost.
//   - NO PREMATURE ACK. Nothing on the buffering path acks; the ack is issued
//     by attempt() at the instant the PTY takes the write. A notice held here
//     when the session ends was never acked, so the platform re-offers it to
//     the next session instead of stranding it acked-and-undelivered.
//
// The single slot is only safe BECAUSE it is bounded (attempts) and has an exit
// that is not delivery (dead-letter). Without both, one un-placeable payload
// holds the slot for the life of the session and every later notice — including
// agent-memory ones that have nothing to do with the stuck peer message — is
// starved behind it.
type interactiveNoticeQueue struct {
	// channel is the harness's DECLARED notice-delivery mechanism. It decides
	// whether this leg may deliver at all, and by which shape; see
	// noticeChannelDrivenByRunner and noticeChannelIsPull.
	channel agent.NoticeDelivery
	sink    noticeSink

	// text is the held payload as the PRODUCER wrote it — unflattened,
	// untruncated, no submit byte. Each transport renders it for its own
	// surface at attempt time, because the renderings are incompatible: the
	// PTY needs one flattened self-submitting line, a pull channel needs the
	// message intact.
	text       string
	deliveryID string

	// attempts counts REFUSALS and errors (the push path's bound).
	attempts int
	// polls counts waits on a pull channel that has taken the offer but not
	// yet been collected from. Deliberately not the same counter as attempts —
	// see interactiveNoticePullMaxPolls.
	polls int
	// offered records that the pull channel has custody, so the offer is not
	// rewritten on every poll.
	offered bool

	warned bool // hard write error surfaced on the Result once
}

// idle reports whether the slot is free (the supervisor may read injectCh).
func (q *interactiveNoticeQueue) idle() bool { return q.text == "" }

// undelivered reports how many notices this session never got onto the PTY:
// the one still held plus everything left buffered upstream. Reported at
// session end so an operator sees the shortfall instead of inferring it.
// Dead-lettered payloads are NOT counted — they were reported individually.
func (q *interactiveNoticeQueue) undelivered(buffered int) int {
	if !q.idle() {
		return buffered + 1
	}
	return buffered
}

// hold parks a payload in the slot.
func (q *interactiveNoticeQueue) hold(text, deliveryID string) {
	q.text, q.deliveryID = text, deliveryID
	q.attempts, q.polls, q.offered = 0, 0, false
}

// clear frees the slot.
func (q *interactiveNoticeQueue) clear() {
	q.text, q.deliveryID = "", ""
	q.attempts, q.polls, q.offered = 0, 0, false
}

// deadLetter reports the held payload as undeliverable and frees the slot.
//
// This is the observability half of the head-of-line fix: a sender that was
// told "queued" can learn the delivery died, and why, instead of watching a
// message that will never arrive stay eternally in flight.
func (q *interactiveNoticeQueue) deadLetter(r *Runner, qw QueuedWork, res *Result, reason, detail string) noticeOutcome {
	id := q.deliveryID
	r.logger.Error("[interactive] notice dead-lettered — it will never be delivered by this session",
		"sessionId", qw.SessionID, "deliveryId", id,
		"noticeDelivery", string(q.channel), "reason", reason, "detail", detail)
	res.PostSessionWarnings = append(res.PostSessionWarnings,
		fmt.Sprintf("runtime notice %s dead-lettered (%s): %s", id, reason, detail))
	if q.sink != nil {
		q.sink.DeadLetterInject(id, reason)
	}
	q.clear()
	return noticeDeadLettered
}

// attempt tries to place the held notice.
//
// Order matters: the STRUCTURAL refusals are settled first and settled
// permanently, because retrying them is theatre — the harness's declared
// channel does not change mid-session, and neither does whether the surface
// implements the notifier. Only genuinely transient conditions (the human is
// mid-composition, the child is on the alternate screen, a write failed) earn a
// retry, and even those are bounded.
func (q *interactiveNoticeQueue) attempt(
	r *Runner, qw QueuedWork, res *Result,
	isess agent.InteractiveSession, nch agent.NoticeChannel,
) noticeOutcome {
	if q.idle() {
		return noticeDelivered
	}

	// STRUCTURAL 1: the harness's declared channel is not one this build
	// drives. A PTY write here would be a keystroke into a live agent UI, so
	// it is refused outright and the message goes back to its producer.
	if !noticeChannelDrivenByRunner(q.channel) {
		return q.deadLetter(r, qw, res, noticeDeadChannelNotDriven,
			fmt.Sprintf("harness declares notice delivery %q, which this runner does not drive; "+
				"a PTY write would be a keystroke into the harness's own UI, not a message to the agent",
				declaredOrUndeclared(q.channel)))
	}

	if noticeChannelIsPull(q.channel) {
		return q.attemptPull(r, qw, res, nch)
	}
	return q.attemptPush(r, qw, res, isess)
}

// attemptPull drives one poll of a collect-from channel: offer once, then ask
// the recipient's own record whether it has been taken.
//
// The ack is issued on CONSUMPTION and nowhere else. Offer succeeding means the
// message is where the harness will look; it does not mean the harness looked,
// and on a Stop hook the two can be separated by an entire agent turn — or
// forever, if the turn never ends.
func (q *interactiveNoticeQueue) attemptPull(
	r *Runner, qw QueuedWork, res *Result, nch agent.NoticeChannel,
) noticeOutcome {
	// STRUCTURAL: the mechanism is declared and driven, but THIS session has
	// no channel. Established at spawn or not at all, so retrying is theatre.
	// The message goes back to its producer and waits in the durable mailbox
	// for the agent to pull it.
	if nch == nil {
		return q.deadLetter(r, qw, res, noticeDeadChannelAbsent,
			fmt.Sprintf("harness declares notice delivery %q but this session exposed no channel; "+
				"live-turn delivery is unavailable for its lifetime and the message stays with its producer",
				declaredOrUndeclared(q.channel)))
	}

	if !q.offered {
		if err := nch.Offer(q.deliveryID, q.text); err != nil {
			q.attempts++
			q.warnOnce(res, fmt.Sprintf("notice channel offer failed (held for retry): %v", err))
			r.logger.Warn("[interactive] notice offer failed — holding for retry",
				"sessionId", qw.SessionID, "deliveryId", q.deliveryID,
				"noticeDelivery", string(q.channel), "attempt", q.attempts, "err", err)
			return q.holdOrDeadLetter(r, qw, res, err.Error())
		}
		q.offered = true
		r.logger.Info("[interactive] notice offered on the harness's declared channel — awaiting consumption",
			"sessionId", qw.SessionID, "deliveryId", q.deliveryID,
			"noticeDelivery", string(q.channel), "bytes", len(q.text))
		// Fall through rather than returning: the harness can collect between
		// the offer and the next tick, and asking costs one read.
	}

	consumed, err := nch.Consumed()
	if err != nil {
		q.attempts++
		q.warnOnce(res, fmt.Sprintf("notice consumption check failed (held for retry): %v", err))
		r.logger.Warn("[interactive] notice consumption check failed — holding for retry",
			"sessionId", qw.SessionID, "deliveryId", q.deliveryID,
			"attempt", q.attempts, "err", err)
		return q.holdOrDeadLetter(r, qw, res, err.Error())
	}
	if consumed {
		r.logger.Info("[interactive] notice consumed by the live session",
			"sessionId", qw.SessionID, "deliveryId", q.deliveryID,
			"noticeDelivery", string(q.channel), "polls", q.polls)
		// ACK ON CONSUMPTION: the recipient's own record now shows the message
		// in its conversation. This is the first instant at which upstream may
		// call it delivered, and no earlier instant is honest.
		if q.sink != nil {
			q.sink.AckInject(q.deliveryID)
		}
		q.clear()
		return noticeDelivered
	}

	q.polls++
	if q.polls < interactiveNoticePullMaxPolls {
		return noticeHeld
	}

	// The wait is spent. Withdraw the offer first so a late collection cannot
	// deliver a message we are about to report as dead — and note that a
	// failed withdrawal is not a reason to keep waiting: retracted == false
	// means the harness already claimed it and produced no record of it
	// landing, which is exactly the discarded-output case.
	retracted, rerr := nch.Retract()
	detail := fmt.Sprintf("offered but not consumed after %d polls over ~%s (retracted=%v)",
		q.polls, time.Duration(interactiveNoticePullMaxPolls)*interactiveNoticeRetry, retracted)
	if rerr != nil {
		detail += fmt.Sprintf("; retract failed: %v", rerr)
	}
	return q.deadLetter(r, qw, res, noticeDeadNotConsumed, detail)
}

// attemptPush drives one write of a write-to channel (pty-notice).
func (q *interactiveNoticeQueue) attemptPush(
	r *Runner, qw QueuedWork, res *Result, isess agent.InteractiveSession,
) noticeOutcome {
	notice := formatInteractiveNotice(q.text)
	if notice == nil {
		// Unreachable: the supervisor drops blank payloads before holding
		// them. Settling it here rather than looping keeps the slot free.
		q.clear()
		return noticeDelivered
	}

	q.attempts++
	written, err := deliverInteractiveNotice(isess, notice)
	switch {
	// STRUCTURAL 2: pty-notice is declared but this surface cannot take one.
	case err != nil && errors.Is(err, agent.ErrUnsupported):
		return q.deadLetter(r, qw, res, noticeDeadSurfaceNotNotifier, err.Error())

	case err != nil:
		q.warnOnce(res, fmt.Sprintf("interactive notice delivery failed (held for retry): %v", err))
		r.logger.Warn("[interactive] notice write failed — holding for retry",
			"sessionId", qw.SessionID, "deliveryId", q.deliveryID,
			"attempt", q.attempts, "err", err)
		return q.holdOrDeadLetter(r, qw, res, err.Error())

	case !written:
		r.logger.Debug("[interactive] notice refused — the terminal is not at a safe moment; holding",
			"sessionId", qw.SessionID, "deliveryId", q.deliveryID, "attempt", q.attempts)
		return q.holdOrDeadLetter(r, qw, res,
			"the session refused every attempt (human mid-composition, or a full-screen UI held the terminal)")
	}

	r.logger.Info("[interactive] notice delivered to the live PTY",
		"sessionId", qw.SessionID, "deliveryId", q.deliveryID, "bytes", len(notice))
	// ACK ON DELIVERY, not on buffer: for a harness with no agent behind the
	// terminal, the PTY write IS the arrival — there is nothing further to
	// consult. Contrast attemptPull, where the same instant proves nothing.
	if q.sink != nil {
		q.sink.AckInject(q.deliveryID)
	}
	q.clear()
	return noticeDelivered
}

// warnOnce records the first hard failure of a held notice on the Result. Later
// failures of the same notice are logged but not repeated on the Result, which
// would otherwise carry one warning per retry.
func (q *interactiveNoticeQueue) warnOnce(res *Result, msg string) {
	if q.warned {
		return
	}
	q.warned = true
	res.PostSessionWarnings = append(res.PostSessionWarnings, msg)
}

// holdOrDeadLetter keeps a transiently-refused notice for another try, or
// dead-letters it once the attempt cap is spent.
func (q *interactiveNoticeQueue) holdOrDeadLetter(r *Runner, qw QueuedWork, res *Result, detail string) noticeOutcome {
	if q.attempts >= interactiveNoticeMaxAttempts {
		return q.deadLetter(r, qw, res, noticeDeadAttemptCap,
			fmt.Sprintf("%d attempts over ~%s: %s", q.attempts,
				time.Duration(interactiveNoticeMaxAttempts)*interactiveNoticeRetry, detail))
	}
	return noticeHeld
}

// declaredOrUndeclared renders a channel for an operator-facing message,
// distinguishing "the manifest said none" from "the manifest said nothing".
func declaredOrUndeclared(nd agent.NoticeDelivery) string {
	if nd == "" {
		return "<undeclared>"
	}
	return string(nd)
}

// noticeRetryClock returns the clock the interactive supervisor arms its
// retry timer from. Production uses real time; tests substitute a fake so
// the retry is driven deterministically rather than by sleeping.
func (r *Runner) noticeRetryClock() interviewClock {
	if r.interactiveNoticeClock != nil {
		return r.interactiveNoticeClock
	}
	return realInterviewClock{}
}
