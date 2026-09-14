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
