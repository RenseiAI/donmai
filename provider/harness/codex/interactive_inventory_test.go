package codex

import (
	"context"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestCompareInteractiveMCPInventoryRequiresExactExclusiveSurface(t *testing.T) {
	want := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example/api/mcp/session",
		Headers: map[string]string{
			"Authorization": "Bearer fixture",
		},
	}}
	exact := codexMCPInventoryEntry{Name: "donmai-platform", Enabled: true}
	exact.Transport.Type = "streamable_http"
	exact.Transport.URL = want[0].URL
	exact.Transport.EnvHTTPHeaders = map[string]string{
		"Authorization": codexHTTPHeaderEnvName("donmai-platform", "Authorization"),
	}

	if err := compareInteractiveMCPEntry(want[0], exact); err != nil {
		t.Fatalf("exact inventory: %v", err)
	}

	extra := codexMCPInventoryEntry{Name: "ambient", Enabled: true}
	extra.Transport.Type = "stdio"
	extra.Transport.Command = "ambient-server"
	if err := compareInteractiveMCPListNames(want, []codexMCPInventoryEntry{exact, extra}); err == nil {
		t.Fatal("undeclared ambient server was accepted")
	}

	widened := exact
	widened.Transport.HTTPHeaders = map[string]string{"X-Poison": "present"}
	if err := compareInteractiveMCPEntry(want[0], widened); err == nil {
		t.Fatal("same-name server with a merged literal header was accepted")
	}

	widened = exact
	timeout := 30.0
	widened.ToolTimeout = &timeout
	if err := compareInteractiveMCPEntry(want[0], widened); err == nil {
		t.Fatal("same-name server with a merged timeout was accepted")
	}

	widened = exact
	widened.DisabledTools = []string{"a2a_send_message"}
	if err := compareInteractiveMCPEntry(want[0], widened); err == nil {
		t.Fatal("same-name server with a merged tool filter was accepted")
	}
}

func TestVerifyExclusiveInteractiveMCPFailsBeforePTYOnReadbackError(t *testing.T) {
	spec := agent.Spec{
		Cwd: t.TempDir(),
		MCPServers: []agent.MCPServerConfig{{
			Name: "donmai-platform", Type: "http", URL: "https://platform.example/api/mcp/session",
		}},
	}
	launch, err := buildInteractiveLaunch(spec)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyExclusiveInteractiveMCP(
		t.Context(),
		func(context.Context, string, string, []string, []string, []string) ([]byte, error) {
			return nil, errors.New("readback unavailable")
		},
		"codex",
		spec,
		launch,
		t.TempDir(),
	)
	if !errors.Is(err, ErrInteractiveCodexMCPIsolation) {
		t.Fatalf("readback error = %v", err)
	}
}
