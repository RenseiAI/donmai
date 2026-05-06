package runner

import (
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/prompt"
)

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
