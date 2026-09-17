package agent

import (
	"encoding/json"
	"testing"
)

func TestProtectedRuntimeMCPHeadersHelperIsPrivateAndNormalized(t *testing.T) {
	base := MCPServerConfig{Name: "protected", Type: "http", URL: "https://example.test/mcp"}
	got, err := WithProtectedRuntimeMCPHeadersHelper(base, "'/bin/donmai' mcp gateway-headers --token-file '/tmp/token'")
	if err != nil {
		t.Fatal(err)
	}
	if command, ok := ProtectedRuntimeMCPHeadersHelper(got); !ok || command == "" {
		t.Fatal("helper missing")
	}
	if _, ok := ProtectedRuntimeMCPHeadersHelper(base); ok {
		t.Fatal("constructor mutated input")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var replay MCPServerConfig
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if _, ok := ProtectedRuntimeMCPHeadersHelper(replay); ok {
		t.Fatalf("helper survived JSON: %s", raw)
	}
	if _, ok := ProtectedRuntimeMCPHeadersHelper(normalizeRuntimeMCPServer(got)); ok {
		t.Fatal("helper survived normalization")
	}
}

func TestProtectedRuntimeMCPHeadersHelperRefusesConflicts(t *testing.T) {
	tests := []MCPServerConfig{
		{Name: "stdio", Type: "stdio", Command: "x"},
		{Name: "missing-url", Type: "http"},
		{Name: "auth", Type: "http", URL: "https://example.test", Headers: map[string]string{"authorization": "Bearer x"}},
	}
	for _, server := range tests {
		if _, err := WithProtectedRuntimeMCPHeadersHelper(server, "helper"); err == nil {
			t.Fatalf("%s succeeded", server.Name)
		}
	}
}
