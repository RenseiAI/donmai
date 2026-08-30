package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/a2a"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/spf13/cobra"
)

type a2aCLIFixture struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	calls  []map[string]any
	header []http.Header
	card   a2a.AgentCard
}

func newA2ACLIFixture(t *testing.T, requiredExtension string) *a2aCLIFixture {
	t.Helper()
	fixture := &a2aCLIFixture{t: t}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	extensions := []a2a.AgentExtension{}
	if requiredExtension != "" {
		extensions = append(extensions, a2a.AgentExtension{URI: requiredExtension, Required: true})
	}
	fixture.card = a2a.AgentCard{
		Name: "CLI fixture", Description: "Formal A2A fixture", Version: "1.0.0",
		SupportedInterfaces: []a2a.AgentInterface{{
			URL: fixture.server.URL + "/rpc", ProtocolBinding: a2a.ProtocolBindingJSONRPC,
			ProtocolVersion: "1.0.4", Tenant: "seat-opaque-1",
		}},
		Capabilities:       a2a.AgentCapabilities{Extensions: extensions},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []a2a.AgentSkill{},
	}
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *a2aCLIFixture) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/card" {
		if request.Header.Get(a2a.VersionHeader) != a2a.ProtocolVersion {
			f.t.Errorf("card version header = %q", request.Header.Get(a2a.VersionHeader))
		}
		_ = json.NewEncoder(w).Encode(f.card)
		return
	}
	if request.URL.Path != "/rpc" {
		http.NotFound(w, request)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		f.t.Errorf("decode RPC: %v", err)
	}
	f.mu.Lock()
	f.calls = append(f.calls, body)
	f.header = append(f.header, request.Header.Clone())
	f.mu.Unlock()
	method, _ := body["method"].(string)
	id := body["id"]
	var result any
	switch method {
	case "SendMessage":
		result = map[string]any{"task": cliTask("task-send", a2a.TaskStateCompleted)}
	case "GetTask":
		result = cliTask("task-get", a2a.TaskStateWorking)
	case "ListTasks":
		result = map[string]any{"tasks": []any{cliTask("task-list", a2a.TaskStateSubmitted)}, "nextPageToken": "next", "pageSize": 1, "totalSize": 1}
	case "CancelTask":
		result = cliTask("task-cancel", a2a.TaskStateCanceled)
	default:
		f.t.Errorf("unknown method %q", method)
		result = map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *a2aCLIFixture) snapshot() ([]map[string]any, []http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.calls...), append([]http.Header(nil), f.header...)
}

func cliTask(id string, state a2a.TaskState) map[string]any {
	return map[string]any{"id": id, "contextId": "context-1", "status": map[string]any{"state": state}}
}

func executeA2ACommand(t *testing.T, cfg Config, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newA2ACmd(cfg))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{"a2a"}, args...))
	err := root.ExecuteContext(context.Background())
	return output.String(), err
}

func TestA2ACLIFormalOperationsUseCardTenantAuthAndExtensions(t *testing.T) {
	t.Parallel()
	const extension = "https://example.test/a2a/fixture/v1"
	fixture := newA2ACLIFixture(t, extension)
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("ephemeral-fixture-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	commands := [][]string{
		{"send", "--card", fixture.server.URL + "/card", "--bearer-token-file", tokenPath, "--extension", extension, "--message-id", "message-1", "--message", "hello", "--json"},
		{"get", "--card", fixture.server.URL + "/card", "--bearer-token-file", tokenPath, "--extension", extension, "--id", "task-get", "--json"},
		{"list", "--card", fixture.server.URL + "/card", "--bearer-token-file", tokenPath, "--extension", extension, "--page-size", "10", "--json"},
		{"cancel", "--card", fixture.server.URL + "/card", "--bearer-token-file", tokenPath, "--extension", extension, "--id", "task-cancel", "--json"},
	}
	for _, command := range commands {
		output, err := executeA2ACommand(t, Config{A2AHTTPClient: fixture.server.Client()}, command...)
		if err != nil {
			t.Fatalf("a2a %v: %v\n%s", command, err, output)
		}
		if !json.Valid([]byte(output)) {
			t.Fatalf("a2a %v output is not JSON: %q", command, output)
		}
	}

	calls, headers := fixture.snapshot()
	if len(calls) != 4 {
		t.Fatalf("RPC calls = %d, want 4", len(calls))
	}
	wantMethods := []string{"SendMessage", "GetTask", "ListTasks", "CancelTask"}
	for index, call := range calls {
		if call["method"] != wantMethods[index] {
			t.Errorf("call %d method = %#v, want %s", index, call["method"], wantMethods[index])
		}
		params, _ := call["params"].(map[string]any)
		if params["tenant"] != "seat-opaque-1" {
			t.Errorf("call %d tenant = %#v", index, params["tenant"])
		}
		if headers[index].Get("Authorization") != "Bearer ephemeral-fixture-token" || headers[index].Get(a2a.VersionHeader) != "1.0" || headers[index].Get(a2a.ExtensionsHeader) != extension {
			t.Errorf("call %d headers auth=%q version=%q extensions=%q", index, headers[index].Get("Authorization"), headers[index].Get(a2a.VersionHeader), headers[index].Get(a2a.ExtensionsHeader))
		}
	}
	params := calls[0]["params"].(map[string]any)
	message := params["message"].(map[string]any)
	extensions, _ := message["extensions"].([]any)
	if message["messageId"] != "message-1" || len(extensions) != 1 || extensions[0] != extension {
		t.Fatalf("SendMessage = %#v", params)
	}
}

func TestA2ACLIRefusesMissingRequiredExtensionBeforeRPC(t *testing.T) {
	t.Parallel()
	fixture := newA2ACLIFixture(t, "https://example.test/a2a/required/v1")
	_, err := executeA2ACommand(t, Config{A2AHTTPClient: fixture.server.Client()},
		"send", "--card", fixture.server.URL+"/card", "--message", "hello")
	if err == nil || !strings.Contains(err.Error(), "required extension") {
		t.Fatalf("error = %v, want required-extension refusal", err)
	}
	calls, _ := fixture.snapshot()
	if len(calls) != 0 {
		t.Fatalf("RPC calls = %d, want zero", len(calls))
	}
}

func TestA2ACLIEmbedderPeerResolverAndAuth(t *testing.T) {
	t.Parallel()
	fixture := newA2ACLIFixture(t, "")
	var resolved string
	output, err := executeA2ACommand(t, Config{
		A2AHTTPClient: fixture.server.Client(),
		A2ACardURL: func(_ context.Context, peer string) (string, error) {
			resolved = peer
			return fixture.server.URL + "/card", nil
		},
		A2AAuthorization: func(context.Context) (string, error) { return "Bearer config-token", nil },
	}, "get", "--peer", "worker-7", "--id", "task-get", "--json")
	if err != nil {
		t.Fatalf("get: %v\n%s", err, output)
	}
	if resolved != "worker-7" {
		t.Fatalf("resolved peer = %q", resolved)
	}
	_, headers := fixture.snapshot()
	if headers[0].Get("Authorization") != "Bearer config-token" {
		t.Fatalf("authorization = %q", headers[0].Get("Authorization"))
	}
}

func TestA2ACLILeavesEmbedderRootInitializationAuthoritative(t *testing.T) {
	t.Parallel()
	fixture := newA2ACLIFixture(t, "")
	var initialized bool
	root := &cobra.Command{
		Use: "embedder",
		PersistentPreRunE: func(*cobra.Command, []string) error {
			initialized = true
			return nil
		},
	}
	root.AddCommand(newA2ACmd(Config{
		A2AHTTPClient: fixture.server.Client(),
		A2AAuthorization: func(context.Context) (string, error) {
			if !initialized {
				return "", errors.New("embedder authority was not initialized")
			}
			return "Bearer initialized-authority", nil
		},
	}))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"a2a", "get", "--card", fixture.server.URL + "/card", "--id", "task-get", "--json"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, output.String())
	}
	if !initialized {
		t.Fatal("embedding root PersistentPreRunE did not execute")
	}
	_, headers := fixture.snapshot()
	if len(headers) != 1 || headers[0].Get("Authorization") != "Bearer initialized-authority" {
		t.Fatalf("headers = %v, want initialized embedder authority", headers)
	}
}

func TestA2ACLIBearerFileRotatesBetweenSendAndPoll(t *testing.T) {
	t.Parallel()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("token-one\n"), 0o600); err != nil {
		t.Fatalf("write first token: %v", err)
	}
	var server *httptest.Server
	var mu sync.Mutex
	var authorizations []string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/card" {
			_ = json.NewEncoder(w).Encode(a2a.AgentCard{
				Name: "poll fixture", Description: "fixture", Version: "1",
				SupportedInterfaces: []a2a.AgentInterface{{URL: server.URL + "/rpc", ProtocolBinding: a2a.ProtocolBindingJSONRPC, ProtocolVersion: "1.0", Tenant: "seat-poll"}},
				Capabilities:        a2a.AgentCapabilities{}, DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"}, Skills: []a2a.AgentSkill{},
			})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		mu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		mu.Unlock()
		method := body["method"]
		state := a2a.TaskStateCompleted
		if method == "SendMessage" {
			state = a2a.TaskStateWorking
			if err := os.WriteFile(tokenPath, []byte("token-two\n"), 0o600); err != nil {
				t.Errorf("rotate token: %v", err)
			}
		}
		result := any(cliTask("task-poll", state))
		if method == "SendMessage" {
			result = map[string]any{"task": result}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body["id"], "result": result})
	}))
	t.Cleanup(server.Close)

	output, err := executeA2ACommand(t, Config{A2AHTTPClient: server.Client()},
		"send", "--card", server.URL+"/card", "--bearer-token-file", tokenPath,
		"--message", "hello", "--return-immediately", "--wait", "--poll-interval", "1ms", "--json")
	if err != nil {
		t.Fatalf("send --wait: %v\n%s", err, output)
	}
	mu.Lock()
	got := append([]string(nil), authorizations...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "Bearer token-one" || got[1] != "Bearer token-two" {
		t.Fatalf("authorizations = %v, want per-request rotation", got)
	}
}

func TestA2ACLIRequiresExplicitCardOrConfiguredPeerResolver(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"get", "--id", "task"},
		{"get", "--peer", "worker", "--id", "task"},
		{"get", "--card", "https://agent.example/card", "--peer", "worker", "--id", "task"},
	} {
		_, err := executeA2ACommand(t, Config{}, args...)
		if err == nil {
			t.Fatalf("a2a %v succeeded, want explicit-target refusal", args)
		}
	}
}

func TestA2ACommandRegistrationIsOptInForEmbedders(t *testing.T) {
	t.Parallel()
	factory := func() afclient.DataSource { return afclient.NewMockClient() }
	off := &cobra.Command{Use: "embedder"}
	RegisterCommands(off, Config{ClientFactory: factory})
	if findSub(off, "a2a") != nil {
		t.Fatal("a2a command registered without EnableA2AClient")
	}
	on := &cobra.Command{Use: "embedder"}
	RegisterCommands(on, Config{ClientFactory: factory, EnableA2AClient: true})
	formal := findSub(on, "a2a")
	if formal == nil {
		t.Fatal("a2a command missing with EnableA2AClient")
	}
	for _, name := range []string{"send", "get", "list", "cancel"} {
		if findSub(formal, name) == nil {
			t.Errorf("a2a command missing %s", name)
		}
	}
}
