package worker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRegisterRequest_JSONRoundTrip(t *testing.T) {
	in := RegisterRequest{
		Hostname:     "mac-01",
		PID:          4242,
		Version:      "v1.2.3",
		Capabilities: []string{"claude", "codex"},
		MaxAgents:    4,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Check snake_case field names are present.
	s := string(data)
	for _, want := range []string{`"hostname"`, `"pid"`, `"version"`, `"capabilities"`, `"max_agents"`} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled output missing field %s: %s", want, s)
		}
	}
	var out RegisterRequest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestRegisterRequest_OmitEmpty(t *testing.T) {
	in := RegisterRequest{
		Hostname: "h",
		PID:      1,
		Version:  "v",
	}
	data, _ := json.Marshal(in)
	s := string(data)
	if strings.Contains(s, "capabilities") {
		t.Errorf("expected capabilities to be omitted: %s", s)
	}
	if strings.Contains(s, "max_agents") {
		t.Errorf("expected max_agents to be omitted: %s", s)
	}
}

func TestRegisterResponse_JSONRoundTripAndInterval(t *testing.T) {
	raw := `{"worker_id":"w1","runtime_jwt":"jwt","heartbeat_interval_seconds":30}`
	var r RegisterResponse
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.WorkerID != "w1" || r.RuntimeJWT != "jwt" || r.HeartbeatIntervalSeconds != 30 {
		t.Errorf("unexpected fields: %+v", r)
	}
	if got := r.HeartbeatInterval(); got != 30*time.Second {
		t.Errorf("HeartbeatInterval() = %v, want 30s", got)
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"heartbeat_interval_seconds":30`) {
		t.Errorf("expected heartbeat_interval_seconds in output: %s", string(data))
	}
}

func TestWorkItem_JSONRoundTrip(t *testing.T) {
	ts := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)
	in := WorkItem{
		ID:        "wi_1",
		Type:      "session.start",
		Payload:   json.RawMessage(`{"session":"s_1"}`),
		CreatedAt: ts,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"id":"wi_1"`, `"type":"session.start"`, `"payload":{"session":"s_1"}`, `"created_at":"2026-04-19T10:00:00Z"`} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled output missing %s: %s", want, s)
		}
	}

	var out WorkItem
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != in.ID || out.Type != in.Type {
		t.Errorf("scalars mismatch: %+v", out)
	}
	if !out.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", out.CreatedAt, in.CreatedAt)
	}
	// json.RawMessage equality is bytewise after possible reserialization;
	// check semantic equivalence by decoding.
	var got map[string]string
	if err := json.Unmarshal(out.Payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got["session"] != "s_1" {
		t.Errorf("payload = %+v", got)
	}
}

func TestHeartbeatRequest_JSONRoundTrip(t *testing.T) {
	in := HeartbeatRequest{ActiveAgentCount: 3, Status: "busy"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"active_agent_count":3`) {
		t.Errorf("missing active_agent_count: %s", string(data))
	}
	if !strings.Contains(string(data), `"status":"busy"`) {
		t.Errorf("missing status: %s", string(data))
	}
	var out HeartbeatRequest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestHeartbeatRequest_StatusOmitEmpty(t *testing.T) {
	in := HeartbeatRequest{ActiveAgentCount: 0}
	data, _ := json.Marshal(in)
	if strings.Contains(string(data), "status") {
		t.Errorf("expected status to be omitted: %s", string(data))
	}
}

func TestPollRequest_OmitEmpty(t *testing.T) {
	data, _ := json.Marshal(PollRequest{})
	if string(data) != `{}` {
		t.Errorf("empty PollRequest marshaled to %s, want {}", string(data))
	}
	data, _ = json.Marshal(PollRequest{MaxItems: 5})
	if !strings.Contains(string(data), `"max_items":5`) {
		t.Errorf("expected max_items:5 in %s", string(data))
	}
}

func TestPollResponse_JSONRoundTrip(t *testing.T) {
	raw := `{"work_items":[{"id":"wi_1","type":"t","payload":{"k":"v"},"created_at":"2026-01-02T03:04:05Z"}]}`
	var r PollResponse
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.WorkItems) != 1 {
		t.Fatalf("WorkItems = %+v", r.WorkItems)
	}
	if r.WorkItems[0].ID != "wi_1" {
		t.Errorf("ID = %q", r.WorkItems[0].ID)
	}
}

func TestHeartbeatResponse_EmptyBodyDecodes(t *testing.T) {
	var r HeartbeatResponse
	if err := json.Unmarshal([]byte(`{}`), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Ack {
		t.Errorf("Ack = true, want false")
	}
	if err := json.Unmarshal([]byte(`{"ack":true}`), &r); err != nil {
		t.Fatalf("unmarshal ack=true: %v", err)
	}
	if !r.Ack {
		t.Errorf("Ack = false after {\"ack\":true}")
	}
}
