package afclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonYAMLAdmissionModeRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")

	// Start from a file with unrelated operator-owned keys, so the splice path
	// (not the fresh-marshal path) is what gets exercised — the CLI must never
	// clobber machine identity or the orchestrator token while editing consent.
	seed := "" +
		"apiVersion: donmai.dev/v1\n" +
		"kind: LocalDaemon\n" +
		"machine:\n" +
		"  id: machine-1\n" +
		"orchestrator:\n" +
		"  url: https://example.test\n" +
		"  authToken: secret-token\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	cfg, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML: %v", err)
	}
	if cfg.AdmitsAnyRoutedProject() {
		t.Fatal("a config with no admission opinion reported all-routed consent")
	}

	cfg.SetProjectAdmissionMode(ProjectAdmissionModeAllRouted)
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "projectAdmissionMode: all-routed") {
		t.Fatalf("mode not written:\n%s", got)
	}
	if !strings.Contains(got, "authToken: secret-token") || !strings.Contains(got, "id: machine-1") {
		t.Fatalf("splice dropped operator-owned keys:\n%s", got)
	}

	reloaded, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML(reload): %v", err)
	}
	if !reloaded.AdmitsAnyRoutedProject() {
		t.Fatal("reloaded config lost its all-routed consent")
	}

	// Opting back out must REMOVE the key, not leave a stale grant behind.
	reloaded.SetProjectAdmissionMode(ProjectAdmissionModeEnumerated)
	if err := WriteDaemonYAML(path, reloaded); err != nil {
		t.Fatalf("WriteDaemonYAML(opt out): %v", err)
	}
	raw, err = os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("ReadFile(opt out): %v", err)
	}
	if strings.Contains(string(raw), "projectAdmissionMode") {
		t.Fatalf("withdrawing consent left the key behind:\n%s", raw)
	}
	final, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML(final): %v", err)
	}
	if final.AdmitsAnyRoutedProject() {
		t.Fatal("consent survived being withdrawn")
	}
}

func TestNormalizeProjectAdmissionModeFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want string
	}{
		{raw: "", want: ProjectAdmissionModeEnumerated},
		{raw: "enumerated", want: ProjectAdmissionModeEnumerated},
		{raw: "all-routed", want: ProjectAdmissionModeAllRouted},
		{raw: "  All-Routed ", want: ProjectAdmissionModeAllRouted},
		{raw: "all_routed", want: ProjectAdmissionModeEnumerated},
		{raw: "allrouted", want: ProjectAdmissionModeEnumerated},
	}
	for _, tc := range tests {
		if got := NormalizeProjectAdmissionMode(tc.raw); got != tc.want {
			t.Errorf("NormalizeProjectAdmissionMode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
