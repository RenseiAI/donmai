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

			var fatal, completed, decisions int
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
					if value.Subtype == "permission_decision" {
						decisions++
					}
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

// TestPolicyFence_TokenMismatchRefusalIsRecordedUntrusted pins the fifth
// unregistered path: an adjudication request whose token does not match is
// still refused, that refusal is still recorded so the call is not a hole, and
// — because the payload is unauthenticated — it can never arm the fatal path
// for the call id it names.
func TestPolicyFence_TokenMismatchRefusalIsRecordedUntrusted(t *testing.T) {
	t.Parallel()
	forged := uiRequest("a1", map[string]any{
		"donmai":     adjudicateKind,
		"token":      "not-the-session-token",
		"toolName":   "bash",
		"toolCallId": "c-forged",
		"input":      map[string]any{"command": "echo hi"},
	})
	body := getStateResponse("ses_forged") +
		event(map[string]any{"type": "agent_start"}) +
		forged +
		endEvent("bash", "toolCallId", "c-forged", false) +
		event(map[string]any{"type": "agent_settled"})

	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi", Cwd: t.TempDir(), Autonomous: true},
		handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	evs := drainToResultOrClose(t, h)

	for _, e := range evs {
		if ee, ok := e.(agent.ErrorEvent); ok {
			t.Fatalf("an unauthenticated adjudication request ended the session: %+v", ee)
		}
	}
	outcome, ruled := h.(*Handle).adjudication("c-forged")
	if !ruled {
		t.Fatal("the token-mismatch refusal was not recorded as the call's outcome")
	}
	if outcome.allow || outcome.trusted {
		t.Errorf("token-mismatch outcome = %+v, want a refusal recorded untrusted", outcome)
	}

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
