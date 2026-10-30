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
			for _, ev := range events {
				switch e := ev.(type) {
				case agent.AssistantTextEvent:
					text = append(text, e.Text)
				case agent.SystemEvent:
					if e.Subtype == "reasoning" {
						reasoning = append(reasoning, e.Message)
					}
				}
			}
			if want := []string{"first", " second", " third", " last", "final answer"}; !reflect.DeepEqual(text, want) {
				t.Errorf("assistant chunks = %q, want %q", text, want)
			}
			if want := []string{"first", " second", " third", "\nnext", " last"}; !reflect.DeepEqual(reasoning, want) {
				t.Errorf("reasoning chunks = %q, want %q", reasoning, want)
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
	got := mapNotification("item/delta", delta, state, nil)
	if len(got) != 1 || got[0].(agent.AssistantTextEvent).Text != " world" {
		t.Fatalf("default text delta: %v", got)
	}
	snapshot(2, strings.Repeat("x", 1<<20))
	got = mapNotification("item/delta", delta, state, nil)
	if len(got) != 1 || got[0].(agent.SystemEvent).Subtype != agent.SystemSubtypeStreamProgress {
		t.Fatalf("oversized delta: %v", got)
	}
	params, err := json.Marshal(map[string]any{"item": map[string]any{"itemId": "open", "kind": "reasoning", "revision": 3, "summary": []string{strings.Repeat("x", (1<<20)-1)}}})
	if err != nil {
		t.Fatal(err)
	}
	mapNotification("item/updated", params, state, nil)
	got = mapNotification("item/delta", json.RawMessage(`{"itemId":"open","field":"summary.1","delta":"y"}`), state, nil)
	if len(got) != 1 || got[0].(agent.SystemEvent).Subtype != agent.SystemSubtypeStreamProgress {
		t.Fatalf("summary boundary exceeded retained-text bound: %v", got)
	}
}
