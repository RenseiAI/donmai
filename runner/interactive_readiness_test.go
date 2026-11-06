package runner

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	claudeprovider "github.com/RenseiAI/donmai/provider/harness/claude"
	codexprovider "github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/provider/harness/muse"
	"github.com/RenseiAI/donmai/provider/harness/opencode"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	shellprovider "github.com/RenseiAI/donmai/provider/harness/shell"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
)

// TestInteractiveReadiness_Contract pins the readiness truth table: every
// declarable mechanism, every input state, and the live-versus-queue split.
//
// Declared support and actually driven/ready stay separate: the same manifest
// value that admits a session can still read as not live here, and only the
// hook reads as live for chat. No harness name appears in the table — the only
// inputs are the declared mechanism, what the session exposes, and what this
// build drives.
func TestInteractiveReadiness_Contract(t *testing.T) {
	liveHook := &fakeNoticeChannel{}
	pullTests := []struct {
		name         string
		channel      agent.NoticeDelivery
		nch          agent.NoticeChannel
		busy         bool
		alt          bool
		ended        bool
		wantState    agent.ChatState
		wantLive     bool
		wantQueue    bool
		wantReason   string
		wantDeclared bool
		wantDriven   bool
		wantExposed  bool
		wantPull     bool
	}{
		{
			name:    "hook idle is live and queued",
			channel: agent.NoticeDeliveryHook, nch: liveHook,
			wantState: agent.ChatStateIdle, wantLive: true, wantQueue: true,
			wantDeclared: true, wantDriven: true, wantExposed: true, wantPull: true,
		},
		{
			name:    "hook busy stays live and queued",
			channel: agent.NoticeDeliveryHook, nch: liveHook, busy: true,
			wantState: agent.ChatStateBusy, wantLive: true, wantQueue: true,
			wantDeclared: true, wantDriven: true, wantExposed: true, wantPull: true,
		},
		{
			name:    "hook ignores the alternate screen: pull never touches the terminal",
			channel: agent.NoticeDeliveryHook, nch: liveHook, alt: true,
			wantState: agent.ChatStateIdle, wantLive: true, wantQueue: true,
			wantDeclared: true, wantDriven: true, wantExposed: true, wantPull: true,
		},
		{
			name:    "hook ended takes nothing",
			channel: agent.NoticeDeliveryHook, nch: liveHook, ended: true,
			wantState: agent.ChatStateEnded, wantLive: false, wantQueue: false,
			wantReason:   agent.ChatReasonSessionEnded,
			wantDeclared: true, wantDriven: true, wantExposed: true, wantPull: true,
		},
		{
			name:    "hook declared but absent is queue-only with the absent reason",
			channel: agent.NoticeDeliveryHook, nch: nil,
			wantState: agent.ChatStateIdle, wantLive: false, wantQueue: true,
			wantReason:   agent.ChatReasonChannelAbsent,
			wantDeclared: true, wantDriven: true, wantPull: true,
		},
		{
			name:    "pty-notice is not semantic chat even when push would succeed",
			channel: agent.NoticeDeliveryPTYNotice, nch: nil,
			wantState: agent.ChatStateIdle, wantLive: false, wantQueue: false,
			wantReason:   agent.ChatReasonPTYNoticeNotChat,
			wantDeclared: true, wantDriven: true, wantExposed: true,
		},
		{
			name:    "pty-notice on the alternate screen stays not-chat with the modal state",
			channel: agent.NoticeDeliveryPTYNotice, nch: nil, alt: true,
			wantState: agent.ChatStateModal, wantLive: false, wantQueue: false,
			wantReason:   agent.ChatReasonModalAlternateScreen,
			wantDeclared: true, wantDriven: true, wantExposed: true,
		},
		{
			name:    "pty-notice ended takes nothing",
			channel: agent.NoticeDeliveryPTYNotice, nch: nil, ended: true,
			wantState: agent.ChatStateEnded, wantLive: false, wantQueue: false,
			wantReason:   agent.ChatReasonSessionEnded,
			wantDeclared: true, wantDriven: true, wantExposed: true,
		},
	}

	for _, tc := range pullTests {
		t.Run(tc.name, func(t *testing.T) {
			sess := liveRecordingInteractiveSession()
			if tc.alt {
				sess.setAlternateScreen(true)
			}
			if tc.ended {
				close(sess.done)
			}
			got := interactiveChatReadiness(tc.channel, sess, tc.nch, tc.busy)
			if got.State != tc.wantState {
				t.Fatalf("State = %q; want %q", got.State, tc.wantState)
			}
			if got.LiveSupported != tc.wantLive {
				t.Fatalf("LiveSupported = %v; want %v", got.LiveSupported, tc.wantLive)
			}
			if got.QueueSupported != tc.wantQueue {
				t.Fatalf("QueueSupported = %v; want %v", got.QueueSupported, tc.wantQueue)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("Reason = %q; want %q", got.Reason, tc.wantReason)
			}
			if got.Declared != tc.wantDeclared {
				t.Fatalf("Declared = %v; want %v", got.Declared, tc.wantDeclared)
			}
			if got.Driven != tc.wantDriven {
				t.Fatalf("Driven = %v; want %v", got.Driven, tc.wantDriven)
			}
			if got.Exposed != tc.wantExposed {
				t.Fatalf("Exposed = %v; want %v", got.Exposed, tc.wantExposed)
			}
			if got.Pull != tc.wantPull {
				t.Fatalf("Pull = %v; want %v", got.Pull, tc.wantPull)
			}
			if got.Channel != tc.channel {
				t.Fatalf("Channel = %q; want %q", got.Channel, tc.channel)
			}
		})
	}

	undriven := []agent.NoticeDelivery{
		agent.NoticeDeliveryMCPRPC,
		agent.NoticeDeliveryHTTPSession,
		agent.NoticeDeliveryRPCSteer,
		agent.NoticeDeliveryACP,
		agent.NoticeDeliveryResumeInject,
		agent.NoticeDeliveryInBoxLoop,
		agent.NoticeDeliveryNone,
		"",
	}
	for _, channel := range undriven {
		t.Run("undriven "+declaredOrUndeclared(channel), func(t *testing.T) {
			sess := liveRecordingInteractiveSession()
			got := interactiveChatReadiness(channel, sess, nil, false)
			if got.LiveSupported {
				t.Fatalf("LiveSupported = true for %q; undriven channels must fail closed", channel)
			}
			if sess != nil && !isInteractiveSessionEnded(sess) && !got.QueueSupported {
				// Ended sessions queue nothing; every live undriven session
				// keeps the durable mailbox as the floor.
				t.Fatalf("QueueSupported = false for live %q; the mailbox is the floor", channel)
			}
			if got.Driven {
				t.Fatalf("Driven = true for %q; only hook and pty-notice are driven", channel)
			}
			if got.Pull && channel != agent.NoticeDeliveryHook {
				t.Fatalf("Pull = true for %q; only hook is pull", channel)
			}
			if channel == agent.NoticeDeliveryNone && got.Reason != agent.ChatReasonNone {
				t.Fatalf("Reason = %q; want %q", got.Reason, agent.ChatReasonNone)
			}
			if channel == "" && got.Reason != agent.ChatReasonUndeclared {
				t.Fatalf("Reason = %q; want %q", got.Reason, agent.ChatReasonUndeclared)
			}
			if channel != agent.NoticeDeliveryNone && channel != "" && got.Reason != agent.ChatReasonChannelNotDriven {
				t.Fatalf("Reason = %q; want %q", got.Reason, agent.ChatReasonChannelNotDriven)
			}
			if len(sess.recordedWrites()) != 0 {
				t.Fatal("readiness read wrote to the terminal")
			}
		})
	}
}

// TestInteractiveReadiness_PinnedHarnessDeclarations inspects the exact
// supported versions and public structured ingress interfaces for the current
// interactive handles. It fails when a manifest's declaration drifts without
// the readiness table moving with it.
//
// Pinned source versions read here (the single source of truth in each
// harness): pi 0.80.10 (provider/harness/pi/probe.go), opencode 1.17.18
// (provider/harness/opencode/probe.go), muse 1.4.6
// (provider/harness/muse/muse.go), claude floor 2.1.197 for the Sonnet 5
// model probe (provider/harness/claude/model_version.go). Codex pins no
// binary version in-tree; its public ingress is the app-server JSON-RPC
// surface (thread/start, turn/start, thread/resume) the headless Inject rail
// deliberately leaves ErrUnsupported.
func TestInteractiveReadiness_PinnedHarnessDeclarations(t *testing.T) {
	t.Run("hook is the only live structured chat channel", func(t *testing.T) {
		if !noticeChannelDrivenByRunner(agent.NoticeDeliveryHook) {
			t.Fatal("hook must stay driven: it is the first safe structured channel")
		}
		if !noticeChannelIsPull(agent.NoticeDeliveryHook) {
			t.Fatal("hook must stay pull: ack on consumption, never on placement")
		}
	})
	t.Run("pty-notice stays push and stays not-chat", func(t *testing.T) {
		if !noticeChannelDrivenByRunner(agent.NoticeDeliveryPTYNotice) {
			t.Fatal("pty-notice must stay driven as push for shell")
		}
		if noticeChannelIsPull(agent.NoticeDeliveryPTYNotice) {
			t.Fatal("pty-notice must stay push, never pull")
		}
		sess := liveRecordingInteractiveSession()
		got := interactiveChatReadiness(agent.NoticeDeliveryPTYNotice, sess, nil, false)
		if got.LiveSupported {
			t.Fatal("pty-notice readiness must stay not-live for semantic chat")
		}
		if got.Reason != agent.ChatReasonPTYNoticeNotChat {
			t.Fatalf("pty-notice chat reason = %q; want %q", got.Reason, agent.ChatReasonPTYNoticeNotChat)
		}
	})
	t.Run("every other structured channel stays undriven interactively", func(t *testing.T) {
		for _, channel := range []agent.NoticeDelivery{
			agent.NoticeDeliveryMCPRPC,
			agent.NoticeDeliveryHTTPSession,
			agent.NoticeDeliveryRPCSteer,
			agent.NoticeDeliveryACP,
			agent.NoticeDeliveryResumeInject,
			agent.NoticeDeliveryInBoxLoop,
		} {
			if noticeChannelDrivenByRunner(channel) {
				t.Fatalf("%q became driven without its own consumption evidence lane", channel)
			}
		}
	})
	t.Run("pinned harness versions and declarations match the inspected sources", func(t *testing.T) {
		if pi.PinnedVersion != "0.80.10" || pi.MinVersion != "0.80.10" || pi.VerifiedAgainst != "0.80.10" {
			t.Fatalf("pi versions = %q/%q/%q; want 0.80.10 pinned (probe.go)", pi.PinnedVersion, pi.MinVersion, pi.VerifiedAgainst)
		}
		if opencode.PinnedVersion != "1.17.18" || opencode.MinVersion != "1.17.18" || opencode.VerifiedAgainst != "1.17.18" {
			t.Fatalf("opencode versions = %q/%q/%q; want 1.17.18 pinned (probe.go)", opencode.PinnedVersion, opencode.MinVersion, opencode.VerifiedAgainst)
		}
		if muse.PinnedVersion != "1.4.6" || muse.MinVersion != "1.4.6" || muse.VerifiedAgainst != "1.4.6" {
			t.Fatalf("muse versions = %q/%q/%q; want 1.4.6 pinned (muse.go)", muse.PinnedVersion, muse.MinVersion, muse.VerifiedAgainst)
		}
		// Manifest declarations are the public ingress interfaces the
		// readiness table is keyed on. A manifest that flips without the
		// readiness lane moving in the same change breaks the proof above.
		if got := (&pi.Provider{}).Manifest().Caps.NoticeDelivery; got != agent.NoticeDeliveryRPCSteer {
			t.Fatalf("pi manifest NoticeDelivery = %q; want rpc-steer (same-session RPC, not the interactive TUI)", got)
		}
		if got := (&muse.Provider{}).Manifest().Caps.NoticeDelivery; got != agent.NoticeDeliveryMCPRPC {
			t.Fatalf("muse manifest NoticeDelivery = %q; want mcp-rpc (serve host, not the interactive TUI)", got)
		}
		if got := (&claudeprovider.Provider{}).Manifest().Caps.NoticeDelivery; got != agent.NoticeDeliveryHook {
			t.Fatalf("claude manifest NoticeDelivery = %q; want hook (the first safe structured channel)", got)
		}
		if got := (&codexprovider.Provider{}).Manifest().Caps.NoticeDelivery; got != agent.NoticeDeliveryMCPRPC {
			t.Fatalf("codex manifest NoticeDelivery = %q; want mcp-rpc (app-server JSON-RPC, not driven interactively)", got)
		}
		if got := (&shellprovider.Provider{}).Manifest().Caps.NoticeDelivery; got != agent.NoticeDeliveryPTYNotice {
			t.Fatalf("shell manifest NoticeDelivery = %q; want pty-notice (push, explicitly not chat)", got)
		}
		if got := (&opencode.Provider{}).Manifest().Caps.NoticeDelivery; got != agent.NoticeDeliveryHTTPSession {
			t.Fatalf("opencode manifest NoticeDelivery = %q; want http-session (serve POST, no interactive ownership)", got)
		}
	})
}

// TestInteractiveReadiness_SameSessionStructuredTurn is the runnable proof for
// the first safe driven structured channel: one hook session takes one
// multiline message with a stable identity from queue to consumption, with no
// terminal typing, no flattening, and no ack before the recipient's own
// record proves it.
func TestInteractiveReadiness_SameSessionStructuredTurn(t *testing.T) {
	const text = "line one\nline two\n\n  indented three\nfinal — with multibyte: あ"
	const deliveryID = "dlv-structured-proof"

	nch := &fakeNoticeChannel{}
	d := startInteractiveDispatchWith(t, "sess-structured-proof", agent.NoticeDeliveryHook, nch)

	idle := (&interactiveNoticeQueue{channel: agent.NoticeDeliveryHook}).readiness(d.session, nch)
	if !idle.LiveSupported || idle.State != agent.ChatStateIdle {
		t.Fatalf("idle readiness = %+v; want live idle", idle)
	}

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: deliveryID, Text: text}
	offers := nch.waitOffers(t, 1)
	if offers[0].deliveryID != deliveryID {
		t.Fatalf("offered deliveryID = %q; want stable %q", offers[0].deliveryID, deliveryID)
	}
	if offers[0].text != text {
		t.Fatalf("offered text = %q; want verbatim multiline %q", offers[0].text, text)
	}
	if strings.ContainsRune(offers[0].text, interactiveNoticeSubmit) {
		t.Fatalf("offered text carries the PTY submit byte: %q", offers[0].text)
	}

	busy := (&interactiveNoticeQueue{channel: agent.NoticeDeliveryHook, text: text, deliveryID: deliveryID}).readiness(d.session, nch)
	if !busy.LiveSupported || busy.State != agent.ChatStateBusy {
		t.Fatalf("busy readiness = %+v; want live busy while outstanding", busy)
	}

	// Queued, not applied: the channel holds custody and upstream is told
	// nothing. Queue acceptance is not live delivery.
	d.clock.waitArmed(t)
	if got := d.pulser.acked(); len(got) != 0 {
		t.Fatalf("offered-but-unconsumed notice was acked: %v", got)
	}
	if got := d.pulser.deadLettered(); len(got) != 0 {
		t.Fatalf("notice awaiting collection was dead-lettered: %+v", got)
	}
	if got := d.session.recordedWrites(); len(got) != 0 {
		t.Fatalf("structured delivery wrote %d byte-sequence(s) to the terminal: %q — "+
			"a PTY write on a harness with an agent behind it is a keystroke into its UI", len(got), got)
	}
	d.clock.fire(t)

	// Applied: the recipient's own record proves the message entered the
	// conversation, and only then is it acked — exactly once, with the same id.
	// Prove the last false-consumption poll completed and re-armed before
	// changing the channel. A buffered fake-clock fire only proves enqueue.
	d.clock.waitArmed(t)
	nch.setConsumed(true)
	d.clock.fire(t)
	d.pulser.waitAcked(t, deliveryID)
	if got := d.pulser.acked(); len(got) != 1 || got[0] != deliveryID {
		t.Fatalf("acks = %v; want exactly the consumed delivery", got)
	}
	if got := d.pulser.deadLettered(); len(got) != 0 {
		t.Fatalf("a consumed notice was also dead-lettered: %+v", got)
	}
}

// TestInteractiveReadiness_QueuedAppliedDeadLetteredReceipts pins the three
// durable receipts: queued stays unacked while outstanding, applied acks once
// on consumption, and dead-lettered reports once with the not-consumed reason
// after withdrawing the offer so a late collection cannot deliver a message
// already reported dead.
func TestInteractiveReadiness_QueuedAppliedDeadLetteredReceipts(t *testing.T) {
	nch := &fakeNoticeChannel{retractOK: true}
	d := startInteractiveDispatchWith(t, "sess-receipts", agent.NoticeDeliveryHook, nch)

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-receipt-stuck", Text: "never collected"}
	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-receipt-behind", Text: "starved behind it"}
	nch.waitOffers(t, 1)

	// Queued: polls elapse, nothing is reported either way.
	for range 3 {
		d.clock.waitArmed(t)
		if got := d.pulser.acked(); len(got) != 0 {
			t.Fatalf("queued notice was acked: %v", got)
		}
		if got := d.pulser.deadLettered(); len(got) != 0 {
			t.Fatalf("queued notice was dead-lettered early: %+v", got)
		}
		d.clock.fire(t)
	}

	// Dead-lettered: the wait is spent, the offer is withdrawn first, and the
	// producer learns instead of watching eternal in-flight.
	for range interactiveNoticePullMaxPolls - 4 {
		d.clock.waitArmed(t)
		d.clock.fire(t)
	}
	if reason := d.pulser.waitDeadLettered(t, "dlv-receipt-stuck"); reason != noticeDeadNotConsumed {
		t.Fatalf("dead-letter reason = %q; want %q", reason, noticeDeadNotConsumed)
	}
	if got := nch.retractCount(); got != 1 {
		t.Fatalf("Retract called %d times; want exactly 1", got)
	}
	if got := d.pulser.acked(); len(got) != 0 {
		t.Fatalf("a never-consumed notice was acked: %v", got)
	}

	// Applied: the slot freed, so the payload behind it got its own offer with
	// its own stable identity.
	offers := nch.waitOffers(t, 2)
	if offers[1].deliveryID != "dlv-receipt-behind" {
		t.Fatalf("second offer = %+v; want the payload queued behind the stuck one", offers[1])
	}
}

// TestInteractiveReadiness_RestartEndedGenerationFencing proves an offer dies
// with its session: a notice left outstanding when the PTY exits is neither
// acked nor dead-lettered (left unacked for requeue), and a replacement
// session starts with no offer from its predecessor.
func TestInteractiveReadiness_RestartEndedGenerationFencing(t *testing.T) {
	nch := &fakeNoticeChannel{}
	d := startInteractiveDispatchWith(t, "sess-fence-old", agent.NoticeDeliveryHook, nch)

	d.injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-orphan", Text: "arrived too late"}
	nch.waitOffers(t, 1)
	d.clock.waitArmed(t)

	ended := interactiveChatReadiness(agent.NoticeDeliveryHook, completedRecordingInteractiveSession(), nch, true)
	if ended.State != agent.ChatStateEnded || ended.LiveSupported || ended.QueueSupported {
		t.Fatalf("ended readiness = %+v; want ended with no live and no queue", ended)
	}

	d.finish(t)
	if got := d.pulser.acked(); len(got) != 0 {
		t.Fatalf("a notice the session never consumed was acked at session end: %v", got)
	}
	if got := d.pulser.deadLettered(); len(got) != 0 {
		t.Fatalf("a notice left for requeue was dead-lettered at session end: %+v", got)
	}

	fresh := &fakeNoticeChannel{}
	d2 := startInteractiveDispatchWith(t, "sess-fence-new", agent.NoticeDeliveryHook, fresh)
	if got := fresh.offered(); len(got) != 0 {
		t.Fatalf("replacement session inherited %d offer(s): %+v", len(got), got)
	}
	idle := interactiveChatReadiness(agent.NoticeDeliveryHook, d2.session, fresh, false)
	if !idle.LiveSupported || idle.State != agent.ChatStateIdle {
		t.Fatalf("replacement readiness = %+v; want live idle", idle)
	}
}

// TestInteractiveReadiness_QueueReadinessMirrorsTheSlot pins the observer to
// the rail: the queue's own readiness read reports busy exactly while it holds
// a payload and idle once it clears, with no terminal side effects.
func TestInteractiveReadiness_QueueReadinessMirrorsTheSlot(t *testing.T) {
	nch := &fakeNoticeChannel{}
	sess := liveRecordingInteractiveSession()
	q := &interactiveNoticeQueue{channel: agent.NoticeDeliveryHook, sink: newFakeInteractivePulser()}
	if got := q.readiness(sess, nch); got.State != agent.ChatStateIdle || !got.LiveSupported {
		t.Fatalf("empty slot readiness = %+v; want live idle", got)
	}
	q.hold("held text", "dlv-slot")
	if got := q.readiness(sess, nch); got.State != agent.ChatStateBusy || !got.LiveSupported {
		t.Fatalf("held slot readiness = %+v; want live busy", got)
	}
	if len(sess.recordedWrites()) != 0 {
		t.Fatal("readiness read wrote to the terminal")
	}
	// A pull queue with no session channel is queue-only with the absent
	// reason — the durable mailbox stays the floor.
	if got := q.readiness(sess, nil); got.LiveSupported || got.Reason != agent.ChatReasonChannelAbsent {
		t.Fatalf("absent-channel readiness = %+v; want queue-only with %q", got, agent.ChatReasonChannelAbsent)
	}
}
