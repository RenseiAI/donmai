package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
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

// firstTurnDiagnosticLines returns only the log lines carrying the
// first-turn diagnostic marker. Level and method pins must inspect
// the marker line itself: the surrounding output carries unrelated
// WARN/DEBUG lines (serve exit, handshake) that would mask a level
// or method regression behind a whole-output substring check.
func firstTurnDiagnosticLines(t *testing.T, out string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "first-turn request shape") {
			lines = append(lines, line)
		}
	}
	return lines
}

// firstTurnPayloadFromTextLine decodes the bounded shape JSON carried in
// the emitted diagnostic's shape attribute (slog TextHandler quoting).
// It pins the wire payload itself: deleting an encoder assignment that
// serializes a flag removes the key from this struct, so the check fails.
func firstTurnPayloadFromTextLine(t *testing.T, line string) firstTurnDiagnosticData {
	t.Helper()
	const key = `shape="`
	idx := strings.Index(line, key)
	if idx < 0 {
		t.Fatalf("diagnostic line has no shape attribute; line=%q", line)
	}
	start := idx + len(`shape=`)
	// The TextHandler quotes the payload with Go syntax: inner quotes
	// appear escaped (\") while the outer closing quote follows the
	// payload's final } unescaped. Scan for that unescaped closer.
	end := -1
	escaped := false
	for i := start + 1; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			end = i
			break
		}
	}
	if end < 0 {
		t.Fatalf("diagnostic line has unterminated shape attribute; line=%q", line)
	}
	quoted := line[start : end+1]
	raw, err := strconv.Unquote(quoted)
	if err != nil {
		t.Fatalf("shape attribute is not Go-quoted: %v; line=%q", err, line)
	}
	var data firstTurnDiagnosticData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("shape payload is not JSON: %v; payload=%q", err, raw)
	}
	return data
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
			if !call.TurnCompleted || call.InputTokens != 270 || call.ReasoningTokens != 15 {
				t.Errorf("aggregate=%+v", call)
			}
			continue
		}
		calls++
		if call.UsageSource != agent.LlmUsageProvider || call.TurnCompleted || call.Model != "step-model" || call.System != "muse" || call.InputTokens != 90 || call.OutputTokens != 50 || call.CachedInputTokens != 10 || call.CacheWriteTokens != 4 || call.ReasoningTokens != 5 || call.ObservedCostUsd == nil || *call.ObservedCostUsd != 0.25 {
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
		lines := firstTurnDiagnosticLines(t, out)
		if len(lines) != 1 {
			t.Fatalf("failed diagnostic emissions=%d, want exactly 1; logs=%q", len(lines), out)
		}
		line := lines[0]
		if !strings.Contains(line, "level=WARN") {
			t.Errorf("failed diagnostic line is not WARN; line=%q", line)
		}
		if strings.Contains(line, "level=DEBUG") {
			t.Errorf("failed diagnostic line must not be DEBUG; line=%q", line)
		}
		if !strings.Contains(line, "errorKind=overloaded") {
			t.Errorf("failed diagnostic line has no classified errorKind; line=%q", line)
		}
		if strings.Contains(line, "errorKind=unknown") {
			t.Errorf("failed diagnostic line collapsed kind to unknown; line=%q", line)
		}
		if !strings.Contains(line, "errorMethod=turn/start") {
			t.Errorf("failed diagnostic line has no turn/start errorMethod; line=%q", line)
		}
		if strings.Contains(line, "errorMethod=unknown") {
			t.Errorf("failed diagnostic line collapsed method to unknown; line=%q", line)
		}
		if !strings.Contains(line, "sessionMethod=session/start") {
			t.Errorf("failed diagnostic line has no session/start sessionMethod; line=%q", line)
		}
		if hh, ok := h.(*Handle); !ok {
			t.Fatalf("Spawn handle is %T, want *Handle", h)
		} else if shape, _ := hh.storedFirstTurnShape(); shape.SessionMethod != "session/start" {
			t.Errorf("stored SessionMethod=%q, want session/start", shape.SessionMethod)
		} else if shape.InputPartCount == 0 {
			t.Errorf("stored InputPartCount=0, want populated shape not an empty marker")
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
		lines := firstTurnDiagnosticLines(t, out)
		if len(lines) != 1 {
			t.Fatalf("success diagnostic emissions=%d, want exactly 1; logs=%q", len(lines), out)
		}
		line := lines[0]
		if !strings.Contains(line, "level=DEBUG") {
			t.Errorf("success diagnostic line is not DEBUG; line=%q", line)
		}
		if strings.Contains(line, "level=WARN") {
			t.Errorf("success diagnostic line must not be WARN failure; line=%q", line)
		}
		if strings.Contains(line, "errorKind=") {
			t.Errorf("success diagnostic line must carry no errorKind; line=%q", line)
		}
		if strings.Contains(line, "errorMethod=") {
			t.Errorf("success diagnostic line must carry no errorMethod; line=%q", line)
		}
		if !strings.Contains(line, "sessionMethod=session/start") {
			t.Errorf("success diagnostic line has no session/start sessionMethod; line=%q", line)
		}
		if hh, ok := h.(*Handle); !ok {
			t.Fatalf("Spawn handle is %T, want *Handle", h)
		} else if shape, _ := hh.storedFirstTurnShape(); shape.SessionMethod != "session/start" {
			t.Errorf("stored SessionMethod=%q, want session/start", shape.SessionMethod)
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
	lines := firstTurnDiagnosticLines(t, out)
	if len(lines) != 1 {
		t.Fatalf("resume diagnostic emissions=%d, want exactly 1; logs=%q", len(lines), out)
	}
	line := lines[0]
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("resume diagnostic line is not WARN; line=%q", line)
	}
	if strings.Contains(line, "level=DEBUG") {
		t.Errorf("resume failure line must not be DEBUG; line=%q", line)
	}
	if !strings.Contains(line, "errorKind=overloaded") {
		t.Errorf("resume diagnostic line has no classified errorKind; line=%q", line)
	}
	if strings.Contains(line, "errorKind=unknown") {
		t.Errorf("resume diagnostic line collapsed kind to unknown; line=%q", line)
	}
	if !strings.Contains(line, "errorMethod=turn/start") {
		t.Errorf("resume diagnostic line has no turn/start errorMethod; line=%q", line)
	}
	if strings.Contains(line, "errorMethod=unknown") {
		t.Errorf("resume diagnostic line collapsed method to unknown; line=%q", line)
	}
	if !strings.Contains(line, "sessionMethod=session/resume") {
		t.Errorf("resume diagnostic line has no session/resume sessionMethod; line=%q", line)
	}
	if hh, ok := h.(*Handle); !ok {
		t.Fatalf("Resume handle is %T, want *Handle", h)
	} else if shape, _ := hh.storedFirstTurnShape(); shape.SessionMethod != "session/resume" {
		t.Errorf("stored SessionMethod=%q, want session/resume", shape.SessionMethod)
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
		lines := firstTurnDiagnosticLines(t, out)
		if len(lines) != 1 {
			t.Fatalf("sync session emissions=%d, want exactly 1; logs=%q", len(lines), out)
		}
		line := lines[0]
		if !strings.Contains(line, "level=WARN") {
			t.Errorf("sync session line is not WARN; line=%q", line)
		}
		if strings.Contains(line, "level=DEBUG") {
			t.Errorf("sync session failure line must not be DEBUG; line=%q", line)
		}
		if !strings.Contains(line, "errorMethod=session/start") {
			t.Errorf("sync session line has no session/start errorMethod; line=%q", line)
		}
		if strings.Contains(line, "errorMethod=unknown") {
			t.Errorf("sync session line collapsed method to unknown; line=%q", line)
		}
		if !strings.Contains(line, "errorKind=internal") {
			t.Errorf("sync session line has no classified errorKind=internal; line=%q", line)
		}
		if strings.Contains(line, "errorKind=unknown") {
			t.Errorf("sync session line collapsed kind to unknown; line=%q", line)
		}
		if !strings.Contains(line, "sessionMethod=session/start") {
			t.Errorf("sync session line has no session/start sessionMethod; line=%q", line)
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
		lines := firstTurnDiagnosticLines(t, out)
		if len(lines) != 1 {
			t.Fatalf("sync turn emissions=%d, want exactly 1; logs=%q", len(lines), out)
		}
		line := lines[0]
		if !strings.Contains(line, "level=WARN") {
			t.Errorf("sync turn line is not WARN; line=%q", line)
		}
		if strings.Contains(line, "level=DEBUG") {
			t.Errorf("sync turn failure line must not be DEBUG; line=%q", line)
		}
		if !strings.Contains(line, "errorMethod=turn/start") {
			t.Errorf("sync turn line has no turn/start errorMethod; line=%q", line)
		}
		if strings.Contains(line, "errorMethod=unknown") {
			t.Errorf("sync turn line collapsed method to unknown; line=%q", line)
		}
		if !strings.Contains(line, "errorKind=overloaded") {
			t.Errorf("sync turn line has no classified errorKind=overloaded; line=%q", line)
		}
		if strings.Contains(line, "errorKind=unknown") {
			t.Errorf("sync turn line collapsed kind to unknown; line=%q", line)
		}
		if !strings.Contains(line, "sessionMethod=session/start") {
			t.Errorf("sync turn line has no session/start sessionMethod; line=%q", line)
		}
	})
}

// TestHandle_MalformedModelNeverEntersDiagnostic pins the
// model-character filter through the production Spawn entry point: a
// Spec.Model carrying spaces, newlines, slashes or other non-token
// characters must never enter the shape summary. Disabling the
// validatedModelID rejection branch makes the stored ModelID carry
// the raw value and the capture assertions fail.
func TestHandle_MalformedModelNeverEntersDiagnostic(t *testing.T) {
	captureAtDebug := func(t *testing.T) (*bytes.Buffer, func()) {
		t.Helper()
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		return &logs, func() { slog.SetDefault(prev) }
	}

	malformed := []string{
		"bad model",
		"bad\nmodel",
		"bad\tmodel",
		"bad/model",
		"bad;model",
		"-",
	}
	for _, model := range malformed {
		t.Run("model "+strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(model, "\n", "<nl>"), "\t", "<tab>"), " ", "<sp>"), func(t *testing.T) {
			logs, restore := captureAtDebug(t)
			defer restore()
			p, _ := newTestProvider(t, "single-turn")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Model: model, Cwd: t.TempDir(), Autonomous: true})
			if err != nil {
				t.Fatalf("Spawn with malformed model %q: %v", model, err)
			}
			events := drainEvents(t, h.Events(), 10*time.Second)
			if res, ok := terminalResult(events); !ok || !res.Success {
				t.Fatalf("terminal result missing or failed: %+v", res)
			}
			out := logs.String()
			if !strings.Contains(out, "first-turn request shape") {
				t.Fatalf("malformed model run emitted no diagnostic; logs=%q", out)
			}
			hh := h.(*Handle)
			shape, _ := hh.storedFirstTurnShape()
			if shape.ModelID != "" {
				t.Errorf("malformed model %q copied into diagnostic ModelID=%q, want empty", model, shape.ModelID)
			}
			// The space variant is JSON-stable, so the log must
			// not carry it verbatim either; newline/tab forms
			// are pinned via the stored shape above because
			// JSON escapes them.
			if model == "bad model" && strings.Contains(out, model) {
				t.Errorf("diagnostic copied malformed model %q; logs=%q", model, out)
			}
		})
	}

	t.Run("valid control still recorded", func(t *testing.T) {
		logs, restore := captureAtDebug(t)
		defer restore()
		p, _ := newTestProvider(t, "single-turn")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Model: DefaultModel, Cwd: t.TempDir(), Autonomous: true})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		events := drainEvents(t, h.Events(), 10*time.Second)
		if res, ok := terminalResult(events); !ok || !res.Success {
			t.Fatalf("terminal result missing or failed: %+v", res)
		}
		hh := h.(*Handle)
		shape, _ := hh.storedFirstTurnShape()
		if shape.ModelID != DefaultModel {
			t.Errorf("ModelID=%q, want validated default %q", shape.ModelID, DefaultModel)
		}
		if out := logs.String(); !strings.Contains(out, "first-turn request shape") {
			t.Errorf("valid control emitted no diagnostic; logs=%q", out)
		}
	})

	t.Run("overlong model never enters diagnostic", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			model string
		}{
			{"129 chars", strings.Repeat("a", 129)},
			{"500 chars", strings.Repeat("b", 500)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				logs, restore := captureAtDebug(t)
				defer restore()
				p, _ := newTestProvider(t, "single-turn")
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Model: tc.model, Cwd: t.TempDir(), Autonomous: true})
				if err != nil {
					t.Fatalf("Spawn with overlong model: %v", err)
				}
				events := drainEvents(t, h.Events(), 10*time.Second)
				if res, ok := terminalResult(events); !ok || !res.Success {
					t.Fatalf("terminal result missing or failed: %+v", res)
				}
				out := logs.String()
				if !strings.Contains(out, "first-turn request shape") {
					t.Fatalf("overlong model run emitted no diagnostic; logs=%q", out)
				}
				hh := h.(*Handle)
				shape, _ := hh.storedFirstTurnShape()
				if shape.ModelID != "" {
					t.Errorf("overlong model (%d chars) copied into diagnostic ModelID=%q, want empty", len(tc.model), shape.ModelID)
				}
				if strings.Contains(out, tc.model) {
					t.Errorf("diagnostic copied overlong model (%d chars); logs=%q", len(tc.model), out)
				}
				lines := firstTurnDiagnosticLines(t, out)
				if len(lines) != 1 {
					t.Fatalf("overlong diagnostic emissions=%d, want exactly 1; logs=%q", len(lines), out)
				}
				if strings.Contains(lines[0], "level=WARN") {
					t.Errorf("overlong success line must not be WARN failure; line=%q", lines[0])
				}
			})
		}
	})

	t.Run("128-char boundary still recorded", func(t *testing.T) {
		logs, restore := captureAtDebug(t)
		defer restore()
		boundary := strings.Repeat("c", 128)
		p, _ := newTestProvider(t, "single-turn")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Model: boundary, Cwd: t.TempDir(), Autonomous: true})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		events := drainEvents(t, h.Events(), 10*time.Second)
		if res, ok := terminalResult(events); !ok || !res.Success {
			t.Fatalf("terminal result missing or failed: %+v", res)
		}
		hh := h.(*Handle)
		shape, _ := hh.storedFirstTurnShape()
		if shape.ModelID != boundary {
			t.Errorf("ModelID length=%d, want 128-char boundary recorded", len(shape.ModelID))
		}
		if out := logs.String(); !strings.Contains(out, "first-turn request shape") {
			t.Errorf("boundary control emitted no diagnostic; logs=%q", out)
		}
	})
}

// TestHandle_ResumeSyncFailureEmitsDiagnostic pins the synchronous
// session/resume door through the production Resume entry point: a
// host refusing session/resume must still emit one bounded warn
// diagnostic without leaking the host error text, and Resume must
// keep failing with the typed session-gone error. Deleting the
// emitFirstTurnSyncFailure call on the resume path leaves the suite
// green without this test; with it the capture assertion fails.
func TestHandle_ResumeSyncFailureEmitsDiagnostic(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	p, fh := newTestProvider(t, "single-turn")
	fh.mu.Lock()
	fh.resumeErr = &rpcError{Code: -32000, Message: "SENTINEL-RESUME-SYNC-9f8a7b6c5d4e"}
	fh.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Resume(ctx, "22222222-2222-7222-8222-222222222222", agent.Spec{Prompt: "continue", Cwd: t.TempDir(), Autonomous: true})
	if err == nil {
		t.Fatal("expected resume failure, got nil")
	}
	if !isSessionGoneErr(err) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "first-turn request shape") {
		t.Fatalf("sync resume failure emitted no diagnostic; logs=%q", out)
	}
	if !strings.Contains(out, "session/resume") {
		t.Errorf("sync resume diagnostic has no resume method; logs=%q", out)
	}
	if strings.Contains(out, "SENTINEL-RESUME-SYNC-9f8a7b6c5d4e") {
		t.Errorf("sync resume diagnostic copied host error text; logs=%q", out)
	}
	lines := firstTurnDiagnosticLines(t, out)
	if len(lines) != 1 {
		t.Fatalf("sync resume emissions=%d, want exactly 1; logs=%q", len(lines), out)
	}
	line := lines[0]
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("sync resume line is not WARN; line=%q", line)
	}
	if strings.Contains(line, "level=DEBUG") {
		t.Errorf("sync resume failure line must not be DEBUG; line=%q", line)
	}
	if !strings.Contains(line, "errorMethod=session/resume") {
		t.Errorf("sync resume line has no session/resume errorMethod; line=%q", line)
	}
	if strings.Contains(line, "errorMethod=unknown") {
		t.Errorf("sync resume line collapsed method to unknown; line=%q", line)
	}
	if !strings.Contains(line, "errorKind=sessionNotFound") {
		t.Errorf("sync resume line has no classified errorKind=sessionNotFound; line=%q", line)
	}
	if strings.Contains(line, "errorKind=unknown") {
		t.Errorf("sync resume line collapsed kind to unknown; line=%q", line)
	}
	if !strings.Contains(line, "sessionMethod=session/resume") {
		t.Errorf("sync resume line has no session/resume sessionMethod; line=%q", line)
	}
}

// TestHandle_FirstTurnDiagnosticCorrelatesTurnID pins the B1 fix: a
// completion for an older turn must not emit the queued first-turn
// shape nor consume its once flag. The older terminal ResultEvent
// still flows (existing contract); only the diagnostic is gated.
func TestHandle_FirstTurnDiagnosticCorrelatesTurnID(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	h := newHandle(nil, agent.Spec{Prompt: "x"}, NewSpawnPlan(agent.Spec{Prompt: "x"}, t.TempDir()), HandleOptions{})
	sid := "33333333-3333-7333-8333-333333333333"
	h.setSessionID(sid)
	queuedID := "44444444-4444-7444-8444-444444444444"
	oldID := "11111111-1111-7111-8111-111111111111"
	h.recordSessionShape("session/resume", sessionResumeParams(sid, "c", nil))
	h.recordTurnShape(turnStartParams(sid, "c", agent.Spec{Prompt: "x"}, []TurnInputPart{{Type: "text"}}, []string{t.TempDir()}), queuedID)
	h.setCurrentTurnID(queuedID)
	oldParams, _ := json.Marshal(map[string]any{"sessionId": sid, "turnId": oldID, "terminal": "completed", "viewCursor": "c"})
	if done := h.handleNotification(context.Background(), notification{Method: "turn/completed", Params: oldParams}); done {
		t.Fatalf("stale turn completion terminated the session; want non-terminal so the queued first turn still reports")
	}
	if got := strings.Count(logs.String(), "first-turn request shape"); got != 0 {
		t.Fatalf("stale turn completion emitted queued shape; logs=%q", logs.String())
	}
	h.turnMu.RLock()
	foreground := h.currentTurnID
	h.turnMu.RUnlock()
	if foreground != queuedID {
		t.Fatalf("stale completion cleared foreground turn %q, want %q (queued first turn still tracked)", foreground, queuedID)
	}
	queuedParams, _ := json.Marshal(map[string]any{"sessionId": sid, "turnId": queuedID, "terminal": "completed", "viewCursor": "c2"})
	if done := h.handleNotification(context.Background(), notification{Method: "turn/completed", Params: queuedParams}); !done {
		t.Fatalf("queued first-turn completion did not terminate; want terminal so the session ends")
	}
	if got := strings.Count(logs.String(), "first-turn request shape"); got != 1 {
		t.Fatalf("queued turn completion emissions=%d, want 1 (stale must not consume flag); logs=%q", got, logs.String())
	}
	h.closeEvents()
}

// TestHandle_QueuedFirstTurnSurvivesStaleCompletionViaResume pins the
// production lifecycle: through the Resume entry point with the fake
// transport, a stale older success must not terminate the session
// before the acknowledged queued first turn fails. Removing the stale
// terminal gate makes the stale success terminate with zero diagnostics.
func TestHandle_QueuedFirstTurnSurvivesStaleCompletionViaResume(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	p, fh := newTestProvider(t, "stale-queued-failure")
	fh.sessionID = "22222222-2222-7222-8222-222222222222"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Resume(ctx, "22222222-2222-7222-8222-222222222222", agent.Spec{Prompt: "continue", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	res, ok := terminalResult(events)
	if !ok {
		t.Fatalf("no terminal ResultEvent; kinds=%v", kindsOf(events))
	}
	if res.Success {
		t.Fatalf("stale success terminated the session as success; want the queued first-turn failure (errors=%v)", res.Errors)
	}
	if res.ErrorSubtype != "overloaded" {
		t.Errorf("ErrorSubtype=%q, want overloaded (queued first-turn failure)", res.ErrorSubtype)
	}
	lines := firstTurnDiagnosticLines(t, logs.String())
	if len(lines) != 1 {
		t.Fatalf("diagnostic emissions=%d, want 1 after stale success plus queued failure; logs=%q", len(lines), logs.String())
	}
	if !strings.Contains(lines[0], "errorKind=overloaded") {
		t.Errorf("queued failure line has no errorKind=overloaded; line=%q", lines[0])
	}
	if !strings.Contains(lines[0], "sessionMethod=session/resume") {
		t.Errorf("queued failure line has no session/resume sessionMethod; line=%q", lines[0])
	}
}

// TestHandle_FirstTurnDiagnosticRejectsUnknownTokens pins the token
// allowlist through the Handle record doors: unknown approval,
// provider, effort, busy and input-type tokens never enter the stored
// shape. Deleting the validatedToken gate copies the sentinel.
func TestHandle_FirstTurnDiagnosticRejectsUnknownTokens(t *testing.T) {
	const sentinel = "SENTINEL-UNKNOWN-TOKEN-9f8a7b6c5d4e"
	h := newHandle(nil, agent.Spec{Prompt: "x"}, NewSpawnPlan(agent.Spec{Prompt: "x"}, t.TempDir()), HandleOptions{})
	sessionParams := map[string]any{
		"sessionId": "s", "commandId": "c", "workspaceRoot": t.TempDir(),
		"modelId":      DefaultModel,
		"providerId":   sentinel,
		"approvalMode": sentinel,
		"config":       map[string]any{},
	}
	h.recordSessionShape("session/start", sessionParams)
	turnParams := map[string]any{
		"sessionId": "s", "commandId": "c",
		"input":           []map[string]any{{"type": sentinel, "text": "x"}, {"type": "text", "text": "y"}},
		"ifBusy":          sentinel,
		"reasoningEffort": sentinel,
		"displayText":     "hi",
		"workspaceRoots":  []string{t.TempDir()},
	}
	h.recordTurnShape(turnParams, "turn-9")
	shape, _ := h.storedFirstTurnShape()
	if shape.ProviderID == sentinel || shape.ApprovalMode == sentinel || shape.ReasoningEffort == sentinel || shape.IfBusy == sentinel {
		t.Fatalf("unknown token copied into shape: %+v", shape)
	}
	if shape.ProviderID != "" || shape.ApprovalMode != "" || shape.ReasoningEffort != "" || shape.IfBusy != "" {
		t.Fatalf("unknown tokens must leave validated fields empty: %+v", shape)
	}
	for _, typ := range shape.InputPartTypes {
		if typ == sentinel {
			t.Fatalf("unknown input type copied into shape: %+v", shape.InputPartTypes)
		}
	}
	if len(shape.InputPartTypes) != 2 || shape.InputPartTypes[0] != "unknown" || shape.InputPartTypes[1] != "text" {
		t.Fatalf("InputPartTypes=%v, want [unknown text]", shape.InputPartTypes)
	}
	payload := encodeFirstTurnDiagnostic(shape, nil)
	if strings.Contains(payload, sentinel) {
		t.Fatalf("diagnostic payload leaked sentinel; payload=%q", payload)
	}
	h.closeEvents()
}

// TestHandle_FirstTurnDiagnosticBoundsCounts pins both ceilings
// through production doors: the 16-type cap via the Handle turn door
// and the 9999 cap via Spawn with many servers. Deleting either cap
// copies the overflow.
func TestHandle_FirstTurnDiagnosticBoundsCounts(t *testing.T) {
	t.Run("input types capped at 16 via handle", func(t *testing.T) {
		h := newHandle(nil, agent.Spec{Prompt: "x"}, NewSpawnPlan(agent.Spec{Prompt: "x"}, t.TempDir()), HandleOptions{})
		parts := make([]map[string]any, 0, 20)
		for i := 0; i < 20; i++ {
			parts = append(parts, map[string]any{"type": "text", "text": "x"})
		}
		h.recordTurnShape(map[string]any{"sessionId": "s", "commandId": "c", "input": parts}, "turn-cap")
		shape, _ := h.storedFirstTurnShape()
		if shape.InputPartCount != 20 {
			t.Fatalf("InputPartCount=%d, want 20", shape.InputPartCount)
		}
		if len(shape.InputPartTypes) != 16 {
			t.Fatalf("InputPartTypes len=%d, want 16 (capped); got %v", len(shape.InputPartTypes), shape.InputPartTypes)
		}
		h.closeEvents()
	})
	t.Run("mcp count capped at 9999 via spawn", func(t *testing.T) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		servers := make([]agent.MCPServerConfig, 0, 10001)
		for i := 0; i < 10001; i++ {
			servers = append(servers, agent.MCPServerConfig{Name: "srv", Type: "stdio", Command: "true"})
		}
		// Names must be unique or the translator collapses them.
		for i := range servers {
			servers[i].Name = fmt.Sprintf("srv-%05d", i)
		}
		p, _ := newTestProvider(t, "single-turn")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true, MCPServers: servers})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		events := drainEvents(t, h.Events(), 15*time.Second)
		if res, ok := terminalResult(events); !ok || !res.Success {
			t.Fatalf("terminal result missing or failed: %+v", res)
		}
		hh := h.(*Handle)
		shape, _ := hh.storedFirstTurnShape()
		if shape.MCPServerCount != 9999 {
			t.Fatalf("MCPServerCount=%d, want 9999 (capped)", shape.MCPServerCount)
		}
		if out := logs.String(); !strings.Contains(out, "first-turn request shape") {
			t.Fatalf("no diagnostic emitted; logs=%q", out)
		}
	})
}

// TestHandle_FirstTurnDiagnosticCancelledAndQuota pins the async
// error side through production doors: cancelled via Spawn with the
// interrupt script, quota and request-denied via the Handle
// notification door. Deleting any branch collapses the assertion.
func TestHandle_FirstTurnDiagnosticCancelledAndQuota(t *testing.T) {
	t.Run("cancelled via spawn", func(t *testing.T) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		p, _ := newTestProvider(t, "interrupt")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		events := drainEvents(t, h.Events(), 10*time.Second)
		if res, ok := terminalResult(events); !ok || res.Success {
			t.Fatalf("interrupt terminal missing or successful: %+v", res)
		}
		lines := firstTurnDiagnosticLines(t, logs.String())
		if len(lines) != 1 {
			t.Fatalf("cancelled emissions=%d, want 1; logs=%q", len(lines), logs.String())
		}
		if !strings.Contains(lines[0], "errorKind=cancelled") {
			t.Errorf("cancelled line has no errorKind=cancelled; line=%q", lines[0])
		}
		if strings.Contains(lines[0], "interrupted by user") {
			t.Errorf("cancelled line copied reason text; line=%q", lines[0])
		}
	})
	t.Run("quota and request-denied via handle", func(t *testing.T) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		h := newHandle(nil, agent.Spec{Prompt: "x"}, NewSpawnPlan(agent.Spec{Prompt: "x"}, t.TempDir()), HandleOptions{})
		sid := "55555555-5555-7555-8555-555555555555"
		turnID := "66666666-6666-7666-8666-666666666666"
		h.setSessionID(sid)
		h.recordSessionShape("session/start", sessionStartParams(agent.Spec{Prompt: "x"}, "s", "c", t.TempDir(), nil))
		h.recordTurnShape(turnStartParams(sid, "c", agent.Spec{Prompt: "x"}, []TurnInputPart{{Type: "text"}}, []string{t.TempDir()}), turnID)
		quota := json.RawMessage(`{"usd":1}`)
		denied := true
		turnErr := &TurnError{Kind: "overloaded", Message: "HOST-SENTINEL-QUOTA-9f8a7b6c5d4e", Retryable: true, Quota: quota, RequestDenied: &denied}
		params, _ := json.Marshal(map[string]any{
			"sessionId": sid, "turnId": turnID, "terminal": "failed", "viewCursor": "c",
			"error": map[string]any{"kind": "overloaded", "message": "HOST-SENTINEL-QUOTA-9f8a7b6c5d4e", "retryable": true, "quota": map[string]any{"usd": 1}, "requestDenied": true},
		})
		_ = turnErr
		_ = quota
		_ = denied
		if done := h.handleNotification(context.Background(), notification{Method: "turn/completed", Params: params}); !done {
			t.Fatalf("quota failure did not terminate; want terminal ResultEvent")
		}
		lines := firstTurnDiagnosticLines(t, logs.String())
		if len(lines) != 1 {
			t.Fatalf("quota emissions=%d, want 1; logs=%q", len(lines), logs.String())
		}
		if !strings.Contains(lines[0], "errorKind=overloaded") {
			t.Errorf("quota line has no errorKind=overloaded; line=%q", lines[0])
		}
		if strings.Contains(logs.String(), "HOST-SENTINEL-QUOTA-9f8a7b6c5d4e") {
			t.Errorf("quota line copied host text; logs=%q", logs.String())
		}
		// The emitted payload itself must carry the retryable, quota
		// and request-denied flags without the raw body. Deleting any
		// of the three encoder assignments drops the key and fails.
		emitted := firstTurnPayloadFromTextLine(t, lines[0])
		if emitted.ErrorRetryable == nil || !*emitted.ErrorRetryable {
			t.Errorf("emitted payload lost errorRetryable; payload=%+v; line=%q", emitted, lines[0])
		}
		if !emitted.ErrorHasQuota {
			t.Errorf("emitted payload lost errorHasQuota; payload=%+v; line=%q", emitted, lines[0])
		}
		if !emitted.ErrorHasRequestDenied {
			t.Errorf("emitted payload lost errorHasRequestDenied; payload=%+v; line=%q", emitted, lines[0])
		}
		if emitted.ErrorKind != "overloaded" {
			t.Errorf("emitted payload ErrorKind=%q, want overloaded; payload=%+v", emitted.ErrorKind, emitted)
		}
		h.closeEvents()
	})
}

// TestHandle_FirstTurnDiagnosticEmitsOnce pins the once flag through
// a Spawned handle: the async success consumes the flag, so a second
// emit with the same turn stays silent. Deleting the flag emits twice.
func TestHandle_FirstTurnDiagnosticEmitsOnce(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	p, _ := newTestProvider(t, "single-turn")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Fatalf("terminal result missing or failed: %+v", res)
	}
	if got := strings.Count(logs.String(), "first-turn request shape"); got != 1 {
		t.Fatalf("spawn emissions=%d, want 1; logs=%q", got, logs.String())
	}
	hh := h.(*Handle)
	hh.firstTurn.mu.Lock()
	firstID := hh.firstTurn.firstTurnID
	hh.firstTurn.mu.Unlock()
	if firstID == "" {
		t.Fatalf("spawn recorded no first-turn id")
	}
	hh.emitFirstTurnAsyncCompletion(firstID, TurnTerminalCompleted, nil, nil)
	if got := strings.Count(logs.String(), "first-turn request shape"); got != 1 {
		t.Fatalf("second emit with same turn produced %d diagnostics, want 1 (once flag); logs=%q", got, logs.String())
	}
}
