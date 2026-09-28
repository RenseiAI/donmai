package gemini

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestToolPolicy_ExactNativeBoundary(t *testing.T) {
	t.Parallel()
	policy := newToolPolicy(agent.Spec{
		AllowedTools:    []string{"Read", "Bash(git:*)", "Write"},
		DisallowedTools: []string{"Write", "Bash(git push:*)"},
	})
	tests := []struct {
		name string
		call candidateFuncCall
		want bool
	}{
		{name: "allowed read", call: candidateFuncCall{Name: "Read", Args: map[string]any{"path": "README.md"}}, want: true},
		{name: "allowed git status", call: candidateFuncCall{Name: "Bash", Args: map[string]any{"command": "git status"}}, want: true},
		{name: "denied git push wins", call: candidateFuncCall{Name: "Bash", Args: map[string]any{"command": "git push origin main"}}, want: false},
		{name: "unlisted shell prefix", call: candidateFuncCall{Name: "Bash", Args: map[string]any{"command": "curl example.com"}}, want: false},
		{name: "deny wins over allow", call: candidateFuncCall{Name: "Write", Args: map[string]any{"path": "x"}}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _ := policy.allow(test.call)
			if got != test.want {
				t.Fatalf("allow=%v, want %v", got, test.want)
			}
		})
	}
}

func TestToolPolicy_MCPServerGrantIsScoped(t *testing.T) {
	t.Parallel()
	policy := newToolPolicy(agent.Spec{AllowedTools: []string{"Read"}, MCPServers: []agent.MCPServerConfig{{Name: "platform-gate"}}})
	if ok, _ := policy.allow(candidateFuncCall{Name: "mcp__platform-gate__get_issue"}); !ok {
		t.Fatal("declared server tool must be allowed")
	}
	if ok, _ := policy.allow(candidateFuncCall{Name: "mcp__other__get_issue"}); ok {
		t.Fatal("undeclared server tool must be denied")
	}
}

func TestToolExecutor_DeniesBeforeSideEffect(t *testing.T) {
	t.Parallel()
	executor := newToolExecutor(t.TempDir(), nil, nil)
	executor.policy = newToolPolicy(agent.Spec{AllowedTools: []string{"Read"}})
	result := executor.execute(t.Context(), candidateFuncCall{Name: "Write", Args: map[string]any{"path": "forbidden", "content": "no"}})
	if !result.isError {
		t.Fatal("unlisted write must fail")
	}
}

// TestToolPolicy_AllowGateOnlyFromConfiguredListAtBypass pins the removal of
// the runner's hidden allow list for this in-box loop: below an allow-gated
// toolApproval level (bypass — what a Spec with no section reads as — and
// deny-list) with no configured allow list there is no allow gate — a
// declared MCP server does not conjure one — while deny entries still hold.
// A configured list, or an allow-gated level, keeps the gate.
func TestToolPolicy_AllowGateOnlyFromConfiguredListAtBypass(t *testing.T) {
	t.Parallel()
	denyList := &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
	denyList.Levels.ToolApproval = agent.ToolApprovalDenyList
	allowList := &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
	allowList.Levels.ToolApproval = agent.ToolApprovalAllowList
	bash := candidateFuncCall{Name: "bash", Args: map[string]any{"command": "ls -la"}}
	tests := []struct {
		name string
		spec agent.Spec
		call candidateFuncCall
		want bool
	}{
		{"bypass without a list is ungated", agent.Spec{}, bash, true},
		{"bypass: a declared MCP server does not create a gate", agent.Spec{MCPServers: []agent.MCPServerConfig{{Name: "platform-gate"}}}, bash, true},
		{"bypass: deny entries still hold", agent.Spec{DisallowedTools: []string{"Bash(ls:*)"}}, bash, false},
		{"bypass: a configured list gates", agent.Spec{AllowedTools: []string{"Read"}}, bash, false},
		{"deny-list without a list is ungated", agent.Spec{ExecutionSecurity: denyList}, bash, true},
		{"allow-list without a list is gated", agent.Spec{ExecutionSecurity: allowList}, bash, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got, reason := newToolPolicy(test.spec).allow(test.call); got != test.want {
				t.Fatalf("allow=%v (%s), want %v", got, reason, test.want)
			}
		})
	}
}

// TestToolsFromSpecOffersTheNativeSurfaceWithoutAnAllowGate is the tool-list
// half of the hidden-default removal: with no allow gate the model is still
// offered the full native tool set (what the removed default list declared),
// a configured list declares exactly what it names, and an allow-gated level
// with no list declares no native tool.
func TestToolsFromSpecOffersTheNativeSurfaceWithoutAnAllowGate(t *testing.T) {
	t.Parallel()
	allowList := &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
	allowList.Levels.ToolApproval = agent.ToolApprovalAllowList
	names := func(spec agent.Spec) []string {
		var out []string
		for _, tool := range toolsFromSpec(spec) {
			for _, decl := range tool.FunctionDeclarations {
				out = append(out, decl.Name)
			}
		}
		return out
	}
	tests := []struct {
		name string
		spec agent.Spec
		want []string
	}{
		{"bypass without a list offers every native tool", agent.Spec{}, []string{"Bash", "Edit", "Write", "Read", "Grep", "Glob", "Task"}},
		{"bypass with MCP tools keeps the native tools too", agent.Spec{MCPToolNames: []string{"mcp__code__search"}}, []string{"Bash", "Edit", "Write", "Read", "Grep", "Glob", "Task", "mcp__code__search"}},
		{"a configured list declares what it names", agent.Spec{AllowedTools: []string{"Read", "Bash(git:*)"}}, []string{"Read", "Bash"}},
		{"allow-list without a list declares no native tool", agent.Spec{ExecutionSecurity: allowList}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := names(tc.spec); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("declared tools = %v, want %v", got, tc.want)
			}
		})
	}
}
