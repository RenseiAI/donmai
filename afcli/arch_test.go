package afcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// fakeArchBin writes a shell script that echoes JSON describing the invocation,
// installs it as AGENTFACTORY_ARCH_BIN, and returns the path.
func fakeArchBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "af-arch")
	content := `#!/bin/sh
printf '{"command":"%s","argv":"%s","gated":false}' "$1" "$*"
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil { //nolint:gosec // #nosec G306 -- test fake binary; needs owner exec bit
		t.Fatalf("write fake af-arch: %v", err)
	}
	t.Setenv("AGENTFACTORY_ARCH_BIN", script)
	return script
}

// execArchCmd builds a fresh `af arch <subArgs>` command tree with a fake
// binary and runs it, capturing stdout as a decoded JSON map.
func execArchCmd(t *testing.T, subArgs ...string) (map[string]any, error) {
	t.Helper()
	fakeArchBin(t)

	root := &cobra.Command{Use: "af", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newArchCmd())

	oldOut := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	args := append([]string{"arch"}, subArgs...)
	root.SetArgs(args)
	err := root.Execute()

	_ = w.Close()
	os.Stdout = oldOut

	var out bytes.Buffer
	if _, readErr := out.ReadFrom(r); readErr != nil {
		t.Fatalf("read stdout pipe: %v", readErr)
	}

	if err != nil {
		return nil, err
	}

	var m map[string]any
	if jsonErr := json.Unmarshal(out.Bytes(), &m); jsonErr != nil {
		return nil, fmt.Errorf("parse stdout JSON: %w (raw: %q)", jsonErr, out.String())
	}
	return m, nil
}

// ── assess ────────────────────────────────────────────────────────────────────

func TestArchAssess_WithPrURL(t *testing.T) {
	m, err := execArchCmd(t, "assess", "https://github.com/org/repo/pull/123")
	if err != nil {
		t.Fatalf("arch assess: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "assess") {
		t.Errorf("argv %q missing assess subcommand", argv)
	}
	if !strings.Contains(argv, "https://github.com/org/repo/pull/123") {
		t.Errorf("argv %q missing PR URL", argv)
	}
}

func TestArchAssess_WithRepoAndPR(t *testing.T) {
	m, err := execArchCmd(t, "assess",
		"--repository", "github.com/org/repo",
		"--pr", "42",
	)
	if err != nil {
		t.Fatalf("arch assess --repository --pr: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "--repository") {
		t.Errorf("argv %q missing --repository", argv)
	}
	if !strings.Contains(argv, "--pr") || !strings.Contains(argv, "42") {
		t.Errorf("argv %q missing --pr 42", argv)
	}
}

func TestArchAssess_WithGatePolicy(t *testing.T) {
	m, err := execArchCmd(t, "assess",
		"https://github.com/org/repo/pull/1",
		"--gate-policy", "zero-deviations",
	)
	if err != nil {
		t.Fatalf("arch assess --gate-policy: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "--gate-policy") || !strings.Contains(argv, "zero-deviations") {
		t.Errorf("argv %q missing --gate-policy zero-deviations", argv)
	}
}

func TestArchAssess_WithScopeLevel(t *testing.T) {
	m, err := execArchCmd(t, "assess",
		"https://github.com/org/repo/pull/1",
		"--scope-level", "org",
	)
	if err != nil {
		t.Fatalf("arch assess --scope-level: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "--scope-level") || !strings.Contains(argv, "org") {
		t.Errorf("argv %q missing --scope-level org", argv)
	}
}

func TestArchAssess_WithDB(t *testing.T) {
	m, err := execArchCmd(t, "assess",
		"https://github.com/org/repo/pull/1",
		"--db", "/tmp/test.sqlite",
	)
	if err != nil {
		t.Fatalf("arch assess --db: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "--db") || !strings.Contains(argv, "/tmp/test.sqlite") {
		t.Errorf("argv %q missing --db /tmp/test.sqlite", argv)
	}
}

func TestArchAssess_AllFlags(t *testing.T) {
	m, err := execArchCmd(t, "assess",
		"https://github.com/org/repo/pull/99",
		"--repository", "github.com/org/repo",
		"--pr", "99",
		"--gate-policy", "max:5",
		"--scope-level", "tenant",
		"--project-id", "proj-123",
		"--db", ".agentfactory/arch-intelligence/db.sqlite",
	)
	if err != nil {
		t.Fatalf("arch assess all flags: %v", err)
	}
	argv, _ := m["argv"].(string)
	for _, want := range []string{
		"--repository", "github.com/org/repo",
		"--pr", "99",
		"--gate-policy", "max:5",
		"--scope-level", "tenant",
		"--project-id", "proj-123",
		"--db",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q missing %q", argv, want)
		}
	}
}

// ── Unavailable binary ────────────────────────────────────────────────────────

func TestArchCmd_UnavailableBinary(t *testing.T) {
	t.Setenv("AGENTFACTORY_ARCH_BIN", "")
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	defer func() { _ = os.Setenv("PATH", origPath) }()

	if _, err := exec.LookPath("pnpm"); err == nil {
		t.Skip("pnpm found in PATH; cannot test unavailable binary path")
	}

	root := &cobra.Command{Use: "af", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newArchCmd())
	root.SetArgs([]string{"arch", "assess", "https://github.com/org/repo/pull/1"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when af-arch binary is not available")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "af-arch") {
		t.Errorf("expected 'not found' or 'af-arch' in error, got: %v", err)
	}
}

// ── Command tree structure ────────────────────────────────────────────────────

func TestArchCmd_SubcommandsExist(t *testing.T) {
	root := newArchCmd()
	names := make(map[string]bool)
	for _, sub := range root.Commands() {
		names[sub.Name()] = true
	}
	if !names["assess"] {
		t.Error("expected 'assess' subcommand to exist")
	}
}

func TestCodeCmd_SubcommandsExist(t *testing.T) {
	root := newCodeCmd()
	names := make(map[string]bool)
	for _, sub := range root.Commands() {
		names[sub.Name()] = true
	}
	for _, want := range []string{
		"get-repo-map", "search-symbols", "search-code",
		"check-duplicate", "find-type-usages", "validate-cross-deps",
	} {
		if !names[want] {
			t.Errorf("expected subcommand %q to exist, got %v", want, names)
		}
	}
}
