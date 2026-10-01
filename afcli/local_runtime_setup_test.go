package afcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
)

func syntheticClaudeVersionBinary(t *testing.T, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("synthetic shell fixture requires POSIX")
	}
	path := filepath.Join(t.TempDir(), "claude-fixture")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = '--version' ] || exit 7\nprintf '"+version+" (Claude Code)\\n'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(path, 0o500); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProductionSetupResolverGatesClaudeModelOnActualVersionPredicate(t *testing.T) {
	resolver, ok := localSetupResolverFactory().(setupResolver)
	if !ok || resolver.checkModelVersion == nil {
		t.Fatal("production setup factory omitted model version authority")
	}
	loginChecks := 0
	resolver.checkLogin = func(_ context.Context, harness, _ string) error {
		if harness != "claude-code" {
			t.Fatal("unexpected login harness")
		}
		loginChecks++
		return nil
	}
	resolver.models = func(harness string) []agent.ModelDesc {
		if harness != "claude-code" {
			return nil
		}
		return []agent.ModelDesc{{ID: "claude-sonnet-5", HumanLabel: "Synthetic Sonnet 5", Hosts: []agent.ServingHost{agent.HostOAuthCLI}}}
	}
	for _, fixture := range []struct {
		version string
		offered bool
	}{{"2.1.196", false}, {"2.1.197", true}} {
		binary := syntheticClaudeVersionBinary(t, fixture.version)
		resolver.lookPath = func(name string) (string, error) {
			if name == "claude" {
				return binary, nil
			}
			return "", errors.New("codex absent")
		}
		profiles, err := resolver.Profiles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if (len(profiles) == 1) != fixture.offered {
			t.Fatalf("Claude %s profile offer=%v, want %v", fixture.version, len(profiles) == 1, fixture.offered)
		}
	}
	if loginChecks != 1 {
		t.Fatalf("version gate allowed %d login checks, want one supported candidate", loginChecks)
	}
}

func TestCheckNativeSetupLoginRejectsEmptyCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	binary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNativeSetupLogin(context.Background(), "codex", binary); err == nil {
		t.Fatal("constructor-only check accepted missing host login")
	}
}

type commandSetupResolver struct{}

func (commandSetupResolver) Profiles(context.Context) ([]daemon.LocalRuntimeSetupProfile, error) {
	return []daemon.LocalRuntimeSetupProfile{{Label: "Synthetic Codex", Harness: "codex", Model: "builtin-host", ModelAuthor: "openai"}}, nil
}

func (commandSetupResolver) Repository(_ context.Context, name string) (daemon.LocalGitHubRepository, error) {
	if name != "acme/repo" {
		return daemon.LocalGitHubRepository{}, errors.New("unexpected repository")
	}
	return daemon.LocalGitHubRepository{RepositoryID: 1234, OwnerRepo: "acme/repo", Ref: "main"}, nil
}

func TestDaemonSetupCommandWiresLocalResolverIntoWizard(t *testing.T) {
	oldFactory, oldTTY, oldStdin := localSetupResolverFactory, setupWizardTTYOverride, os.Stdin
	tru := true
	localSetupResolverFactory = func() daemon.LocalRuntimeSetupResolver { return commandSetupResolver{} }
	setupWizardTTYOverride = &tru
	t.Cleanup(func() { localSetupResolverFactory, setupWizardTTYOverride, os.Stdin = oldFactory, oldTTY, oldStdin })
	input := strings.Join([]string{"", "", "y", "", "", "", "y", "2", "y", "", "acme/repo", "", "", "y", "", "", ""}, "\n") + "\n"
	inputPath := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(inputPath, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	os.Stdin = file
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	command := newDaemonSetupCmd("donmai")
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetArgs([]string{"--config", path})
	if err := command.Execute(); err != nil {
		t.Fatalf("setup command: %v", err)
	}
	configured, err := daemon.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if configured == nil || configured.LocalRuntime == nil || configured.LocalRuntime.Repositories[0].RepositoryID != 1234 || configured.LocalRuntime.Model != "builtin-host" {
		t.Fatalf("command did not reach local wizard resolver: %+v", configured)
	}
}

func TestSetupResolverProfilesUsesInstalledLoginAndBuiltInHostCatalog(t *testing.T) {
	seen := []string{}
	resolver := setupResolver{
		lookPath: func(name string) (string, error) {
			if name == "codex" {
				return "/synthetic/codex", nil
			}
			return "/synthetic/claude", nil
		},
		checkLogin: func(_ context.Context, harness, binary string) error {
			seen = append(seen, harness+":"+binary)
			if harness == "claude-code" {
				return errors.New("synthetic unavailable login")
			}
			return nil
		},
		checkModelVersion: func(_ context.Context, binary, model string) error {
			if binary != "/synthetic/claude" || model != "other-host" {
				t.Fatalf("model version check lost candidate identity: %q %q", binary, model)
			}
			return nil
		},
		models: func(harness string) []agent.ModelDesc {
			if harness == "codex" {
				return []agent.ModelDesc{
					{ID: "builtin-host", HumanLabel: "Host model", Hosts: []agent.ServingHost{agent.HostOAuthCLI}},
					{ID: "direct-only", HumanLabel: "Direct model", Hosts: []agent.ServingHost{agent.HostDirect}},
				}
			}
			return []agent.ModelDesc{{ID: "other-host", Hosts: []agent.ServingHost{agent.HostOAuthCLI}}}
		},
	}
	profiles, err := resolver.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Model != "builtin-host" || profiles[0].Harness != "codex" || profiles[0].ModelAuthor != "openai" || profiles[0].ModelCatalogRevision != "" {
		t.Fatalf("unattested route offered: %+v", profiles)
	}
	if strings.Join(seen, ",") != "codex:/synthetic/codex,claude-code:/synthetic/claude" {
		t.Fatalf("unexpected login probes: %v", seen)
	}
}

func TestSetupResolverRefusesUnprovenClaudeModelBeforeLogin(t *testing.T) {
	steps := []string{}
	resolver := setupResolver{
		lookPath: func(name string) (string, error) {
			if name == "claude" {
				return "/synthetic/claude", nil
			}
			return "", errors.New("codex absent")
		},
		models: func(harness string) []agent.ModelDesc {
			if harness != "claude-code" {
				return nil
			}
			return []agent.ModelDesc{{ID: "old-unsupported", Hosts: []agent.ServingHost{agent.HostOAuthCLI}}, {ID: "supported", Hosts: []agent.ServingHost{agent.HostOAuthCLI}}}
		},
		checkModelVersion: func(_ context.Context, binary, model string) error {
			steps = append(steps, "version:"+model)
			if binary != "/synthetic/claude" {
				t.Fatal("wrong binary in version check")
			}
			if model == "old-unsupported" {
				return errors.New("synthetic old CLI")
			}
			return nil
		},
		checkLogin: func(_ context.Context, harness, binary string) error {
			steps = append(steps, "login")
			if harness != "claude-code" || binary != "/synthetic/claude" {
				t.Fatal("wrong login candidate")
			}
			return nil
		},
	}
	profiles, err := resolver.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Model != "supported" || strings.Join(steps, ",") != "version:old-unsupported,version:supported,login" {
		t.Fatalf("unsupported Claude model offered or ordering wrong: profiles=%+v steps=%v", profiles, steps)
	}
}

func TestSetupResolverRepositoryRequiresStandaloneToken(t *testing.T) {
	called := false
	resolver := setupResolver{repository: func(_ context.Context, name string) (daemon.LocalGitHubRepository, error) {
		called = true
		if name != "acme/repo" {
			t.Fatalf("repository argument discarded: %q", name)
		}
		return daemon.LocalGitHubRepository{RepositoryID: 1234, OwnerRepo: name, Ref: "main"}, nil
	}}
	t.Setenv("GITHUB_TOKEN", "")
	if _, err := resolver.Repository(context.Background(), "acme/repo"); err == nil || called {
		t.Fatalf("missing token accepted or lookup called: %v", err)
	}
	t.Setenv("GITHUB_TOKEN", "synthetic-token")
	got, err := resolver.Repository(context.Background(), "acme/repo")
	if err != nil || got.RepositoryID != 1234 || !called {
		t.Fatalf("repository metadata not forwarded: %+v %v", got, err)
	}
}

func (commandSetupResolver) VerifyBaseBranch(_ context.Context, repository daemon.LocalGitHubRepository) error {
	if repository.RepositoryID != 1234 || repository.Ref != "main" {
		return errors.New("unexpected fixture base branch")
	}
	return nil
}
