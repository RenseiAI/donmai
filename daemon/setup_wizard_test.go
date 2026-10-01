package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type setupResolverFixture struct {
	profiles     []LocalRuntimeSetupProfile
	repo         LocalGitHubRepository
	repositories map[string]LocalGitHubRepository
}

type countingSetupResolver struct{ calls *int }

func (s countingSetupResolver) Profiles(context.Context) ([]LocalRuntimeSetupProfile, error) {
	(*s.calls)++
	return nil, errors.New("should not inspect profiles")
}

func (s countingSetupResolver) Repository(context.Context, string) (LocalGitHubRepository, error) {
	(*s.calls)++
	return LocalGitHubRepository{}, errors.New("should not inspect repository")
}

func (f setupResolverFixture) Profiles(context.Context) ([]LocalRuntimeSetupProfile, error) {
	return f.profiles, nil
}

func (f setupResolverFixture) Repository(_ context.Context, ownerRepo string) (LocalGitHubRepository, error) {
	if f.repositories != nil {
		if repository, ok := f.repositories[ownerRepo]; ok {
			return repository, nil
		}
		return LocalGitHubRepository{}, fmt.Errorf("unexpected repository %q", ownerRepo)
	}
	if ownerRepo != f.repo.OwnerRepo {
		return LocalGitHubRepository{}, fmt.Errorf("unexpected repository %q", ownerRepo)
	}
	return f.repo, nil
}

func defaultLocalRerunAnswers() string {
	return strings.Join([]string{"", "", "y", "", "", "", "y", "", "y", "", "", "", "", "y", "", "", ""}, "\n") + "\n"
}

func TestRunSetupWizard_DefaultRerunPreservesLocalQueueAndAllSources(t *testing.T) {
	tru := true
	existing := DefaultConfig()
	existing.APIVersion = LocalRuntimeConfigAPIVersion
	existing.Orchestrator.URL = "file:///tmp/synthetic-nondefault-queue"
	policy := InitialLocalExecutionSecurity()
	policy.Network = "logged"
	existing.LocalRuntime = &LocalRuntimeConfig{ExecutionSecurity: policy, Harness: "codex", Model: "builtin-host", ModelAuthor: "openai", Repositories: []LocalGitHubRepository{
		{RepositoryID: 1234, OwnerRepo: "acme/repo", Label: "issues", Ref: "release"},
		{RepositoryID: 5678, OwnerRepo: "acme/other", Label: "queue", Ref: "main"},
	}}
	resolver := setupResolverFixture{profiles: []LocalRuntimeSetupProfile{{Label: "Codex", Harness: "codex", Model: "builtin-host", ModelAuthor: "openai"}}, repositories: map[string]LocalGitHubRepository{
		"acme/repo":  {RepositoryID: 1234, OwnerRepo: "acme/repo", Ref: "main"},
		"acme/other": {RepositoryID: 5678, OwnerRepo: "acme/other", Ref: "main"},
	}}
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	var out bytes.Buffer
	cfg, err := RunSetupWizard(WizardOptions{Existing: existing, ConfigPath: path, Stdin: strings.NewReader(defaultLocalRerunAnswers()), Stdout: &out, IsTTY: &tru, LocalRuntimeResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIVersion != LocalRuntimeConfigAPIVersion || cfg.Orchestrator.URL != existing.Orchestrator.URL || cfg.LocalRuntime == nil || len(cfg.LocalRuntime.Repositories) != 2 || cfg.LocalRuntime.ExecutionSecurity.Network != "logged" || cfg.LocalRuntime.Repositories[0].Label != "issues" || cfg.LocalRuntime.Repositories[0].Ref != "release" || cfg.LocalRuntime.Repositories[1] != existing.LocalRuntime.Repositories[1] {
		t.Fatalf("default rerun lost local schema, queue, policy, or source set")
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded.LocalRuntime == nil || len(loaded.LocalRuntime.Repositories) != 2 || loaded.Orchestrator.URL != existing.Orchestrator.URL {
		t.Fatalf("written rerun lost local schema, queue, or source set: %v", err)
	}
	if strings.Contains(out.String(), "Accept any project routed") {
		t.Fatal("local rerun displayed remote admission")
	}

	resolver.repositories["acme/other"] = LocalGitHubRepository{RepositoryID: 9999, OwnerRepo: "acme/other", Ref: "main"}
	if _, err := RunSetupWizard(WizardOptions{Existing: existing, Stdin: strings.NewReader(defaultLocalRerunAnswers()), Stdout: &bytes.Buffer{}, IsTTY: &tru, LocalRuntimeResolver: resolver}); err == nil {
		t.Fatal("changed second repository identity accepted")
	}
}

func TestRunSetupWizard_ExplicitRemoteDoesNotInheritFileURL(t *testing.T) {
	tru := true
	existing := DefaultConfig()
	existing.APIVersion = LocalRuntimeConfigAPIVersion
	existing.Orchestrator.URL = "file:///tmp/synthetic-queue"
	existing.LocalRuntime = &LocalRuntimeConfig{ExecutionSecurity: InitialLocalExecutionSecurity(), Harness: "codex", Model: "builtin-host", ModelAuthor: "openai", Repositories: []LocalGitHubRepository{{RepositoryID: 1234, OwnerRepo: "acme/repo", Label: "donmai", Ref: "main"}}}
	prefix := strings.Join([]string{"", "", "y", "", "", "", "y", "1"}, "\n") + "\n"
	for _, remoteURL := range []string{"", "file:///tmp/other-queue"} {
		path := filepath.Join(t.TempDir(), "daemon.yaml")
		var out bytes.Buffer
		_, err := RunSetupWizard(WizardOptions{Existing: existing, ConfigPath: path, Stdin: strings.NewReader(prefix + remoteURL + "\n"), Stdout: &out, IsTTY: &tru})
		if err == nil {
			t.Fatalf("explicit remote accepted %q", remoteURL)
		}
		if strings.Contains(out.String(), "Orchestrator URL [file:") {
			t.Fatal("remote prompt inherited the local file URL")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid remote choice wrote config: %v", err)
		}
	}
}

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
		"2",          // Choice 2 — local file queue
		"y",          // Continue?
		"1",          // Native profile
		"acme/repo",  // GitHub repository
		"donmai",     // Issue label
		"main",       // Ref
		"y",          // Continue?
		"stable",     // Channel
		"manual",     // Schedule
		"30",         // Drain timeout
	}, "\n") + "\n")
	var out bytes.Buffer

	cfgPath := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg, err := RunSetupWizard(WizardOptions{
		ConfigPath:           cfgPath,
		BinaryName:           "fleetctl",
		Stdin:                in,
		CPUCount:             4,
		MemoryMB:             8192,
		DetectGitRemote:      func() string { return "" },
		Stdout:               &out,
		IsTTY:                &tru,
		LocalRuntimeResolver: setupResolverFixture{profiles: []LocalRuntimeSetupProfile{{Label: "Codex", Harness: "codex", Model: "gpt-5-codex", ModelAuthor: "openai"}}, repo: LocalGitHubRepository{RepositoryID: 1234, OwnerRepo: "acme/repo", Ref: "main"}},
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
	if cfg.AdmitsAnyRoutedProject() {
		t.Errorf("local ProjectAdmissionMode = %q, want enumerated", cfg.EffectiveProjectAdmissionMode())
	}
	for _, guidance := range []string{"fleetctl host status", "fleetctl host logs", "fleetctl host stop"} {
		if !strings.Contains(out.String(), guidance) {
			t.Errorf("setup completion omitted configured-binary guidance %q: %s", guidance, out.String())
		}
	}
	if cfg.APIVersion != LocalRuntimeConfigAPIVersion || cfg.LocalRuntime == nil || cfg.LocalRuntime.Repositories[0].RepositoryID != 1234 {
		t.Fatalf("local config missing verified profile: %+v", cfg.LocalRuntime)
	}
	if !strings.Contains(out.String(), "ambient-host-login") || strings.Contains(out.String(), "all-routed") {
		t.Fatalf("local policy/intake summary wrong: %s", out.String())
	}
}

func localWizardAnswers() string {
	return strings.Join([]string{
		"", "", "y", // machine
		"", "", "", "y", // capacity
		"2", "y", // local queue
		"", "", "", "", // profile, repository, label, ref
		"y",        // local intake
		"", "", "", // updates
	}, "\n") + "\n"
}

func TestRunSetupWizard_LocalTransitionClearsRemoteTokenAndSeedsPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	tru := true
	existing := DefaultConfig()
	existing.Orchestrator.AuthToken = "synthetic-remote-token"
	var out bytes.Buffer
	cfg, err := RunSetupWizard(WizardOptions{
		Existing: existing, ConfigPath: path, Stdin: strings.NewReader(localWizardAnswers()), Stdout: &out, IsTTY: &tru,
		DetectGitRemote:      func() string { return "git@github.com:acme/repo.git" },
		LocalRuntimeResolver: setupResolverFixture{profiles: []LocalRuntimeSetupProfile{{Label: "Codex", Harness: "codex", Model: "gpt-5-codex", ModelAuthor: "openai"}}, repo: LocalGitHubRepository{RepositoryID: 1234, OwnerRepo: "acme/repo", Ref: "main"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Orchestrator.AuthToken != "" {
		t.Fatal("remote token survived local selection")
	}
	if cfg.LocalRuntime.ExecutionSecurity == nil || string(cfg.LocalRuntime.ExecutionSecurity.Credentials) != "ambient-host-login" {
		t.Fatal("visible initial policy was not persisted")
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Orchestrator.AuthToken != "" || loaded.APIVersion != LocalRuntimeConfigAPIVersion {
		t.Fatalf("written local config retained remote authority: %+v", loaded.Orchestrator)
	}
	if !strings.Contains(out.String(), "host-user is uncontained") {
		t.Fatal("ambient/uncontained meaning missing from setup")
	}
}

func TestRunSetupWizard_LocalV2RerunPreservesPolicyAndRepository(t *testing.T) {
	tru := true
	existing := DefaultConfig()
	existing.APIVersion = LocalRuntimeConfigAPIVersion
	existing.Orchestrator.URL = "file:///tmp/synthetic-queue"
	policy := InitialLocalExecutionSecurity()
	policy.Network = "logged"
	existing.LocalRuntime = &LocalRuntimeConfig{ExecutionSecurity: policy, Harness: "codex", Model: "gpt-5-codex", ModelAuthor: "openai", Repositories: []LocalGitHubRepository{{RepositoryID: 1234, OwnerRepo: "acme/repo", Label: "issues", Ref: "release"}}}
	resolver := setupResolverFixture{profiles: []LocalRuntimeSetupProfile{{Label: "Codex", Harness: "codex", Model: "gpt-5-codex", ModelAuthor: "openai"}}, repo: LocalGitHubRepository{RepositoryID: 1234, OwnerRepo: "acme/repo", Ref: "main"}}
	cfg, err := RunSetupWizard(WizardOptions{Existing: existing, Stdin: strings.NewReader(localWizardAnswers()), Stdout: &bytes.Buffer{}, IsTTY: &tru, LocalRuntimeResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalRuntime.ExecutionSecurity.Network != "logged" || cfg.LocalRuntime.Repositories[0].Label != "issues" || cfg.LocalRuntime.Repositories[0].Ref != "release" {
		t.Fatalf("rerun lost authored local settings: %+v", cfg.LocalRuntime)
	}

	// A damaged v2 own policy is never reseeded by another setup run.
	existing.LocalRuntime.ExecutionSecurity.FileRead = ""
	_, err = RunSetupWizard(WizardOptions{Existing: existing, Stdin: strings.NewReader(localWizardAnswers()), Stdout: &bytes.Buffer{}, IsTTY: &tru, LocalRuntimeResolver: resolver})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing v2 policy was repaired or hidden: %v", err)
	}
}

func TestRunSetupWizard_MissingV2PolicyRefusesBeforeDiscovery(t *testing.T) {
	tru := true
	existing := DefaultConfig()
	existing.APIVersion = LocalRuntimeConfigAPIVersion
	existing.Orchestrator.URL = "file:///tmp/synthetic-queue"
	existing.LocalRuntime = &LocalRuntimeConfig{ExecutionSecurity: InitialLocalExecutionSecurity(), Harness: "codex", Model: "builtin-host", ModelAuthor: "openai", Repositories: []LocalGitHubRepository{{RepositoryID: 1234, OwnerRepo: "acme/repo", Label: "donmai", Ref: "main"}}}
	existing.LocalRuntime.ExecutionSecurity.FileRead = ""
	calls := 0
	_, err := RunSetupWizard(WizardOptions{Existing: existing, Stdin: strings.NewReader(localWizardAnswers()), Stdout: &bytes.Buffer{}, IsTTY: &tru, LocalRuntimeResolver: countingSetupResolver{calls: &calls}})
	if err == nil || calls != 0 {
		t.Fatalf("missing own policy reached external discovery: calls=%d err=%v", calls, err)
	}
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	fals := false
	if _, err := RunSetupWizard(WizardOptions{Existing: existing, ConfigPath: path, IsTTY: &fals}); err == nil {
		t.Fatal("noninteractive setup repaired missing own policy")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid local config was written: %v", err)
	}
}

// TestRunSetupWizard_Interactive_DeclinedAdmissionStaysEnumerated pins that the
// wizard's admission question is a real choice, not a formality: answering "n"
// must leave the machine on the narrow per-project mode.
func TestRunSetupWizard_Interactive_DeclinedAdmissionStaysEnumerated(t *testing.T) {
	tru := true
	in := strings.NewReader(strings.Join([]string{
		"my-machine",              // Machine ID
		"home-net",                // Region
		"y",                       // Continue?
		"2",                       // Reserve cores
		"1024",                    // Reserve memory
		"4",                       // Max sessions
		"y",                       // Continue?
		"1",                       // Remote orchestrator
		"https://example.invalid", // Orchestrator URL
		"synthetic-token",         // Token
		"y",                       // Continue?
		"n",                       // Add another project?
		"n",                       // Accept any project routed to this machine?
		"y",                       // Continue?
		"stable",                  // Channel
		"manual",                  // Schedule
		"30",                      // Drain timeout
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
	if cfg.AdmitsAnyRoutedProject() {
		t.Fatal("declining the admission question still granted all-routed consent")
	}
	if got := cfg.EffectiveProjectAdmissionMode(); got != ProjectAdmissionModeEnumerated {
		t.Fatalf("mode = %q, want %q", got, ProjectAdmissionModeEnumerated)
	}
	for _, guidance := range []string{"donmai host status", "donmai host logs", "donmai host stop"} {
		if !strings.Contains(out.String(), guidance) {
			t.Errorf("setup completion omitted usable host guidance %q: %s", guidance, out.String())
		}
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

func (f setupResolverFixture) VerifyBaseBranch(_ context.Context, repository LocalGitHubRepository) error {
	if repository.Ref != "main" && repository.Ref != "release" {
		return errors.New("unexpected fixture base branch")
	}
	return nil
}

func TestRunSetupWizardRefusesUnverifiedBaseBeforeConfigWrite(t *testing.T) {
	tru := true
	existing := DefaultConfig()
	existing.APIVersion = LocalRuntimeConfigAPIVersion
	existing.Orchestrator.URL = "file:///tmp/synthetic-queue"
	existing.LocalRuntime = &LocalRuntimeConfig{ExecutionSecurity: InitialLocalExecutionSecurity(), Harness: "codex", Model: "builtin-host", ModelAuthor: "openai", Repositories: []LocalGitHubRepository{{RepositoryID: 1234, OwnerRepo: "acme/repo", Label: "donmai", Ref: "missing-branch"}}}
	resolver := setupResolverFixture{profiles: []LocalRuntimeSetupProfile{{Label: "Codex", Harness: "codex", Model: "builtin-host", ModelAuthor: "openai"}}, repo: LocalGitHubRepository{RepositoryID: 1234, OwnerRepo: "acme/repo", Ref: "main"}}
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	_, err := RunSetupWizard(WizardOptions{Existing: existing, ConfigPath: path, Stdin: strings.NewReader(defaultLocalRerunAnswers()), Stdout: &bytes.Buffer{}, IsTTY: &tru, LocalRuntimeResolver: resolver})
	if err == nil || !strings.Contains(err.Error(), "unexpected fixture base branch") {
		t.Fatalf("unverified branch refusal=%v", err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("refused branch wrote configuration: %v", err)
	}
}
