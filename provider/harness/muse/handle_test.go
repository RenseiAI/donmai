package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
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

// TestHandle_ApprovalDeclineDecidesAbort drives the decline path
// through Spawn: a deny-by-default bridge policy against the live
// shell choice set (approved + abort) must send approval/decide
// carrying the abort choice — the denial vocabulary live hosts
// actually offer. The server-request presentation still gets only
// its `{}` receipt (no second decide).
func TestHandle_ApprovalDeclineDecidesAbort(t *testing.T) {
	p, fh := newTestProvider(t, "approval")
	fh.mu.Lock()
	fh.denyApproval = true
	fh.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		Prompt:           "run echo",
		Cwd:              t.TempDir(),
		Autonomous:       true,
		PermissionConfig: &agent.PermissionConfig{DefaultDecision: "deny"},
	})
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
	decides := 0
	sawDecideAbort := false
	for i, m := range methods {
		if m != "approval/decide" {
			continue
		}
		decides++
		if params[i]["approvalId"] == "appr-1" && params[i]["choiceId"] == "abort" {
			sawDecideAbort = true
		}
	}
	if !sawDecideAbort {
		t.Errorf("approval/decide with the abort choice never reached the host; methods=%v params=%v", methods, params)
	}
	if decides != 1 {
		t.Errorf("approval/decide sent %d times, want exactly once (notification drives it; the server-request takes only the receipt)", decides)
	}
	// The decline must surface in the event stream as a denial.
	sawDenied := false
	for _, ev := range events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == "approval_denied" {
			sawDenied = true
		}
	}
	if !sawDenied {
		t.Errorf("no approval_denied system event; kinds=%v", kindsOf(events))
	}
}

// TestHandle_UserInputRequestAnsweredWithReceipt drives a
// userInput/request server-request through Spawn: the handle must
// answer `{}` (verified as a response on the wire is covered at the
// client layer) and record the prompt as a user_input_requested
// system event, then run to its terminal without stalling.
func TestHandle_UserInputRequestAnsweredWithReceipt(t *testing.T) {
	p, fh := newTestProvider(t, "user-input")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Errorf("terminal result missing or failed: %+v", res)
	}
	sawPrompt := false
	for _, ev := range events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == "user_input_requested" {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Errorf("no user_input_requested system event; kinds=%v", kindsOf(events))
	}

	_ = fh
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
	// "steer" holds the turn open until turn/steer arrives; a turn that
	// completes on its own races the steer against handle teardown.
	p, fh := newTestProvider(t, "steer")

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

func TestHandle_StepUsage(t *testing.T) {
	p, _ := newTestProvider(t, "step-usage")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatal(err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	var total agent.CostData
	calls, aggregates := 0, 0
	for _, ev := range events {
		call, ok := ev.(agent.LlmCallEvent)
		if !ok {
			continue
		}
		if call.UsageSource == agent.LlmUsageAggregate {
			aggregates++
			if !call.TurnCompleted || call.InputTokens != 300 || call.ReasoningTokens != 15 {
				t.Errorf("aggregate=%+v", call)
			}
			continue
		}
		calls++
		if call.UsageSource != agent.LlmUsageProvider || call.TurnCompleted || call.Model != "step-model" || call.System != "muse" || call.InputTokens != 100 || call.OutputTokens != 50 || call.CachedInputTokens != 10 || call.CacheWriteTokens != 4 || call.ReasoningTokens != 5 || call.ObservedCostUsd == nil || *call.ObservedCostUsd != 0.25 {
			t.Fatalf("step %d=%+v", calls, call)
		}
		total.InputTokens += call.InputTokens
		total.OutputTokens += call.OutputTokens
		total.CachedInputTokens += call.CachedInputTokens
		total.CacheWriteTokens += call.CacheWriteTokens
		total.ReasoningTokens += call.ReasoningTokens
		total.TotalCostUsd += *call.ObservedCostUsd
	}
	if calls != 3 || aggregates != 1 {
		t.Fatalf("calls=%d aggregates=%d, want 3 and 1", calls, aggregates)
	}
	res, ok := terminalResult(events)
	if !ok || res.Cost == nil || !res.Success {
		t.Fatalf("result=%+v", res)
	}
	if total != *res.Cost || total.TotalCostUsd != 0.75 {
		t.Fatalf("step sum=%+v terminal=%+v", total, res.Cost)
	}
}

// TestHandle_FirstTurnFailureEmitsShapeAndSuccessControlEmitsDebug pins
// AC-1: a failed first turn through the production Spawn entry point
// emits a correlated shape summary, and a successful control emits a
// comparable debug summary. Removing the production emission makes the
// capture assertions fail.
func TestHandle_FirstTurnFailureEmitsShapeAndSuccessControlEmitsDebug(t *testing.T) {
	captureAtDebug := func(t *testing.T) (*bytes.Buffer, func()) {
		t.Helper()
		var logs bytes.Buffer
		prev := slog.Default()
		handler := slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})
		slog.SetDefault(slog.New(handler))
		return &logs, func() { slog.SetDefault(prev) }
	}

	t.Run("failed", func(t *testing.T) {
		logs, restore := captureAtDebug(t)
		defer restore()
		p, _ := newTestProvider(t, "failed")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		events := drainEvents(t, h.Events(), 10*time.Second)
		res, ok := terminalResult(events)
		if !ok || res.Success {
			t.Fatalf("terminal result missing or unexpectedly successful: %+v", res)
		}
		if res.ErrorSubtype != "overloaded" {
			t.Errorf("ErrorSubtype=%q, want overloaded", res.ErrorSubtype)
		}
		out := logs.String()
		if !strings.Contains(out, "first-turn request shape") {
			t.Fatalf("failed first turn emitted no shape diagnostic; logs=%q", out)
		}
		if !strings.Contains(out, "session/start") {
			t.Errorf("failed diagnostic has no correlated session method; logs=%q", out)
		}
		if !strings.Contains(out, "overloaded") {
			t.Errorf("failed diagnostic reports no host error classification; logs=%q", out)
		}
		if strings.Contains(out, "host overloaded") {
			t.Errorf("failed diagnostic copied free-form host text; logs=%q", out)
		}
	})

	t.Run("success", func(t *testing.T) {
		logs, restore := captureAtDebug(t)
		defer restore()
		p, _ := newTestProvider(t, "single-turn")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		events := drainEvents(t, h.Events(), 10*time.Second)
		res, ok := terminalResult(events)
		if !ok || !res.Success {
			t.Fatalf("terminal result missing or failed: %+v", res)
		}
		out := logs.String()
		if !strings.Contains(out, "first-turn request shape") {
			t.Fatalf("successful first turn emitted no debug shape; logs=%q", out)
		}
		if !strings.Contains(out, "session/start") {
			t.Errorf("success diagnostic has no comparable session method; logs=%q", out)
		}
	})
}

// TestHandle_FirstTurnDiagnosticExcludesSecretsAndStaysBounded pins AC-2:
// distinct sentinels in every excluded surface never reach the log, and
// oversize plus duplicate deliveries stay bounded. Including one raw
// value or removing the emission cap makes these assertions fail.
func TestHandle_FirstTurnDiagnosticExcludesSecretsAndStaysBounded(t *testing.T) {
	const (
		promptSentinel  = "PROMPT-SENTINEL-9f8a7b6c5d4eaa01"
		baseSentinel    = "BASE-SENTINEL-9f8a7b6c5d4eaa02"
		appendSentinel  = "APPEND-SENTINEL-9f8a7b6c5d4eaa03"
		ctxSentinel     = "CTX-SENTINEL-9f8a7b6c5d4eaa04"
		envSentinel     = "ENV-SENTINEL-9f8a7b6c5d4eaa05"
		urlSentinel     = "https://sentinel-9f8a7b6c5d4eaa06.invalid/mcp"
		headerSentinel  = "HEADER-SENTINEL-9f8a7b6c5d4eaa07"
		cmdSentinel     = "sentinel-cmd-9f8a7b6c5d4eaa08"
		nameSentinel    = "SESSION-NAME-SENTINEL-9f8a7b6c5d4eaa09"
		hostErrSentinel = "HOST-ERROR-SENTINEL-9f8a7b6c5d4eaa10"
	)
	sentinels := []string{promptSentinel, baseSentinel, appendSentinel, ctxSentinel, envSentinel, urlSentinel, headerSentinel, cmdSentinel, nameSentinel, hostErrSentinel}

	specWithSecrets := func(prompt string) agent.Spec {
		return agent.Spec{
			Prompt:             prompt,
			BaseInstructions:   baseSentinel,
			SystemPromptAppend: appendSentinel,
			InitialContext:     ctxSentinel,
			SessionName:        nameSentinel,
			Cwd:                t.TempDir(),
			Autonomous:         true,
			Env:                map[string]string{"SENTINEL_ENV": envSentinel},
			MCPServers: []agent.MCPServerConfig{
				{Name: "http-srv", Type: "http", URL: urlSentinel, Headers: map[string]string{"Authorization": headerSentinel}},
				{Name: "stdio-srv", Type: "stdio", Command: cmdSentinel, Args: []string{"--sentinel"}, Env: map[string]string{"K": envSentinel}},
			},
		}
	}

	captureAtDebug := func(t *testing.T) (*bytes.Buffer, func()) {
		t.Helper()
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		return &logs, func() { slog.SetDefault(prev) }
	}

	t.Run("failed omits every excluded surface", func(t *testing.T) {
		logs, restore := captureAtDebug(t)
		defer restore()
		p, fh := newTestProvider(t, "failed")
		fh.mu.Lock()
		fh.failedMessage = hostErrSentinel
		fh.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h, err := p.Spawn(ctx, specWithSecrets(promptSentinel))
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		events := drainEvents(t, h.Events(), 10*time.Second)
		if res, ok := terminalResult(events); !ok || res.Success {
			t.Fatalf("terminal result missing or successful: %+v", res)
		}
		out := logs.String()
		if !strings.Contains(out, "first-turn request shape") {
			t.Fatalf("no diagnostic emitted; logs=%q", out)
		}
		for _, s := range sentinels {
			if strings.Contains(out, s) {
				t.Errorf("diagnostic leaked excluded sentinel %q; logs=%q", s, out)
			}
		}
		if len(out) > 8192 {
			t.Errorf("diagnostic log length=%d, want bounded (<8KiB)", len(out))
		}
	})

	t.Run("oversize prompt stays bounded", func(t *testing.T) {
		logs, restore := captureAtDebug(t)
		defer restore()
		p, _ := newTestProvider(t, "single-turn")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		huge := strings.Repeat("x", 20000)
		h, err := p.Spawn(ctx, agent.Spec{Prompt: huge, Cwd: t.TempDir(), Autonomous: true})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		events := drainEvents(t, h.Events(), 10*time.Second)
		if res, ok := terminalResult(events); !ok || !res.Success {
			t.Fatalf("terminal result missing or failed: %+v", res)
		}
		out := logs.String()
		if !strings.Contains(out, "first-turn request shape") {
			t.Fatalf("no diagnostic emitted; logs=%q", out)
		}
		if strings.Contains(out, huge[:100]) {
			t.Error("diagnostic copied oversize prompt text")
		}
		if len(out) > 8192 {
			t.Errorf("oversize diagnostic log length=%d, want bounded", len(out))
		}
	})

	t.Run("duplicate completion emits once", func(t *testing.T) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		h := newHandle(nil, agent.Spec{Prompt: "x"}, NewSpawnPlan(agent.Spec{Prompt: "x"}, t.TempDir()), HandleOptions{})
		sid := "33333333-3333-7333-8333-333333333333"
		h.setSessionID(sid)
		h.recordSessionShape("session/start", sessionStartParams(agent.Spec{Prompt: "x"}, "s", "c", t.TempDir(), nil))
		h.recordTurnShape(turnStartParams("s", "c", agent.Spec{Prompt: "x"}, []TurnInputPart{{Type: "text"}}, []string{t.TempDir()}), "turn-1")
		params, _ := json.Marshal(map[string]any{"sessionId": sid, "turnId": "turn-1", "terminal": "completed", "viewCursor": "c"})
		h.handleNotification(context.Background(), notification{Method: "turn/completed", Params: params})
		h.handleNotification(context.Background(), notification{Method: "turn/completed", Params: params})
		h.closeEvents()
		if got := strings.Count(logs.String(), "first-turn request shape"); got != 1 {
			t.Errorf("diagnostic emissions=%d, want exactly 1 (duplicates stay bounded); logs=%q", got, logs.String())
		}
	})
}

// TestHandle_ResumeFirstTurnDiagnostic pins AC-3: Resume receives the
// same protection, terminal failure stays failure with existing event
// ordering. Bypassing the resume diagnostic hook makes the capture
// assertions fail.
func TestHandle_ResumeFirstTurnDiagnostic(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	p, fh := newTestProvider(t, "failed")
	fh.sessionID = "22222222-2222-7222-8222-222222222222"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Resume(ctx, "22222222-2222-7222-8222-222222222222", agent.Spec{Prompt: "continue", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	res, ok := terminalResult(events)
	if !ok || res.Success {
		t.Fatalf("resume terminal result missing or successful: %+v", res)
	}
	if res.ErrorSubtype != "overloaded" {
		t.Errorf("ErrorSubtype=%q, want overloaded", res.ErrorSubtype)
	}
	if _, ok := events[len(events)-1].(agent.ResultEvent); !ok {
		t.Errorf("last event is %T, want ResultEvent (existing ordering preserved)", events[len(events)-1])
	}
	out := logs.String()
	if !strings.Contains(out, "first-turn request shape") {
		t.Fatalf("resume failed turn emitted no diagnostic; logs=%q", out)
	}
	if !strings.Contains(out, "session/resume") {
		t.Errorf("resume diagnostic has no resume method; logs=%q", out)
	}
}

// TestHandle_FirstTurnSyncFailuresEmitDiagnostic pins the synchronous
// doors: session/start and turn/start RPC failures emit one bounded
// diagnostic without changing the Spawn error contract.
func TestHandle_FirstTurnSyncFailuresEmitDiagnostic(t *testing.T) {
	captureAtDebug := func(t *testing.T) (*bytes.Buffer, func()) {
		t.Helper()
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		return &logs, func() { slog.SetDefault(prev) }
	}

	t.Run("session start", func(t *testing.T) {
		logs, restore := captureAtDebug(t)
		defer restore()
		p, fh := newTestProvider(t, "single-turn")
		fh.mu.Lock()
		fh.sessionStartErr = &rpcError{Code: -32000, Message: "SENTINEL-SYNC-SESSION-9f8a7b6c5d4e"}
		fh.mu.Unlock()
		_, err := p.Spawn(context.Background(), agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
		if err == nil {
			t.Fatal("expected session/start failure, got nil")
		}
		out := logs.String()
		if !strings.Contains(out, "first-turn request shape") {
			t.Fatalf("sync session failure emitted no diagnostic; logs=%q", out)
		}
		if strings.Contains(out, "SENTINEL-SYNC-SESSION-9f8a7b6c5d4e") {
			t.Errorf("diagnostic copied sync error text; logs=%q", out)
		}
	})

	t.Run("turn start", func(t *testing.T) {
		logs, restore := captureAtDebug(t)
		defer restore()
		p, fh := newTestProvider(t, "single-turn")
		fh.mu.Lock()
		fh.turnStartErr = &rpcError{Code: -32000, Message: "SENTINEL-SYNC-TURN-9f8a7b6c5d4e"}
		fh.mu.Unlock()
		_, err := p.Spawn(context.Background(), agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
		if err == nil {
			t.Fatal("expected turn/start failure, got nil")
		}
		out := logs.String()
		if !strings.Contains(out, "first-turn request shape") {
			t.Fatalf("sync turn failure emitted no diagnostic; logs=%q", out)
		}
		if strings.Contains(out, "SENTINEL-SYNC-TURN-9f8a7b6c5d4e") {
			t.Errorf("diagnostic copied sync error text; logs=%q", out)
		}
	})
}
