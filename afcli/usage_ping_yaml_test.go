package afcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeMinimalDaemonYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readUsagePingBlock(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	out := map[string]string{}
	block := findMappingValue(root, "usagePing")
	if block == nil {
		return out
	}
	for i := 0; i+1 < len(block.Content); i += 2 {
		if block.Content[i].Kind == yaml.ScalarNode {
			out[block.Content[i].Value] = block.Content[i+1].Value
		}
	}
	return out
}

func TestApplyUsagePingSet_EnableMintsID(t *testing.T) {
	path := writeMinimalDaemonYAML(t, "machine:\n  id: test\norchestrator:\n  url: https://example.invalid\n")
	msg, err := applyUsagePingSet(path, "usagePing.enabled", "1")
	if err != nil {
		t.Fatalf("applyUsagePingSet: %v", err)
	}
	if !strings.Contains(msg, "weekly") {
		t.Fatalf("message = %q", msg)
	}
	block := readUsagePingBlock(t, path)
	if block["enabled"] != "true" {
		t.Fatalf("enabled = %q", block["enabled"])
	}
	if !validUsagePingID(block["installationId"]) {
		t.Fatalf("installationId = %q", block["installationId"])
	}
	// Enabling again keeps the same id (no churn).
	msg2, err := applyUsagePingSet(path, "usagePing.enabled", "1")
	if err != nil {
		t.Fatal(err)
	}
	_ = msg2
	if block2 := readUsagePingBlock(t, path); block2["installationId"] != block["installationId"] {
		t.Fatalf("re-enable minted a new id: %q -> %q", block["installationId"], block2["installationId"])
	}
}

func TestApplyUsagePingSet_DisableClearsBlock(t *testing.T) {
	path := writeMinimalDaemonYAML(t, "machine:\n  id: test\nusagePing:\n  enabled: true\n  installationId: 0123456789abcdef0123456789abcdef\n  endpoint: https://example.invalid/usage\n")
	msg, err := applyUsagePingSet(path, "usagePing.enabled", "0")
	if err != nil {
		t.Fatalf("applyUsagePingSet: %v", err)
	}
	if !strings.Contains(msg, "no ping will be sent") {
		t.Fatalf("message = %q", msg)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "usagePing") {
		t.Fatalf("block survived disable:\n%s", data)
	}
	if !strings.Contains(string(data), "machine:") {
		t.Fatalf("unrelated keys lost:\n%s", data)
	}
}

func TestApplyUsagePingSet_EndpointValidation(t *testing.T) {
	path := writeMinimalDaemonYAML(t, "machine:\n  id: test\n")
	if _, err := applyUsagePingSet(path, "usagePing.endpoint", "http://example.invalid"); err == nil {
		t.Fatal("non-https endpoint accepted")
	}
	if _, err := applyUsagePingSet(path, "usagePing.endpoint", "https://example.invalid/usage"); err != nil {
		t.Fatalf("https endpoint: %v", err)
	}
	if block := readUsagePingBlock(t, path); block["endpoint"] != "https://example.invalid/usage" {
		t.Fatalf("endpoint = %q", block["endpoint"])
	}
	if _, err := applyUsagePingSet(path, "usagePing.endpoint", ""); err != nil {
		t.Fatalf("clear endpoint: %v", err)
	}
	if block := readUsagePingBlock(t, path); block["endpoint"] != "" {
		t.Fatalf("endpoint not cleared: %q", block["endpoint"])
	}
	if _, err := applyUsagePingSet(path, "usagePing.enabled", "2"); err == nil {
		t.Fatal("value 2 accepted for boolean")
	}
	if _, err := applyUsagePingSet(path, "usagePing.bogus", "1"); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestApplyUsagePingSet_PreservesUnknownKeys(t *testing.T) {
	path := writeMinimalDaemonYAML(t, "machine:\n  id: test\ncustomBlock:\n  keep: me\n")
	if _, err := applyUsagePingSet(path, "usagePing.enabled", "1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "keep: me") {
		t.Fatalf("unknown keys lost:\n%s", data)
	}
}
