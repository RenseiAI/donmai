package pi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestHandshakeSHA_Verification is the cryptographic core of smoke 1
// (tampered-extension fixture): only the exact embedded-extension SHA
// verifies; a wrong or empty SHA fails closed.
func TestHandshakeSHA_Verification(t *testing.T) {
	t.Parallel()
	realSHA := extensionSHA()
	if realSHA == "" || len(realSHA) != 64 {
		t.Fatalf("extensionSHA() = %q, want a 64-char hex sha256", realSHA)
	}
	if !verifyHandshakeSHA(realSHA) {
		t.Errorf("verifyHandshakeSHA(realSHA) = false, want true")
	}
	for _, bad := range []string{"", "deadbeef", strings.Repeat("0", 64), realSHA[:63] + "f"} {
		if verifyHandshakeSHA(bad) {
			t.Errorf("verifyHandshakeSHA(%q) = true, want false (must fail closed)", bad)
		}
	}
}

// TestHandshakeToken_Verification pins the per-session token half of the
// handshake: only the exact token verifies, and an empty token/claim fails
// closed (a foreign extension cannot forge the token the harness set in env).
func TestHandshakeToken_Verification(t *testing.T) {
	t.Parallel()
	const want = "session-token-xyz"
	if !verifyHandshakeToken(want, want) {
		t.Errorf("verifyHandshakeToken(want, want) = false, want true")
	}
	for _, claimed := range []string{"", "wrong", want + "x"} {
		if verifyHandshakeToken(claimed, want) {
			t.Errorf("verifyHandshakeToken(%q, want) = true, want false (fail closed)", claimed)
		}
	}
	// An empty expected token never verifies — a session with no token set
	// cannot be spoofed into "matching".
	if verifyHandshakeToken(want, "") {
		t.Errorf("verifyHandshakeToken(want, \"\") = true, want false")
	}
}

// TestMaterializeExtension writes the boundary payload for `-e` loading with
// the right mode and content: the extension file is 0600 and byte-identical to
// the embedded source (so its self-hash matches the embedded SHA — a
// materialized copy that drifted would fail the handshake).
func TestMaterializeExtension(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	layout, err := materializeExtension(cwd)
	if err != nil {
		t.Fatalf("materializeExtension: %v", err)
	}

	data, err := os.ReadFile(layout.extension)
	if err != nil {
		t.Fatalf("read extension: %v", err)
	}
	if string(data) != string(extensionSource()) {
		t.Errorf("materialized extension differs from embedded source")
	}
	assertMode0600(t, layout.extension)

	// The extension lives at the state-dir ROOT (not under an auto-discovered
	// extensions/ subdir), so the `-e --no-extensions` load is the only load.
	if filepath.Dir(layout.extension) != layout.root {
		t.Errorf("extension at %q, want it directly under the state root %q", layout.extension, layout.root)
	}
	if base := filepath.Base(filepath.Dir(layout.extension)); base != piStateDir {
		t.Errorf("state root basename = %q, want %q", base, piStateDir)
	}
}

// TestProviderPinEnv pins design §6: the routing pin rides ENV (baseUrl / api /
// model) so the extension can register the "donmai" provider — and the API
// key is NOT among these vars (it rides PiKeyEnvVar via applyEndpoint), so no
// secret is placed on disk or in the pin env.
func TestProviderPinEnv(t *testing.T) {
	t.Parallel()
	ep := &agent.EndpointBinding{
		Company:  agent.CompanyAnthropic,
		Model:    "claude-x",
		BaseURL:  "https://api.anthropic.com",
		Protocol: agent.ProtoAnthropicMessages,
		Host:     agent.HostDirect,
		Env:      map[string]string{"ANTHROPIC_API_KEY": "sk-super-secret-canary"},
	}
	env := providerPinEnv(agent.Spec{Endpoint: ep, Model: "claude-x"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "sk-super-secret-canary") {
		t.Errorf("provider-pin env leaked the API key: %q", joined)
	}
	if !containsEnv(env, piBaseURLEnvVar, "https://api.anthropic.com") {
		t.Errorf("pin env missing base URL: %v", env)
	}
	if !containsEnv(env, piAPIEnvVar, "anthropic-messages") {
		t.Errorf("pin env missing/incorrect api name: %v", env)
	}
	if !containsEnv(env, piModelEnvVar, "claude-x") {
		t.Errorf("pin env missing model: %v", env)
	}
}

// TestProviderPinEnv_StripsBuiltinProviderPrefix is the regression proof at
// the injected-provider registration layer: when the resolved
// model names one of pi's built-in providers, the env this function exports
// for the extension's `pi.registerProvider("donmai", …)` call carries the
// BARE model id — never the "<provider>/" prefix, which is pi's own --model
// selector syntax, not a wire model code the upstream API understands. This
// applies REGARDLESS of whether modelPinArgs ultimately selects the
// injected provider or routes natively — the injected provider stays
// registered either way, and must never register a code the upstream API
// will reject with a 400 (the exact z.ai "modelCode: does not exist" this
// bug report observed).
func TestProviderPinEnv_StripsBuiltinProviderPrefix(t *testing.T) {
	t.Parallel()
	ep := &agent.EndpointBinding{
		Company:  agent.CompanyOpenAI,
		Model:    "zai/glm-5.3",
		BaseURL:  "https://api.z.ai/api/coding/paas/v4",
		Protocol: agent.ProtoOpenAIChat,
		Host:     agent.HostDirect,
		Env:      map[string]string{"OPENAI_API_KEY": "sk-super-secret-canary"},
	}
	env := providerPinEnv(agent.Spec{Endpoint: ep, Model: "zai/glm-5.3"})
	if !containsEnv(env, piModelEnvVar, "glm-5.3") {
		t.Errorf("pin env model must be bare (\"glm-5.3\"), not the prefixed pin: %v", env)
	}
	for _, e := range env {
		if strings.Contains(e, "zai/glm-5.3") {
			t.Errorf("pin env still carries the prefixed model string %q: %v", "zai/glm-5.3", env)
		}
	}
}

// TestProviderPinEnv_UnprefixedModelUnchanged is requirement 3's proof at
// this same layer: a model with no recognized builtin-provider prefix (an
// unprefixed pin, or an aggregator's own "vendor/model"-shaped catalog slug)
// rides providerPinEnv completely unchanged, byte-for-byte.
func TestProviderPinEnv_UnprefixedModelUnchanged(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"claude-opus-4-8", "gpt-5.4", "agg-vendor/claude-3-haiku"} {
		env := providerPinEnv(agent.Spec{Model: model})
		if !containsEnv(env, piModelEnvVar, model) {
			t.Errorf("providerPinEnv(%q) changed the model id; env: %v", model, env)
		}
	}
}

// TestProviderPinEnvContextWindow pins the context-window half of the pin:
// a positive ProviderConfig["contextWindow"] (whatever numeric type the JSON
// decode produced) rides piContextWindowEnvVar to the child extension, and a
// missing/zero/invalid value leaves the var UNSET so the extension keeps its
// built-in default — the pin never invents a value the dispatch did not carry.
func TestProviderPinEnvContextWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pc   map[string]any
		want string // "" => piContextWindowEnvVar must be absent
	}{
		{name: "int value exported", pc: map[string]any{"contextWindow": 1_000_000}, want: "1000000"},
		{name: "float64 from JSON decode exported", pc: map[string]any{"contextWindow": float64(1_000_000)}, want: "1000000"},
		{name: "absent config leaves env unset", pc: nil, want: ""},
		{name: "zero leaves env unset", pc: map[string]any{"contextWindow": 0}, want: ""},
		{name: "negative leaves env unset", pc: map[string]any{"contextWindow": -1}, want: ""},
		{name: "non-numeric leaves env unset", pc: map[string]any{"contextWindow": "1000000"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := providerPinEnv(agent.Spec{Model: "claude-x", ProviderConfig: tc.pc})
			if tc.want != "" {
				if !containsEnv(env, piContextWindowEnvVar, tc.want) {
					t.Errorf("pin env missing %s=%s: %v", piContextWindowEnvVar, tc.want, env)
				}
				return
			}
			for _, e := range env {
				if strings.HasPrefix(e, piContextWindowEnvVar+"=") {
					t.Errorf("pin env must not carry %s for config %v: %v", piContextWindowEnvVar, tc.pc, env)
				}
			}
		})
	}
}

// TestExtensionReadsContextWindowEnv pins the Go↔extension env contract by
// name: the embedded policy extension's source must read the exact variable
// providerPinEnv exports (there is no TS test harness for the embedded
// extension — this is the Go-side contract check), and the old hardcoded
// descriptor value must not survive as a literal.
func TestExtensionReadsContextWindowEnv(t *testing.T) {
	t.Parallel()
	src := string(extensionSource())
	if !strings.Contains(src, piContextWindowEnvVar) {
		t.Errorf("embedded extension does not read %s", piContextWindowEnvVar)
	}
	if strings.Contains(src, "contextWindow: 200000") {
		t.Errorf("embedded extension still hardcodes the descriptor contextWindow")
	}
}

// TestProviderPinEnvMaxTokens pins the output-limit half of the pin: a
// positive ProviderConfig["maxOutputTokens"] (whatever numeric type the JSON
// decode produced) rides piOutputLimitEnvVar to the child extension, and a
// missing/zero/invalid value leaves the var UNSET — the harness never invents
// an output limit the dispatch did not carry.
func TestProviderPinEnvMaxTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pc   map[string]any
		want string // "" => piOutputLimitEnvVar must be absent
	}{
		{name: "int value exported", pc: map[string]any{"maxOutputTokens": 64000}, want: "64000"},
		{name: "float64 from JSON decode exported", pc: map[string]any{"maxOutputTokens": float64(128000)}, want: "128000"},
		{name: "int64 exported", pc: map[string]any{"maxOutputTokens": int64(32000)}, want: "32000"},
		{name: "absent config leaves env unset", pc: nil, want: ""},
		{name: "context window alone leaves env unset", pc: map[string]any{"contextWindow": 1_000_000}, want: ""},
		{name: "zero leaves env unset", pc: map[string]any{"maxOutputTokens": 0}, want: ""},
		{name: "negative leaves env unset", pc: map[string]any{"maxOutputTokens": -1}, want: ""},
		{name: "non-numeric leaves env unset", pc: map[string]any{"maxOutputTokens": "64000"}, want: ""},
		{name: "fractional leaves env unset, never truncated", pc: map[string]any{"maxOutputTokens": 1.9}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := providerPinEnv(agent.Spec{Model: "claude-x", ProviderConfig: tc.pc})
			if tc.want != "" {
				if !containsEnv(env, piOutputLimitEnvVar, tc.want) {
					t.Errorf("pin env missing %s=%s: %v", piOutputLimitEnvVar, tc.want, env)
				}
				return
			}
			for _, e := range env {
				if strings.HasPrefix(e, piOutputLimitEnvVar+"=") {
					t.Errorf("pin env must not carry %s for config %v: %v", piOutputLimitEnvVar, tc.pc, env)
				}
			}
		})
	}
}

// TestExtensionRegistersOutputLimitOnlyWhenConfigured activates the REAL
// embedded extension (testdata harness, no pi binary) and reads back the
// model it registers for the "donmai" provider. A configured output limit is
// registered as the model's maxTokens; with none configured (or an invalid
// value) the model carries NO maxTokens at all, so pi requests no output cap
// and the serving endpoint's own limit applies. The removed fixed cap must
// not come back as a literal either.
func TestExtensionRegistersOutputLimitOnlyWhenConfigured(t *testing.T) {
	t.Parallel()
	if src := string(extensionSource()); strings.Contains(src, "maxTokens: 16384") {
		t.Fatalf("embedded extension still hardcodes the output limit")
	}
	cases := []struct {
		name     string
		maxToken string
		want     float64 // 0 => maxTokens must be absent
	}{
		{name: "configured limit is registered", maxToken: "64000", want: 64000},
		{name: "unset registers no limit", maxToken: ""},
		{name: "zero registers no limit", maxToken: "0"},
		{name: "negative registers no limit", maxToken: "-5"},
		{name: "fractional registers no limit", maxToken: "1.5"},
		{name: "non-numeric registers no limit", maxToken: "lots"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := runExtensionFixture(t, []string{
				piBaseURLEnvVar + "=http://127.0.0.1:9/v1",
				piModelEnvVar + "=served-model",
				piContextWindowEnvVar + "=262144",
				// Always set, even when empty, so an inherited value in the
				// test process's own environment can never leak in.
				piOutputLimitEnvVar + "=" + tc.maxToken,
			}, "read", `{"path":"README.md"}`)
			model := registeredDonmaiModel(t, out)
			if cw, _ := model["contextWindow"].(float64); cw != 262144 {
				t.Errorf("registered contextWindow = %v, want 262144", model["contextWindow"])
			}
			got, present := model["maxTokens"]
			if tc.want == 0 {
				if present {
					t.Errorf("registered maxTokens = %v, want the key absent", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("registered maxTokens = %v (present=%t), want %v", got, present, tc.want)
			}
		})
	}
}

// TestExtensionRegistersSessionKeyFromCredentialFile activates the REAL
// embedded extension with the session key delivered ONLY through the
// credential file (no DONMAI_PI_KEY in env): the registered "donmai"
// provider must carry the file's key. A second run with an empty file
// (keyless session) registers an empty key, never an inherited one.
// RED proof: read the key from process.env instead of the file and the
// first run registers the inherited value (or empty) instead of the file's.
func TestExtensionRegistersSessionKeyFromCredentialFile(t *testing.T) {
	t.Parallel()
	nodeAvailable(t)
	dir := t.TempDir()
	credPath := filepath.Join(dir, "session-credentials.json")
	payload := `{"schemaVersion":1,"credentials":[{"env":"DONMAI_PI_KEY","value":"file-rail-key"}]}`
	if err := os.WriteFile(credPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runExtensionFixture(t, []string{
		piBaseURLEnvVar + "=http://127.0.0.1:9/v1",
		piModelEnvVar + "=served-model",
		credentialFileEnvVar + "=" + credPath,
	}, "read", `{"path":"README.md"}`)
	model := registeredDonmaiModel(t, out)
	providers, _ := out["providers"].([]any)
	config, _ := providers[0].(map[string]any)["config"].(map[string]any)
	if key, _ := config["apiKey"].(string); key != "file-rail-key" {
		t.Errorf("registered apiKey = %q, want the credential file's key", key)
	}
	_ = model

	// Keyless session: no credential file at all — the provider registers
	// with an empty key rather than any inherited value.
	out2 := runExtensionFixture(t, []string{
		piBaseURLEnvVar + "=http://127.0.0.1:9/v1",
		piModelEnvVar + "=served-model",
	}, "read", `{"path":"README.md"}`)
	providers2, _ := out2["providers"].([]any)
	config2, _ := providers2[0].(map[string]any)["config"].(map[string]any)
	if key, _ := config2["apiKey"].(string); key != "" {
		t.Errorf("keyless registered apiKey = %q, want empty", key)
	}
}

// TestExtensionRestoresSessionEnvironmentFromCredentialFile activates the
// REAL embedded extension with a credential file whose environment section
// carries the session's deferred bindings (child_env.go): after activation
// they are in the process environment the tools pi starts inherit — while
// the exec environment never carried them. A name the exec environment
// already defines keeps its exec value, the extension's and pi's own
// control namespaces are never written from the file, a malformed name is
// skipped, and the credentials section is never restored (the provider pin
// reads the one key it needs directly).
//
// RED proof: make restoreSessionEnvironment a no-op and the restored-binding
// assertion fails; drop its namespace or defined-name guards and the
// steering assertions fail.
func TestExtensionRestoresSessionEnvironmentFromCredentialFile(t *testing.T) {
	t.Parallel()
	nodeAvailable(t)
	dir := t.TempDir()
	credPath := filepath.Join(dir, "session-credentials.json")
	payload, err := json.Marshal(sessionCredentialFile{
		SchemaVersion: 1,
		Credentials: []sessionCredential{
			{Env: PiKeyEnvVar, Value: "file-rail-key"},
			{Env: "SENTINEL_MODEL_KEY_RESTORE", Value: "credential-must-not-restore"},
		},
		Environment: []sessionCredential{
			{Env: "SENTINEL_TOOL_TOKEN", Value: "restored-tool-token"},
			{Env: "SENTINEL_EXEC_DEFINED", Value: "file-must-not-override"},
			{Env: piBaseURLEnvVar, Value: "http://file-must-not-steer.invalid/v1"},
			{Env: "PI_FUTURE_CONFIG", Value: "file-must-not-steer"},
			{Env: "SENTINEL_BAD=NAME", Value: "malformed-must-not-restore"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	out := runExtensionFixture(t, []string{
		piBaseURLEnvVar + "=http://127.0.0.1:9/v1",
		piModelEnvVar + "=served-model",
		credentialFileEnvVar + "=" + credPath,
		"SENTINEL_EXEC_DEFINED=exec-value",
		"HARNESS_REPORT_ENV=SENTINEL_TOOL_TOKEN,SENTINEL_EXEC_DEFINED," + piBaseURLEnvVar + ",PI_FUTURE_CONFIG,SENTINEL_BAD,SENTINEL_MODEL_KEY_RESTORE," + PiKeyEnvVar,
	}, "read", `{"path":"README.md"}`)
	env, _ := out["environment"].(map[string]any)
	if env == nil {
		t.Fatalf("fixture reported no environment: %v", out)
	}
	want := map[string]any{
		"SENTINEL_TOOL_TOKEN":        "restored-tool-token",
		"SENTINEL_EXEC_DEFINED":      "exec-value",
		piBaseURLEnvVar:              "http://127.0.0.1:9/v1",
		"PI_FUTURE_CONFIG":           nil,
		"SENTINEL_BAD":               nil,
		"SENTINEL_MODEL_KEY_RESTORE": nil,
		PiKeyEnvVar:                  nil,
	}
	for name, wantValue := range want {
		if got := env[name]; got != wantValue {
			t.Errorf("after activation %s = %v, want %v", name, got, wantValue)
		}
	}
	providers, _ := out["providers"].([]any)
	if len(providers) == 0 {
		t.Fatalf("extension registered no provider: %v", out)
	}
	config, _ := providers[0].(map[string]any)["config"].(map[string]any)
	if key, _ := config["apiKey"].(string); key != "file-rail-key" {
		t.Errorf("registered apiKey = %q, want the credential file's key", key)
	}
	if baseURL, _ := config["baseUrl"].(string); baseURL != "http://127.0.0.1:9/v1" {
		t.Errorf("registered baseUrl = %q, want the exec environment's pin", baseURL)
	}
}

// injectedCell is an endpoint binding the injected provider serves (a
// loopback gateway speaking protocol), with the output limit, when positive,
// on the spec's provider config.
func injectedCell(protocol agent.WireProtocol, maxOut any) agent.Spec {
	spec := agent.Spec{
		Prompt: "hi",
		Model:  "claude-x",
		Endpoint: &agent.EndpointBinding{
			Company:  agent.CompanyAnthropic,
			BaseURL:  "http://127.0.0.1:4000/v1",
			Host:     agent.HostGateway,
			Protocol: protocol,
			Model:    "claude-x",
			Env:      map[string]string{"ANTHROPIC_API_KEY": "k-test"},
		},
	}
	if maxOut != nil {
		spec.ProviderConfig = map[string]any{"maxOutputTokens": maxOut}
	}
	return spec
}

// TestRequireOutputLimit pins which cells are refused for want of a
// configured output limit: exactly those the injected provider serves over a
// protocol that cannot omit it (anthropic-messages, google-generative-ai)
// with no positive, whole maxOutputTokens. A configured limit, an
// OpenAI-style protocol, or a model pi serves natively (its own catalog
// carries the limit) is never refused, and nothing supplies a default.
func TestRequireOutputLimit(t *testing.T) {
	t.Parallel()
	native := injectedCell(agent.ProtoAnthropicMessages, nil)
	native.Endpoint = nil
	native.Model = "anthropic/claude-x"
	cases := []struct {
		name    string
		spec    agent.Spec
		wantAPI string // "" => not refused
	}{
		{name: "anthropic without a limit is refused", spec: injectedCell(agent.ProtoAnthropicMessages, nil), wantAPI: "anthropic-messages"},
		{name: "gemini without a limit is refused", spec: injectedCell(agent.ProtoGeminiGenerate, nil), wantAPI: "google-generative-ai"},
		{name: "anthropic with a zero limit is refused", spec: injectedCell(agent.ProtoAnthropicMessages, 0), wantAPI: "anthropic-messages"},
		{name: "anthropic with a fractional limit is refused", spec: injectedCell(agent.ProtoAnthropicMessages, 1.5), wantAPI: "anthropic-messages"},
		{name: "anthropic with a non-numeric limit is refused", spec: injectedCell(agent.ProtoAnthropicMessages, "64000"), wantAPI: "anthropic-messages"},
		{name: "anthropic with a configured limit is admitted", spec: injectedCell(agent.ProtoAnthropicMessages, float64(64000))},
		{name: "gemini with a configured limit is admitted", spec: injectedCell(agent.ProtoGeminiGenerate, 64000)},
		{name: "openai chat without a limit is admitted", spec: injectedCell(agent.ProtoOpenAIChat, nil)},
		{name: "openai responses without a limit is admitted", spec: injectedCell(agent.ProtoOpenAIResponses, nil)},
		{name: "a model pi serves natively is admitted", spec: native},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := requireOutputLimit(tc.spec)
			if tc.wantAPI == "" {
				if err != nil {
					t.Fatalf("requireOutputLimit = %v, want admitted", err)
				}
				return
			}
			var refused *OutputLimitRequiredError
			if !errors.As(err, &refused) {
				t.Fatalf("requireOutputLimit = %v, want *OutputLimitRequiredError", err)
			}
			if refused.API != tc.wantAPI || !strings.Contains(err.Error(), "maxOutputTokens") {
				t.Errorf("refusal = %+v (%q), want API %s and a message naming maxOutputTokens", refused, err, tc.wantAPI)
			}
		})
	}
}

// TestSpawn_RefusesMissingOutputLimitBeforeAnyChild drives the refusal
// through Provider.Spawn in both spawn modes: the session fails with a typed
// configuration error wrapped as a spawn failure, and not one command reaches
// the child. The control spawns the same cell with a configured limit.
func TestSpawn_RefusesMissingOutputLimitBeforeAnyChild(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{false, true} {
		name := "headless"
		if interactive {
			name = "interactive"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec := injectedCell(agent.ProtoAnthropicMessages, nil)
			if interactive {
				spec.Interactive = &agent.InteractiveSpec{}
			}
			cmds, _, err := spawnScripted(t, spec, "", "")
			var refused *OutputLimitRequiredError
			if !errors.Is(err, agent.ErrSpawnFailed) || !errors.As(err, &refused) {
				t.Fatalf("Spawn = %v, want agent.ErrSpawnFailed wrapping *OutputLimitRequiredError", err)
			}
			if got := cmds.commands(); len(got) != 0 {
				t.Errorf("commands reached the child before the refusal: %v", got)
			}
		})
	}
	t.Run("configured limit spawns", func(t *testing.T) {
		t.Parallel()
		_, h, err := spawnScripted(t, injectedCell(agent.ProtoAnthropicMessages, float64(64000)),
			handshakeEvent("h1"), getStateResponse("ses_limit")+event(map[string]any{"type": "agent_settled"}))
		if err != nil {
			t.Fatalf("Spawn with a configured limit: %v", err)
		}
		drain(t, h)
	})
}

// registeredDonmaiModel returns the single model the extension registered
// under the "donmai" provider in a fixture report.
func registeredDonmaiModel(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	providers, _ := out["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("registered providers = %v, want exactly one", out["providers"])
	}
	p, _ := providers[0].(map[string]any)
	if name, _ := p["name"].(string); name != pinnedProviderName {
		t.Fatalf("registered provider name = %v, want %q", p["name"], pinnedProviderName)
	}
	config, _ := p["config"].(map[string]any)
	models, _ := config["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("registered models = %v, want exactly one", config["models"])
	}
	model, _ := models[0].(map[string]any)
	if id, _ := model["id"].(string); id != "served-model" {
		t.Fatalf("registered model id = %v, want served-model", model["id"])
	}
	return model
}

func containsEnv(env []string, key, val string) bool {
	for _, e := range env {
		if e == key+"="+val {
			return true
		}
	}
	return false
}

func assertMode0600(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s mode = %o, want 600", filepath.Base(path), perm)
	}
}

// TestProviderPinEnvThinkingLevel pins the thinking-level half of the pin: the
// configured effort rides piThinkingLevelEnvVar under its own name, and with
// none configured the var is still exported, empty, so an inherited value can
// never stand in for configuration.
func TestProviderPinEnvThinkingLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		effort agent.EffortLevel
		want   string
	}{
		{"", ""},
		{agent.EffortMedium, "medium"},
		{agent.EffortXHigh, "xhigh"},
		{agent.EffortMax, "max"},
	}
	for _, tc := range cases {
		env := providerPinEnv(agent.Spec{Model: "claude-x", Effort: tc.effort})
		if !containsEnv(env, piThinkingLevelEnvVar, tc.want) {
			t.Errorf("effort %q: pin env missing %s=%s: %v", tc.effort, piThinkingLevelEnvVar, tc.want, env)
		}
	}
}

// TestExtensionRegistersConfiguredThinkingLevel activates the REAL embedded
// extension and reads back the model it registers. pi clamps xhigh and max to
// high unless the model's thinkingLevelMap lists them, so a session pinned to
// either must register it, mapped to the provider's own value of the same
// name. On the Anthropic Messages protocol those tiers exist only as the
// adaptive-thinking effort, so the model is marked adaptive for them. Lower
// levels register no map. With no configured effort nothing is invented: on
// Chat Completions the model is registered without reasoning-effort support,
// so pi omits the parameter instead of sending its own default level.
func TestExtensionRegistersConfiguredThinkingLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		level      string
		api        string
		wantMap    map[string]any // nil => thinkingLevelMap must be absent
		wantCompat map[string]any // nil => compat must be absent
	}{
		{name: "max on chat completions", level: "max", api: "openai-completions", wantMap: map[string]any{"max": "max"}},
		{name: "xhigh on chat completions", level: "xhigh", api: "openai-completions", wantMap: map[string]any{"xhigh": "xhigh"}},
		{name: "max on responses", level: "max", api: "openai-responses", wantMap: map[string]any{"max": "max"}},
		{name: "max on anthropic messages is adaptive", level: "max", api: "anthropic-messages", wantMap: map[string]any{"max": "max"}, wantCompat: map[string]any{"forceAdaptiveThinking": true}},
		{name: "high needs no map", level: "high", api: "openai-completions"},
		{name: "high on anthropic stays budget-based", level: "high", api: "anthropic-messages"},
		{name: "not configured on chat completions sends no effort", level: "", api: "openai-completions", wantCompat: map[string]any{"supportsReasoningEffort": false}},
		{name: "not configured on anthropic registers nothing", level: "", api: "anthropic-messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := runExtensionFixture(t, []string{
				piBaseURLEnvVar + "=http://127.0.0.1:9/v1",
				piAPIEnvVar + "=" + tc.api,
				piModelEnvVar + "=served-model",
				piOutputLimitEnvVar + "=64000",
				piThinkingLevelEnvVar + "=" + tc.level,
			}, "read", `{"path":"README.md"}`)
			model := registeredDonmaiModel(t, out)
			got, present := model["thinkingLevelMap"]
			if tc.wantMap == nil {
				if present {
					t.Errorf("registered thinkingLevelMap = %v, want the key absent", got)
				}
			} else if !reflect.DeepEqual(got, tc.wantMap) {
				t.Errorf("registered thinkingLevelMap = %v (present=%t), want %v", got, present, tc.wantMap)
			}
			gotCompat, compatPresent := model["compat"]
			if tc.wantCompat == nil {
				if compatPresent {
					t.Errorf("registered compat = %v, want the key absent", gotCompat)
				}
			} else if !reflect.DeepEqual(gotCompat, tc.wantCompat) {
				t.Errorf("registered compat = %v (present=%t), want %v", gotCompat, compatPresent, tc.wantCompat)
			}
		})
	}
}
