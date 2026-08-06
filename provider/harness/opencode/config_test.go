package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestBuildConfig_ProviderPinAndLockout(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		Endpoint: &agent.EndpointBinding{
			Company:   agent.CompanyOpenAI,
			Model:     "gpt-4o-mini",
			BaseURL:   "https://compat.example/v1",
			Mechanism: agent.AuthAPIKey,
		},
	}
	cfg := buildConfig(spec)

	prov, ok := cfg.Provider[OCProviderID]
	if !ok {
		t.Fatalf("config: want a %q provider entry", OCProviderID)
	}
	if prov.NPM != "@ai-sdk/openai-compatible" {
		t.Errorf("provider npm = %q, want @ai-sdk/openai-compatible", prov.NPM)
	}
	if prov.Options.BaseURL != "https://compat.example/v1" {
		t.Errorf("baseURL = %q, want the endpoint's", prov.Options.BaseURL)
	}
	// §5.1: the key value never enters the JSON — only the {env:...} indirection.
	if prov.Options.APIKey != "{env:"+OCKeyEnvVar+"}" {
		t.Errorf("apiKey = %q, want {env:%s}", prov.Options.APIKey, OCKeyEnvVar)
	}
	if _, ok := prov.Models["gpt-4o-mini"]; !ok {
		t.Errorf("models = %v, want gpt-4o-mini", prov.Models)
	}
	if cfg.Model != OCProviderID+"/gpt-4o-mini" {
		t.Errorf("model = %q, want %s/gpt-4o-mini", cfg.Model, OCProviderID)
	}
	// Lockout: enabled_providers + deny-all-but-donmai provider.use policy.
	if len(cfg.EnabledProviders) != 1 || cfg.EnabledProviders[0] != OCProviderID {
		t.Errorf("enabled_providers = %v, want [%s]", cfg.EnabledProviders, OCProviderID)
	}
	if cfg.Experimental == nil || len(cfg.Experimental.Policies) != 2 {
		t.Fatalf("experimental.policies: want deny-all + allow-donmai, got %+v", cfg.Experimental)
	}
	var sawAllowDonmai, sawDenyAll bool
	for _, pol := range cfg.Experimental.Policies {
		if pol.Action != "provider.use" {
			t.Errorf("policy action = %q, want provider.use", pol.Action)
		}
		if pol.Effect == "allow" && pol.Resource == OCProviderID {
			sawAllowDonmai = true
		}
		if pol.Effect == "deny" && pol.Resource == "*" {
			sawDenyAll = true
		}
	}
	if !sawAllowDonmai || !sawDenyAll {
		t.Errorf("policies missing lockout pair: %+v", cfg.Experimental.Policies)
	}
}

func TestBuildConfig_KeyNeverInlined(t *testing.T) {
	t.Parallel()
	// Even when a spec carries a credential value in env, the rendered config
	// must not inline it — the value rides the process env, never disk.
	spec := agent.Spec{
		Endpoint: &agent.EndpointBinding{Company: agent.CompanyOpenAI, Model: "m", BaseURL: "http://x/v1", Mechanism: agent.AuthAPIKey},
		Env:      map[string]string{OCKeyEnvVar: "sk-super-secret-value"},
	}
	data, err := json.Marshal(buildConfig(spec))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "sk-super-secret-value") {
		t.Fatalf("config JSON leaked the credential value: %s", data)
	}
}

func TestProjectPermissions_ClaudeGrammar(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		AllowedTools:    []string{"Read", "Edit", "Bash(npm:*)"},
		DisallowedTools: []string{"WebFetch", "Bash(git push:*)"},
		PermissionConfig: &agent.PermissionConfig{
			DefaultDecision: "deny",
		},
	}
	perm := projectPermissions(spec)

	if perm["read"] != "allow" {
		t.Errorf("read = %v, want allow", perm["read"])
	}
	if perm["edit"] != "allow" {
		t.Errorf("edit = %v, want allow", perm["edit"])
	}
	if perm["webfetch"] != "deny" {
		t.Errorf("webfetch = %v, want deny", perm["webfetch"])
	}
	// Safety guards always route to the pump.
	if perm["external_directory"] != "ask" || perm["doom_loop"] != "ask" {
		t.Errorf("safety guards not pinned to ask: ext=%v doom=%v", perm["external_directory"], perm["doom_loop"])
	}
	bash, ok := perm["bash"].(map[string]string)
	if !ok {
		t.Fatalf("bash = %T, want map[string]string", perm["bash"])
	}
	if bash["npm*"] != "allow" {
		t.Errorf("bash[npm*] = %q, want allow", bash["npm*"])
	}
	if bash["git push*"] != "deny" {
		t.Errorf("bash[git push*] = %q, want deny", bash["git push*"])
	}
	// Structured regex policy is enforced by the permission pump, so unknown
	// commands must reach it instead of being decided by the static map.
	if bash["*"] != "ask" {
		t.Errorf("bash[*] = %q, want ask (permission-pump boundary)", bash["*"])
	}
}

func TestProjectPermissions_DenyWinsOverAllow(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		AllowedTools:    []string{"Edit"},
		DisallowedTools: []string{"Edit"},
	}
	perm := projectPermissions(spec)
	if perm["edit"] != "deny" {
		t.Errorf("edit = %v, want deny (deny wins over allow)", perm["edit"])
	}
}

func TestProjectPermissions_MCPToolNamesUseOpenCodeKeys(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		MCPToolNames: []string{"mcp__af-code-intelligence__af_code_search"},
		MCPServers:   []agent.MCPServerConfig{{Name: "af-code-intelligence", Command: "donmai"}},
	}
	perm := projectPermissions(spec)
	if perm["*"] != "deny" {
		t.Fatalf("default permission = %v, want deny for an explicit allowlist", perm["*"])
	}
	if got := perm["af-code-intelligence_af_code_search"]; got != "allow" {
		t.Errorf("MCP tool permission = %v, want allow", got)
	}
	if _, broad := perm["af-code-intelligence_*"]; broad {
		t.Error("exact MCP tool allowlist unexpectedly admitted the whole server")
	}
}

func TestProjectMCP_LocalAndPlatformRemote(t *testing.T) {
	t.Parallel()
	// No MCP servers → no mcp key at all (§5.3).
	if got := projectMCP(agent.Spec{}); got != nil {
		t.Errorf("projectMCP(empty) = %v, want nil", got)
	}
	// Both code-intel/card stdio servers and the platform A2A HTTP gate are
	// represented in the session-scoped project config.
	spec := agent.Spec{MCPServers: []agent.MCPServerConfig{
		{Name: "code", Command: "donmai", Args: []string{"mcp", "code"}, Env: map[string]string{"X": "1"}},
		{Name: "platform", Type: "http", URL: "https://platform.example/api/mcp/session", Headers: map[string]string{"Authorization": "Bearer session"}},
		{Name: "bad"}, // no command
	}}
	mcp := projectMCP(spec)
	if len(mcp) != 2 {
		t.Fatalf("mcp = %v, want local code-intel and remote platform entries", mcp)
	}
	code, ok := mcp["code"]
	if !ok {
		t.Fatalf("mcp missing 'code' entry: %v", mcp)
	}
	if code.Type != "local" || len(code.Command) != 3 || code.Command[0] != "donmai" {
		t.Errorf("mcp code entry malformed: %+v", code)
	}
	if !code.Enabled || code.Environment["X"] != "1" {
		t.Errorf("mcp code entry env/enabled wrong: %+v", code)
	}
	remote, ok := mcp["platform"]
	if !ok {
		t.Fatalf("mcp missing platform remote entry: %v", mcp)
	}
	if remote.Type != "remote" || remote.URL != "https://platform.example/api/mcp/session" || remote.Headers["Authorization"] != "Bearer session" || remote.OAuth == nil || *remote.OAuth {
		t.Errorf("mcp remote entry malformed: %+v", remote)
	}
}

func TestWriteConfig_Perms0600(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	spec := agent.Spec{Endpoint: &agent.EndpointBinding{Company: agent.CompanyOpenAI, Model: "m", BaseURL: "http://x/v1", Mechanism: agent.AuthAPIKey}}
	path, err := writeConfig(dir, spec)
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %o, want 0600", perm)
	}
	if filepath.Base(path) != "opencode.json" {
		t.Errorf("config file name = %q, want opencode.json", filepath.Base(path))
	}
	// It must be valid JSON.
	data, _ := os.ReadFile(path)
	var cfg ocConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("written config is not valid JSON: %v", err)
	}
}
