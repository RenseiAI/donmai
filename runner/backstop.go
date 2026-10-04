package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/gitexec"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/harnessstate"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// ============================================================================
// Path-exclude data tables (verbatim port from
// donmai-libraries/packages/core/src/orchestrator/session-backstop.ts:57-95).
// ============================================================================
//
// Most entries below mirror the legacy TS shouldExcludeFromBackstop tables.
// Donmai's Go-native code-intel MCP adds one Go-only generated-cache path;
// unlike source-owned .donmai configuration, that index must never enter a
// backstop commit merely because the MCP server warmed at session startup.

// excludeDirAnyDepth lists directory names that disqualify a path at
// any depth. Mirrors EXCLUDE_DIR_ANY_DEPTH from the legacy TS.
var excludeDirAnyDepth = []string{
	// Node / JS
	"node_modules",
	".next",
	".nuxt",
	"dist",
	".turbo",
	// Python
	"__pycache__",
	".venv",
	// Go
	"go-build",
	".gocache",
	"gocache",
	".golangci-lint-cache",
	// General
	".cache",
}

// excludeDirTopLevel lists directory names that disqualify a path
// only when they are the top-level component. The legacy TS
// EXCLUDE_DIR_TOP_LEVEL named only `.agent` — the runner's own state
// dir — which left every OTHER harness's checkout-resident state
// (session storage, project-local CLI config) eligible for a backstop
// commit purely because nothing had thought to list it.
//
// The list now comes from runtime/harnessstate, the single source of
// truth shared with the provision-time git exclude. Two lists that must
// agree about what "harness state" means will eventually disagree; one
// list cannot.
var excludeDirTopLevel = harnessstate.Dirs()

// excludeExtensions is the set of file extensions (with leading dot)
// that disqualify a path. Mirrors EXCLUDE_EXTENSIONS.
var excludeExtensions = []string{".pyc", ".tmp", ".log"}

// excludeBasenamePrefixes is the set of basename prefixes that
// disqualify a path. Mirrors EXCLUDE_BASENAME_PREFIXES.
var excludeBasenamePrefixes = []string{"__debug_bin"}

// excludePathPrefixes is the set of exact path-prefixes (relative to
// the worktree root, with trailing slash stripped) that disqualify a
// path. Mirrors EXCLUDE_PATH_PREFIXES.
var excludePathPrefixes = []string{
	"target/debug/",
	"target/release/",
	".donmai/code-index/",
}

// backstopMaxFiles is the upper bound on the number of files the
// backstop will auto-commit. Beyond this we abort rather than create
// a polluted PR. Mirrors BACKSTOP_MAX_FILES.
const backstopMaxFiles = 200

// backstopUnstageBatch caps the number of paths passed to one
// `git reset HEAD --` invocation when unstaging matched files.
// Keeps us under ARG_MAX. Mirrors BACKSTOP_UNSTAGE_BATCH.
const backstopUnstageBatch = 100

// shouldExcludeFromBackstop reports whether a staged path should be
// unstaged before the backstop commits. Verbatim port of
// session-backstop.ts::shouldExcludeFromBackstop. Exported via
// the lower-case identifier so tests in the same package can
// exercise the decision table.
//
// `path` is relative to the worktree root, with forward slashes
// (git's --name-only output uses forward slashes on every platform).
func shouldExcludeFromBackstop(path string) bool {
	if path == "" {
		return false
	}
	parts := strings.Split(path, "/")
	basename := parts[len(parts)-1]

	// Directory names anywhere in the path.
	for _, part := range parts {
		for _, ex := range excludeDirAnyDepth {
			if part == ex {
				return true
			}
		}
	}

	// Top-level-only directory names. Index 0 is the top-level
	// component; require parts.length > 1 so we don't exclude a
	// file *named* like a state dir.
	if len(parts) > 1 {
		for _, ex := range excludeDirTopLevel {
			if parts[0] == ex {
				return true
			}
		}
	}

	// Extension match (on basename).
	if dotIdx := strings.LastIndex(basename, "."); dotIdx > 0 {
		ext := basename[dotIdx:]
		for _, ex := range excludeExtensions {
			if ext == ex {
				return true
			}
		}
	}

	// Basename prefix match.
	for _, prefix := range excludeBasenamePrefixes {
		if strings.HasPrefix(basename, prefix) {
			return true
		}
	}

	// Exact path-prefix match. The legacy TS treats an entry like
	// "target/debug/" as also matching the bare prefix "target/debug"
	// (no trailing slash); replicate by stripping the trailing slash
	// before comparison.
	for _, prefix := range excludePathPrefixes {
		bare := strings.TrimRight(prefix, "/")
		if path == bare || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// ============================================================================
// Backstop runner
// ============================================================================

// shouldBackstop reports whether stage 2 of tail recovery
// (deterministic backstop) should run for this Result. Today's rule:
// run when the result envelope has no PR URL yet and the session is
// either completed-but-pr-less or failed-recoverable.
//
// Sessions classified as "lost-ownership" or "timeout" skip backstop
// because the daemon has already lost the right to push from this
// worker.
//
// workType gates the chain on the completion contract's PR obligation.
// Result sensitivity is independent: QA/review work needs a verdict to drive
// its transition but legitimately owes no commit, branch, or PR.
func shouldBackstop(res *Result, workType string) bool {
	if res == nil {
		return false
	}
	// Contract gate: only work that explicitly owes a PR may enter the
	// deterministic git backstop.
	if !RequiresPRURL(workType) {
		return false
	}
	switch res.FailureMode {
	case FailureLostOwnership, FailureTimeout, FailureProviderResolve, FailureAgentBlocked, FailureOperatorCancelled:
		// FailureContinuationsUnproductive and FailureContinuationsCeiling
		// are deliberately absent: a session whose turn never finished
		// still gets the open-PR attempt, so a real pull request the
		// verifier could not confirm is recovered and the work is pushed
		// rather than left only on this host.
		// FailureAgentBlocked: the agent deliberately declined — there is
		// no in-progress work to commit, and an empty-branch backstop PR
		// would misrepresent a reasoned refusal as abandoned work.
		// FailureOperatorCancelled: the platform deterministically stopped
		// the session — the worker has lost the right to push (same posture
		// as lost-ownership) and the cancel is intentional, so an empty
		// backstop PR would misrepresent a deliberate cancel as abandoned
		// work. Mirrors FailureAgentBlocked routing.
		return false
	}
	// If the agent already produced a PR, nothing to do.
	return res.PullRequestURL == ""
}

// runBackstop executes the deterministic git workflow when the agent
// failed to commit/push/PR. Returns a [agent.BackstopReport]
// describing what happened so the caller can attach it to the
// Result.
//
// Steps:
//
//  1. `git status --porcelain` — snapshot uncommitted state.
//  2. `git add -A` — stage everything.
//  3. Enumerate staged files; unstage any matching the path-exclude
//     list above (verbatim port).
//  4. Safety cap: abort when the staged count exceeds
//     [backstopMaxFiles].
//  5. `git commit -m "Backstop: <session-id>"` (skipped when nothing
//     remains staged).
//  6. `git push origin HEAD:refs/heads/<session branch>` — the work,
//     whatever is checked out, to the branch the session owns; never any
//     other remote branch, and never forced.
//  7. `gh pr create --head <session branch>` — return the URL.
//
// Errors at any step short-circuit and are recorded on
// BackstopReport.Diagnostics; the caller decides whether to surface
// them as Result.FailureMode = FailureBackstop.
//
// backstopVisibility is the repository-visibility gate for the backstop's
// public surfaces (commit message, PR title/body). Only
// backstopVisibilityPrivate keeps the tracker identifier; every other
// value — public or unknown — uses the redacted format so a private
// tracker identifier never leaks into a public repository.
type backstopVisibility string

const (
	backstopVisibilityPublic  backstopVisibility = "PUBLIC"
	backstopVisibilityPrivate backstopVisibility = "PRIVATE"
	backstopVisibilityUnknown backstopVisibility = "UNKNOWN"
)

// parseBackstopVisibility normalizes `gh repo view` output. Anything that
// is not an explicit PRIVATE — including empty output and future values —
// maps to UNKNOWN so callers fail safe to the redacted format.
func parseBackstopVisibility(out string) backstopVisibility {
	switch strings.ToUpper(strings.TrimSpace(out)) {
	case string(backstopVisibilityPrivate):
		return backstopVisibilityPrivate
	case string(backstopVisibilityPublic):
		return backstopVisibilityPublic
	default:
		return backstopVisibilityUnknown
	}
}

// resolveBackstopVisibility determines repository visibility via
// `gh repo view --json visibility`. Callers resolve it once per backstop
// run, before composing any public surface, and reuse it for the commit
// message and the PR title/body. Any failure (gh missing,
// unauthenticated, not a GitHub repository) returns UNKNOWN, which
// renders with the public (redacted) format.
func resolveBackstopVisibility(ctx context.Context, worktreePath string) backstopVisibility {
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := runGh(lookupCtx, worktreePath, "repo", "view", "--json", "visibility", "-q", ".visibility")
	if err != nil {
		return backstopVisibilityUnknown
	}
	return parseBackstopVisibility(out)
}

// backstopCommitMessage composes the backstop commit message. Private
// repositories keep the tracker identifier for correlation; public or
// unknown-visibility repositories carry only the session id.
func backstopCommitMessage(qw QueuedWork, vis backstopVisibility) string {
	if vis == backstopVisibilityPrivate && qw.IssueIdentifier != "" {
		return fmt.Sprintf("Backstop: %s (%s)", qw.IssueIdentifier, qw.SessionID)
	}
	return fmt.Sprintf("Backstop: %s", qw.SessionID)
}

// backstopPRTitle composes the backstop PR title. Private repositories
// keep the tracker identifier prefix; public or unknown-visibility
// repositories use a neutral title with no tracker identifier — the
// session's title with any identifier occurrence removed, or the
// fallback when nothing remains.
func backstopPRTitle(qw QueuedWork, vis backstopVisibility) string {
	if vis == backstopVisibilityPrivate {
		title := commitSubject(qw)
		if qw.IssueIdentifier != "" && !strings.Contains(title, qw.IssueIdentifier) {
			title = qw.IssueIdentifier + ": " + title
		}
		return title
	}
	title := strings.TrimSpace(qw.Title)
	if title != "" && qw.IssueIdentifier != "" && strings.Contains(title, qw.IssueIdentifier) {
		title = strings.TrimSpace(strings.ReplaceAll(title, qw.IssueIdentifier, ""))
		title = strings.TrimSpace(strings.TrimLeft(title, ":\u2014\u2013- "))
	}
	if title == "" {
		title = "Auto-recovered session work"
	}
	return title
}

// backstopPRBody composes the backstop PR body. It carries only the
// session id — never the tracker identifier — on every visibility, so
// the body needs no visibility branch.
func backstopPRBody(qw QueuedWork) string {
	return fmt.Sprintf(
		"## Summary\n\nAuto-recovered by the runner backstop for session %s.\n\n"+
			"The agent finished without opening a pull request. The backstop "+
			"committed pending changes (excluding build artifacts) and opened "+
			"this PR so the work is not lost.",
		qw.SessionID,
	)
}

//nolint:gocyclo,funlen // step ordering is the package's contract; splitting hides intent
func (r *Runner) runBackstop(ctx context.Context, qw QueuedWork, branch string, res *Result, allowedRepositories map[string]struct{}) agent.BackstopReport {
	// Keep the completion contract at the mutation boundary too. Normal callers
	// use shouldBackstop, but a future direct caller must not turn verdict-only
	// review output into a commit, push, or PR.
	if !RequiresPRURL(qw.WorkType) {
		return agent.BackstopReport{}
	}
	report := agent.BackstopReport{Triggered: true}
	worktreePath := res.WorktreePath
	if worktreePath == "" {
		report.Diagnostics = "backstop skipped — no worktree path on result"
		return report
	}

	// Single-source the git identity through buildSessionEnv so backstop
	// commits carry the SAME author as the agent's own in-box commits: a
	// provisioner-stamped GIT_AUTHOR_*/GIT_COMMITTER_* identity wins when
	// present, otherwise the session-derived default is used. Either way
	// this beats whatever the cloud sandbox's git global config says (often
	// absent). runGit appends these overrides AFTER os.Environ() so the chosen
	// identity is authoritative for the commit subprocess.
	sessionEnv := buildSessionEnv(qw)
	id := gitIdentity{
		Name:  sessionEnv["GIT_AUTHOR_NAME"],
		Email: sessionEnv["GIT_AUTHOR_EMAIL"],
	}

	// 1. Sanity check that the worktree has uncommitted changes.
	statusOut, err := runGit(ctx, worktreePath, id, "status", "--porcelain")
	if err != nil {
		report.Diagnostics = fmt.Sprintf("git status failed: %v", err)
		return report
	}
	hasUncommitted := strings.TrimSpace(statusOut) != ""

	// 2. Stage everything.
	if hasUncommitted {
		if _, err := runGit(ctx, worktreePath, id, "add", "-A"); err != nil {
			report.Diagnostics = fmt.Sprintf("git add -A failed: %v", err)
			return report
		}
	}

	// 3. Enumerate staged files and unstage anything excluded.
	stagedOut, err := runGit(ctx, worktreePath, id,
		"-c", "core.quotePath=false",
		"diff", "--cached", "--name-only",
	)
	if err != nil {
		report.Diagnostics = fmt.Sprintf("git diff --cached failed: %v", err)
		return report
	}
	staged := strings.Split(strings.TrimSpace(stagedOut), "\n")
	staged = filterEmpty(staged)
	toUnstage := make([]string, 0, len(staged))
	for _, p := range staged {
		if shouldExcludeFromBackstop(p) {
			toUnstage = append(toUnstage, p)
		}
	}
	for i := 0; i < len(toUnstage); i += backstopUnstageBatch {
		end := i + backstopUnstageBatch
		if end > len(toUnstage) {
			end = len(toUnstage)
		}
		batch := toUnstage[i:end]
		args := append([]string{"reset", "HEAD", "--"}, batch...)
		// Best-effort: a path unknown to git fails the command but we
		// keep going — subsequent safety checks catch any residue.
		_, _ = runGit(ctx, worktreePath, id, args...)
	}

	// 4. Re-check staged count post-filter.
	stagedAfterOut, _ := runGit(ctx, worktreePath, id,
		"-c", "core.quotePath=false",
		"diff", "--cached", "--name-only",
	)
	stagedAfter := filterEmpty(strings.Split(strings.TrimSpace(stagedAfterOut), "\n"))
	if len(stagedAfter) > backstopMaxFiles {
		// Reset the index to leave a clean slate.
		_, _ = runGit(ctx, worktreePath, id, "reset", "HEAD")
		report.Diagnostics = fmt.Sprintf(
			"backstop aborted — %d files staged exceeds safety cap (%d)",
			len(stagedAfter), backstopMaxFiles,
		)
		return report
	}

	// 5. Commit when there's something to commit.
	// Visibility is resolved before composing any public surface so a
	// private work identifier never reaches a public repository's
	// commit message, PR title, or PR body. Only PRIVATE keeps the
	// identifier; PUBLIC and UNKNOWN (including every gh failure) use
	// the redacted format. Continue mode returns before any PR is
	// opened, so defer the gh visibility lookup until the new-PR path
	// actually needs it.
	var vis backstopVisibility
	visibilityResolved := false
	resolveVis := func() backstopVisibility {
		if !visibilityResolved {
			vis = resolveBackstopVisibility(ctx, worktreePath)
			visibilityResolved = true
		}
		return vis
	}
	if len(stagedAfter) > 0 {
		commitMsg := backstopCommitMessage(qw, resolveVis())
		if _, err := runGit(ctx, worktreePath, id, "commit", "-m", commitMsg); err != nil {
			report.Diagnostics = fmt.Sprintf("git commit failed: %v", err)
			return report
		}
	}

	// 6. Publish the work to the SESSION branch, and only there. `branch` is
	// the branch the runner owns for this session (the dispatched branch, or
	// agent/<session>). The commit above landed on whatever is CHECKED OUT,
	// which an agent may have changed: its own new branch, a shared branch it
	// checked out by name (`git checkout develop` tracks origin/develop), a
	// branch that tracks a differently named remote branch, or a detached
	// HEAD. Whatever it is, HEAD is what carries the work, so HEAD is pushed
	// to refs/heads/<session branch> — never to the checked-out branch's own
	// name or its upstream, either of which can be a branch this session does
	// not own. No -u: the checked-out branch's tracking configuration is left
	// alone, and a detached HEAD has none to set.
	//
	// There is deliberately no force retry. A session branch that moved on
	// the remote (a branch name reused across attempts at the same work)
	// holds work this session cannot vouch for; a non-fast-forward fails the
	// backstop with git's own explanation instead of overwriting it.
	if branch == "" {
		report.Diagnostics = "backstop refused to push: no session branch to publish to"
		return report
	}
	if branch == "main" || branch == "master" {
		report.Diagnostics = "backstop refused to push from main/master"
		return report
	}
	if cur, _ := runGit(ctx, worktreePath, id, "branch", "--show-current"); strings.TrimSpace(cur) != branch && r.logger != nil {
		checkedOut := strings.TrimSpace(cur)
		if checkedOut == "" {
			checkedOut = "(detached HEAD)"
		}
		r.logger.Info("backstop publishing the checked-out work to the session branch",
			"sessionId", qw.SessionID, "sessionBranch", branch, "checkedOut", checkedOut)
	}
	if out, err := runGit(ctx, worktreePath, id, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		// Continue-mode divergence is typed: the pull request's head moved
		// after dispatch, so the push is refused rather than forced. The
		// typed flag tells the platform the run needs a fresh dispatch,
		// not a retry of the same head. The ancestry probe (not the push
		// output) decides: a remote head that is not an ancestor of the
		// session's HEAD is divergence, while a policy rejection of an
		// otherwise fast-forward push is not.
		if qw.ContinuePullRequest != nil && continueHeadDiverged(ctx, worktreePath, branch) {
			report.ContinueDiverged = true
			report.Diagnostics = fmt.Sprintf("%s: continued pull request #%d branch %q moved after dispatch; refusing to push: %v\noutput: %s", ErrContinuePullRequestDiverged, qw.ContinuePullRequest.Number, branch, err, out)
			return report
		}
		report.Diagnostics = fmt.Sprintf("git push of the work to session branch %q failed: %v\noutput: %s", branch, err, out)
		return report
	}
	report.Pushed = true

	// 7. Open a PR via the gh CLI. Continue-mode never opens one: the
	// run's pull request is the continued one, already seeded on the
	// envelope before the backstop ran. The pushed branch IS that pull
	// request's head, so report the envelope URL without creating anything.
	if qw.ContinuePullRequest != nil {
		report.PRURL = res.PullRequestURL
		return report
	}
	prTitle := backstopPRTitle(qw, resolveVis())
	prBody := backstopPRBody(qw)
	// --head names the session branch just pushed, so gh never infers the
	// head from the checked-out branch — which may be one this session does
	// not own. An "already exists" PR recovered below is therefore always the
	// session branch's own PR.
	prArgs := []string{"pr", "create", "--head", branch, "--title", prTitle, "--body", prBody}
	if qw.BaseRef != "" {
		prArgs = append(prArgs, "--base", qw.BaseRef)
	}
	prOut, err := runGh(ctx, worktreePath, prArgs...)
	if err != nil {
		// `gh pr create` fails ("a pull request for branch ... already
		// exists:\n<url>") when the branch already has an open PR — e.g. the
		// agent pushed and opened one but its URL never surfaced in the event
		// stream, or a prior backstop already created it. gh prints the
		// existing PR's URL in that failure output (runGh uses CombinedOutput,
		// so stderr is in prOut). An existing PR satisfies the completion
		// contract, so recover the URL and report success rather than a
		// pushed-but-no-PR failure that would reclassify the session failed.
		if u := scanPRURL(prOut); u != "" {
			report.PRURL = u
			report.PRCreated = true
			report.PullRequest = lookupGitHubPullRequest(ctx, worktreePath, report.PRURL, allowedRepositories)
			return report
		}
		report.Diagnostics = fmt.Sprintf("gh pr create failed: %v\noutput: %s", err, prOut)
		return report
	}
	if u := scanPRURL(prOut); u != "" {
		report.PRURL = u
		report.PRCreated = true
	} else {
		// gh pr create prints the URL on its own line; if the regex
		// didn't match, surface the raw output for diagnostics.
		report.PRURL = strings.TrimSpace(prOut)
		report.PRCreated = true
	}
	report.PullRequest = lookupGitHubPullRequest(ctx, worktreePath, report.PRURL, allowedRepositories)
	return report
}

func (r *Runner) runDeclaredBackstops(
	ctx context.Context,
	qw QueuedWork,
	branch string,
	res *Result,
	declaration workarea.NormalizedDeclaration,
	repositoryPaths map[string]string,
) agent.BackstopReport {
	aggregate := agent.BackstopReport{}
	manifestByName := make(map[string]agent.TurnManifestRepository)
	if res.Manifest != nil {
		for _, repository := range manifestRepositoryEntries(res.Manifest) {
			manifestByName[repository.Name] = repository
		}
	}
	for _, repository := range declaration.Repositories {
		if repository.Authority != workarea.RepositoryMutable {
			continue
		}
		allowedRepositories := pullRequestAuthorityForDeclaredSource(repository.Source.Repository)
		repositoryResult := *res
		repositoryResult.WorktreePath = repositoryPaths[repository.Name]
		repositoryResult.BackstopReport = nil
		repositoryResult.PullRequestURL = manifestByName[repository.Name].PullRequestURL
		if repository.Name == declaration.Selected.Name && repositoryResult.PullRequestURL == "" {
			repositoryResult.PullRequestURL = res.PullRequestURL
		}
		report := agent.BackstopReport{PRURL: repositoryResult.PullRequestURL}
		if shouldBackstop(&repositoryResult, qw.WorkType) {
			report = r.runBackstop(ctx, qw, branch, &repositoryResult, allowedRepositories)
		} else if report.PRURL != "" {
			report.PullRequest = lookupGitHubPullRequest(ctx, repositoryResult.WorktreePath, report.PRURL, allowedRepositories)
		}
		aggregate.Repositories = append(aggregate.Repositories, agent.RepositoryBackstopReport{Name: repository.Name, Report: report})
		aggregate.Triggered = aggregate.Triggered || report.Triggered
		aggregate.Pushed = aggregate.Pushed || report.Pushed
		aggregate.PRCreated = aggregate.PRCreated || report.PRCreated
		aggregate.ContinueDiverged = aggregate.ContinueDiverged || report.ContinueDiverged
		aggregate.UnfilledFields = append(aggregate.UnfilledFields, report.UnfilledFields...)
		if report.Diagnostics != "" {
			if aggregate.Diagnostics != "" {
				aggregate.Diagnostics += "; "
			}
			aggregate.Diagnostics += repository.Name + ": " + report.Diagnostics
		}
		if repository.Name == declaration.Selected.Name && report.PRURL != "" {
			aggregate.PRURL = report.PRURL
			if res.PullRequestURL == "" {
				res.PullRequestURL = report.PRURL
			}
			if report.PullRequest != nil {
				aggregate.PullRequest = report.PullRequest
			}
		}
	}
	return aggregate
}

type ghPullRequestView struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	IsDraft     bool   `json:"isDraft"`
}

// ghPullRequestViewFields is the field list of the runner's one `gh pr view`
// query, shared by the pr.opened fact and the draft read.
const ghPullRequestViewFields = "number,url,baseRefName,headRefName,isDraft"

// viewGitHubPullRequest runs the runner's `gh pr view` query for prURL in
// the checkout.
func viewGitHubPullRequest(ctx context.Context, worktreePath, prURL string) (ghPullRequestView, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := runGh(lookupCtx, worktreePath, "pr", "view", prURL, "--json", ghPullRequestViewFields)
	if err != nil {
		return ghPullRequestView{}, fmt.Errorf("gh pr view: %w: %s", err, firstLine(out))
	}
	var view ghPullRequestView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return ghPullRequestView{}, fmt.Errorf("gh pr view: decode: %w", err)
	}
	return view, nil
}

// lookupGitHubPullRequest reads the complete PR projection after gh created
// or detected a URL. The URL-only completion contract remains valid when this
// best-effort metadata query cannot run; only the normalized pr.opened fact is
// omitted in that case.
func lookupGitHubPullRequest(ctx context.Context, worktreePath, prURL string, allowedRepositories map[string]struct{}) *agent.PullRequestFact {
	if strings.TrimSpace(worktreePath) == "" || strings.TrimSpace(prURL) == "" || len(allowedRepositories) == 0 {
		return nil
	}
	view, err := viewGitHubPullRequest(ctx, worktreePath, prURL)
	if err != nil {
		return nil
	}
	return pullRequestFactFromGitHubViewAuthorized(view, allowedRepositories)
}

func pullRequestFactFromGitHubView(view ghPullRequestView) *agent.PullRequestFact {
	repository, number, err := parseCanonicalGitHubPullRequestURL(view.URL)
	if err != nil || number != view.Number {
		return nil
	}
	fact := &agent.PullRequestFact{
		Provider: "github", Number: view.Number, Repository: repository,
		URL: view.URL, BaseBranch: view.BaseRefName, HeadBranch: view.HeadRefName,
	}
	if err := agent.ValidatePullRequestFact(*fact); err != nil {
		return nil
	}
	return fact
}

func pullRequestFactFromGitHubViewAuthorized(view ghPullRequestView, allowedRepositories map[string]struct{}) *agent.PullRequestFact {
	fact := pullRequestFactFromGitHubView(view)
	if fact == nil {
		return nil
	}
	return authorizePullRequestFact(fact, allowedRepositories)
}

func authorizePullRequestFact(fact *agent.PullRequestFact, allowedRepositories map[string]struct{}) *agent.PullRequestFact {
	if fact == nil || !pullRequestRepositoryAllowed(fact.Repository, allowedRepositories) {
		return nil
	}
	return fact
}

func pullRequestRepositoryAllowed(repository string, allowedRepositories map[string]struct{}) bool {
	if len(allowedRepositories) == 0 {
		return false
	}
	_, ok := allowedRepositories[normalizeGitHubRepositorySlug(repository)]
	return ok
}

func pullRequestAuthorityForDeclaredSource(source string) map[string]struct{} {
	slug := workarea.CanonicalGitHubRepositorySource(source)
	if slug == "" {
		return nil
	}
	return map[string]struct{}{slug: {}}
}

func normalizeGitHubRepositorySlug(repository string) string {
	s := strings.TrimSpace(repository)
	if s == "" {
		return ""
	}
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return strings.ToLower(parts[0] + "/" + parts[1])
}

func parseCanonicalGitHubPullRequestURL(raw string) (string, int, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, fmt.Errorf("parse GitHub pull request URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return "", 0, fmt.Errorf("GitHub pull request URL must use https")
	}
	if parsed.User != nil {
		return "", 0, fmt.Errorf("GitHub pull request URL must not include userinfo")
	}
	if !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Hostname() == "" {
		return "", 0, fmt.Errorf("GitHub pull request URL must target github.com")
	}
	if parsed.Port() != "" {
		return "", 0, fmt.Errorf("GitHub pull request URL must not include a port")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", 0, fmt.Errorf("GitHub pull request URL must not include query or fragment")
	}
	parts := strings.Split(parsed.EscapedPath(), "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] == "" || parts[2] == "" || parts[3] != "pull" || parts[4] == "" {
		return "", 0, fmt.Errorf("GitHub pull request URL must match /<owner>/<repo>/pull/<number>")
	}
	number, err := strconv.Atoi(parts[4])
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("GitHub pull request URL must end with a positive pull request number")
	}
	return parts[1] + "/" + parts[2], number, nil
}

func missingMutablePullRequests(res *Result, declaration workarea.NormalizedDeclaration) []string {
	known := make(map[string]string)
	if res != nil && res.Manifest != nil {
		for _, repository := range manifestRepositoryEntries(res.Manifest) {
			known[repository.Name] = repository.PullRequestURL
		}
	}
	if res != nil && res.BackstopReport != nil {
		for _, repository := range res.BackstopReport.Repositories {
			if repository.Report.PRURL != "" {
				known[repository.Name] = repository.Report.PRURL
			}
		}
	}
	if res != nil && res.PullRequestURL != "" {
		known[declaration.Selected.Name] = res.PullRequestURL
	}
	var missing []string
	for _, repository := range declaration.Repositories {
		if repository.Authority == workarea.RepositoryMutable && known[repository.Name] == "" {
			missing = append(missing, repository.Name)
		}
	}
	return missing
}

// gitIdentity carries the GIT_AUTHOR_*/GIT_COMMITTER_* values the
// backstop injects into every git subprocess so commits carry the
// agent's session identity regardless of the sandbox's git global config
// (which is absent in most cloud runners).
type gitIdentity struct {
	Name  string
	Email string
}

// envOverrides returns the four GIT_* env vars as KEY=VALUE strings
// suitable for appending to os.Environ(). Later entries in a process
// environment WIN on Linux/macOS (exec(3) uses the last value for a
// duplicated key when the libc getenv/putenv implementation is used, and
// Go's os/exec passes the slice directly to execve — so appending after
// os.Environ() guarantees the override wins over any inherited value).
func (id gitIdentity) envOverrides() []string {
	if id.Name == "" && id.Email == "" {
		return nil
	}
	return []string{
		"GIT_AUTHOR_NAME=" + id.Name,
		"GIT_AUTHOR_EMAIL=" + id.Email,
		"GIT_COMMITTER_NAME=" + id.Name,
		"GIT_COMMITTER_EMAIL=" + id.Email,
	}
}

// runGit invokes the git binary in cwd with the supplied args and
// returns the combined stdout+stderr trimmed of trailing whitespace.
// The caller chooses ctx; a cancelled ctx aborts the subprocess.
//
// id carries the GIT_AUTHOR_*/GIT_COMMITTER_* overrides that are
// appended AFTER os.Environ() so they WIN over any stale inherited
// values (later entries in an exec env override earlier ones). This is
// the fix for the cloud-runner NO-OP: simply setting cmd.Env =
// os.Environ() is equivalent to leaving it nil (Go inherits the process
// env either way), so the agent-session identity built by buildSessionEnv
// never reached backstop commits. Now the identity is threaded explicitly.
func runGit(ctx context.Context, cwd string, id gitIdentity, args ...string) (string, error) {
	//nolint:gosec // G204: args come from runner-controlled call sites.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	// Append identity overrides AFTER os.Environ() so they WIN, then run the
	// result through gitexec.HardenedEnv to add the headless non-interactive
	// baseline (GIT_TERMINAL_PROMPT=0, GCM_INTERACTIVE=never). The backstop runs
	// in the daemon's workarea under launchd, where a credential/passphrase
	// prompt would hang the push forever; the baseline makes git fail fast
	// instead. suppress/header are off here — the backstop reuses whatever
	// credential the agent session left in the cloned remote (the worktree
	// manager's GitAuth seam is the chokepoint for per-invocation auth at
	// clone time). HardenedEnv with (false, "") appends only the two
	// non-interactive vars, so the identity overrides remain the last identity
	// entries and still win.
	cmd.Env = gitexec.HardenedEnv(runtimeenv.FilterRunnerOnly(append(os.Environ(), id.envOverrides()...)), false, gitexec.Auth{})
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), " \n\t"), err
}

// captureHeadSHA returns the worktree's current HEAD commit sha via
// `git rev-parse HEAD`. It is the runner's correlation-key capture for
// the orchestration-owned durable CI wait
// (ADR-2026-06-10-durable-ci-wait.md): the sha is stamped onto
// Result.CommitSHA at envelope-build time — after tail recovery and the
// backstop, both of which may add commits — so the platform can bind CI
// completion events to this session's pushed head.
//
// The output is validated against the hex object-name shape (40 chars
// for SHA-1 repos, 64 for SHA-256) so a git error message or an
// unborn-HEAD diagnostic is never stamped onto the wire field.
func captureHeadSHA(ctx context.Context, worktreePath string) (string, error) {
	out, err := runGit(ctx, worktreePath, gitIdentity{}, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w (output: %s)", err, out)
	}
	sha := strings.TrimSpace(out)
	if !headSHARE.MatchString(sha) {
		return "", fmt.Errorf("git rev-parse HEAD: unexpected output %q", sha)
	}
	return sha, nil
}

// headSHARE matches a full git object name: 40 lowercase hex chars
// (SHA-1) or 64 (SHA-256 repos).
var headSHARE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// runGh invokes the gh binary similarly to runGit. Kept distinct so
// PATH-lookup failures surface with the right binary name in the
// diagnostic message.
func runGh(ctx context.Context, cwd string, args ...string) (string, error) {
	//nolint:gosec // G204: args come from runner-controlled call sites.
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = cwd
	cmd.Env = runtimeenv.FilterRunnerOnly(cmd.Environ())
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), " \n\t"), err
}

// filterEmpty returns in with empty / whitespace-only entries removed.
// Used when splitting `git diff --cached --name-only` output, which
// emits a trailing empty entry when the diff is empty.
func filterEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
