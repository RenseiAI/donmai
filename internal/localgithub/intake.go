package localgithub

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/daemon"
)

func issueURL(repository daemon.LocalGitHubRepository, number int) string {
	return "https://github.com/" + repository.OwnerRepo + "/issues/" + strconv.Itoa(number)
}

func issueHasLabel(issue issueResponse, label string) bool {
	for _, candidate := range issue.Labels {
		if candidate.Name == label {
			return true
		}
	}
	return false
}

func checkedIssue(repository daemon.LocalGitHubRepository, issue issueResponse) (daemon.LocalIntakeRequest, bool, error) {
	if issue.PullRequest != nil || issue.State != "open" || !issueHasLabel(issue, repository.Label) {
		return daemon.LocalIntakeRequest{}, false, nil
	}
	if issue.Number <= 0 || issue.HTMLURL != issueURL(repository, issue.Number) ||
		issue.Title == "" || len(issue.Title) > maxIssueTitle || len(issue.Body) > maxIssueBody {
		return daemon.LocalIntakeRequest{}, false, ErrScopeMismatch
	}
	return daemon.LocalIntakeRequest{
		RepositoryID: repository.RepositoryID, OwnerRepo: repository.OwnerRepo,
		IssueNumber: issue.Number, IssueURL: issue.HTMLURL,
		Title: issue.Title, Body: issue.Body,
	}, true, nil
}

// Poll fully reads configured repositories' open, exactly labeled issues.
// Any pagination/identity failure discards the whole result, never a truncated
// partial list that could silently change configured source authority.
func (s *Source) Poll(ctx context.Context) ([]daemon.LocalIntakeRequest, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	results := make([]daemon.LocalIntakeRequest, 0)
	seen := map[string]daemon.LocalIntakeRequest{}
	textBytes := 0
	for _, repository := range s.repositories {
		if err := s.verifiedRepositoryBranch(ctx, repository); err != nil {
			return nil, err
		}
		for page := 1; page <= maxIssuePageCount; page++ {
			query := url.Values{"state": {"open"}, "labels": {repository.Label}, "per_page": {strconv.Itoa(itemsPerPage)}, "page": {strconv.Itoa(page)}}
			path := repoPath(repository) + "/issues?" + query.Encode()
			var issues []issueResponse
			if err := s.request(ctx, http.MethodGet, path, nil, &issues); err != nil {
				return nil, err
			}
			if len(issues) > itemsPerPage {
				return nil, fmt.Errorf("%w: issue page exceeds bound", ErrRemote)
			}
			for _, issue := range issues {
				candidate, eligible, err := checkedIssue(repository, issue)
				if err != nil {
					return nil, err
				}
				if !eligible {
					continue
				}
				key := strconv.FormatInt(repository.RepositoryID, 10) + ":" + strconv.Itoa(candidate.IssueNumber)
				if prior, exists := seen[key]; exists {
					if prior != candidate {
						return nil, fmt.Errorf("%w: issue changed across pages", ErrRemote)
					}
					continue
				}
				if len(results) >= maxPollItems || len(candidate.Title)+len(candidate.Body) > maxPollTextBytes-textBytes {
					return nil, fmt.Errorf("%w: issue result bound reached", ErrRemote)
				}
				textBytes += len(candidate.Title) + len(candidate.Body)
				seen[key] = candidate
				results = append(results, candidate)
			}
			if len(issues) < itemsPerPage {
				break
			}
			if page == maxIssuePageCount {
				return nil, fmt.Errorf("%w: issue pagination bound reached", ErrRemote)
			}
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].RepositoryID != results[j].RepositoryID {
			return results[i].RepositoryID < results[j].RepositoryID
		}
		return results[i].IssueNumber < results[j].IssueNumber
	})
	return results, nil
}

// Eligible is a fresh point-in-time source observation for B/C's pre-Claim
// and delayed prelaunch gates. Changed issue intent is held, not rewritten.
func (s *Source) Eligible(ctx context.Context, frozen daemon.LocalIntakeRequest) (bool, error) {
	if err := s.checkOpen(); err != nil {
		return false, err
	}
	repository, ok := s.configured(frozen)
	if !ok || frozen.IssueNumber <= 0 || frozen.IssueURL != issueURL(repository, frozen.IssueNumber) ||
		frozen.Title == "" || len(frozen.Title) > maxIssueTitle || len(frozen.Body) > maxIssueBody {
		return false, ErrScopeMismatch
	}
	if err := s.verifiedRepositoryBranch(ctx, repository); err != nil {
		return false, err
	}
	actual, err := s.getIssue(ctx, repository, frozen.IssueNumber)
	if err != nil {
		return false, err
	}
	if actual.Number != frozen.IssueNumber || actual.HTMLURL != frozen.IssueURL {
		return false, ErrScopeMismatch
	}
	current, eligible, err := checkedIssue(repository, actual)
	if err != nil {
		return false, err
	}
	if !eligible {
		return false, nil
	}
	return strings.EqualFold(current.OwnerRepo, frozen.OwnerRepo) &&
		current.Title == frozen.Title && current.Body == frozen.Body, nil
}
