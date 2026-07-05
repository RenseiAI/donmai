package afcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestMCPCmd_HiddenAndWired(t *testing.T) {
	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newMCPCmd(Config{}))

	mcp := findSub(root, "mcp")
	if mcp == nil {
		t.Fatal("mcp command not registered")
	}
	if !mcp.Hidden {
		t.Error("mcp command group should be hidden")
	}

	ci := findSub(mcp, "code-intel")
	if ci == nil {
		t.Fatal("code-intel subcommand not registered")
	}
	if !ci.Hidden {
		t.Error("code-intel command should be hidden")
	}
	for _, f := range []string{"root", "repo-path", "tools"} {
		if ci.Flags().Lookup(f) == nil {
			t.Errorf("code-intel missing --%s flag", f)
		}
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
