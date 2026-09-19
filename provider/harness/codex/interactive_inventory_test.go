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

func TestCompareInteractiveMCPInventoryAcceptsOnlyExactDeclaredProtectedHelper(t *testing.T) {
	want := helperBackedServer(t)
	want.Headers = map[string]string{"X-Declared": "fixture-value"}
	helper, ok := agent.ProtectedRuntimeMCPHeadersHelper(want)
	if !ok {
		t.Fatal("protected helper missing from requested server")
	}
	exact := codexMCPInventoryEntry{Name: want.Name, Enabled: true}
	exact.Transport.Type = "streamable_http"
	exact.Transport.URL = want.URL
	exact.Transport.HTTPHeadersHelper = &helper
	exact.Transport.EnvHTTPHeaders = map[string]string{
		"X-Declared": codexHTTPHeaderEnvName(want.Name, "X-Declared"),
	}
	if err := compareInteractiveMCPEntry(want, exact); err != nil {
		t.Fatalf("exact declared protected helper: %v", err)
	}

	missing := exact
	missing.Transport.HTTPHeadersHelper = nil
	if err := compareInteractiveMCPEntry(want, missing); err == nil {
		t.Fatal("missing declared protected helper was accepted")
	}

	changed := exact
	other := "different-private-helper"
	changed.Transport.HTTPHeadersHelper = &other
	if err := compareInteractiveMCPEntry(want, changed); err == nil {
		t.Fatal("changed declared protected helper was accepted")
	}

	plainHTTP := agent.MCPServerConfig{
		Name: want.Name, Type: "http", URL: want.URL,
		Headers: map[string]string{"X-Declared": "fixture-value"},
	}
	if err := compareInteractiveMCPEntry(plainHTTP, exact); err == nil {
		t.Fatal("undeclared protected helper was accepted")
	}

	stdio := agent.MCPServerConfig{Name: "stdio", Type: "stdio", Command: "/usr/bin/false"}
	stdioInventory := codexMCPInventoryEntry{Name: stdio.Name, Enabled: true}
	stdioInventory.Transport.Type = "stdio"
	stdioInventory.Transport.Command = stdio.Command
	stdioInventory.Transport.HTTPHeadersHelper = &helper
	if err := compareInteractiveMCPEntry(stdio, stdioInventory); err == nil {
		t.Fatal("protected HTTP helper on stdio transport was accepted")
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
