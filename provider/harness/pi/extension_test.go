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

// flipLastHexDigit returns sha with its final hex digit replaced by a
// different one, so the result is always a wrong SHA even when sha itself
// ends in "f" (naively appending "f" would be a no-op there and the
// "wrong" claim would verify).
func flipLastHexDigit(sha string) string {
	last := sha[len(sha)-1]
	replacement := byte('f')
	if last == 'f' {
		replacement = 'e'
	}
	return sha[:len(sha)-1] + string(replacement)
}

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
	for _, bad := range []string{"", "deadbeef", strings.Repeat("0", 64), flipLastHexDigit(realSHA)} {
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
// missing/zero/invalid value leaves the var UNSET so the extension registers
// no window — the pin never invents a value the dispatch did not carry.
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

// TestProviderPinEnvGatewayLane pins the lane mark the extension reads
// because it cannot see the host itself: a gateway binding exports "1",
// every other shape exports an explicit clearing entry (present but empty)
// so an inherited value can never survive as a stale lane mark.
// RED proof: export the mark only for gateway (omit otherwise) and the
// clearing rows fail; stop exporting it for gateway and the gateway row fails.
func TestProviderPinEnvGatewayLane(t *testing.T) {
	t.Parallel()
	gateway := gatewayCell(agent.ProtoOpenAIChat, nil, 1_000_000)
	if got := providerPinEnv(gateway); !containsEnv(got, piGatewayEnvVar, "1") {
		t.Errorf("gateway pin env missing %s=1: %v", piGatewayEnvVar, got)
	}
	direct := gatewayCell(agent.ProtoOpenAIChat, nil, 1_000_000)
	direct.Endpoint.Host = agent.HostDirect
	if got := providerPinEnv(direct); !containsEnv(got, piGatewayEnvVar, "") {
		t.Errorf("direct pin env must carry an explicit clearing entry for %s: %v", piGatewayEnvVar, got)
	}
	if got := providerPinEnv(agent.Spec{Model: "claude-x"}); !containsEnv(got, piGatewayEnvVar, "") {
		t.Errorf("unbound pin env must carry an explicit clearing entry for %s: %v", piGatewayEnvVar, got)
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
		piContextWindowEnvVar + "=262144",
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
		piContextWindowEnvVar + "=262144",
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
		piContextWindowEnvVar + "=262144",
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

// gatewayCell builds an endpoint binding the injected provider serves over
// a loopback gateway speaking protocol, with the output limit (when
// non-nil) and the context window (when non-nil) on the spec's provider
// config. A nil window models a dispatch that carried no window: provider
// config entries are only ever ADDED by the test (the reconciliation never
// injects a default), so omitting the key is exactly the missing-value
// shape.
func gatewayCell(protocol agent.WireProtocol, maxOut, contextWindow any) agent.Spec {
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
	pc := map[string]any{}
	if maxOut != nil {
		pc["maxOutputTokens"] = maxOut
	}
	if contextWindow != nil {
		pc["contextWindow"] = contextWindow
	}
	if len(pc) > 0 {
		spec.ProviderConfig = pc
	}
	return spec
}

// injectedCell is an endpoint binding the injected provider serves (a
// loopback gateway speaking protocol), with the output limit, when positive,
// on the spec's provider config.
func injectedCell(protocol agent.WireProtocol, maxOut any) agent.Spec {
	return gatewayCell(protocol, maxOut, 1_000_000)
}

// TestRequireContextWindow pins which gateway cells are refused for want of
// a configured context window: exactly those the injected provider serves
// over a gateway binding with no positive, whole ProviderConfig
// ["contextWindow"]. A configured window, a non-gateway injected route
// (pi's catalog carries no window there either, but the serving endpoint's
// own limit applies without a translating gateway in the middle), or a
// model pi serves natively (its own catalog carries the window) is never
// refused, and nothing supplies a default.
func TestRequireContextWindow(t *testing.T) {
	t.Parallel()
	nonGateway := gatewayCell(agent.ProtoOpenAIChat, nil, nil)
	nonGateway.Endpoint.Host = agent.HostDirect
	nonGateway.Endpoint.BaseURL = "https://api.openai.com/v1"
	// A gateway binding with no BaseURL selects no injected provider
	// (injectedProviderSelected is false), so it is admitted even without
	// a window: pi resolves the provider from its own config.
	unboundGateway := gatewayCell(agent.ProtoOpenAIChat, nil, nil)
	unboundGateway.Endpoint.BaseURL = ""
	native := gatewayCell(agent.ProtoAnthropicMessages, nil, nil)
	native.Endpoint = nil
	native.Model = "anthropic/claude-x"
	cases := []struct {
		name string
		spec agent.Spec
		want bool // true => refused with *ContextWindowRequiredError
	}{
		{name: "gateway without a window is refused", spec: gatewayCell(agent.ProtoOpenAIChat, nil, nil), want: true},
		{name: "gateway with a zero window is refused", spec: gatewayCell(agent.ProtoOpenAIChat, nil, 0), want: true},
		{name: "gateway with a negative window is refused", spec: gatewayCell(agent.ProtoOpenAIChat, nil, -1), want: true},
		{name: "gateway with a fractional window is refused", spec: gatewayCell(agent.ProtoOpenAIChat, nil, 1.5), want: true},
		{name: "gateway with a non-numeric window is refused", spec: gatewayCell(agent.ProtoOpenAIChat, nil, "1000000"), want: true},
		{name: "gateway with a configured window is admitted", spec: gatewayCell(agent.ProtoOpenAIChat, nil, 1_000_000)},
		{name: "gateway with a float64 window from JSON decode is admitted", spec: gatewayCell(agent.ProtoOpenAIChat, nil, float64(1_000_000))},
		{name: "gateway with output limit but no window is refused", spec: gatewayCell(agent.ProtoAnthropicMessages, float64(64000), nil), want: true},
		{name: "gateway with both limits is admitted", spec: gatewayCell(agent.ProtoAnthropicMessages, float64(64000), 1_000_000)},
		{name: "non-gateway injected route without a window is admitted", spec: nonGateway},
		{name: "gateway binding with no BaseURL selects no injected provider and is admitted", spec: unboundGateway},
		{name: "a model pi serves natively is admitted", spec: native},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := requireContextWindow(tc.spec)
			if !tc.want {
				if err != nil {
					t.Fatalf("requireContextWindow = %v, want admitted", err)
				}
				return
			}
			var refused *ContextWindowRequiredError
			if !errors.As(err, &refused) {
				t.Fatalf("requireContextWindow = %v, want *ContextWindowRequiredError", err)
			}
			if !strings.Contains(err.Error(), "contextWindow") {
				t.Errorf("refusal %q does not name contextWindow", err)
			}
		})
	}
}

// TestSpawn_RefusesMissingContextWindowBeforeAnyChild drives the refusal
// through Provider.Spawn in both spawn modes: the session fails with a typed
// configuration error wrapped as a spawn failure, and not one command reaches
// the child. The control spawns the same gateway cell with a configured
// window. RED proof: restore the extension's 200000 fallback (or drop the
// requireContextWindow call in prepare) and the refusal cases below go red —
// the spawn succeeds on the guessed window instead of failing closed.
func TestSpawn_RefusesMissingContextWindowBeforeAnyChild(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{false, true} {
		name := "headless"
		if interactive {
			name = "interactive"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec := gatewayCell(agent.ProtoOpenAIChat, nil, nil)
			if interactive {
				spec.Interactive = &agent.InteractiveSpec{}
			}
			cmds, _, err := spawnScripted(t, spec, "", "")
			var refused *ContextWindowRequiredError
			if !errors.Is(err, agent.ErrSpawnFailed) || !errors.As(err, &refused) {
				t.Fatalf("Spawn = %v, want agent.ErrSpawnFailed wrapping *ContextWindowRequiredError", err)
			}
			if got := cmds.commands(); len(got) != 0 {
				t.Errorf("commands reached the child before the refusal: %v", got)
			}
		})
	}
	t.Run("configured window spawns", func(t *testing.T) {
		t.Parallel()
		_, h, err := spawnScripted(t, gatewayCell(agent.ProtoOpenAIChat, nil, 1_000_000),
			handshakeEvent("h1"), getStateResponse("ses_window")+event(map[string]any{"type": "agent_settled"}))
		if err != nil {
			t.Fatalf("Spawn with a configured window: %v", err)
		}
		drain(t, h)
	})
	t.Run("non-gateway injected route without a window spawns", func(t *testing.T) {
		t.Parallel()
		// The refusal keys on the gateway lane alone: a direct injected
		// cell with no window is admitted at spawn (the extension registers
		// it with no window, and the serving endpoint's own limit applies).
		// RED proof: drop the HostGateway gate in requireContextWindow and
		// this spawn is refused.
		spec := gatewayCell(agent.ProtoOpenAIChat, nil, nil)
		spec.Endpoint.Host = agent.HostDirect
		spec.Endpoint.BaseURL = "https://api.openai.com/v1"
		_, h, err := spawnScripted(t, spec,
			handshakeEvent("h1"), getStateResponse("ses_direct")+event(map[string]any{"type": "agent_settled"}))
		if err != nil {
			t.Fatalf("Spawn of a non-gateway injected route without a window: %v", err)
		}
		drain(t, h)
	})
}

// TestExtensionRefusesMissingContextWindow activates the REAL embedded
// extension without the context-window pin on the gateway lane: no provider
// is registered (there is deliberately no default), and the refusal is
// surfaced as a session-start notification naming the missing window — never
// a silent run on a guessed 200000. Off the gateway lane the same missing
// value registers with no window, and the serving endpoint's own limit
// applies. The control registers the configured window. RED proof: restore
// the `contextWindowEnv : 200000` fallback in the extension and the refusal
// case below goes red — a provider registers with the guessed window.
func TestExtensionRefusesMissingContextWindow(t *testing.T) {
	t.Parallel()
	// The test process itself may run with DONMAI_PI_* vars set (a session
	// worker's own environment carries the live session's pin). Every var
	// the extension reads for registration is set explicitly here — empty
	// when the case means "absent" — so an inherited value can never leak
	// in through cmd.Env layering (runExtensionFixture appends to
	// os.Environ), exactly as TestExtensionRegistersOutputLimitOnlyWhenConfigured
	// does for the output-limit pin.
	base := []string{
		piBaseURLEnvVar + "=http://127.0.0.1:9/v1",
		piModelEnvVar + "=served-model",
		piAPIEnvVar + "=openai-completions",
		// The gateway lane mark selects which rule the extension applies:
		// it cannot see the host itself. "1" refuses a missing window;
		// empty registers with no window. Always set explicitly so an
		// inherited value can never leak in through cmd.Env layering.
		piGatewayEnvVar + "=1",
		piThinkingLevelEnvVar + "=",
		piOutputLimitEnvVar + "=",
		piPricesBoundEnvVar + "=",
		piPriceInputEnvVar + "=",
		piPriceOutputEnvVar + "=",
		piPriceCacheReadEnvVar + "=",
		piPriceCacheWriteEnvVar + "=",
		// The handshake token selects the RPC lane (verified boundary);
		// clearing it runs the fixture in the interactive lane, where the
		// missing-window refusal surfaces as a session-start notification.
		// Without this a worker that itself runs under a token would leak
		// it in through os.Environ layering and flip the lane.
		piHandshakeEnvVar + "=",
		credentialFileEnvVar + "=",
		piAllowedToolsEnvVar + "=",
		piDisallowedToolsEnvVar + "=",
		piStateDirEnvVar + "=",
	}
	t.Run("missing window registers no provider, blocks tools, and notifies", func(t *testing.T) {
		t.Parallel()
		// Always set, even when empty, so an inherited value in the
		// test process's own environment can never leak in.
		out := runExtensionFixture(t, append(append([]string(nil), base...), piContextWindowEnvVar+"="), "read", `{"path":"README.md"}`)
		if providers, _ := out["providers"].([]any); len(providers) != 0 {
			t.Errorf("registered providers = %v, want none without a context window", out["providers"])
		}
		// No silent routing downgrade: the tool_call the fixture offered
		// is refused with the missing-window reason, not run against
		// whatever provider pi would resolve on its own.
		verdict, _ := out["verdict"].(map[string]any)
		if block, _ := verdict["block"].(bool); verdict == nil || !block {
			t.Errorf("missing-window tool_call verdict = %v, want a block", out["verdict"])
		}
		if reason, _ := verdict["reason"].(string); !strings.Contains(reason, "context window") {
			t.Errorf("missing-window refusal %q does not name the context window", reason)
		}
		notifications, _ := out["notifications"].([]any)
		found := false
		for _, n := range notifications {
			if msg, _ := n.(map[string]any)["message"].(string); strings.Contains(msg, "context window") {
				found = true
			}
		}
		if !found {
			t.Errorf("no session-start notification names the missing context window: %v", out)
		}
	})
	t.Run("invalid window registers no provider", func(t *testing.T) {
		t.Parallel()
		for _, value := range []string{"", "0", "-5", "1.5", "lots"} {
			out := runExtensionFixture(t, append(append([]string(nil), base...), piContextWindowEnvVar+"="+value),
				"read", `{"path":"README.md"}`)
			if providers, _ := out["providers"].([]any); len(providers) != 0 {
				t.Errorf("value %q: registered providers = %v, want none", value, out["providers"])
			}
		}
	})
	t.Run("mixed-case tool spelling is still refused", func(t *testing.T) {
		t.Parallel()
		// The refusal gate normalizes the tool name before matching
		// GUARDED_TOOLS, exactly like the interactive lane's own gate:
		// a mixed-case spelling must block, never bypass the gate and run
		// against whatever provider pi resolves on its own.
		out := runExtensionFixture(t, append(append([]string(nil), base...), piContextWindowEnvVar+"="), "Read", `{"path":"README.md"}`)
		if providers, _ := out["providers"].([]any); len(providers) != 0 {
			t.Errorf("registered providers = %v, want none without a context window", out["providers"])
		}
		verdict, _ := out["verdict"].(map[string]any)
		if block, _ := verdict["block"].(bool); verdict == nil || !block {
			t.Errorf("mixed-case tool_call verdict = %v, want a block", out["verdict"])
		}
		if reason, _ := verdict["reason"].(string); !strings.Contains(reason, "context window") {
			t.Errorf("mixed-case refusal %q does not name the context window", reason)
		}
	})
	t.Run("configured window registers", func(t *testing.T) {
		t.Parallel()
		out := runExtensionFixture(t, append(append([]string(nil), base...), piContextWindowEnvVar+"=262144", piOutputLimitEnvVar+"=64000"),
			"read", `{"path":"README.md"}`)
		model := registeredDonmaiModel(t, out)
		if cw, _ := model["contextWindow"].(float64); cw != 262144 {
			t.Errorf("registered contextWindow = %v, want 262144", model["contextWindow"])
		}
	})
	t.Run("non-gateway lane without a window registers with no window", func(t *testing.T) {
		t.Parallel()
		// The same missing value off the gateway lane must NOT refuse:
		// the session stays on the injected provider against the bound
		// endpoint, with no window pinned, and the serving endpoint's own
		// limit applies. RED proof: key the extension refusal on the window
		// alone (drop the gateway-lane gate) and this registers no provider.
		nonGateway := make([]string, 0, len(base)+1)
		for _, e := range base {
			if strings.HasPrefix(e, piGatewayEnvVar+"=") {
				nonGateway = append(nonGateway, piGatewayEnvVar+"=")
				continue
			}
			nonGateway = append(nonGateway, e)
		}
		out := runExtensionFixture(t, append(nonGateway, piContextWindowEnvVar+"="), "read", `{"path":"README.md"}`)
		model := registeredDonmaiModel(t, out)
		if _, present := model["contextWindow"]; present {
			t.Errorf("registered contextWindow = %v, want the key absent off the gateway lane", model["contextWindow"])
		}
		// No refusing gate: the offered tool must not be blocked with the
		// missing-window reason.
		if verdict, _ := out["verdict"].(map[string]any); verdict != nil {
			if reason, _ := verdict["reason"].(string); strings.Contains(reason, "context window") {
				t.Errorf("non-gateway tool_call verdict = %v, want no missing-window refusal", verdict)
			}
		}
	})
	t.Run("rpc lane records the refusal as the call outcome", func(t *testing.T) {
		t.Parallel()
		// RPC mode (handshake token set): the missing-window gate records
		// its refusal via registerRefusal before blocking, so the Go side
		// records it as the call's outcome instead of meeting the end event
		// with no record. RED proof: drop the registerRefusal call and no
		// refusal reaches the recorder.
		rpc := make([]string, 0, len(base)+1)
		for _, e := range base {
			if strings.HasPrefix(e, piHandshakeEnvVar+"=") {
				rpc = append(rpc, piHandshakeEnvVar+"=test-token")
				continue
			}
			rpc = append(rpc, e)
		}
		out := runExtensionFixture(t, append(rpc, piContextWindowEnvVar+"="), "read", `{"path":"README.md"}`)
		if providers, _ := out["providers"].([]any); len(providers) != 0 {
			t.Errorf("registered providers = %v, want none without a context window on the gateway lane", out["providers"])
		}
		verdict, _ := out["verdict"].(map[string]any)
		if block, _ := verdict["block"].(bool); verdict == nil || !block {
			t.Fatalf("rpc missing-window tool_call verdict = %v, want a block", out["verdict"])
		}
		refusals, _ := out["refusals"].([]any)
		found := false
		for _, r := range refusals {
			if payload, _ := r.(map[string]any)["payload"].(string); strings.Contains(payload, "context window") {
				found = true
			}
		}
		if !found {
			t.Errorf("no registered refusal names the missing context window: %v", out["refusals"])
		}
	})
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
				piContextWindowEnvVar + "=262144",
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

// routingCell builds an openai-chat cell (the default gateway binding
// protocol) carrying pref, so the routing-preference pin tests share one
// shape.
func routingCell(pref *agent.RoutingPreference) agent.Spec {
	return agent.Spec{
		Prompt: "hi",
		Model:  "served-model",
		Endpoint: &agent.EndpointBinding{
			Company:           agent.CompanyOpenAI,
			Model:             "served-model",
			BaseURL:           "http://127.0.0.1:4000/v1",
			Protocol:          agent.ProtoOpenAIChat,
			Host:              agent.HostGateway,
			RoutingPreference: pref,
			Env:               map[string]string{"ANTHROPIC_API_KEY": "k-test"},
		},
	}
}

// TestProviderPinEnvRoutingPreference pins the routing-preference half of
// the pin: a set preference on an openai-completions cell rides the two
// routing env vars as JSON-encoded string arrays; an unset preference, a
// non-chat protocol, or an unusable slug list exports clearing entries
// (each key present but empty) so an inherited value can never survive as
// a stale routing option.
func TestProviderPinEnvRoutingPreference(t *testing.T) {
	t.Parallel()
	set := &agent.RoutingPreference{Order: []string{"anthropic", "openai"}, Only: []string{"anthropic"}}
	cases := []struct {
		name  string
		spec  agent.Spec
		want  map[string]string // exact expected routing env entries
		clear bool              // both keys must be present but empty
	}{
		{
			name: "chat binding with a preference sets pi's routing option",
			spec: routingCell(set),
			want: map[string]string{
				piRoutingOrderEnvVar: `["anthropic","openai"]`,
				piRoutingOnlyEnvVar:  `["anthropic"]`,
			},
		},
		{
			name: "only-only preference exports an empty order array alongside it",
			spec: routingCell(&agent.RoutingPreference{Only: []string{"anthropic"}}),
			want: map[string]string{
				piRoutingOrderEnvVar: `[]`,
				piRoutingOnlyEnvVar:  `["anthropic"]`,
			},
		},
		{
			name:  "no binding clears",
			spec:  agent.Spec{Model: "claude-x"},
			clear: true,
		},
		{
			name:  "nil preference clears",
			spec:  routingCell(nil),
			clear: true,
		},
		{
			name:  "empty preference clears, never exports empty meaning",
			spec:  routingCell(&agent.RoutingPreference{}),
			clear: true,
		},
		{
			name: "anthropic-messages binding gets nothing",
			spec: func() agent.Spec {
				spec := routingCell(set)
				spec.Endpoint.Protocol = agent.ProtoAnthropicMessages
				return spec
			}(),
			clear: true,
		},
		{
			name: "responses binding gets nothing",
			spec: func() agent.Spec {
				spec := routingCell(set)
				spec.Endpoint.Protocol = agent.ProtoOpenAIResponses
				return spec
			}(),
			clear: true,
		},
		{
			name:  "blank slug fails closed to clearing",
			spec:  routingCell(&agent.RoutingPreference{Order: []string{"anthropic", "  "}}),
			clear: true,
		},
		{
			name:  "control characters in a slug fail closed to clearing",
			spec:  routingCell(&agent.RoutingPreference{Only: []string{"anthropic\nX-Injected: 1"}}),
			clear: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := providerPinEnv(tc.spec)
			got := map[string]string{}
			for _, e := range env {
				for _, key := range []string{piRoutingOrderEnvVar, piRoutingOnlyEnvVar} {
					if strings.HasPrefix(e, key+"=") {
						got[key] = strings.TrimPrefix(e, key+"=")
					}
				}
			}
			if tc.clear {
				for _, key := range []string{piRoutingOrderEnvVar, piRoutingOnlyEnvVar} {
					v, present := got[key]
					if !present {
						t.Errorf("pin env must carry an explicit clearing entry for %s: %v", key, env)
					} else if v != "" {
						t.Errorf("%s = %q, want empty (clearing): %v", key, v, env)
					}
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("routing pin env = %v, want %v (full env: %v)", got, tc.want, env)
			}
		})
	}
}

// TestExtensionRegistersRoutingPreferenceOnlyOnChatCompletions activates
// the REAL embedded extension and reads back the model it registers for
// the "donmai" provider. A chat cell with a set preference registers the
// routing option on the model's compat; an anthropic-messages cell — whose
// protocol has no routing option — registers no compat routing keys even
// when the binding carries a preference (the Go pin already clears it;
// this proves the extension honors the protocol gate too). An unset
// preference registers no routing keys on any protocol.
//
// RED proof: drop the api gate in the extension and the
// anthropic-messages case registers the option; stop spreading
// modelCompat and the chat case loses the thinking-level compat entry.
func TestExtensionRegistersRoutingPreferenceOnlyOnChatCompletions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		api       string
		order     string
		only      string
		wantOrder []any
		wantOnly  []any
	}{
		{
			name: "chat cell with a preference registers the routing option",
			api:  "openai-completions", order: `["anthropic","openai"]`, only: `["anthropic"]`,
			wantOrder: []any{"anthropic", "openai"}, wantOnly: []any{"anthropic"},
		},
		{
			name: "chat cell with only-only registers only and no order key",
			api:  "openai-completions", order: `[]`, only: `["anthropic"]`,
			wantOnly: []any{"anthropic"},
		},
		{
			name: "anthropic-messages binding gets nothing",
			api:  "anthropic-messages", order: `["anthropic"]`, only: `["anthropic"]`,
		},
		{
			name: "chat cell without a preference registers nothing",
			api:  "openai-completions", order: "", only: "",
		},
		{
			name: "malformed pin registers nothing",
			api:  "openai-completions", order: `not-json`, only: `["anthropic"]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := runExtensionFixture(t, []string{
				piBaseURLEnvVar + "=http://127.0.0.1:9/v1",
				piAPIEnvVar + "=" + tc.api,
				piModelEnvVar + "=served-model",
				piContextWindowEnvVar + "=262144",
				piOutputLimitEnvVar + "=64000",
				piThinkingLevelEnvVar + "=max",
				piRoutingOrderEnvVar + "=" + tc.order,
				piRoutingOnlyEnvVar + "=" + tc.only,
			}, "read", `{"path":"README.md"}`)
			model := registeredDonmaiModel(t, out)
			compat, _ := model["compat"].(map[string]any)
			if compat == nil {
				compat = map[string]any{}
			}
			for _, key := range []string{"openRouterRouting", "vercelGatewayRouting"} {
				routing, _ := compat[key].(map[string]any)
				if routing == nil {
					routing = map[string]any{}
				}
				var wantOrder, wantOnly []any
				if tc.api == "openai-completions" {
					wantOrder, wantOnly = tc.wantOrder, tc.wantOnly
				}
				if got := routing["order"]; !reflect.DeepEqual(got, wantOrderToAny(wantOrder)) {
					t.Errorf("%s.order = %v, want %v (compat: %v)", key, got, wantOrder, compat)
				}
				if got := routing["only"]; !reflect.DeepEqual(got, wantOnlyToAny(wantOnly)) {
					t.Errorf("%s.only = %v, want %v (compat: %v)", key, got, wantOnly, compat)
				}
			}
			// The thinking-level entry the same activation registers must
			// survive the routing merge: compat layering must not displace
			// it. It lives on the model itself, beside compat — not inside
			// it.
			if tc.api == "openai-completions" {
				if got, _ := model["thinkingLevelMap"].(map[string]any); !reflect.DeepEqual(got, map[string]any{"max": "max"}) {
					t.Errorf("thinkingLevelMap = %v, want the max entry to survive the routing merge (model: %v)", got, model)
				}
			}
		})
	}
}

// wantOrderToAny / wantOnlyToAny normalize a nil expectation to a nil
// interface so DeepEqual distinguishes "key absent" from "key present but
// empty": the JSON fixture decodes an absent key as nil and an explicit
// empty array as an empty slice.
func wantOrderToAny(want []any) any {
	if want == nil {
		return nil
	}
	return want
}

func wantOnlyToAny(want []any) any {
	if want == nil {
		return nil
	}
	return want
}
