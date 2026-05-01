package stub

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// Test_Stop_EmitsStoppedResultAndClosesChannel verifies that Stop on
// a script that would otherwise block forever (HangThenTimeout)
// causes the scripting goroutine to emit a terminal ResultEvent with
// ErrorSubtype "stopped" and close the events channel.
func Test_Stop_EmitsStoppedResultAndClosesChannel(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{behaviorConfigKey: string(BehaviorHangThenTimeout)},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Drain the preamble so the script is parked on ctx.Done / stopped.
	<-h.Events() // Init
	<-h.Events() // System

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := h.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Expect terminal ResultEvent with ErrorSubtype "stopped".
	select {
	case ev, ok := <-h.Events():
		if !ok {
			t.Fatalf("expected stopped Result before close")
		}
		res, isResult := ev.(agent.ResultEvent)
		if !isResult {
			t.Fatalf("post-Stop event is %T, want ResultEvent", ev)
		}
		if res.ErrorSubtype != "stopped" {
			t.Fatalf("ResultEvent.ErrorSubtype = %q want \"stopped\"", res.ErrorSubtype)
		}
		if res.Success {
			t.Fatalf("ResultEvent.Success = true; stopped result should be false")
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for stopped Result")
	}

	// Channel must close after the stopped result.
	select {
	case _, ok := <-h.Events():
		if ok {
			t.Fatalf("events channel still open after stopped Result")
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for events channel close")
	}
}

// Test_Stop_Idempotent verifies that a second Stop call returns nil
// and does not panic on the already-closed `stopped` channel.
func Test_Stop_Idempotent(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{behaviorConfigKey: string(BehaviorHangThenTimeout)},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Drain preamble, then stop twice.
	<-h.Events()
	<-h.Events()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := h.Stop(stopCtx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := h.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	drainAll(h)
}

// Test_Stop_AfterChannelClosed_NoPanic verifies that Stop called
// after the script completed naturally returns cleanly.
func Test_Stop_AfterChannelClosed_NoPanic(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{behaviorConfigKey: string(BehaviorSucceedWithPR)},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drainAll(h)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := h.Stop(stopCtx); err != nil {
		t.Fatalf("Stop after natural completion: %v", err)
	}
}

// drainAll consumes the events channel until close. Used by tests
// that need to release the scripting goroutine without asserting on
// any further events.
func drainAll(h agent.Handle) {
	for range h.Events() { //nolint:revive // intentional drain
		_ = struct{}{}
	}
}
