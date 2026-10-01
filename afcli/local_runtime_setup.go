package afcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/internal/localgithub"
	"github.com/RenseiAI/donmai/provider/endpoint/anthropic"
	"github.com/RenseiAI/donmai/provider/endpoint/openai"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
)

// setupResolver is scoped to interactive setup; it reads no credential into
// daemon configuration. The function fields permit hermetic discovery tests.
type setupResolver struct {
	lookPath          func(string) (string, error)
	checkLogin        func(context.Context, string, string) error
	checkModelVersion func(context.Context, string, string) error
	models            func(string) []agent.ModelDesc
	repository        func(context.Context, string) (daemon.LocalGitHubRepository, error)
}

var localSetupResolverFactory = func() daemon.LocalRuntimeSetupResolver {
	return setupResolver{
		lookPath:          exec.LookPath,
		checkLogin:        checkNativeSetupLogin,
		checkModelVersion: claude.CheckModelBinaryVersion,
		models:            nativeSetupModels,
		repository:        resolveSetupGitHubRepository,
	}
}

// nil in production; a focused command test forces the interactive branch
// while supplying a synthetic stdin pipe.
var setupWizardTTYOverride *bool

func nativeSetupModels(harness string) []agent.ModelDesc {
	switch harness {
	case "codex":
		return openai.New().Manifest().Models
	case "claude-code":
		return anthropic.New().Manifest().Models
	default:
		return nil
	}
}

func checkNativeSetupLogin(ctx context.Context, harness, binary string) error {
	switch harness {
	case "codex":
		return codex.CheckHostSessionLogin(ctx, binary)
	case "claude-code":
		return checkLocalClaudeLogin(ctx, binary)
	default:
		return errors.New("unsupported local harness")
	}
}

func (s setupResolver) Profiles(ctx context.Context) ([]daemon.LocalRuntimeSetupProfile, error) {
	if s.lookPath == nil || s.checkLogin == nil || s.models == nil {
		return nil, errors.New("native setup resolver is incomplete")
	}
	var profiles []daemon.LocalRuntimeSetupProfile
	for _, choice := range []struct{ harness, binary, label string }{{"codex", "codex", "Codex"}, {"claude-code", "claude", "Claude Code"}} {
		binary, err := s.lookPath(choice.binary)
		if err != nil || binary == "" {
			continue
		}
		for _, model := range s.models(choice.harness) {
			if !slices.Contains(model.Hosts, agent.HostOAuthCLI) || model.ID == "" {
				continue
			}
			if choice.harness == "claude-code" {
				if s.checkModelVersion == nil || s.checkModelVersion(ctx, binary, model.ID) != nil {
					continue
				}
			}
			if err := s.checkLogin(ctx, choice.harness, binary); err != nil {
				continue
			}
			profiles = append(profiles, daemon.LocalRuntimeSetupProfile{Label: fmt.Sprintf("%s / %s", choice.label, model.HumanLabel), Harness: choice.harness, Model: model.ID, ModelAuthor: map[string]string{"codex": "openai", "claude-code": "anthropic"}[choice.harness]})
		}
	}
	return profiles, nil
}

func (s setupResolver) Repository(ctx context.Context, ownerRepo string) (daemon.LocalGitHubRepository, error) {
	if s.repository == nil {
		return daemon.LocalGitHubRepository{}, errors.New("GitHub setup resolver is incomplete")
	}
	if strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) == "" {
		return daemon.LocalGitHubRepository{}, errors.New("GITHUB_TOKEN is required for local GitHub setup")
	}
	return s.repository(ctx, ownerRepo)
}

func resolveSetupGitHubRepository(ctx context.Context, ownerRepo string) (daemon.LocalGitHubRepository, error) {
	return localgithub.ResolveRepository(ctx, ownerRepo, localgithub.Options{Token: os.Getenv("GITHUB_TOKEN")})
}

func (s setupResolver) VerifyBaseBranch(ctx context.Context, repository daemon.LocalGitHubRepository) error {
	return localgithub.VerifyRepositoryBranch(ctx, repository, localgithub.Options{Token: os.Getenv("GITHUB_TOKEN")})
}
