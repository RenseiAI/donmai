package runner

import (
	"bytes"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// TestTranslateSpec_PlatformDisallowedTools_Appended verifies that
// platform-supplied DisallowedTools patterns (Option B) are
// appended to the runner's defaultDisallowedTools() baseline rather
// than replacing it. The merge is purely additive — the static floor
// remains intact.
func TestTranslateSpec_PlatformDisallowedTools_Appended(t *testing.T) {
	t.Parallel()
	caps := agent.Capabilities{
		AcceptsAllowedToolsList: true,
	}
	in := SpecInputs{
		Cwd:    "/tmp/wt",
		Prompt: "do something",
	}

	platformPatterns := []string{
		"Read(/Users/**/.env*)",
		"Bash(gh secret*)",
		"WebFetch(http://169.254.169.254/*)",
	}

	qw := QueuedWork{
		QueuedWork: prompt.QueuedWork{
			DisallowedTools: platformPatterns,
		},
	}
	spec := translateSpec(qw, caps, in)

	// The runner's own baseline must still be present.
	baseline := defaultDisallowedTools()
	for _, want := range baseline {
		if !slices.Contains(spec.DisallowedTools, want) {
			t.Errorf("baseline pattern %q missing from spec.DisallowedTools: %v", want, spec.DisallowedTools)
		}
	}

	// Every platform-supplied pattern must appear in the merged list.
	for _, want := range platformPatterns {
		if !slices.Contains(spec.DisallowedTools, want) {
			t.Errorf("platform pattern %q missing from spec.DisallowedTools: %v", want, spec.DisallowedTools)
		}
	}

	// Sanity: combined list must be larger than the baseline alone.
	if len(spec.DisallowedTools) < len(baseline)+len(platformPatterns) {
		t.Errorf("DisallowedTools: want at least %d entries, got %d (%v)",
			len(baseline)+len(platformPatterns), len(spec.DisallowedTools), spec.DisallowedTools)
	}
}

// TestTranslateSpec_EmptyPlatformDisallowedTools_IsNoop verifies that an
// absent or empty platform DisallowedTools field does not affect the
// runner's baseline — the baseline is unchanged.
func TestTranslateSpec_EmptyPlatformDisallowedTools_IsNoop(t *testing.T) {
	t.Parallel()
	caps := agent.Capabilities{
		AcceptsAllowedToolsList: true,
	}
	in := SpecInputs{
		Cwd:    "/tmp/wt",
		Prompt: "do something",
	}

	// Empty slice — simulates a dispatch from a platform version that
	// does not yet stamp DisallowedTools (backward-compatible path).
	qw := QueuedWork{
		QueuedWork: prompt.QueuedWork{
			DisallowedTools: nil,
		},
	}
	spec := translateSpec(qw, caps, in)

	baseline := defaultDisallowedTools()
	if len(spec.DisallowedTools) != len(baseline) {
		t.Errorf("DisallowedTools: want exactly %d baseline entries, got %d (%v)",
			len(baseline), len(spec.DisallowedTools), spec.DisallowedTools)
	}
	for _, want := range baseline {
		if !slices.Contains(spec.DisallowedTools, want) {
			t.Errorf("baseline pattern %q missing from spec.DisallowedTools: %v", want, spec.DisallowedTools)
		}
	}
}

// TestTranslateSpec_InitialContext_GatedByCapability verifies that
// Spec.InitialContext only flows through when the resolved provider
// declares Capabilities.SupportsTurnInputContext. Providers without that
// split receive the same context folded into the system prompt by the
// loop, so the field must be zeroed here to avoid duplication.
func TestTranslateSpec_InitialContext_GatedByCapability(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		supports  bool
		wantValue string
	}{
		{name: "supported-flows-through", supports: true, wantValue: "MEMORY BLOCK"},
		{name: "unsupported-zeroed", supports: false, wantValue: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			caps := agent.Capabilities{SupportsTurnInputContext: tt.supports}
			in := SpecInputs{
				Cwd:            "/tmp/wt",
				Prompt:         "do",
				InitialContext: "MEMORY BLOCK",
			}
			qw := QueuedWork{QueuedWork: prompt.QueuedWork{}}
			spec := translateSpec(qw, caps, in)
			if spec.InitialContext != tt.wantValue {
				t.Fatalf("InitialContext: want %q, got %q", tt.wantValue, spec.InitialContext)
			}
		})
	}
}

// TestTranslateSpec_ToolUse_Honored verifies that AllowedTools and
// MCPServers flow through to the produced agent.Spec when the resolved
// provider declares the matching v2 tool-use accept flags.
func TestTranslateSpec_ToolUse_Honored(t *testing.T) {
	t.Parallel()
	caps := agent.Capabilities{
		SupportsToolPlugins:     true,
		AcceptsAllowedToolsList: true,
		AcceptsMcpServerSpec:    true,
	}
	in := SpecInputs{
		Cwd:    "/tmp/wt",
		Prompt: "do",
		MCPServers: []agent.MCPServerConfig{{
			Name: "af_linear", Command: "pnpm", Args: []string{"af-linear"},
		}},
	}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{}}
	spec := translateSpec(qw, caps, in)
	if len(spec.AllowedTools) == 0 {
		t.Fatal("AllowedTools: expected default list to flow through, got nil")
	}
	if len(spec.MCPServers) != 1 || spec.MCPServers[0].Name != "af_linear" {
		t.Fatalf("MCPServers: want [af_linear], got %+v", spec.MCPServers)
	}
}

// TestTranslateSpec_ToolUse_Stripped_NoMCP verifies that MCPServers is
// silently dropped when the provider does not advertise
// SupportsToolPlugins OR AcceptsMcpServerSpec — either gate trips the
// strip. AllowedTools is independently gated on
// AcceptsAllowedToolsList.
func TestTranslateSpec_ToolUse_Stripped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		caps           agent.Capabilities
		wantAllowed    bool // expect spec.AllowedTools non-nil
		wantMCPServers bool // expect spec.MCPServers non-nil
	}{
		{
			name: "tools-off",
			caps: agent.Capabilities{
				SupportsToolPlugins:     false,
				AcceptsAllowedToolsList: false,
				AcceptsMcpServerSpec:    false,
			},
			wantAllowed:    false,
			wantMCPServers: false,
		},
		{
			name: "tools-supported-but-mcp-shape-not-accepted",
			caps: agent.Capabilities{
				SupportsToolPlugins:     true,
				AcceptsAllowedToolsList: true,
				AcceptsMcpServerSpec:    false,
			},
			wantAllowed:    true,
			wantMCPServers: false,
		},
		{
			name: "mcp-shape-accepted-but-allowed-not",
			caps: agent.Capabilities{
				SupportsToolPlugins:     true,
				AcceptsAllowedToolsList: false,
				AcceptsMcpServerSpec:    true,
			},
			wantAllowed:    false,
			wantMCPServers: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := SpecInputs{
				Cwd:    "/tmp/wt",
				Prompt: "do",
				MCPServers: []agent.MCPServerConfig{{
					Name: "af_linear", Command: "pnpm", Args: []string{"af-linear"},
				}},
			}
			qw := QueuedWork{QueuedWork: prompt.QueuedWork{}}
			spec := translateSpec(qw, tt.caps, in)
			if got := len(spec.AllowedTools) > 0; got != tt.wantAllowed {
				t.Errorf("AllowedTools non-empty: want %v, got %v (%v)", tt.wantAllowed, got, spec.AllowedTools)
			}
			if got := len(spec.MCPServers) > 0; got != tt.wantMCPServers {
				t.Errorf("MCPServers non-empty: want %v, got %v (%v)", tt.wantMCPServers, got, spec.MCPServers)
			}
		})
	}
}

// TestTranslateSpec_CardAllowedTools_ReplacesDefault verifies the WS5 rule:
// when the agent card supplies an explicit AllowedTools list it is
// AUTHORITATIVE and used verbatim in place of the runner's curated default.
// An empty/absent card list falls back to defaultAllowedTools().
func TestTranslateSpec_CardAllowedTools_ReplacesDefault(t *testing.T) {
	t.Parallel()
	caps := agent.Capabilities{AcceptsAllowedToolsList: true}
	in := SpecInputs{Cwd: "/tmp/wt", Prompt: "do"}

	t.Run("card list replaces default verbatim", func(t *testing.T) {
		t.Parallel()
		card := []string{"Bash(cargo:*)", "Read"}
		qw := QueuedWork{QueuedWork: prompt.QueuedWork{AllowedTools: card}}
		spec := translateSpec(qw, caps, in)
		if !slices.Equal(spec.AllowedTools, card) {
			t.Errorf("AllowedTools = %v, want exactly the card list %v", spec.AllowedTools, card)
		}
		// A default-only entry must NOT be present (verbatim replacement).
		if slices.Contains(spec.AllowedTools, "Bash(pnpm:*)") {
			t.Errorf("default entry leaked into card-authoritative list: %v", spec.AllowedTools)
		}
	})

	t.Run("absent card falls back to default", func(t *testing.T) {
		t.Parallel()
		qw := QueuedWork{QueuedWork: prompt.QueuedWork{}}
		spec := translateSpec(qw, caps, in)
		if !slices.Equal(spec.AllowedTools, defaultAllowedTools()) {
			t.Errorf("AllowedTools = %v, want default %v", spec.AllowedTools, defaultAllowedTools())
		}
	})
}

// TestTranslateSpec_Codex_RoutesAllowedToolsToPermissionConfig verifies the
// WS5 codex bridge: codex does not accept a flat allowlist but DOES consume a
// structured PermissionConfig, so the card's AllowedTools route into
// PermissionConfig.AllowPatterns and the disallowed set into
// DisallowPatterns instead of being dropped.
func TestTranslateSpec_Codex_RoutesAllowedToolsToPermissionConfig(t *testing.T) {
	t.Parallel()
	// codex-shaped caps: needs permission config, does not accept a flat list.
	caps := agent.Capabilities{
		NeedsPermissionConfig:   true,
		AcceptsAllowedToolsList: false,
	}
	card := []string{"Bash(cargo:*)", "Edit"}
	platformDisallow := []string{"WebFetch(http://169.254.169.254/*)"}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{
		AllowedTools:    card,
		DisallowedTools: platformDisallow,
	}}
	spec := translateSpec(qw, caps, SpecInputs{Cwd: "/tmp/wt", Prompt: "do"})

	if spec.AllowedTools != nil {
		t.Errorf("AllowedTools must be zeroed for codex (routed to PermissionConfig); got %v", spec.AllowedTools)
	}
	if spec.PermissionConfig == nil {
		t.Fatal("PermissionConfig must be set for codex when the card supplies an allowlist")
	}
	for _, want := range card {
		if !slices.Contains(spec.PermissionConfig.AllowPatterns, want) {
			t.Errorf("AllowPatterns missing %q; got %v", want, spec.PermissionConfig.AllowPatterns)
		}
	}
	// The runner's disallowed floor + the platform disallow must reach DisallowPatterns.
	for _, want := range append(defaultDisallowedTools(), platformDisallow...) {
		if !slices.Contains(spec.PermissionConfig.DisallowPatterns, want) {
			t.Errorf("DisallowPatterns missing %q; got %v", want, spec.PermissionConfig.DisallowPatterns)
		}
	}
}

// TestTranslateSpec_AmpAgyCli_WarnAndDrop verifies that for providers that
// accept neither a flat allowlist NOR a PermissionConfig (amp / agy-cli), the
// card's AllowedTools are dropped — but the previously-silent zero is now a
// structured WARN naming the dropped field + provider.
func TestTranslateSpec_AmpAgyCli_WarnAndDrop(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"amp", "agy-cli"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			caps := agent.Capabilities{
				NeedsPermissionConfig:   false,
				AcceptsAllowedToolsList: false,
			}
			qw := QueuedWork{QueuedWork: prompt.QueuedWork{AllowedTools: []string{"Bash(cargo:*)"}}}
			spec := translateSpec(qw, caps, SpecInputs{
				Cwd:          "/tmp/wt",
				Prompt:       "do",
				Logger:       logger,
				ProviderName: provider,
			})
			if spec.AllowedTools != nil {
				t.Errorf("AllowedTools must be dropped for %s; got %v", provider, spec.AllowedTools)
			}
			if spec.PermissionConfig != nil {
				t.Errorf("%s must NOT get a PermissionConfig; got %+v", provider, spec.PermissionConfig)
			}
			logs := buf.String()
			if !strings.Contains(logs, "dropping the agent card's AllowedTools") {
				t.Errorf("expected WARN about dropped AllowedTools; got logs:\n%s", logs)
			}
			if !strings.Contains(logs, provider) {
				t.Errorf("WARN must name the provider %q; got logs:\n%s", provider, logs)
			}
		})
	}
}

// TestTranslateSpec_AllowedToolsDrop_NoWarnWhenNoLogger verifies the WARN path
// is nil-safe: with no logger and no card allowlist, the drop is silent and no
// panic occurs.
func TestTranslateSpec_AllowedToolsDrop_NoWarnWhenNoLogger(t *testing.T) {
	t.Parallel()
	caps := agent.Capabilities{NeedsPermissionConfig: false, AcceptsAllowedToolsList: false}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{AllowedTools: []string{"Edit"}}}
	spec := translateSpec(qw, caps, SpecInputs{Cwd: "/tmp/wt", Prompt: "do"})
	if spec.AllowedTools != nil {
		t.Errorf("AllowedTools must be dropped; got %v", spec.AllowedTools)
	}
}

// mcpCapableCaps returns capabilities that advertise MCP tool-plugin support
// end-to-end (claude/codex/gemini shape): SupportsToolPlugins &&
// AcceptsMcpServerSpec. This is the exact gate the code-intel MCPToolNames
// allow-list and the Spec.MCPServers forwarding both key off.
func mcpCapableCaps() agent.Capabilities {
	return agent.Capabilities{
		SupportsToolPlugins:     true,
		AcceptsMcpServerSpec:    true,
		AcceptsAllowedToolsList: true,
	}
}

var wantCodeIntelFQ = []string{
	"mcp__af-code-intelligence__af_code_get_repo_map",
	"mcp__af-code-intelligence__af_code_search_symbols",
	"mcp__af-code-intelligence__af_code_search_code",
	"mcp__af-code-intelligence__af_code_check_duplicate",
	"mcp__af-code-intelligence__af_code_find_type_usages",
	"mcp__af-code-intelligence__af_code_validate_cross_deps",
}

// TestTranslateSpec_CodeIntelMCPToolNames_MCPCapable verifies that on an
// MCP-capable provider a present CodeIntel block allow-lists all six FQ
// code-intel tool names, in canonical order, on Spec.MCPToolNames.
func TestTranslateSpec_CodeIntelMCPToolNames_MCPCapable(t *testing.T) {
	t.Parallel()
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{
		CodeIntel: &prompt.CodeIntelWork{Repo: "owner/repo"},
	}}
	spec := translateSpec(qw, mcpCapableCaps(), SpecInputs{Cwd: "/tmp/wt"})
	if !slices.Equal(spec.MCPToolNames, wantCodeIntelFQ) {
		t.Errorf("MCPToolNames = %v\nwant %v", spec.MCPToolNames, wantCodeIntelFQ)
	}
}

// TestTranslateSpec_CodeIntelMCPToolNames_FilteredSubset verifies the block's
// Tools subset narrows the allow-list to just the requested tools (canonical
// order), so a restricted capability does not over-allow.
func TestTranslateSpec_CodeIntelMCPToolNames_FilteredSubset(t *testing.T) {
	t.Parallel()
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{
		CodeIntel: &prompt.CodeIntelWork{
			Repo:  "owner/repo",
			Tools: []string{"af_code_search_code", "af_code_search_symbols"},
		},
	}}
	spec := translateSpec(qw, mcpCapableCaps(), SpecInputs{Cwd: "/tmp/wt"})
	want := []string{
		"mcp__af-code-intelligence__af_code_search_symbols",
		"mcp__af-code-intelligence__af_code_search_code",
	}
	if !slices.Equal(spec.MCPToolNames, want) {
		t.Errorf("MCPToolNames = %v\nwant %v (canonical order, filtered)", spec.MCPToolNames, want)
	}
}

// TestTranslateSpec_CodeIntelMCPToolNames_NilBlockEmpty verifies MCPToolNames
// stays empty when there is no CodeIntel block — byte-identical to today.
func TestTranslateSpec_CodeIntelMCPToolNames_NilBlockEmpty(t *testing.T) {
	t.Parallel()
	spec := translateSpec(QueuedWork{}, mcpCapableCaps(), SpecInputs{Cwd: "/tmp/wt"})
	if len(spec.MCPToolNames) != 0 {
		t.Errorf("no-block MCPToolNames must be empty, got %v", spec.MCPToolNames)
	}
}

// TestTranslateSpec_CodeIntelMCPToolNames_NonMCPProviderEmpty verifies that a
// provider that ignores MCP specs (ollama/opencode/agycli shape:
// SupportsToolPlugins=false) never gets the FQ allow-list — the gate matches
// the Spec.MCPServers forwarding gate.
func TestTranslateSpec_CodeIntelMCPToolNames_NonMCPProviderEmpty(t *testing.T) {
	t.Parallel()
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{
		CodeIntel: &prompt.CodeIntelWork{Repo: "owner/repo"},
	}}
	caps := agent.Capabilities{SupportsToolPlugins: false, AcceptsMcpServerSpec: false}
	spec := translateSpec(qw, caps, SpecInputs{Cwd: "/tmp/wt"})
	if len(spec.MCPToolNames) != 0 {
		t.Errorf("non-MCP provider MCPToolNames must be empty, got %v", spec.MCPToolNames)
	}
}

// TestCodeIntelFQToolNames_AllAndFiltered pins the pure helper: nil/empty subset
// yields all six FQ names in canonical order; a subset filters and canonicalises
// order; unknown names are ignored (never allow-listed).
func TestCodeIntelFQToolNames_AllAndFiltered(t *testing.T) {
	t.Parallel()
	if got := codeIntelFQToolNames(nil); !slices.Equal(got, wantCodeIntelFQ) {
		t.Errorf("nil block: got %v\nwant %v", got, wantCodeIntelFQ)
	}
	if got := codeIntelFQToolNames(&prompt.CodeIntelWork{}); !slices.Equal(got, wantCodeIntelFQ) {
		t.Errorf("empty subset: got %v\nwant %v", got, wantCodeIntelFQ)
	}
	got := codeIntelFQToolNames(&prompt.CodeIntelWork{
		Tools: []string{"af_code_validate_cross_deps", "bogus_tool", "af_code_get_repo_map"},
	})
	want := []string{
		"mcp__af-code-intelligence__af_code_get_repo_map",
		"mcp__af-code-intelligence__af_code_validate_cross_deps",
	}
	if !slices.Equal(got, want) {
		t.Errorf("filtered subset (unknown ignored, canonical order): got %v\nwant %v", got, want)
	}
}

// TestTranslateSpec_EndpointBindingCopied verifies that all resolved endpoint
// fields flow to Spec.Endpoint while provider-side mutation cannot alter queued
// work or its credential map.
func TestTranslateSpec_EndpointBindingCopied(t *testing.T) {
	t.Parallel()
	endpoint := &agent.EndpointBinding{
		Company:       agent.CompanyOpenAI,
		Model:         "model-a",
		BaseURL:       "https://gateway.example.test/v1",
		Protocol:      agent.ProtoOpenAIResponses,
		Host:          agent.HostGateway,
		Auth:          agent.AuthBYOK,
		Region:        "us-east-1",
		CostModel:     agent.CostMeteredPerToken,
		BringsOwnAuth: true,
		Env:           map[string]string{"API_KEY": "queued-key", "REGION": "us-east-1"},
	}
	qw := QueuedWork{ResolvedProfile: ResolvedProfile{Endpoint: endpoint}}

	spec := translateSpec(qw, agent.Capabilities{}, SpecInputs{})
	if spec.Endpoint == nil {
		t.Fatal("Endpoint: expected resolved binding to propagate")
	}
	if spec.Endpoint == endpoint {
		t.Fatal("Endpoint: binding aliases queued work")
	}

	type endpointFields struct {
		company       agent.Company
		model         string
		baseURL       string
		protocol      agent.WireProtocol
		host          agent.ServingHost
		auth          agent.AuthMode
		region        string
		costModel     agent.CostModel
		bringsOwnAuth bool
	}
	got := endpointFields{
		company:       spec.Endpoint.Company,
		model:         spec.Endpoint.Model,
		baseURL:       spec.Endpoint.BaseURL,
		protocol:      spec.Endpoint.Protocol,
		host:          spec.Endpoint.Host,
		auth:          spec.Endpoint.Auth,
		region:        spec.Endpoint.Region,
		costModel:     spec.Endpoint.CostModel,
		bringsOwnAuth: spec.Endpoint.BringsOwnAuth,
	}
	want := endpointFields{
		company:       endpoint.Company,
		model:         endpoint.Model,
		baseURL:       endpoint.BaseURL,
		protocol:      endpoint.Protocol,
		host:          endpoint.Host,
		auth:          endpoint.Auth,
		region:        endpoint.Region,
		costModel:     endpoint.CostModel,
		bringsOwnAuth: endpoint.BringsOwnAuth,
	}
	if got != want {
		t.Errorf("Endpoint structural fields = %+v, want %+v", got, want)
	}
	if !maps.Equal(spec.Endpoint.Env, endpoint.Env) {
		t.Errorf("Endpoint.Env = %v, want %v", spec.Endpoint.Env, endpoint.Env)
	}

	spec.Endpoint.Model = "provider-mutated"
	spec.Endpoint.Env["API_KEY"] = "provider-mutated"
	if endpoint.Model != "model-a" {
		t.Errorf("queued endpoint model mutated through Spec: %q", endpoint.Model)
	}
	if endpoint.Env["API_KEY"] != "queued-key" {
		t.Errorf("queued endpoint env mutated through Spec: %q", endpoint.Env["API_KEY"])
	}
	endpoint.Env["REGION"] = "queued-mutated"
	if spec.Endpoint.Env["REGION"] != "us-east-1" {
		t.Errorf("Spec endpoint env mutated through queued work: %q", spec.Endpoint.Env["REGION"])
	}
}

// TestTranslateSpec_NilEndpointRemainsNil keeps pre-endpoint queued work on
// the legacy path without serializing or materializing a binding.
func TestTranslateSpec_NilEndpointRemainsNil(t *testing.T) {
	t.Parallel()
	spec := translateSpec(QueuedWork{}, agent.Capabilities{}, SpecInputs{})
	if spec.Endpoint != nil {
		t.Errorf("Endpoint = %+v, want nil", spec.Endpoint)
	}
}
