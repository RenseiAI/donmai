package afcli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	provideramp "github.com/RenseiAI/donmai/provider/harness/amp"
	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
	providergemini "github.com/RenseiAI/donmai/provider/harness/gemini"
	provideropencode "github.com/RenseiAI/donmai/provider/harness/opencode"
	providerstub "github.com/RenseiAI/donmai/provider/harness/stub"
)

// TestToolUseCapabilityMatrix asserts the v2 tool-use surface flags
// (`AcceptsAllowedToolsList`, `AcceptsMcpServerSpec`,
// `SupportsToolPlugins`) declared by every provider that can be
// constructed in-test match the canonical matrix in
// donmai-architecture/002-provider-base-contract.md §"Tool-use surface".
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
			// Gemini-first-class program: native function-calling +
			// in-provider tool executor (Bash/Read/Edit/Write) → tool
			// plugins + AllowedTools honoured. MCP-server spec is NOT yet
			// honored (no in-box MCP stdio client; acceptsMcpServerSpec=false,
			// MCP→functionDeclaration bridge is a follow-up).
			want: want{supportsToolPlugins: true, acceptsAllowedToolsList: true, acceptsMcpServerSpec: false},
		},
		{
			name: "amp",
			new: func(t *testing.T) agent.Provider {
				// Inject a fake LookPath so the test doesn't depend on whether
				// `amp` is installed on the CI runner.
				p, err := provideramp.New(provideramp.Options{
					APIKey:   "test-key",
					LookPath: func(string) (string, error) { return "/usr/bin/amp", nil },
				})
				if err != nil {
					t.Fatalf("amp.New: %v", err)
				}
				return p
			},
			// Since commit 3c6b6c6, amp Spawn writes a
			// per-session MCP tmpfile and passes --mcp-config, so
			// SupportsToolPlugins+AcceptsMcpServerSpec are both true.
			// AllowedTools is not honoured (amp has no --allowedTools;
			// permission control is via settings.json + --dangerously-
			// allow-all), so AcceptsAllowedToolsList stays false.
			want: want{supportsToolPlugins: true, acceptsAllowedToolsList: false, acceptsMcpServerSpec: true},
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
