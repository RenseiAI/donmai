package localgithub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

func TestCompletedPRRequiresIndependentExactReadback(t *testing.T) {
	fixture := newGitHubFixture(t)
	source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
	task := fixtureTask(publicationPR)
	task.Status = "completed"
	positive, err := source.Publish(context.Background(), task)
	if err != nil || positive.URL != task.PullRequestURL || positive.HeadSHA != task.CommitSHA || positive.ReadAt == "" {
		t.Fatalf("actual PR readback = %+v, %v", positive, err)
	}
	for _, mutate := range []func(*githubFixture){
		func(f *githubFixture) { f.prURL = "https://github.com/acme/repo/pull/10" },
		func(f *githubFixture) { f.prHead = strings.Repeat("c", 40) },
		func(f *githubFixture) { f.prBranch = "agent/unrelated" },
		func(f *githubFixture) { f.prBaseRef = "release" },
		func(f *githubFixture) { f.prBaseID = 9999 },
		func(f *githubFixture) { f.prHeadID = 9999 },
	} {
		fixture.mu.Lock()
		original := struct {
			url, head, branch, baseRef string
			baseID, headID             int64
		}{fixture.prURL, fixture.prHead, fixture.prBranch, fixture.prBaseRef, fixture.prBaseID, fixture.prHeadID}
		mutate(fixture)
		fixture.mu.Unlock()
		if _, err := source.Publish(context.Background(), task); !errors.Is(err, ErrPublicationMismatch) {
			t.Fatalf("substituted PR/head/branch/repo accepted: %v", err)
		}
		fixture.mu.Lock()
		fixture.prURL, fixture.prHead, fixture.prBranch = original.url, original.head, original.branch
		fixture.prBaseRef = original.baseRef
		fixture.prBaseID, fixture.prHeadID = original.baseID, original.headID
		fixture.mu.Unlock()
	}
	for _, badURL := range []string{
		"http://github.com/acme/repo/pull/9", "https://evil.example/acme/repo/pull/9",
		"https://user@github.com/acme/repo/pull/9", "https://github.com/acme/repo/pull/9?view=1",
		"https://github.com/acme/other/pull/9",
	} {
		changed := task
		changed.PullRequestURL = badURL
		if _, err := source.Publish(context.Background(), changed); !errors.Is(err, ErrPublicationMismatch) {
			t.Fatalf("uncanonical PR URL accepted: %v", err)
		}
	}
	fixture.mu.Lock()
	posts := fixture.postCalls
	fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("invalid PR substitutions changed receipt POST count to %d, want the original one", posts)
	}
}

func TestCompletedPRPublishesSessionReceiptOnVerifiedPR(t *testing.T) {
	fixture := newGitHubFixture(t)
	root := filepath.Join(t.TempDir(), "publication")
	source := fixture.source(t, root)
	task := fixtureTask(publicationPR)
	task.Status = "completed"
	result, err := source.Publish(context.Background(), task)
	if err != nil || result.URL != task.PullRequestURL || result.HeadSHA != task.CommitSHA || result.ReadAt == "" {
		t.Fatalf("completed PR evidence = %+v, %v", result, err)
	}
	fixture.mu.Lock()
	if fixture.postCalls != 1 || len(fixture.comments) != 1 || len(fixture.postTargets) != 1 || fixture.postTargets[0] != 9 || fixture.prReads != 2 || fixture.userCalls != 1 {
		fixture.mu.Unlock()
		t.Fatal("completed PR receipt did not verify PR twice and post once to that PR")
	}
	comment := fixture.comments[0]
	fixture.mu.Unlock()
	if comment.TargetNumber != 9 {
		t.Fatal("completed PR receipt targeted the source issue")
	}
	body := comment.Body
	for _, part := range []string{
		task.Source.IssueURL, task.SessionID, task.ResultSHA256, task.CommitSHA,
		"Verified PR base ref (base64url): " + base64.RawURLEncoding.EncodeToString([]byte(task.BaseRef)), commentMarker(task.Key),
	} {
		if !strings.Contains(body, part) {
			t.Fatalf("completed PR receipt missing verified metadata")
		}
	}
	if strings.Contains(body, fixtureIntake().Title) || strings.Contains(body, fixtureIntake().Body) {
		t.Fatal("completed PR receipt leaked issue text")
	}
	raw, err := os.ReadFile(filepath.Join(root, intentName(task.Key)))
	if err != nil || bytes.Contains(raw, []byte("synthetic-token")) || bytes.Contains(raw, []byte(fixtureIntake().Body)) {
		t.Fatalf("PR receipt intent leaked credentials or issue content: %v", err)
	}
	var intent publicationIntent
	if err := json.Unmarshal(raw, &intent); err != nil || intent.IssueNumber != 9 || intent.Status != "completed" || intent.BodySHA256 != digestHex([]byte(body)) {
		t.Fatalf("PR receipt intent did not bind exact target/body: %v", err)
	}
	if _, err := source.Publish(context.Background(), task); err != nil {
		t.Fatalf("same-process PR receipt reconciliation: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := fixture.source(t, root)
	if _, err := reopened.Publish(context.Background(), task); err != nil {
		t.Fatalf("restart PR receipt reconciliation: %v", err)
	}
	fixture.mu.Lock()
	posts := fixture.postCalls
	fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("completed PR receipt posted %d times across retries, want one", posts)
	}
}

func TestPRBaseRefReceiptUsesImmutableTaskAndSafeRendering(t *testing.T) {
	fixture := newGitHubFixture(t)
	fixture.prBaseRef = "feature/@everyone"
	root := filepath.Join(t.TempDir(), "publication")
	source := fixture.source(t, root) // current configured ref remains main
	task := fixtureTask(publicationPR)
	task.Status = "completed"
	task.BaseRef = "feature/@everyone"
	if _, err := source.Publish(context.Background(), task); err != nil {
		t.Fatalf("immutable task base ref refused against different current configuration: %v", err)
	}
	fixture.mu.Lock()
	body := fixture.comments[0].Body
	posts := fixture.postCalls
	fixture.mu.Unlock()
	encoded := base64.RawURLEncoding.EncodeToString([]byte(task.BaseRef))
	if posts != 1 || !strings.Contains(body, "Verified PR base ref (base64url): "+encoded) ||
		strings.Contains(body, "@everyone") || strings.Contains(body, "feature/") {
		t.Fatalf("base ref receipt was unbound or rendered unsafe: posts=%d", posts)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	repository := fixture.repository()
	repository.Ref = "release" // the mutable setup ref changed after completion
	reopened, err := New(Options{
		Repositories: []daemon.LocalGitHubRepository{repository}, Token: "synthetic-token",
		HTTPClient: fixture.server.Client(), BaseURL: fixture.server.URL, PublicationRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Publish(context.Background(), task); err != nil {
		t.Fatalf("recorded task base ref was replaced by current config: %v", err)
	}
	fixture.mu.Lock()
	posts = fixture.postCalls
	fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("ref-drift replay made %d POSTs, want one", posts)
	}
}

func TestPRReceiptIntentRefusesBaseRefReplay(t *testing.T) {
	fixture := newGitHubFixture(t)
	source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
	task := fixtureTask(publicationPR)
	task.Status = "completed"
	if _, err := source.Publish(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.prBaseRef = "release"
	fixture.mu.Unlock()
	changed := task
	changed.BaseRef = "release"
	if _, err := source.Publish(context.Background(), changed); !errors.Is(err, ErrPublicationMismatch) {
		t.Fatalf("changed immutable base ref replayed original receipt: %v", err)
	}
	fixture.mu.Lock()
	posts := fixture.postCalls
	fixture.prBaseRef = "main"
	fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("changed base ref caused %d POSTs, want one", posts)
	}
	if _, err := source.Publish(context.Background(), task); err != nil {
		t.Fatalf("original base ref no longer reconciles: %v", err)
	}
}

func TestLegacyPRReceiptWithoutBaseRefKeepsOriginalBody(t *testing.T) {
	fixture := newGitHubFixture(t)
	fixture.prBaseRef = "release"
	task := fixtureTask(publicationPR)
	task.Status = "completed"
	task.BaseRef = ""
	wantBody := "Donmai local session " + task.SessionID + " completed for " + task.Source.IssueURL + ".\n" +
		"Result receipt SHA-256: " + task.ResultSHA256 + "\n" +
		"Verified PR head: " + task.CommitSHA + "\n\n" + commentMarker(task.Key)
	if got := prReceiptBody(task); got != wantBody {
		t.Fatalf("legacy receipt body changed: %q", got)
	}
	source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
	if _, err := source.Publish(context.Background(), task); err != nil {
		t.Fatalf("legacy no-BaseRef PR completion refused: %v", err)
	}
}

func TestFailedCommentIntentBytesRemainStableWithBaseRefCarrier(t *testing.T) {
	fixture := newGitHubFixture(t)
	root := filepath.Join(t.TempDir(), "publication")
	source := fixture.source(t, root)
	task := fixtureTask(publicationComment)
	if _, err := source.Publish(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, intentName(task.Key)))
	if err != nil {
		t.Fatal(err)
	}
	task.BaseRef = "feature/@everyone"
	if _, err := source.Publish(context.Background(), task); err != nil {
		t.Fatalf("BaseRef carrier changed existing failed-comment intent: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, intentName(task.Key)))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed-comment intent bytes changed after BaseRef carrier: %v", err)
	}
	fixture.mu.Lock()
	posts := fixture.postCalls
	fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("failed-comment BaseRef replay made %d POSTs, want one", posts)
	}
}

func TestInvalidPRRefusesBeforeSessionReceiptPOST(t *testing.T) {
	for _, fixtureCase := range []struct {
		name   string
		change func(*githubFixture)
	}{
		{"wrong URL", func(f *githubFixture) { f.prURL = "https://github.com/acme/repo/pull/10" }},
		{"wrong head", func(f *githubFixture) { f.prHead = strings.Repeat("c", 40) }},
		{"wrong branch", func(f *githubFixture) { f.prBranch = "unrelated" }},
		{"wrong base ref", func(f *githubFixture) { f.prBaseRef = "release" }},
		{"wrong base repository", func(f *githubFixture) { f.prBaseID = 9999 }},
		{"wrong head repository", func(f *githubFixture) { f.prHeadID = 9999 }},
	} {
		t.Run(fixtureCase.name, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			fixtureCase.change(fixture)
			source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
			task := fixtureTask(publicationPR)
			task.Status = "completed"
			if _, err := source.Publish(context.Background(), task); !errors.Is(err, ErrPublicationMismatch) {
				t.Fatalf("invalid PR publication was accepted: %v", err)
			}
			fixture.mu.Lock()
			posts := fixture.postCalls
			fixture.mu.Unlock()
			if posts != 0 {
				t.Fatalf("invalid PR caused %d receipt POSTs before readback", posts)
			}
		})
	}
}

func TestPRReceiptRejectsSpoofedTargetAuthorAndBody(t *testing.T) {
	for _, mutate := range []struct {
		name   string
		change func(*fixtureComment)
	}{
		{"wrong author", func(c *fixtureComment) { c.AuthorID = 999 }},
		{"wrong body", func(c *fixtureComment) { c.Body += "\nforged" }},
		{"wrong API issue URL", func(c *fixtureComment) { c.IssueURL = "https://api.github.com/repos/acme/repo/issues/7" }},
		{"wrong HTML URL", func(c *fixtureComment) { c.HTMLURL = "https://github.com/acme/repo/issues/7#issuecomment-101" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			task := fixtureTask(publicationPR)
			task.Status = "completed"
			comment := fixtureComment{ID: 101, Body: prReceiptBody(task), AuthorID: 42, TargetNumber: 9}
			mutate.change(&comment)
			fixture.comments = []fixtureComment{comment}
			source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
			if _, err := source.Publish(context.Background(), task); !errors.Is(err, ErrPublicationMismatch) {
				t.Fatalf("spoofed PR receipt was accepted: %v", err)
			}
			fixture.mu.Lock()
			posts := fixture.postCalls
			fixture.mu.Unlock()
			if posts != 0 {
				t.Fatalf("spoofed PR receipt caused %d POSTs", posts)
			}
		})
	}
}

func TestPRReceiptIgnoresMarkerOnSourceIssueAndPostsToVerifiedPR(t *testing.T) {
	fixture := newGitHubFixture(t)
	task := fixtureTask(publicationPR)
	task.Status = "completed"
	fixture.comments = []fixtureComment{{ID: 101, Body: prReceiptBody(task), AuthorID: 42, TargetNumber: 7}}
	source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
	if _, err := source.Publish(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.postCalls != 1 || len(fixture.postTargets) != 1 || fixture.postTargets[0] != 9 {
		t.Fatal("source-issue marker suppressed or redirected verified PR receipt")
	}
}

func TestPRReceiptRechecksTargetAfterCommentBeforePublication(t *testing.T) {
	for _, drift := range []struct {
		name    string
		change  func(*githubFixture)
		restore func(*githubFixture)
	}{
		{"head SHA", func(f *githubFixture) { f.prHead = strings.Repeat("c", 40) }, func(f *githubFixture) { f.prHead = strings.Repeat("a", 40) }},
		{"branch", func(f *githubFixture) { f.prBranch = "unrelated" }, func(f *githubFixture) { f.prBranch = "agent/session-seven" }},
		{"base ref", func(f *githubFixture) { f.prBaseRef = "release" }, func(f *githubFixture) { f.prBaseRef = "main" }},
		{"base repository", func(f *githubFixture) { f.prBaseID = 9999 }, func(f *githubFixture) { f.prBaseID = 1234 }},
		{"head repository", func(f *githubFixture) { f.prHeadID = 9999 }, func(f *githubFixture) { f.prHeadID = 1234 }},
	} {
		t.Run(drift.name, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			fixture.prAfterPost = drift.change
			root := filepath.Join(t.TempDir(), "publication")
			source := fixture.source(t, root)
			task := fixtureTask(publicationPR)
			task.Status = "completed"
			if _, err := source.Publish(context.Background(), task); !errors.Is(err, ErrPublicationMismatch) {
				t.Fatalf("PR target drift after comment was published: %v", err)
			}
			fixture.mu.Lock()
			posts := fixture.postCalls
			drift.restore(fixture)
			fixture.prAfterPost = nil
			fixture.mu.Unlock()
			if posts != 1 {
				t.Fatalf("PR target drift caused %d receipt POSTs, want one", posts)
			}
			if _, err := source.Publish(context.Background(), task); err != nil {
				t.Fatalf("restored exact PR did not reconcile its receipt: %v", err)
			}
			fixture.mu.Lock()
			posts = fixture.postCalls
			fixture.mu.Unlock()
			if posts != 1 {
				t.Fatalf("target-drift retry sent %d POSTs, want one", posts)
			}
		})
	}
}

func TestAmbiguousPRReceiptIsGETOnlyAcrossRestart(t *testing.T) {
	for _, mode := range []string{"lose-before", "lose-after"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			fixture.postMode = mode
			root := filepath.Join(t.TempDir(), "publication")
			source := fixture.source(t, root)
			task := fixtureTask(publicationPR)
			task.Status = "completed"
			result, err := source.Publish(context.Background(), task)
			if mode == "lose-after" {
				if err != nil || result.URL != task.PullRequestURL || result.HeadSHA != task.CommitSHA {
					t.Fatalf("lost PR-comment response was not reconciled: %+v %v", result, err)
				}
			} else if !errors.Is(err, ErrAmbiguousPublication) {
				t.Fatalf("unobserved PR-comment POST was retried or acknowledged: %v", err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := fixture.source(t, root)
			_, err = reopened.Publish(context.Background(), task)
			if mode == "lose-before" && !errors.Is(err, ErrAmbiguousPublication) {
				t.Fatalf("restart reposted uncertain PR receipt: %v", err)
			}
			if mode == "lose-after" && err != nil {
				t.Fatalf("restart refused exact PR receipt: %v", err)
			}
			fixture.mu.Lock()
			if fixture.postCalls != 1 || len(fixture.postTargets) != 1 || fixture.postTargets[0] != 9 {
				fixture.mu.Unlock()
				t.Fatal("ambiguous PR receipt posted more than once or to the wrong target")
			}
			if mode == "lose-before" {
				fixture.comments = []fixtureComment{{ID: 101, Body: prReceiptBody(task), AuthorID: fixture.actorID, TargetNumber: 9}}
			}
			fixture.mu.Unlock()
			if mode == "lose-before" {
				result, err := reopened.Publish(context.Background(), task)
				if err != nil || result.URL != task.PullRequestURL || result.HeadSHA != task.CommitSHA {
					t.Fatalf("late exact PR receipt was not reconciled: %+v %v", result, err)
				}
			}
		})
	}
}

func TestPRReceiptRetryRefusesChangedCredentialActor(t *testing.T) {
	fixture := newGitHubFixture(t)
	fixture.postMode = "lose-before"
	root := filepath.Join(t.TempDir(), "publication")
	source := fixture.source(t, root)
	task := fixtureTask(publicationPR)
	task.Status = "completed"
	if _, err := source.Publish(context.Background(), task); !errors.Is(err, ErrAmbiguousPublication) {
		t.Fatalf("initial lost PR receipt response was not held: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.actorID = 43
	fixture.mu.Unlock()
	reopened := fixture.source(t, root)
	if _, err := reopened.Publish(context.Background(), task); !errors.Is(err, ErrPublicationMismatch) {
		t.Fatalf("changed actor reused PR receipt intent: %v", err)
	}
	fixture.mu.Lock()
	posts := fixture.postCalls
	fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("changed actor caused %d PR receipt POSTs, want one", posts)
	}
}

func TestPRReceiptIntentDurabilityFailureMakesNoPOST(t *testing.T) {
	for _, failure := range []string{"file-sync", "directory-sync"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			root := filepath.Join(t.TempDir(), "publication")
			source := fixture.source(t, root)
			task := fixtureTask(publicationPR)
			task.Status = "completed"
			switch failure {
			case "file-sync":
				source.fileSync = func(*os.File) error { return errors.New("synthetic file fsync failure") }
			case "directory-sync":
				source.dirSync = func(root *os.Root) error {
					if _, err := root.Lstat(intentName(task.Key)); err == nil {
						return errors.New("synthetic directory fsync failure")
					}
					return syncDirectory(root)
				}
			}
			if _, err := source.Publish(context.Background(), task); err == nil {
				t.Fatal("durability failure returned successful PR receipt publication")
			}
			fixture.mu.Lock()
			posts := fixture.postCalls
			fixture.mu.Unlock()
			if posts != 0 {
				t.Fatalf("durability failure still sent %d PR receipt POSTs", posts)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			if failure == "directory-sync" {
				reopened := fixture.source(t, root)
				if _, err := reopened.Publish(context.Background(), task); !errors.Is(err, ErrAmbiguousPublication) {
					t.Fatalf("uncertain PR receipt intent reposted after restart: %v", err)
				}
			}
		})
	}
}

func TestPRReceiptReconciliationScansPastSpoofedMarkers(t *testing.T) {
	fixture := newGitHubFixture(t)
	task := fixtureTask(publicationPR)
	task.Status = "completed"
	for i := range itemsPerPage {
		fixture.comments = append(fixture.comments, fixtureComment{ID: int64(i + 1), Body: prReceiptBody(task), AuthorID: 999, TargetNumber: 9})
	}
	fixture.comments = append(fixture.comments, fixtureComment{ID: 1001, Body: prReceiptBody(task), AuthorID: 42, TargetNumber: 9})
	source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
	result, err := source.Publish(context.Background(), task)
	if err != nil || result.URL != task.PullRequestURL || result.HeadSHA != task.CommitSHA {
		t.Fatalf("later-page exact PR receipt not reconciled: %+v %v", result, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.postCalls != 0 || !strings.Contains(strings.Join(fixture.paths, "\n"), "page=2") {
		t.Fatalf("PR receipt readback skipped later page or posted: posts=%d", fixture.postCalls)
	}
}

func TestConcurrentPRReceiptSourcesPostAtMostOnce(t *testing.T) {
	fixture := newGitHubFixture(t)
	root := filepath.Join(t.TempDir(), "publication")
	first := fixture.source(t, root)
	second := fixture.source(t, root)
	task := fixtureTask(publicationPR)
	task.Status = "completed"
	type answer struct {
		result daemon.LocalPublicationResult
		err    error
	}
	answers := make(chan answer, 2)
	for _, source := range []*Source{first, second} {
		go func(source *Source) {
			result, err := source.Publish(context.Background(), task)
			answers <- answer{result: result, err: err}
		}(source)
	}
	for range 2 {
		answer := <-answers
		if answer.err != nil && !errors.Is(answer.err, ErrAmbiguousPublication) {
			t.Fatalf("concurrent PR receipt error = %v", answer.err)
		}
		if answer.err == nil && (answer.result.URL != task.PullRequestURL || answer.result.HeadSHA != task.CommitSHA) {
			t.Fatalf("concurrent PR receipt lost original PR evidence: %+v", answer.result)
		}
	}
	fixture.mu.Lock()
	posts := fixture.postCalls
	fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("concurrent sources sent %d PR receipt POSTs, want one", posts)
	}
}

func TestFailedCommentIntentPostsOnceAndReplaysAcrossRestart(t *testing.T) {
	fixture := newGitHubFixture(t)
	root := filepath.Join(t.TempDir(), "publication")
	source := fixture.source(t, root)
	task := fixtureTask(publicationComment)
	result, err := source.Publish(context.Background(), task)
	if err != nil || result.URL != "https://github.com/acme/repo/issues/7#issuecomment-101" || result.HeadSHA != "" || result.ReadAt == "" {
		t.Fatalf("published comment readback = %+v, %v", result, err)
	}
	fixture.mu.Lock()
	if fixture.postCalls != 1 || fixture.userCalls != 1 || len(fixture.comments) != 1 {
		t.Fatalf("publication calls=%d actorReads=%d comments=%d", fixture.postCalls, fixture.userCalls, len(fixture.comments))
	}
	body := fixture.comments[0].Body
	fixture.mu.Unlock()
	if !strings.Contains(body, commentMarker(task.Key)) || strings.Contains(body, fixtureIntake().Title) ||
		strings.Contains(body, fixtureIntake().Body) || !strings.Contains(body, task.ResultSHA256) {
		t.Fatal("comment did not use closed metadata-only body")
	}
	intentPath := filepath.Join(root, intentName(task.Key))
	info, err := os.Lstat(intentPath)
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("intent file is not private regular state: %v", err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("publication root is not private: %v", err)
	}
	raw, err := os.ReadFile(intentPath)
	if err != nil || bytes.Contains(raw, []byte("synthetic-token")) || bytes.Contains(raw, []byte(fixtureIntake().Body)) {
		t.Fatalf("private intent leaked bearer or authored issue content: %v", err)
	}
	if _, err := source.Publish(context.Background(), task); err != nil {
		t.Fatalf("same-process publication retry: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := fixture.source(t, root)
	if _, err := reopened.Publish(context.Background(), task); err != nil {
		t.Fatalf("restart publication reconciliation: %v", err)
	}
	fixture.mu.Lock()
	posts := fixture.postCalls
	fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("repeated publication sent %d POSTs, want one", posts)
	}
}

func TestFailedCommentRejectsSpoofedAuthorBodyAndFreeformSource(t *testing.T) {
	for _, kind := range []string{"other-author", "wrong-body", "freeform-source"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
			task := fixtureTask(publicationComment)
			switch kind {
			case "other-author":
				fixture.comments = []fixtureComment{{ID: 101, Body: commentBody(task), AuthorID: 999}}
			case "wrong-body":
				fixture.comments = []fixtureComment{{ID: 101, Body: commentBody(task) + "\nforged", AuthorID: 42}}
			case "freeform-source":
				task.Source.Title = "untrusted model summary"
			}
			if _, err := source.Publish(context.Background(), task); err == nil {
				t.Fatal("spoofed author/body or freeform source was accepted")
			}
			fixture.mu.Lock()
			posts := fixture.postCalls
			fixture.mu.Unlock()
			if posts != 0 {
				t.Fatal("refused publication made a remote POST")
			}
		})
	}
}

func TestCommentReconciliationPaginatesToExactAuthorBody(t *testing.T) {
	fixture := newGitHubFixture(t)
	task := fixtureTask(publicationComment)
	fixture.mu.Lock()
	for i := 1; i <= itemsPerPage; i++ {
		fixture.comments = append(fixture.comments, fixtureComment{ID: int64(i), Body: "unrelated", AuthorID: 42})
	}
	fixture.comments = append(fixture.comments, fixtureComment{ID: 101, Body: commentBody(task), AuthorID: 42})
	fixture.mu.Unlock()
	source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
	got, err := source.Publish(context.Background(), task)
	if err != nil || got.URL != commentURL(fixture.repository(), 7, 101) {
		t.Fatalf("second-page authored comment readback = %+v, %v", got, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.postCalls != 0 || !strings.Contains(strings.Join(fixture.paths, "\n"), "page=2") {
		t.Fatalf("comment readback skipped pagination or POSTed: posts=%d paths=%v", fixture.postCalls, fixture.paths)
	}
}

func TestCommentReconciliationScansPastSpoofedMarkers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wrongBody  bool
		secondPage bool
		valid      bool
	}{
		{name: "other author before valid", valid: true},
		{name: "wrong body before valid", wrongBody: true, valid: true},
		{name: "valid on later page", secondPage: true, valid: true},
		{name: "only spoofs across pages", secondPage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			task := fixtureTask(publicationComment)
			body := commentBody(task)
			spoof := fixtureComment{Body: body, AuthorID: 999}
			if tc.wrongBody {
				spoof.AuthorID = 42
				spoof.Body += "\nforged"
			}
			count := 1
			if tc.secondPage {
				count = itemsPerPage + 1
			}
			fixture.mu.Lock()
			for i := range count {
				comment := spoof
				comment.ID = int64(i + 1)
				fixture.comments = append(fixture.comments, comment)
			}
			if tc.valid {
				fixture.comments = append(fixture.comments, fixtureComment{ID: 1001, Body: body, AuthorID: 42})
			}
			fixture.mu.Unlock()

			root := filepath.Join(t.TempDir(), "publication")
			for attempt := range 2 {
				source := fixture.source(t, root)
				got, err := source.Publish(context.Background(), task)
				if tc.valid {
					if err != nil || got.URL != "https://github.com/acme/repo/issues/7#issuecomment-1001" || got.ReadAt == "" {
						t.Fatalf("attempt %d: exact comment after spoof = %+v, %v", attempt, got, err)
					}
				} else if !errors.Is(err, ErrPublicationMismatch) {
					t.Fatalf("attempt %d: spoof-only reconciliation must hold, got %+v, %v", attempt, got, err)
				}
				if err := source.Close(); err != nil {
					t.Fatal(err)
				}
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.postCalls != 0 {
				t.Fatalf("reconciliation made %d POSTs, want zero", fixture.postCalls)
			}
			if tc.secondPage && !strings.Contains(strings.Join(fixture.paths, "\n"), "page=2") {
				t.Fatalf("reconciliation stopped before the later page: %v", fixture.paths)
			}
		})
	}
}

func TestAmbiguousCommentPostIsGETOnlyAcrossRetries(t *testing.T) {
	for _, mode := range []string{"lose-before", "lose-after"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			fixture.postMode = mode
			root := filepath.Join(t.TempDir(), "publication")
			source := fixture.source(t, root)
			task := fixtureTask(publicationComment)
			result, err := source.Publish(context.Background(), task)
			if mode == "lose-after" {
				if err != nil || result.URL != commentURL(fixture.repository(), 7, 101) {
					t.Fatalf("lost POST response was not reconciled: %+v, %v", result, err)
				}
			} else if !errors.Is(err, ErrAmbiguousPublication) {
				t.Fatalf("unobserved POST response was retried or acknowledged: %v", err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := fixture.source(t, root)
			_, err = reopened.Publish(context.Background(), task)
			if mode == "lose-before" && !errors.Is(err, ErrAmbiguousPublication) {
				t.Fatalf("restart reposted uncertain comment: %v", err)
			}
			if mode == "lose-after" && err != nil {
				t.Fatalf("restart failed exact comment readback: %v", err)
			}
			fixture.mu.Lock()
			if fixture.postCalls != 1 {
				t.Fatalf("ambiguous publication issued %d POSTs", fixture.postCalls)
			}
			if mode == "lose-before" {
				fixture.comments = []fixtureComment{{ID: 101, Body: commentBody(task), AuthorID: fixture.actorID}}
			}
			fixture.mu.Unlock()
			if mode == "lose-before" {
				if observed, err := reopened.Publish(context.Background(), task); err != nil || observed.URL != commentURL(fixture.repository(), 7, 101) {
					t.Fatalf("late exact comment was not reconciled: %+v, %v", observed, err)
				}
			}
		})
	}
}

func TestCommentRetryRefusesChangedCredentialActor(t *testing.T) {
	fixture := newGitHubFixture(t)
	fixture.postMode = "lose-before"
	root := filepath.Join(t.TempDir(), "publication")
	source := fixture.source(t, root)
	task := fixtureTask(publicationComment)
	if _, err := source.Publish(context.Background(), task); !errors.Is(err, ErrAmbiguousPublication) {
		t.Fatalf("initial lost response was not held: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.actorID = 43
	fixture.mu.Unlock()
	reopened := fixture.source(t, root)
	if _, err := reopened.Publish(context.Background(), task); !errors.Is(err, ErrPublicationMismatch) {
		t.Fatalf("changed authenticated actor reused prior intent: %v", err)
	}
	fixture.mu.Lock()
	posts := fixture.postCalls
	fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("actor change caused %d POSTs, want one", posts)
	}
}

func TestIntentDurabilityAndCorruptionRefuseAutomaticPOST(t *testing.T) {
	for _, kind := range []string{"file-sync", "directory-sync", "corrupt-intent"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			root := filepath.Join(t.TempDir(), "publication")
			source := fixture.source(t, root)
			task := fixtureTask(publicationComment)
			switch kind {
			case "file-sync":
				source.fileSync = func(*os.File) error { return errors.New("injected file fsync") }
			case "directory-sync":
				source.dirSync = func(root *os.Root) error {
					if _, err := root.Lstat(intentName(task.Key)); err == nil {
						return errors.New("injected post-link directory fsync")
					}
					return syncDirectory(root)
				}
			}
			if kind != "corrupt-intent" {
				if _, err := source.Publish(context.Background(), task); err == nil {
					t.Fatal("durability failure returned successful publication")
				}
			} else {
				intent := publicationIntent{
					Version: intentVersion, KeySHA256: digestHex([]byte(task.Key)), RepositoryID: 1234,
					OwnerRepo: "acme/repo", IssueNumber: 7, SessionID: task.SessionID, Status: task.Status,
					ResultSHA256: task.ResultSHA256, Marker: commentMarker(task.Key),
					BodySHA256: digestHex([]byte(commentBody(task))), AuthorID: 42,
				}
				raw, err := json.Marshal(intent)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw[:len(raw)-1], []byte(`,"extra":true}`)...)
				if err := os.WriteFile(filepath.Join(root, intentName(task.Key)), raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := source.Publish(context.Background(), task); !errors.Is(err, ErrCorruptPublicationRoot) {
					t.Fatalf("corrupt intent was overwritten or acknowledged: %v", err)
				}
			}
			fixture.mu.Lock()
			posts := fixture.postCalls
			fixture.mu.Unlock()
			if posts != 0 {
				t.Fatalf("durability/corruption refusal still POSTed %d comments", posts)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			if kind == "directory-sync" {
				reopened := fixture.source(t, root)
				if _, err := reopened.Publish(context.Background(), task); !errors.Is(err, ErrAmbiguousPublication) {
					t.Fatalf("ambiguous intent reposted after restart: %v", err)
				}
			}
		})
	}
}

func TestSourceOptionsFormattingRedactsCredential(t *testing.T) {
	options := Options{Token: "synthetic-secret-not-for-logs"}
	formatted := fmt.Sprintf("%v %+v %#v %q", options, options, options, options)
	raw, err := json.Marshal(options)
	if err != nil || strings.Contains(formatted, options.Token) || bytes.Contains(raw, []byte(options.Token)) {
		t.Fatal("options formatting or JSON exposed the configured token")
	}
}
