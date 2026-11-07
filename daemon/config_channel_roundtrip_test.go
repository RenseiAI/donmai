package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression: an external caller builds on DefaultConfig, assigns a
// non-default channel through the exported Go API, and writes it. The
// write must preserve the operator's local channel selection; reloading
// must report it back.
func TestPublicChannelRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoUpdate.Channel = ChannelBeta
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if reloaded.AutoUpdate.Channel != ChannelBeta {
		t.Fatalf("explicit channel lost: wrote beta, reloaded %q", reloaded.AutoUpdate.Channel)
	}
}

// An explicit stable set through the exported setter must survive the
// round-trip as an authored local override (masking any system value),
// while a default-filled stable from DefaultConfig must stay omitted so
// a later system snapshot can win.
func TestExplicitStableSetterRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg := DefaultConfig()
	cfg.SetAutoUpdateChannel(ChannelStable)
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "stable") {
		t.Fatalf("explicit stable was not persisted in %s", path)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reloaded.AutoUpdateChannelAuthored() {
		t.Fatalf("explicit stable lost its authored mark after reload")
	}

	fresh := DefaultConfig()
	freshPath := filepath.Join(t.TempDir(), "fresh.yaml")
	if err := WriteConfig(freshPath, fresh); err != nil {
		t.Fatalf("WriteConfig(fresh): %v", err)
	}
	freshRaw, err := os.ReadFile(freshPath)
	if err != nil {
		t.Fatalf("ReadFile(fresh): %v", err)
	}
	if strings.Contains(string(freshRaw), "channel") {
		t.Fatalf("default-filled channel must stay omitted so the system layer can win, got:\n%s", freshRaw)
	}
	freshReloaded, err := LoadConfig(freshPath)
	if err != nil {
		t.Fatalf("LoadConfig(fresh): %v", err)
	}
	if freshReloaded.AutoUpdateChannelAuthored() {
		t.Fatalf("default-filled channel must not count as authored after reload")
	}
}
