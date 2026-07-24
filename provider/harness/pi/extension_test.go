package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestHandshakeSHA_Verification is the cryptographic core of smoke 1
// (tampered-extension fixture): only the exact embedded-extension SHA
// verifies; a wrong or empty SHA fails closed.
func TestHandshakeSHA_Verification(t *testing.T) {
	t.Parallel()
	realSHA := extensionSHA()
	if realSHA == "" || len(realSHA) != 64 {
		t.Fatalf("extensionSHA() = %q, want a 64-char hex sha256", realSHA)
	}
	if !verifyHandshakeSHA(realSHA) {
		t.Errorf("verifyHandshakeSHA(realSHA) = false, want true")
	}
	for _, bad := range []string{"", "deadbeef", strings.Repeat("0", 64), realSHA[:63] + "f"} {
		if verifyHandshakeSHA(bad) {
			t.Errorf("verifyHandshakeSHA(%q) = true, want false (must fail closed)", bad)
		}
	}
}

// TestMaterializeExtension writes the boundary payload with the right modes and
// content: the extension file matches the embedded SHA, models.json pins the
// provider with an env-REFERENCE (never the secret), and settings deny
// cycle_model (design §5.2/§6).
func TestMaterializeExtension(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	ep := &agent.EndpointBinding{
		Company:  agent.CompanyAnthropic,
		Model:    "claude-x",
		BaseURL:  "https://api.anthropic.com",
		Protocol: agent.ProtoAnthropicMessages,
		Host:     agent.HostDirect,
		Env:      map[string]string{"ANTHROPIC_API_KEY": "sk-super-secret-canary"},
	}
	layout, err := materializeExtension(cwd, ep, "claude-x")
	if err != nil {
		t.Fatalf("materializeExtension: %v", err)
	}

	// Extension file present, 0600, and byte-identical to the embed (so its
	// SHA matches — a materialized copy that drifts would fail the handshake).
	data, err := os.ReadFile(layout.extension)
	if err != nil {
		t.Fatalf("read extension: %v", err)
	}
	if string(data) != string(extensionSource()) {
		t.Errorf("materialized extension differs from embedded source")
	}
	assertMode0600(t, layout.extension)
	assertMode0600(t, layout.settings)
	assertMode0600(t, layout.modelsJSON)

	// models.json pins the provider via an env reference — the secret VALUE
	// must never be on disk (design §6, §5.3).
	models, _ := os.ReadFile(layout.modelsJSON)
	if strings.Contains(string(models), "sk-super-secret-canary") {
		t.Errorf("provider-pin config leaked the API key onto disk: %s", models)
	}
	var mj map[string]any
	if err := json.Unmarshal(models, &mj); err != nil {
		t.Fatalf("models.json is not valid JSON: %v", err)
	}
	if got, _ := mj["model"].(string); got != "donmai/claude-x" {
		t.Errorf("models.json model = %q, want donmai/claude-x", got)
	}

	// settings deny cycle_model so the session cannot wander off the cell.
	settings, _ := os.ReadFile(layout.settings)
	if !strings.Contains(string(settings), "cycle_model") {
		t.Errorf("settings must deny cycle_model, got: %s", settings)
	}
}

func assertMode0600(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s mode = %o, want 600", filepath.Base(path), perm)
	}
}
