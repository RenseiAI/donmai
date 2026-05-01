package claude

import (
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func TestBuildArgs_RequiredFlags(t *testing.T) {
	t.Parallel()

	argv, _ := buildArgs(agent.Spec{}, "", "")

	for _, want := range []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv missing required flag %q: %v", want, argv)
		}
	}
}

func TestBuildArgs_PromptToStdin(t *testing.T) {
	t.Parallel()

	argv, stdin := buildArgs(agent.Spec{Prompt: "do the thing"}, "", "")

	if stdin != "do the thing" {
		t.Errorf("stdin = %q, want %q", stdin, "do the thing")
	}
	for _, a := range argv {
		if strings.Contains(a, "do the thing") {
			t.Errorf("prompt should not appear in argv (would leak via process listing): %q", a)
		}
	}
}

func TestBuildArgs_AllSpecFields(t *testing.T) {
	t.Parallel()

	maxTurns := 7
	spec := agent.Spec{
		Prompt:             "implement REN-1",
		Cwd:                "/tmp/work",
		Env:                map[string]string{"FOO": "bar"},
		Autonomous:         true,
		SandboxEnabled:     true,
		SandboxLevel:       agent.SandboxWorkspaceWrite,
		AllowedTools:       []string{"Bash(pnpm:*)", "Edit"},
		DisallowedTools:    []string{"WebSearch"},
		MCPServers:         []agent.MCPServerConfig{{Name: "x"}}, // non-empty so mcpConfigPath is used in argv
		MCPToolNames:       []string{"mcp__af_code_search"},
		MaxTurns:           &maxTurns,
		Model:              "claude-sonnet-4-7",
		Effort:             agent.EffortHigh,
		BaseInstructions:   "ignored: codex-only",
		SystemPromptAppend: "extra system prompt",
		PermissionConfig: &agent.PermissionConfig{
			AllowPatterns: []string{"x"},
		}, // ignored: codex-only
		CodeIntelEnforcement: &agent.CodeIntelEnforcement{
			EnforceUsage: true,
		}, // ignored: v0.5.0 CLI limitation
		ProviderConfig:   map[string]any{"opaque": true},
		SubAgentProvider: agent.ProviderCodex,
		// OnProcessSpawned is not consumed by buildArgs.
	}

	argv, stdin := buildArgs(spec, "/tmp/mcp.json", "")

	if stdin != "implement REN-1" {
		t.Errorf("stdin: got %q, want prompt", stdin)
	}

	type want struct {
		flag, val string
	}
	expectedPairs := []want{
		{"--add-dir", "/tmp/work"},
		{"--model", "claude-sonnet-4-7"},
		{"--max-turns", "7"},
		{"--effort", "high"},
		{"--append-system-prompt", "extra system prompt"},
		{"--permission-mode", "bypassPermissions"},
		{"--mcp-config", "/tmp/mcp.json"},
	}
	for _, w := range expectedPairs {
		if i := slices.Index(argv, w.flag); i < 0 || i+1 >= len(argv) || argv[i+1] != w.val {
			t.Errorf("argv missing %s %s; argv=%v", w.flag, w.val, argv)
		}
	}

	if !slices.Contains(argv, "--strict-mcp-config") {
		t.Errorf("argv should include --strict-mcp-config when mcp config set: %v", argv)
	}

	// AllowedTools merges Spec.AllowedTools + MCPToolNames, sorted.
	allowedIdx := slices.Index(argv, "--allowedTools")
	if allowedIdx < 0 || allowedIdx+1 >= len(argv) {
		t.Fatalf("argv missing --allowedTools: %v", argv)
	}
	allowedJoined := argv[allowedIdx+1]
	for _, expect := range []string{"Bash(pnpm:*)", "Edit", "mcp__af_code_search"} {
		if !strings.Contains(allowedJoined, expect) {
			t.Errorf("allowedTools missing %q: %s", expect, allowedJoined)
		}
	}

	// DisallowedTools includes Spec.DisallowedTools and the Linear MCP block list (Autonomous=true).
	disallowedIdx := slices.Index(argv, "--disallowedTools")
	if disallowedIdx < 0 || disallowedIdx+1 >= len(argv) {
		t.Fatalf("argv missing --disallowedTools: %v", argv)
	}
	disallowedJoined := argv[disallowedIdx+1]
	for _, expect := range []string{"WebSearch", "AskUserQuestion", "mcp__claude_ai_Linear__get_issue"} {
		if !strings.Contains(disallowedJoined, expect) {
			t.Errorf("disallowedTools missing %q: %s", expect, disallowedJoined)
		}
	}
}

func TestBuildArgs_NotAutonomous_NoLinearBlocklist(t *testing.T) {
	t.Parallel()

	argv, _ := buildArgs(agent.Spec{
		Prompt:          "x",
		Autonomous:      false,
		DisallowedTools: []string{"WebSearch"},
	}, "", "")

	disallowedIdx := slices.Index(argv, "--disallowedTools")
	if disallowedIdx < 0 {
		t.Fatalf("argv missing --disallowedTools: %v", argv)
	}
	if strings.Contains(argv[disallowedIdx+1], "AskUserQuestion") {
		t.Errorf("non-autonomous mode should not include Linear block list: %s", argv[disallowedIdx+1])
	}
	// Permission mode should not be set when not autonomous.
	if slices.Contains(argv, "--permission-mode") {
		t.Errorf("non-autonomous mode should not set --permission-mode: %v", argv)
	}
}

func TestBuildArgs_ResumeFlag(t *testing.T) {
	t.Parallel()

	argv, _ := buildArgs(agent.Spec{Prompt: "p"}, "", "session-uuid")

	idx := slices.Index(argv, "--resume")
	if idx < 0 || idx+1 >= len(argv) {
		t.Fatalf("argv missing --resume: %v", argv)
	}
	if argv[idx+1] != "session-uuid" {
		t.Errorf("--resume value = %q, want %q", argv[idx+1], "session-uuid")
	}
}

func TestBuildArgs_NoMCPConfig(t *testing.T) {
	t.Parallel()

	argv, _ := buildArgs(agent.Spec{Prompt: "p"}, "", "")

	if slices.Contains(argv, "--mcp-config") {
		t.Errorf("argv should not include --mcp-config when path empty: %v", argv)
	}
}

func TestBuildArgs_DedupAllowedTools(t *testing.T) {
	t.Parallel()

	argv, _ := buildArgs(agent.Spec{
		AllowedTools: []string{"Edit", "Edit", "Bash"},
		MCPToolNames: []string{"Edit"},
	}, "", "")

	idx := slices.Index(argv, "--allowedTools")
	if idx < 0 {
		t.Fatalf("missing --allowedTools: %v", argv)
	}
	parts := strings.Split(argv[idx+1], ",")
	count := 0
	for _, p := range parts {
		if p == "Edit" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("Edit appears %d times, want 1: %s", count, argv[idx+1])
	}
}

func TestComposeEnv_DeterministicOrder(t *testing.T) {
	t.Parallel()

	out1 := composeEnv([]string{"PATH=/usr/bin"}, map[string]string{"B": "2", "A": "1"})
	out2 := composeEnv([]string{"PATH=/usr/bin"}, map[string]string{"A": "1", "B": "2"})

	if !slices.Equal(out1, out2) {
		t.Errorf("composeEnv not deterministic:\n%v\n%v", out1, out2)
	}
	want := []string{"PATH=/usr/bin", "A=1", "B=2"}
	if !slices.Equal(out1, want) {
		t.Errorf("composeEnv = %v, want %v", out1, want)
	}
}

func TestComposeEnv_EmptySpecEnv(t *testing.T) {
	t.Parallel()

	parent := []string{"PATH=/usr/bin", "HOME=/home/agent"}
	out := composeEnv(parent, nil)
	if !slices.Equal(out, parent) {
		t.Errorf("composeEnv with empty spec.Env = %v, want %v", out, parent)
	}
}

func TestDedupAndSort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, nil},
		{"unique", []string{"b", "a", "c"}, []string{"a", "b", "c"}},
		{"dups", []string{"a", "b", "a"}, []string{"a", "b"}},
		{"blanks", []string{"a", "", "b", ""}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := dedupAndSort(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("dedupAndSort(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
