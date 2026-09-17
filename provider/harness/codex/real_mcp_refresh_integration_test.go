//go:build codex_integration

package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/mcpheaders"
)

type refreshMCPFixture struct {
	t              *testing.T
	server         *httptest.Server
	tokenPath      string
	launchToken    string
	refreshedToken string

	mu          sync.Mutex
	toolHeaders []string
	toolBodies  []string
	allRequests int
}

func newRefreshMCPFixture(t *testing.T, tokenPath, launchToken, refreshedToken string) *refreshMCPFixture {
	t.Helper()
	f := &refreshMCPFixture{t: t, tokenPath: tokenPath, launchToken: launchToken, refreshedToken: refreshedToken}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *refreshMCPFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.allRequests++
	f.mu.Unlock()
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	var raw bytes.Buffer
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(&raw).Encode(request)
	if request.Method == "tools/call" {
		f.mu.Lock()
		f.toolHeaders = append(f.toolHeaders, r.Header.Get("Authorization"))
		f.toolBodies = append(f.toolBodies, raw.String())
		callIndex := len(f.toolHeaders)
		f.mu.Unlock()
		if callIndex == 2 && r.Header.Get("Authorization") == "Bearer "+f.launchToken {
			replacement := f.tokenPath + ".next"
			if err := os.WriteFile(replacement, []byte(f.refreshedToken), 0o600); err != nil {
				f.t.Error(err)
			}
			if err := os.Rename(replacement, f.tokenPath); err != nil {
				f.t.Error(err)
			}
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	var result any
	switch request.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "refresh-fixture", "version": "1"}}
	case "tools/list":
		result = map[string]any{"tools": []map[string]any{{"name": "fixture_refresh", "description": "refresh control", "inputSchema": map[string]any{"type": "object", "additionalProperties": false}}}}
	case "tools/call":
		result = map[string]any{"content": []map[string]any{{"type": "text", "text": "called:fixture_refresh"}}, "isError": false}
	case "ping":
		result = map[string]any{}
	default:
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result = map[string]any{}
	}
	body := map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (f *refreshMCPFixture) snapshot() ([]string, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.toolHeaders...), append([]string(nil), f.toolBodies...), f.allRequests
}

func requireDonmaiFixtureBinary(t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("DONMAI_BIN"))
	if path == "" {
		t.Fatal("DONMAI_BIN is required for the mandatory protected MCP helper fixture")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("DONMAI_BIN %q: %v", path, err)
	}
	t.Logf("Donmai helper executable sha256=%s", sha256File(t, path))
	return path
}

func helperServerForNative(t *testing.T, donmaiBin, tokenPath, url string) agent.MCPServerConfig {
	t.Helper()
	command, err := mcpheaders.BuildHelperCommand(donmaiBin, tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	server, err := agent.WithProtectedRuntimeMCPHeadersHelper(agent.MCPServerConfig{Name: "fixture-platform", Type: "http", URL: url}, command)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

type nativeRefreshRun struct {
	pid      int
	threadID string
	methods  []string
}

func runNativeRefreshCalls(t *testing.T, ctx context.Context, native, ownedHome, workdir string, server agent.MCPServerConfig, calls int) nativeRefreshRun {
	t.Helper()
	spec := agent.Spec{Cwd: workdir, Env: map[string]string{"OPENAI_API_KEY": "fixture-model-auth-not-a-real-credential"}, MCPServers: []agent.MCPServerConfig{server}}
	launch, err := buildInteractiveLaunch(spec)
	if err != nil {
		t.Fatal(err)
	}
	configArgs, err := interactiveConfigArgs(launch.argv)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, native, append([]string{"app-server", "--stdio"}, append(configArgs, "--strict-config")...)...) //nolint:gosec
	cmd.Dir = workdir
	cmd.Env = mergeEnv(nil, launch.env, ownedHome)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	client := NewClient(stdin, stdout)
	defer client.Stop(errors.New("fixture complete"))
	methods := []string{}
	request := func(method string, params map[string]any) json.RawMessage {
		methods = append(methods, method)
		raw, err := client.Request(ctx, method, params, 20*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return raw
	}
	_ = request("initialize", map[string]any{"clientInfo": map[string]any{"name": "refresh-fixture", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}})
	if err := client.Notify("initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	threadRaw := request("thread/start", map[string]any{"cwd": workdir, "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true})
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(threadRaw, &thread); err != nil || thread.Thread.ID == "" {
		t.Fatalf("thread=%s err=%v", threadRaw, err)
	}
	_ = request("mcpServerStatus/list", map[string]any{"threadId": thread.Thread.ID, "detail": "toolsAndAuthOnly"})
	for i := 0; i < calls; i++ {
		raw := request("mcpServer/tool/call", map[string]any{"threadId": thread.Thread.ID, "server": "fixture-platform", "tool": "fixture_refresh", "arguments": map[string]any{}})
		var response directToolCallResponse
		if err := json.Unmarshal(raw, &response); err != nil || response.IsError == nil || *response.IsError {
			t.Fatalf("call %d response=%s err=%v", i, raw, err)
		}
	}
	for _, method := range methods {
		if method == "turn/start" {
			t.Fatal("fixture sent a model turn")
		}
	}
	return nativeRefreshRun{pid: cmd.Process.Pid, threadID: thread.Thread.ID, methods: methods}
}

func seedActualMCPOAuthStore(t *testing.T, home string, server agent.MCPServerConfig, sentinel string) {
	t.Helper()
	key, err := codexMCPOAuthCredentialKey(server)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{key: map[string]any{"server_name": server.Name, "server_url": server.URL, "client_id": "fixture-client", "access_token": sentinel, "scopes": []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, codexMCPOAuthCredentialsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_RealCodexInteractiveMCPHeaderHelperRefreshControls(t *testing.T) {
	launcher, native := requireCodexFixtureBinaries(t)
	donmaiBin := requireDonmaiFixtureBinary(t)
	root := t.TempDir()
	workdir := filepath.Join(root, "work")
	home := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("mcp_oauth_credentials_store = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(root, "mcp-token")
	const launchToken = "launch-refresh-sentinel"
	const refreshedToken = "rotated-refresh-sentinel"
	if err := os.WriteFile(tokenPath, []byte(launchToken), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newRefreshMCPFixture(t, tokenPath, launchToken, refreshedToken)
	server := helperServerForNative(t, donmaiBin, tokenPath, fixture.server.URL+"/api/mcp/fixture-session")
	run := runNativeRefreshCalls(t, t.Context(), native, home, workdir, server, 2)
	headers, bodies, _ := fixture.snapshot()
	wantHeaders := []string{"Bearer " + launchToken, "Bearer " + launchToken, "Bearer " + refreshedToken}
	if !reflect.DeepEqual(headers, wantHeaders) {
		t.Fatalf("tool Authorization order=%v want=%v pid=%d thread=%s", headers, wantHeaders, run.pid, run.threadID)
	}
	if len(bodies) != 3 || bodies[1] != bodies[2] {
		t.Fatalf("retry bodies differ: %q", bodies)
	}

	const oauthSentinel = "stored-mcp-oauth-sentinel"
	oauthHome := filepath.Join(root, "oauth-home")
	if err := os.MkdirAll(oauthHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauthHome, "config.toml"), []byte("mcp_oauth_credentials_store = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oauthFixture := newRefreshMCPFixture(t, tokenPath, oauthSentinel, refreshedToken)
	oauthServer := helperServerForNative(t, donmaiBin, tokenPath, oauthFixture.server.URL+"/api/mcp/fixture-session")
	seedActualMCPOAuthStore(t, oauthHome, oauthServer, oauthSentinel)
	_ = runNativeRefreshCalls(t, t.Context(), native, oauthHome, workdir, oauthServer, 1)
	oauthHeaders, _, _ := oauthFixture.snapshot()
	if len(oauthHeaders) != 1 || oauthHeaders[0] != "Bearer "+oauthSentinel {
		t.Fatalf("pinned native MCP OAuth headers=%v", oauthHeaders)
	}

	wrapper, unexpectedPTYMarker := writeNoUIFixtureWrapper(t, launcher)
	_, _, requestsBeforeGuard := oauthFixture.snapshot()
	guardCallbackCalls := 0
	guardBoundaryRoot := filepath.Join(root, "guard-boundaries")
	if err := os.MkdirAll(guardBoundaryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	_, guardErr := SpawnInteractive(t.Context(), Options{
		CodexBin: wrapper, configTempDir: guardBoundaryRoot,
		interactiveAuthSeeder: func(_ context.Context, _ string, ownedHome string, _ interactiveCodexAuthProjection) error {
			seedActualMCPOAuthStore(t, ownedHome, oauthServer, oauthSentinel)
			return nil
		},
		interactiveBeforePTYSpawn: func(context.Context, string, agent.Spec, interactiveLaunch, string) error {
			guardCallbackCalls++
			return nil
		},
	}, agent.Spec{
		Cwd: workdir, Env: map[string]string{"OPENAI_API_KEY": "fixture-model-auth-not-a-real-credential"},
		MCPServers: []agent.MCPServerConfig{oauthServer}, Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if guardErr == nil || !strings.Contains(guardErr.Error(), "saved MCP OAuth authority") {
		t.Fatalf("production stored-OAuth guard err=%v", guardErr)
	}
	_, _, requestsAfterGuard := oauthFixture.snapshot()
	if guardCallbackCalls != 0 || requestsAfterGuard != requestsBeforeGuard {
		t.Fatalf("guard caused native activity: callback=%d requests=%d->%d", guardCallbackCalls, requestsBeforeGuard, requestsAfterGuard)
	}
	if _, err := os.Stat(unexpectedPTYMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guard spawned interactive process: %v", err)
	}
	requests := requestsAfterGuard
	if requests == 0 {
		t.Fatal("unguarded native OAuth control made no request")
	}
	identity := sha256.Sum256([]byte(strings.Join(run.methods, "\x00") + "\x00" + run.threadID))
	t.Logf("same-process refresh pid=%d thread=%s identity=%s", run.pid, run.threadID, hex.EncodeToString(identity[:]))
}
