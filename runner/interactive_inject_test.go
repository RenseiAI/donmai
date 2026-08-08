package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// ─── deterministic retry clock ─────────────────────────────────────────────

// noticeRetryClock is a fake interviewClock for the interactive notice-retry
// timer. It is BOTH the clock and the timer (the supervisor arms exactly one),
// and it publishes every Reset on a channel so a test can wait for "the loop
// attempted delivery and re-armed" instead of sleeping.
type noticeRetryClock struct {
	mu     sync.Mutex
	ch     chan time.Time
	resets chan time.Duration
	armed  bool
}

func newNoticeRetryClock() *noticeRetryClock {
	return &noticeRetryClock{
		ch:     make(chan time.Time, 1),
		resets: make(chan time.Duration, 32),
	}
}

func (c *noticeRetryClock) NewTimer(time.Duration) interviewTimer { return c }

func (c *noticeRetryClock) Chan() <-chan time.Time { return c.ch }

func (c *noticeRetryClock) Reset(d time.Duration) {
	c.mu.Lock()
	c.armed = d > 0
	c.mu.Unlock()
	select {
	case c.resets <- d:
	default:
	}
}

func (c *noticeRetryClock) Stop() {
	c.mu.Lock()
	c.armed = false
	c.mu.Unlock()
}

// waitArmed blocks until the supervisor re-arms the retry timer — i.e. it
// attempted a delivery and the notice is still held.
func (c *noticeRetryClock) waitArmed(t *testing.T) {
	t.Helper()
	select {
	case <-c.resets:
	case <-time.After(10 * time.Second):
		t.Fatal("retry timer was never re-armed — the supervisor did not hold the notice")
	}
}

// fire trips the retry tick.
func (c *noticeRetryClock) fire(t *testing.T) {
	t.Helper()
	select {
	case c.ch <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor never consumed the retry tick")
	}
}

// ─── supervisor harness ────────────────────────────────────────────────────

// fakeInteractivePulser is an interactivePulser double that records every
// delivery acknowledgement the supervisor issues, so a test can assert WHEN
// an ack happens rather than merely that one eventually did.
type deadLetterRecord struct {
	deliveryID string
	reason     string
}

type fakeInteractivePulser struct {
	mu   sync.Mutex
	acks []string
	dead []deadLetterRecord
	lost chan struct{}
}

func newFakeInteractivePulser() *fakeInteractivePulser {
	return &fakeInteractivePulser{lost: make(chan struct{})}
}

func (p *fakeInteractivePulser) AckInject(deliveryID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acks = append(p.acks, deliveryID)
}

// DeadLetterInject records a delivery the supervisor gave up on. Recording it
// separately from acks is the point: "delivered" and "will never be delivered"
// are different facts and a test must be able to tell them apart — conflating
// them is how nine messages were reported delivered and destroyed.
func (p *fakeInteractivePulser) DeadLetterInject(deliveryID, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead = append(p.dead, deadLetterRecord{deliveryID: deliveryID, reason: reason})
}

func (p *fakeInteractivePulser) LostOwnership() <-chan struct{} { return p.lost }
func (p *fakeInteractivePulser) StopRequested() bool            { return false }

func (p *fakeInteractivePulser) deadLettered() []deadLetterRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]deadLetterRecord(nil), p.dead...)
}

// waitDeadLettered blocks until deliveryID has been dead-lettered and returns
// the reason, or fails.
func (p *fakeInteractivePulser) waitDeadLettered(t *testing.T, deliveryID string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, got := range p.deadLettered() {
			if got.deliveryID == deliveryID {
				return got.reason
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery %q was never dead-lettered; a payload this session cannot place must be "+
				"reported, not silently held forever. got %v", deliveryID, p.deadLettered())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (p *fakeInteractivePulser) acked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.acks...)
}

// waitAcked polls until deliveryID has been acked, or fails.
func (p *fakeInteractivePulser) waitAcked(t *testing.T, deliveryID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, got := range p.acked() {
			if got == deliveryID {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery %q was written to the PTY but never acked; got %v", deliveryID, p.acked())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type interactiveDispatch struct {
	session  *recordingInteractiveSession
	injectCh chan heartbeat.InjectPayload
	clock    *noticeRetryClock
	pulser   *fakeInteractivePulser
	res      *Result
	done     chan error
	stop     sync.Once
}

// startInteractiveDispatch runs dispatchInteractive against a live recording
// PTY surface that DECLARES pty-notice — the one harness shape where writing a
// notice into the terminal is the correct primitive. The caller ends the
// session with finish().
func startInteractiveDispatch(t *testing.T, sessionID string) *interactiveDispatch {
	t.Helper()
	return startInteractiveDispatchOn(t, sessionID, agent.NoticeDeliveryPTYNotice)
}

// startInteractiveDispatchOn is the same harness with the declared
// notice-delivery channel under the test's control, so the refusal path can be
// driven as directly as the delivery path.
func startInteractiveDispatchOn(t *testing.T, sessionID string, channel agent.NoticeDelivery) *interactiveDispatch {
	t.Helper()
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	d := &interactiveDispatch{
		session:  liveRecordingInteractiveSession(),
		injectCh: make(chan heartbeat.InjectPayload, 8),
		clock:    newNoticeRetryClock(),
		pulser:   newFakeInteractivePulser(),
		res:      &Result{SessionID: sessionID},
		done:     make(chan error, 1),
	}
	r := minimalRunner(t)
	r.interactiveNoticeClock = d.clock

	handle := &testInteractiveHandle{
		Handle:  &fakeHandle{events: make(chan agent.Event)},
		session: d.session,
	}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{
		SessionID: sessionID,
		Mode:      interactiveRunMode,
	}}
	go func() {
		_, err := r.dispatchInteractive(
			context.Background(), handle, t.TempDir(), qw, d.res, &recordingSink{}, d.pulser, d.injectCh, channel,
		)
		d.done <- err
	}()
	t.Cleanup(func() { d.finish(t) })
	return d
}

// finish ends the PTY session and waits for the supervisor to return.
// Idempotent: tests that assert on the terminal Result call it explicitly and
// the cleanup hook calls it again.
func (d *interactiveDispatch) finish(t *testing.T) {
	t.Helper()
	d.stop.Do(func() {
		close(d.session.done)
		select {
		case <-d.done:
		case <-time.After(10 * time.Second):
			t.Error("dispatchInteractive did not return after the session ended")
		}
	})
}

// waitWrites blocks until the PTY has recorded exactly n writes (failing on a
// timeout, or immediately if it overshoots) and returns them.
func (d *interactiveDispatch) waitWrites(t *testing.T, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		writes := d.session.recordedWrites()
		switch {
		case len(writes) == n:
			return writes
		case len(writes) > n:
			t.Fatalf("expected %d PTY write(s), got %d: %q", n, len(writes), writes)
		case time.Now().After(deadline):
			t.Fatalf("timed out waiting for %d PTY write(s); got %d: %q", n, len(writes), writes)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ─── formatInteractiveNotice ───────────────────────────────────────────────

func TestFormatInteractiveNotice(t *testing.T) {
	longLine := strings.Repeat("x", 4000)
	multibyte := strings.Repeat("あ", 1000) // 3 bytes per rune

	tests := []struct {
		name    string
		text    string
		wantNil bool
		want    string // exact expected notice (when short enough to state)
	}{
		{name: "empty", text: "", wantNil: true},
		{name: "whitespace only", text: "   \n\t ", wantNil: true},
		{name: "simple line gains one submit byte", text: "hello there", want: "hello there\r"},
		{name: "surrounding whitespace trimmed", text: "  hello  ", want: "hello\r"},
		{name: "newlines flattened to spaces", text: "line one\nline two\r\nline three", want: "line one line two  line three\r"},
		{name: "vertical whitespace flattened", text: "a\vb\fc", want: "a b c\r"},
		{name: "oversize line truncated", text: longLine},
		{name: "oversize multibyte truncated on a rune boundary", text: multibyte},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatInteractiveNotice(heartbeat.InjectPayload{DeliveryID: "d1", Text: tc.text})
			if tc.wantNil {
				if got != nil {
					t.Fatalf("formatInteractiveNotice = %q; want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("formatInteractiveNotice returned nil for non-empty text")
			}
			if tc.want != "" && string(got) != tc.want {
				t.Fatalf("formatInteractiveNotice = %q; want %q", got, tc.want)
			}

			// Invariants that hold for EVERY non-nil notice.
			if n := bytes.Count(got, []byte{interactiveNoticeSubmit}); n != 1 {
				t.Fatalf("notice carries %d submit bytes; want exactly 1 (self-submitting, one turn)", n)
			}
			if n := bytes.Count(got, []byte("\n")); n != 0 {
				t.Fatalf("notice carries %d LF bytes; a raw-mode TUI reads LF as a key that is NOT Return", n)
			}
			if got[len(got)-1] != interactiveNoticeSubmit {
				t.Fatalf("notice must end with the submit byte; got %q", got)
			}
			if len(got) > maxInitialPromptBytes+1 {
				t.Fatalf("notice is %d bytes; the canonical-mode bound is %d", len(got), maxInitialPromptBytes+1)
			}
			if !utf8.Valid(got) {
				t.Fatalf("notice is not valid UTF-8 (a rune was split): %q", got)
			}
			if len(tc.text) > maxInitialPromptBytes && !bytes.Contains(got, []byte(interactiveNoticeTruncated)) {
				t.Fatalf("an oversize notice must be marked truncated; got %q", got)
			}
		})
	}
}

// ─── the submit byte, against a raw-mode reader ────────────────────────────

// spawnRawKeys re-execs this test binary as the raw-mode key reader (see
// main_test.go) under a real PTY and returns it with a subscription
// positioned after the readiness marker.
func spawnRawKeys(t *testing.T) (*ptyhost.Session, agent.InteractiveSubscription) {
	t.Helper()
	sess, err := ptyhost.Spawn(ptyhost.Spec{
		Command: []string{os.Args[0]},
		Env:     []string{testRoleEnv + "=rawkeys"},
		// The fixture stands in for the shell shape — the one harness whose
		// declaration permits a PTY notice at all.
		NoticeDelivery: agent.NoticeDeliveryPTYNotice,
	})
	if err != nil {
		t.Fatalf("ptyhost.Spawn(raw key reader): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sess.Stop(ctx)
	})
	sub, err := sess.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	if !waitForTerminalOutput(sub, rawKeysReady, 30*time.Second) {
		t.Fatal("the raw-mode key reader never signalled readiness")
	}
	return sess, sub
}

// TestInteractiveNotice_SubmitByteDistinguishesReturnFromCtrlJ closes the gap
// the reviewers named: every other harness in this suite is an environment
// where LF works trivially (a cooked-mode `read` loop, or `stty raw -echo;
// exec cat`, which echoes and interprets nothing), so none of them can tell
// "the bytes reached the PTY" from "the message arrived as a TURN".
//
// This one drives a raw-mode reader that treats CR and LF as DIFFERENT KEYS —
// which is what a raw-mode TUI's keypress parser does — and asserts that the
// notice the runner actually builds commits a line there, while the previously
// shipped LF-terminated form does not.
//
// STILL UNVERIFIED, stated plainly: this proves the byte we send is the byte
// the application receives and that the two are separable. It does not, and
// in this environment cannot, prove any specific third-party REPL's key
// bindings. The argument for CR is that a terminal emulator emits CR for the
// Return key (LF is Ctrl-J), so CR is what a Return-bound app is waiting for,
// and ICRNL makes CR work for cooked-mode children too — which the sibling
// runLoop test exercises end to end.
func TestInteractiveNotice_SubmitByteDistinguishesReturnFromCtrlJ(t *testing.T) {
	const text = "PING-4c1e from chief-of-staff"

	tests := []struct {
		name       string
		notice     []byte
		wantMarker string
		wantSubmit bool
	}{
		{
			name:       "the notice the runner builds submits a turn",
			notice:     formatInteractiveNotice(heartbeat.InjectPayload{DeliveryID: "d1", Text: text}),
			wantMarker: rawKeysSubmit + text,
			wantSubmit: true,
		},
		{
			name:       "an LF-terminated notice is a different key and never submits",
			notice:     append([]byte(text), '\n'),
			wantMarker: rawKeysNotSubmit + "0x0a",
			wantSubmit: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess, sub := spawnRawKeys(t)

			written, err := sess.TryWriteNotice(tc.notice)
			if err != nil {
				t.Fatalf("TryWriteNotice: %v", err)
			}
			if !written {
				t.Fatal("the raw-mode reader refused the notice at an idle prompt")
			}
			if !waitForTerminalOutput(sub, tc.wantMarker, 30*time.Second) {
				t.Fatalf("the raw-mode reader never reported %q", tc.wantMarker)
			}
			if tc.wantSubmit {
				return
			}
			// The non-submit marker is emitted only AFTER every text byte has
			// been consumed, so its arrival proves no turn was committed.
			if waitForTerminalOutput(sub, rawKeysSubmit, 500*time.Millisecond) {
				t.Fatal("an LF-terminated notice committed a turn; the fixture cannot tell CR from LF")
			}
		})
	}
}

// ─── the drain itself ──────────────────────────────────────────────────────

// TestInteractive_InjectDeliveredAsSingleWrite is the core regression for the
// production defect: an interactive session buffered injects on injectCh and
// NOBODY read them, so the first 8 messages were accepted, acked, and lost.
// It must arrive, and it must arrive as exactly ONE PTY write.
func TestInteractive_InjectDeliveredAsSingleWrite(t *testing.T) {
	d := startInteractiveDispatch(t, "sess-notice-single")

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "PING-9f2a from chief-of-staff"}

	writes := d.waitWrites(t, 1)
	got := string(writes[0])
	if !strings.Contains(got, "PING-9f2a from chief-of-staff") {
		t.Fatalf("PTY write %q does not carry the inject text", got)
	}
	if !strings.HasSuffix(got, string(interactiveNoticeSubmit)) {
		t.Fatalf("PTY write %q must end with the submit byte", got)
	}
	if len(writes[0]) > maxInitialPromptBytes+1 {
		t.Fatalf("PTY write is %d bytes; bound is %d", len(writes[0]), maxInitialPromptBytes+1)
	}
}

// TestInteractive_InjectSkipsEmptyPayload keeps an empty/whitespace inject off
// the PTY entirely — it would submit a bare newline as a turn.
func TestInteractive_InjectSkipsEmptyPayload(t *testing.T) {
	d := startInteractiveDispatch(t, "sess-notice-empty")

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-empty", Text: "  \n\t "}
	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-real", Text: "real message"}

	writes := d.waitWrites(t, 1)
	if !strings.Contains(string(writes[0]), "real message") {
		t.Fatalf("expected only the real message on the PTY; got %q", writes[0])
	}
}

// TestInteractive_InjectRefusedWhileHumanComposing is the typing-safety
// contract: while the human has unsubmitted bytes in the line editor the
// notice is NOT written, is NOT dropped, and lands on a later retry once the
// line has been submitted.
func TestInteractive_InjectRefusedWhileHumanComposing(t *testing.T) {
	d := startInteractiveDispatch(t, "sess-notice-composing")
	d.session.setRefuseNotice(true)

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-hold", Text: "message during typing"}

	// The supervisor attempted delivery, was refused, and re-armed the retry.
	d.clock.waitArmed(t)
	if got := d.session.recordedWrites(); len(got) != 0 {
		t.Fatalf("notice was written while the human was composing: %q", got)
	}

	// A retry while STILL composing must not write either.
	d.clock.fire(t)
	d.clock.waitArmed(t)
	if got := d.session.recordedWrites(); len(got) != 0 {
		t.Fatalf("notice was written on retry while still composing: %q", got)
	}

	// Human submits: the very next retry delivers the held notice.
	d.session.setRefuseNotice(false)
	d.clock.fire(t)
	writes := d.waitWrites(t, 1)
	if !strings.Contains(string(writes[0]), "message during typing") {
		t.Fatalf("held notice did not land after the human submitted: %q", writes[0])
	}
}

// TestInteractive_AckHappensOnDeliveryNotOnBuffer is the ack-or-requeue
// contract, at the only instant that can honour it.
//
// The regression: the payload was acked the moment it landed on the inject
// channel. A notice held for a composing human is not delivered, so acking
// there told the producer "delivered" about bytes that had not left the
// process — and once the producer stamps it acked, it is never re-offered.
func TestInteractive_AckHappensOnDeliveryNotOnBuffer(t *testing.T) {
	d := startInteractiveDispatch(t, "sess-ack-on-delivery")
	d.session.setRefuseNotice(true)

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-ack", Text: "message during typing"}

	// Attempted, refused, held — and NOT acked.
	d.clock.waitArmed(t)
	if got := d.pulser.acked(); len(got) != 0 {
		t.Fatalf("payload acked while it was only buffered/held: %v", got)
	}
	d.clock.fire(t)
	d.clock.waitArmed(t)
	if got := d.pulser.acked(); len(got) != 0 {
		t.Fatalf("payload acked on a retry that still wrote nothing: %v", got)
	}

	// The human submits; the write lands; NOW it may be acked.
	d.session.setRefuseNotice(false)
	d.clock.fire(t)
	d.waitWrites(t, 1)
	d.pulser.waitAcked(t, "dlv-ack")
}

// TestInteractive_UndeliveredNoticeIsNeverAckedAtSessionEnd is the other half
// of the same contract: a notice the session never managed to write must stay
// unacked so the producer re-offers it somewhere else. Acking it here is
// exactly how nine messages were destroyed with a success reported to each
// sender.
func TestInteractive_UndeliveredNoticeIsNeverAckedAtSessionEnd(t *testing.T) {
	d := startInteractiveDispatch(t, "sess-ack-session-end")
	d.session.setRefuseNotice(true)

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-lost-1", Text: "never delivered"}
	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-lost-2", Text: "also never delivered"}
	d.clock.waitArmed(t)

	d.finish(t)

	if got := d.pulser.acked(); len(got) != 0 {
		t.Fatalf("undelivered notices were acked at session end: %v — the producer will never re-offer them", got)
	}
	if got := d.session.recordedWrites(); len(got) != 0 {
		t.Fatalf("expected no PTY writes at all; got %q", got)
	}
}

// TestInteractive_InjectHoldsOrderAcrossARefusal proves the single-slot
// discipline: while one notice is held the supervisor does not consume the
// next payload (so nothing can overtake it), and both are delivered in order
// once the PTY accepts writes again.
func TestInteractive_InjectHoldsOrderAcrossARefusal(t *testing.T) {
	d := startInteractiveDispatch(t, "sess-notice-order")
	d.session.setRefuseNotice(true)

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "A2A-SEQ-01"}
	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "A2A-SEQ-02"}

	d.clock.waitArmed(t)
	if got := len(d.injectCh); got != 1 {
		t.Fatalf("second payload was consumed while the first was held (buffered=%d, want 1)", got)
	}

	d.session.setRefuseNotice(false)
	d.clock.fire(t)

	writes := d.waitWrites(t, 2)
	if !strings.Contains(string(writes[0]), "A2A-SEQ-01") || !strings.Contains(string(writes[1]), "A2A-SEQ-02") {
		t.Fatalf("notices arrived out of order: %q then %q", writes[0], writes[1])
	}
}

// TestInteractive_InjectSurfacesAHardWriteError keeps a failed write visible:
// it is surfaced on the Result and the notice is held for retry rather than
// swallowed the way injectDirective swallows agent.ErrUnsupported.
func TestInteractive_InjectSurfacesAHardWriteError(t *testing.T) {
	d := startInteractiveDispatch(t, "sess-notice-writeerr")
	d.session.mu.Lock()
	d.session.noticeErr = errors.New("pty master closed")
	d.session.mu.Unlock()

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-err", Text: "will not land"}
	d.clock.waitArmed(t)

	// Recover the surface; the held notice must still be delivered.
	d.session.mu.Lock()
	d.session.noticeErr = nil
	d.session.mu.Unlock()
	d.clock.fire(t)
	d.waitWrites(t, 1)

	d.finish(t)
	if len(d.res.PostSessionWarnings) == 0 {
		t.Fatal("a failed notice write must be surfaced on the Result")
	}
	if !strings.Contains(strings.Join(d.res.PostSessionWarnings, "|"), "pty master closed") {
		t.Fatalf("warning does not carry the write error: %v", d.res.PostSessionWarnings)
	}
}

// noNoticeSession is an agent.InteractiveSession that is deliberately NOT an
// agent.InteractiveNotifier — the shape of a PTY surface that predates the
// capability.
type noNoticeSession struct {
	agent.InteractiveSession
	mu     sync.Mutex
	writes int
	done   chan struct{}
}

func (s *noNoticeSession) WriteInput(p []byte) (int, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return len(p), nil
}
func (s *noNoticeSession) Done() <-chan struct{} { return s.done }
func (s *noNoticeSession) Exit() (attachwire.ExitPayload, bool) {
	return attachwire.NewNormalExit(0), true
}

func (s *noNoticeSession) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// TestInteractive_SurfaceThatCannotNotifyIsDeadLetteredNotHeld covers the
// surface half of the capability gate.
//
// Two properties, and the second is the one that changed. A surface without
// TryWriteNotice must NOT be written to through WriteInput — that would splice
// runner text into a live line. And the payload must be DEAD-LETTERED rather
// than held forever: "this surface cannot take notices" does not become true
// later, so retrying it is theatre, and holding it occupies the single in-flight
// slot for the life of the session, starving every payload behind it. Neither
// path may ack.
func TestInteractive_SurfaceThatCannotNotifyIsDeadLetteredNotHeld(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	sess := &noNoticeSession{done: make(chan struct{})}
	clock := newNoticeRetryClock()
	pulser := newFakeInteractivePulser()
	r := minimalRunner(t)
	r.interactiveNoticeClock = clock

	injectCh := make(chan heartbeat.InjectPayload, 8)
	handle := &testInteractiveHandle{
		Handle:  &fakeHandle{events: make(chan agent.Event)},
		session: sess,
	}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "sess-no-notifier", Mode: interactiveRunMode}}
	res := &Result{SessionID: qw.SessionID}
	done := make(chan error, 1)
	go func() {
		_, err := r.dispatchInteractive(
			context.Background(), handle, t.TempDir(), qw, res, &recordingSink{}, pulser, injectCh,
			agent.NoticeDeliveryPTYNotice,
		)
		done <- err
	}()

	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "first"}
	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "second"}

	if reason := pulser.waitDeadLettered(t, "dlv-1"); reason != noticeDeadSurfaceNotNotifier {
		t.Fatalf("dead-letter reason = %q; want %q", reason, noticeDeadSurfaceNotNotifier)
	}
	// The slot freed, so the SECOND payload was consumed and settled too — the
	// head-of-line block is gone.
	pulser.waitDeadLettered(t, "dlv-2")
	if got := sess.writeCount(); got != 0 {
		t.Fatalf("a non-notifier surface received %d raw WriteInput call(s)", got)
	}
	if got := pulser.acked(); len(got) != 0 {
		t.Fatalf("an undeliverable payload was acked: %v", got)
	}

	close(sess.done)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatchInteractive did not return")
	}
}

// TestInteractive_UndrivenChannelRefusesWithoutWritingOrAcking is the ruling in
// one test: PTY writes are reserved for harnesses with no agent behind them.
//
// A claude or codex interactive session declares its own application-level
// channel (hook / mcp-rpc). This runner does not drive either, and a PTY write
// there is a keystroke into a live agent UI where the submit byte selects
// whatever option is drawn. So the supervisor must write NOTHING, ack NOTHING,
// and say why — the message stays with its producer, which is the only place it
// is durable.
func TestInteractive_UndrivenChannelRefusesWithoutWritingOrAcking(t *testing.T) {
	channels := []agent.NoticeDelivery{
		agent.NoticeDeliveryHook,        // claude-code
		agent.NoticeDeliveryMCPRPC,      // codex
		agent.NoticeDeliveryHTTPSession, // opencode
		agent.NoticeDeliveryRPCSteer,    // pi
		agent.NoticeDeliveryNone,        // antigravity / ollama
		"",                              // a manifest that never answered
	}
	for _, channel := range channels {
		t.Run(declaredOrUndeclared(channel), func(t *testing.T) {
			d := startInteractiveDispatchOn(t, "sess-undriven", channel)

			d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-undriven", Text: "hello peer"}

			if reason := d.pulser.waitDeadLettered(t, "dlv-undriven"); reason != noticeDeadChannelNotDriven {
				t.Fatalf("dead-letter reason = %q; want %q", reason, noticeDeadChannelNotDriven)
			}
			if got := d.session.recordedWrites(); len(got) != 0 {
				t.Fatalf("bytes were written into a session whose harness declares %q: %q", channel, got)
			}
			if got := d.pulser.acked(); len(got) != 0 {
				t.Fatalf("a refused delivery was acked: %v — the producer will never re-offer it", got)
			}
		})
	}
}

// TestInteractive_AttemptCapDeadLettersAndUnblocksTheQueue is the head-of-line
// regression.
//
// The rail allows ONE notice in flight per session. With no attempt cap and no
// exit other than success, a single payload the session can never place — the
// human walked away mid-line, the child is parked in a pager — holds that slot
// for the life of the session and starves everything behind it, including
// agent-memory injects that have nothing to do with the stuck message. Nobody
// upstream can see it happen.
//
// The cap gives the slot an exit that is not delivery, and the dead letter makes
// it visible.
func TestInteractive_AttemptCapDeadLettersAndUnblocksTheQueue(t *testing.T) {
	d := startInteractiveDispatch(t, "sess-attempt-cap")
	d.session.setRefuseNotice(true)

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-stuck", Text: "never placeable"}
	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-behind", Text: "starved behind it"}

	// Burn the cap. The first attempt happens on receipt, so the remaining
	// attempts ride the retry timer.
	d.clock.waitArmed(t)
	for i := 1; i < interactiveNoticeMaxAttempts; i++ {
		d.clock.fire(t)
		if i < interactiveNoticeMaxAttempts-1 {
			d.clock.waitArmed(t)
		}
	}

	if reason := d.pulser.waitDeadLettered(t, "dlv-stuck"); reason != noticeDeadAttemptCap {
		t.Fatalf("dead-letter reason = %q; want %q", reason, noticeDeadAttemptCap)
	}
	// The queue moved on: the payload behind it got its own turn instead of
	// waiting on a slot that would never free. It is attempted (and refused,
	// the surface is still shut) the moment the slot frees, so wait for that
	// attempt before letting the surface accept and firing its retry.
	d.clock.waitArmed(t)
	d.session.setRefuseNotice(false)
	d.clock.fire(t)
	writes := d.waitWrites(t, 1)
	if !strings.Contains(string(writes[0]), "starved behind it") {
		t.Fatalf("the payload behind the stuck one never got a turn; wrote %q", writes[0])
	}
	if got := d.pulser.acked(); len(got) != 1 || got[0] != "dlv-behind" {
		t.Fatalf("acks = %v; want exactly the delivered payload", got)
	}
}

// ─── the runLoop seam ──────────────────────────────────────────────────────

// TestInteractive_RunLoopHandsInjectChToDispatch drives the FULL runner:
// runLoop spawns a real PTY child, the heartbeat piggybacks an inject on a
// lock-refresh response, and the child must read the text as a submitted line.
//
// This is the seam the production defect lived on — runLoop returned via
// dispatchInteractive without passing injectCh — so it fails outright if the
// channel is not handed over, no matter how correct the drain itself is.
//
// It runs over BOTH provider injection capabilities. An interactive session
// never calls Handle.Inject; it writes into its PTY, and every interactive
// session has one. Gating the rail on the provider capability silently
// disabled it for the harnesses that declare SupportsMessageInjection=false
// (codex, shell, amp, agycli, ollama) while the platform still reported the
// message delivered — so the capability=false row is the regression.
func TestInteractive_RunLoopHandsInjectChToDispatch(t *testing.T) {
	tests := []struct {
		name          string
		supportsInjec bool
	}{
		{name: "provider supports message injection", supportsInjec: true},
		{name: "provider does NOT support message injection", supportsInjec: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runInteractiveInjectViaRunLoop(t, tc.supportsInjec, agent.NoticeDeliveryPTYNotice)
		})
	}
}

func runInteractiveInjectViaRunLoop(t *testing.T, supportsInjection bool, channel agent.NoticeDelivery) {
	t.Helper()
	requireSh(t)

	const (
		sessionID = "sess-runloop-inject"
		ping      = "PING-runloop-4c1e"
	)

	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")
	t.Setenv(envAttachTokenFile, "")

	platform := newRecordingPlatformServer(t)
	platform.queueInject(heartbeat.InjectPayload{DeliveryID: "dlv-runloop-1", Text: ping})

	bareRepo := makeBareRepo(t)
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: platform.URL,
		WorkerID:    "w1",
		AuthToken:   "tok",
		HTTPClient:  platform.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}

	reg := NewRegistry()
	spawned := make(chan *ptyhost.Session, 1)
	prov := &interactivePTYProvider{
		command: []string{
			"/bin/sh", "-c",
			`while IFS= read -r line; do echo "got:$line"; [ "$line" = quit ] && break; done`,
		},
		// The PROVIDER capability answers "can Handle.Inject deliver a
		// message?", which an interactive session never asks: its delivery
		// surface is the PTY. Both values must therefore behave identically
		// here.
		caps: agent.Capabilities{SupportsMessageInjection: supportsInjection},
		// The harness's DECLARED channel. runLoop reads it off the manifest,
		// so a regression that stops plumbing the declaration fails here too.
		noticeDelivery: channel,
		spawned:        spawned,
	}
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	r, err := New(Options{
		Registry:           reg,
		WorktreeManager:    wtm,
		Poster:             poster,
		HTTPClient:         platform.Client(),
		HeartbeatInterval:  50 * time.Millisecond,
		MaxSessionDuration: -1,
		SkipBackstop:       true,
		SkipSteering:       true,
		SkipPostSession:    true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	qw := QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID:       sessionID,
			IssueID:         "issue-runloop-inject",
			IssueIdentifier: "INT-RUNLOOP",
			WorkType:        "development",
			Body:            "interactive session",
			Mode:            interactiveRunMode,
			InitialPrompt:   "seed",
			Repository:      bareRepo,
		},
		WorkerID:        "w1",
		AuthToken:       "tok",
		PlatformURL:     platform.URL,
		ResolvedProfile: ResolvedProfile{Provider: agent.ProviderShell},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resCh := make(chan *Result, 1)
	go func() {
		res, _ := r.Run(ctx, qw)
		resCh <- res
	}()

	var sess *ptyhost.Session
	select {
	case sess = <-spawned:
	case <-time.After(60 * time.Second):
		t.Fatal("the interactive provider never spawned a PTY session")
	}
	sub, err := sess.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	if channel == agent.NoticeDeliveryPTYNotice {
		// The child echoes "got:<line>" for every SUBMITTED line, so seeing it
		// proves the notice arrived as a turn, not merely as bytes on the master.
		if !waitForTerminalOutput(sub, "got:"+ping, 60*time.Second) {
			t.Fatal("the inject never reached the live PTY as a submitted line")
		}
	} else {
		// An undriven channel: the bytes must NOT appear, and the refusal must
		// ride the real wire as a dead letter rather than an ack.
		if waitForTerminalOutput(sub, "got:"+ping, 3*time.Second) {
			t.Fatalf("the inject was typed into a session whose harness declares %q", channel)
		}
		waitFor(t, 30*time.Second, "the dead letter never reached the platform", func() bool {
			_, dead := platform.injectReports()
			return len(dead) > 0
		})
		acks, dead := platform.injectReports()
		if len(acks) != 0 {
			t.Fatalf("an undeliverable inject was acked to the platform: %v — it is now destroyed, "+
				"because acked_at is what stops it being re-offered", acks)
		}
		if dead[0] != "dlv-runloop-1:"+noticeDeadChannelNotDriven {
			t.Fatalf("dead letter = %q; want the delivery id and the channel-not-driven reason", dead[0])
		}
	}

	if _, err := sess.WriteInput([]byte("quit\n")); err != nil {
		t.Fatalf("WriteInput quit: %v", err)
	}
	select {
	case res := <-resCh:
		if res.Status != "completed" {
			t.Fatalf("Run status=%q error=%q; want completed", res.Status, res.Error)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Run did not finish after the interactive child exited")
	}
}

// waitForTerminalOutput drains a live PTY subscription until want appears in
// the accumulated Output bytes.
func waitForTerminalOutput(sub agent.InteractiveSubscription, want string, d time.Duration) bool {
	var seen []byte
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				return false
			}
			if f.Type != attachwire.TypeOutput {
				continue
			}
			seen = append(seen, attachwire.DecodeOutput(f.Payload).Data...)
			if bytes.Contains(seen, []byte(want)) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// waitFor polls cond until it holds, or fails with msg.
func waitFor(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestInteractive_RunLoopNeverAcksAnUndeliverableInject drives the FULL runner
// over the seam that decides WHERE the ack happens.
//
// The rail acked the instant a payload landed on the 8-slot channel. For an
// interactive session that is a lie by construction: the write waits on the
// terminal and may never happen at all. Here it can never happen — the harness
// declares a channel this build does not drive — so the platform must see a
// dead letter and NEVER an ackedInject echo.
//
// It is deliberately end-to-end over the real HTTP heartbeat: the wiring
// (runLoop choosing ack-on-delivery for interactive), the refusal (the consumer
// declining a channel it cannot drive) and the report (the dead letter on the
// wire) are three separate places a regression can hide, and only a test that
// crosses all three catches a change to any one of them.
func TestInteractive_RunLoopNeverAcksAnUndeliverableInject(t *testing.T) {
	runInteractiveInjectViaRunLoop(t, true, agent.NoticeDeliveryHook)
}
