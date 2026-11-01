package muse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/activity"
)

// appliedEventOf returns the first applied_reasoning_effort system event.
func appliedEventOf(events []agent.Event) (agent.SystemEvent, bool) {
	for _, ev := range events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort {
			return sys, true
		}
	}
	return agent.SystemEvent{}, false
}

// turnStartEffortParam returns the reasoningEffort wire value the fake host
// saw on turn/start, and whether the key was present.
func turnStartEffortParam(t *testing.T, fh *fakeHost) (string, bool) {
	t.Helper()
	fh.mu.Lock()
	defer fh.mu.Unlock()
	for i, m := range fh.methods {
		if m != "turn/start" {
			continue
		}
		if i >= len(fh.params) {
			continue
		}
		v, ok := fh.params[i]["reasoningEffort"]
		if !ok {
			return "", false
		}
		s, _ := v.(string)
		return s, true
	}
	t.Fatalf("fake host never saw turn/start; methods=%v", fh.methods)
	return "", false
}

// postAppliedThroughRealPoster drives one harness-emitted event through the
// real activity poster and returns the captured context pair and content.
func postAppliedThroughRealPoster(t *testing.T, ev agent.Event) (contextPair, content string) {
	t.Helper()
	var gotContext, gotContent, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/activity") {
			w.WriteHeader(http.StatusOK)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Activity struct {
				Type         string `json:"type"`
				Content      string `json:"content"`
				ContextKey   string `json:"contextKey"`
				ContextValue string `json:"contextValue"`
			} `json:"activity"`
		}
		if err := json.Unmarshal(raw, &body); err == nil && body.Activity.ContextKey != "" {
			gotContext = body.Activity.ContextKey + "=" + body.Activity.ContextValue
			gotContent = body.Activity.Content
			gotType = body.Activity.Type
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p, err := activity.New(activity.Config{
		SessionID: "s1", WorkerID: "w1", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	p.Send(context.Background(), ev)
	// Stop drains the queue, so the POST has landed when it returns.
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if gotType != "context" {
		t.Fatalf("poster type = %q, want context (payload=%q %q)", gotType, gotContext, gotContent)
	}
	return gotContext, gotContent
}

// TestAppliedEffort_SpawnHighCapturesHigh pins AC-1's Spawn half: a Spawn
// with high captures high in the actual turn/start request and produces the
// same new activity (high with the adapter-sent source) through the real
// poster, ordered ahead of terminal completion.
func TestAppliedEffort_SpawnHighCapturesHigh(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortHigh})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if got, present := turnStartEffortParam(t, fh); !present || got != "high" {
		t.Fatalf("turn/start reasoningEffort = %q present=%v, want high", got, present)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	applied, ok := appliedEventOf(events)
	if !ok {
		t.Fatalf("no applied_reasoning_effort event; kinds=%v", kindsOf(events))
	}
	if applied.Message != "high" {
		t.Errorf("applied Message = %q, want high", applied.Message)
	}
	// Ordering: the applied evidence rides ahead of terminal completion.
	appliedIdx, terminalIdx := -1, -1
	for i, ev := range events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort && appliedIdx == -1 {
			appliedIdx = i
		}
		if _, ok := ev.(agent.ResultEvent); ok && terminalIdx == -1 {
			terminalIdx = i
		}
	}
	if appliedIdx == -1 || terminalIdx == -1 || appliedIdx > terminalIdx {
		t.Errorf("ordering applied=%d terminal=%d, want applied ahead of terminal; kinds=%v", appliedIdx, terminalIdx, kindsOf(events))
	}
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Fatalf("terminal missing or failed: %+v", res)
	}

	gotCtx, gotContent := postAppliedThroughRealPoster(t, applied)
	if gotCtx != "appliedReasoningEffort=high" {
		t.Errorf("poster context = %q, want appliedReasoningEffort=high", gotCtx)
	}
	if gotContent != "applied reasoning effort: high (as applied)" {
		t.Errorf("poster content = %q, want the fixed applied sentence", gotContent)
	}
}

// TestAppliedEffort_ResumeHighCapturesHigh pins AC-1's Resume half: a resumed
// headless turn uses the same emission path as a new turn.
func TestAppliedEffort_ResumeHighCapturesHigh(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")
	fh.sessionID = "22222222-2222-7222-8222-222222222222"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Resume(ctx, "22222222-2222-7222-8222-222222222222", agent.Spec{Prompt: "continue", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortHigh})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if got, present := turnStartEffortParam(t, fh); !present || got != "high" {
		t.Fatalf("resumed turn/start reasoningEffort = %q present=%v, want high", got, present)
	}

	events := drainEvents(t, h.Events(), 10*time.Second)
	applied, ok := appliedEventOf(events)
	if !ok {
		t.Fatalf("no applied_reasoning_effort on resume; kinds=%v", kindsOf(events))
	}
	if applied.Message != "high" {
		t.Errorf("resumed applied Message = %q, want high", applied.Message)
	}
	appliedIdx, terminalIdx := -1, -1
	for i, ev := range events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort && appliedIdx == -1 {
			appliedIdx = i
		}
		if _, ok := ev.(agent.ResultEvent); ok && terminalIdx == -1 {
			terminalIdx = i
		}
	}
	if appliedIdx == -1 || terminalIdx == -1 || appliedIdx > terminalIdx {
		t.Errorf("resume ordering applied=%d terminal=%d; kinds=%v", appliedIdx, terminalIdx, kindsOf(events))
	}

	gotCtx, _ := postAppliedThroughRealPoster(t, applied)
	if gotCtx != "appliedReasoningEffort=high" {
		t.Errorf("resume poster context = %q, want appliedReasoningEffort=high", gotCtx)
	}

	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	fh.mu.Unlock()
	for _, m := range methods {
		if m == "session/start" {
			t.Errorf("resume path must not call session/start; methods=%v", methods)
		}
	}
}

// TestAppliedEffort_AbsentAndSentinelReportNotConfigured pins AC-2: an
// absent or sentinel effort sends no request tier and reports
// not-configured. Raw profile input (ultra, unknown) cannot bypass
// translation into a guessed level.
func TestAppliedEffort_AbsentAndSentinelReportNotConfigured(t *testing.T) {
	for _, tc := range []struct {
		name   string
		effort agent.EffortLevel
	}{
		{"absent", ""},
		{"default sentinel", "default"},
		{"none sentinel", "none"},
		{"ultra wire value", "ultra"},
		{"unknown", "turbo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, fh := newTestProvider(t, "single-turn")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: tc.effort})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if _, present := turnStartEffortParam(t, fh); present {
				t.Errorf("effort %q sent a reasoningEffort tier, want omitted", tc.effort)
			}
			events := drainEvents(t, h.Events(), 10*time.Second)
			applied, ok := appliedEventOf(events)
			if !ok {
				t.Fatalf("no applied event for effort %q; kinds=%v", tc.effort, kindsOf(events))
			}
			if applied.Message != "" {
				t.Errorf("applied Message = %q for effort %q, want empty (not-configured)", applied.Message, tc.effort)
			}
			// RED pin: building the report directly from Spec.Effort would
			// carry the raw input ("default"/"ultra"/"turbo") instead of
			// the translated absence. The harness must read the accepted
			// final request, not the raw profile.
			if direct := agent.AppliedReasoningEffortEvent(tc.effort); tc.effort != "" && direct.Message == applied.Message {
				t.Errorf("direct-from-spec report coincides with translated report for %q; translation bypass not pinned", tc.effort)
			}
			gotCtx, gotContent := postAppliedThroughRealPoster(t, applied)
			if gotCtx != "appliedReasoningEffort=not-configured" {
				t.Errorf("poster context = %q, want appliedReasoningEffort=not-configured", gotCtx)
			}
			if !strings.Contains(gotContent, "not configured") {
				t.Errorf("poster content = %q, want the not-configured sentence", gotContent)
			}
		})
	}
}

// TestAppliedEffort_MinimalRidesWireSpelling pins the minimal tier: it has no
// agent.EffortLevel constant but rides the wire by its spelling and reports
// as minimal through the real poster.
func TestAppliedEffort_MinimalRidesWireSpelling(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortLevel("minimal")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got, present := turnStartEffortParam(t, fh); !present || got != "minimal" {
		t.Fatalf("turn/start reasoningEffort = %q present=%v, want minimal", got, present)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	applied, ok := appliedEventOf(events)
	if !ok {
		t.Fatalf("no applied event for minimal; kinds=%v", kindsOf(events))
	}
	if applied.Message != "minimal" {
		t.Errorf("applied Message = %q, want minimal", applied.Message)
	}
	gotCtx, gotContent := postAppliedThroughRealPoster(t, applied)
	if gotCtx != "appliedReasoningEffort=minimal" {
		t.Errorf("poster context = %q, want appliedReasoningEffort=minimal", gotCtx)
	}
	if gotContent != "applied reasoning effort: minimal (as applied)" {
		t.Errorf("poster content = %q, want the minimal sentence", gotContent)
	}
}

// TestAppliedEffortFromTurnParams_Translation pins that the report derives
// from the accepted final request's tier, never the raw spec value.
func TestAppliedEffortFromTurnParams_Translation(t *testing.T) {
	for _, tc := range []struct {
		params map[string]any
		want   agent.EffortLevel
	}{
		{map[string]any{"reasoningEffort": "high"}, agent.EffortHigh},
		{map[string]any{"reasoningEffort": "minimal"}, agent.EffortLevel("minimal")},
		{map[string]any{}, ""},
		{map[string]any{"reasoningEffort": ""}, ""},
		// Rejected/unrecognized wire spellings never become a guessed level.
		{map[string]any{"reasoningEffort": "ultra"}, ""},
		{map[string]any{"reasoningEffort": "none"}, ""},
		{map[string]any{"reasoningEffort": "bogus"}, ""},
	} {
		if got := appliedEffortFromTurnParams(tc.params); got != tc.want {
			t.Errorf("params %v = %q, want %q", tc.params, got, tc.want)
		}
	}
}

// TestAppliedEffort_HostRejectionEmitsNothing pins AC-3's rejection half: a
// host that rejects turn/start returns an error and emits no success report.
func TestAppliedEffort_HostRejectionEmitsNothing(t *testing.T) {
	fh, stdinW, stdoutR := newFakeHost()
	fh.turnStartErr = &rpcError{Code: -32000, Message: "overloaded"}
	fh.script = "single-turn"
	go fh.run(t)
	t.Cleanup(fh.close)

	spec := agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortHigh}
	plan := NewSpawnPlan(spec, t.TempDir())
	h := newHandle(nil, spec, plan, HandleOptions{RPCTimeout: 2 * time.Second})
	h.client = NewClient(stdinW, stdoutR)
	h.setSessionID("11111111-1111-7111-8111-111111111111")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := h.startTurn(ctx, plan.PromptInput)
	if err == nil {
		t.Fatal("startTurn succeeded against a rejecting host, want error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("rejection returned after %s, want within the bounded request", elapsed)
	}
	select {
	case ev := <-h.Events():
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort {
			t.Errorf("rejection emitted applied event %q, want nothing", sys.Message)
		} else {
			t.Errorf("rejection emitted unexpected event %#v, want nothing", ev)
		}
	default:
	}
	// The Spawn door surfaces the same rejection within its bound.
	p, fh2 := newTestProviderWithRPCTimeout(t, "single-turn", 2*time.Second)
	fh2.mu.Lock()
	fh2.turnStartErr = &rpcError{Code: -32000, Message: "overloaded"}
	fh2.mu.Unlock()
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	sstart := time.Now()
	if _, err := p.Spawn(sctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortHigh}); err == nil {
		t.Error("Spawn succeeded against a rejecting host, want error")
	} else if elapsed := time.Since(sstart); elapsed > 5*time.Second {
		t.Errorf("Spawn rejection returned after %s, want within bound", elapsed)
	}
}

// TestAppliedEffort_MalformedAckEmitsNothing pins AC-3's malformed half: an
// acknowledgement that fails validation emits no success report.
func TestAppliedEffort_MalformedAckEmitsNothing(t *testing.T) {
	fh, stdinW, stdoutR := newFakeHost()
	fh.turnStartMalformed = true
	fh.script = "single-turn"
	go fh.run(t)
	t.Cleanup(fh.close)

	spec := agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortHigh}
	plan := NewSpawnPlan(spec, t.TempDir())
	h := newHandle(nil, spec, plan, HandleOptions{RPCTimeout: 2 * time.Second})
	h.client = NewClient(stdinW, stdoutR)
	h.setSessionID("11111111-1111-7111-8111-111111111111")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.startTurn(ctx, plan.PromptInput); err == nil {
		t.Fatal("startTurn succeeded on a malformed ack, want error")
	}
	select {
	case ev := <-h.Events():
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort {
			t.Errorf("malformed ack emitted applied event %q, want nothing", sys.Message)
		} else {
			t.Errorf("malformed ack emitted unexpected event %#v, want nothing", ev)
		}
	default:
	}
}

// TestAppliedEffort_NeverSettlingHostTimesOut pins AC-3's hang half: a host
// that never settles the bounded request yields no report and returns
// within the timeout.
func TestAppliedEffort_NeverSettlingHostTimesOut(t *testing.T) {
	fh, stdinW, stdoutR := newFakeHost()
	fh.turnStartHang = true
	fh.script = "single-turn"
	go fh.run(t)
	t.Cleanup(fh.close)

	spec := agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortHigh}
	plan := NewSpawnPlan(spec, t.TempDir())
	h := newHandle(nil, spec, plan, HandleOptions{RPCTimeout: 300 * time.Millisecond})
	h.client = NewClient(stdinW, stdoutR)
	h.setSessionID("11111111-1111-7111-8111-111111111111")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := h.startTurn(ctx, plan.PromptInput)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("startTurn succeeded against a never-settling host, want timeout")
	}
	if elapsed > 4*time.Second {
		t.Errorf("hang returned after %s, want within the bounded request", elapsed)
	}
	select {
	case ev := <-h.Events():
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort {
			t.Errorf("hang emitted applied event %q, want nothing", sys.Message)
		} else {
			t.Errorf("hang emitted unexpected event %#v, want nothing", ev)
		}
	default:
	}
}

// TestAppliedEffort_CancelledContextEmitsNothing pins cancellation: a
// cancelled turn/start emits no success report.
func TestAppliedEffort_CancelledContextEmitsNothing(t *testing.T) {
	fh, stdinW, stdoutR := newFakeHost()
	fh.turnStartHang = true
	fh.script = "single-turn"
	go fh.run(t)
	t.Cleanup(fh.close)

	spec := agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortHigh}
	plan := NewSpawnPlan(spec, t.TempDir())
	h := newHandle(nil, spec, plan, HandleOptions{RPCTimeout: 5 * time.Second})
	h.client = NewClient(stdinW, stdoutR)
	h.setSessionID("11111111-1111-7111-8111-111111111111")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.startTurn(ctx, plan.PromptInput); err == nil {
		t.Fatal("startTurn succeeded on a cancelled context, want error")
	}
	select {
	case ev := <-h.Events():
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort {
			t.Errorf("cancelled start emitted applied event %q, want nothing", sys.Message)
		}
	default:
	}
}
