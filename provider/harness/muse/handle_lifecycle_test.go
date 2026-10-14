package muse

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// spawnHanging starts a session whose turn never completes, so the
// test drives the teardown path itself.
func spawnHanging(ctx context.Context, t *testing.T) (*Handle, *fakeHost) {
	t.Helper()
	p, fh := newTestProvider(t, "hang")
	ah, err := p.Spawn(ctx, agent.Spec{Prompt: "long task", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	h, ok := ah.(*Handle)
	if !ok {
		t.Fatalf("Spawn returned %T, want *Handle", ah)
	}
	return h, fh
}

// returnsWithin fails the test when fn does not return in d (a hung
// teardown deadlocks instead of failing).
func returnsWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

// TestHandle_StopMidTurn stops a live turn: Stop must return, send
// turn/interrupt, close the event stream, and not report the operator
// stop as a serve crash.
func TestHandle_StopMidTurn(t *testing.T) {
	h, fh := spawnHanging(context.Background(), t)

	returnsWithin(t, 5*time.Second, "Stop", func() { _ = h.Stop(context.Background()) })
	events := drainEvents(t, h.Events(), 5*time.Second)
	for _, ev := range events {
		if e, ok := ev.(agent.ErrorEvent); ok && e.Code == "serve_crashed" {
			t.Errorf("operator Stop reported as serve crash: %q", e.Message)
		}
	}
	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	fh.mu.Unlock()
	sawInterrupt := false
	for _, m := range methods {
		if m == "turn/interrupt" {
			sawInterrupt = true
		}
	}
	if !sawInterrupt {
		t.Errorf("Stop never sent turn/interrupt; methods=%v", methods)
	}
	// Idempotent.
	returnsWithin(t, 5*time.Second, "second Stop", func() { _ = h.Stop(context.Background()) })
}

// TestHandle_ContextCancelMidTurn cancels the session context: the
// consumer sees the context_cancelled error, then the stream closes.
func TestHandle_ContextCancelMidTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h, _ := spawnHanging(ctx, t)
	cancel()

	events := drainEvents(t, h.Events(), 5*time.Second)
	var codes []string
	for _, ev := range events {
		if e, ok := ev.(agent.ErrorEvent); ok {
			codes = append(codes, e.Code)
		}
	}
	if len(codes) != 1 || codes[0] != "context_cancelled" {
		t.Errorf("error codes=%v, want exactly [context_cancelled]", codes)
	}
}

// TestHandle_HostStreamClosedMidTurn closes the host's stdout mid-turn:
// the handle reports a serve crash, closes, and a later Stop returns.
func TestHandle_HostStreamClosedMidTurn(t *testing.T) {
	h, fh := spawnHanging(context.Background(), t)
	_ = fh.stdout.Close()

	events := drainEvents(t, h.Events(), 5*time.Second)
	sawCrash := false
	for _, ev := range events {
		if e, ok := ev.(agent.ErrorEvent); ok && e.Code == "serve_crashed" {
			sawCrash = true
		}
	}
	if !sawCrash {
		t.Errorf("no serve_crashed after the host stream closed; kinds=%v", kindsOf(events))
	}
	returnsWithin(t, 5*time.Second, "Stop after crash", func() { _ = h.Stop(context.Background()) })
}

// TestProvider_HandshakeRejectedReturns rejects the handshake (wrong
// schema version): Spawn must fail promptly rather than hang tearing
// the child down.
func TestProvider_HandshakeRejectedReturns(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")
	fh.mu.Lock()
	fh.schemaVersion = 2
	fh.mu.Unlock()

	var err error
	returnsWithin(t, 5*time.Second, "Spawn", func() {
		_, err = p.Spawn(context.Background(), agent.Spec{Prompt: "x", Cwd: t.TempDir(), Autonomous: true})
	})
	if err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Errorf("Spawn err=%v, want a schema version failure", err)
	}
}

// TestHandle_DeliverNeverDrops floods deliver past the notify buffer
// before the forwarder drains it: every notification must arrive (a
// dropped turn/completed or server-request stalls the session).
func TestHandle_DeliverNeverDrops(t *testing.T) {
	h := newHandle(nil, agent.Spec{}, SpawnPlan{}, HandleOptions{})
	const n = 600
	go func() {
		for i := 0; i < n; i++ {
			h.deliver(notification{Method: "item/delta", Params: json.RawMessage(`{}`)})
		}
		h.deliver(notification{Method: "turn/completed", Params: json.RawMessage(`{}`)})
	}()
	got := 0
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for got < n+1 {
		select {
		case <-h.notifyCh:
			got++
		case <-timer.C:
			t.Fatalf("received %d of %d notifications; deliver dropped the rest", got, n+1)
		}
	}
}

// TestResolveApprovalMode_ReadOnlyIsTightenOnly pins the read-only lane
// to denyUnmatched whatever else the Spec carries.
func TestResolveApprovalMode_ReadOnlyIsTightenOnly(t *testing.T) {
	for _, spec := range []agent.Spec{
		{SandboxLevel: agent.SandboxReadOnly},
		{SandboxLevel: agent.SandboxReadOnly, Autonomous: true},
		{SandboxLevel: agent.SandboxReadOnly, Autonomous: true, PermissionConfig: &agent.PermissionConfig{DefaultDecision: "allow"}},
	} {
		if got := resolveApprovalMode(spec); got != ApprovalModeDenyUnmatched {
			t.Errorf("read-only spec %+v: approvalMode=%q, want denyUnmatched", spec, got)
		}
	}
	args := strings.Join(serveArgs(agent.Spec{SandboxLevel: agent.SandboxReadOnly}, false, true), " ")
	if strings.Contains(args, "--disable-sandbox") || !strings.Contains(args, "--disable-shell --disable-write") {
		t.Errorf("read-only lane with DisableSandbox: args=%q, want the read-only flags and no --disable-sandbox", args)
	}
}

// TestHandle_ViewGapIsNotTheTerminal keeps the recovered gap off the
// run's failure path: the gap error continues the session and the
// turn's ResultEvent is the only terminal.
func TestHandle_ViewGapIsNotTheTerminal(t *testing.T) {
	p, _ := newTestProvider(t, "gap")
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	terminals := 0
	for _, ev := range events {
		switch e := ev.(type) {
		case agent.ErrorEvent:
			if !e.SessionContinues {
				terminals++
			}
		case agent.ResultEvent:
			terminals++
		}
	}
	if terminals != 1 {
		t.Errorf("terminal events=%d, want 1; kinds=%v", terminals, kindsOf(events))
	}
	if _, ok := events[len(events)-1].(agent.ResultEvent); !ok {
		t.Errorf("last event is %T, want ResultEvent", events[len(events)-1])
	}
}
