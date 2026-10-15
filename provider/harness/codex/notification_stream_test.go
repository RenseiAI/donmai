package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// Drive the production notification handler and consume its public Events
// channel. No mapper replacement: removing the dispatch must fail these tests.
func notificationHandle() *Handle {
	return newHandle(nil, nil, agent.Spec{}, HandleOptions{EventBuffer: 512})
}

func dispatch(t *testing.T, h *Handle, method, params string) []agent.Event {
	t.Helper()
	h.handleNotification(notification{Method: method, Params: json.RawMessage(params)})
	var events []agent.Event
	for {
		select {
		case event := <-h.Events():
			events = append(events, event)
		default:
			return events
		}
	}
}

func usagePayload(totalIn, totalOut, cached int64) string {
	raw, _ := json.Marshal(map[string]any{"threadId": "thread", "turnId": "turn", "tokenUsage": map[string]any{
		"total": map[string]any{"inputTokens": totalIn, "outputTokens": totalOut, "cachedInputTokens": cached, "reasoningOutputTokens": 2, "totalTokens": totalIn + totalOut},
		"last":  map[string]any{"inputTokens": 100, "outputTokens": 20, "cachedInputTokens": 30, "totalTokens": 120}, "modelContextWindow": 200,
	}})
	return string(raw)
}

func TestNotificationStreamUsage(t *testing.T) {
	t.Parallel()
	h := notificationHandle()
	dispatch(t, h, "turn/started", `{"turn":{"id":"turn"}}`)
	got := dispatch(t, h, "thread/tokenUsage/updated", usagePayload(1100, 220, 330))
	if len(got) != 2 {
		t.Fatalf("usage notification events = %d, want spend and context", len(got))
	}
	spend, ok := got[0].(agent.LlmCallEvent)
	if !ok || spend.InputTokens != 70 || spend.OutputTokens != 20 || spend.CachedInputTokens != 30 || spend.TurnCompleted {
		t.Fatalf("resumed-thread spend = %+v", got[0])
	}
	notice, ok := got[1].(agent.ContextUsageEvent)
	if !ok || notice.ContextWindowSize == nil || *notice.ContextWindowSize != 200 || notice.UsedPercent == nil || *notice.UsedPercent != 60 || *notice.InputTokens != 1100 || *notice.OutputTokens != 220 {
		t.Fatalf("context = %+v", got[1])
	}
	got = dispatch(t, h, "thread/tokenUsage/updated", usagePayload(1100, 220, 330))
	if len(got) != 1 {
		t.Fatalf("duplicate billed again: %+v", got)
	}
	got = dispatch(t, h, "thread/tokenUsage/updated", usagePayload(1200, 250, 350))
	spend, ok = got[0].(agent.LlmCallEvent)
	if !ok || spend.InputTokens != 80 || spend.OutputTokens != 30 || spend.CachedInputTokens != 20 {
		t.Fatalf("incremental spend = %+v", got)
	}
	got = dispatch(t, h, "turn/completed", `{"turn":{"id":"turn","status":"completed","usage":{"inputTokens":200,"outputTokens":50,"cachedInputTokens":50}}}`)
	if len(got) != 2 {
		t.Fatalf("completion = %+v", got)
	}
	final := got[0].(agent.LlmCallEvent)
	result := got[1].(agent.ResultEvent)
	if final.InputTokens != 0 || final.OutputTokens != 0 || !final.TurnCompleted || result.Cost.InputTokens != 150 || result.Cost.OutputTokens != 50 || result.Cost.CachedInputTokens != 50 || result.Cost.TotalCostUsd <= 0 {
		t.Fatalf("usage double-counted or missing: %+v %+v", final, result.Cost)
	}
}

func TestNotificationStreamWarningsAndProgress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ method, params, subtype, content string }{
		{"model/rerouted", `{"fromModel":"first","toModel":"second","reason":"safety"}`, "codex_model_rerouted", "second"},
		{"turn/moderationMetadata", `{"metadata":{"review":true}}`, "codex_moderation", "review"},
		{"model/safetyBuffering/updated", `{"model":"first","reasons":["review"]}`, "codex_safety_buffering", "review"},
		{"item/fileChange/outputDelta", `{"delta":"patch output"}`, "codex_progress", "patch output"},
		{"item/fileChange/patchUpdated", `{"changes":[{"path":"file.go","diff":"+added"}]}`, "codex_progress", "+added"},
		{"item/plan/delta", `{"delta":"next step"}`, "codex_progress", "next step"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()
			h := notificationHandle()
			got := dispatch(t, h, tc.method, tc.params)
			if len(got) != 1 {
				t.Fatalf("events = %+v", got)
			}
			event, ok := got[0].(agent.SystemEvent)
			if !ok || event.Subtype != tc.subtype || !strings.Contains(event.Message, tc.content) || event.Raw == nil {
				t.Fatalf("event = %+v", got[0])
			}
			if tc.method == "model/rerouted" && h.state.model != "second" {
				t.Fatalf("reroute model = %s", h.state.model)
			}
		})
	}
}

func TestNotificationStreamCompaction(t *testing.T) {
	t.Parallel()
	got := dispatch(t, notificationHandle(), "thread/compacted", `{"threadId":"thread"}`)
	if len(got) != 1 {
		t.Fatalf("compaction = %+v", got)
	}
	event, ok := got[0].(agent.ContextUsageEvent)
	if !ok || event.NoticeKind != agent.ContextUsageCompaction || event.HasMeasurements() {
		t.Fatalf("compaction = %+v", got[0])
	}
}

func TestNotificationStreamReasoning(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"item/reasoning/summaryTextDelta", "item/reasoning/textDelta"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			h := notificationHandle()
			dispatch(t, h, method, `{"itemId":"r","delta":"Think "}`)
			dispatch(t, h, "item/reasoning/summaryPartAdded", `{"itemId":"r","summaryIndex":1}`)
			dispatch(t, h, method, `{"itemId":"r","delta":"carefully"}`)
			got := dispatch(t, h, "item/completed", `{"item":{"id":"r","type":"reasoning","summary":[],"content":[]}}`)
			if len(got) != 1 {
				t.Fatalf("thought = %+v", got)
			}
			thought, ok := got[0].(agent.SystemEvent)
			if !ok || thought.Subtype != "reasoning" || !strings.Contains(thought.Message, "Think ") || !strings.Contains(thought.Message, "carefully") {
				t.Fatalf("thought = %+v", got[0])
			}
			if len(h.state.reasoning) != 0 {
				t.Fatal("completed reasoning buffer retained")
			}
		})
	}
	h := notificationHandle()
	got := dispatch(t, h, "item/completed", `{"item":{"id":"r","type":"reasoning","summary":["first","second"],"content":[]}}`)
	if len(got) != 1 || got[0].(agent.SystemEvent).Message != "first\nsecond" {
		t.Fatalf("native summary = %+v", got)
	}
	dispatch(t, h, "item/reasoning/textDelta", `{"itemId":"r","delta":"final thought"}`)
	got = dispatch(t, h, "turn/completed", `{"turn":{"id":"t","status":"completed"}}`)
	if len(got) != 3 || got[0].(agent.SystemEvent).Message != "final thought" {
		t.Fatalf("turn reasoning flush = %+v", got)
	}
}

func TestNotificationStreamExplicitIgnores(t *testing.T) {
	t.Parallel()
	h := notificationHandle()
	for method := range ignoredNotifications {
		for range 2 {
			if got := dispatch(t, h, method, `{}`); len(got) != 0 {
				t.Fatalf("%s emitted %+v", method, got)
			}
		}
		if h.state.ignoredCounts[method] != 2 {
			t.Fatalf("%s debug count = %d", method, h.state.ignoredCounts[method])
		}
	}
	for kind := range ignoredItems {
		got := dispatch(t, h, "item/completed", `{"item":{"type":"`+kind+`"}}`)
		if len(got) != 0 || h.state.ignoredCounts["item/"+kind] != 1 {
			t.Fatalf("%s not silently counted: %+v", kind, got)
		}
	}
	if got := dispatch(t, h, "future/notification", `{}`); len(got) != 0 || h.state.ignoredCounts["future/notification"] != 1 {
		t.Fatalf("unknown notification = %+v", got)
	}
}

func TestNotificationStreamUsageRejectsStaleAndInvalid(t *testing.T) {
	t.Parallel()
	h := notificationHandle()
	dispatch(t, h, "thread/tokenUsage/updated", usagePayload(100, 20, 30))
	for _, params := range []string{
		usagePayload(90, 10, 20),
		`{"tokenUsage":{"total":{},"last":{}}}`,
		`{"tokenUsage":{"total":{"inputTokens":-1},"last":{}}}`,
		`{"tokenUsage":{"total":{"inputTokens":5,"cachedInputTokens":6},"last":{}}}`,
		`{"tokenUsage":null}`,
		`{`,
	} {
		if got := dispatch(t, h, "thread/tokenUsage/updated", params); len(got) != 0 {
			t.Fatalf("invalid/stale usage emitted %+v", got)
		}
	}
	got := dispatch(t, h, "thread/tokenUsage/updated", usagePayload(110, 30, 30))
	if len(got) != 2 || got[0].(agent.LlmCallEvent).InputTokens != 10 {
		t.Fatalf("stale snapshot rewound baseline: %+v", got)
	}
}

func TestNotificationStreamUsageWithoutWindowAndAfterReroute(t *testing.T) {
	t.Parallel()
	h := notificationHandle()
	dispatch(t, h, "model/rerouted", `{"fromModel":"old","toModel":"gpt-5.2","reason":"safety"}`)
	var payload map[string]any
	if err := json.Unmarshal([]byte(usagePayload(100, 20, 30)), &payload); err != nil {
		t.Fatal(err)
	}
	payload["tokenUsage"].(map[string]any)["modelContextWindow"] = nil
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	got := dispatch(t, h, "thread/tokenUsage/updated", string(raw))
	if len(got) != 2 || got[0].(agent.LlmCallEvent).Model != "gpt-5.2" {
		t.Fatalf("rerouted usage = %+v", got)
	}
	notice := got[1].(agent.ContextUsageEvent)
	if notice.ContextWindowSize != nil || notice.UsedPercent != nil {
		t.Fatalf("fabricated window: %+v", notice)
	}
}

func TestNotificationStreamReasoningPrefersNativeSummary(t *testing.T) {
	t.Parallel()
	h := notificationHandle()
	dispatch(t, h, "item/reasoning/textDelta", `{"itemId":"r","delta":"raw reasoning"}`)
	dispatch(t, h, "item/reasoning/summaryTextDelta", `{"itemId":"r","delta":"partial summary"}`)
	got := dispatch(t, h, "item/completed", `{"item":{"id":"r","type":"reasoning","summary":["native summary"],"content":[]}}`)
	if len(got) != 1 || got[0].(agent.SystemEvent).Message != "native summary" {
		t.Fatalf("summary duplicated or discarded: %+v", got)
	}
	if len(h.state.reasoning) != 0 {
		t.Fatal("buffer retained after authoritative summary")
	}
	got = dispatch(t, h, "item/completed", `{"item":{"id":"unknown","type":"futureItem"}}`)
	if len(got) != 0 || h.state.ignoredCounts["item/futureItem"] != 1 {
		t.Fatalf("unknown item not silently counted: %+v", got)
	}
}

func TestNotificationStreamGlobalIgnores(t *testing.T) {
	// Nonparallel: the default debug logger is replaced only for this test.
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	client := NewClient(io.Discard, strings.NewReader(""))
	provider := &Provider{client: client}
	provider.forwardRateLimitUpdates()
	for range 2 {
		client.dispatchNotification(rpcInbound{Method: "account/updated", Params: json.RawMessage(`{}`)}, false)
	}
	if !strings.Contains(logs.String(), `"method":"account/updated","count":2`) {
		t.Fatalf("global notification missing debug count: %s", logs.String())
	}
}

func TestNotificationStreamUsageKeepsReroutePrices(t *testing.T) {
	t.Parallel()
	h := notificationHandle()
	h.state.model = "gpt-5-codex"
	dispatch(t, h, "thread/tokenUsage/updated", usagePayload(100, 20, 30))
	dispatch(t, h, "model/rerouted", `{"fromModel":"gpt-5-codex","toModel":"gpt-5.3-codex","reason":"safety"}`)
	dispatch(t, h, "thread/tokenUsage/updated", usagePayload(200, 40, 60))
	got := dispatch(t, h, "turn/completed", `{"turn":{"id":"t","status":"completed"}}`)
	result := got[len(got)-1].(agent.ResultEvent)
	want := calculateCostUSD(70, 30, 20, "gpt-5-codex") + calculateCostUSD(70, 30, 20, "gpt-5.3-codex")
	if result.Cost.TotalCostUsd != want {
		t.Fatalf("reroute repriced earlier usage: cost=%v want=%v", result.Cost.TotalCostUsd, want)
	}
}
