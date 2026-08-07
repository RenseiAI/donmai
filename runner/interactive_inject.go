package runner

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
)

// This file is the interactive half of the runtime-inject rail.
//
// The headless path folds a buffered inject into the model as a follow-up
// turn at the post-terminal seam (drainMemoryInjects); the interview path
// parks on injectCh and resumes the session per inject. An interactive
// PTY session has neither seam — there is no turn boundary to wait for and
// no Handle.Inject to call (the PTY harness answers agent.ErrUnsupported;
// the terminal IS the input surface). So an interactive session delivers an
// inject as a NOTICE: one atomic, self-submitting line written into the live
// PTY, refusable while the human is mid-composition.
//
// Reliability posture: the notice is best-effort BY DESIGN. The durable
// record of a message lives with whoever queued it; this leg only surfaces
// it in the terminal promptly. That is what lets the composition gate refuse
// a write outright rather than corrupting a half-typed line, and what makes
// an undelivered notice a logged warning rather than a session failure.
const (
	// interactiveNoticeRetry is how long the supervisor waits before
	// re-attempting a notice that was refused (human mid-composition) or
	// whose write failed. Short enough that a notice lands within a beat of
	// the human pressing Enter; long enough to cost nothing while they type.
	interactiveNoticeRetry = 2 * time.Second

	// interactiveNoticeTruncated marks a notice clipped to the canonical-mode
	// input bound. The full text remains wherever it was durably queued.
	interactiveNoticeTruncated = " …[truncated]"
)

// formatInteractiveNotice renders one inject payload as the exact byte slice
// to hand to agent.InteractiveNotifier.TryWriteNotice, or nil when the
// payload carries nothing worth showing.
//
// Three rules, each load-bearing:
//
//  1. FLATTENED. Every CR/LF (and VT/FF) becomes a single space. An
//     interactive REPL submits on newline, so an unflattened multi-line
//     payload would submit its first line as one turn and type the rest into
//     the next prompt as separate turns. Bracketed paste is the only correct
//     multi-line framing and this layer emits none, so flattening is the
//     honest option.
//  2. BOUNDED. The complete write stays within the conservative 1,024-byte
//     canonical-mode boundary shared by the supported host environments
//     (maxInitialPromptBytes leaves the byte for the newline), and the clip
//     lands on a UTF-8 boundary so no partial rune reaches the terminal.
//  3. SELF-SUBMITTING. Exactly one trailing newline, so the notice arrives as
//     its own submitted turn and never merges with anything else.
func formatInteractiveNotice(p heartbeat.InjectPayload) []byte {
	text := strings.TrimSpace(flattenNoticeText(p.Text))
	if text == "" {
		return nil
	}
	if len(text) > maxInitialPromptBytes {
		keep := maxInitialPromptBytes - len(interactiveNoticeTruncated)
		text = truncateOnRuneBoundary(text, keep) + interactiveNoticeTruncated
	}
	notice := make([]byte, 0, len(text)+1)
	notice = append(notice, text...)
	return append(notice, '\n')
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
// error the caller must see, and the payload is held rather than dropped.
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

// interactiveNoticeQueue holds AT MOST ONE formatted notice awaiting
// delivery. One slot, not a queue of many, on purpose:
//
//   - ORDERING. While a notice is held the supervisor stops reading injectCh,
//     so a later payload can never overtake an earlier one.
//   - BACK-PRESSURE. Unread payloads stay buffered on injectCh; once that
//     fills, the heartbeat rejects further deliveries instead of acking them,
//     which is the "leave it unacked and re-offer it" contract the producer
//     already implements. Nothing is accepted-then-lost.
type interactiveNoticeQueue struct {
	pending     []byte
	deliveryID  string
	warned      bool // hard write error surfaced on the Result once
	unsupported bool // unsupported-surface logged once
}

// idle reports whether the slot is free (the supervisor may read injectCh).
func (q *interactiveNoticeQueue) idle() bool { return q.pending == nil }

// undelivered reports how many notices this session never got onto the PTY:
// the one still held plus everything left buffered upstream. Reported at
// session end so an operator sees the shortfall instead of inferring it.
func (q *interactiveNoticeQueue) undelivered(buffered int) int {
	if q.pending != nil {
		return buffered + 1
	}
	return buffered
}

// hold parks a formatted notice in the slot.
func (q *interactiveNoticeQueue) hold(notice []byte, deliveryID string) {
	q.pending, q.deliveryID = notice, deliveryID
}

// attempt tries to write the held notice. It returns true only when the PTY
// accepted it (the slot is then free again). In EVERY other case — refused
// because the human is composing, the surface cannot take notices, or the
// write failed — the notice stays held and the caller re-arms the retry.
func (q *interactiveNoticeQueue) attempt(r *Runner, qw QueuedWork, res *Result, isess agent.InteractiveSession) bool {
	if q.pending == nil {
		return true
	}
	written, err := deliverInteractiveNotice(isess, q.pending)
	switch {
	case err != nil && errors.Is(err, agent.ErrUnsupported):
		// Held, not dropped: the payload keeps its place and the buffer
		// applies back-pressure upstream instead of silently eating it.
		if !q.unsupported {
			q.unsupported = true
			r.logger.Warn("[interactive] PTY surface cannot accept runtime notices — holding",
				"sessionId", qw.SessionID, "deliveryId", q.deliveryID, "err", err)
		}
		return false
	case err != nil:
		if !q.warned {
			q.warned = true
			res.PostSessionWarnings = append(res.PostSessionWarnings,
				fmt.Sprintf("interactive notice delivery failed (held for retry): %v", err))
		}
		r.logger.Warn("[interactive] notice write failed — holding for retry",
			"sessionId", qw.SessionID, "deliveryId", q.deliveryID, "err", err)
		return false
	case !written:
		r.logger.Debug("[interactive] notice refused — human is mid-composition; holding",
			"sessionId", qw.SessionID, "deliveryId", q.deliveryID)
		return false
	}
	r.logger.Info("[interactive] notice delivered to the live PTY",
		"sessionId", qw.SessionID, "deliveryId", q.deliveryID, "bytes", len(q.pending))
	q.pending, q.deliveryID = nil, ""
	return true
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
