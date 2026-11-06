package runner

import (
	"github.com/RenseiAI/donmai/agent"
)

// This file is the per-session semantic input readiness read for interactive
// sessions: the truthful answer to "can THIS session take a structured chat
// message right now", kept separate from every weaker fact around it.
//
// Declared (the manifest), exposed (this session's live seam), driven (what
// this build implements) and ready (this session right now) are four facts.
// Conflating any two is how a message gets accepted, acked and never
// delivered, so this read reports all four and derives live/queue from them.
//
// Supported versions and public ingress interfaces inspected for the current
// interactive handles (the bounded first packet):
//
//   - shell: NoticeDeliveryPTYNotice over agent.InteractiveNotifier.
//     The terminal IS the recipient (no agent behind it). Correct push
//     delivery, but explicitly NOT semantic chat: readiness reports
//     pty-notice-is-not-semantic-chat for chat.
//   - claude-code interactive: NoticeDeliveryHook over agent.NoticeChannel.
//     The harness calls out at Stop; the runner offers, the hook collects,
//     and the ack waits on the harness's own transcript. Multiline is
//     preserved verbatim and the delivery id is stable; nothing is typed.
//     This is the first safe driven structured channel, and the runnable
//     proof below pins it.
//   - codex interactive: declares NoticeDeliveryMCPRPC (app-server JSON-RPC).
//     The interactive PTY owns no same-session RPC the runner may steer
//     through: headless Handle.Inject is ErrUnsupported and the named PTY
//     bootstrap server exists only for thread naming/attach, not for live
//     steering. Undriven here; fail closed.
//   - pi interactive: declares NoticeDeliveryRPCSteer (steer/follow_up over
//     `pi --mode rpc` JSONL). The interactive TUI (`bare pi`) runs no RPC
//     channel, so the headless Inject rail belongs to a different
//     session/process. Undriven here; fail closed. Its interactive Inject
//     already refuses with ErrUnsupported.
//   - opencode: declares NoticeDeliveryHTTPSession (POST /api/session/:id/
//     prompt on a live serve session). There is no interactive spawn mode to
//     own that server in this build, so no same-session ownership exists.
//     Undriven here; fail closed.
//   - muse interactive: declares NoticeDeliveryMCPRPC (serve-host turn/steer).
//     The interactive TUI (`bare muse`) starts no serve host, so the
//     headless turn/steer rail belongs to a different process. Undriven here;
//     fail closed.
//   - gemini-direct / ollama / stub in-box loop: Handle.Inject appends inside
//     this process for the headless loop, but no interactive spawn mode owns
//     that loop in the same session/process. Undriven interactively; fail
//     closed. The stub interactive PTY is a deterministic fake agent with no
//     tool surface, not an in-box loop.
//   - resume-inject / none / undeclared: no live channel by declaration.
//     The durable mailbox is the path.
//
// Agent-sized follow-ons (not driven here, one lane each): a codex lane
// proving same-session app-server steer ownership without breaking thread
// naming; a pi lane proving a TUI-owned RPC steer channel exists; an opencode
// lane proving interactive serve ownership; a muse lane proving TUI-owned
// turn/steer ownership; an ACP lane for a future CLI-wrapping harness. Each
// follow-on must land its delivery with its own consumption evidence in the
// same change that flips it driven.
//
// No provider-name switch appears here or anywhere on this rail: the only
// inputs are the declared mechanism, what the session exposes, and what this
// build drives. A hardcoded name exemption has already caused one live
// regression on this rail.

// alternateScreenReporter is the narrow observable modal seam: a session that
// can report its alternate-screen state. ptyhost.Session and ptycli.Handle
// implement it; test doubles may or may not. Absence means "not modal".
type alternateScreenReporter interface {
	AlternateScreenActive() bool
}

// interactiveChatReadiness reads one truthful readiness for the session the
// supervisor currently owns. queueBusy reports whether the single-slot notice
// queue holds an outstanding offer (busy) or is free (idle).
//
// Rules, in order:
//
//   - Ended wins: a session whose PTY has exited takes nothing. Both live and
//     queue are false; held payloads stay unacked for requeue elsewhere.
//   - Push modal wins for push only: an alternate-screen PTY refuses a PTY
//     write. Pull delivery never touches the terminal so it never reports
//     modal for an alternate screen; its inline modals are invisible here by
//     construction and fail closed through consumption evidence instead.
//   - pty-notice asked as chat is unavailable even when push delivery would
//     succeed: a shell line is a command the shell runs, not a turn the agent
//     takes.
//   - Undriven mechanisms and declared-but-absent pull channels are
//     unavailable for live delivery. Queue stays true (the durable mailbox is
//     the floor) so the producer keeps the message; live stays false so
//     nothing is typed, restarted, replayed, or acked without evidence.
func interactiveChatReadiness(
	channel agent.NoticeDelivery,
	isess agent.InteractiveSession,
	nch agent.NoticeChannel,
	queueBusy bool,
) agent.ChatReadiness {
	declared := channel.Declared()
	driven := noticeChannelDrivenByRunner(channel)
	pull := noticeChannelIsPull(channel)

	exposed := false
	if pull {
		exposed = nch != nil
	} else if channel == agent.NoticeDeliveryPTYNotice {
		_, exposed = isess.(agent.InteractiveNotifier)
	}

	state := agent.ChatStateIdle
	switch {
	case isInteractiveSessionEnded(isess):
		state = agent.ChatStateEnded
	case !pull && isAlternateScreenActive(isess):
		state = agent.ChatStateModal
	case queueBusy:
		state = agent.ChatStateBusy
	case isess == nil:
		state = agent.ChatStateUnknown
	}

	readiness := agent.ChatReadiness{
		Channel:  channel,
		Declared: declared,
		Exposed:  exposed,
		Driven:   driven,
		Pull:     pull,
		State:    state,
	}

	switch {
	case state == agent.ChatStateEnded:
		readiness.Reason = agent.ChatReasonSessionEnded
	case state == agent.ChatStateModal:
		readiness.Reason = agent.ChatReasonModalAlternateScreen
	case channel == agent.NoticeDeliveryPTYNotice:
		// Push delivery may still succeed on this session, but semantic chat
		// did not: the terminal is the shell's input, not an agent turn.
		readiness.Reason = agent.ChatReasonPTYNoticeNotChat
	case channel == agent.NoticeDeliveryNone:
		readiness.Reason = agent.ChatReasonNone
	case !declared:
		readiness.Reason = agent.ChatReasonUndeclared
	case !driven:
		readiness.Reason = agent.ChatReasonChannelNotDriven
	case !exposed:
		readiness.Reason = agent.ChatReasonChannelAbsent
	default:
		readiness.LiveSupported = true
		readiness.QueueSupported = true
		return readiness
	}

	// Unavailable for live delivery. Queue stays true wherever the durable
	// mailbox can still carry the message: every non-ended session except a
	// pty-notice asked as chat (no agent exists to pull it).
	if state != agent.ChatStateEnded && channel != agent.NoticeDeliveryPTYNotice {
		readiness.QueueSupported = true
	}
	return readiness
}

// isInteractiveSessionEnded reports Done-closed without blocking. A nil
// session or a nil Done reads as ended: there is no live surface to deliver
// to, so fail closed.
func isInteractiveSessionEnded(isess agent.InteractiveSession) bool {
	if isess == nil {
		return true
	}
	done := isess.Done()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// isAlternateScreenActive reports the observable full-screen state. Sessions
// that do not implement the reporter read as not modal.
func isAlternateScreenActive(isess agent.InteractiveSession) bool {
	if reporter, ok := isess.(alternateScreenReporter); ok && reporter != nil {
		return reporter.AlternateScreenActive()
	}
	return false
}
