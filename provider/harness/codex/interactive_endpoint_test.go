package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/coder/websocket"
)

const (
	codexEndpointFixtureEnv = "DONMAI_CODEX_ENDPOINT_FIXTURE"
	codexEndpointReportEnv  = "DONMAI_CODEX_ENDPOINT_REPORT_DIR"
	codexEndpointRootEnv    = "DONMAI_CODEX_ENDPOINT_FIXTURE_ROOT"
	codexEndpointThreadID   = "01a0548d-9a06-7a30-a72c-f7c94b8c899c"
)

type codexEndpointFixtureConfig struct {
	Provider  string `toml:"model_provider"`
	Providers map[string]struct {
		BaseURL string `toml:"base_url"`
		EnvKey  string `toml:"env_key"`
		WireAPI string `toml:"wire_api"`
		OpenAI  bool   `toml:"requires_openai_auth"`
	} `toml:"model_providers"`
	MCP map[string]any `toml:"mcp_servers"`
}

type codexEndpointReport struct {
	Role, Home, ConfigDigest, TokenDigest, EnvKey, Model string
	PID                                                  int
}

func endpointDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// This is a synthetic native-process role, not a Codex parser conformance
// fixture. It consumes the actual config/env/argv delivered by SpawnInteractive,
// sends one authenticated loopback Responses request, and records only metadata.
func runCodexEndpointFixture() int {
	if err := runCodexEndpointFixtureChecked(); err != nil {
		// Never print parser, network, environment or config values.
		_, _ = fmt.Fprintln(os.Stderr, "endpoint fixture refused projected configuration")
		return 2
	}
	return 0
}

func runCodexEndpointFixtureChecked() error {
	home := os.Getenv("CODEX_HOME")
	root, homeRelative, err := openCodexEndpointFixtureRoot(home)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	configRelative := filepath.Join(homeRelative, codexConfigFileName)
	body, err := root.ReadFile(configRelative)
	if err != nil {
		return err
	}
	var config codexEndpointFixtureConfig
	if _, err := toml.Decode(string(body), &config); err != nil {
		return err
	}
	selected, found := config.Providers[config.Provider]
	if !found || selected.BaseURL == "" || selected.EnvKey == "" || selected.WireAPI != "responses" || selected.OpenAI || len(config.MCP) != 0 {
		return errors.New("invalid provider metadata")
	}
	key := os.Getenv(selected.EnvKey)
	if key == "" || strings.Contains(string(body), key) || strings.Contains(strings.Join(os.Args, " "), key) {
		return errors.New("invalid credential placement")
	}
	for _, path := range []string{homeRelative, configRelative} {
		info, statErr := root.Stat(path)
		if statErr != nil || (path == homeRelative && info.Mode().Perm() != 0o700) || (path == configRelative && info.Mode().Perm() != 0o600) {
			return errors.New("private mode missing")
		}
	}
	if _, err := root.Lstat(filepath.Join(homeRelative, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("host login imported")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, selected.BaseURL+"/responses", strings.NewReader(`{"model":"fixture-model","input":"fixture"}`))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("selected bearer refused")
	}
	role := "pty"
	if argvHasFlag(os.Args, "--listen") {
		role = "server"
	}
	model := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "model=") {
			model = strings.TrimPrefix(arg, "model=")
		}
	}
	report, err := json.Marshal(codexEndpointReport{Role: role, Home: home, PID: os.Getpid(), ConfigDigest: endpointDigest(body), TokenDigest: endpointDigest([]byte(key)), EnvKey: selected.EnvKey, Model: model})
	if err != nil {
		return err
	}
	file, err := root.OpenFile(filepath.Join("reports", role+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(report)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if role == "server" {
		return serveCodexEndpointFixture(ctx)
	}
	if argvHasFlag(os.Args, "--remote") {
		return attachCodexEndpointFixture(ctx)
	}
	return nil
}

// The synthetic child receives one declared fixture-owned root. Validate its
// canonical temporary location and exact home/report descendants before any
// config/report I/O; rooted operations then prevent symlink/path escapes.
func openCodexEndpointFixtureRoot(home string) (*os.Root, string, error) {
	owned := os.Getenv(codexEndpointRootEnv)
	if !filepath.IsAbs(owned) || owned != filepath.Clean(owned) || !filepath.IsAbs(home) || home != filepath.Clean(home) {
		return nil, "", errors.New("fixture paths must be absolute and clean")
	}
	temporary, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return nil, "", errors.New("fixture temporary root cannot be resolved")
	}
	relative, err := filepath.Rel(temporary, owned)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, "", errors.New("fixture root is outside the owned temporary boundary")
	}
	canonical, err := filepath.EvalSymlinks(owned)
	if err != nil || canonical != owned {
		return nil, "", errors.New("fixture root must have canonical identity")
	}
	leaf := filepath.Base(home)
	if !strings.HasPrefix(leaf, codexHomePrefix) || leaf == codexHomePrefix || home != filepath.Join(owned, "boundaries", leaf) ||
		os.Getenv(codexEndpointReportEnv) != filepath.Join(owned, "reports") {
		return nil, "", errors.New("fixture home or report is outside its declared owned root")
	}
	root, err := os.OpenRoot(owned)
	if err != nil {
		return nil, "", errors.New("fixture owned root cannot be opened")
	}
	homeRelative := filepath.Join("boundaries", leaf)
	for _, entry := range []struct {
		path string
		dir  bool
	}{
		{".", true}, {"boundaries", true}, {"reports", true}, {homeRelative, true}, {filepath.Join(homeRelative, codexConfigFileName), false},
	} {
		info, err := root.Lstat(entry.path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || entry.dir && (!info.IsDir() || info.Mode().Perm() != 0o700) ||
			!entry.dir && (!info.Mode().IsRegular() || info.Mode().Perm() != 0o600) {
			_ = root.Close()
			return nil, "", errors.New("fixture path must have its exact private non-symlink shape")
		}
	}
	return root, homeRelative, nil
}

func codexEndpointArg(flag string) string {
	for index, arg := range os.Args {
		if arg == flag && index+1 < len(os.Args) {
			return strings.TrimPrefix(os.Args[index+1], "unix://")
		}
	}
	return ""
}

func serveCodexEndpointFixture(ctx context.Context) error {
	listener, err := net.Listen("unix", codexEndpointArg("--listen"))
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	hub := &fakeNamedAppServerHub{}
	var namingComplete atomic.Bool
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		hub.add(conn)
		defer func() { _ = conn.CloseNow() }()
		readbackDelivered := false
		defer func() {
			// The parent closes its diagnostic connection only after naming
			// and readback succeed. Keep the PTY alive until that point so
			// natural-exit cleanup cannot race the final naming RPC.
			if readbackDelivered {
				namingComplete.Store(true)
			}
		}()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var inbound rpcInbound
			if json.Unmarshal(raw, &inbound) != nil {
				return
			}
			var result any = map[string]any{}
			switch inbound.Method {
			case "initialize":
				result = map[string]any{"codexHome": os.Getenv("CODEX_HOME")}
			case "initialized":
				continue
			case "thread/start":
				started := map[string]any{"thread": map[string]any{"id": codexEndpointThreadID}}
				result = started
				notification, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "thread/started", Params: started})
				hub.broadcast(ctx, notification)
			case "thread/resume":
				result = map[string]any{"thread": map[string]any{"id": codexEndpointThreadID}}
			case "thread/name/set":
				var params struct{ Name string }
				_ = json.Unmarshal(inbound.Params, &params)
				hub.setName(params.Name)
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"id": codexEndpointThreadID, "name": hub.name()}}
				readbackDelivered = true
			case "fixture/named":
				result = map[string]any{"named": namingComplete.Load()}
			default:
				return
			}
			encoded, _ := json.Marshal(result)
			body, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: inbound.ID, Result: encoded})
			if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
				return
			}
		}
	})}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	return server.Serve(listener)
}

func attachCodexEndpointFixture(ctx context.Context) error {
	client, err := dialInteractiveWebSocket(ctx, codexEndpointArg("--remote"))
	if err != nil {
		return err
	}
	defer func() { _ = client.close() }()
	if argvHasFlag(os.Args, "resume") {
		_, err := client.request(ctx, "thread/resume", map[string]any{"threadId": codexEndpointThreadID}, time.Second)
		return err
	}
	if _, err := client.request(ctx, "thread/start", nil, time.Second); err != nil {
		return err
	}
	for {
		raw, err := client.request(ctx, "fixture/named", nil, time.Second)
		if err != nil {
			return err
		}
		var result struct{ Named bool }
		if json.Unmarshal(raw, &result) == nil && result.Named {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func endpointFixtureBinding(baseURL, token string) *agent.EndpointBinding {
	return &agent.EndpointBinding{
		Company: agent.CompanyOpenAI, Host: agent.HostGateway, Protocol: agent.ProtoOpenAIResponses,
		Model: "fixture-model", BaseURL: baseURL, Mechanism: agent.AuthAPIKey, Auth: agent.AuthBYOK,
		Env: map[string]string{gateway.TokenEnvVar: token},
	}
}

func TestSpawnInteractiveEndpointProcesses(t *testing.T) {
	// Each entry uses a different bearer and owns both the child processes and
	// HTTP fixture. No ambient host authentication is read or copied.
	for _, mode := range []string{"unnamed", "named-new", "named-resume"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			ambient := filepath.Join(root, "ambient")
			if err := os.Mkdir(ambient, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"config.toml": "model_provider = \"poison\"\n", "auth.json": "ambient-login-must-not-be-imported"} {
				if err := os.WriteFile(filepath.Join(ambient, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CODEX_HOME", ambient)
			t.Setenv("OPENAI_API_KEY", "ambient-api-key-must-not-select")
			t.Setenv("CODEX_ACCESS_TOKEN", "ambient-login-must-not-select")
			token := "selected-session-bearer-" + mode
			var requests atomic.Int32
			fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" || request.Header.Get("Authorization") != "Bearer "+token {
					writer.WriteHeader(http.StatusUnauthorized)
					return
				}
				requests.Add(1)
				_, _ = writer.Write([]byte(`{"id":"fixture-response","object":"response","status":"completed","output":[]}`))
			}))
			t.Cleanup(fixture.Close)
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			// MkdirTemp creates this directory with mode 0700; the outer
			// test-owned directory retains cleanup of its entire lifetime.
			fixtureRoot, err := os.MkdirTemp(t.TempDir(), "endpoint-fixture-")
			if err != nil {
				t.Fatal(err)
			}
			fixtureRoot, err = filepath.EvalSymlinks(fixtureRoot)
			if err != nil {
				t.Fatal(err)
			}
			boundaries, reports := filepath.Join(fixtureRoot, "boundaries"), filepath.Join(fixtureRoot, "reports")
			for _, directory := range []string{boundaries, reports} {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			spec := agent.Spec{
				Cwd: root, Endpoint: endpointFixtureBinding(fixture.URL+"/v1", token), Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
				Env: map[string]string{codexEndpointFixtureEnv: "1", codexEndpointReportEnv: reports, codexEndpointRootEnv: fixtureRoot, "CODEX_HOME": ambient},
			}
			if mode != "unnamed" {
				spec.SessionName = codexEndpointThreadID
				spec.Interactive.ResumeExisting = mode == "named-resume"
			}
			socket := ""
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			t.Cleanup(cancel)
			handle, err := SpawnInteractive(ctx, Options{
				CodexBin: self, configTempDir: boundaries, HandshakeTimeout: 5 * time.Second, RPCTimeout: 3 * time.Second,
				interactiveAuthSeeder: func(context.Context, string, string, interactiveCodexAuthProjection) error {
					return errors.New("endpoint selection reached host-login seed")
				},
				interactiveNameServerStarted: func(remote string) { socket = strings.TrimPrefix(remote, "unix://") },
			}, spec)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = handle.Stop(context.Background()) })
			terminal := false
			for {
				select {
				case event, open := <-handle.Events():
					if !open {
						if !terminal {
							t.Fatal("missing terminal event")
						}
						goto exited
					}
					if result, ok := event.(agent.ResultEvent); ok {
						terminal = true
						if !result.Success {
							t.Fatalf("endpoint child failed: %+v", result)
						}
					}
				case <-ctx.Done():
					t.Fatal("endpoint child deadline")
				}
			}
		exited:
			roles := []string{"pty"}
			var wantRequests int32 = 1
			if mode != "unnamed" {
				roles = append(roles, "server")
				wantRequests = 2
			}
			if requests.Load() != wantRequests {
				t.Fatalf("selected endpoint requests=%d, want %d", requests.Load(), wantRequests)
			}
			var previous codexEndpointReport
			for _, role := range roles {
				body, err := os.ReadFile(filepath.Join(reports, role+".json"))
				if err != nil {
					t.Fatal(err)
				}
				var report codexEndpointReport
				if err := json.Unmarshal(body, &report); err != nil {
					t.Fatal(err)
				}
				if report.EnvKey != gateway.TokenEnvVar || report.TokenDigest != endpointDigest([]byte(token)) || report.Model != `"fixture-model"` || report.Home == ambient {
					t.Fatalf("wrong projected metadata: %+v", report)
				}
				if previous.Home != "" && (previous.Home != report.Home || previous.ConfigDigest != report.ConfigDigest || previous.TokenDigest != report.TokenDigest) {
					t.Fatal("named PTY and app-server projection differs")
				}
				if _, err := os.Stat(report.Home); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("private home survived child exit: %v", err)
				}
				if err := syscall.Kill(report.PID, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("owned child still exists after cleanup: %v", err)
				}
				previous = report
			}
			if socket != "" {
				if _, err := os.Stat(filepath.Dir(socket)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("owned socket directory survived: %v", err)
				}
			}
			entries, err := os.ReadDir(boundaries)
			if err != nil || len(entries) != 0 {
				t.Fatalf("private boundary residue: entries=%d err=%v", len(entries), err)
			}
		})
	}
}

func TestSpawnInteractiveEndpointRefusesBeforeSideEffects(t *testing.T) {
	for _, mutation := range []struct {
		name string
		edit func(*agent.EndpointBinding)
		want error
	}{
		{"chat", func(binding *agent.EndpointBinding) { binding.Protocol = agent.ProtoOpenAIChat }, agent.ErrEndpointProtocolUnsupported},
		{"empty-protocol", func(binding *agent.EndpointBinding) { binding.Protocol = "" }, agent.ErrEndpointProtocolUnsupported},
		{"unknown-protocol", func(binding *agent.EndpointBinding) { binding.Protocol = "unknown" }, agent.ErrEndpointProtocolUnsupported},
		{"wrong-model", func(binding *agent.EndpointBinding) { binding.Model = "wrong" }, agent.ErrEndpointProtocolMismatch},
		{"empty-url", func(binding *agent.EndpointBinding) { binding.BaseURL = "" }, agent.ErrEndpointProtocolMismatch},
		{"unsafe-url", func(binding *agent.EndpointBinding) { binding.BaseURL = "http://remote.example/v1" }, agent.ErrEndpointProtocolMismatch},
		{"userinfo", func(binding *agent.EndpointBinding) {
			binding.BaseURL = "https://credential-sentinel@remote.example/v1"
		}, agent.ErrEndpointProtocolMismatch},
		{"query", func(binding *agent.EndpointBinding) { binding.BaseURL += "?key=credential-sentinel" }, agent.ErrEndpointProtocolMismatch},
		{"remote-gateway", func(binding *agent.EndpointBinding) { binding.BaseURL = "https://remote.example/v1" }, agent.ErrEndpointProtocolMismatch},
		{"missing-key", func(binding *agent.EndpointBinding) { binding.Env = nil }, agent.ErrEndpointProtocolMismatch},
		{"wrong-key", func(binding *agent.EndpointBinding) {
			binding.Env = map[string]string{"OTHER_KEY": "credential-sentinel"}
		}, agent.ErrEndpointProtocolMismatch},
		{"wrong-auth", func(binding *agent.EndpointBinding) { binding.Auth = agent.AuthHostSession }, agent.ErrEndpointProtocolMismatch},
		{"wrong-mechanism", func(binding *agent.EndpointBinding) { binding.Mechanism = agent.AuthOAuth }, agent.ErrEndpointProtocolMismatch},
		{"unknown-host", func(binding *agent.EndpointBinding) { binding.Host = "unknown" }, agent.ErrEndpointProtocolUnsupported},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			root := t.TempDir()
			binding := endpointFixtureBinding("http://127.0.0.1:1/v1", "credential-sentinel")
			mutation.edit(binding)
			_, err := SpawnInteractive(t.Context(), Options{CodexBin: filepath.Join(root, "absent-binary"), configTempDir: root}, agent.Spec{Cwd: root, Model: "fixture-model", Endpoint: binding, Interactive: &agent.InteractiveSpec{}})
			if !errors.Is(err, agent.ErrSpawnFailed) || !errors.Is(err, mutation.want) {
				t.Fatalf("wrong pre-spawn refusal: %v", err)
			}
			if strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatal("credential leaked through denial")
			}
			entries, readErr := os.ReadDir(root)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("pre-spawn side effects: entries=%d err=%v", len(entries), readErr)
			}
		})
	}
}

func TestInteractiveEndpointCredentialAndLegacySelection(t *testing.T) {
	for _, host := range []agent.ServingHost{agent.HostGateway, agent.HostDirect} {
		t.Run(string(host), func(t *testing.T) {
			binding := endpointFixtureBinding("http://127.0.0.1:1/v1", "selected-key")
			key := gateway.TokenEnvVar
			if host == agent.HostDirect {
				key = "OPENAI_API_KEY"
				binding.Host = host
				binding.Env = map[string]string{key: "selected-key"}
			}
			selection, err := prepareInteractiveCodexEndpoint(agent.Spec{Endpoint: binding})
			if err != nil || selection.envKey != key || selection.envValue != "selected-key" {
				t.Fatalf("declared endpoint key not selected: %v", err)
			}
			binding.Env = nil
			selection, err = prepareInteractiveCodexEndpoint(agent.Spec{Endpoint: binding, Env: map[string]string{key: "wire-key"}})
			if err != nil || selection.envValue != "wire-key" {
				t.Fatalf("wire-delivered key not selected: %v", err)
			}
			binding.Env = map[string]string{key: "binding-key"}
			_, err = prepareInteractiveCodexEndpoint(agent.Spec{Endpoint: binding, Env: map[string]string{key: "different-key"}})
			if !errors.Is(err, agent.ErrEndpointProtocolMismatch) {
				t.Fatalf("credential authority conflict not refused: %v", err)
			}
		})
	}
	for _, endpoint := range []*agent.EndpointBinding{nil, {}} {
		selection, err := prepareInteractiveCodexEndpoint(agent.Spec{Endpoint: endpoint})
		if err != nil || selection != nil {
			t.Fatalf("zero binding changed legacy selection: %v", err)
		}
	}
	legacy := &agent.EndpointBinding{
		Company: agent.CompanyOpenAI, Model: "fixture-model", Protocol: agent.ProtoOpenAIResponses,
		Host: agent.HostOAuthCLI, Mechanism: agent.AuthCLISession, Auth: agent.AuthHostSession,
	}
	selection, err := prepareInteractiveCodexEndpoint(agent.Spec{Endpoint: legacy})
	if err != nil || !selection.nativeHostLogin {
		t.Fatalf("valid legacy host-login binding changed: %v", err)
	}
	legacy.BaseURL = "https://endpoint.example/v1"
	if _, err := prepareInteractiveCodexEndpoint(agent.Spec{Endpoint: legacy}); !errors.Is(err, agent.ErrEndpointProtocolMismatch) {
		t.Fatalf("host-login binding accepted custom URL: %v", err)
	}
}

func TestSpawnInteractiveEndpointFailureCleansPrivateHome(t *testing.T) {
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("bounded fixture refusal")
	_, err = SpawnInteractive(t.Context(), Options{
		CodexBin: self, configTempDir: root,
		interactiveBeforePTYSpawn: func(context.Context, string, agent.Spec, interactiveLaunch, string) error { return want },
	},
		agent.Spec{Cwd: t.TempDir(), Endpoint: endpointFixtureBinding("http://127.0.0.1:1/v1", "selected-key"), Interactive: &agent.InteractiveSpec{}})
	if !errors.Is(err, want) {
		t.Fatalf("wrong setup refusal: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed setup retained private home: entries=%d err=%v", len(entries), err)
	}
}
