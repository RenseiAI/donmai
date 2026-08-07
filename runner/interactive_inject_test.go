package runner

import (
	"bytes"
	"context"
	"errors"
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

type interactiveDispatch struct {
	session  *recordingInteractiveSession
	injectCh chan heartbeat.InjectPayload
	clock    *noticeRetryClock
	res      *Result
	done     chan error
	stop     sync.Once
}

// startInteractiveDispatch runs dispatchInteractive against a live recording
// PTY surface with a deterministic retry clock, and returns the handles a test
// needs to drive it. The caller ends the session with finish().
func startInteractiveDispatch(t *testing.T, sessionID string) *interactiveDispatch {
	t.Helper()
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	d := &interactiveDispatch{
		session:  liveRecordingInteractiveSession(),
		injectCh: make(chan heartbeat.InjectPayload, 8),
		clock:    newNoticeRetryClock(),
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
			context.Background(), handle, t.TempDir(), qw, d.res, &recordingSink{}, nil, d.injectCh,
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
		{name: "simple line gains one newline", text: "hello there", want: "hello there\n"},
		{name: "surrounding whitespace trimmed", text: "  hello  ", want: "hello\n"},
		{name: "newlines flattened to spaces", text: "line one\nline two\r\nline three", want: "line one line two  line three\n"},
		{name: "vertical whitespace flattened", text: "a\vb\fc", want: "a b c\n"},
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
			if n := bytes.Count(got, []byte("\n")); n != 1 {
				t.Fatalf("notice carries %d newlines; want exactly 1 (self-submitting, one turn)", n)
			}
			if got[len(got)-1] != '\n' {
				t.Fatalf("notice must end with the submitting newline; got %q", got)
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
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("PTY write %q must end with the submitting newline", got)
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

// TestInteractive_InjectHeldWhenSurfaceCannotNotify covers the capability
// gate. A surface without TryWriteNotice must NOT be written to through
// WriteInput (that would splice text into a live line), and the payload must
// be HELD rather than drained-and-discarded — the exact failure mode
// injectDirective would have produced by swallowing agent.ErrUnsupported.
func TestInteractive_InjectHeldWhenSurfaceCannotNotify(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	sess := &noNoticeSession{done: make(chan struct{})}
	clock := newNoticeRetryClock()
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
			context.Background(), handle, t.TempDir(), qw, res, &recordingSink{}, nil, injectCh,
		)
		done <- err
	}()

	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "first"}
	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "second"}

	clock.waitArmed(t)
	if got := sess.writeCount(); got != 0 {
		t.Fatalf("a non-notifier surface received %d raw WriteInput call(s)", got)
	}
	if got := len(injectCh); got != 1 {
		t.Fatalf("payloads were drained and discarded (buffered=%d, want 1 held upstream)", got)
	}

	close(sess.done)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatchInteractive did not return")
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
func TestInteractive_RunLoopHandsInjectChToDispatch(t *testing.T) {
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
		// The capability is what makes runLoop wire the heartbeat's OnInject;
		// it is provider-level and mode-independent, exactly as in production.
		caps:    agent.Capabilities{SupportsMessageInjection: true},
		spawned: spawned,
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

	// The child echoes "got:<line>" for every SUBMITTED line, so seeing it
	// proves the notice arrived as a turn, not merely as bytes on the master.
	if !waitForTerminalOutput(sub, "got:"+ping, 60*time.Second) {
		t.Fatal("the inject never reached the live PTY as a submitted line")
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
