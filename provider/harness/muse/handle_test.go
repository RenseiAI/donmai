package muse

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// TestHandle_ConformsToTerminalEventContract wires the shared
// cross-harness event-contract check onto the muse harness: a
// successful turn against the fake host must emit exactly one
// terminal event, and it must be last.
func TestHandle_ConformsToTerminalEventContract(t *testing.T) {
	p, _ := newTestProvider(t, "single-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Errorf("terminal-event contract violated: %v\nevents: %v", err, kindsOf(events))
	}
}

// TestHandle_SingleTurnResult drives the PRODUCTION Spawn entry point
// against the fake host: handshake → session/start → turn/start →
// tokenUsage → agentMessage → turn/completed, ending in a successful
// ResultEvent carrying the host's cumulative cost.
func TestHandle_SingleTurnResult(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if h.SessionID() == "" {
		t.Fatal("SessionID is empty after Spawn")
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	res, ok := terminalResult(events)
	if !ok {
		t.Fatalf("no terminal ResultEvent; kinds=%v", kindsOf(events))
	}
	if !res.Success {
		t.Errorf("Success=false, errors=%v", res.Errors)
	}
	if res.Cost == nil {
		t.Fatal("ResultEvent has no Cost")
	}
	if res.Cost.TotalCostUsd != 0.0123 {
		t.Errorf("TotalCostUsd=%v, want 0.0123 (host cumulative.cost.usd)", res.Cost.TotalCostUsd)
	}
	if res.ObservedCostUsd == nil || *res.ObservedCostUsd != 0.0123 {
		t.Errorf("ObservedCostUsd=%v, want 0.0123", res.ObservedCostUsd)
	}

	// The handshake must have used the schema-version-1 initialize
	// with the sessionMcp capability, then initialized.
	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	fh.mu.Unlock()
	want := []string{"initialize", "initialized", "session/start", "turn/start"}
	if len(methods) < len(want) {
		t.Fatalf("host saw methods %v, want prefix %v", methods, want)
	}
	for i, w := range want {
		if methods[i] != w {
			t.Fatalf("host method[%d]=%q, want %q (full: %v)", i, methods[i], w, methods)
		}
	}
}

// TestHandle_MultiTurnToolCall drives a multi-step turn (toolCall
// started → delta → completed, then agentMessage) through Spawn and
// asserts the ToolUse/ToolResult pair plus the terminal result.
func TestHandle_MultiTurnToolCall(t *testing.T) {
	p, _ := newTestProvider(t, "multi-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "edit a file", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	var sawUse, sawResult, sawText bool
	for _, ev := range events {
		switch e := ev.(type) {
		case agent.ToolUseEvent:
			if e.ToolName == "edit" && e.ToolUseID == "tool-1" {
				sawUse = true
			}
		case agent.ToolResultEvent:
			if e.ToolUseID == "tool-1" && e.Content == "edited a.txt" && !e.IsError {
				sawResult = true
			}
		case agent.AssistantTextEvent:
			if e.Text == "done" {
				sawText = true
			}
		}
	}
	if !sawUse || !sawResult || !sawText {
		t.Errorf("use=%v result=%v text=%v; kinds=%v", sawUse, sawResult, sawText, kindsOf(events))
	}
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Errorf("terminal result missing or failed: %+v", res)
	}
}

// TestHandle_Interrupt maps a cancelled terminal to an interrupted
// ResultEvent through the production Spawn path.
func TestHandle_Interrupt(t *testing.T) {
	p, _ := newTestProvider(t, "interrupt")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "long task", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	res, ok := terminalResult(events)
	if !ok {
		t.Fatalf("no terminal ResultEvent; kinds=%v", kindsOf(events))
	}
	if res.Success {
		t.Error("interrupted turn reported Success=true")
	}
	if res.ErrorSubtype != "interrupted" {
		t.Errorf("ErrorSubtype=%q, want interrupted", res.ErrorSubtype)
	}
}

// TestHandle_FailedTurn maps a failed terminal (with the host error
// object) to a failed ResultEvent carrying the host message.
func TestHandle_FailedTurn(t *testing.T) {
	p, _ := newTestProvider(t, "failed")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	res, ok := terminalResult(events)
	if !ok {
		t.Fatalf("no terminal ResultEvent; kinds=%v", kindsOf(events))
	}
	if res.Success {
		t.Error("failed turn reported Success=true")
	}
	if len(res.Errors) != 1 || res.Errors[0] != "host overloaded" {
		t.Errorf("Errors=%v, want [host overloaded]", res.Errors)
	}
	if res.ErrorSubtype != "overloaded" {
		t.Errorf("ErrorSubtype=%q, want overloaded", res.ErrorSubtype)
	}
}

// TestHandle_Resume drives the PRODUCTION Resume entry point: the
// session id is validated as a UUID, session/resume re-attaches, and
// the turn runs to a terminal result.
func TestHandle_Resume(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")
	fh.sessionID = "22222222-2222-7222-8222-222222222222"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Resume(ctx, "22222222-2222-7222-8222-222222222222", agent.Spec{Prompt: "continue", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Errorf("resume terminal result missing or failed: %+v", res)
	}

	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	fh.mu.Unlock()
	sawResume := false
	for _, m := range methods {
		if m == "session/resume" {
			sawResume = true
		}
		for _, banned := range []string{"session/start"} {
			_ = banned
		}
	}
	if !sawResume {
		t.Errorf("host never saw session/resume; methods=%v", methods)
	}
	for _, m := range methods {
		if m == "session/start" {
			t.Errorf("resume path must not call session/start; methods=%v", methods)
		}
	}
}

// TestHandle_ResumeGoneSession maps sessionNotFound to
// ErrSessionNotFound through the production Resume path.
func TestHandle_ResumeGoneSession(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")
	fh.mu.Lock()
	fh.resumeErr = &rpcError{Code: -32000, Message: "no such session"}
	fh.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Resume(ctx, "33333333-3333-7333-8333-333333333333", agent.Spec{Prompt: "continue", Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isSessionGoneErr(err) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

// TestHandle_ViewGapRecovery drives the gap path through Spawn: the
// view/gap ErrorEvent fires (fatal for the stream), the handle
// re-anchors via session/resume, and the turn still completes.
func TestHandle_ViewGapRecovery(t *testing.T) {
	p, fh := newTestProvider(t, "gap")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	var sawGap, sawRecovered bool
	for _, ev := range events {
		switch e := ev.(type) {
		case agent.ErrorEvent:
			if e.Code == "view_gap" {
				sawGap = true
			}
		case agent.SystemEvent:
			if e.Subtype == "view_gap_recovered" {
				sawRecovered = true
			}
		case agent.AssistantTextEvent:
			if e.Text == "recovered" && sawGap && !sawRecovered {
				t.Error("post-gap item flowed before recovery completed")
			}
		}
	}
	if !sawGap {
		t.Errorf("no view_gap ErrorEvent; kinds=%v", kindsOf(events))
	}
	if !sawRecovered {
		t.Errorf("no view_gap_recovered after gap; kinds=%v", kindsOf(events))
	}
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Errorf("post-gap terminal result missing or failed: %+v", res)
	}

	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	fh.mu.Unlock()
	sawResume := false
	for _, m := range methods {
		if m == "session/resume" {
			sawResume = true
		}
	}
	if !sawResume {
		t.Errorf("gap recovery never called session/resume; methods=%v", methods)
	}
}

// TestHandle_ApprovalAllowThroughSpawn drives the approval path
// through Spawn: the host's approval/requested notification triggers
// an approval/decide allow (verified by the command reaching the
// host), and the turn completes.
func TestHandle_ApprovalAllowThroughSpawn(t *testing.T) {
	// Deny-by-default policy: without the bridge decision reaching
	// the host as approval/decide, nothing approves the call. The
	// assertion below (approval/decide reached the host carrying the
	// allow choice) is what fails when the decision path is broken.
	p, fh := newTestProvider(t, "approval")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "run echo", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Errorf("terminal result missing or failed: %+v", res)
	}

	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	params := append([]map[string]any(nil), fh.params...)
	fh.mu.Unlock()
	sawDecideAllow := false
	for i, m := range methods {
		if m != "approval/decide" {
			continue
		}
		if params[i]["approvalId"] == "appr-1" && params[i]["choiceId"] == "allow" {
			sawDecideAllow = true
		}
	}
	if !sawDecideAllow {
		t.Errorf("approval/decide with the allow choice never reached the host; methods=%v params=%v", methods, params)
	}
}

// TestHandle_InjectWithoutTurnFails asserts Inject without a running
// turn fails instead of hanging.
func TestHandle_InjectWithoutTurnFails(t *testing.T) {
	h := newHandle(nil, agent.Spec{Prompt: "x"}, NewSpawnPlan(agent.Spec{Prompt: "x"}, t.TempDir()), HandleOptions{})
	if err := h.Inject(context.Background(), "hello"); err == nil {
		t.Fatal("expected error injecting with no running turn, got nil")
	}
}

// TestHandle_InjectSteersRunningTurn drives Inject while a turn is
// foregrounded: it must send turn/steer carrying the expectedTurnId
// race guard plus the injected text.
func TestHandle_InjectSteersRunningTurn(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Wait until the handle foregrounds the turn, then steer it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		hh := h.(*Handle)
		hh.turnMu.RLock()
		turn := hh.currentTurnID
		hh.turnMu.RUnlock()
		if turn != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn never foregrounded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	hh := h.(*Handle)
	hh.turnMu.RLock()
	expected := hh.currentTurnID
	hh.turnMu.RUnlock()
	if err := h.Inject(ctx, "steered text"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Errorf("terminal result missing or failed: %+v", res)
	}

	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	params := append([]map[string]any(nil), fh.params...)
	fh.mu.Unlock()
	sawSteer := false
	for i, m := range methods {
		if m != "turn/steer" {
			continue
		}
		if params[i]["expectedTurnId"] != expected {
			t.Errorf("turn/steer expectedTurnId=%v, want %s", params[i]["expectedTurnId"], expected)
		}
		input, _ := params[i]["input"].([]any)
		found := false
		for _, part := range input {
			if pm, ok := part.(map[string]any); ok && pm["text"] == "steered text" {
				found = true
			}
		}
		if !found {
			t.Errorf("turn/steer input=%v, want the steered text", params[i]["input"])
		}
		sawSteer = true
	}
	if !sawSteer {
		t.Errorf("turn/steer never reached the host; methods=%v", methods)
	}
}

func isSessionGoneErr(err error) bool {
	for err != nil {
		if err == agent.ErrSessionNotFound {
			return true
		}
		if u, ok := err.(interface{ Unwrap() []error }); ok {
			for _, inner := range u.Unwrap() {
				if isSessionGoneErr(inner) {
					return true
				}
			}
			return false
		}
		if u1, ok := err.(interface{ Unwrap() error }); ok {
			err = u1.Unwrap()
			continue
		}
		return false
	}
	return false
}
