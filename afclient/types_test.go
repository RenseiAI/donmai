package afclient

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestStopSessionResponseRoundTrip(t *testing.T) {
	in := StopSessionResponse{
		Stopped:        true,
		SessionID:      "sess-1",
		PreviousStatus: StatusWorking,
		NewStatus:      StatusStopped,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out StopSessionResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	for _, f := range []string{"stopped", "sessionId", "previousStatus", "newStatus"} {
		if !bytes.Contains(data, []byte(f)) {
			t.Errorf("marshalled output missing field %q: %s", f, data)
		}
	}
}

func TestChatSessionRequestMarshal(t *testing.T) {
	in := ChatSessionRequest{Prompt: "hello agent"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(data), `{"prompt":"hello agent"}`; got != want {
		t.Errorf("marshal = %s, want %s", got, want)
	}
	var out ChatSessionRequest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}

func TestChatSessionResponseRoundTrip(t *testing.T) {
	in := ChatSessionResponse{
		Delivered:     true,
		PromptID:      "p-123",
		SessionID:     "sess-1",
		SessionStatus: StatusWorking,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ChatSessionResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}

func TestReconnectSessionRequestOmitEmpty(t *testing.T) {
	// Empty request — both optional fields should be omitted.
	empty, err := json.Marshal(ReconnectSessionRequest{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if got := string(empty); got != `{}` {
		t.Errorf("empty request = %s, want {}", got)
	}

	cur := "abc"
	withCursor, err := json.Marshal(ReconnectSessionRequest{Cursor: &cur})
	if err != nil {
		t.Fatalf("marshal cursor: %v", err)
	}
	if !strings.Contains(string(withCursor), `"cursor":"abc"`) {
		t.Errorf("cursor not present: %s", withCursor)
	}
	if strings.Contains(string(withCursor), "lastEventId") {
		t.Errorf("lastEventId should be omitted when nil: %s", withCursor)
	}
}

func TestReconnectSessionRequestRoundTrip(t *testing.T) {
	cur := "cursor-42"
	lastID := "evt-99"
	in := ReconnectSessionRequest{Cursor: &cur, LastEventID: &lastID}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ReconnectSessionRequest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Cursor == nil || *out.Cursor != cur {
		t.Errorf("Cursor round-trip failed: got %v", out.Cursor)
	}
	if out.LastEventID == nil || *out.LastEventID != lastID {
		t.Errorf("LastEventID round-trip failed: got %v", out.LastEventID)
	}
}

func TestReconnectSessionResponseRoundTrip(t *testing.T) {
	in := ReconnectSessionResponse{
		Reconnected:   true,
		SessionID:     "sess-1",
		SessionStatus: StatusWorking,
		MissedEvents:  7,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ReconnectSessionResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	for _, f := range []string{"reconnected", "sessionId", "sessionStatus", "missedEvents"} {
		if !strings.Contains(string(data), f) {
			t.Errorf("marshalled output missing field %q: %s", f, data)
		}
	}
}
