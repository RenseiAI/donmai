package afcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/RenseiAI/donmai/afclient"
	daemonRuntime "github.com/RenseiAI/donmai/daemon"
)

// TestDaemonSetSessionScratchDerive pins the CLI half of the declared
// sessionScratch.derive setting: `host set sessionScratch.derive 0|1`
// states the value explicitly in daemon.yaml, preserves every other key
// (including one the daemon's own model does not declare), reads back
// through the daemon's own config loader, notifies the running daemon, and
// refuses anything but 0 or 1 without touching the file.
//
// RED: skip the daemon.yaml write and the loader still reads the default.
func TestDaemonSetSessionScratchDerive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	fixture := "machine: {id: scratch-fixture}\norchestrator: {url: https://example.test}\nembedderOwned: {keep: me}\n"
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value      string
		wantDerive bool
		wantYAML   bool
	}{
		{value: "0", wantDerive: false, wantYAML: false},
		{value: "1", wantDerive: true, wantYAML: true},
	} {
		mock := &mockDaemon{setCapResp: &afclient.SetCapacityResponse{OK: true, Key: daemonRuntime.SessionScratchDeriveKey, Value: tc.value}}
		out, err := newTestHostCmd(mock, []string{"set", daemonRuntime.SessionScratchDeriveKey, tc.value, "--config", path})
		if err != nil {
			t.Fatalf("set %s=%s: %v", daemonRuntime.SessionScratchDeriveKey, tc.value, err)
		}
		if !strings.Contains(out.String(), "restart") {
			t.Errorf("set %s=%s output %q does not say a restart is needed", daemonRuntime.SessionScratchDeriveKey, tc.value, out.String())
		}
		if mock.setCapKey != daemonRuntime.SessionScratchDeriveKey || mock.setCapValue != tc.value {
			t.Fatalf("running daemon notified with %s=%s, want %s=%s", mock.setCapKey, mock.setCapValue, daemonRuntime.SessionScratchDeriveKey, tc.value)
		}
		body, err := os.ReadFile(path) //nolint:gosec // G304: a t.TempDir fixture.
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		block, ok := doc["sessionScratch"].(map[string]any)
		if !ok || block["derive"] != tc.wantYAML {
			t.Fatalf("daemon.yaml after set %s: sessionScratch = %#v, want derive: %v stated explicitly\n%s", tc.value, doc["sessionScratch"], tc.wantYAML, body)
		}
		if kept, ok := doc["embedderOwned"].(map[string]any); !ok || kept["keep"] != "me" {
			t.Fatalf("set dropped a key the daemon model does not declare:\n%s", body)
		}
		cfg, err := daemonRuntime.LoadConfig(path)
		if err != nil {
			t.Fatalf("daemon loader: %v", err)
		}
		if cfg == nil || cfg.SessionScratch.DeriveEnabled() != tc.wantDerive {
			t.Fatalf("daemon loader reads derive=%v after set %s, want %v", cfg != nil && cfg.SessionScratch.DeriveEnabled(), tc.value, tc.wantDerive)
		}
	}

	before, err := os.ReadFile(path) //nolint:gosec // G304: a t.TempDir fixture.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newTestHostCmd(&mockDaemon{}, []string{"set", daemonRuntime.SessionScratchDeriveKey, "yes", "--config", path}); err == nil || !strings.Contains(err.Error(), "0 or 1") {
		t.Fatalf("set %s=yes = %v, want a refusal naming 0 or 1", daemonRuntime.SessionScratchDeriveKey, err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // G304: a t.TempDir fixture.
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("a refused set rewrote daemon.yaml:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
