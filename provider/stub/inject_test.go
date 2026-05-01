package stub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func Test_InjectAppearsAsAssistantText(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{behaviorConfigKey: string(BehaviorInjectTest)},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Drain the lifecycle preamble (Init + System) so we know the
	// scripting goroutine is parked on the inject channel.
	if k := mustNextKind(t, h, time.Second); k != agent.EventInit {
		t.Fatalf("expected Init first, got %s", k)
	}
	if k := mustNextKind(t, h, time.Second); k != agent.EventSystem {
		t.Fatalf("expected System second, got %s", k)
	}

	// Inject; the next event should be the echoed AssistantText, then
	// the terminal Result.
	if err := h.Inject(ctx, "please rebase"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	ev := mustNext(t, h, time.Second)
	at, ok := ev.(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("expected AssistantTextEvent after inject, got %T (%v)", ev, ev)
	}
	if at.Text != "injected: please rebase" {
		t.Fatalf("AssistantTextEvent.Text = %q want \"injected: please rebase\"", at.Text)
	}

	ev = mustNext(t, h, time.Second)
	res, ok := ev.(agent.ResultEvent)
	if !ok {
		t.Fatalf("expected terminal ResultEvent, got %T", ev)
	}
	if !res.Success {
		t.Fatalf("inject-test ResultEvent.Success should be true")
	}

	// Channel should close after the result.
	if _, ok := <-h.Events(); ok {
		t.Fatalf("expected events channel to close after terminal Result")
	}
}

func Test_InjectUnsupportedWhenFlagSet(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{
			behaviorConfigKey:        string(BehaviorInjectTest),
			"stub.injectUnsupported": true,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Drain init/system to give the scripting goroutine a chance to
	// be parked, but it won't matter — the gate fires before send.
	<-h.Events()
	<-h.Events()

	err = h.Inject(ctx, "ignored")
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Inject with unsupported flag: got %v want ErrUnsupported", err)
	}
	// Stop to release the scripting goroutine which is parked on
	// h.injects.
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// mustNext reads the next event from h.Events with a deadline.
func mustNext(t *testing.T, h agent.Handle, d time.Duration) agent.Event {
	t.Helper()
	select {
	case ev, ok := <-h.Events():
		if !ok {
			t.Fatalf("events channel closed unexpectedly")
		}
		return ev
	case <-time.After(d):
		t.Fatalf("mustNext: timed out after %s", d)
	}
	return nil // unreachable
}

// mustNextKind reads the next event and returns its kind, asserting
// that the channel is still open.
func mustNextKind(t *testing.T, h agent.Handle, d time.Duration) agent.EventKind {
	t.Helper()
	return mustNext(t, h, d).Kind()
}
