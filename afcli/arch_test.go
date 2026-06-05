package afcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// fakeArchBin writes a shell script that echoes JSON describing the invocation,
// installs it as DONMAI_ARCH_BIN, and returns the path.
func fakeArchBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "donmai-arch")
	content := `#!/bin/sh
printf '{"command":"%s","argv":"%s","gated":false}' "$1" "$*"
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil { //nolint:gosec // #nosec G306 -- test fake binary; needs owner exec bit
		t.Fatalf("write fake donmai-arch: %v", err)
	}
	t.Setenv("DONMAI_ARCH_BIN", script)
	return script
}

// execArchCmd builds a fresh `donmai arch <subArgs>` command tree with a fake
// binary and runs it, capturing stdout as a decoded JSON map.
func execArchCmd(t *testing.T, subArgs ...string) (map[string]any, error) {
	t.Helper()
	fakeArchBin(t)

	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newArchCmd(Config{}))

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
		"--db", ".donmai/arch-intelligence/db.sqlite",
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

// ── Native fallback (no binary) ───────────────────────────────────────────────

// TestArchAssess_NativeFallback verifies that `donmai arch assess` works even
// without DONMAI_ARCH_BIN or donmai-arch on PATH, using the native diff/gate path.
func TestArchAssess_NativeFallback(t *testing.T) {
	t.Setenv("DONMAI_ARCH_BIN", "")
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir()) // empty PATH — no donmai-arch found
	defer func() { _ = os.Setenv("PATH", origPath) }()

	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newArchCmd(Config{}))

	oldOut := os.Stdout
	oldErr := os.Stderr
	r, w, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = w
	os.Stderr = wErr

	root.SetArgs([]string{"arch", "assess", "https://github.com/org/repo/pull/5"})
	err := root.Execute()

	_ = w.Close()
	_ = wErr.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr

	var outBuf, errBuf bytes.Buffer
	_, _ = outBuf.ReadFrom(r)
	_, _ = errBuf.ReadFrom(rErr)

	if err != nil {
		t.Fatalf("native fallback should not error; got %v\nstdout: %s\nstderr: %s",
			err, outBuf.String(), errBuf.String())
	}

	var m map[string]any
	if jsonErr := json.Unmarshal(outBuf.Bytes(), &m); jsonErr != nil {
		t.Fatalf("native fallback output not JSON: %v (raw: %q)", jsonErr, outBuf.String())
	}

	// Must have mode = "native-diff-only"
	if mode, _ := m["mode"].(string); mode != "native-diff-only" {
		t.Errorf("expected mode 'native-diff-only', got %q", mode)
	}

	// Must have "gated" field (bool).
	if _, ok := m["gated"]; !ok {
		t.Error("native fallback output missing 'gated' field")
	}

	// stderr must contain the "notice:" informational message.
	if !strings.Contains(errBuf.String(), "notice:") {
		t.Errorf("expected notice in stderr; got: %q", errBuf.String())
	}
}

// ── Command tree structure ────────────────────────────────────────────────────

func TestArchCmd_SubcommandsExist(t *testing.T) {
	root := newArchCmd(Config{})
	names := make(map[string]bool)
	for _, sub := range root.Commands() {
		names[sub.Name()] = true
	}
	if !names["assess"] {
		t.Error("expected 'assess' subcommand to exist")
	}
}

func TestCodeCmd_SubcommandsExist(t *testing.T) {
	root := newCodeCmd(Config{})
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
