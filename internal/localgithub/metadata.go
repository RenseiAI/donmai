package localgithub

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/RenseiAI/donmai/daemon"
)

// ResolveRepository performs one bounded, authenticated GitHub read for setup.
// It returns only immutable numeric identity, canonical name, and default ref.
// The caller supplies the normal standalone GITHUB_TOKEN via Options.Token;
// this function never persists or reports its value.
func ResolveRepository(ctx context.Context, ownerRepo string, options Options) (daemon.LocalGitHubRepository, error) {
	if !validOwnerRepo(ownerRepo) || !validAPIToken(options.Token) {
		return daemon.LocalGitHubRepository{}, ErrInvalidConfiguration
	}
	baseURL, client, err := apiTransport(options)
	if err != nil {
		return daemon.LocalGitHubRepository{}, err
	}
	token := options.Token
	source := &Source{baseURL: baseURL, client: client, token: &token}
	var response struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := source.request(ctx, http.MethodGet, repoPath(daemon.LocalGitHubRepository{OwnerRepo: ownerRepo}), nil, &response); err != nil {
		return daemon.LocalGitHubRepository{}, err
	}
	if response.ID <= 0 || !validOwnerRepo(response.FullName) || !strings.EqualFold(response.FullName, ownerRepo) || response.DefaultBranch == "" || !validConfiguredRef(response.DefaultBranch) || strings.ContainsAny(response.DefaultBranch, " \t\r\n") {
		return daemon.LocalGitHubRepository{}, fmt.Errorf("%w: repository identity or default ref invalid", ErrScopeMismatch)
	}
	return daemon.LocalGitHubRepository{RepositoryID: response.ID, OwnerRepo: response.FullName, Ref: response.DefaultBranch}, nil
}

// VerifyRepositoryBranch reads a configured repository's immutable identity
// and its exact named branch through the same bounded, redirect-refusing API
// transport used by intake. It never resolves a tag, SHA, or default HEAD as
// a substitute for repository.Ref.
func VerifyRepositoryBranch(ctx context.Context, repository daemon.LocalGitHubRepository, options Options) error {
	if !validAPIToken(options.Token) {
		return ErrInvalidConfiguration
	}
	baseURL, client, err := apiTransport(options)
	if err != nil {
		return err
	}
	token := options.Token
	source := &Source{baseURL: baseURL, client: client, token: &token}
	return source.verifiedRepositoryBranch(ctx, repository)
}
