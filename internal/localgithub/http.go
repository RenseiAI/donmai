package localgithub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/daemon"
)

type repositoryResponse struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

type branchResponse struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type issueResponse struct {
	Number      int              `json:"number"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	State       string           `json:"state"`
	HTMLURL     string           `json:"html_url"`
	PullRequest *json.RawMessage `json:"pull_request"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type pullResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"base"`
}

type commentResponse struct {
	ID       int64  `json:"id"`
	Body     string `json:"body"`
	HTMLURL  string `json:"html_url"`
	IssueURL string `json:"issue_url"`
	User     struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

type actorResponse struct {
	ID int64 `json:"id"`
}

func repoPath(repository daemon.LocalGitHubRepository) string {
	owner, name, _ := strings.Cut(repository.OwnerRepo, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (s *Source) request(ctx context.Context, method, path string, body any, target any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil || len(raw) > maxAPIResponse {
			return ErrInvalidConfiguration
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reader)
	if err != nil {
		return ErrInvalidConfiguration
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+*s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: request did not produce a trusted response", ErrRemote)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse+1))
	if err != nil || len(raw) > maxAPIResponse {
		return fmt.Errorf("%w: response exceeds bound or is unreadable", ErrRemote)
	}
	if (method == http.MethodGet && resp.StatusCode != http.StatusOK) ||
		(method == http.MethodPost && resp.StatusCode != http.StatusCreated) {
		return fmt.Errorf("%w: HTTP %d", ErrRemote, resp.StatusCode)
	}
	if target != nil && json.Unmarshal(raw, target) != nil {
		return fmt.Errorf("%w: invalid response JSON", ErrRemote)
	}
	return nil
}

func (s *Source) verifiedRepository(ctx context.Context, repository daemon.LocalGitHubRepository) error {
	var actual repositoryResponse
	if err := s.request(ctx, http.MethodGet, repoPath(repository), nil, &actual); err != nil {
		return err
	}
	if actual.ID != repository.RepositoryID || actual.FullName != repository.OwnerRepo {
		return ErrScopeMismatch
	}
	return nil
}

func (s *Source) verifiedRepositoryBranch(ctx context.Context, repository daemon.LocalGitHubRepository) error {
	if repository.RepositoryID <= 0 || !validOwnerRepo(repository.OwnerRepo) ||
		repository.Ref == "" || !validConfiguredRef(repository.Ref) {
		return ErrInvalidConfiguration
	}
	if err := s.verifiedRepository(ctx, repository); err != nil {
		return err
	}
	var branch branchResponse
	path := repoPath(repository) + "/branches/" + url.PathEscape(repository.Ref)
	if err := s.request(ctx, http.MethodGet, path, nil, &branch); err != nil {
		return err
	}
	if branch.Name != repository.Ref || !shaPattern.MatchString(branch.Commit.SHA) {
		return ErrScopeMismatch
	}
	return nil
}

func (s *Source) getIssue(ctx context.Context, repository daemon.LocalGitHubRepository, number int) (issueResponse, error) {
	var issue issueResponse
	if err := s.request(ctx, http.MethodGet, repoPath(repository)+"/issues/"+strconv.Itoa(number), nil, &issue); err != nil {
		return issueResponse{}, err
	}
	return issue, nil
}

func (s *Source) actor(ctx context.Context) (int64, error) {
	var actor actorResponse
	if err := s.request(ctx, http.MethodGet, "/user", nil, &actor); err != nil {
		return 0, err
	}
	if actor.ID <= 0 {
		return 0, ErrScopeMismatch
	}
	return actor.ID, nil
}
