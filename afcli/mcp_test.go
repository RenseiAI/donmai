package afcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
	"github.com/spf13/cobra"
)

// findSub returns the named direct subcommand or nil.
func findSub(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func TestMCPCmd_DocumentedAndWired(t *testing.T) {
	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newMCPCmd(Config{}))

	mcp := findSub(root, "mcp")
	if mcp == nil {
		t.Fatal("mcp command not registered")
	}
	if mcp.Hidden {
		t.Error("mcp command group should be visible")
	}
	if !strings.Contains(mcp.Long, "docs/CODE-INTELLIGENCE-MCP-CONTRACT.md") {
		t.Errorf("mcp command help should cite the contract doc, got: %q", mcp.Long)
	}

	ci := findSub(mcp, "code-intel")
	if ci == nil {
		t.Fatal("code-intel subcommand not registered")
	}
	if ci.Hidden {
		t.Error("code-intel command should be visible")
	}
	if !strings.Contains(ci.Long, "docs/CODE-INTELLIGENCE-MCP-CONTRACT.md") {
		t.Errorf("code-intel command help should cite the contract doc, got: %q", ci.Long)
	}
	for _, f := range []string{"root", "repo-path", "tools", "verify"} {
		if ci.Flags().Lookup(f) == nil {
			t.Errorf("code-intel missing --%s flag", f)
		}
	}
}

func TestMCPCodeIntel_Verify(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n\ntype Widget struct{}\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var out, errBuf bytes.Buffer
	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newMCPCmd(Config{}))
	root.SetArgs([]string{"mcp", "code-intel", "--root", dir, "--verify"})
	root.SetOut(&out)
	root.SetErr(&errBuf)

	if err := root.Execute(); err != nil {
		t.Fatalf("verify returned error: %v (stderr: %s)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "Verified") || !strings.Contains(out.String(), "called 6 tools") {
		t.Fatalf("verify stdout = %q, want a six-tool call verification summary", out.String())
	}
}

type brokenMCPServer struct{}

func (brokenMCPServer) Serve(_ context.Context, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &request); err != nil {
			return err
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]string{"name": mcpserver.ServerName}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]string{
				{"name": mcpserver.ToolGetRepoMap},
				{"name": mcpserver.ToolSearchSymbols},
				{"name": mcpserver.ToolSearchCode},
				{"name": mcpserver.ToolCheckDuplicate},
				{"name": mcpserver.ToolFindTypeUsages},
				{"name": mcpserver.ToolValidateCrossDeps},
			}}
		case "tools/call":
			result = map[string]any{
				"content": []map[string]string{{"type": "text", "text": "fixture"}},
				"isError": request.Params.Name == mcpserver.ToolSearchCode,
			}
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			return err
		}
	}
	return sc.Err()
}

func TestMCPCodeIntel_VerifyDetectsBrokenTool(t *testing.T) {
	var out bytes.Buffer
	err := verifyMCPCodeIntel(context.Background(), &out, brokenMCPServer{})
	if err == nil || !strings.Contains(err.Error(), mcpserver.ToolSearchCode) {
		t.Fatalf("verify error = %v, want failing tool %q", err, mcpserver.ToolSearchCode)
	}
}

func TestMCPCodeIntel_MissingRootErrors(t *testing.T) {
	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newMCPCmd(Config{}))
	root.SetArgs([]string{"mcp", "code-intel"})
	root.SetIn(strings.NewReader(""))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})

	err := root.Execute()
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "root") {
		t.Fatalf("missing --root should error mentioning root, got %v", err)
	}
}

func TestMCPCodeIntel_InvalidRootErrors(t *testing.T) {
	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newMCPCmd(Config{}))
	root.SetArgs([]string{"mcp", "code-intel", "--root", "relative/not/absolute"})
	root.SetIn(strings.NewReader(""))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative --root should error mentioning absolute, got %v", err)
	}
}

func TestMCPCodeIntel_Serves(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"),
		[]byte("package x\n\n// Widget is a thing.\ntype Widget struct{}\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// initialize, then a tools/call (which blocks until warm-up completes, so
	// the async index write finishes before EOF and TempDir cleanup), then EOF.
	stdin := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"af_code_search_symbols","arguments":{"query":"Widget"}}}`,
		"",
	}, "\n")

	var out, errBuf bytes.Buffer
	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newMCPCmd(Config{}))
	root.SetArgs([]string{"mcp", "code-intel", "--root", dir})
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&errBuf)

	if err := root.Execute(); err != nil {
		t.Fatalf("serve returned error: %v (stderr: %s)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "af_code") && !strings.Contains(out.String(), "Widget") {
		t.Fatalf("expected a tool result on stdout, got: %s", out.String())
	}
	// Protocol output must not leak onto stderr; warm-up logging is fine there.
	if strings.Contains(errBuf.String(), "jsonrpc") {
		t.Fatalf("JSON-RPC leaked to stderr: %s", errBuf.String())
	}
}
