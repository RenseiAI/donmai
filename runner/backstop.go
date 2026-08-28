package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/gitexec"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
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
// only when they are the top-level component. Mirrors
// EXCLUDE_DIR_TOP_LEVEL from the legacy TS.
//
// `.agent` is the runner's own state dir; the backstop must never
// commit it even if a previous commit accidentally tracked it.
var excludeDirTopLevel = []string{
	".agent",
}

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
	// file *named* ".agent".
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
// workType gates the chain on result-sensitivity (isResultSensitive,
// sdlc.go): a work type whose completion requires no PR/branch
// artifact (e.g. WorkTypeBacklogGroomer / research / refinement) is
// never backstopped into an empty marker PR. This is the root-cause
// fix for the empty-commit bug: a backlog groomer that posts a comment
// and exits has nothing to commit, so the deterministic git backstop
// must not run. Development / qa / acceptance are result-sensitive and
// keep their existing backstop flow.
func shouldBackstop(res *Result, workType string) bool {
	if res == nil {
		return false
	}
	// Contract gate: non-result-sensitive work types never enter the
	// deterministic git backstop — there is no code to commit/push.
	if !isResultSensitive(workType) {
		return false
	}
	switch res.FailureMode {
	case FailureLostOwnership, FailureTimeout, FailureProviderResolve, FailureAgentBlocked, FailureOperatorCancelled:
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
//  6. `git push -u origin <branch>` (with --force-with-lease retry on
//     non-fast-forward).
//  7. `gh pr create --fill` — return the URL.
//
// Errors at any step short-circuit and are recorded on
// BackstopReport.Diagnostics; the caller decides whether to surface
// them as Result.FailureMode = FailureBackstop.
//
//nolint:gocyclo,funlen // step ordering is the package's contract; splitting hides intent
func (r *Runner) runBackstop(ctx context.Context, qw QueuedWork, branch string, res *Result) agent.BackstopReport {
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
	if len(stagedAfter) > 0 {
		commitMsg := fmt.Sprintf("Backstop: %s", qw.SessionID)
		if qw.IssueIdentifier != "" {
			commitMsg = fmt.Sprintf("Backstop: %s (%s)", qw.IssueIdentifier, qw.SessionID)
		}
		if _, err := runGit(ctx, worktreePath, id, "commit", "-m", commitMsg); err != nil {
			report.Diagnostics = fmt.Sprintf("git commit failed: %v", err)
			return report
		}
	}

	// 6. Push.
	pushBranch := branch
	if pushBranch == "" {
		// Derive from current branch.
		bOut, _ := runGit(ctx, worktreePath, id, "branch", "--show-current")
		pushBranch = strings.TrimSpace(bOut)
	}
	if pushBranch == "" || pushBranch == "main" || pushBranch == "master" {
		report.Diagnostics = "backstop refused to push from main/master"
		return report
	}
	pushArgs := []string{"push", "-u", "origin", pushBranch}
	if _, err := runGit(ctx, worktreePath, id, pushArgs...); err != nil {
		// Try force-with-lease on diverged history.
		errMsg := err.Error()
		if !strings.Contains(errMsg, "non-fast-forward") && !strings.Contains(errMsg, "rejected") {
			report.Diagnostics = fmt.Sprintf("git push failed: %v", err)
			return report
		}
		forceArgs := []string{"push", "--force-with-lease", "-u", "origin", pushBranch}
		if _, err := runGit(ctx, worktreePath, id, forceArgs...); err != nil {
			report.Diagnostics = fmt.Sprintf("git push --force-with-lease failed: %v", err)
			return report
		}
	}
	report.Pushed = true

	// 7. Open a PR via the gh CLI.
	prTitle := commitSubject(qw)
	if !strings.Contains(prTitle, qw.IssueIdentifier) && qw.IssueIdentifier != "" {
		prTitle = qw.IssueIdentifier + ": " + prTitle
	}
	prBody := fmt.Sprintf(
		"## Summary\n\nAuto-recovered by the runner backstop for session %s.\n\n"+
			"The agent finished without opening a pull request. The backstop "+
			"committed pending changes (excluding build artifacts) and opened "+
			"this PR so the work is not lost.",
		qw.SessionID,
	)
	prOut, err := runGh(ctx, worktreePath, "pr", "create",
		"--title", prTitle,
		"--body", prBody,
	)
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
			report.PullRequest = lookupGitHubPullRequest(ctx, worktreePath, report.PRURL)
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
	report.PullRequest = lookupGitHubPullRequest(ctx, worktreePath, report.PRURL)
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
		repositoryResult := *res
		repositoryResult.WorktreePath = repositoryPaths[repository.Name]
		repositoryResult.BackstopReport = nil
		repositoryResult.PullRequestURL = manifestByName[repository.Name].PullRequestURL
		if repository.Name == declaration.Selected.Name && repositoryResult.PullRequestURL == "" {
			repositoryResult.PullRequestURL = res.PullRequestURL
		}
		report := agent.BackstopReport{PRURL: repositoryResult.PullRequestURL}
		if shouldBackstop(&repositoryResult, qw.WorkType) {
			report = r.runBackstop(ctx, qw, branch, &repositoryResult)
		}
		aggregate.Repositories = append(aggregate.Repositories, agent.RepositoryBackstopReport{Name: repository.Name, Report: report})
		aggregate.Triggered = aggregate.Triggered || report.Triggered
		aggregate.Pushed = aggregate.Pushed || report.Pushed
		aggregate.PRCreated = aggregate.PRCreated || report.PRCreated
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
	Repository  struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// lookupGitHubPullRequest reads the complete PR projection after gh created
// or detected a URL. The URL-only completion contract remains valid when this
// best-effort metadata query cannot run; only the normalized pr.opened fact is
// omitted in that case.
func lookupGitHubPullRequest(ctx context.Context, worktreePath, prURL string) *agent.PullRequestFact {
	if strings.TrimSpace(worktreePath) == "" || strings.TrimSpace(prURL) == "" {
		return nil
	}
	allowedRepositories := pullRequestAuthorityForWorktree(ctx, worktreePath)
	if len(allowedRepositories) == 0 {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := runGh(lookupCtx, worktreePath, "pr", "view", prURL,
		"--json", "number,url,baseRefName,headRefName,repository")
	if err != nil {
		return nil
	}
	var view ghPullRequestView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return nil
	}
	return pullRequestFactFromGitHubViewAuthorized(view, allowedRepositories)
}

func pullRequestFactFromGitHubView(view ghPullRequestView) *agent.PullRequestFact {
	fact := &agent.PullRequestFact{
		Provider: "github", Number: view.Number, Repository: view.Repository.NameWithOwner,
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
	_, ok := allowedRepositories[githubRepositorySlug(repository)]
	return ok
}

func pullRequestAuthorityForWorktree(ctx context.Context, worktreePath string) map[string]struct{} {
	if strings.TrimSpace(worktreePath) == "" {
		return nil
	}
	out, err := runGit(ctx, worktreePath, gitIdentity{}, "remote", "get-url", "origin")
	if err != nil {
		return nil
	}
	slug := githubRepositorySlug(out)
	if slug == "" {
		return nil
	}
	return map[string]struct{}{slug: {}}
}

func githubRepositorySlug(repository string) string {
	s := strings.TrimSpace(repository)
	if s == "" {
		return ""
	}
	s = strings.TrimSuffix(s, ".git")
	if i := strings.Index(s, ":"); i >= 0 && !strings.Contains(s[:i], "/") {
		s = s[i+1:]
	} else if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if i := strings.Index(s, "/"); i >= 0 {
			s = s[i+1:]
		}
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.ToLower(parts[len(parts)-2] + "/" + parts[len(parts)-1])
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
