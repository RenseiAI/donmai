package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
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
			// two agentMessage items complete as two full messages.
			// Fragments never surface as AssistantTextEvent (they would
			// split line-anchored verdict markers and shard the summary).
			if want := []string{"first second third last", "final answer"}; !reflect.DeepEqual(text, want) {
				t.Errorf("assistant chunks = %q, want %q", text, want)
			}
			if want := []string{"first", " second", " third", "\nnext", " last"}; !reflect.DeepEqual(reasoning, want) {
				t.Errorf("reasoning chunks = %q, want %q", reasoning, want)
			}
			if progress == 0 {
				t.Errorf("no stream-progress liveness for assistant deltas")
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
	// B1: a verdict marker split across streaming fragments must not lose
	// its verdict or shard the summary. Assistant deltas are liveness
	// only; the completion carries the full message in one event so the
	// runner's per-message scan sees the complete marker.
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
	if got := mapNotification("item/delta", delta, state, nil); len(got) != 1 {
		t.Fatalf("delta produced %v, want one liveness signal", kindsOf(got))
	} else if sys, ok := got[0].(agent.SystemEvent); !ok || sys.Subtype != agent.SystemSubtypeStreamProgress {
		t.Fatalf("delta produced %T %+v, want stream_progress, not a text fragment", got[0], got[0])
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

func TestStreaming_GapAndBounds(t *testing.T) {
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
	// Assistant deltas are liveness only, never text fragments: the full
	// message arrives on item/completed so verdict markers never split.
	got := mapNotification("item/delta", delta, state, nil)
	if len(got) != 1 || got[0].(agent.SystemEvent).Subtype != agent.SystemSubtypeStreamProgress {
		t.Fatalf("assistant delta must be liveness, got %v", got)
	}
	// Completion emits the full snapshot, not just the unseen suffix.
	completed, err := json.Marshal(map[string]any{"item": map[string]any{"itemId": "open", "kind": "agentMessage", "revision": 2, "text": "hello world", "status": "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	got = mapNotification("item/completed", completed, state, nil)
	if len(got) != 1 {
		t.Fatalf("completion produced %v", kindsOf(got))
	}
	if text, ok := got[0].(agent.AssistantTextEvent); !ok || text.Text != "hello world" {
		t.Fatalf("completion event=%+v, want full snapshot", got[0])
	}
	snapshot(3, strings.Repeat("x", 1<<20))
	got = mapNotification("item/delta", delta, state, nil)
	if len(got) != 1 || got[0].(agent.SystemEvent).Subtype != agent.SystemSubtypeStreamProgress {
		t.Fatalf("oversized delta: %v", got)
	}
	params, err := json.Marshal(map[string]any{"item": map[string]any{"itemId": "open", "kind": "reasoning", "revision": 4, "summary": []string{strings.Repeat("x", (1<<20)-1)}}})
	if err != nil {
		t.Fatal(err)
	}
	mapNotification("item/updated", params, state, nil)
	got = mapNotification("item/delta", json.RawMessage(`{"itemId":"open","field":"summary.1","delta":"y"}`), state, nil)
	if len(got) != 1 || got[0].(agent.SystemEvent).Subtype != agent.SystemSubtypeStreamProgress {
		t.Fatalf("summary boundary exceeded retained-text bound: %v", got)
	}
}
