package runner

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func generationEvent(id string) agent.Event {
	return agent.SystemEvent{Subtype: agent.SystemSubtypeGenerationID, GenerationID: id}
}

func TestRun_GenerationIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bound  bool
		events []agent.Event
		want   []string
	}{
		{name: "bound with ids across turns", bound: true, events: []agent.Event{generationEvent("response-one"), generationEvent("response-one"), generationEvent(""), generationEvent("  "), generationEvent(strings.Repeat("x", 257)), agent.LlmCallEvent{GenerationID: "synthetic", Synthetic: true}, agent.LlmCallEvent{GenerationID: "aggregate", UsageSource: agent.LlmUsageAggregate}}, want: []string{"response-one", "response-two"}},
		{name: "metadata and native call ids", bound: true, events: []agent.Event{agent.SystemEvent{Subtype: agent.SystemSubtypeModelIdentity, GenerationID: "response-model"}, agent.LlmCallEvent{GenerationID: "response-call", UsageSource: agent.LlmUsageProvider}}, want: []string{"response-model", "response-call", "response-one", "response-two"}},
		{name: "bound without ids", bound: true},
		{name: "unbound ignores ids", events: []agent.Event{generationEvent("response-one")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform := newRecordingPlatformServer(t)
			var endpoint *agent.EndpointBinding
			var next []agent.Event
			if tc.bound {
				endpoint = &agent.EndpointBinding{}
			}
			if len(tc.want) > 0 {
				next = []agent.Event{generationEvent("response-one"), generationEvent("response-two")}
			}
			res, _ := runScriptedSession(t, scriptedSession{
				workType: WorkTypeQAStr, platform: platform, endpoint: endpoint,
				turns: []verdictScriptTurn{{text: "Working.", events: tc.events}, {text: "WORK_RESULT: passed", events: next}},
			})
			if !slices.Equal(res.GenerationIDs, tc.want) {
				t.Fatalf("GenerationIDs = %v; want %v", res.GenerationIDs, tc.want)
			}
			wire := platform.terminalStatus(t)
			raw, present := wire["generationIds"]
			if len(tc.want) == 0 {
				if present {
					t.Fatalf("unexpected generationIds = %s", raw)
				}
			} else {
				var got []string
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("terminal generationIds = %v; want %v", got, tc.want)
				}
			}
		})
	}
}

func TestRun_GenerationIDsBound(t *testing.T) {
	var events []agent.Event
	for i := 0; i < maxGenerationIDs+2; i++ {
		events = append(events, generationEvent(fmt.Sprintf("response-%d", i)))
	}
	platform := newRecordingPlatformServer(t)
	res, _ := runScriptedSession(t, scriptedSession{workType: WorkTypeQAStr, skipSteering: true, platform: platform, endpoint: &agent.EndpointBinding{}, turns: []verdictScriptTurn{{text: "WORK_RESULT: passed", events: events}}})
	if len(res.GenerationIDs) != maxGenerationIDs || res.GenerationIDs[0] != "response-0" || res.GenerationIDs[maxGenerationIDs-1] != fmt.Sprintf("response-%d", maxGenerationIDs-1) {
		t.Fatalf("GenerationIDs count = %d; want %d in first-observed order", len(res.GenerationIDs), maxGenerationIDs)
	}
	var got []string
	if err := json.Unmarshal(platform.terminalStatus(t)["generationIds"], &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, res.GenerationIDs) {
		t.Fatal("terminal post differs from bounded result")
	}
}
