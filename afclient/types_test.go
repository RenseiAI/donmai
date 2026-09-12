package afclient

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSessionStatusWireValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status SessionStatus
		want   string
	}{
		{StatusPending, "pending"},
		{StatusQueued, "queued"},
		{StatusParked, "parked"},
		{StatusStarting, "starting"},
		{StatusWorking, "working"},
		{StatusCompleted, "completed"},
		{StatusFailed, "failed"},
		{StatusStopped, "stopped"},
		{StatusTimedOut, "timed_out"},
	}

	for _, test := range tests {
		t.Run(test.want, func(t *testing.T) {
			t.Parallel()
			if got := string(test.status); got != test.want {
				t.Errorf("status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSessionStatusJSONPreservesUnknownValue(t *testing.T) {
	t.Parallel()

	var got SessionStatus
	if err := json.Unmarshal([]byte(`"future_status"`), &got); err != nil {
		t.Fatalf("unmarshal future status: %v", err)
	}
	if got != SessionStatus("future_status") {
		t.Errorf("status = %q, want future_status", got)
	}
}

func TestSessionsListResponseDecodesStartingAndTimedOut(t *testing.T) {
	t.Parallel()

	var resp SessionsListResponse
	data := []byte(`{"sessions":[{"id":"s-start","status":"starting"},{"id":"s-timeout","status":"timed_out"}],"count":2,"timestamp":"now"}`)
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal sessions: %v", err)
	}
	if got, want := len(resp.Sessions), 2; got != want {
		t.Fatalf("session count = %d, want %d", got, want)
	}
	if got := resp.Sessions[0].Status; got != StatusStarting {
		t.Errorf("first status = %q, want %q", got, StatusStarting)
	}
	if got := resp.Sessions[1].Status; got != StatusTimedOut {
		t.Errorf("second status = %q, want %q", got, StatusTimedOut)
	}
}

func TestStopSessionResponseRoundTrip(t *testing.T) {
	in := StopSessionResponse{
		Stopped:        true,
		SessionID:      "sess-1",
		PreviousStatus: StatusWorking,
		NewStatus:      StatusStopped,
		Receipt: &StopSessionReceipt{
			Version: 1, Kind: "session_stop", SessionID: "storage-1",
			MutationID: "linear-stop:external:storage-1", IntentRevision: "7",
			Disposition: StatusStopped, IdempotentReplay: true,
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out StopSessionResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	for _, f := range []string{"stopped", "sessionId", "previousStatus", "newStatus", "receipt", "mutationId", "intentRevision", "idempotentReplay"} {
		if !bytes.Contains(data, []byte(f)) {
			t.Errorf("marshalled output missing field %q: %s", f, data)
		}
	}
}

func TestStopSessionResponseDecodesLegacySuccessWithoutReceipt(t *testing.T) {
	var out StopSessionResponse
	if err := json.Unmarshal([]byte(`{"stopped":true,"sessionId":"public","previousStatus":"working","newStatus":"stopped"}`), &out); err != nil {
		t.Fatalf("unmarshal legacy success: %v", err)
	}
	if out.Receipt != nil || !out.Stopped || out.SessionID != "public" {
		t.Fatalf("legacy response changed: %+v", out)
	}
}

func TestStopSessionReceiptRejectsUnsafeFields(t *testing.T) {
	valid := `{"version":1,"kind":"session_stop","sessionId":"storage-uuid","mutationId":"linear-stop:reconcile:7:storage-uuid","intentRevision":"7","disposition":"stopped","idempotentReplay":false}`
	var legitimate StopSessionReceipt
	if err := json.Unmarshal([]byte(valid), &legitimate); err != nil {
		t.Fatalf("legitimate reconciliation receipt rejected: %v", err)
	}

	tests := []struct {
		name      string
		json      string
		forbidden string
	}{
		{"secret storage id", strings.Replace(valid, "storage-uuid", "rsk_do_not_echo", 1), "rsk_do_not_echo"},
		{"secret mutation id", strings.Replace(valid, "linear-stop:reconcile:7:storage-uuid", "Bearer do_not_echo", 1), "do_not_echo"},
		{"secret revision", strings.Replace(valid, `"intentRevision":"7"`, `"intentRevision":"sk-do_not_echo"`, 1), "sk-do_not_echo"},
		{"secret unknown key", strings.TrimSuffix(valid, "}") + `,"privateKey":"do_not_echo"}`, "do_not_echo"},
		{"overlength storage id", strings.Replace(valid, "storage-uuid", strings.Repeat("a", 257), 1), ""},
		{"non atom mutation id", strings.Replace(valid, "linear-stop:reconcile:7:storage-uuid", "mutation with spaces", 1), ""},
		{"invalid version", strings.Replace(valid, `"version":1`, `"version":2`, 1), ""},
		{"invalid kind", strings.Replace(valid, "session_stop", "other", 1), ""},
		{"nonterminal disposition", strings.Replace(valid, `"disposition":"stopped"`, `"disposition":"working"`, 1), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var receipt StopSessionReceipt
			err := json.Unmarshal([]byte(tc.json), &receipt)
			if err == nil {
				t.Fatalf("unsafe receipt decoded: %+v", receipt)
			}
			if tc.forbidden != "" && strings.Contains(err.Error(), tc.forbidden) {
				t.Fatalf("decode error leaked secret %q: %v", tc.forbidden, err)
			}
		})
	}
}

func TestStopSessionReceiptPreSessionReconciliationFamily(t *testing.T) {
	t.Parallel()

	const valid = `{"version":1,"kind":"pre_session_dispatch_reconciliation","orgId":"org-example","projectId":"project-example","sessionId":"storage-session","workflowInstanceId":"workflow-instance","previousStatus":"pending","terminalDisposition":"stopped","reason":"Explicit stop reconciled a terminal pre-session dispatch artifact.","actorId":"public:user","intentDigest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","recordedAt":"2026-09-11T00:42:01Z","idempotentReplay":true}`
	var receipt StopSessionReceipt
	if err := json.Unmarshal([]byte(valid), &receipt); err != nil {
		t.Fatalf("unmarshal reconciliation receipt: %v", err)
	}
	if receipt.OrganizationID != "org-example" || receipt.ProjectID != "project-example" ||
		receipt.WorkflowInstanceID != "workflow-instance" ||
		receipt.ReconciliationPreviousStatus != StatusPending ||
		receipt.TerminalDisposition != StatusStopped || !receipt.IdempotentReplay {
		t.Fatalf("reconciliation receipt fields lost: %+v", receipt)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal reconciliation receipt: %v", err)
	}
	var roundTrip StopSessionReceipt
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatalf("round-trip reconciliation receipt: %v", err)
	}
	if !reflect.DeepEqual(roundTrip, receipt) {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", roundTrip, receipt)
	}
}

func TestStopSessionReceiptRejectsInvalidPreSessionReconciliationFields(t *testing.T) {
	t.Parallel()

	const valid = `{"version":1,"kind":"pre_session_dispatch_reconciliation","orgId":"org-example","projectId":"project-example","sessionId":"storage-session","workflowInstanceId":"workflow-instance","previousStatus":"pending","terminalDisposition":"stopped","reason":"Explicit stop reconciled a terminal pre-session dispatch artifact.","actorId":"public:user","intentDigest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","recordedAt":"2026-09-11T00:42:01Z","idempotentReplay":false}`
	tests := []struct {
		name      string
		json      string
		forbidden string
	}{
		{"missing replay field", strings.Replace(valid, `,"idempotentReplay":false`, "", 1), ""},
		{"null replay field", strings.Replace(valid, `"idempotentReplay":false`, `"idempotentReplay":null`, 1), ""},
		{"runtime field hybrid", strings.TrimSuffix(valid, "}") + `,"mutationId":"stop:storage-session"}`, ""},
		{"missing organization", strings.Replace(valid, `"orgId":"org-example"`, `"orgId":""`, 1), ""},
		{"invalid previous status", strings.Replace(valid, `"previousStatus":"pending"`, `"previousStatus":"working"`, 1), ""},
		{"invalid terminal disposition", strings.Replace(valid, `"terminalDisposition":"stopped"`, `"terminalDisposition":"completed"`, 1), ""},
		{"control character in reason", strings.Replace(valid, `terminal pre-session`, `terminal\npre-session`, 1), ""},
		{"invalid digest", strings.Replace(valid, `0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`, strings.Repeat("f", 63), 1), ""},
		{"invalid recorded time", strings.Replace(valid, `2026-09-11T00:42:01Z`, `yesterday`, 1), ""},
		{"secret unknown key", strings.TrimSuffix(valid, "}") + `,"apiToken":"rsk_do_not_echo"}`, "rsk_do_not_echo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var receipt StopSessionReceipt
			err := json.Unmarshal([]byte(tc.json), &receipt)
			if err == nil {
				t.Fatalf("invalid receipt decoded: %+v", receipt)
			}
			if tc.forbidden != "" && strings.Contains(err.Error(), tc.forbidden) {
				t.Fatalf("decode error leaked secret %q: %v", tc.forbidden, err)
			}
		})
	}
}

func TestChatSessionRequestMarshal(t *testing.T) {
	in := ChatSessionRequest{Prompt: "hello agent", IdempotencyKey: "prompt-key-123"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(data), `{"prompt":"hello agent","idempotencyKey":"prompt-key-123"}`; got != want {
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
		Delivered:      false,
		DeliveryTier:   "durable",
		Disposition:    "held",
		IdempotencyKey: "prompt-key-123",
		PromptID:       "p-123",
		SessionID:      "sess-1",
		SessionStatus:  SessionStatus("held"),
		Code:           "TERMINAL_PROMPT_CONTINUITY_UNAVAILABLE",
		RecoveryAction: "start_new_session",
		MissingFields:  []string{"workType", "providerSessionId"},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ChatSessionResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
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
