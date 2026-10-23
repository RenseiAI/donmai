//go:build codex_integration

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/RenseiAI/donmai/gateway/costfeed"
	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/pool"
	"github.com/RenseiAI/donmai/gateway/token"
	"github.com/RenseiAI/donmai/gateway/translate"
	"github.com/RenseiAI/donmai/gateway/upstream"
	"github.com/RenseiAI/donmai/sessionshim"
)

const nativeEndpointModel = "gpt-5-codex"

// This mandatory tagged fixture runs only against the exact native version.
// Every child receives a synthetic HOME and no account credential. A missing
// executable or a different version is a failure, never a skipped proof.
func nativeEndpointBinary(t *testing.T) string {
	t.Helper()
	for _, key := range codexEnvironmentAuthKeys {
		t.Setenv(key, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv("TERM", "xterm-256color")
	name := strings.TrimSpace(os.Getenv("DONMAI_TEST_CODEX_BIN"))
	if name == "" {
		name = "codex"
	}
	binary, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("mandatory offline Codex fixture executable missing: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version") //nolint:gosec // explicitly selected pinned offline fixture executable
	output, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(output)) != "codex-cli 0.157.1" {
		t.Fatalf("mandatory offline Codex version mismatch: got %q, error=%v", strings.TrimSpace(string(output)), err)
	}
	return binary
}

func nativeEndpointSpec(t *testing.T, baseURL string) agent.Spec {
	t.Helper()
	project := t.TempDir()
	for _, name := range []string{".git", ".codex"} {
		if err := os.Mkdir(filepath.Join(project, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Pinned upstream config/src/loader/mod.rs denies both model_provider and
	// model_providers from project-local config. Exercise that actual parser
	// rule with a trusted project rather than changing production trust policy.
	poison := "model_provider = \"project_poison\"\n[model_providers.project_poison]\nname = \"Project poison\"\nbase_url = \"http://127.0.0.1:1/v1\"\nwire_api = \"responses\"\nenv_key = \"PROJECT_POISON_KEY\"\n"
	if err := os.WriteFile(filepath.Join(project, ".codex", "config.toml"), []byte(poison), 0o600); err != nil {
		t.Fatal(err)
	}
	binding := endpointFixtureBinding(baseURL, "offline-native-endpoint-bearer")
	binding.Model = nativeEndpointModel
	return agent.Spec{Cwd: project, Endpoint: binding, Model: nativeEndpointModel,
		Env: map[string]string{"HOME": os.Getenv("HOME"), "TERM": "xterm-256color", "CODEX_HOME": os.Getenv("CODEX_HOME"),
			"OPENAI_API_KEY": "", "CODEX_API_KEY": "", "CODEX_ACCESS_TOKEN": "", "PROJECT_POISON_KEY": ""},
		Interactive: &agent.InteractiveSpec{Cols: 120, Rows: 40}}
}

func TestIntegration_RealCodexEndpointEffectiveConfig(t *testing.T) {
	binary := nativeEndpointBinary(t)
	spec := nativeEndpointSpec(t, "http://127.0.0.1:1/v1")
	boundaries := t.TempDir()
	wantStop := errors.New("native effective config verified before PTY")
	checked := false
	_, spawnErr := SpawnInteractive(t.Context(), Options{CodexBin: binary, configTempDir: boundaries,
		interactiveBeforePTYSpawn: func(ctx context.Context, bin string, selectedSpec agent.Spec, launch interactiveLaunch, home string) error {
			server, err := startNamedInteractiveAppServer(ctx, bin, Options{HandshakeTimeout: 10 * time.Second, RPCTimeout: 5 * time.Second},
				selectedSpec, launch, launch.env, home)
			if err != nil {
				return err
			}
			defer func() {
				if err := server.close(); err != nil {
					t.Errorf("native config parser cleanup: %v", err)
				}
				if _, err := os.Stat(server.socketDir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("native parser socket survived: %v", err)
				}
			}()
			raw, err := server.getClient().request(ctx, "config/read", map[string]any{"includeLayers": true, "cwd": selectedSpec.Cwd}, 5*time.Second)
			if err != nil {
				return err
			}
			var result struct{ Config map[string]any }
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			providers, ok := result.Config["model_providers"].(map[string]any)
			if !ok || result.Config["model_provider"] != "donmai_session" || result.Config["model"] != nativeEndpointModel {
				return errors.New("native parser did not select the session provider/model")
			}
			selected, ok := providers["donmai_session"].(map[string]any)
			if !ok || selected["base_url"] != selectedSpec.Endpoint.BaseURL || selected["env_key"] != "DONMAI_GW_TOKEN" ||
				selected["wire_api"] != "responses" || selected["requires_openai_auth"] != false {
				return errors.New("native parser changed selected provider metadata")
			}
			if _, found := providers["project_poison"]; found {
				return errors.New("native parser admitted project provider poison")
			}
			checked = true
			return wantStop
		}}, spec)
	if !checked || !errors.Is(spawnErr, wantStop) {
		t.Fatalf("mandatory native config read failed: %v", spawnErr)
	}
	entries, err := os.ReadDir(boundaries)
	if err != nil || len(entries) != 0 {
		t.Fatalf("native config-only caller retained private boundary: count=%d error=%v", len(entries), err)
	}
	t.Log("actual Codex 0.157.1 config/read retained selected provider, model, URL and env-key name despite trusted project provider poison")
}

// The three SSE events use the pinned upstream core/tests/common/responses.rs
// helpers' shapes: ev_response_created, ev_assistant_message, ev_completed.
// The response is generated locally and has no provider/account involvement.
func nativeEndpointSSE(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "offline-response"}},
		{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "offline-message", "content": []map[string]any{{"type": "output_text", "text": "OFFLINE_OK"}}}},
		{"type": "response.completed", "response": map[string]any{"id": "offline-response", "usage": map[string]any{"input_tokens": 1, "input_tokens_details": nil, "output_tokens": 1, "output_tokens_details": nil, "total_tokens": 2}}},
	} {
		body, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event["type"], body)
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// nativeEndpointChildren reads only this fixture process's own direct child
// lists, across its Go runtime threads. It never scans unrelated processes or
// matches process names. The native consumer test records OS start identities
// for the newly owned children and probes those exact identities after Stop.
func nativeEndpointChildren(t *testing.T) map[int]bool {
	t.Helper()
	threads, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatalf("offline native fixture requires Linux child-identity evidence: %v", err)
	}
	pids := make(map[int]bool)
	for _, thread := range threads {
		body, err := os.ReadFile(filepath.Join("/proc/self/task", thread.Name(), "children"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range strings.Fields(string(body)) {
			pid, err := strconv.Atoi(value)
			if err != nil {
				t.Fatal(err)
			}
			pids[pid] = true
		}
	}
	return pids
}

func TestIntegration_RealCodexEndpointSeededInteractiveTurn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatal("mandatory offline native process-identity fixture must run in Linux")
	}
	binary := nativeEndpointBinary(t)
	const seed = "Reply briefly to the offline endpoint fixture."
	var accepted atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" || request.Header.Get("Authorization") != "Bearer offline-native-endpoint-bearer" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 2<<20))
		var payload struct {
			Model string          `json:"model"`
			Input json.RawMessage `json:"input"`
		}
		if err != nil || json.Unmarshal(body, &payload) != nil || payload.Model != nativeEndpointModel || !strings.Contains(string(payload.Input), seed) {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if accepted.Add(1) != 1 {
			writer.WriteHeader(http.StatusConflict)
			return
		}
		nativeEndpointSSE(writer)
	}))
	t.Cleanup(fixture.Close)
	// Even a deliberate wiring-removal control cannot reach a paid origin:
	// built-in provider fallback has no credential and its base URL is local.
	t.Setenv("OPENAI_BASE_URL", fixture.URL+"/v1")
	spec := nativeEndpointSpec(t, fixture.URL+"/v1")
	spec.Prompt = seed
	boundaries := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	before := nativeEndpointChildren(t)
	privateHome := ""
	handle, err := SpawnInteractive(ctx, Options{CodexBin: binary, configTempDir: boundaries,
		interactiveBeforePTYSpawn: func(_ context.Context, _ string, _ agent.Spec, _ interactiveLaunch, home string) error {
			privateHome = home
			return nil
		}}, spec)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = handle.Stop(stopCtx)
		}
	})
	interactive, ok := handle.(agent.InteractiveCapable)
	if !ok {
		t.Fatal("native endpoint caller returned no interactive surface")
	}
	session := interactive.InteractiveSession()
	subscription, err := session.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscription.Close() })
	owned := []sessionshim.ProcessIdentity{}
	for pid := range nativeEndpointChildren(t) {
		if !before[pid] {
			identity, err := sessionshim.ProcessIdentityFor(pid)
			if err != nil {
				t.Fatal(err)
			}
			owned = append(owned, identity)
		}
	}
	if len(owned) == 0 {
		t.Fatal("native interactive child identity was not observed")
	}
	var output strings.Builder
	pendingQueries := ""
	for !strings.Contains(output.String(), "OFFLINE_OK") {
		select {
		case frame, open := <-subscription.Frames():
			if !open || frame.Type == attachwire.TypeExit {
				t.Fatal("native endpoint consumer exited before rendering the offline response")
			}
			if frame.Type != attachwire.TypeOutput {
				continue
			}
			output.Write(frame.Payload)
			pendingQueries += string(frame.Payload)
			for _, query := range []struct{ request, response string }{
				{"\x1b[6n", "\x1b[1;1R"}, {"\x1b[?u", "\x1b[?0u"}, {"\x1b[c", "\x1b[?1;2c"},
				{"\x1b]10;?\x1b\\", "\x1b]10;rgb:ffff/ffff/ffff\x1b\\"}, {"\x1b]11;?\x1b\\", "\x1b]11;rgb:0000/0000/0000\x1b\\"},
			} {
				if strings.Contains(pendingQueries, query.request) {
					if _, err := session.WriteInput([]byte(query.response)); err != nil {
						t.Fatal(err)
					}
					pendingQueries = strings.ReplaceAll(pendingQueries, query.request, "")
				}
			}
			if len(pendingQueries) > 256 {
				pendingQueries = pendingQueries[len(pendingQueries)-256:]
			}
		case <-ctx.Done():
			t.Fatalf("native endpoint consumer deadline: accepted requests=%d, output bytes=%d", accepted.Load(), output.Len())
		}
	}
	if accepted.Load() != 1 || strings.Contains(output.String(), "offline-native-endpoint-bearer") {
		t.Fatal("native endpoint request count or secret boundary failed")
	}
	select {
	case <-session.Done():
		t.Fatal("native consumer exited instead of remaining interactive after its seeded turn")
	case <-time.After(100 * time.Millisecond):
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := handle.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	stopped = true
	for _, identity := range owned {
		alive, err := identity.Alive()
		if err != nil || alive {
			t.Fatalf("exact owned native child survived Stop: pid=%d alive=%v error=%v", identity.PID, alive, err)
		}
	}
	// Production retains real rollout state for resume. Verify that exact
	// lifecycle rule instead of deleting native conversation state to make
	// this fixture pass; t.TempDir owns the eventual fixture-directory removal.
	if hasResumableSessionState(privateHome) {
		if _, err := os.Stat(filepath.Join(privateHome, codexPluginCacheSubdir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("native retained home kept reclaimable cache: %v", err)
		}
		config, err := os.ReadFile(filepath.Join(privateHome, codexConfigFileName))
		if err != nil || strings.Contains(string(config), "offline-native-endpoint-bearer") {
			t.Fatal("retained native resume config secret boundary failed")
		}
	} else if _, err := os.Stat(privateHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-resumable native private home survived Stop: %v", err)
	}
	t.Log("actual Codex 0.157.1 seeded interactive turn reached the authenticated loopback Responses fixture, rendered OFFLINE_OK, stayed interactive, and stopped its exact owned child")
}

// This exercises the actual native PTY consumer through the public gateway
// binding, token route, credential pool, Responses codecs, canonical IR and
// cost sink. It is a component caller fixture; it does not assert that a
// producer's qualified execution-cell admission has been implemented.
func TestIntegration_RealCodexGatewayResponsesSeededTurn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatal("mandatory offline native gateway fixture must run in Linux")
	}
	binary := nativeEndpointBinary(t)
	sink := &costfeed.MemorySink{}
	proxy := gateway.New(gateway.Options{Sink: sink})
	if err := proxy.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := proxy.Stop(ctx); err != nil {
			t.Errorf("owned gateway stop: %v", err)
		}
		if proxy.Started() || proxy.Routes() != 0 {
			t.Error("owned gateway listener or route state survived Stop")
		}
	})
	previousTokenDigest := ""
	for _, sessionID := range []string{"native-gateway-a", "native-gateway-b"} {
		t.Run(sessionID, func(t *testing.T) {
			seed := "Reply briefly to the offline gateway fixture " + sessionID + "."
			upstreamKey := "offline-upstream-key-" + sessionID
			var mainRequests, titleRequests atomic.Int32
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("native gateway exact legs: main=%d title=%d", mainRequests.Load(), titleRequests.Load())
				}
			})
			fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				// The backend must receive the selected pool credential, never
				// the gateway bearer delivered privately to the native child.
				if request.Method != http.MethodPost || request.URL.Path != "/responses" || request.Header.Get("Authorization") != "Bearer "+upstreamKey {
					writer.WriteHeader(http.StatusUnauthorized)
					return
				}
				body, err := io.ReadAll(io.LimitReader(request.Body, 2<<20))
				decoded, decodeErr := translate.DecodeResponsesRequest(body)
				if err != nil || decodeErr != nil || decoded.Model != nativeEndpointModel || !decoded.Stream || !strings.Contains(string(body), seed) {
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				// The pinned TUI concurrently starts one hidden title thread
				// and one visible tool-capable turn. Correlate by their actual
				// request contracts instead of arrival order or a global quota.
				leg := nativeGatewayRequestLeg(decoded)
				var requests *atomic.Int32
				switch leg {
				case "main":
					requests = &mainRequests
				case "title":
					requests = &titleRequests
				default:
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if requests.Add(1) != 1 {
					writer.WriteHeader(http.StatusConflict)
					return
				}
				nativeGatewayLegSSE(writer, sessionID, leg)
			}))
			t.Cleanup(fixture.Close)
			binding, err := proxy.BindProtocol(gateway.BindConfig{SessionID: sessionID, DispatchID: sessionID + "-dispatch",
				Harness: "codex", Company: agent.CompanyOpenAI, Model: nativeEndpointModel, AuthMode: agent.AuthBYOK,
				Upstream: &upstream.OpenAIResponses{OpenAICompat: upstream.OpenAICompat{Company: "openai", BaseURL: fixture.URL}},
				Source:   pool.SingleKey{Key: pool.Credential{ID: sessionID + "-key", Secret: upstreamKey}}}, agent.ProtoOpenAIResponses)
			if err != nil {
				t.Fatal(err)
			}
			bearer := binding.Env[gateway.TokenEnvVar]
			digest := endpointDigest([]byte(bearer))
			if binding.Protocol != agent.ProtoOpenAIResponses || binding.Host != agent.HostGateway || bearer == "" || bearer == upstreamKey || digest == previousTokenDigest {
				t.Fatal("gateway did not mint a distinct exact Responses session binding")
			}
			previousTokenDigest = digest
			t.Cleanup(func() { proxy.Unbind(token.Token(bearer)) })
			// Keep any deliberate native-config removal control loopback-only.
			t.Setenv("OPENAI_BASE_URL", binding.BaseURL)
			spec := nativeEndpointSpec(t, binding.BaseURL)
			spec.Endpoint = &binding
			spec.Prompt = seed
			runNativeGatewayEndpointTurn(t, binary, spec, bearer, upstreamKey)
			if mainRequests.Load() != 1 || titleRequests.Load() != 1 {
				t.Fatalf("native gateway exact leg counts: main=%d title=%d, want one each", mainRequests.Load(), titleRequests.Load())
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				rows := sink.Events()
				count, mainUsage, titleUsage := 0, 0, 0
				for _, row := range rows {
					if row.SessionID != sessionID {
						continue
					}
					count++
					if row.DispatchID != sessionID+"-dispatch" || row.Harness != "codex" || row.ProviderID != "openai" || row.Host != costfeed.Host ||
						row.AuthMode != string(agent.AuthBYOK) || row.Model != nativeEndpointModel || row.TokensOut != 1 {
						t.Fatalf("native gateway usage attribution changed: %+v", row)
					}
					// Distinct, exact usage vectors correlate the two successful
					// fixture responses without imposing their completion order.
					switch row.TokensIn {
					case 1:
						mainUsage++
					case 2:
						titleUsage++
					default:
						t.Fatalf("native gateway unknown fixture usage vector: %+v", row)
					}
				}
				if count == 2 && mainUsage == 1 && titleUsage == 1 {
					break
				}
				if count > 2 || mainUsage > 1 || titleUsage > 1 || time.Now().After(deadline) {
					t.Fatalf("native gateway usage cardinality: rows=%d main=%d title=%d, want one exact row per leg", count, mainUsage, titleUsage)
				}
				time.Sleep(10 * time.Millisecond)
			}
			proxy.Unbind(token.Token(bearer))
			if proxy.Routes() != 0 {
				t.Fatal("native gateway session route survived explicit Unbind")
			}
		})
	}
	if len(sink.Events()) != 4 {
		t.Fatalf("native gateway terminal usage events=%d, want two exact legs for each of two sessions", len(sink.Events()))
	}
	if !t.Failed() {
		t.Log("actual Codex 0.157.1 seeded PTYs consumed distinct gateway bindings, one title and one visible turn each, through real token routing, pools, Responses HTTP/SSE, IR and exact per-leg usage metering")
	}
}

// Pinned TUI app/thread_title.rs uses the visible custom provider/model for
// its hidden title thread; temporary_structured_request.rs disables tools in
// that isolated thread and submits the title JSON schema. The visible turn
// retains its actual namespace and web-search declarations.
func nativeGatewayRequestLeg(request ir.Request) string {
	if request.Responses == nil {
		return "unrecognized"
	}
	if len(request.Tools) == 0 {
		format, ok := request.Responses.Text["format"].(map[string]any)
		if !ok || format["type"] != "json_schema" {
			return "unrecognized"
		}
		schema, ok := format["schema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			return "unrecognized"
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok || len(properties) != 1 {
			return "unrecognized"
		}
		title, ok := properties["title"].(map[string]any)
		if !ok || title["type"] != "string" {
			return "unrecognized"
		}
		required, ok := schema["required"].([]any)
		if !ok || len(required) != 1 || required[0] != "title" || schema["additionalProperties"] != false {
			return "unrecognized"
		}
		return "title"
	}
	hasNamespace, hasWebSearch := false, false
	for _, tool := range request.Tools {
		hasNamespace = hasNamespace || tool.Kind == "namespace"
		hasWebSearch = hasWebSearch || tool.Kind == "web_search"
	}
	if hasNamespace && hasWebSearch {
		return "main"
	}
	return "unrecognized"
}

func nativeGatewayLegSSE(writer http.ResponseWriter, sessionID, leg string) {
	responseID := "offline-" + sessionID + "-" + leg
	text, inputTokens := "OFFLINE_OK", 1
	if leg == "title" {
		body, _ := json.Marshal(map[string]string{"title": "Verify offline gateway"})
		text, inputTokens = string(body), 2
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": responseID}},
		{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "phase": "final_answer", "id": responseID + "-message", "content": []map[string]any{{"type": "output_text", "text": text}}}},
		{"type": "response.completed", "response": map[string]any{"id": responseID, "usage": map[string]any{"input_tokens": inputTokens, "input_tokens_details": nil, "output_tokens": 1, "output_tokens_details": nil, "total_tokens": inputTokens + 1}}},
	} {
		body, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event["type"], body)
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func runNativeGatewayEndpointTurn(t *testing.T, binary string, spec agent.Spec, secrets ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	before := nativeEndpointChildren(t)
	privateHome := ""
	handle, err := SpawnInteractive(ctx, Options{CodexBin: binary, configTempDir: t.TempDir(),
		interactiveBeforePTYSpawn: func(_ context.Context, _ string, _ agent.Spec, _ interactiveLaunch, home string) error {
			privateHome = home
			return nil
		}}, spec)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = handle.Stop(ctx)
		}
	})
	interactive, ok := handle.(agent.InteractiveCapable)
	if !ok {
		t.Fatal("native gateway caller returned no interactive surface")
	}
	session := interactive.InteractiveSession()
	subscription, err := session.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscription.Close() })
	owned := []sessionshim.ProcessIdentity{}
	for pid := range nativeEndpointChildren(t) {
		if !before[pid] {
			identity, err := sessionshim.ProcessIdentityFor(pid)
			if err != nil {
				t.Fatal(err)
			}
			owned = append(owned, identity)
		}
	}
	if len(owned) == 0 {
		t.Fatal("native gateway child identity was not observed")
	}
	var output strings.Builder
	pending := ""
	for !strings.Contains(output.String(), "OFFLINE_OK") {
		select {
		case frame, open := <-subscription.Frames():
			if !open || frame.Type == attachwire.TypeExit {
				t.Fatal("native gateway consumer exited before rendering the offline response")
			}
			if frame.Type != attachwire.TypeOutput {
				continue
			}
			output.Write(frame.Payload)
			pending += string(frame.Payload)
			for _, query := range []struct{ request, response string }{
				{"\x1b[6n", "\x1b[1;1R"}, {"\x1b[?u", "\x1b[?0u"}, {"\x1b[c", "\x1b[?1;2c"},
				{"\x1b]10;?\x1b\\", "\x1b]10;rgb:ffff/ffff/ffff\x1b\\"}, {"\x1b]11;?\x1b\\", "\x1b]11;rgb:0000/0000/0000\x1b\\"},
			} {
				if strings.Contains(pending, query.request) {
					if _, err := session.WriteInput([]byte(query.response)); err != nil {
						t.Fatal(err)
					}
					pending = strings.ReplaceAll(pending, query.request, "")
				}
			}
			if len(pending) > 256 {
				pending = pending[len(pending)-256:]
			}
		case <-ctx.Done():
			t.Fatalf("native gateway consumer deadline: output bytes=%d", output.Len())
		}
	}
	for _, secret := range secrets {
		if strings.Contains(output.String(), secret) {
			t.Fatal("native gateway credential appeared in PTY output")
		}
	}
	select {
	case <-session.Done():
		t.Fatal("native gateway consumer did not stay interactive after its seeded turn")
	case <-time.After(100 * time.Millisecond):
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := handle.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	stopped = true
	for _, identity := range owned {
		alive, err := identity.Alive()
		if err != nil || alive {
			t.Fatalf("owned native gateway child survived Stop: pid=%d alive=%v error=%v", identity.PID, alive, err)
		}
	}
	if hasResumableSessionState(privateHome) {
		if _, err := os.Stat(filepath.Join(privateHome, codexPluginCacheSubdir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("native gateway retained home kept reclaimable cache: %v", err)
		}
		body, err := os.ReadFile(filepath.Join(privateHome, codexConfigFileName))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(body), secret) {
				t.Fatal("native gateway credential appeared in retained resume config")
			}
		}
	} else if _, err := os.Stat(privateHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-resumable native gateway home survived Stop: %v", err)
	}
}
