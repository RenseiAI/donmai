package afcli

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
)

func TestCodexHostSessionCtorHint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		detail *daemon.SessionDetail
		want   bool
	}{
		{name: "nil detail"},
		{
			name: "codex host session",
			detail: &daemon.SessionDetail{ResolvedProfile: &daemon.SessionResolvedProfile{
				Provider: string(agent.ProviderCodex), AuthMode: string(agent.AuthHostSession),
			}},
			want: true,
		},
		{
			name: "codex keyed auth",
			detail: &daemon.SessionDetail{ResolvedProfile: &daemon.SessionResolvedProfile{
				Provider: string(agent.ProviderCodex), AuthMode: string(agent.AuthBYOK),
			}},
		},
		{
			name: "other provider host session",
			detail: &daemon.SessionDetail{ResolvedProfile: &daemon.SessionResolvedProfile{
				Provider: string(agent.ProviderClaude), AuthMode: string(agent.AuthHostSession),
			}},
		},
		{
			name: "model profile selects codex",
			detail: &daemon.SessionDetail{
				ModelProfile: &daemon.SessionModelProfile{ProviderID: string(agent.ProviderCodex)},
				ResolvedProfile: &daemon.SessionResolvedProfile{
					Provider: string(agent.ProviderClaude), AuthMode: string(agent.AuthHostSession),
				},
			},
			want: true,
		},
		{
			name: "explicit codex harness overrides legacy provider",
			detail: &daemon.SessionDetail{ResolvedProfile: &daemon.SessionResolvedProfile{
				Harness: string(agent.HarnessCodex), Provider: string(agent.ProviderClaude), AuthMode: string(agent.AuthHostSession),
			}},
			want: true,
		},
		{
			name: "unknown explicit harness does not fall through to codex provider",
			detail: &daemon.SessionDetail{ResolvedProfile: &daemon.SessionResolvedProfile{
				Harness: "future-harness", Provider: string(agent.ProviderCodex), AuthMode: string(agent.AuthHostSession),
			}},
		},
		{
			name: "model profile explicit non-codex harness overrides codex provider id",
			detail: &daemon.SessionDetail{
				ModelProfile: &daemon.SessionModelProfile{
					Harness: string(agent.HarnessClaudeCode), ProviderID: string(agent.ProviderCodex),
				},
				ResolvedProfile: &daemon.SessionResolvedProfile{AuthMode: string(agent.AuthHostSession)},
			},
		},
		{
			name: "model profile explicit codex harness overrides provider id",
			detail: &daemon.SessionDetail{
				ModelProfile: &daemon.SessionModelProfile{
					Harness: string(agent.HarnessCodex), ProviderID: string(agent.ProviderClaude),
				},
				ResolvedProfile: &daemon.SessionResolvedProfile{AuthMode: string(agent.AuthHostSession)},
			},
			want: true,
		},
		{
			name: "legacy runner selects codex",
			detail: &daemon.SessionDetail{ResolvedProfile: &daemon.SessionResolvedProfile{
				Runner: string(agent.ProviderCodex), AuthMode: string(agent.AuthHostSession),
			}},
			want: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := codexHostSessionCtorHint(tt.detail); got != tt.want {
				t.Fatalf("codexHostSessionCtorHint() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentRunHintsThreadsCodexHostSessionAuth(t *testing.T) {
	t.Parallel()

	detail := &daemon.SessionDetail{ResolvedProfile: &daemon.SessionResolvedProfile{
		Provider: string(agent.ProviderCodex), AuthMode: string(agent.AuthHostSession),
	}}
	hints := agentRunHints(detail)
	if !hints.CodexHostSessionAuth {
		t.Fatal("agentRunHints dropped resolved codex host-session auth")
	}
	if !codexCtorOptions(hints).HostSessionAuth {
		t.Fatal("codexCtorOptions dropped host-session auth hint")
	}
	if codexCtorOptions(agentRunCtorHints{}).HostSessionAuth {
		t.Fatal("zero-value ctor hints unexpectedly enable host-session auth")
	}
}
