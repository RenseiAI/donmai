package span

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

type spanRecorder struct {
	mu    sync.Mutex
	spans []agent.Span
}

func (r *spanRecorder) Send(s agent.Span) bool {
	r.mu.Lock()
	r.spans = append(r.spans, s)
	r.mu.Unlock()
	return true
}

func (r *spanRecorder) snapshot() []agent.Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agent.Span(nil), r.spans...)
}

type sequentialIDs struct {
	mu   sync.Mutex
	next uint64
}

func (g *sequentialIDs) generate(byteLen int) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return fmt.Sprintf("%0*x", byteLen*2, g.next), nil
}

func newTestProcessor(t *testing.T, recorder *spanRecorder, now func() time.Time) *Processor {
	t.Helper()
	ids := &sequentialIDs{}
	p, err := NewProcessor(ProcessorConfig{
		SessionID:   "session-1",
		OrgID:       "org-1",
		WorkspaceID: "workspace-1",
		WorkType:    "development",
		System:      "anthropic",
		Model:       "claude-opus-4",
		Sender:      recorder,
		Now:         now,
		IDGenerator: ids.generate,
	})
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	return p
}

func TestProcessor_AggregateFallbackIsExplicitAndUnapportioned(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		cost  *agent.CostData
		in    int64
		out   int64
		cache int64
	}{
		{name: "reported aggregate", cost: &agent.CostData{InputTokens: 900, OutputTokens: 120, CachedInputTokens: 400, NumTurns: 7}, in: 900, out: 120, cache: 400},
		{name: "usage unavailable", cost: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder := &spanRecorder{}
			now := time.Unix(1_700_000_000, 0)
			p := newTestProcessor(t, recorder, func() time.Time { return now })
			out := p.Process(agent.ResultEvent{Success: true, Cost: tc.cost})
			if len(out) != 2 {
				t.Fatalf("Process(ResultEvent) returned %d events, want synthetic LLM + result", len(out))
			}
			llm, ok := out[0].(agent.LlmCallEvent)
			if !ok {
				t.Fatalf("event[0] = %T, want LlmCallEvent", out[0])
			}
			if !llm.Synthetic || llm.UsageSource != agent.LlmUsageAggregate {
				t.Fatalf("fallback provenance = synthetic:%v source:%q", llm.Synthetic, llm.UsageSource)
			}
			if llm.InputTokens != tc.in || llm.OutputTokens != tc.out || llm.CachedInputTokens != tc.cache {
				t.Fatalf("fallback changed aggregate counts: got %d/%d/%d want %d/%d/%d", llm.InputTokens, llm.OutputTokens, llm.CachedInputTokens, tc.in, tc.out, tc.cache)
			}
			if llm.PromptHash != "" || llm.ContextHash != "" {
				t.Fatalf("fallback fabricated content correlation: %+v", llm)
			}
			spans := recorder.snapshot()
			if len(spans) != 1 {
				t.Fatalf("emitted %d spans, want one LLM span", len(spans))
			}
			span, ok := spans[0].(agent.LlmCallSpan)
			if !ok {
				t.Fatalf("span = %T, want LlmCallSpan", spans[0])
			}
			if span.GenAI.UsageInputTokens != tc.in || span.GenAI.UsageOutputTokens != tc.out || span.GenAI.UsageCacheReadInputTokens != tc.cache {
				t.Fatalf("span changed aggregate counts: %+v", span.GenAI)
			}
		})
	}
}

func TestProcessor_ProviderUsageSuppressesAggregateFallback(t *testing.T) {
	t.Parallel()
	recorder := &spanRecorder{}
	now := time.Unix(1_700_000_000, 0)
	p := newTestProcessor(t, recorder, func() time.Time { return now })
	provider := p.Process(agent.LlmCallEvent{
		InputTokens:  10,
		OutputTokens: 5,
		UsageSource:  agent.LlmUsageProvider,
	})
	if len(provider) != 1 || provider[0].(agent.LlmCallEvent).Synthetic {
		t.Fatalf("provider event was rewritten as synthetic: %#v", provider)
	}
	terminal := p.Process(agent.ResultEvent{
		Success: true,
		Cost:    &agent.CostData{InputTokens: 999, OutputTokens: 999, NumTurns: 9},
	})
	if len(terminal) != 1 {
		t.Fatalf("provider usage should suppress aggregate fallback; got %#v", terminal)
	}
	spans := recorder.snapshot()
	if len(spans) != 1 {
		t.Fatalf("emitted %d spans, want one measured LLM span", len(spans))
	}
	llm := spans[0].(agent.LlmCallSpan)
	if llm.GenAI.UsageInputTokens != 10 || llm.GenAI.UsageOutputTokens != 5 {
		t.Fatalf("provider span replaced by aggregate totals: %+v", llm.GenAI)
	}
}

func TestProcessor_CorrelatesToolUnderLlmAndSessionRoot(t *testing.T) {
	t.Parallel()
	recorder := &spanRecorder{}
	clock := time.Unix(1_700_000_000, 0)
	now := func() time.Time {
		clock = clock.Add(time.Millisecond)
		return clock
	}
	p := newTestProcessor(t, recorder, now)

	llmOut := p.Process(agent.LlmCallEvent{
		System:       "openai",
		Model:        "gpt-5-codex",
		InputTokens:  100,
		OutputTokens: 20,
		FinishReason: "tool_calls",
		UsageSource:  agent.LlmUsageProvider,
	})[0].(agent.LlmCallEvent)
	useOut := p.Process(agent.ToolUseEvent{ToolName: "Read", ToolUseID: "call-1", Input: map[string]any{"path": "README.md"}})[0].(agent.ToolUseEvent)
	resultOut := p.Process(agent.ToolResultEvent{ToolUseID: "call-1", Content: "contents"})[0].(agent.ToolResultEvent)

	if useOut.TraceID != llmOut.TraceID || useOut.ParentSpanID != llmOut.SpanID {
		t.Fatalf("tool parentage = trace:%q parent:%q, want trace:%q parent:%q", useOut.TraceID, useOut.ParentSpanID, llmOut.TraceID, llmOut.SpanID)
	}
	if resultOut.TraceID != useOut.TraceID || resultOut.SpanID != useOut.SpanID || resultOut.ParentSpanID != useOut.ParentSpanID {
		t.Fatalf("tool result lost correlation: use=%+v result=%+v", useOut, resultOut)
	}

	_ = p.Process(agent.ResultEvent{Success: true})
	p.Finish("completed", "")
	spans := recorder.snapshot()
	if len(spans) != 3 {
		t.Fatalf("emitted %d spans, want LLM + tool + session root", len(spans))
	}
	llmSpan := spans[0].(agent.LlmCallSpan)
	toolSpan := spans[1].(agent.ToolSpan)
	rootSpan := spans[2].(agent.SessionSpan)
	if llmSpan.ParentSpanID != rootSpan.SpanID {
		t.Fatalf("LLM parent = %q, want root %q", llmSpan.ParentSpanID, rootSpan.SpanID)
	}
	if toolSpan.ParentSpanID != llmSpan.SpanID || toolSpan.ToolUseID != "call-1" {
		t.Fatalf("tool span correlation wrong: %+v", toolSpan)
	}
	if toolSpan.TraceID != rootSpan.TraceID || llmSpan.TraceID != rootSpan.TraceID {
		t.Fatalf("trace id spine diverged: root=%s llm=%s tool=%s", rootSpan.TraceID, llmSpan.TraceID, toolSpan.TraceID)
	}
}

func TestProcessor_ConcurrentToolEventsAreRaceSafe(t *testing.T) {
	recorder := &spanRecorder{}
	var clockMu sync.Mutex
	clock := time.Unix(1_700_000_000, 0)
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(time.Microsecond)
		return clock
	}
	p := newTestProcessor(t, recorder, now)
	p.Process(agent.LlmCallEvent{UsageSource: agent.LlmUsageProvider})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("call-%d", i)
			p.Process(agent.ToolUseEvent{ToolName: "Read", ToolUseID: id, Input: map[string]any{}})
			p.Process(agent.ToolResultEvent{ToolUseID: id, Content: "ok"})
		}()
	}
	wg.Wait()
	p.Process(agent.ResultEvent{Success: true})
	p.Finish("completed", "")
	if got := len(recorder.snapshot()); got != 102 {
		t.Fatalf("emitted %d spans, want 1 LLM + 100 tools + 1 root", got)
	}
}

func TestProcessor_ToolCorrelationHandlesMissingAndOutOfOrderIDs(t *testing.T) {
	t.Parallel()
	recorder := &spanRecorder{}
	now := time.Unix(1_700_000_000, 0)
	p := newTestProcessor(t, recorder, func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	})
	p.Process(agent.LlmCallEvent{UsageSource: agent.LlmUsageProvider})

	// Result-before-use is possible on a replayed/out-of-order event bridge.
	lateResult := p.Process(agent.ToolResultEvent{ToolName: "Bash", ToolUseID: "late-1", Content: "ok"})[0].(agent.ToolResultEvent)
	lateUse := p.Process(agent.ToolUseEvent{ToolName: "Bash", ToolUseID: "late-1", Input: map[string]any{}})[0].(agent.ToolUseEvent)
	if lateUse.SpanID != lateResult.SpanID || lateUse.ParentSpanID != lateResult.ParentSpanID {
		t.Fatalf("out-of-order correlation diverged: result=%+v use=%+v", lateResult, lateUse)
	}

	// Providers that omit toolUseId fall back to deterministic FIFO matching
	// within the active LLM call; the result must reuse the use's span id.
	noIDUse := p.Process(agent.ToolUseEvent{ToolName: "Read", Input: map[string]any{}})[0].(agent.ToolUseEvent)
	noIDResult := p.Process(agent.ToolResultEvent{ToolName: "Read", Content: "contents"})[0].(agent.ToolResultEvent)
	if noIDUse.SpanID != noIDResult.SpanID || noIDUse.ParentSpanID != noIDResult.ParentSpanID {
		t.Fatalf("missing-id correlation diverged: use=%+v result=%+v", noIDUse, noIDResult)
	}

	// A terminal event may overtake the final tool result. The processor
	// closes the orphan once, remembers its correlation, and enriches the late
	// result without emitting a duplicate span.
	terminalUse := p.Process(agent.ToolUseEvent{ToolName: "Write", ToolUseID: "terminal-late", Input: map[string]any{}})[0].(agent.ToolUseEvent)
	p.Process(agent.ResultEvent{Success: true})
	toolCountBeforeLate := 0
	for _, s := range recorder.snapshot() {
		if _, ok := s.(agent.ToolSpan); ok {
			toolCountBeforeLate++
		}
	}
	terminalResult := p.Process(agent.ToolResultEvent{ToolUseID: "terminal-late", Content: "ok"})[0].(agent.ToolResultEvent)
	if terminalResult.SpanID != terminalUse.SpanID || terminalResult.ParentSpanID != terminalUse.ParentSpanID {
		t.Fatalf("late terminal result lost correlation: use=%+v result=%+v", terminalUse, terminalResult)
	}

	spans := recorder.snapshot()
	toolCount := 0
	for _, s := range spans {
		if _, ok := s.(agent.ToolSpan); ok {
			toolCount++
		}
	}
	if toolCount != 3 || toolCount != toolCountBeforeLate {
		t.Fatalf("emitted %d tool spans after late result, want unchanged %d", toolCount, toolCountBeforeLate)
	}
}

func TestNewProcessor_RequiredFields(t *testing.T) {
	t.Parallel()
	recorder := &spanRecorder{}
	cases := []struct {
		name string
		cfg  ProcessorConfig
	}{
		{"session", ProcessorConfig{OrgID: "o", WorkspaceID: "w", Sender: recorder}},
		{"org", ProcessorConfig{SessionID: "s", WorkspaceID: "w", Sender: recorder}},
		{"workspace", ProcessorConfig{SessionID: "s", OrgID: "o", Sender: recorder}},
		{"sender", ProcessorConfig{SessionID: "s", OrgID: "o", WorkspaceID: "w"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewProcessor(tc.cfg); err == nil {
				t.Fatal("NewProcessor returned nil error")
			}
		})
	}
}

func TestProviderSystem_AllRuntimeHarnesses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider agent.ProviderName
		want     string
	}{
		{agent.ProviderClaude, "anthropic"},
		{agent.ProviderCodex, "openai"},
		{agent.ProviderGemini, "gcp.gemini"},
		{agent.ProviderAGYCLI, "gcp.gemini"},
		{agent.ProviderOllama, "ollama"},
		{agent.ProviderOpenCode, "opencode"},
		{agent.ProviderAmp, "amp"},
		{agent.ProviderStub, "stub"},
	}
	for _, tt := range tests {
		t.Run(string(tt.provider), func(t *testing.T) {
			t.Parallel()
			if got := ProviderSystem(tt.provider); got != tt.want {
				t.Fatalf("ProviderSystem(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}
