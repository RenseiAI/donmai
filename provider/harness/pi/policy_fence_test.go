package pi

// policy_fence_test.go — the fail-safe contract of the tool-call integrity
// monitor (handle.go dispatch, doc.go "The fail-safe fence").
//
// The monitor's question on a guarded tool_execution_end is NOT "did this call
// complete a round-trip" but "what outcome did the boundary record for it".
// These fixtures pin the three-way split that answer produces: an honoured
// ruling, a recorded refusal, and a call with no record at all — plus the one
// case that still ends the session, a trusted denial that the runtime executed
// successfully anyway.
//
// Every case whose behaviour this change moves was watched RED first: the
// missing-record cases and both call-id cases aborted the session on the prior
// cut, and the demonstrable-bypass case was NOT fatal there. The allow path is
// the unchanged control.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// refusalEvent builds an extension-side refusal round-trip: the request the
// embedded extension raises when it refuses a call on its own, before (or
// instead of) asking for a verdict.
func refusalEvent(reqID, toolName, toolCallID, reason string) string {
	return uiRequest(reqID, map[string]any{
		"donmai":     refusalKind,
		"token":      testHandshakeToken,
		"toolName":   toolName,
		"toolCallId": toolCallID,
		"reason":     reason,
	})
}

// endEvent builds a tool_execution_end carrying the call id under the given
// key, so a fixture can drive any of the spellings the monitor accepts. An
// empty key omits the id entirely.
func endEvent(toolName, idKey, callID string, isError bool) string {
	fields := map[string]any{
		"type":     "tool_execution_end",
		"toolName": toolName,
		"isError":  isError,
	}
	if idKey != "" {
		fields[idKey] = callID
	}
	return event(fields)
}

// Recorded shapes: a live session whose assistant turn hit the registered
// output-token limit. pi emitted the assistant message_end (stopReason
// "length", the edit call's arguments salvaged to {}), then — without running
// the tool_call hook — a tool_execution_start and this exact
// tool_execution_end for the call.
const (
	recordedTruncatedCallID = "call_01a0e414ad3f70c880a43aa0ac2f0de8"
	recordedTruncationText  = "Tool call \"edit\" was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments."
)

// assistantMessageEnd builds an assistant message_end with the given stop
// reason whose content carries one toolCall part per {id, name} pair, in the
// runtime's AssistantMessage shape.
func assistantMessageEnd(stopReason string, calls ...[2]string) string {
	return messageEnd("assistant", stopReason, calls...)
}

// messageEnd is assistantMessageEnd for any message role.
func messageEnd(role, stopReason string, calls ...[2]string) string {
	content := []any{map[string]any{"type": "thinking", "thinking": "planning the edit"}}
	for _, c := range calls {
		content = append(content, map[string]any{"type": "toolCall", "id": c[0], "name": c[1], "arguments": map[string]any{}})
	}
	return event(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       role,
			"content":    content,
			"api":        "openai-completions",
			"provider":   "donmai",
			"model":      "served-model",
			"usage":      map[string]any{"input": 3649, "output": 16613, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 20262},
			"stopReason": stopReason,
			"timestamp":  1790532782311,
		},
	})
}

// truncatedCallPair builds the hook-less start/end pair pi emits for a call it
// refused on an output-limit stop (the recorded end shape, isError:true).
func truncatedCallPair(toolName, callID string) string {
	return event(map[string]any{
		"type": "tool_execution_start", "toolCallId": callID, "toolName": toolName, "args": map[string]any{},
	}) + event(map[string]any{
		"type":       "tool_execution_end",
		"toolCallId": callID,
		"toolName":   toolName,
		"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": recordedTruncationText}},
			"details": map[string]any{},
		},
		"isError": true,
	})
}

// Recorded shape: the runtime's argument validator rejecting an edit call
// whose second replacement has no oldText. recordedRejectionText is the exact
// text the validator produced for recordedMalformedEditArgs; the runtime
// finalizes the call with it as the call's error result, without running the
// tool_call hook.
const recordedRejectionText = "Validation failed for tool \"edit\":\n  - edits.1.oldText: must have required properties oldText\n\nReceived arguments:\n{\n  \"path\": \"README.md\",\n  \"edits\": [\n    {\n      \"oldText\": \"old\",\n      \"newText\": \"new\"\n    },\n    {\n      \"newText\": \"orphan\"\n    }\n  ]\n}"

var recordedMalformedEditArgs = map[string]any{
	"path":  "README.md",
	"edits": []any{map[string]any{"oldText": "old", "newText": "new"}, map[string]any{"newText": "orphan"}},
}

// startEvent builds the tool_execution_start pi emits for a call, before it
// validates the call's arguments or runs the tool_call hook.
func startEvent(toolName, callID string, args any) string {
	return event(map[string]any{"type": "tool_execution_start", "toolCallId": callID, "toolName": toolName, "args": args})
}

// textEnd builds a tool_execution_end whose result is a single text part with
// empty details — the shape the runtime gives every call it finalizes with an
// error message.
func textEnd(toolName, callID, text string, isError bool) string {
	return event(map[string]any{
		"type":       "tool_execution_end",
		"toolCallId": callID,
		"toolName":   toolName,
		"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
			"details": map[string]any{},
		},
		"isError": isError,
	})
}

// rejectedCallPair builds the hook-less start/end pair pi emits for the
// recorded edit call its argument validator rejected.
func rejectedCallPair(callID string) string {
	return startEvent("edit", callID, recordedMalformedEditArgs) + textEnd("edit", callID, recordedRejectionText, true)
}

// drainToResultOrClose collects events until the session produces a
// ResultEvent or closes its channel. Unlike drain it does NOT stop on an
// ErrorEvent: a non-fatal fence event is exactly the case under test, and the
// point is that the session keeps running past it.
func drainToResultOrClose(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var out []agent.Event
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			if _, done := ev.(agent.ResultEvent); done {
				return out
			}
		case <-timeout:
			t.Fatalf("timed out draining events; got %d so far: %+v", len(out), out)
			return out
		}
	}
}

func TestPolicyFence_GuardedToolEndOutcomes(t *testing.T) {
	t.Parallel()

	const denyCommand = "rm -rf /"

	cases := []struct {
		name string
		// body is the scripted event stream delivered after Spawn returns.
		body string
		// wantMissIDs are the call ids the monitor must record as unproven,
		// in order. nil means the monitor must record nothing.
		wantMissIDs []string
		// wantFatal is true when the session must abort on this stream.
		wantFatal bool
		// wantDecisions is the number of permission_decision SystemEvents the
		// boundary must surface.
		wantDecisions int
		// wantRefusedIDs are the call ids the monitor must record — and
		// surface — as refused before execution on an output-limit stop, in
		// order. nil means none.
		wantRefusedIDs []string
		// wantRejectedIDs are the call ids the monitor must record as
		// rejected on their arguments before execution, in order. nil means
		// none.
		wantRejectedIDs []string
	}{
		{
			// A ruling that never reached the registry — the ordering failure
			// that killed real sessions: the end event arrives with no record,
			// which cannot be told apart from a bypass. Recorded, not fatal.
			name:          "ruling never recorded",
			body:          endEvent("write", "toolCallId", "c-lost", false),
			wantMissIDs:   []string{"c-lost"},
			wantDecisions: 0,
		},
		{
			// No correlatable id at all. Uncorrelatable is unproven, never a
			// bypass: the miss is recorded under the empty id.
			name:          "no call id",
			body:          endEvent("write", "", "", false),
			wantMissIDs:   []string{""},
			wantDecisions: 0,
		},
		{
			// An extension-side refusal, registered over the refusal
			// round-trip before the block was returned to pi. pi still emits
			// the end event for the blocked call (isError:true); the recorded
			// refusal is what keeps it from reading as a hole.
			name: "extension refusal recorded",
			body: refusalEvent("r1", "write", "c-refused", "donmai policy boundary not verified") +
				endEvent("write", "toolCallId", "c-refused", true),
			wantMissIDs:   nil,
			wantDecisions: 1,
		},
		{
			// Writer/reader call-id asymmetry: the ruling arrives spelled
			// `callId`, the end event spelled `toolCallId`. One shared rule on
			// both sides means the ruling is still found.
			name: "call id spelled differently on each side",
			body: uiRequest("a1", map[string]any{
				"donmai":   adjudicateKind,
				"token":    testHandshakeToken,
				"toolName": "bash",
				"callId":   "c-spelling",
				"input":    map[string]any{"command": "echo hi"},
			}) + endEvent("bash", "toolCallId", "c-spelling", false),
			wantMissIDs:   nil,
			wantDecisions: 1,
		},
		{
			// The one demonstrable bypass this event stream can show: the
			// boundary denied the call, and the runtime reports it ran and
			// SUCCEEDED anyway. Still fatal.
			name: "denied call executed successfully anyway",
			body: adjudicateEvent("a1", "bash", "c-denied", map[string]any{"command": denyCommand}, "") +
				endEvent("bash", "toolCallId", "c-denied", false),
			wantFatal:     true,
			wantDecisions: 1,
		},
		{
			// The same denial, honoured: pi finalizes a blocked call with
			// isError:true and the reason as its result. Ordinary, non-fatal.
			name: "denied call blocked as an error result",
			body: adjudicateEvent("a1", "bash", "c-denied", map[string]any{"command": denyCommand}, "") +
				endEvent("bash", "toolCallId", "c-denied", true),
			wantMissIDs:   nil,
			wantDecisions: 1,
		},
		{
			// Unchanged control: an allowed call that executed.
			name: "allowed call executed",
			body: adjudicateEvent("a1", "bash", "c-allowed", map[string]any{"command": "echo hi"}, "") +
				endEvent("bash", "toolCallId", "c-allowed", false),
			wantMissIDs:   nil,
			wantDecisions: 1,
		},
		{
			// The recorded failure: the turn hit the output-token limit, pi
			// failed the edit call without running the hook, and the end
			// arrived with no ruling. It never executed, so it is a refusal
			// before execution — not an unproven call, and not a run failure.
			name: "length-stopped call is refused before execution (recorded shape)",
			body: assistantMessageEnd("length", [2]string{recordedTruncatedCallID, "edit"}) +
				truncatedCallPair("edit", recordedTruncatedCallID),
			wantRefusedIDs: []string{recordedTruncatedCallID},
		},
		{
			// Every call in a length-stopped message is failed the same way.
			name: "every call of a length-stopped message is refused",
			body: assistantMessageEnd("length", [2]string{"c-t1", "edit"}, [2]string{"c-t2", "bash"}) +
				truncatedCallPair("edit", "c-t1") + truncatedCallPair("bash", "c-t2"),
			wantRefusedIDs: []string{"c-t1", "c-t2"},
		},
		{
			// A length-stopped id whose end claims SUCCESS executed after all:
			// that is an unruled execution, so it stays the miss.
			name: "length-stopped call that reports success stays unproven",
			body: assistantMessageEnd("length", [2]string{"c-trunc", "edit"}) +
				endEvent("edit", "toolCallId", "c-trunc", false),
			wantMissIDs: []string{"c-trunc"},
		},
		{
			// The control: a normally stopped message excuses nothing. An
			// unruled error end is still unproven.
			name: "unruled call from a normally stopped message stays unproven",
			body: assistantMessageEnd("toolUse", [2]string{"c-normal", "edit"}) +
				truncatedCallPair("edit", "c-normal"),
			wantMissIDs: []string{"c-normal"},
		},
		{
			// The stop excuses only the ids it named.
			name: "length stop does not vouch for another call id",
			body: assistantMessageEnd("length", [2]string{"c-trunc", "edit"}) +
				truncatedCallPair("edit", "c-other"),
			wantMissIDs: []string{"c-other"},
		},
		{
			// Same id, different tool: not the call the message named.
			name: "length-stopped id ending under another tool stays unproven",
			body: assistantMessageEnd("length", [2]string{"c-trunc", "edit"}) +
				truncatedCallPair("bash", "c-trunc"),
			wantMissIDs: []string{"c-trunc"},
		},
		{
			// Only an ASSISTANT message's stop reason says the runtime cut
			// the response off; the same fields on any other role excuse
			// nothing.
			name: "a non-assistant message with a length stop excuses nothing",
			body: messageEnd("toolResult", "length", [2]string{"c-trunc", "edit"}) +
				truncatedCallPair("edit", "c-trunc"),
			wantMissIDs: []string{"c-trunc"},
		},
		{
			// A note does not outlive its turn: the refused pair always
			// precedes turn_end, so a later end reusing the id is unexplained.
			name: "a length-stop note does not survive turn_end",
			body: assistantMessageEnd("length", [2]string{"c-trunc", "edit"}) +
				event(map[string]any{"type": "turn_end"}) +
				truncatedCallPair("edit", "c-trunc"),
			wantMissIDs: []string{"c-trunc"},
		},
		{
			// A success-claiming end leaves its note unconsumed; turn_end
			// still drops it, so it cannot excuse a later errored end.
			name: "an unconsumed note is dropped at turn_end",
			body: assistantMessageEnd("length", [2]string{"c-trunc", "edit"}) +
				endEvent("edit", "toolCallId", "c-trunc", false) +
				event(map[string]any{"type": "turn_end"}) +
				endEvent("edit", "toolCallId", "c-trunc", true),
			wantMissIDs: []string{"c-trunc", "c-trunc"},
		},
		{
			// One named call explains one end event, not a second one.
			name: "a length-stopped id explains a single end",
			body: assistantMessageEnd("length", [2]string{"c-trunc", "edit"}) +
				truncatedCallPair("edit", "c-trunc") +
				endEvent("edit", "toolCallId", "c-trunc", true),
			wantMissIDs:    []string{"c-trunc"},
			wantRefusedIDs: []string{"c-trunc"},
		},
		{
			// The recorded failure: the runtime's argument validator rejected
			// an edit call before the hook, so no ruling exists. It never ran,
			// so it is not an unproven call.
			name:            "call rejected on its arguments is not executed (recorded shape)",
			body:            rejectedCallPair("c-invalid"),
			wantRejectedIDs: []string{"c-invalid"},
		},
		{
			// The agent reads the rejection, sends a valid call, and the
			// session runs on to its own terminal with nothing to report.
			name: "a rejected call followed by a valid one ends normally",
			body: rejectedCallPair("c-invalid") +
				startEvent("edit", "c-valid", map[string]any{"path": "README.md", "edits": []any{map[string]any{"oldText": "old", "newText": "new"}}}) +
				adjudicateEvent("a1", "edit", "c-valid", map[string]any{"path": "README.md", "edits": []any{map[string]any{"oldText": "old", "newText": "new"}}}, "") +
				endEvent("edit", "toolCallId", "c-valid", false),
			wantDecisions:   1,
			wantRejectedIDs: []string{"c-invalid"},
		},
		{
			// A rejection excuses nothing after it: a later denied call that
			// the runtime executed anyway still ends the session.
			name: "a rejected call does not excuse a later denied call that ran",
			body: rejectedCallPair("c-invalid") +
				adjudicateEvent("a1", "bash", "c-denied", map[string]any{"command": denyCommand}, "") +
				endEvent("bash", "toolCallId", "c-denied", false),
			wantFatal:       true,
			wantDecisions:   1,
			wantRejectedIDs: []string{"c-invalid"},
		},
		{
			// The text must close with the arguments the call's start
			// carried; a rejection of other arguments is not this call's.
			name: "a rejection of other arguments stays unproven",
			body: startEvent("edit", "c-inv", map[string]any{"path": "OTHER.md", "edits": []any{}}) +
				textEnd("edit", "c-inv", recordedRejectionText, true),
			wantMissIDs: []string{"c-inv"},
		},
		{
			// An end that claims SUCCESS ran something: never excused.
			name: "a rejection text on a successful end stays unproven",
			body: startEvent("edit", "c-inv", recordedMalformedEditArgs) +
				textEnd("edit", "c-inv", recordedRejectionText, false),
			wantMissIDs: []string{"c-inv"},
		},
		{
			// Without the call's start there are no arguments to match.
			name:        "a rejection with no start stays unproven",
			body:        textEnd("edit", "c-inv", recordedRejectionText, true),
			wantMissIDs: []string{"c-inv"},
		},
		{
			// Same id, different tool: not the call the start named.
			name: "a rejection ending under another tool stays unproven",
			body: startEvent("edit", "c-inv", recordedMalformedEditArgs) +
				textEnd("write", "c-inv", strings.Replace(recordedRejectionText, `tool "edit"`, `tool "write"`, 1), true),
			wantMissIDs: []string{"c-inv"},
		},
		{
			// The rejection line must name the call's own tool.
			name: "a rejection naming another tool stays unproven",
			body: startEvent("edit", "c-inv", recordedMalformedEditArgs) +
				textEnd("edit", "c-inv", strings.Replace(recordedRejectionText, `tool "edit"`, `tool "write"`, 1), true),
			wantMissIDs: []string{"c-inv"},
		},
		{
			// An executed command's error result carries its exit status
			// after its output, so output that imitates a rejection is not
			// one.
			name: "command output imitating a rejection stays unproven",
			body: startEvent("bash", "c-bash", map[string]any{"command": "printf imitation; exit 1"}) +
				textEnd("bash", "c-bash", strings.Replace(recordedRejectionText, `tool "edit"`, `tool "bash"`, 1)+"\n\nCommand exited with code 1", true),
			wantMissIDs: []string{"c-bash"},
		},
		{
			// A result that carries tool details is not the runtime's bare
			// rejection.
			name: "a rejection text with result details stays unproven",
			body: startEvent("edit", "c-inv", recordedMalformedEditArgs) +
				event(map[string]any{
					"type": "tool_execution_end", "toolCallId": "c-inv", "toolName": "edit", "isError": true,
					"result": map[string]any{
						"content": []any{map[string]any{"type": "text", "text": recordedRejectionText}},
						"details": map[string]any{"diff": "+new"},
					},
				}),
			wantMissIDs: []string{"c-inv"},
		},
		{
			// A start note does not outlive its turn.
			name: "a start note does not survive turn_end",
			body: startEvent("edit", "c-inv", recordedMalformedEditArgs) +
				event(map[string]any{"type": "turn_end"}) +
				textEnd("edit", "c-inv", recordedRejectionText, true),
			wantMissIDs: []string{"c-inv"},
		},
		{
			// One start explains one end, not a second one.
			name: "a start explains a single end",
			body: rejectedCallPair("c-inv") +
				textEnd("edit", "c-inv", recordedRejectionText, true),
			wantMissIDs:     []string{"c-inv"},
			wantRejectedIDs: []string{"c-inv"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := getStateResponse("ses_fence") +
				event(map[string]any{"type": "agent_start"}) +
				tc.body +
				event(map[string]any{"type": "agent_settled"})

			_, h, err := spawnScripted(t, agent.Spec{Prompt: "hi", Cwd: t.TempDir(), Autonomous: true},
				handshakeEvent("h1"), body)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			evs := drainToResultOrClose(t, h)

			var fatal, completed, decisions, refusedEvents int
			var missEvents []agent.ErrorEvent
			for _, e := range evs {
				switch value := e.(type) {
				case agent.ErrorEvent:
					if value.Code == adjudicationMissingCode {
						missEvents = append(missEvents, value)
						continue
					}
					fatal++
					if value.Code != "policy_extension_failed" {
						t.Errorf("unexpected fatal error code %q: %s", value.Code, value.Message)
					}
				case agent.ResultEvent:
					completed++
				case agent.SystemEvent:
					switch value.Subtype {
					case "permission_decision":
						decisions++
					case outputLimitRefusalSubtype:
						refusedEvents++
					}
				}
			}

			refused := h.(*Handle).recordedOutputLimitRefusals()
			if refusedEvents != len(tc.wantRefusedIDs) {
				t.Errorf("%s SystemEvents = %d, want %d: %+v", outputLimitRefusalSubtype, refusedEvents, len(tc.wantRefusedIDs), evs)
			}
			if len(refused) != len(tc.wantRefusedIDs) {
				t.Fatalf("recorded output-limit refusals = %+v, want ids %q", refused, tc.wantRefusedIDs)
			}
			for i, want := range tc.wantRefusedIDs {
				if refused[i].callID != want || refused[i].tool == "" {
					t.Errorf("refusal[%d] = %+v, want call id %q with its tool", i, refused[i], want)
				}
			}

			if tc.wantFatal {
				if fatal != 1 {
					t.Errorf("fatal ErrorEvents = %d, want exactly 1: %+v", fatal, evs)
				}
				if completed != 0 {
					t.Errorf("session reached a ResultEvent after a demonstrated bypass: %+v", evs)
				}
			} else {
				if fatal != 0 {
					t.Errorf("session aborted on an unproven call: %+v", evs)
				}
				if completed != 1 {
					t.Errorf("session did not run to its own terminal (ResultEvents = %d): %+v", completed, evs)
				}
			}
			if decisions != tc.wantDecisions {
				t.Errorf("permission_decision SystemEvents = %d, want %d", decisions, tc.wantDecisions)
			}

			if len(missEvents) != len(tc.wantMissIDs) {
				t.Fatalf("%s events = %d, want %d: %+v", adjudicationMissingCode, len(missEvents), len(tc.wantMissIDs), missEvents)
			}
			misses := h.(*Handle).adjudicationMisses()
			if len(misses) != len(tc.wantMissIDs) {
				t.Fatalf("recorded misses = %+v, want ids %q", misses, tc.wantMissIDs)
			}
			for i, want := range tc.wantMissIDs {
				if misses[i].callID != want {
					t.Errorf("miss[%d] call id = %q, want %q", i, misses[i].callID, want)
				}
				if misses[i].tool == "" {
					t.Errorf("miss[%d] did not name the tool: %+v", i, misses[i])
				}
				if missEvents[i].Message == "" {
					t.Errorf("miss[%d] event carried no message", i)
				}
				if !missEvents[i].SessionContinues {
					t.Errorf("miss[%d] event does not say the session continues: %+v", i, missEvents[i])
				}
			}

			rejected := h.(*Handle).recordedArgumentRejections()
			if len(rejected) != len(tc.wantRejectedIDs) {
				t.Fatalf("recorded argument rejections = %+v, want ids %q", rejected, tc.wantRejectedIDs)
			}
			for i, want := range tc.wantRejectedIDs {
				if rejected[i].callID != want || rejected[i].tool == "" {
					t.Errorf("rejection[%d] = %+v, want call id %q with its tool", i, rejected[i], want)
				}
			}
		})
	}
}

// TestPolicyFence_RefusalRoundTripRecordsADenial pins the wire half of the
// refusal registration: the Go side answers the round-trip so the extension is
// never left waiting, and the call ends recorded as a denial rather than as an
// adjudication that allowed anything.
func TestPolicyFence_RefusalRoundTripRecordsADenial(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		token      string
		reason     string
		wantReply  string
		wantRuled  bool
		wantReason string
	}{
		{
			name:       "session token refusal is recorded",
			token:      testHandshakeToken,
			reason:     "donmai policy boundary requires a UI channel",
			wantReply:  "ok",
			wantRuled:  true,
			wantReason: "donmai policy boundary requires a UI channel",
		},
		{
			name:       "refusal with no reason still records one",
			token:      testHandshakeToken,
			wantReply:  "ok",
			wantRuled:  true,
			wantReason: "refused by the policy boundary before execution",
		},
		{
			name:      "forged token records nothing",
			token:     "not-the-session-token",
			reason:    "pretending to be the boundary",
			wantReply: "reject",
			wantRuled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := map[string]any{
				"donmai":     refusalKind,
				"token":      tc.token,
				"toolName":   "write",
				"toolCallId": "c-refusal",
			}
			if tc.reason != "" {
				payload["reason"] = tc.reason
			}
			cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi", Cwd: t.TempDir(), Autonomous: true},
				handshakeEvent("h1"),
				getStateResponse("ses_refusal")+uiRequest("r1", payload)+event(map[string]any{"type": "agent_settled"}))
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			drainToResultOrClose(t, h)

			var reply string
			for _, c := range cmds.commands() {
				if c["type"] == "extension_ui_response" && c["id"] == "r1" {
					reply, _ = c["value"].(string)
				}
			}
			if reply != tc.wantReply {
				t.Errorf("refusal round-trip reply = %q, want %q", reply, tc.wantReply)
			}

			outcome, ruled := h.(*Handle).adjudication("c-refusal")
			if ruled != tc.wantRuled {
				t.Fatalf("recorded outcome = %v, want %v", ruled, tc.wantRuled)
			}
			if !ruled {
				return
			}
			if outcome.allow {
				t.Error("an extension-side refusal was recorded as an allow")
			}
			if outcome.reason != tc.wantReason {
				t.Errorf("recorded reason = %q, want %q", outcome.reason, tc.wantReason)
			}
		})
	}
}

// forgedFrame builds a boundary round-trip carrying a token that is not the
// session's — the shape any extension co-resident in the child can raise,
// since the marker is all it takes to reach the handler.
func forgedFrame(reqID, kind, toolName, callID string) string {
	payload := map[string]any{
		"donmai":     kind,
		"token":      "not-the-session-token",
		"toolName":   toolName,
		"toolCallId": callID,
		"input":      map[string]any{"command": "echo hi"},
		"reason":     "pretending to be the boundary",
	}
	return uiRequest(reqID, payload)
}

// TestPolicyFence_UnverifiedFrameCannotWriteTheRegistry is the security half
// of the fence: the registry the monitor reads is written ONLY behind the
// handshake-token check, so an unauthenticated frame naming a call id can
// neither vouch for that call (suppressing the missing-adjudication record)
// nor overwrite a real ruling (disarming the session-fatal path). The frame is
// still answered on the wire so the caller never hangs, and still surfaced as
// an observation.
func TestPolicyFence_UnverifiedFrameCannotWriteTheRegistry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		// wantFatal is true when the stream must still abort.
		wantFatal bool
		// wantMissID is the call id that must be recorded as unproven; empty
		// means no miss is expected.
		wantMissID string
		// wantReply is the value the forged frame must be answered with.
		wantReply string
		// forgedReqID / forgedCallID identify the forged frame in the stream.
		forgedReqID  string
		forgedCallID string
	}{
		{
			// Suppression: a forged adjudication naming the call id of a
			// genuinely unadjudicated built-in execution. It must NOT silence
			// the record — the record is the whole compensating control.
			name: "forged adjudication does not suppress the miss",
			body: forgedFrame("a1", adjudicateKind, "bash", "c-forged") +
				endEvent("bash", "toolCallId", "c-forged", false),
			wantMissID:   "c-forged",
			wantReply:    `{"allow":false,"reason":"token mismatch"}`,
			forgedReqID:  "a1",
			forgedCallID: "c-forged",
		},
		{
			// Disarm: a real trusted deny, then a forged frame for the SAME
			// call id, then the denied call reported as having executed
			// successfully. The forged frame must not demote or erase the
			// denial, so the fatal still fires.
			name: "forged adjudication does not disarm a real denial",
			body: adjudicateEvent("a1", "bash", "c-denied", map[string]any{"command": "rm -rf /"}, "") +
				forgedFrame("a2", adjudicateKind, "bash", "c-denied") +
				endEvent("bash", "toolCallId", "c-denied", false),
			wantFatal:    true,
			wantReply:    `{"allow":false,"reason":"token mismatch"}`,
			forgedReqID:  "a2",
			forgedCallID: "c-denied",
		},
		{
			// The refusal round-trip is token-gated the same way, and its
			// forged form writes nothing either.
			name: "forged refusal does not suppress the miss",
			body: forgedFrame("a1", refusalKind, "write", "c-forged") +
				endEvent("write", "toolCallId", "c-forged", false),
			wantMissID:   "c-forged",
			wantReply:    "reject",
			forgedReqID:  "a1",
			forgedCallID: "c-forged",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := getStateResponse("ses_forged") +
				event(map[string]any{"type": "agent_start"}) +
				tc.body +
				event(map[string]any{"type": "agent_settled"})

			cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi", Cwd: t.TempDir(), Autonomous: true},
				handshakeEvent("h1"), body)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			evs := drainToResultOrClose(t, h)
			ph := h.(*Handle)

			var fatal, misses int
			for _, e := range evs {
				ee, ok := e.(agent.ErrorEvent)
				if !ok {
					continue
				}
				if ee.Code == adjudicationMissingCode {
					misses++
					continue
				}
				fatal++
			}
			wantFatal := 0
			if tc.wantFatal {
				wantFatal = 1
			}
			if fatal != wantFatal {
				t.Errorf("fatal ErrorEvents = %d, want %d: %+v", fatal, wantFatal, evs)
			}
			wantMisses := 0
			if tc.wantMissID != "" {
				wantMisses = 1
			}
			if misses != wantMisses {
				t.Errorf("%s events = %d, want %d: %+v", adjudicationMissingCode, misses, wantMisses, evs)
			}
			recorded := ph.adjudicationMisses()
			if len(recorded) != wantMisses {
				t.Fatalf("recorded misses = %+v, want %d", recorded, wantMisses)
			}
			if tc.wantMissID != "" && recorded[0].callID != tc.wantMissID {
				t.Errorf("recorded miss call id = %q, want %q", recorded[0].callID, tc.wantMissID)
			}

			// The forged frame never became any call's recorded outcome.
			if !tc.wantFatal {
				if outcome, ruled := ph.adjudication(tc.forgedCallID); ruled {
					t.Errorf("an unverified frame wrote the adjudication registry: %+v", outcome)
				}
			}
			if tc.wantFatal {
				if outcome, ruled := ph.adjudication("c-denied"); !ruled || outcome.allow ||
					outcome.reason != "rm of filesystem root blocked" {
					t.Errorf("the real denial was altered by an unverified frame: %+v (ruled=%v)", outcome, ruled)
				}
			}

			// It was answered on the wire and surfaced as an observation.
			var reply string
			for _, c := range cmds.commands() {
				if c["type"] == "extension_ui_response" && c["id"] == tc.forgedReqID {
					reply, _ = c["value"].(string)
				}
			}
			if reply != tc.wantReply {
				t.Errorf("unverified frame reply = %q, want %q", reply, tc.wantReply)
			}
			frames := ph.untrustedFrames()
			if len(frames) != 1 || frames[0].callID != tc.forgedCallID {
				t.Errorf("untrusted frames = %+v, want exactly one naming %q", frames, tc.forgedCallID)
			}
			var observed bool
			for _, e := range evs {
				if se, ok := e.(agent.SystemEvent); ok && se.Subtype == "unverified_extension_request" {
					observed = true
				}
			}
			if !observed {
				t.Error("the unverified frame was not surfaced as an observation")
			}
		})
	}
}

// TestExecutionSucceeded_RequiresAPositiveStatement pins what may arm the one
// session-fatal path: only a present boolean error flag saying the call did
// NOT error. It reads the same spellings the event mapper accepts, so the
// fatal path cannot go quietly dead on a spelling the rest of the package
// still understands, and an absent or non-boolean flag is never "succeeded".
func TestExecutionSucceeded_RequiresAPositiveStatement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		fields map[string]any
		want   bool
	}{
		{name: "isError false", fields: map[string]any{"isError": false}, want: true},
		{name: "isError true", fields: map[string]any{"isError": true}},
		{name: "error false", fields: map[string]any{"error": false}, want: true},
		{name: "error true", fields: map[string]any{"error": true}},
		{name: "preferred spelling wins", fields: map[string]any{"isError": true, "error": false}},
		{name: "absent is not success", fields: map[string]any{"toolName": "bash"}},
		{name: "non-boolean is not success", fields: map[string]any{"isError": "false"}},
		{name: "null is not success", fields: map[string]any{"isError": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := rawEvent{Type: "tool_execution_end", Fields: tc.fields}
			if got := executionSucceeded(ev); got != tc.want {
				t.Errorf("executionSucceeded(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

// TestExecutionErrored_RequiresAPositiveStatement pins the other half of the
// same rule: the output-limit refusal path excuses an unruled end only on a
// present boolean flag saying the call ERRORED. Absent or non-boolean is
// unknown, and unknown excuses nothing.
func TestExecutionErrored_RequiresAPositiveStatement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		fields map[string]any
		want   bool
	}{
		{name: "isError true", fields: map[string]any{"isError": true}, want: true},
		{name: "isError false", fields: map[string]any{"isError": false}},
		{name: "error true", fields: map[string]any{"error": true}, want: true},
		{name: "preferred spelling wins", fields: map[string]any{"isError": false, "error": true}},
		{name: "absent is not an error", fields: map[string]any{"toolName": "edit"}},
		{name: "non-boolean is not an error", fields: map[string]any{"isError": "true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := rawEvent{Type: "tool_execution_end", Fields: tc.fields}
			if got := executionErrored(ev); got != tc.want {
				t.Errorf("executionErrored(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

// TestPolicyFence_TokenMismatchIsAnsweredWithAReasonedDenial keeps the wire
// half of the token-mismatch path: the caller is answered promptly with a
// denial carrying a reason, so a forged frame is refused rather than left
// hanging.
func TestPolicyFence_TokenMismatchIsAnsweredWithAReasonedDenial(t *testing.T) {
	t.Parallel()
	body := getStateResponse("ses_forged") +
		event(map[string]any{"type": "agent_start"}) +
		forgedFrame("a1", adjudicateKind, "bash", "c-forged") +
		event(map[string]any{"type": "agent_settled"})

	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi", Cwd: t.TempDir(), Autonomous: true},
		handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drainToResultOrClose(t, h)

	var denied bool
	for _, c := range cmds.commands() {
		if c["type"] != "extension_ui_response" || c["id"] != "a1" {
			continue
		}
		var decision struct {
			Allow  bool   `json:"allow"`
			Reason string `json:"reason"`
		}
		value, _ := c["value"].(string)
		if json.Unmarshal([]byte(value), &decision) == nil && !decision.Allow && decision.Reason != "" {
			denied = true
		}
	}
	if !denied {
		t.Errorf("token mismatch was not answered with a reasoned denial: %v", cmds.commands())
	}
}

// TestToolCallID_AcceptsEveryWireSpelling pins the shared call-id rule both
// sides of the boundary read through (policy.go callIDFieldNames; the
// extension's toolCallIdOf mirrors the same order).
func TestToolCallID_AcceptsEveryWireSpelling(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{name: "toolCallId", fields: map[string]any{"toolCallId": "c1"}, want: "c1"},
		{name: "callId", fields: map[string]any{"callId": "c2"}, want: "c2"},
		{name: "call_id", fields: map[string]any{"call_id": "c3"}, want: "c3"},
		{name: "id", fields: map[string]any{"id": "c4"}, want: "c4"},
		{name: "preferred spelling wins", fields: map[string]any{"id": "c5", "toolCallId": "c6"}, want: "c6"},
		{name: "empty string is no id", fields: map[string]any{"toolCallId": ""}, want: ""},
		{name: "absent is no id", fields: map[string]any{"toolName": "bash"}, want: ""},
		// A numeric id must render exactly as the extension's toolCallIdOf
		// renders it (JavaScript String(n)) — otherwise the writer records
		// "123" while the reader gives up, and every honoured ruling on such a
		// runtime becomes an unproven call.
		{name: "integral number", fields: map[string]any{"toolCallId": float64(123)}, want: "123"},
		{name: "number under an alternative spelling", fields: map[string]any{"callId": float64(7)}, want: "7"},
		{name: "fractional number", fields: map[string]any{"id": 1.5}, want: "1.5"},
		{name: "json.Number", fields: map[string]any{"toolCallId": json.Number("42")}, want: "42"},
		{name: "zero is a real id", fields: map[string]any{"toolCallId": float64(0)}, want: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := toolCallID(tc.fields); got != tc.want {
				t.Errorf("toolCallID(%v) = %q, want %q", tc.fields, got, tc.want)
			}
		})
	}
}
