package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const (
	endpointHelperEnv     = "DONMAI_OPENCODE_ENDPOINT_HELPER"
	endpointHelperRootEnv = "DONMAI_OPENCODE_ENDPOINT_HELPER_ROOT"
)

type endpointRequestObservation struct {
	path         string
	model        string
	authorize    string
	modelArg     string
	configPath   string
	configMode   string
	configAPIKey string
	leaked       string
}

// TestEndpointBoundLaneA_LMStudioConformance exercises an LM Studio-shaped
// OpenAI-compatible server through the generic local endpoint identity. The
// adapter must use only the exact binding, for both supported auth mechanisms.
func TestEndpointBoundLaneA_LMStudioConformance(t *testing.T) {
	for _, key := range []string{
		EnvEndpoint, EnvAPIKey, "OPENAI_API_KEY", "OPENAI_COMPAT_API_KEY",
		"OPENAI_BASE_URL", "OPENAI_MODEL", "OPENCODE_MODEL", "API_KEY",
	} {
		t.Setenv(key, "poison-"+key)
	}

	binary := writeEndpointHelperCLI(t)
	configRoot := t.TempDir()
	t.Setenv("TMPDIR", configRoot)
	t.Setenv("DONMAI_OPENCODE_HELPER_BINARY", os.Args[0])
	t.Setenv(endpointHelperEnv, "1")
	t.Setenv(endpointHelperRootEnv, configRoot)

	var configPaths []string
	tests := []struct {
		name          string
		mechanism     agent.AuthMechanism
		auth          agent.AuthMode
		bindingEnv    map[string]string
		wantAuthorize string
		wantConfigKey string
	}{
		{
			name:          "none",
			mechanism:     agent.AuthNone,
			auth:          agent.AuthLocal,
			bindingEnv:    map[string]string{"SESSION_BOUND_KEY": "must-not-forward"},
			wantConfigKey: "",
		},
		{
			name:          "api_key",
			mechanism:     agent.AuthAPIKey,
			auth:          agent.AuthBYOK,
			bindingEnv:    map[string]string{"SESSION_BOUND_KEY": "exact-session-key"},
			wantAuthorize: "Bearer exact-session-key",
			wantConfigKey: "{env:" + OCKeyEnvVar + "}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observed := make(chan endpointRequestObservation, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				observed <- endpointRequestObservation{
					path:         r.URL.Path,
					model:        body.Model,
					authorize:    r.Header.Get("Authorization"),
					modelArg:     r.Header.Get("X-OpenCode-Model-Arg"),
					configPath:   r.Header.Get("X-OpenCode-Config-Path"),
					configMode:   r.Header.Get("X-OpenCode-Config-Mode"),
					configAPIKey: r.Header.Get("X-OpenCode-Config-API-Key"),
					leaked:       r.Header.Get("X-Endpoint-Control-Leak"),
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer server.Close()

			p := &Provider{binary: binary}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			h, err := p.Spawn(ctx, agent.Spec{
				Prompt: "reply exactly once",
				Env: map[string]string{
					EnvEndpoint:       "http://spec-poison.invalid/v1",
					EnvAPIKey:         "spec-poison-key",
					"OPENAI_BASE_URL": "http://spec-openai-poison.invalid/v1",
					"OPENAI_MODEL":    "spec-poison-model",
					"SAFE_SESSION":    "kept",
				},
				Endpoint: &agent.EndpointBinding{
					Company:   agent.CompanyLocal,
					Model:     "local-model-q4",
					BaseURL:   server.URL + "/v1",
					Protocol:  agent.ProtoOpenAIChat,
					Host:      agent.HostLocal,
					Mechanism: tt.mechanism,
					Auth:      tt.auth,
					Env:       tt.bindingEnv,
				},
			})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			_ = collectUntilResult(ctx, t, h)

			var got endpointRequestObservation
			select {
			case got = <-observed:
			case <-ctx.Done():
				t.Fatalf("LM Studio-shaped request not observed: %v", ctx.Err())
			}
			if got.path != "/v1/chat/completions" || got.model != "local-model-q4" {
				t.Errorf("route = %s model=%q, want /v1/chat/completions model=local-model-q4", got.path, got.model)
			}
			if got.authorize != tt.wantAuthorize {
				t.Errorf("Authorization = %q, want %q", got.authorize, tt.wantAuthorize)
			}
			if got.modelArg != OCProviderID+"/local-model-q4" {
				t.Errorf("--model = %q, want exact bound provider/model", got.modelArg)
			}
			if got.configMode != "0600" {
				t.Errorf("config mode = %q, want 0600", got.configMode)
			}
			if got.configAPIKey != tt.wantConfigKey {
				t.Errorf("config apiKey = %q, want %q", got.configAPIKey, tt.wantConfigKey)
			}
			if got.leaked != "" {
				t.Errorf("ambient or binding endpoint control leaked to child: %s", got.leaked)
			}
			if got.configPath == "" {
				t.Fatal("endpoint-bound Lane A did not receive OPENCODE_CONFIG")
			}
			configPaths = append(configPaths, got.configPath)

			if err := h.Stop(context.Background()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if _, err := os.Stat(got.configPath); !os.IsNotExist(err) {
				t.Errorf("handle did not clean session config %q: %v", got.configPath, err)
			}
		})
	}

	if len(configPaths) != 2 || configPaths[0] == configPaths[1] {
		t.Fatalf("session config paths are not unique: %v", configPaths)
	}
}

func TestEndpointBoundLaneA_DeniesBeforeSpawn(t *testing.T) {
	binary := writeEndpointHelperCLI(t)
	valid := agent.EndpointBinding{
		Company:   agent.CompanyLocal,
		Model:     "model",
		BaseURL:   "http://127.0.0.1:1234/v1",
		Protocol:  agent.ProtoOpenAIChat,
		Host:      agent.HostLocal,
		Mechanism: agent.AuthNone,
		Auth:      agent.AuthLocal,
	}
	tests := []struct {
		name   string
		mutate func(*agent.EndpointBinding)
	}{
		{name: "unknown mechanism", mutate: func(ep *agent.EndpointBinding) { ep.Mechanism = "future" }},
		{name: "missing mechanism", mutate: func(ep *agent.EndpointBinding) { ep.Mechanism = "" }},
		{name: "wrong protocol", mutate: func(ep *agent.EndpointBinding) { ep.Protocol = agent.ProtoOpenAIResponses }},
		{name: "missing model", mutate: func(ep *agent.EndpointBinding) { ep.Model = "" }},
		{name: "inexact model", mutate: func(ep *agent.EndpointBinding) { ep.Model = " model" }},
		{name: "missing base URL", mutate: func(ep *agent.EndpointBinding) { ep.BaseURL = "" }},
		{name: "relative base URL", mutate: func(ep *agent.EndpointBinding) { ep.BaseURL = "/v1" }},
		{name: "non HTTP base URL", mutate: func(ep *agent.EndpointBinding) { ep.BaseURL = "file:///tmp/socket" }},
		{name: "credential in base URL", mutate: func(ep *agent.EndpointBinding) { ep.BaseURL = "http://user:secret@127.0.0.1/v1" }},
		{name: "api key missing bound key", mutate: func(ep *agent.EndpointBinding) { ep.Mechanism = agent.AuthAPIKey }},
		{name: "api key ambiguous bound keys", mutate: func(ep *agent.EndpointBinding) {
			ep.Mechanism = agent.AuthAPIKey
			ep.Env = map[string]string{"KEY_A": "a", "KEY_B": "b"}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := valid
			tt.mutate(&ep)
			var spawned atomic.Int32
			h, err := (&Provider{binary: binary}).Spawn(t.Context(), agent.Spec{
				Endpoint:         &ep,
				OnProcessSpawned: func(int) { spawned.Add(1) },
			})
			if err == nil || h != nil {
				t.Fatalf("Spawn = (%v, %v), want nil handle and denial", h, err)
			}
			if !strings.Contains(err.Error(), agent.ErrSpawnFailed.Error()) {
				t.Fatalf("error = %v, want ErrSpawnFailed", err)
			}
			if spawned.Load() != 0 {
				t.Fatalf("process spawned before denial")
			}
		})
	}
}

func writeEndpointHelperCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\nexec \"$DONMAI_OPENCODE_HELPER_BINARY\" -test.run '^TestEndpointBoundCLIHelperProcess$' -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write helper CLI: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test fixture must be owner-executable
		t.Fatalf("chmod helper CLI: %v", err)
	}
	return path
}

func TestEndpointBoundCLIHelperProcess(t *testing.T) {
	if os.Getenv(endpointHelperEnv) != "1" {
		return
	}
	fail := func(format string, args ...any) {
		_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
		os.Exit(2)
	}
	configPath := os.Getenv(OCConfigEnvVar)
	root := os.Getenv(endpointHelperRootEnv)
	rel, err := filepath.Rel(root, configPath)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		fail("config path %q is outside helper root %q", configPath, root)
	}
	rootFS := os.DirFS(root)
	data, err := fs.ReadFile(rootFS, rel)
	if err != nil {
		fail("read config: %v", err)
	}
	var cfg ocConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		fail("decode config: %v", err)
	}
	provider := cfg.Provider[OCProviderID]
	model := strings.TrimPrefix(cfg.Model, OCProviderID+"/")
	payload, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "fixture"}},
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, strings.TrimRight(provider.Options.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		fail("build request: %v", err)
	}
	if provider.Options.APIKey != "" {
		keyName := strings.TrimSuffix(strings.TrimPrefix(provider.Options.APIKey, "{env:"), "}")
		if key := os.Getenv(keyName); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	info, err := fs.Stat(rootFS, rel)
	if err != nil {
		fail("stat config: %v", err)
	}
	req.Header.Set("X-OpenCode-Config-Path", configPath)
	req.Header.Set("X-OpenCode-Config-Mode", fmt.Sprintf("%04o", info.Mode().Perm()))
	req.Header.Set("X-OpenCode-Config-API-Key", provider.Options.APIKey)
	for i, arg := range os.Args {
		if arg == "--model" && i+1 < len(os.Args) {
			req.Header.Set("X-OpenCode-Model-Arg", os.Args[i+1])
		}
	}
	var leaked []string
	for _, key := range []string{
		EnvEndpoint, EnvAPIKey, OCKeyEnvVar, "SESSION_BOUND_KEY", "OPENAI_API_KEY",
		"OPENAI_COMPAT_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENCODE_MODEL", "API_KEY",
	} {
		if key == OCKeyEnvVar && provider.Options.APIKey != "" {
			continue
		}
		if value, ok := os.LookupEnv(key); ok && value != "" {
			leaked = append(leaked, key+"="+value)
		}
	}
	req.Header.Set("X-Endpoint-Control-Leak", strings.Join(leaked, ","))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail("request status: %s", resp.Status)
	}
	_, _ = fmt.Fprintln(os.Stdout, `{"type":"step_start","sessionID":"ses_endpoint","part":{"type":"step-start"}}`)
	_, _ = fmt.Fprintln(os.Stdout, `{"type":"step_finish","sessionID":"ses_endpoint","part":{"type":"step-finish","reason":"stop","tokens":{"input":1,"output":1},"cost":0}}`)
	os.Exit(0)
}
