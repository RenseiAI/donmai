package codex

import (
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/RenseiAI/donmai/agent"
)

func TestCodexApprovalsPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		getenv  func(string) string
		want    string
		wantErr bool
	}{
		{name: "unset defaults to off", getenv: envOf(nil), want: codexApprovalsOff},
		{name: "nil lookup defaults to off", getenv: nil, want: codexApprovalsOff},
		{name: "blank defaults to off", getenv: envOf(map[string]string{codexApprovalsEnv: "   "}), want: codexApprovalsOff},
		{name: "off is explicit", getenv: envOf(map[string]string{codexApprovalsEnv: "off"}), want: codexApprovalsOff},
		{name: "case and padding are tolerated", getenv: envOf(map[string]string{codexApprovalsEnv: "  Inherit \n"}), want: codexApprovalsInherit},
		{name: "inherit restores codex prompting", getenv: envOf(map[string]string{codexApprovalsEnv: "inherit"}), want: codexApprovalsInherit},
		{name: "a typo fails rather than guessing", getenv: envOf(map[string]string{codexApprovalsEnv: "inherti"}), wantErr: true},
		{name: "a boolean is not a policy", getenv: envOf(map[string]string{codexApprovalsEnv: "true"}), wantErr: true},
		{name: "another harness vocabulary is not accepted", getenv: envOf(map[string]string{codexApprovalsEnv: "yolo"}), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := codexApprovalsPolicy(tt.getenv)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("codexApprovalsPolicy = %q, want an error", got)
				}
				if !strings.Contains(err.Error(), codexApprovalsEnv) {
					t.Fatalf("error %q does not name %s", err, codexApprovalsEnv)
				}
				return
			}
			if err != nil {
				t.Fatalf("codexApprovalsPolicy: %v", err)
			}
			if got != tt.want {
				t.Fatalf("codexApprovalsPolicy = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestInteractiveApprovalArgs pins BOTH keys together. Seeding only
// approval_policy leaves the sandbox raising escalation reviews for commands
// that touch the network or write outside the workspace — the residual class
// that survives turning approvals off from inside the TUI — so a change that
// dropped either key would leave a dispatched session able to hang.
func TestInteractiveApprovalArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy string
		want   []string
	}{
		{
			name:   "off seeds policy and sandbox together",
			policy: codexApprovalsOff,
			want: []string{
				"--config", `approval_policy="never"`,
				"--config", `sandbox_mode="danger-full-access"`,
			},
		},
		{name: "inherit seeds nothing", policy: codexApprovalsInherit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := interactiveApprovalArgs(tt.policy)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("interactiveApprovalArgs(%q) = %q, want %q", tt.policy, got, tt.want)
			}
		})
	}
}

// TestBuildInteractiveLaunch_ApprovalPosture drives the whole launch builder so
// the pin covers what the child process really receives, including the MCP
// per-tool-call gate. The mode value is pinned literally because codex accepts
// four of them and the plausible-sounding "auto" is NOT the one that
// pre-answers the review — "approve" is.
func TestBuildInteractiveLaunch_ApprovalPosture(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	spec := agent.Spec{
		Cwd:        workspace,
		Prompt:     "do the work",
		MCPServers: []agent.MCPServerConfig{{Name: "platform", Command: "/bin/server"}},
	}
	tests := []struct {
		name             string
		env              map[string]string
		wantApprovalArgs bool
		wantToolsMode    string
		wantErr          bool
	}{
		{name: "default posture runs unattended", env: nil, wantApprovalArgs: true, wantToolsMode: "approve"},
		{name: "off is the default spelled out", env: map[string]string{codexApprovalsEnv: "off"}, wantApprovalArgs: true, wantToolsMode: "approve"},
		{name: "inherit hands every gate back to codex", env: map[string]string{codexApprovalsEnv: "inherit"}},
		{name: "an unrecognized value fails the spawn", env: map[string]string{codexApprovalsEnv: "off-ish"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			launch, err := buildInteractiveLaunchEnv(spec, envOf(tt.env))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("buildInteractiveLaunchEnv = %q, want an error", launch.argv)
				}
				if len(launch.argv) != 0 || len(launch.env) != 0 {
					t.Fatalf("a refused spawn still produced a launch: %#v", launch)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildInteractiveLaunchEnv: %v", err)
			}

			joined := strings.Join(launch.argv, "\x00")
			hasPolicy := strings.Contains(joined, `approval_policy="never"`)
			hasSandbox := strings.Contains(joined, `sandbox_mode="danger-full-access"`)
			if hasPolicy != tt.wantApprovalArgs || hasSandbox != tt.wantApprovalArgs {
				t.Fatalf("approval seeds policy=%v sandbox=%v, want both %v: %q", hasPolicy, hasSandbox, tt.wantApprovalArgs, launch.argv)
			}

			var decoded struct {
				MCPServers map[string]struct {
					DefaultToolsApprovalMode string `toml:"default_tools_approval_mode"`
				} `toml:"mcp_servers"`
			}
			if err := toml.Unmarshal([]byte(mcpOverrideFromArgs(t, launch.argv)), &decoded); err != nil {
				t.Fatalf("MCP override is not semantic TOML: %v", err)
			}
			server, ok := decoded.MCPServers["platform"]
			if !ok {
				t.Fatalf("MCP override lost the requested server: %#v", decoded.MCPServers)
			}
			if server.DefaultToolsApprovalMode != tt.wantToolsMode {
				t.Fatalf("default_tools_approval_mode = %q, want %q", server.DefaultToolsApprovalMode, tt.wantToolsMode)
			}
		})
	}
}

// envOf builds a getenv stub. t.Setenv cannot be used in this package because
// every test here runs t.Parallel().
func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
