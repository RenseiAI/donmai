package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestSpawn_StreamingText(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "spawn", true: "resume"}[resume], func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(logger)
			p, fh := newTestProvider(t, "streaming-text")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			spec := agent.Spec{Prompt: "work", BaseInstructions: "base", SystemPromptAppend: "append", InitialContext: "memory", Cwd: t.TempDir(), Autonomous: true}
			var h agent.Handle
			var err error
			if resume {
				h, err = p.Resume(ctx, "22222222-2222-7222-8222-222222222222", spec)
			} else {
				h, err = p.Spawn(ctx, spec)
			}
			if err != nil {
				t.Fatal(err)
			}
			events := drainEvents(t, h.Events(), 10*time.Second)
			var text, reasoning []string
			var progress int
			for _, ev := range events {
				switch e := ev.(type) {
				case agent.AssistantTextEvent:
					text = append(text, e.Text)
				case agent.SystemEvent:
					if e.Subtype == "reasoning" {
						reasoning = append(reasoning, e.Message)
					}
					if e.Subtype == agent.SystemSubtypeStreamProgress {
						progress++
					}
				}
			}
			// Assistant text rides one complete message per item: the
			// agentMessage item completes with its full snapshot and
			// the final agentMessage completes with the answer.
			// Fragments never surface as AssistantTextEvent.
			if want := []string{"first second third last", "final answer"}; !reflect.DeepEqual(text, want) {
				t.Errorf("assistant chunks = %q, want %q", text, want)
			}
			// Reasoning completes once with its full snapshot, not one
			// event per fragment.
			if want := []string{"first second third\nnext last"}; !reflect.DeepEqual(reasoning, want) {
				t.Errorf("reasoning chunks = %q, want %q", reasoning, want)
			}
			// One liveness event per streaming item (agentMessage and
			// reasoning started), never one per fragment.
			if progress != 2 {
				t.Errorf("stream progress events = %d, want exactly 2 (one per item)", progress)
			}
			if r, ok := terminalResult(events); !ok || !r.Success {
				t.Errorf("terminal result: %+v", r)
			}
			if strings.Contains(logs.String(), "schema fingerprint differs") {
				t.Errorf("supported host warned: %s", logs.String())
			}
			fh.mu.Lock()
			defer fh.mu.Unlock()
			for i, method := range fh.methods {
				if method != "turn/start" {
					continue
				}
				input := fh.params[i]["input"].([]any)
				var parts []string
				for _, part := range input {
					parts = append(parts, part.(map[string]any)["text"].(string))
				}
				if want := []string{"append\n\nbase\n\nmemory", "work"}; !reflect.DeepEqual(parts, want) {
					t.Errorf("input = %q, want %q", parts, want)
				}
			}
		})
	}
}

func TestStreaming_VerdictSplitSurvivesFragmentation(t *testing.T) {
	// A verdict marker split across streaming fragments must not lose
	// its verdict: deltas emit nothing and the completion carries the
	// full message in one event so the per-message scan sees it whole.
	state := newMapperState("model")
	started, err := json.Marshal(map[string]any{"item": map[string]any{"itemId": "m1", "kind": "agentMessage", "revision": 1, "text": "Done.\nWORK_", "status": "inProgress"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := mapNotification("item/started", started, state, nil); len(got) != 1 {
		t.Fatalf("started produced %v, want one liveness signal", kindsOf(got))
	} else if sys, ok := got[0].(agent.SystemEvent); !ok || sys.Subtype != agent.SystemSubtypeStreamProgress {
		t.Fatalf("started produced %T %+v, want stream_progress", got[0], got[0])
	}
	delta := json.RawMessage(`{"itemId":"m1","field":"text","delta":"RESULT:passed\n"}`)
	if got := mapNotification("item/delta", delta, state, nil); len(got) != 0 {
		t.Fatalf("delta produced %v, want nothing (completion carries the full text)", kindsOf(got))
	}
	completed, err := json.Marshal(map[string]any{"item": map[string]any{"itemId": "m1", "kind": "agentMessage", "revision": 2, "text": "Done.\nWORK_RESULT:passed\n", "status": "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	got := mapNotification("item/completed", completed, state, nil)
	if len(got) != 1 {
		t.Fatalf("completion produced %v, want one complete message", kindsOf(got))
	}
	text, ok := got[0].(agent.AssistantTextEvent)
	if !ok || text.Text != "Done.\nWORK_RESULT:passed\n" {
		t.Fatalf("completion event=%+v, want the full snapshot in one message", got[0])
	}
}

func TestWire_CurrentExportRoundTrip(t *testing.T) {
	// New members from the stable 1.4.14 export must survive wire decoding.
	for _, tc := range []struct {
		value any
		wire  string
	}{
		{&InitializeResult{}, `{"sandboxAvailability":"unavailable","sandboxUnavailableReason":"bubblewrapUnavailable"}`},
		{&SessionStartParams{}, `{"previousSessionId":"previous","recoveryMode":"on","spawnedBy":{"parentSessionId":"parent"}}`},
		{&Item{}, `{"summary":["thinking"],"messagePhase":"commentary","declaredPhase":"working"}`},
	} {
		if err := json.Unmarshal([]byte(tc.wire), tc.value); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		var want, got map[string]any
		if err := json.Unmarshal([]byte(tc.wire), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		for key, value := range want {
			if !reflect.DeepEqual(got[key], value) {
				t.Errorf("%T lost %s: got %v, want %v", tc.value, key, got[key], value)
			}
		}
	}
}

func TestStreaming_GapDropsDeltaThenRecovers(t *testing.T) {
	state := newMapperState("model")
	snapshot := func(rev int, text string) {
		params, err := json.Marshal(map[string]any{"item": map[string]any{"itemId": "open", "kind": "agentMessage", "revision": rev, "text": text}})
		if err != nil {
			t.Fatal(err)
		}
		mapNotification("item/updated", params, state, nil)
	}
	snapshot(1, "hello")
	delta := json.RawMessage(`{"itemId":"open","delta":" world"}`)
	state.gap = &ViewGapParams{}
	if got := mapNotification("item/delta", delta, state, nil); len(got) != 0 {
		t.Fatalf("gap emitted %v", got)
	}
	state.gap = nil
	// Deltas emit nothing; the completion carries the full snapshot.
	if got := mapNotification("item/delta", delta, state, nil); len(got) != 0 {
		t.Fatalf("delta emitted %v, want nothing", kindsOf(got))
	}
	completed, err := json.Marshal(map[string]any{"item": map[string]any{"itemId": "open", "kind": "agentMessage", "revision": 2, "text": "hello world", "status": "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	got := mapNotification("item/completed", completed, state, nil)
	if len(got) != 1 {
		t.Fatalf("completion produced %v", kindsOf(got))
	}
	if text, ok := got[0].(agent.AssistantTextEvent); !ok || text.Text != "hello world" {
		t.Fatalf("completion event=%+v, want full snapshot", got[0])
	}
}

// TestStreaming_PerItemProgressEmitsOnce drives the production
// Handle.handleNotification path: one progress event per item, deltas
// silent, completion carrying the final text. A second started/updated
// for the same item emits nothing.
func TestStreaming_PerItemProgressEmitsOnce(t *testing.T) {
	h := newHandle(nil, agent.Spec{Prompt: "x"}, NewSpawnPlan(agent.Spec{Prompt: "x"}, t.TempDir()), HandleOptions{})
	sid := "33333333-3333-7333-8333-333333333333"
	turnID := "44444444-4444-7444-8444-444444444444"
	h.setSessionID(sid)
	h.setCurrentTurnID(turnID)
	ctx := context.Background()

	notify := func(method, itemID, kind string, rev int, text string) []agent.Event {
		t.Helper()
		item := map[string]any{"itemId": itemID, "kind": kind, "revision": rev, "status": "inProgress", "turnId": turnID}
		if text != "" {
			if kind == "reasoning" {
				item["summary"] = []string{text}
			} else {
				item["text"] = text
			}
		}
		params, err := json.Marshal(map[string]any{"sessionId": sid, "viewCursor": "c", "item": item})
		if err != nil {
			t.Fatal(err)
		}
		h.handleNotification(ctx, notification{Method: method, Params: params})
		return drainAvailable(h.Events())
	}

	// First started emits the single progress for the item.
	got := notify("item/started", "m1", "agentMessage", 1, "hello")
	if len(got) != 1 {
		t.Fatalf("started produced %v, want one progress event", kindsOf(got))
	}
	if sys, ok := got[0].(agent.SystemEvent); !ok || sys.Subtype != agent.SystemSubtypeStreamProgress {
		t.Fatalf("started produced %T %+v, want stream_progress", got[0], got[0])
	}
	// Updated and deltas for the same item stay silent.
	got = notify("item/updated", "m1", "agentMessage", 2, "hello world")
	if len(got) != 0 {
		t.Fatalf("second update produced %v, want nothing (at most one progress per item)", kindsOf(got))
	}
	deltaParams, err := json.Marshal(map[string]any{"sessionId": sid, "viewCursor": "c", "itemId": "m1", "field": "text", "delta": " more"})
	if err != nil {
		t.Fatal(err)
	}
	h.handleNotification(ctx, notification{Method: "item/delta", Params: deltaParams})
	if got := drainAvailable(h.Events()); len(got) != 0 {
		t.Fatalf("delta produced %v, want nothing", kindsOf(got))
	}
	// Completion emits the final text exactly once.
	completed, err := json.Marshal(map[string]any{
		"sessionId": sid, "viewCursor": "c",
		"item": map[string]any{"itemId": "m1", "kind": "agentMessage", "revision": 3, "status": "completed", "text": "hello world", "turnId": turnID},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.handleNotification(ctx, notification{Method: "item/completed", Params: completed})
	got = drainAvailable(h.Events())
	if len(got) != 1 {
		t.Fatalf("completion produced %v, want one final message", kindsOf(got))
	}
	if text, ok := got[0].(agent.AssistantTextEvent); !ok || text.Text != "hello world" {
		t.Fatalf("completion event=%+v, want the full snapshot", got[0])
	}
	h.closeEvents()
}

// TestConcurrentDrainPreservesToolOrder drives the production
// Handle.handleNotification + Events path with a concurrent reader:
// every tool result must follow its use in delivery order.
func TestConcurrentDrainPreservesToolOrder(t *testing.T) {
	h := newHandle(nil, agent.Spec{Prompt: "x"}, NewSpawnPlan(agent.Spec{Prompt: "x"}, t.TempDir()), HandleOptions{EventBuffer: 64})
	sid := "b0b0b0b0-b0b0-7b0b-8b0b-b0b0b0b0b0b0"
	turnID := "c1c1c1c1-c1c1-7c1c-8c1c-c1c1c1c1c1c1"
	h.setSessionID(sid)
	h.setCurrentTurnID(turnID)
	ctx := context.Background()

	const tools = 20
	var mu sync.Mutex
	var got []agent.Event
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range h.Events() {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		}
	}()
	for i := 0; i < tools; i++ {
		toolID := "concurrent-tool-" + strings.Repeat("x", i+1)
		sParams, err := json.Marshal(map[string]any{
			"sessionId": sid, "viewCursor": "cursor-0",
			"item": map[string]any{"itemId": toolID, "kind": "toolCall", "revision": 1, "status": "inProgress", "tool": "edit", "args": `{"path":"a.txt"}`, "turnId": turnID},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.handleNotification(ctx, notification{Method: "item/started", Params: sParams})
		cParams, err := json.Marshal(map[string]any{
			"sessionId": sid, "viewCursor": "cursor-0",
			"item": map[string]any{"itemId": toolID, "kind": "toolCall", "revision": 2, "status": "completed", "tool": "edit", "visibleOutput": "ok", "turnId": turnID},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.handleNotification(ctx, notification{Method: "item/completed", Params: cParams})
	}
	h.closeEvents()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatalf("concurrent drain did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	useIdx := map[string]int{}
	for idx, ev := range got {
		switch e := ev.(type) {
		case agent.ToolUseEvent:
			if _, dup := useIdx[e.ToolUseID]; !dup {
				useIdx[e.ToolUseID] = idx
			}
		case agent.ToolResultEvent:
			u, ok := useIdx[e.ToolUseID]
			if !ok {
				t.Errorf("tool result without preceding use: %q at index %d", e.ToolUseID, idx)
				continue
			}
			if u > idx {
				t.Errorf("tool %q use at %d delivered after result at %d", e.ToolUseID, u, idx)
			}
		}
	}
	if len(useIdx) != tools {
		t.Fatalf("tool uses=%d, want %d (kinds=%v)", len(useIdx), tools, kindsOf(got))
	}
}

// TestSpawn_FailedSpawnLeavesNoGoroutine fails the handshake and
// asserts no per-handle goroutine survives the failed spawn.
func TestSpawn_FailedSpawnLeavesNoGoroutine(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")
	fh.mu.Lock()
	fh.schemaVersion = 2
	fh.mu.Unlock()

	before := runtime.NumGoroutine()
	_, err := p.Spawn(context.Background(), agent.Spec{Prompt: "x", Cwd: t.TempDir(), Autonomous: true})
	if err == nil {
		t.Fatal("expected handshake failure, got nil")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if runtime.NumGoroutine() <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines before=%d after=%d: failed spawn leaked a goroutine", before, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func drainAvailable(ch <-chan agent.Event) []agent.Event {
	var out []agent.Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}
