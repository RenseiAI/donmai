package runner

import (
	"bytes"
	"log/slog"
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
