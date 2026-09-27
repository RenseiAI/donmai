package pi

import (
	"os"
	"path/filepath"
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
