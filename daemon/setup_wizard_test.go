package daemon

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSetupWizard_NonTTY_ReturnsDefault(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "daemon.yaml")
	tru := false
	cfg, err := RunSetupWizard(WizardOptions{
		ConfigPath: cfgPath,
		IsTTY:      &tru,
	})
	if err != nil {
		t.Fatalf("RunSetupWizard: %v", err)
	}
	if cfg.Machine.ID == "" {
		t.Errorf("expected default machine id")
	}
	if cfg.Capacity.MaxConcurrentSessions == 0 {
		t.Errorf("expected default max sessions > 0")
	}
}

func TestRunSetupWizard_Interactive_HappyPath(t *testing.T) {
	tru := true
	in := strings.NewReader(strings.Join([]string{
		"my-machine", // Machine ID
		"home-net",   // Region
		"y",          // Continue?
		"2",          // Reserve cores
		"1024",       // Reserve memory
		"4",          // Max sessions
		"y",          // Continue?
		"3",          // Choice 3 — local file queue
		"y",          // Continue?
		"n",          // Add another project? (no detected remote in test)
		"y",          // Continue?
		"stable",     // Channel
		"manual",     // Schedule
		"30",         // Drain timeout
	}, "\n") + "\n")
	var out bytes.Buffer

	cfgPath := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg, err := RunSetupWizard(WizardOptions{
		ConfigPath:      cfgPath,
		Stdin:           in,
		Stdout:          &out,
		IsTTY:           &tru,
		CPUCount:        4,
		MemoryMB:        8192,
		DetectGitRemote: func() string { return "" },
	})
	if err != nil {
		t.Fatalf("RunSetupWizard: %v", err)
	}
	if cfg.Machine.ID != "my-machine" {
		t.Errorf("MachineID = %q", cfg.Machine.ID)
	}
	if cfg.Machine.Region != "home-net" {
		t.Errorf("Region = %q", cfg.Machine.Region)
	}
	if cfg.Capacity.MaxConcurrentSessions != 4 {
		t.Errorf("MaxConcurrentSessions = %d", cfg.Capacity.MaxConcurrentSessions)
	}
	if !strings.HasPrefix(cfg.Orchestrator.URL, "file://") {
		t.Errorf("expected file:// URL, got %q", cfg.Orchestrator.URL)
	}
	if cfg.AutoUpdate.Channel != ChannelStable {
		t.Errorf("Channel = %q", cfg.AutoUpdate.Channel)
	}
	if cfg.AutoUpdate.Schedule != ScheduleManual {
		t.Errorf("Schedule = %q", cfg.AutoUpdate.Schedule)
	}
	if cfg.AutoUpdate.DrainTimeoutSeconds != 30 {
		t.Errorf("DrainTimeoutSeconds = %d", cfg.AutoUpdate.DrainTimeoutSeconds)
	}
}

func TestRemoteToRepository(t *testing.T) {
	cases := []struct{ in, want string }{
		{"git@github.com:foo/bar.git", "github.com/foo/bar"},
		{"https://github.com/foo/bar.git", "github.com/foo/bar"},
		{"https://github.com/foo/bar", "github.com/foo/bar"},
	}
	for _, c := range cases {
		if got := remoteToRepository(c.in); got != c.want {
			t.Errorf("remoteToRepository(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
