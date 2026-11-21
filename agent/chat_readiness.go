package agent

// This file defines the per-session semantic input readiness contract for
// interactive sessions: what "this session can take a structured chat message
// right now" actually means, as distinct from every weaker fact around it.
//
// Declared, exposed, driven and ready are four different facts and they stay
// separate:
//
//   - DECLARED is what the harness manifest says (HarnessCaps.NoticeDelivery).
//     It names a mechanism the harness exposes, not one this build drives.
//   - EXPOSED is what THIS session actually owns: a live NoticeChannel for a
//     pull mechanism, or a PTY surface implementing InteractiveNotifier for a
//     push mechanism. A harness declaration without a session channel is a
//     degraded session, not a broken one — the durable mailbox is the floor.
//   - DRIVEN is what this build implements. Today that is exactly two
//     mechanisms: pty-notice (push, shell only, where the terminal IS the
//     recipient) and hook (pull, collected by the harness's own lifecycle and
//     acked only on the recipient's own consumption record). Every other
//     declared mechanism is refused outright and reported, never typed at a
//     terminal and never acked.
//   - READY is whether THIS session can take a message NOW: ended sessions
//     cannot, modal full-screen sessions cannot take a push, and a pull
//     channel with an outstanding offer is busy. Queue acceptance is never
//     live delivery: an offered-but-unconsumed hook message is queued, and a
//     Claude hook can sit idle after Stop already fired where no turn will
//     ever end to collect it.
//
// A shell PTY notice is explicitly NOT semantic chat. It is a line the shell
// runs because there is no agent behind the terminal to route around. The
// readiness contract therefore reports it as unavailable for chat with the
// reason pty-notice-is-not-semantic-chat, even though the same bytes are a
// correct push delivery on that harness.
//
// The contract is keyed off the declared mechanism and the session's own
// capabilities, never off a harness name. A hardcoded name exemption on this
// rail has already caused one live regression, so the only inputs allowed are
// the manifest's answer and what the session exposes.
//
// Fail closed: where no public structured ingress exists for this session,
// LiveSupported is false and Reason names why. Callers leave the message with
// its producer (the durable mailbox), which is the only place it is durable.

// ChatState is the per-session input state a readiness read observes.
type ChatState string

// Stable per-session input states.
const (
	// ChatStateIdle reports a live session with no outstanding offer: it can
	// accept one. Acceptance is queueing, not delivery — delivery still
	// requires the recipient's own consumption record.
	ChatStateIdle ChatState = "idle"
	// ChatStateBusy reports a live session with an outstanding offer still
	// awaiting consumption. New messages wait behind it; the single-slot
	// rail preserves order and back-pressure.
	ChatStateBusy ChatState = "busy"
	// ChatStateModal reports a live session parked on an observable modal
	// surface (the alternate screen: a full-screen UI where every byte is a
	// command). Only push delivery observes this state; pull delivery never
	// touches the terminal so it never reports modal for an alternate screen.
	// An inline application-level modal (a permission prompt drawn as ordinary
	// text) is NOT observable here and is never reported as modal: it fails
	// closed through consumption evidence instead.
	ChatStateModal ChatState = "modal"
	// ChatStateEnded reports a session whose PTY has exited and drained. No
	// delivery is possible; held and buffered payloads stay unacked so the
	// producer re-offers them elsewhere.
	ChatStateEnded ChatState = "ended"
	// ChatStateUnknown reports a session whose liveness could not be read
	// (no PTY surface). Callers treat it as not live.
	ChatStateUnknown ChatState = "unknown"
)

// Unavailable reasons. Short, stable tokens: they ride to the producer, so
// they are wire vocabulary, not prose. The first two reuse the runner's
// dead-letter vocabulary so a readiness read and a delivery refusal agree.
const (
	// ChatReasonSessionEnded reports a session that has exited. Nothing can
	// be delivered; nothing is queued in this session.
	ChatReasonSessionEnded = "session-ended"
	// ChatReasonModalAlternateScreen reports a push session on the alternate
	// screen. A PTY write there is a keystroke into a full-screen UI.
	ChatReasonModalAlternateScreen = "modal-alternate-screen"
	// ChatReasonChannelNotDriven reports a declared mechanism this build does
	// not drive for interactive sessions. Retrying cannot help.
	ChatReasonChannelNotDriven = "channel-not-driven"
	// ChatReasonChannelAbsent reports a declared pull mechanism for which this
	// session exposed no channel. Structural for the life of the session.
	ChatReasonChannelAbsent = "harness-exposed-no-channel"
	// ChatReasonPTYNoticeNotChat reports a pty-notice session asked for
	// semantic chat. The PTY write is correct push delivery for a shell, but
	// a shell notice is a command the shell runs, not a turn the agent takes.
	ChatReasonPTYNoticeNotChat = "pty-notice-is-not-semantic-chat"
	// ChatReasonUndeclared reports a manifest that never answered the
	// notice-delivery question. Omission is denied, not defaulted.
	ChatReasonUndeclared = "undeclared-channel"
	// ChatReasonNone reports an explicit none declaration: the durable
	// mailbox is the delivery path.
	ChatReasonNone = "none"
)

// ChatReadiness is one truthful read of whether THIS session can take a
// structured chat message now.
type ChatReadiness struct {
	// Channel is the session's declared mechanism.
	Channel NoticeDelivery
	// Declared reports the manifest answered with a known mechanism.
	Declared bool
	// Exposed reports this session owns the mechanism's live seam: a
	// non-nil pull channel, or a PTY surface implementing the push seam.
	Exposed bool
	// Driven reports this build implements the mechanism for interactive
	// sessions.
	Driven bool
	// Pull reports the mechanism is collect-from (consumption-acked) rather
	// than write-to (write-acked).
	Pull bool
	// State is the session's input state at read time.
	State ChatState
	// LiveSupported reports a message offered now can reach the live turn
	// (still requiring consumption evidence before it may be called
	// delivered).
	LiveSupported bool
	// QueueSupported reports a message can be queued for this session (offer
	// accepted, durable mailbox as the floor). False only for ended sessions
	// and for pty-notice asked as chat.
	QueueSupported bool
	// Reason is empty when live, otherwise the stable token naming why not.
	Reason string
}

// Live reports whether the read found a live path.
func (r ChatReadiness) Live() bool { return r.LiveSupported }
