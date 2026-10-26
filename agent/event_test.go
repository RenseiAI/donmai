package agent

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestEvent_AllVariantsRoundTrip exercises every Event variant through
// MarshalEvent → UnmarshalEvent and asserts equality.
func TestEvent_AllVariantsRoundTrip(t *testing.T) {
	t.Parallel()
	cost := &CostData{InputTokens: 10, OutputTokens: 5, TotalCostUsd: 0.001, NumTurns: 1}
	cases := []struct {
		name string
		in   Event
		kind EventKind
	}{
		{"init", InitEvent{SessionID: "sess-1", Raw: map[string]any{"foo": "bar"}}, EventInit},
		{"system", SystemEvent{Subtype: "compaction", Message: "compacted", Raw: nil}, EventSystem},
		{"generation_id", SystemEvent{Subtype: SystemSubtypeGenerationID, GenerationID: "response-one"}, EventSystem},
		{"assistant_text", AssistantTextEvent{Text: "hello world"}, EventAssistantText},
		{
			"llm_call",
			LlmCallEvent{
				GenerationID:    "response-two",
				TraceID:         "00112233445566778899aabbccddeeff",
				SpanID:          "0011223344556677",
				ParentSpanID:    "8899aabbccddeeff",
				System:          "anthropic",
				Model:           "claude-opus-4",
				InputTokens:     10,
				OutputTokens:    5,
				FinishReason:    "end_turn",
				UsageSource:     LlmUsageProvider,
				ModelSnapshotID: "claude-opus-4-20260701",
			},
			EventLlmCall,
		},
		{
			"tool_use",
			ToolUseEvent{
				TraceID:      "00112233445566778899aabbccddeeff",
				SpanID:       "0011223344556677",
				ParentSpanID: "8899aabbccddeeff",
				ToolName:     "Bash",
				ToolUseID:    "tu-1",
				Input:        map[string]any{"cmd": "git status"},
				ToolCategory: "shell",
			},
			EventToolUse,
		},
		{
			"tool_result",
			ToolResultEvent{
				TraceID:      "00112233445566778899aabbccddeeff",
				SpanID:       "0011223344556677",
				ParentSpanID: "8899aabbccddeeff",
				ToolName:     "Bash",
				ToolUseID:    "tu-1",
				Content:      "clean",
				IsError:      false,
			},
			EventToolResult,
		},
		{
			"tool_progress",
			ToolProgressEvent{ToolName: "Bash", ElapsedSeconds: 12.5},
			EventToolProgress,
		},
		{
			"subagent",
			SubagentEvent{ToolName: "Task", ToolUseID: "toolu_1", Phase: SubagentStarted, ChildSessionID: "child-1"},
			EventSubagent,
		},
		{
			"result",
			ResultEvent{
				Success: true,
				Message: "done",
				Cost:    cost,
			},
			EventResult,
		},
		{
			"error",
			ErrorEvent{Message: "boom", Code: "spawn_failed"},
			EventError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.in.Kind() != tc.kind {
				t.Fatalf("Kind() = %q, want %q", tc.in.Kind(), tc.kind)
			}
			bytes, err := MarshalEvent(tc.in)
			if err != nil {
				t.Fatalf("MarshalEvent: %v", err)
			}
			wire := string(bytes)
			wantKind := `"kind":"` + string(tc.kind) + `"`
			if !strings.Contains(wire, wantKind) {
				t.Fatalf("wire missing kind discriminator %s\nwire=%s", wantKind, wire)
			}

			out, err := UnmarshalEvent(bytes)
			if err != nil {
				t.Fatalf("UnmarshalEvent: %v", err)
			}
			if out.Kind() != tc.kind {
				t.Fatalf("decoded Kind() = %q, want %q", out.Kind(), tc.kind)
			}

			// Compare normalized via JSON since Raw is `any` and may
			// be re-decoded as map[string]any.
			gotJSON, _ := json.Marshal(out)
			wantJSON, _ := json.Marshal(tc.in)
			if !reflect.DeepEqual(normalize(t, gotJSON), normalize(t, wantJSON)) {
				t.Fatalf("variant mismatch:\nwant=%s\ngot =%s", wantJSON, gotJSON)
			}
		})
	}
}

// normalize unmarshals into a generic map for type-tolerant comparison.
func normalize(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return m
}

// TestUnmarshalEvent_ErrorPaths verifies error reporting on bad input.
func TestUnmarshalEvent_ErrorPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        string
		wantSubst string
	}{
		{"invalid-json", `not json`, "decode event kind"},
		{"missing-kind", `{"sessionId":"x"}`, "missing kind discriminator"},
		{"unknown-kind", `{"kind":"alien"}`, `unknown event kind "alien"`},
		{"variant-decode-bad-text", `{"kind":"assistant_text","text":12}`, "decode AssistantTextEvent"},
		{"variant-decode-bad-llm-usage", `{"kind":"llm_call","inputTokens":"many"}`, "decode LlmCallEvent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := UnmarshalEvent([]byte(tt.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantSubst)
			}
			if !strings.Contains(err.Error(), tt.wantSubst) {
				t.Fatalf("error %q missing %q", err.Error(), tt.wantSubst)
			}
		})
	}
}

// TestMarshalEvent_NilEvent guards against accidental nil panic.
func TestMarshalEvent_NilEvent(t *testing.T) {
	t.Parallel()
	if _, err := MarshalEvent(nil); err == nil {
		t.Fatal("MarshalEvent(nil) returned nil error")
	}
}

// TestEvent_KindWireValues guards the wire-format kind strings.
func TestEvent_KindWireValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ev   Event
		want string
	}{
		{InitEvent{}, "init"},
		{SystemEvent{}, "system"},
		{AssistantTextEvent{}, "assistant_text"},
		{LlmCallEvent{}, "llm_call"},
		{ToolUseEvent{}, "tool_use"},
		{ToolResultEvent{}, "tool_result"},
		{ToolProgressEvent{}, "tool_progress"},
		{ResultEvent{}, "result"},
		{ErrorEvent{}, "error"},
	}
	for _, tc := range cases {
		if got := string(tc.ev.Kind()); got != tc.want {
			t.Errorf("%T.Kind() = %q, want %q", tc.ev, got, tc.want)
		}
	}
}

// TestEvent_PolymorphicDispatch ensures every kind dispatches to the
// correct concrete type after MarshalEvent → UnmarshalEvent.
func TestEvent_PolymorphicDispatch(t *testing.T) {
	t.Parallel()
	in := []Event{
		InitEvent{SessionID: "s1"},
		SystemEvent{Subtype: "x"},
		AssistantTextEvent{Text: "y"},
		LlmCallEvent{System: "openai", UsageSource: LlmUsageAggregate, Synthetic: true},
		ToolUseEvent{ToolName: "Bash", Input: map[string]any{}},
		ToolResultEvent{Content: "ok"},
		ToolProgressEvent{ToolName: "Bash", ElapsedSeconds: 1.0},
		ResultEvent{Success: true},
		ErrorEvent{Message: "boom"},
	}
	wantTypes := []string{
		"agent.InitEvent",
		"agent.SystemEvent",
		"agent.AssistantTextEvent",
		"agent.LlmCallEvent",
		"agent.ToolUseEvent",
		"agent.ToolResultEvent",
		"agent.ToolProgressEvent",
		"agent.ResultEvent",
		"agent.ErrorEvent",
	}
	for i, ev := range in {
		bytes, err := MarshalEvent(ev)
		if err != nil {
			t.Fatalf("[%d] MarshalEvent: %v", i, err)
		}
		out, err := UnmarshalEvent(bytes)
		if err != nil {
			t.Fatalf("[%d] UnmarshalEvent: %v", i, err)
		}
		gotType := reflect.TypeOf(out).String()
		if gotType != wantTypes[i] {
			t.Errorf("[%d] type = %q, want %q", i, gotType, wantTypes[i])
		}
	}
}

// TestToolCallBoundsEvent_SubtypeAndMessage proves the constructor emits the
// tool_call_bounds subtype with the bound in seconds as its message, and an
// empty message when no bound is enforced — the receipt the session record
// carries for the per-call timeout.
func TestToolCallBoundsEvent_SubtypeAndMessage(t *testing.T) {
	t.Parallel()
	got := ToolCallBoundsEvent(300)
	if got.Subtype != SystemSubtypeToolCallBounds {
		t.Errorf("Subtype = %q; want %q", got.Subtype, SystemSubtypeToolCallBounds)
	}
	if got.Message != "300" {
		t.Errorf("Message = %q; want %q", got.Message, "300")
	}
	empty := ToolCallBoundsEvent(0)
	if empty.Subtype != SystemSubtypeToolCallBounds {
		t.Errorf("Subtype = %q; want %q", empty.Subtype, SystemSubtypeToolCallBounds)
	}
	if empty.Message != "" {
		t.Errorf("Message = %q; want empty (no bound enforced)", empty.Message)
	}
}

func TestObservedUsageFieldsRoundTripWithReportedZero(t *testing.T) {
	zero := 0.0
	turns := 0
	for _, input := range []Event{
		LlmCallEvent{SpanID: "call", UsageSource: LlmUsageProvider, ObservedCostUsd: &zero, TurnCompleted: true},
		ResultEvent{Success: true, ObservedCostUsd: &zero, ObservedTurns: &turns},
	} {
		body, err := MarshalEvent(input)
		if err != nil {
			t.Fatalf("marshal %T: %v", input, err)
		}
		decoded, err := UnmarshalEvent(body)
		if err != nil || !reflect.DeepEqual(input, decoded) {
			t.Fatalf("observed zero lost for %T: decoded=%+v error=%v", input, decoded, err)
		}
	}
}

func TestEvent_GenerationIDWire(t *testing.T) {
	t.Parallel()
	for _, event := range []Event{SystemEvent{Subtype: SystemSubtypeGenerationID, GenerationID: "response-one"}, LlmCallEvent{GenerationID: "response-one", UsageSource: LlmUsageProvider}} {
		data, err := MarshalEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		if body["generationId"] != "response-one" {
			t.Fatalf("wire generationId = %v; want response-one", body["generationId"])
		}
		decoded, err := UnmarshalEvent(data)
		if err != nil {
			t.Fatal(err)
		}
		switch e := decoded.(type) {
		case SystemEvent:
			if e.GenerationID != "response-one" {
				t.Fatalf("decoded generationId = %q", e.GenerationID)
			}
		case LlmCallEvent:
			if e.GenerationID != "response-one" {
				t.Fatalf("decoded generationId = %q", e.GenerationID)
			}
		default:
			t.Fatalf("decoded event = %T", decoded)
		}
	}
	for _, ids := range [][]string{nil, {}, {"response-one"}} {
		data, err := json.Marshal(Result{GenerationIDs: ids})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		got, present := body["generationIds"]
		if len(ids) == 0 {
			if present {
				t.Fatalf("empty ids serialized: %s", got)
			}
		} else if string(got) != `["response-one"]` {
			t.Fatalf("wire generationIds = %s; want response-one", got)
		}
	}
}

func usageIntPtr(n int64) *int64       { return &n }
func usageFloatPtr(f float64) *float64 { return &f }

// canonicalInferenceUsage is the AC-1 golden fixture: one logical inference
// with two ordered attempts, identities, model/route/auth metadata, token
// classes, status, an explicit zero (attempt-2 cached input), and an absent
// class (cache write, absent at every level).
func canonicalInferenceUsage() *LlmInferenceUsage {
	return &LlmInferenceUsage{
		Version: LlmInferenceUsageVersionV2, InferenceID: "inf-9f2",
		Evidence: LlmInferenceEvidenceProvider, Completeness: LlmInferenceCompletenessComplete, Status: LlmInferenceStatusSucceeded,
		Provider: "anthropic", GenerationID: "resp-7",
		RequestedModel: "claude-opus-4", ResolvedModel: "claude-opus-4-20260701",
		Route: "responses", Auth: "api_key",
		LlmInferenceCounts: LlmInferenceCounts{
			InputTokens: usageIntPtr(120), OutputTokens: usageIntPtr(45),
			CachedInputTokens: usageIntPtr(80), ReasoningTokens: usageIntPtr(12),
			NativeCostUsd: usageFloatPtr(0.75),
		},
		Attempts: []LlmInferenceAttempt{
			{
				AttemptID: "att-1", Ordinal: 1, StartTimeUnixNano: "1719600000000000000", EndTimeUnixNano: "1719600001000000000",
				LlmInferenceCounts: LlmInferenceCounts{InputTokens: usageIntPtr(100), OutputTokens: usageIntPtr(30), CachedInputTokens: usageIntPtr(80), ReasoningTokens: usageIntPtr(10), NativeCostUsd: usageFloatPtr(0.5)},
			},
			{
				AttemptID: "att-2", Ordinal: 2, StartTimeUnixNano: "1719600001000000000", EndTimeUnixNano: "1719600002500000000",
				LlmInferenceCounts: LlmInferenceCounts{InputTokens: usageIntPtr(20), OutputTokens: usageIntPtr(15), CachedInputTokens: usageIntPtr(0), ReasoningTokens: usageIntPtr(2), NativeCostUsd: usageFloatPtr(0.25)},
			},
		},
	}
}

// usageV2Golden is the exact wire shape the canonical fixture must produce.
const usageV2Golden = `{"kind":"llm_call","generationId":"resp-7","system":"anthropic","model":"claude-opus-4",` +
	`"finishReason":"end_turn","usageSource":"provider","usageV2":{"version":"v2","inferenceId":"inf-9f2",` +
	`"evidence":"provider","completeness":"complete","status":"succeeded","provider":"anthropic","generationId":"resp-7",` +
	`"requestedModel":"claude-opus-4","resolvedModel":"claude-opus-4-20260701","route":"responses","auth":"api_key",` +
	`"inputTokens":120,"outputTokens":45,"cachedInputTokens":80,"reasoningTokens":12,"nativeCostUsd":0.75,` +
	`"attempts":[{"attemptId":"att-1","ordinal":1,"startTimeUnixNano":"1719600000000000000","endTimeUnixNano":"1719600001000000000",` +
	`"inputTokens":100,"outputTokens":30,"cachedInputTokens":80,"reasoningTokens":10,"nativeCostUsd":0.5},` +
	`{"attemptId":"att-2","ordinal":2,"startTimeUnixNano":"1719600001000000000","endTimeUnixNano":"1719600002500000000",` +
	`"inputTokens":20,"outputTokens":15,"cachedInputTokens":0,"reasoningTokens":2,"nativeCostUsd":0.25}]}}`

// TestLlmCallEvent_UsageV2RoundTrip pins the canonical usage-v2 wire shape
// and proves explicit zero stays present while the absent class stays absent.
func TestLlmCallEvent_UsageV2RoundTrip(t *testing.T) {
	t.Parallel()
	in := LlmCallEvent{
		GenerationID: "resp-7", System: "anthropic", Model: "claude-opus-4",
		FinishReason: "end_turn", UsageSource: LlmUsageProvider, UsageV2: canonicalInferenceUsage(),
	}
	wire, err := MarshalEvent(in)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if err := json.Unmarshal([]byte(usageV2Golden), &want); err != nil {
		t.Fatalf("golden: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire mismatch:\n got=%s\nwant=%s", wire, usageV2Golden)
	}
	usage := got["usageV2"].(map[string]any)
	attempts := usage["attempts"].([]any)
	first := attempts[0].(map[string]any)
	second := attempts[1].(map[string]any)
	if zero, ok := second["cachedInputTokens"]; !ok || zero != float64(0) {
		t.Fatalf("explicit zero lost: %v", second)
	}
	for _, m := range []map[string]any{usage, first, second} {
		if _, ok := m["cacheWriteTokens"]; ok {
			t.Fatalf("absent class leaked onto the wire: %v", m)
		}
	}
	out, err := UnmarshalEvent(wire)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", out, in)
	}
}

// TestLlmCallEvent_UsageV2Invalid pins fail-closed validation on both codec
// directions: every case must be rejected by MarshalEvent and by decode,
// with the message fragment identifying the rule that fired. Per-field
// guard cases (negative counts/charges, reasoning above output, no
// attempts, attempt-level counts) run on a partial fixture so the
// complete-totals check cannot mask the guard: deleting the guard must
// turn the case green-lit, not leave the totals check to reject it for
// the wrong reason.
func TestLlmCallEvent_UsageV2Invalid(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	inf := math.Inf(1)
	cases := []struct {
		name string
		// partial runs the case on a partial fixture so complete
		// totals are skipped and only the targeted guard can fire.
		partial bool
		mutate  func(*LlmInferenceUsage)
		want    string
	}{
		{"unknown version", false, func(u *LlmInferenceUsage) { u.Version = "v9" }, `must be "v2"`},
		{"empty version", false, func(u *LlmInferenceUsage) { u.Version = "" }, `must be "v2"`},
		{"empty inference id", false, func(u *LlmInferenceUsage) { u.InferenceID = "" }, "requires an inference id"},
		{"unknown evidence", false, func(u *LlmInferenceUsage) { u.Evidence = "gossip" }, `evidence "gossip" is unknown`},
		{"unknown completeness", false, func(u *LlmInferenceUsage) { u.Completeness = "mostly" }, `completeness "mostly" is unknown`},
		{"unknown status", false, func(u *LlmInferenceUsage) { u.Status = "pending" }, `status "pending" is unknown`},
		{"negative input", true, func(u *LlmInferenceUsage) { u.InputTokens = usageIntPtr(-1) }, "inputTokens count -1 is negative"},
		{"negative cache write", true, func(u *LlmInferenceUsage) { u.CacheWriteTokens = usageIntPtr(-2) }, "cacheWriteTokens count -2 is negative"},
		{"attempt negative input", true, func(u *LlmInferenceUsage) { u.Attempts[0].InputTokens = usageIntPtr(-5) }, "inputTokens count -5 is negative"},
		{"negative charge", true, func(u *LlmInferenceUsage) { u.NativeCostUsd = usageFloatPtr(-0.5) }, "is not finite nonnegative"},
		{"attempt negative charge", true, func(u *LlmInferenceUsage) { u.Attempts[0].NativeCostUsd = usageFloatPtr(-0.5) }, "is not finite nonnegative"},
		{"nan charge", true, func(u *LlmInferenceUsage) { u.NativeCostUsd = &nan }, "is not finite nonnegative"},
		{"inf charge", true, func(u *LlmInferenceUsage) { u.NativeCostUsd = &inf }, "is not finite nonnegative"},
		{"reasoning above output", true, func(u *LlmInferenceUsage) { u.ReasoningTokens = usageIntPtr(46) }, "reasoning 46 exceeds output 45"},
		{"attempt reasoning above output", true, func(u *LlmInferenceUsage) { u.Attempts[0].ReasoningTokens = usageIntPtr(31) }, "reasoning 31 exceeds output 30"},
		{"no attempts", true, func(u *LlmInferenceUsage) { u.Attempts = nil }, "requires at least one attempt"},
		{"empty attempt id", false, func(u *LlmInferenceUsage) { u.Attempts[1].AttemptID = "" }, "requires an attempt id"},
		{"duplicate attempt id", false, func(u *LlmInferenceUsage) { u.Attempts[1].AttemptID = "att-1" }, "is duplicated"},
		{"zero ordinal", false, func(u *LlmInferenceUsage) { u.Attempts[0].Ordinal = 0 }, "must be 1 in order"},
		{"duplicate ordinal", false, func(u *LlmInferenceUsage) { u.Attempts[1].Ordinal = 1 }, "must be 2 in order"},
		{"ordinal gap", false, func(u *LlmInferenceUsage) { u.Attempts[1].Ordinal = 3 }, "must be 2 in order"},
		{"unordered ordinals", false, func(u *LlmInferenceUsage) { u.Attempts[0].Ordinal, u.Attempts[1].Ordinal = 2, 1 }, "must be 1 in order"},
		{"reversed timestamps", false, func(u *LlmInferenceUsage) { u.Attempts[0].EndTimeUnixNano = "1719599999000000000" }, "end time precedes start time"},
		{"malformed timestamp", false, func(u *LlmInferenceUsage) { u.Attempts[0].StartTimeUnixNano = "soon" }, "is not decimal nanoseconds"},
		{"complete total mismatch", false, func(u *LlmInferenceUsage) { u.InputTokens = usageIntPtr(121) }, "inputTokens total 121 does not equal attempt sum 120"},
		{"complete logical absent", false, func(u *LlmInferenceUsage) { u.OutputTokens = nil }, "outputTokens total is absent but attempts report values"},
		{"complete attempt absent", false, func(u *LlmInferenceUsage) { u.Attempts[1].CachedInputTokens = nil }, "cachedInputTokens total is present but an attempt omits it"},
		{"complete charge mismatch", false, func(u *LlmInferenceUsage) { u.NativeCostUsd = usageFloatPtr(0.76) }, "does not equal attempt sum"},
		{"complete charge absent but attempts report", false, func(u *LlmInferenceUsage) { u.NativeCostUsd = nil }, "native charge total is absent but attempts report values"},
		{"complete charge present but attempt omits", false, func(u *LlmInferenceUsage) { u.Attempts[1].NativeCostUsd = nil }, "native charge total is present but an attempt omits it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := canonicalInferenceUsage()
			if tc.partial {
				u.Completeness = LlmInferenceCompletenessPartial
			}
			tc.mutate(u)
			ev := LlmCallEvent{UsageSource: LlmUsageProvider, UsageV2: u}
			if _, err := MarshalEvent(ev); err == nil {
				t.Fatalf("MarshalEvent accepted %s", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MarshalEvent error for %s = %q, missing rule pin %q", tc.name, err.Error(), tc.want)
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				return // non-finite charges cannot ride JSON; marshal already refused above
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("re-decode: %v", err)
			}
			fields["kind"] = string(EventLlmCall)
			raw, err = json.Marshal(fields)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if _, err := UnmarshalEvent(raw); err == nil {
				t.Fatalf("UnmarshalEvent accepted %s", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("UnmarshalEvent error for %s = %q, missing rule pin %q", tc.name, err.Error(), tc.want)
			}
		})
	}
}

// TestLlmCallEvent_UsageV2ChargeTolerance proves a correct complete usage
// whose attempts cost 0.1 and 0.2 with a provider-reported total of 0.3
// validates on both codec directions despite binary floating-point error.
func TestLlmCallEvent_UsageV2ChargeTolerance(t *testing.T) {
	t.Parallel()
	u := canonicalInferenceUsage()
	u.InputTokens = usageIntPtr(3)
	u.OutputTokens = usageIntPtr(3)
	u.CachedInputTokens = nil
	u.ReasoningTokens = nil
	u.NativeCostUsd = usageFloatPtr(0.3)
	u.Attempts[0].InputTokens = usageIntPtr(1)
	u.Attempts[0].OutputTokens = usageIntPtr(1)
	u.Attempts[0].CachedInputTokens = nil
	u.Attempts[0].ReasoningTokens = nil
	u.Attempts[0].NativeCostUsd = usageFloatPtr(0.1)
	u.Attempts[1].InputTokens = usageIntPtr(2)
	u.Attempts[1].OutputTokens = usageIntPtr(2)
	u.Attempts[1].CachedInputTokens = nil
	u.Attempts[1].ReasoningTokens = nil
	u.Attempts[1].NativeCostUsd = usageFloatPtr(0.2)
	ev := LlmCallEvent{UsageSource: LlmUsageProvider, UsageV2: u}
	wire, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent rejected 0.1+0.2 vs 0.3: %v", err)
	}
	if _, err := UnmarshalEvent(wire); err != nil {
		t.Fatalf("UnmarshalEvent rejected 0.1+0.2 vs 0.3: %v", err)
	}
}

// TestLlmCallEvent_UsageV2ReasoningEqualsOutput pins the reasoning
// boundary: reasoning equal to output is a valid subset, only strictly
// greater is rejected.
func TestLlmCallEvent_UsageV2ReasoningEqualsOutput(t *testing.T) {
	t.Parallel()
	u := canonicalInferenceUsage()
	u.Completeness = LlmInferenceCompletenessPartial
	u.OutputTokens = usageIntPtr(12)
	u.ReasoningTokens = usageIntPtr(12)
	u.Attempts[0].OutputTokens = usageIntPtr(10)
	u.Attempts[0].ReasoningTokens = usageIntPtr(10)
	u.Attempts[1].OutputTokens = usageIntPtr(2)
	u.Attempts[1].ReasoningTokens = usageIntPtr(2)
	ev := LlmCallEvent{UsageSource: LlmUsageProvider, UsageV2: u}
	if _, err := MarshalEvent(ev); err != nil {
		t.Fatalf("MarshalEvent rejected reasoning == output: %v", err)
	}
}

// TestLlmCallEvent_UsageV2PointerMarshal pins the *LlmCallEvent marshal
// branch: a valid pointer round-trips and an invalid pointer is rejected
// with the firing rule.
func TestLlmCallEvent_UsageV2PointerMarshal(t *testing.T) {
	t.Parallel()
	valid := &LlmCallEvent{UsageSource: LlmUsageProvider, UsageV2: canonicalInferenceUsage()}
	wire, err := MarshalEvent(valid)
	if err != nil {
		t.Fatalf("MarshalEvent(&LlmCallEvent) valid: %v", err)
	}
	if _, err := UnmarshalEvent(wire); err != nil {
		t.Fatalf("UnmarshalEvent pointer wire: %v", err)
	}
	bad := &LlmCallEvent{UsageSource: LlmUsageProvider, UsageV2: canonicalInferenceUsage()}
	bad.UsageV2.Version = "v9"
	if _, err := MarshalEvent(bad); err == nil {
		t.Fatal("MarshalEvent(&LlmCallEvent) accepted unknown version")
	} else if !strings.Contains(err.Error(), `must be "v2"`) {
		t.Fatalf("MarshalEvent pointer error = %q, missing rule pin", err.Error())
	}
}

// TestLlmCallEvent_UsageV2LegacyCompatibility proves additive compatibility:
// legacy llm_call JSON decodes without the group, and a test-local old
// projection ignores the group in new JSON.
func TestLlmCallEvent_UsageV2LegacyCompatibility(t *testing.T) {
	t.Parallel()
	legacy := `{"kind":"llm_call","generationId":"resp-legacy","system":"anthropic","model":"claude-opus-4",` +
		`"inputTokens":10,"outputTokens":5,"finishReason":"end_turn","usageSource":"provider"}`
	ev, err := UnmarshalEvent([]byte(legacy))
	if err != nil {
		t.Fatalf("UnmarshalEvent legacy: %v", err)
	}
	call, ok := ev.(LlmCallEvent)
	if !ok {
		t.Fatalf("legacy decoded as %T", ev)
	}
	if call.UsageV2 != nil {
		t.Fatalf("legacy UsageV2 = %+v, want nil", call.UsageV2)
	}
	if call.InputTokens != 10 || call.OutputTokens != 5 || call.UsageSource != LlmUsageProvider {
		t.Fatalf("legacy scalars changed: %+v", call)
	}
	wire, err := MarshalEvent(call)
	if err != nil {
		t.Fatalf("MarshalEvent legacy: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if _, ok := fields["usageV2"]; ok {
		t.Fatalf("legacy marshal emits usageV2: %s", wire)
	}
	type legacyLlmCall struct {
		GenerationID string         `json:"generationId,omitempty"`
		System       string         `json:"system,omitempty"`
		Model        string         `json:"model,omitempty"`
		InputTokens  int64          `json:"inputTokens,omitempty"`
		UsageSource  LlmUsageSource `json:"usageSource"`
	}
	fresh, err := MarshalEvent(LlmCallEvent{GenerationID: "resp-7", System: "anthropic", Model: "claude-opus-4", UsageSource: LlmUsageProvider, UsageV2: canonicalInferenceUsage()})
	if err != nil {
		t.Fatalf("MarshalEvent new: %v", err)
	}
	var old legacyLlmCall
	if err := json.Unmarshal(fresh, &old); err != nil {
		t.Fatalf("old projection: %v", err)
	}
	if old.Model != "claude-opus-4" || old.UsageSource != LlmUsageProvider {
		t.Fatalf("old projection mismatch: %+v", old)
	}
}

// TestLlmCallEvent_CostOnlyCumulativeDeltaContract pins the shared-contract
// exception for interactive cost-only deltas: a non-synthetic provider event
// carrying only a price (no tokens, no turn, empty generation id) round-trips
// and is filterable from per-call usage, so spend meters keep it while
// call counters skip it.
func TestLlmCallEvent_CostOnlyCumulativeDeltaContract(t *testing.T) {
	t.Parallel()
	cost := 0.25
	delta := LlmCallEvent{ObservedCostUsd: &cost, UsageSource: LlmUsageProvider}
	if !delta.IsCostOnlyCumulativeDelta() {
		t.Fatalf("cost-only delta not recognized: %+v", delta)
	}
	wire, err := MarshalEvent(delta)
	if err != nil {
		t.Fatalf("MarshalEvent cost-only delta: %v", err)
	}
	decoded, err := UnmarshalEvent(wire)
	if err != nil {
		t.Fatalf("UnmarshalEvent cost-only delta: %v", err)
	}
	call, ok := decoded.(LlmCallEvent)
	if !ok {
		t.Fatalf("decoded type = %T; want LlmCallEvent", decoded)
	}
	if !call.IsCostOnlyCumulativeDelta() || call.ObservedCostUsd == nil || *call.ObservedCostUsd != 0.25 {
		t.Fatalf("round-trip lost cost-only delta: %+v", call)
	}
	// The review wire probe decodes to the same filterable shape.
	probe, err := UnmarshalEvent([]byte(`{"kind":"llm_call","observedCostUsd":0.25,"usageSource":"provider"}`))
	if err != nil {
		t.Fatalf("UnmarshalEvent wire probe: %v", err)
	}
	if probeCall, ok := probe.(LlmCallEvent); !ok || !probeCall.IsCostOnlyCumulativeDelta() {
		t.Fatalf("wire probe not a cost-only delta: %+v", probe)
	}
	// Per-call, synthetic, aggregate, turned and identified events are calls
	// or fallbacks - never cost-only deltas.
	price := 0.5
	for _, tc := range []struct {
		name string
		ev   LlmCallEvent
	}{
		{"per-call tokens", LlmCallEvent{GenerationID: "resp-1", InputTokens: 10, OutputTokens: 5, UsageSource: LlmUsageProvider, TurnCompleted: true}},
		{"turn only", LlmCallEvent{UsageSource: LlmUsageProvider, TurnCompleted: true}},
		{"identified", LlmCallEvent{GenerationID: "resp-1", ObservedCostUsd: &price, UsageSource: LlmUsageProvider}},
		{"tokened price", LlmCallEvent{InputTokens: 1, ObservedCostUsd: &price, UsageSource: LlmUsageProvider}},
		{"synthetic", LlmCallEvent{ObservedCostUsd: &price, UsageSource: LlmUsageAggregate, Synthetic: true}},
		{"aggregate", LlmCallEvent{ObservedCostUsd: &price, UsageSource: LlmUsageAggregate}},
		{"no price", LlmCallEvent{UsageSource: LlmUsageProvider}},
	} {
		if tc.ev.IsCostOnlyCumulativeDelta() {
			t.Fatalf("%s matches as cost-only delta: %+v", tc.name, tc.ev)
		}
	}
}

// TestLlmCallEvent_CostOnlyDeltaPassesStrictValidator proves the interactive
// cost-only delta passes the versioned usage-atom validator on both codec
// directions: the production shape carries no atom (nil stays valid for
// legacy/partial evidence), and an attached atom describing
// aggregate/partial evidence validates as well.
func TestLlmCallEvent_CostOnlyDeltaPassesStrictValidator(t *testing.T) {
	t.Parallel()
	cost := 0.25
	delta := LlmCallEvent{ObservedCostUsd: &cost, UsageSource: LlmUsageProvider}
	if !delta.IsCostOnlyCumulativeDelta() {
		t.Fatalf("cost-only delta not recognized: %+v", delta)
	}
	wire, err := MarshalEvent(delta)
	if err != nil {
		t.Fatalf("MarshalEvent cost-only delta with nil atom: %v", err)
	}
	decoded, err := UnmarshalEvent(wire)
	if err != nil {
		t.Fatalf("UnmarshalEvent cost-only delta with nil atom: %v", err)
	}
	call, ok := decoded.(LlmCallEvent)
	if !ok || !call.IsCostOnlyCumulativeDelta() {
		t.Fatalf("strict-validator round-trip lost cost-only delta: %+v", decoded)
	}
	withAtom := delta
	withAtom.UsageV2 = &LlmInferenceUsage{
		Version: LlmInferenceUsageVersionV2, InferenceID: "inf-cost-only",
		Evidence: LlmInferenceEvidenceAggregate, Completeness: LlmInferenceCompletenessPartial, Status: LlmInferenceStatusSucceeded,
		Attempts: []LlmInferenceAttempt{{AttemptID: "att-1", Ordinal: 1}},
	}
	atomWire, err := MarshalEvent(withAtom)
	if err != nil {
		t.Fatalf("MarshalEvent cost-only delta with aggregate/partial atom: %v", err)
	}
	atomDecoded, err := UnmarshalEvent(atomWire)
	if err != nil {
		t.Fatalf("UnmarshalEvent cost-only delta with aggregate/partial atom: %v", err)
	}
	if atomCall, ok := atomDecoded.(LlmCallEvent); !ok || !atomCall.IsCostOnlyCumulativeDelta() {
		t.Fatalf("aggregate/partial atom lost cost-only delta: %+v", atomDecoded)
	}
}
