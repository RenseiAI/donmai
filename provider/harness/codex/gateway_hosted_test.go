package codex

import (
	"os"
	"strings"
	"testing"
)

// TestGatewayProviderBlock_DisablesHostedTools pins that a gateway-routed
// Codex session never offers hosted tools: the isolated config turns
// web_search off so it is never sent. tool_search has no Codex off switch in
// the pinned CLI (Removed no-op, always enabled), so its enforcement is the
// gateway's typed refusal — verified in gateway/translate.
func TestGatewayProviderBlock_DisablesHostedTools(t *testing.T) {
	t.Parallel()
	boundary, err := newCodexConfigBoundary(t.TempDir(), false)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	if _, err := boundary.appendGatewayProviderBlock("https://gateway.example/codex/v1", "k", map[string]string{}); err != nil {
		t.Fatalf("append block: %v", err)
	}
	body, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(body), `web_search = "disabled"`) {
		t.Fatalf("gateway config must disable web_search so it is never sent:\n%s", body)
	}
}

// TestGatewayProviderOverride_DisablesWebSearch pins the CLI-layer restatement
// of the same off switch: a repo-checked-in web_search stanza cannot re-enable
// a hosted tool the gateway would have to refuse.
func TestGatewayProviderOverride_DisablesWebSearch(t *testing.T) {
	t.Parallel()
	args := gatewayProviderConfigOverride("https://gateway.example/codex/v1")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, `web_search=`) || !strings.Contains(joined, `disabled`) {
		t.Fatalf("gateway overrides must disable web_search so it is never sent: %q", args)
	}
}
