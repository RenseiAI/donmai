package afcli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	provideramp "github.com/RenseiAI/agentfactory-tui/provider/amp"
	providerclaude "github.com/RenseiAI/agentfactory-tui/provider/claude"
	providergemini "github.com/RenseiAI/agentfactory-tui/provider/gemini"
	provideropencode "github.com/RenseiAI/agentfactory-tui/provider/opencode"
	providerstub "github.com/RenseiAI/agentfactory-tui/provider/stub"
)

// TestToolUseCapabilityMatrix asserts the v2 tool-use surface flags
// (`AcceptsAllowedToolsList`, `AcceptsMcpServerSpec`,
// `SupportsToolPlugins`) declared by every provider that can be
// constructed in-test match the canonical matrix in
// rensei-architecture/002-provider-base-contract.md §"Tool-use surface".
//
// Codex and Ollama are exercised by their package-level tests
// (provider/codex/codex_test.go, provider/ollama/integration_test.go);
// constructing them here would require a JSON-RPC handshake / live
// HTTP server, both of which the per-package tests already cover.
func TestToolUseCapabilityMatrix(t *testing.T) {
	t.Parallel()

	// Build a fresh local-only opencode probe target so New() succeeds
	// without touching the real OpenCode default endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	type want struct {
		supportsToolPlugins     bool
		acceptsAllowedToolsList bool
		acceptsMcpServerSpec    bool
	}
	cases := []struct {
		name string
		new  func(t *testing.T) agent.Provider
		want want
	}{
		{
			name: "claude",
			new: func(t *testing.T) agent.Provider {
				p, err := providerclaude.New(providerclaude.Options{
					Binary:   "claude-fake",
					LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
				})
				if err != nil {
					t.Fatalf("claude.New: %v", err)
				}
				return p
			},
			// Both flags ON: --allowedTools + --mcp-config wired through
			// the CLI argv.
			want: want{supportsToolPlugins: true, acceptsAllowedToolsList: true, acceptsMcpServerSpec: true},
		},
		{
			name: "stub",
			new: func(t *testing.T) agent.Provider {
				p, err := providerstub.New()
				if err != nil {
					t.Fatalf("stub.New: %v", err)
				}
				return p
			},
			// Stub mirrors the Claude shape so the runner exercises every
			// gating branch when wired against the stub.
			want: want{supportsToolPlugins: true, acceptsAllowedToolsList: true, acceptsMcpServerSpec: true},
		},
		{
			name: "gemini",
			new: func(t *testing.T) agent.Provider {
				p, err := providergemini.New(providergemini.Options{APIKey: "test-key"})
				if err != nil {
					t.Fatalf("gemini.New: %v", err)
				}
				return p
			},
			// Function-calling / MCP not wired in v0.1.
			want: want{supportsToolPlugins: false, acceptsAllowedToolsList: false, acceptsMcpServerSpec: false},
		},
		{
			name: "amp",
			new: func(t *testing.T) agent.Provider {
				p, err := provideramp.New(provideramp.Options{APIKey: "test-key"})
				if err != nil {
					t.Fatalf("amp.New: %v", err)
				}
				return p
			},
			// Registration-only; no wire surface to honour.
			want: want{supportsToolPlugins: false, acceptsAllowedToolsList: false, acceptsMcpServerSpec: false},
		},
		{
			name: "opencode",
			new: func(t *testing.T) agent.Provider {
				p, err := provideropencode.New(provideropencode.Options{
					Endpoint:  srv.URL,
					SkipProbe: true,
				})
				if err != nil {
					t.Fatalf("opencode.New: %v", err)
				}
				return p
			},
			// Registration-only; no wire surface to honour.
			want: want{supportsToolPlugins: false, acceptsAllowedToolsList: false, acceptsMcpServerSpec: false},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := tc.new(t)
			caps := p.Capabilities()
			if got := caps.SupportsToolPlugins; got != tc.want.supportsToolPlugins {
				t.Errorf("SupportsToolPlugins: want %v, got %v", tc.want.supportsToolPlugins, got)
			}
			if got := caps.AcceptsAllowedToolsList; got != tc.want.acceptsAllowedToolsList {
				t.Errorf("AcceptsAllowedToolsList: want %v, got %v", tc.want.acceptsAllowedToolsList, got)
			}
			if got := caps.AcceptsMcpServerSpec; got != tc.want.acceptsMcpServerSpec {
				t.Errorf("AcceptsMcpServerSpec: want %v, got %v", tc.want.acceptsMcpServerSpec, got)
			}
			// Capability self-consistency: AcceptsMcpServerSpec=true
			// implies SupportsToolPlugins=true (you can't honour MCP
			// shape without supporting tool plugins at all).
			if caps.AcceptsMcpServerSpec && !caps.SupportsToolPlugins {
				t.Errorf("invariant: AcceptsMcpServerSpec=true requires SupportsToolPlugins=true")
			}
		})
	}
}
