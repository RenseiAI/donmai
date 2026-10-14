package daemon

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/usageping"
)

func usagePingIDValidForTest(id string) bool { return usageping.ValidateInstallationID(id) }

func usagePingAnswers(suffix ...string) string {
	// Base answers through auto-update, then "y" Continue, then the
	// usage-count answers.
	base := []string{
		"my-machine", "home-net", "y", // machine
		"2", "1024", "4", "y", // capacity
		"1", "https://example.invalid", "synthetic-token", "y", // remote orchestrator
		"n", "n", "y", // projects: no add, enumerated, continue
		"stable", "manual", "30", // auto-update
		"y", // auto-update continue
	}
	return strings.Join(append(base, suffix...), "\n") + "\n"
}

func TestRunSetupWizard_UsagePingDefaultNo(t *testing.T) {
	tru := true
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	var out bytes.Buffer
	// Answer "n" to the usage-count question: default-no leaves the
	// ping disabled with no id and no endpoint.
	cfg, err := RunSetupWizard(WizardOptions{
		ConfigPath:      path,
		Stdin:           strings.NewReader(usagePingAnswers("n")),
		Stdout:          &out,
		IsTTY:           &tru,
		CPUCount:        4,
		MemoryMB:        8192,
		DetectGitRemote: func() string { return "" },
	})
	if err != nil {
		t.Fatalf("RunSetupWizard: %v", err)
	}
	if cfg.UsagePing.Enabled || cfg.UsagePing.InstallationID != "" || cfg.UsagePing.Endpoint != "" {
		t.Fatalf("default-no answer left ping state: %+v", cfg.UsagePing)
	}
	if cfg.UsagePingEnabled() {
		t.Fatal("disabled ping reports enabled")
	}
	if !strings.Contains(out.String(), "Count this installation in the public usage numbers?") {
		t.Fatalf("setup did not ask the usage-count question:\n%s", out.String())
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UsagePing.Enabled || loaded.UsagePing.InstallationID != "" {
		t.Fatalf("written config kept ping state: %+v", loaded.UsagePing)
	}
}

func TestRunSetupWizard_UsagePingOptInMintsID(t *testing.T) {
	tru := true
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	var out bytes.Buffer
	cfg, err := RunSetupWizard(WizardOptions{
		ConfigPath:      path,
		Stdin:           strings.NewReader(usagePingAnswers("y", "https://example.invalid/usage")),
		Stdout:          &out,
		IsTTY:           &tru,
		CPUCount:        4,
		MemoryMB:        8192,
		DetectGitRemote: func() string { return "" },
	})
	if err != nil {
		t.Fatalf("RunSetupWizard: %v", err)
	}
	if !cfg.UsagePing.Enabled {
		t.Fatal("opt-in answer left ping disabled")
	}
	if !usagePingIDValidForTest(cfg.UsagePing.InstallationID) {
		t.Fatalf("installation id not random hex: %q", cfg.UsagePing.InstallationID)
	}
	if cfg.UsagePing.Endpoint != "https://example.invalid/usage" {
		t.Fatalf("endpoint = %q", cfg.UsagePing.Endpoint)
	}
	if !cfg.UsagePingEnabled() {
		t.Fatal("opted-in ping reports disabled")
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UsagePing != cfg.UsagePing {
		t.Fatalf("written ping = %+v, want %+v", loaded.UsagePing, cfg.UsagePing)
	}
}

func TestRunSetupWizard_UsagePingRejectsNonHTTPS(t *testing.T) {
	tru := true
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	_, err := RunSetupWizard(WizardOptions{
		ConfigPath:      path,
		Stdin:           strings.NewReader(usagePingAnswers("y", "http://example.invalid/usage")),
		Stdout:          &bytes.Buffer{},
		IsTTY:           &tru,
		CPUCount:        4,
		MemoryMB:        8192,
		DetectGitRemote: func() string { return "" },
	})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("non-https endpoint err = %v", err)
	}
}

func TestRunSetupWizard_UsagePingOptOutClearsID(t *testing.T) {
	tru := true
	existing := DefaultConfig()
	existing.UsagePing = UsagePingConfig{Enabled: true, InstallationID: "0123456789abcdef0123456789abcdef", Endpoint: "https://example.invalid/usage"}
	var out bytes.Buffer
	// Answer "n": opting back out clears the id and endpoint.
	cfg, err := RunSetupWizard(WizardOptions{
		Existing:        existing,
		Stdin:           strings.NewReader(usagePingAnswers("n")),
		Stdout:          &out,
		IsTTY:           &tru,
		CPUCount:        4,
		MemoryMB:        8192,
		DetectGitRemote: func() string { return "" },
	})
	if err != nil {
		t.Fatalf("RunSetupWizard: %v", err)
	}
	if cfg.UsagePing != (UsagePingConfig{}) {
		t.Fatalf("opt-out kept state: %+v", cfg.UsagePing)
	}
}

func TestRunSetupWizard_SkipPingLeavesDisabled(t *testing.T) {
	tru := true
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	// --no-ping equivalent: no ping answers on stdin at all.
	in := strings.Join([]string{
		"my-machine", "home-net", "y",
		"2", "1024", "4", "y",
		"1", "https://example.invalid", "synthetic-token", "y",
		"n", "n", "y",
		"stable", "manual", "30", "y",
	}, "\n") + "\n"
	cfg, err := RunSetupWizard(WizardOptions{
		ConfigPath:      path,
		Stdin:           strings.NewReader(in),
		Stdout:          &bytes.Buffer{},
		IsTTY:           &tru,
		SkipPing:        true,
		CPUCount:        4,
		MemoryMB:        8192,
		DetectGitRemote: func() string { return "" },
	})
	if err != nil {
		t.Fatalf("RunSetupWizard: %v", err)
	}
	if cfg.UsagePing != (UsagePingConfig{}) {
		t.Fatalf("skip-ping left state: %+v", cfg.UsagePing)
	}
}

func TestValidateUsagePing(t *testing.T) {
	if err := validateUsagePing(UsagePingConfig{}); err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if err := validateUsagePing(UsagePingConfig{Enabled: true, InstallationID: "bogus", Endpoint: "https://example.invalid"}); err == nil {
		t.Fatal("bad id accepted")
	}
	if err := validateUsagePing(UsagePingConfig{Enabled: true, InstallationID: "0123456789abcdef0123456789abcdef"}); err == nil {
		t.Fatal("missing endpoint accepted")
	}
	if err := validateUsagePing(UsagePingConfig{Enabled: true, InstallationID: "0123456789abcdef0123456789abcdef", Endpoint: "http://example.invalid"}); err == nil {
		t.Fatal("non-https endpoint accepted")
	}
	if err := validateUsagePing(UsagePingConfig{Enabled: true, InstallationID: "0123456789abcdef0123456789abcdef", Endpoint: "https://example.invalid/usage"}); err != nil {
		t.Fatalf("valid: %v", err)
	}
}
