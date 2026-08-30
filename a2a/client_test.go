package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type capturedRequest struct {
	Header http.Header
	Body   map[string]any
}

func TestClientCoreMethodsUseV1WireAndRotatingAuthorization(t *testing.T) {
	t.Parallel()

	requests := make(chan capturedRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests <- capturedRequest{Header: r.Header.Clone(), Body: body}
		id := body["id"]
		method, _ := body["method"].(string)
		var result any
		switch method {
		case "SendMessage":
			result = map[string]any{"task": taskJSON("task-1", TaskStateSubmitted)}
		case "GetTask":
			result = taskJSON("task-1", TaskStateWorking)
		case "ListTasks":
			result = map[string]any{"tasks": []any{taskJSON("task-1", TaskStateWorking)}, "nextPageToken": "", "pageSize": 1, "totalSize": 1}
		case "CancelTask":
			result = taskJSON("task-1", TaskStateCanceled)
		default:
			t.Errorf("unexpected method %q", method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}))
	t.Cleanup(server.Close)

	var authCalls atomic.Int32
	client, err := NewClientFromCard(AgentCard{
		SupportedInterfaces: []AgentInterface{
			{URL: "https://legacy.invalid", ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: "0.3"},
			{URL: server.URL, ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: "1.0", Tenant: "seat-7"},
		},
		Capabilities: AgentCapabilities{Extensions: []AgentExtension{{URI: "https://example.test/ext/v1"}}},
	}, WithTenant("forged-seat"), WithExtensions("https://example.test/ext/v1"), WithAuthorizationProvider(func(context.Context) (string, error) {
		return "Bearer token-" + string(rune('0'+authCalls.Add(1))), nil
	}))
	if err != nil {
		t.Fatalf("NewClientFromCard: %v", err)
	}

	ctx := context.Background()
	if _, err := client.SendMessage(ctx, SendMessageRequest{
		Message: Message{
			MessageID:  "message-1",
			Role:       RoleUser,
			Parts:      []Part{TextPart("hello")},
			Extensions: []string{"https://example.test/ext/v1"},
		},
		Configuration: &SendMessageConfiguration{ReturnImmediately: true},
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, err := client.GetTask(ctx, GetTaskRequest{ID: "task-1"}); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	pageSize := int32(10)
	if _, err := client.ListTasks(ctx, ListTasksRequest{PageSize: &pageSize}); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if _, err := client.CancelTask(ctx, CancelTaskRequest{ID: "task-1"}); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	wantMethods := []string{"SendMessage", "GetTask", "ListTasks", "CancelTask"}
	seenIDs := make(map[string]bool)
	for i, wantMethod := range wantMethods {
		got := <-requests
		if got.Body["jsonrpc"] != "2.0" || got.Body["method"] != wantMethod {
			t.Errorf("request %d envelope = %#v, want method %s", i, got.Body, wantMethod)
		}
		id, ok := got.Body["id"].(string)
		if !ok || id == "" || seenIDs[id] {
			t.Errorf("request %d id = %#v, want unique non-empty string", i, got.Body["id"])
		}
		seenIDs[id] = true
		params, _ := got.Body["params"].(map[string]any)
		if params["tenant"] != "seat-7" {
			t.Errorf("request %d tenant = %#v, want seat-7", i, params["tenant"])
		}
		if got.Header.Get(VersionHeader) != "1.0" || got.Header.Get(ExtensionsHeader) != "https://example.test/ext/v1" {
			t.Errorf("request %d A2A headers = version %q extensions %q", i, got.Header.Get(VersionHeader), got.Header.Get(ExtensionsHeader))
		}
		if got.Header.Get("Authorization") != "Bearer token-"+string(rune('1'+i)) {
			t.Errorf("request %d authorization = %q", i, got.Header.Get("Authorization"))
		}
		if i == 0 {
			message, _ := params["message"].(map[string]any)
			parts, _ := message["parts"].([]any)
			part, _ := parts[0].(map[string]any)
			configuration, _ := params["configuration"].(map[string]any)
			if message["messageId"] != "message-1" || message["role"] != "ROLE_USER" || part["text"] != "hello" {
				t.Errorf("SendMessage ProtoJSON = %#v", params)
			}
			if _, legacyKind := part["kind"]; legacyKind {
				t.Errorf("SendMessage part includes non-v1 kind discriminator: %#v", part)
			}
			if configuration["returnImmediately"] != true {
				t.Errorf("SendMessage configuration = %#v", configuration)
			}
		}
	}
}

func TestClientRefusesLegacyOnlyCard(t *testing.T) {
	t.Parallel()
	_, err := NewClientFromCard(AgentCard{SupportedInterfaces: []AgentInterface{{
		URL: "https://agent.example/rpc", ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: "0.3",
	}}})
	if err == nil || !strings.Contains(err.Error(), "no JSONRPC v1.0 interface") {
		t.Fatalf("error = %v, want strict v1 refusal", err)
	}
}

func TestClientRefusesUnknownRequiredCardExtensionBeforeNetwork(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)

	_, err := NewClientFromCard(AgentCard{
		SupportedInterfaces: []AgentInterface{{URL: server.URL, ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: "1.0"}},
		Capabilities:        AgentCapabilities{Extensions: []AgentExtension{{URI: "https://example.test/required/v1", Required: true}}},
	})
	if err == nil || !strings.Contains(err.Error(), "required extension") {
		t.Fatalf("error = %v, want required-extension refusal", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("network calls = %d, want zero", calls.Load())
	}
}

func TestDataPartPreservesFalseValueOnWire(t *testing.T) {
	t.Parallel()
	part := DataPart(false)
	raw, err := json.Marshal(part)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(raw) != `{"data":false}` {
		t.Fatalf("wire = %s, want false data oneof", raw)
	}
}

func TestClientReturnsRPCErrorWithoutTreatingItAsHTTPFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": request["id"],
			"error": map[string]any{"code": -32011, "message": "denied", "data": []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "AUTHORIZATION_DENIED"}}},
		})
	}))
	t.Cleanup(server.Close)
	client, _ := NewClient(server.URL)
	_, err := client.GetTask(context.Background(), GetTaskRequest{ID: "task-1"})
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32011 || !strings.Contains(string(rpcErr.Data), "AUTHORIZATION_DENIED") {
		t.Fatalf("error = %#v, want structured RPCError", err)
	}
}

func TestSendMessageParsesDirectMessageOneof(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result": map[string]any{"message": map[string]any{
				"messageId": "response-1",
				"contextId": "context-1",
				"role":      "ROLE_AGENT",
				"parts":     []any{map[string]any{"text": "ready"}},
			}},
		})
	}))
	t.Cleanup(server.Close)
	client, _ := NewClient(server.URL)
	response, err := client.SendMessage(context.Background(), SendMessageRequest{Message: Message{
		MessageID: "request-1", Role: RoleUser, Parts: []Part{TextPart("status")},
	}})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if response.Task != nil || response.Message == nil || response.Message.Role != RoleAgent || response.Message.Parts[0].Text == nil || *response.Message.Parts[0].Text != "ready" {
		t.Fatalf("response = %+v, want direct agent Message", response)
	}
}

func TestClientRejectsMismatchedResponseIDAndHTTPFailure(t *testing.T) {
	t.Parallel()
	t.Run("mismatched id", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wrong","result":{"id":"task-1","status":{"state":"TASK_STATE_WORKING"}}}`))
		}))
		t.Cleanup(server.Close)
		client, _ := NewClient(server.URL)
		_, err := client.GetTask(context.Background(), GetTaskRequest{ID: "task-1"})
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || !strings.Contains(err.Error(), "id did not match") {
			t.Fatalf("error = %#v, want response-id TransportError", err)
		}
	})

	t.Run("http failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		}))
		t.Cleanup(server.Close)
		client, _ := NewClient(server.URL)
		_, err := client.GetTask(context.Background(), GetTaskRequest{ID: "task-1"})
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || transportErr.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("error = %#v, want HTTP TransportError", err)
		}
	})
}

func TestWaitTaskPollsUntilInterruptedAndHonorsContext(t *testing.T) {
	t.Parallel()
	t.Run("stops on input required", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			state := TaskStateWorking
			if calls.Add(1) == 2 {
				state = TaskStateInputRequired
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": taskJSON("task-1", state)})
		}))
		t.Cleanup(server.Close)
		client, _ := NewClient(server.URL, WithPollInterval(time.Millisecond))
		task, err := client.WaitTask(context.Background(), "task-1")
		if err != nil || task.Status.State != TaskStateInputRequired || calls.Load() != 2 {
			t.Fatalf("WaitTask = (%+v, %v), calls=%d", task, err, calls.Load())
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": taskJSON("task-1", TaskStateWorking)})
		}))
		t.Cleanup(server.Close)
		client, _ := NewClient(server.URL, WithPollInterval(time.Hour))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.WaitTask(ctx, "task-1")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitTask error = %v, want context.Canceled", err)
		}
	})
}

func TestSendMessageRejectsMalformedOneofResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{}})
	}))
	t.Cleanup(server.Close)
	client, _ := NewClient(server.URL)
	_, err := client.SendMessage(context.Background(), SendMessageRequest{Message: Message{MessageID: "message-1", Role: RoleUser, Parts: []Part{DataPart(map[string]any{"hello": "world"})}}})
	var transportErr *TransportError
	if !errors.As(err, &transportErr) || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("error = %#v, want malformed-oneof TransportError", err)
	}
}

func taskJSON(id string, state TaskState) map[string]any {
	return map[string]any{"id": id, "contextId": "context-1", "status": map[string]any{"state": state}}
}
