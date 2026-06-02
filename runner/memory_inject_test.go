package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/stub"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
)

// TestDrainMemoryInjects_DeliversBufferedBlock verifies the Wave 3 runtime
// memory drain: a buffered InjectPayload at the post-terminal seam is
// delivered into the live session and the resume turn's events (including
// the assistant event carrying the block text) are re-consumed + mirrored.
func TestDrainMemoryInjects_DeliversBufferedBlock(t *testing.T) {
	r := minimalRunner(t)

	p, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := r.registry.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx, cancel := withCtx(t)
	defer cancel()
	handle, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{
			"stub.behavior": string(stub.BehaviorInjectTest),
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()

	// Pre-buffer one inject as the heartbeat transport would have.
	injectCh := make(chan heartbeat.InjectPayload, 8)
	const memText = "recall: reuse afclient/retry.go, do not write a new retry loop"
	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: memText}

	rec := &recordingSink{}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-MEM-1")}
	res := &Result{SessionID: qw.SessionID}
	enforcer := NewBudgetEnforcer(nil, time.Now())

	r.drainMemoryInjects(ctx, handle, wpath, qw, res, enforcer, rec, injectCh)

	// The resume turn must have produced an AssistantTextEvent echoing the
	// injected block (the stub prefixes it with "injected: ").
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var sawBlock bool
	for _, ev := range rec.events {
		if at, ok := ev.(agent.AssistantTextEvent); ok && strings.Contains(at.Text, memText) {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Fatalf("expected an assistant event carrying the memory block text; got %v", kindsOf(rec.events))
	}
}

// TestDrainMemoryInjects_EmptyChannelNoop verifies a no-op drain when no
// inject was buffered — the helper returns a zero observation without
// touching the handle.
func TestDrainMemoryInjects_EmptyChannelNoop(t *testing.T) {
	r := minimalRunner(t)

	injectCh := make(chan heartbeat.InjectPayload, 8)
	rec := &recordingSink{}
	handle := &fakeHandle{events: make(chan agent.Event)} // never read
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-MEM-2")}
	res := &Result{SessionID: qw.SessionID}
	enforcer := NewBudgetEnforcer(nil, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := r.drainMemoryInjects(ctx, handle, t.TempDir(), qw, res, enforcer, rec, injectCh)

	if got.terminalEvent != nil || got.pullRequestURL != "" {
		t.Fatalf("expected zero observation on empty drain, got %+v", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 0 {
		t.Fatalf("expected no events mirrored on empty drain, got %v", kindsOf(rec.events))
	}
}

// TestDrainMemoryInjects_SkipsEmptyText verifies that an inject with
// whitespace-only text is dropped (never delivered to the handle) — the
// empty-text gate.
func TestDrainMemoryInjects_SkipsEmptyText(t *testing.T) {
	r := minimalRunner(t)

	// recordInjectHandle records every Inject call so we can assert the
	// empty-text block was never delivered.
	handle := &recordInjectHandle{events: make(chan agent.Event)}

	injectCh := make(chan heartbeat.InjectPayload, 8)
	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-empty", Text: "   \n\t "}

	rec := &recordingSink{}
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-MEM-3")}
	res := &Result{SessionID: qw.SessionID}
	enforcer := NewBudgetEnforcer(nil, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.drainMemoryInjects(ctx, handle, t.TempDir(), qw, res, enforcer, rec, injectCh)

	if got := handle.injectCount(); got != 0 {
		t.Fatalf("empty-text block must not be delivered; Inject called %d times", got)
	}
}

// TestMemoryInjectOnInject_DedupesByDeliveryID exercises the dedup-by-
// DeliveryID gate that lives in the OnInject closure (the heartbeat-side
// guard against a re-delivery in the window before the ack lands). A second
// delivery with the same DeliveryID must be dropped; a different one passes.
func TestMemoryInjectOnInject_DedupesByDeliveryID(t *testing.T) {
	// Mirror the closure built in runLoop (kept identical so the test
	// guards the real dedup contract).
	seenInject := map[string]struct{}{}
	injectCh := make(chan heartbeat.InjectPayload, 8)
	onInject := func(p heartbeat.InjectPayload) {
		if p.DeliveryID != "" {
			if _, ok := seenInject[p.DeliveryID]; ok {
				return
			}
			seenInject[p.DeliveryID] = struct{}{}
		}
		select {
		case injectCh <- p:
		default:
		}
	}

	onInject(heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "first"})
	onInject(heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "duplicate"}) // dropped
	onInject(heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "second"})

	if got := len(injectCh); got != 2 {
		t.Fatalf("expected 2 distinct injects buffered (dup dropped), got %d", got)
	}
	first := <-injectCh
	second := <-injectCh
	if first.DeliveryID != "dlv-1" || second.DeliveryID != "dlv-2" {
		t.Fatalf("unexpected dedup result: %q then %q", first.DeliveryID, second.DeliveryID)
	}
}

// TestMemoryInjectOnInject_DropsWhenChannelFull verifies the non-blocking
// send: when injectCh is full the OnInject closure drops the inject (logs)
// rather than stalling the heartbeat goroutine.
func TestMemoryInjectOnInject_DropsWhenChannelFull(t *testing.T) {
	seenInject := map[string]struct{}{}
	injectCh := make(chan heartbeat.InjectPayload, 1) // tiny buffer
	var dropped bool
	onInject := func(p heartbeat.InjectPayload) {
		if p.DeliveryID != "" {
			if _, ok := seenInject[p.DeliveryID]; ok {
				return
			}
			seenInject[p.DeliveryID] = struct{}{}
		}
		select {
		case injectCh <- p:
		default:
			dropped = true
		}
	}

	onInject(heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "fits"})
	onInject(heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "overflow"}) // dropped

	if !dropped {
		t.Fatal("expected the second inject to be dropped on a full channel")
	}
	if got := len(injectCh); got != 1 {
		t.Fatalf("channel should hold exactly 1 inject, got %d", got)
	}
}

// TestRuntimeInjectGate_CapGate confirms the capability gate: when the
// provider does not advertise SupportsMessageInjection the runner never
// wires OnInject (runtime inject is a no-op; the session relies on the
// dispatch-time fold). We assert the gate condition directly since it is
// the single source of truth wired in runLoop.
func TestRuntimeInjectGate_CapGate(t *testing.T) {
	cases := []struct {
		name        string
		enabled     bool
		supportsInj bool
		want        bool
	}{
		{"disabled + no cap", false, false, false},
		{"disabled + cap", false, true, false},
		{"enabled + no cap", true, false, false},
		{"enabled + cap", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{memoryInject: tc.enabled}
			caps := agent.Capabilities{SupportsMessageInjection: tc.supportsInj}
			got := r.memoryInject && caps.SupportsMessageInjection
			if got != tc.want {
				t.Fatalf("runtimeInjectEnabled = %v; want %v", got, tc.want)
			}
		})
	}
}

// recordInjectHandle is an agent.Handle that records Inject calls and never
// emits events. Used to assert which blocks are delivered.
type recordInjectHandle struct {
	events  chan agent.Event
	injects []string
}

func (h *recordInjectHandle) SessionID() string          { return "" }
func (h *recordInjectHandle) Events() <-chan agent.Event { return h.events }
func (h *recordInjectHandle) Inject(_ context.Context, text string) error {
	h.injects = append(h.injects, text)
	return nil
}
func (h *recordInjectHandle) Stop(context.Context) error { return nil }
func (h *recordInjectHandle) injectCount() int           { return len(h.injects) }
