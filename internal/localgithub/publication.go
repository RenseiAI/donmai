package localgithub

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/daemon"
)

const (
	publicationPR      = "github_pr"
	publicationComment = "github_comment"
)

func (s *Source) publicationRepository(task daemon.LocalPublicationTask) (daemon.LocalGitHubRepository, error) {
	repository, ok := s.configured(task.Source)
	if !ok || task.Source.IssueNumber <= 0 || task.Source.IssueURL != issueURL(repository, task.Source.IssueNumber) ||
		task.Source.Title != "" || task.Source.Body != "" || task.Key == "" || len(task.Key) > 256 ||
		!localIDPattern.MatchString(task.SessionID) || !digestPattern.MatchString(task.ResultSHA256) {
		return daemon.LocalGitHubRepository{}, ErrScopeMismatch
	}
	return repository, nil
}

func canonicalPRURL(raw string, repository daemon.LocalGitHubRepository) (int, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" {
		return 0, false
	}
	prefix := "/" + repository.OwnerRepo + "/pull/"
	if !strings.HasPrefix(parsed.Path, prefix) {
		return 0, false
	}
	numberText := strings.TrimPrefix(parsed.Path, prefix)
	number, err := strconv.Atoi(numberText)
	if err != nil || number <= 0 || strconv.Itoa(number) != numberText || raw != "https://github.com"+parsed.Path {
		return 0, false
	}
	return number, true
}

func (s *Source) readPR(ctx context.Context, task daemon.LocalPublicationTask, repository daemon.LocalGitHubRepository) (daemon.LocalPublicationResult, error) {
	if task.Status != "completed" || !shaPattern.MatchString(task.CommitSHA) ||
		(task.BaseRef != "" && !validConfiguredRef(task.BaseRef)) {
		return daemon.LocalPublicationResult{}, ErrPublicationMismatch
	}
	number, ok := canonicalPRURL(task.PullRequestURL, repository)
	if !ok {
		return daemon.LocalPublicationResult{}, ErrPublicationMismatch
	}
	var actual pullResponse
	if err := s.request(ctx, http.MethodGet, repoPath(repository)+"/pulls/"+strconv.Itoa(number), nil, &actual); err != nil {
		return daemon.LocalPublicationResult{}, err
	}
	if actual.Number != number || actual.HTMLURL != task.PullRequestURL || actual.Base.Repo.ID != repository.RepositoryID ||
		actual.Head.Repo.ID != repository.RepositoryID || actual.Head.SHA != task.CommitSHA ||
		(task.Branch != "" && actual.Head.Ref != task.Branch) ||
		(task.BaseRef != "" && actual.Base.Ref != task.BaseRef) {
		return daemon.LocalPublicationResult{}, ErrPublicationMismatch
	}
	return daemon.LocalPublicationResult{URL: actual.HTMLURL, HeadSHA: actual.Head.SHA, ReadAt: s.now().UTC().Format(time.RFC3339Nano)}, nil
}

func commentMarker(key string) string {
	return "<!-- donmai-local-publication:" + digestHex([]byte(key)) + " -->"
}

func commentBody(task daemon.LocalPublicationTask) string {
	return "Local session " + task.SessionID + " ended with status " + task.Status + ".\n" +
		"Result receipt SHA-256: " + task.ResultSHA256 + "\n\n" + commentMarker(task.Key)
}

func prReceiptBody(task daemon.LocalPublicationTask) string {
	baseRef := ""
	if task.BaseRef != "" {
		// A branch name is operator-authored metadata, but may contain
		// markdown or mention syntax. Encode it into a fixed safe alphabet.
		baseRef = "Verified PR base ref (base64url): " + base64.RawURLEncoding.EncodeToString([]byte(task.BaseRef)) + "\n"
	}
	return "Donmai local session " + task.SessionID + " completed for " + task.Source.IssueURL + ".\n" +
		"Result receipt SHA-256: " + task.ResultSHA256 + "\n" +
		"Verified PR head: " + task.CommitSHA + "\n" + baseRef + "\n" + commentMarker(task.Key)
}

func commentURL(repository daemon.LocalGitHubRepository, issueNumber int, commentID int64) string {
	return issueURL(repository, issueNumber) + "#issuecomment-" + strconv.FormatInt(commentID, 10)
}

func matchingComment(comment commentResponse, repository daemon.LocalGitHubRepository, issueNumber int, body string, actorID int64) bool {
	return comment.ID > 0 && comment.User.ID == actorID && comment.Body == body &&
		comment.HTMLURL == commentURL(repository, issueNumber, comment.ID)
}

func matchingPRReceiptComment(comment commentResponse, repository daemon.LocalGitHubRepository, prNumber int, body string, actorID int64) bool {
	if comment.ID <= 0 || comment.User.ID != actorID || comment.Body != body {
		return false
	}
	number := strconv.Itoa(prNumber)
	return comment.IssueURL == "https://api.github.com"+repoPath(repository)+"/issues/"+number &&
		comment.HTMLURL == "https://github.com/"+repository.OwnerRepo+"/pull/"+number+"#issuecomment-"+strconv.FormatInt(comment.ID, 10)
}

func (s *Source) findComment(ctx context.Context, repository daemon.LocalGitHubRepository, issueNumber int, marker string, matches func(commentResponse) bool) (commentResponse, bool, error) {
	sawMismatch := false
	for page := 1; page <= maxCommentPageCount; page++ {
		query := url.Values{"per_page": {strconv.Itoa(itemsPerPage)}, "page": {strconv.Itoa(page)}}
		path := repoPath(repository) + "/issues/" + strconv.Itoa(issueNumber) + "/comments?" + query.Encode()
		var comments []commentResponse
		if err := s.request(ctx, http.MethodGet, path, nil, &comments); err != nil {
			return commentResponse{}, false, err
		}
		if len(comments) > itemsPerPage {
			return commentResponse{}, false, fmt.Errorf("%w: comment page exceeds bound", ErrRemote)
		}
		for _, comment := range comments {
			if !strings.Contains(comment.Body, marker) {
				continue
			}
			if !matches(comment) {
				// A copied marker must not hide an exact authored comment later
				// in the history. Retain the refusal if no exact match exists.
				sawMismatch = true
				continue
			}
			return comment, true, nil
		}
		if len(comments) < itemsPerPage {
			if sawMismatch {
				return commentResponse{}, false, ErrPublicationMismatch
			}
			return commentResponse{}, false, nil
		}
		if page == maxCommentPageCount {
			return commentResponse{}, false, fmt.Errorf("%w: comment pagination bound reached", ErrRemote)
		}
	}
	return commentResponse{}, false, ErrRemote
}

func (s *Source) matchingIntent(name string, expected publicationIntent) (bool, error) {
	if err := s.dirSync(s.root); err != nil {
		return false, fmt.Errorf("%w: intent directory reaffirmation failed", ErrAmbiguousPublication)
	}
	existing, err := s.readIntent(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if existing != expected {
		return false, ErrPublicationMismatch
	}
	return true, nil
}

func (s *Source) publishComment(ctx context.Context, task daemon.LocalPublicationTask, repository daemon.LocalGitHubRepository) (daemon.LocalPublicationResult, error) {
	if task.Status != "failed" && task.Status != "cancelled" && task.Status != "blocked" {
		return daemon.LocalPublicationResult{}, ErrPublicationMismatch
	}
	matcher := func(comment commentResponse, actorID int64) bool {
		return matchingComment(comment, repository, task.Source.IssueNumber, commentBody(task), actorID)
	}
	comment, err := s.publishRecordedComment(ctx, task, repository, task.Source.IssueNumber, commentBody(task), matcher)
	if err != nil {
		return daemon.LocalPublicationResult{}, err
	}
	return daemon.LocalPublicationResult{URL: comment.HTMLURL, ReadAt: s.now().UTC().Format(time.RFC3339Nano)}, nil
}

func (s *Source) publishPRReceipt(ctx context.Context, task daemon.LocalPublicationTask, repository daemon.LocalGitHubRepository) (daemon.LocalPublicationResult, error) {
	// The reported PR tuple is only a candidate. Verify its exact target and
	// head before even constructing an intent that could permit a remote POST.
	verified, err := s.readPR(ctx, task, repository)
	if err != nil {
		return daemon.LocalPublicationResult{}, err
	}
	prNumber, ok := canonicalPRURL(verified.URL, repository)
	if !ok {
		return daemon.LocalPublicationResult{}, ErrPublicationMismatch
	}
	body := prReceiptBody(task)
	matcher := func(comment commentResponse, actorID int64) bool {
		return matchingPRReceiptComment(comment, repository, prNumber, body, actorID)
	}
	if _, err := s.publishRecordedComment(ctx, task, repository, prNumber, body, matcher); err != nil {
		return daemon.LocalPublicationResult{}, err
	}
	// The PR may have moved while its timeline receipt was being reconciled.
	// A second independent read must still match the terminal's original head.
	reaffirmed, err := s.readPR(ctx, task, repository)
	if err != nil {
		return daemon.LocalPublicationResult{}, err
	}
	if reaffirmed.URL != verified.URL || reaffirmed.HeadSHA != verified.HeadSHA {
		return daemon.LocalPublicationResult{}, ErrPublicationMismatch
	}
	return verified, nil
}

// publishRecordedComment is the single durable POST/reconciliation path for
// failure comments on source issues and success receipts on verified PRs.
// targetNumber is the GitHub issue-comments target, never issue-authored text.
func (s *Source) publishRecordedComment(ctx context.Context, task daemon.LocalPublicationTask, repository daemon.LocalGitHubRepository, targetNumber int, body string, matches func(commentResponse, int64) bool) (commentResponse, error) {
	actorID, err := s.actor(ctx) // one credential identity for this operation
	if err != nil {
		return commentResponse{}, err
	}
	marker := commentMarker(task.Key)
	intent := publicationIntent{
		Version: intentVersion, KeySHA256: digestHex([]byte(task.Key)),
		RepositoryID: repository.RepositoryID, OwnerRepo: repository.OwnerRepo,
		IssueNumber: targetNumber, SessionID: task.SessionID,
		Status: task.Status, ResultSHA256: task.ResultSHA256,
		Marker: marker, BodySHA256: digestHex([]byte(body)), AuthorID: actorID,
	}
	name := intentName(task.Key)
	prior, err := s.matchingIntent(name, intent)
	if err != nil {
		return commentResponse{}, err
	}
	match := func(comment commentResponse) bool { return matches(comment, actorID) }
	comment, found, err := s.findComment(ctx, repository, targetNumber, marker, match)
	if err != nil {
		return commentResponse{}, err
	}
	if found {
		return comment, nil
	}
	if prior {
		return commentResponse{}, ErrAmbiguousPublication
	}
	created, err := s.publishIntent(ctx, name, intent)
	if err != nil {
		return commentResponse{}, err
	}
	if !created {
		// A concurrent writer won the no-replace intent link. It owns any
		// first POST; this caller can only reconcile by GET.
		if _, err := s.matchingIntent(name, intent); err != nil {
			return commentResponse{}, err
		}
		return commentResponse{}, ErrAmbiguousPublication
	}
	var posted commentResponse
	path := repoPath(repository) + "/issues/" + strconv.Itoa(targetNumber) + "/comments"
	postErr := s.request(ctx, http.MethodPost, path, map[string]string{"body": body}, &posted)
	if postErr == nil && posted.ID > 0 {
		var observed commentResponse
		readPath := repoPath(repository) + "/issues/comments/" + strconv.FormatInt(posted.ID, 10)
		if err := s.request(ctx, http.MethodGet, readPath, nil, &observed); err == nil && match(observed) {
			return observed, nil
		}
	}
	// A missing response or failed readback is ambiguous even after an HTTP
	// error. Reconcile once immediately; later calls remain GET-only forever.
	comment, found, err = s.findComment(ctx, repository, targetNumber, marker, match)
	if err == nil && found {
		return comment, nil
	}
	return commentResponse{}, ErrAmbiguousPublication
}

// Publish performs independent PR readback and one conservatively journaled
// receipt/comment POST. It never treats a reported tuple as proof.
func (s *Source) Publish(ctx context.Context, task daemon.LocalPublicationTask) (daemon.LocalPublicationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return daemon.LocalPublicationResult{}, ErrClosed
	}
	if s.poisoned {
		return daemon.LocalPublicationResult{}, ErrAmbiguousPublication
	}
	repository, err := s.publicationRepository(task)
	if err != nil {
		return daemon.LocalPublicationResult{}, err
	}
	if err := s.verifiedRepository(ctx, repository); err != nil {
		return daemon.LocalPublicationResult{}, err
	}
	switch task.Kind {
	case publicationPR:
		return s.publishPRReceipt(ctx, task, repository)
	case publicationComment:
		return s.publishComment(ctx, task, repository)
	default:
		return daemon.LocalPublicationResult{}, ErrPublicationMismatch
	}
}
