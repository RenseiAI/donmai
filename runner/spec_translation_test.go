package runner

import (
	"slices"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// TestTranslateSpec_PlatformDisallowedTools_Appended verifies that
// platform-supplied DisallowedTools patterns (SUP-1840 Option B) are
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
