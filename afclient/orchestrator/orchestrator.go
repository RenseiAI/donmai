// Package orchestrator implements the local orchestrator entrypoint for OSS
// users who do not run the daemon.  It is the Go port of the legacy TS
// af-orchestrator process (packages/cli/src/orchestrator.ts +
// packages/cli/src/lib/orchestrator-runner.ts).
//
// The orchestrator:
//  1. Loads .agentfactory/config.yaml and enforces allowedProjects /
//     projectPaths.
//  2. Validates git remote get-url origin against the repository: field at
//     startup and before each agent spawn.
//  3. Picks Linear backlog issues and dispatches them to provider processes.
//  4. Tracks agent processes and reports results.
//
// Provider dispatch (Claude / Codex) shells out to the provider CLI tool.
// Inject a Dispatcher to override in tests (e.g. dry-run or mock).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient/repoconfig"
	"github.com/RenseiAI/agentfactory-tui/internal/linear"
)

// DispatchStatus is the lifecycle status of a dispatched agent.
type DispatchStatus string

const (
	// DispatchStarting means the agent process has been spawned but hasn't
	// started processing yet.
	DispatchStarting DispatchStatus = "starting"
	// DispatchRunning means the agent process is actively working.
	DispatchRunning DispatchStatus = "running"
	// DispatchCompleted means the agent process exited successfully.
	DispatchCompleted DispatchStatus = "completed"
	// DispatchFailed means the agent process exited with a non-zero status.
	DispatchFailed DispatchStatus = "failed"
	// DispatchSkipped is used in dry-run mode to indicate an issue would have
	// been dispatched.
	DispatchSkipped DispatchStatus = "skipped"
)

// AgentDispatch tracks the lifecycle of a single dispatched agent.
type AgentDispatch struct {
	// IssueID is the Linear issue UUID.
	IssueID string
	// Identifier is the human-readable issue identifier (e.g. "REN-42").
	Identifier string
	// Title is the issue title.
	Title string
	// Project is the Linear project name.
	Project string
	// Status is the current lifecycle status of the agent.
	Status DispatchStatus
	// StartedAt is the time the agent was dispatched.
	StartedAt time.Time
	// CompletedAt is the time the agent completed (zero when still running).
	CompletedAt time.Time
	// ExitCode is the exit code of the agent process (0 = success).
	ExitCode int
	// Error holds any error encountered during dispatch or execution.
	Error error
}

// Dispatcher decides how to run an agent for an issue.  The default
// implementation shells out to the provider CLI.  Tests inject a mock.
type Dispatcher interface {
	Dispatch(ctx context.Context, issue linear.Issue, cfg Config) (*AgentDispatch, error)
}

// Config carries all settings for an Orchestrator run.
type Config struct {
	// LinearAPIKey is the API key for Linear authentication.
	LinearAPIKey string
	// Project filters issues to a single Linear project name.  Empty means all
	// allowed projects from config.yaml are scanned.
	Project string
	// Single is a Linear issue ID to process exactly one issue.
	Single string
	// Max is the maximum number of concurrent agent dispatches (default: 3).
	Max int
	// DryRun prints what would be dispatched without actually spawning agents.
	DryRun bool
	// Repository is the git remote URL pattern validated against origin.
	// Overrides the repository: field from config.yaml when set.
	Repository string
	// TemplateDir is the path to custom workflow template YAML files.
	TemplateDir string
	// GitRoot is the root of the git repository.  Defaults to the directory
	// returned by `git rev-parse --show-toplevel`.
	GitRoot string
	// Logger receives structured log output.  Defaults to slog.Default().
	Logger *slog.Logger
}

// Result summarises a completed orchestrator run.
type Result struct {
	// Dispatched is the list of agents that were dispatched (or would have been
	// dispatched in dry-run mode).
	Dispatched []*AgentDispatch
	// Errors holds per-issue errors that occurred during the run.
	Errors []error
}

// Orchestrator is the local orchestrator that picks Linear issues and
// dispatches agents.
type Orchestrator struct {
	cfg        Config
	repoConfig *repoconfig.RepositoryConfig
	linClient  linear.Linear
	dispatcher Dispatcher
	logger     *slog.Logger
}

// providerDispatcher is the production Dispatcher implementation that shells
// out to the claude / codex CLI tool.
type providerDispatcher struct{}

// Dispatch shells out to the provider CLI to run an agent for the given issue.
// In the Go port the provider binary is resolved from PATH (claude, codex, etc.).
// The dispatch is fire-and-forget — the orchestrator tracks the os.Process and
// waits for completion in a goroutine.
func (d *providerDispatcher) Dispatch(ctx context.Context, issue linear.Issue, cfg Config) (*AgentDispatch, error) {
	ad := &AgentDispatch{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Title:      issue.Title,
		Project:    issue.Project.Name,
		Status:     DispatchStarting,
		StartedAt:  time.Now(),
	}

	// Resolve the provider CLI binary.  We try "claude" first (most common),
	// then "codex" as a fallback.  This matches the legacy TS default of the
	// claude provider.
	providerBin := resolveProviderBin()
	if providerBin == "" {
		ad.Status = DispatchFailed
		ad.Error = errors.New("no provider binary found (tried: claude, codex)")
		return ad, ad.Error
	}

	args := buildProviderArgs(issue, cfg)
	cmd := exec.CommandContext(ctx, providerBin, args...) //nolint:gosec
	cmd.Env = append(os.Environ(),
		"LINEAR_ISSUE_ID="+issue.ID,
		"LINEAR_ISSUE_IDENTIFIER="+issue.Identifier,
	)

	if err := cmd.Start(); err != nil {
		ad.Status = DispatchFailed
		ad.Error = fmt.Errorf("dispatch: start %s: %w", providerBin, err)
		return ad, ad.Error
	}

	ad.Status = DispatchRunning

	// Wait for completion in background.
	go func() {
		if werr := cmd.Wait(); werr != nil {
			var exitErr *exec.ExitError
			if errors.As(werr, &exitErr) {
				ad.ExitCode = exitErr.ExitCode()
			}
			ad.Status = DispatchFailed
			ad.Error = werr
		} else {
			ad.Status = DispatchCompleted
			ad.ExitCode = 0
		}
		ad.CompletedAt = time.Now()
	}()

	return ad, nil
}

// resolveProviderBin returns the first provider binary found in PATH.
func resolveProviderBin() string {
	for _, bin := range []string{"claude", "codex"} {
		if p, err := exec.LookPath(bin); err == nil {
			return p
		}
	}
	return ""
}

// buildProviderArgs builds the argument list for the provider binary based on
// the issue and orchestrator config.
func buildProviderArgs(issue linear.Issue, cfg Config) []string {
	// The provider CLI is expected to accept `--print` mode and a prompt
	// describing the issue to work on.  The exact flags depend on the
	// provider, but we standardise on the claude-code `--print` + prompt form.
	prompt := fmt.Sprintf("Work on Linear issue %s: %s", issue.Identifier, issue.Title)
	args := []string{"--print", prompt}
	if cfg.TemplateDir != "" {
		args = append(args, "--templates", cfg.TemplateDir)
	}
	return args
}

// New creates an Orchestrator with a real Linear client and provider dispatcher.
// It validates and loads .agentfactory/config.yaml when present.
func New(cfg Config) (*Orchestrator, error) {
	if cfg.LinearAPIKey == "" {
		cfg.LinearAPIKey = os.Getenv("LINEAR_API_KEY")
	}
	if cfg.LinearAPIKey == "" {
		return nil, errors.New("orchestrator: LINEAR_API_KEY is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	linClient, err := linear.NewClient(cfg.LinearAPIKey)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	o := &Orchestrator{
		cfg:        cfg,
		linClient:  linClient,
		dispatcher: &providerDispatcher{},
		logger:     logger,
	}

	// Resolve git root.
	if o.cfg.GitRoot == "" {
		root, rootErr := gitRevParseTopLevel()
		if rootErr != nil {
			return nil, fmt.Errorf("orchestrator: resolve git root: %w", rootErr)
		}
		o.cfg.GitRoot = root
	}

	// Load .agentfactory/config.yaml (optional — not an error when absent).
	rc, rcErr := repoconfig.Load(o.cfg.GitRoot)
	if rcErr != nil && !errors.Is(rcErr, repoconfig.ErrConfigNotFound) {
		return nil, fmt.Errorf("orchestrator: load config: %w", rcErr)
	}
	o.repoConfig = rc // nil when not found

	// Resolve repository from config when not set via flag.
	if o.cfg.Repository == "" && o.repoConfig != nil {
		o.cfg.Repository = o.repoConfig.Repository
	}

	// Validate git remote at startup when a repository pattern is configured.
	if o.cfg.Repository != "" {
		if verr := ValidateGitRemote(o.cfg.Repository, o.cfg.GitRoot); verr != nil {
			return nil, verr
		}
	}

	return o, nil
}

// WithDispatcher replaces the dispatcher — primarily used in tests.
func (o *Orchestrator) WithDispatcher(d Dispatcher) {
	o.dispatcher = d
}

// WithLinearClient replaces the Linear client — primarily used in tests.
func (o *Orchestrator) WithLinearClient(l linear.Linear) {
	o.linClient = l
}

// NewForTest creates an Orchestrator using the provided Linear client and a
// no-op dispatcher.  Git-remote validation and config.yaml loading are
// skipped when no repository is set (reducing test friction).
//
// This constructor is intentionally exported so _test packages can call it
// without losing the encapsulation of the production New() path.
func NewForTest(cfg Config, lin linear.Linear) (*Orchestrator, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	o := &Orchestrator{
		cfg:        cfg,
		linClient:  lin,
		dispatcher: &providerDispatcher{},
		logger:     logger,
	}

	// Resolve git root if not provided.
	if o.cfg.GitRoot == "" {
		root, err := gitRevParseTopLevel()
		if err != nil {
			return nil, fmt.Errorf("orchestrator: resolve git root: %w", err)
		}
		o.cfg.GitRoot = root
	}

	// Load config.yaml (ignore ErrConfigNotFound).
	rc, rcErr := repoconfig.Load(o.cfg.GitRoot)
	if rcErr != nil && !errors.Is(rcErr, repoconfig.ErrConfigNotFound) {
		return nil, fmt.Errorf("orchestrator: load config: %w", rcErr)
	}
	o.repoConfig = rc

	// Repository from config.
	if o.cfg.Repository == "" && o.repoConfig != nil {
		o.cfg.Repository = o.repoConfig.Repository
	}

	return o, nil
}

// Run executes the orchestrator loop:
//  1. Fetch backlog issues.
//  2. Apply project allowlist and --max cap.
//  3. Dispatch agents (or log in dry-run mode).
//  4. Wait for all agents to complete.
func (o *Orchestrator) Run(ctx context.Context) (*Result, error) {
	// Re-validate git remote before each run (mirrors TS behaviour).
	if o.cfg.Repository != "" {
		if err := ValidateGitRemote(o.cfg.Repository, o.cfg.GitRoot); err != nil {
			return nil, err
		}
	}

	result := &Result{}

	if o.cfg.Single != "" {
		return o.runSingle(ctx, result)
	}
	return o.runBacklog(ctx, result)
}

// runSingle processes exactly one issue identified by cfg.Single.
func (o *Orchestrator) runSingle(ctx context.Context, result *Result) (*Result, error) {
	issue, err := o.linClient.GetIssue(ctx, o.cfg.Single)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: get issue %s: %w", o.cfg.Single, err)
	}

	// Enforce project allowlist.
	if o.repoConfig != nil && !o.repoConfig.IsProjectAllowed(issue.Project.Name) {
		return nil, fmt.Errorf(
			"orchestrator: issue %s belongs to project %q which is not in allowedProjects",
			issue.Identifier, issue.Project.Name,
		)
	}

	if o.cfg.DryRun {
		o.logger.Info("dry-run: would dispatch", "issue", issue.Identifier, "title", issue.Title)
		result.Dispatched = append(result.Dispatched, &AgentDispatch{
			IssueID:    issue.ID,
			Identifier: issue.Identifier,
			Title:      issue.Title,
			Project:    issue.Project.Name,
			Status:     DispatchSkipped,
			StartedAt:  time.Now(),
		})
		return result, nil
	}

	ad, err := o.dispatcher.Dispatch(ctx, *issue, o.cfg)
	if err != nil {
		result.Errors = append(result.Errors, err)
	}
	result.Dispatched = append(result.Dispatched, ad)
	return result, nil
}

// runBacklog picks backlog issues and dispatches up to cfg.Max agents
// concurrently.
func (o *Orchestrator) runBacklog(ctx context.Context, result *Result) (*Result, error) {
	projects, err := o.resolveProjects()
	if err != nil {
		return nil, err
	}

	maxInt := o.cfg.Max
	if maxInt <= 0 {
		maxInt = 3
	}

	var (
		mu  sync.Mutex
		sem = make(chan struct{}, maxInt)
		wg  sync.WaitGroup
	)

	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			break
		}

		issues, listErr := o.linClient.ListIssuesByProject(ctx, project, []string{"Backlog"})
		if listErr != nil {
			o.logger.Error("orchestrator: list issues", "project", project, "error", listErr)
			mu.Lock()
			result.Errors = append(result.Errors, fmt.Errorf("project %q: %w", project, listErr))
			mu.Unlock()
			continue
		}

		for _, issue := range issues {
			if err := ctx.Err(); err != nil {
				goto done
			}

			// Re-validate git remote before each spawn.
			if o.cfg.Repository != "" {
				if verr := ValidateGitRemote(o.cfg.Repository, o.cfg.GitRoot); verr != nil {
					mu.Lock()
					result.Errors = append(result.Errors, verr)
					mu.Unlock()
					goto done
				}
			}

			if o.cfg.DryRun {
				o.logger.Info("dry-run: would dispatch",
					"issue", issue.Identifier,
					"title", issue.Title,
					"project", issue.Project.Name,
				)
				mu.Lock()
				result.Dispatched = append(result.Dispatched, &AgentDispatch{
					IssueID:    issue.ID,
					Identifier: issue.Identifier,
					Title:      issue.Title,
					Project:    issue.Project.Name,
					Status:     DispatchSkipped,
					StartedAt:  time.Now(),
				})
				mu.Unlock()
				continue
			}

			// Acquire semaphore slot before spawning.
			sem <- struct{}{}
			wg.Add(1)

			issueCopy := issue
			go func() {
				defer func() {
					<-sem
					wg.Done()
				}()

				ad, dispErr := o.dispatcher.Dispatch(ctx, issueCopy, o.cfg)
				mu.Lock()
				if dispErr != nil {
					result.Errors = append(result.Errors, dispErr)
				}
				if ad != nil {
					result.Dispatched = append(result.Dispatched, ad)
				}
				mu.Unlock()

				// Wait for the agent to complete before releasing the semaphore slot.
				if ad != nil {
					for ad.Status == DispatchStarting || ad.Status == DispatchRunning {
						time.Sleep(500 * time.Millisecond)
						if err := ctx.Err(); err != nil {
							return
						}
					}
				}
			}()
		}
	}

done:
	wg.Wait()
	return result, nil
}

// resolveProjects returns the ordered list of Linear project names to scan.
// Priority: --project flag > allowedProjects / projectPaths from config.yaml.
func (o *Orchestrator) resolveProjects() ([]string, error) {
	if o.cfg.Project != "" {
		// Enforce allowlist when a config is loaded.
		if o.repoConfig != nil && !o.repoConfig.IsProjectAllowed(o.cfg.Project) {
			return nil, fmt.Errorf(
				"orchestrator: project %q is not in allowedProjects",
				o.cfg.Project,
			)
		}
		return []string{o.cfg.Project}, nil
	}

	if o.repoConfig != nil {
		if allowed := o.repoConfig.GetEffectiveAllowedProjects(); len(allowed) > 0 {
			return allowed, nil
		}
	}

	return nil, errors.New("orchestrator: no project specified (use --project or set allowedProjects in .agentfactory/config.yaml)")
}

// ValidateGitRemote validates that the git remote origin URL matches the
// expectedRepo pattern.  It supports both HTTPS and SSH URL formats.
//
// This is a package-level function so it can be called from command wiring
// without constructing a full Orchestrator.
func ValidateGitRemote(expectedRepo, cwd string) error {
	args := []string{"remote", "get-url", "origin"}
	var cmd *exec.Cmd
	if cwd != "" {
		cmd = exec.Command("git", args...) //nolint:gosec
		cmd.Dir = cwd
	} else {
		cmd = exec.Command("git", args...) //nolint:gosec
	}

	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf(
			"orchestrator: repository validation failed: could not get git remote URL (expected %q): %w",
			expectedRepo, err,
		)
	}

	remoteURL := strings.TrimSpace(string(out))

	// Normalise both sides for comparison:
	//   git@github.com:org/repo.git → github.com/org/repo
	//   https://github.com/org/repo.git → github.com/org/repo
	normalizeURL := func(u string) string {
		u = strings.TrimSuffix(u, ".git")
		// SSH: git@github.com:org/repo → github.com/org/repo
		if idx := strings.Index(u, "@"); idx >= 0 {
			u = u[idx+1:]
			u = strings.Replace(u, ":", "/", 1)
		}
		// Strip https:// or http://
		u = strings.TrimPrefix(u, "https://")
		u = strings.TrimPrefix(u, "http://")
		return u
	}

	normRemote := normalizeURL(remoteURL)
	normExpected := normalizeURL(expectedRepo)

	if !strings.Contains(normRemote, normExpected) {
		return fmt.Errorf(
			"orchestrator: repository mismatch: expected %q but git remote is %q; refusing to proceed",
			expectedRepo, remoteURL,
		)
	}

	return nil
}

// gitRevParseTopLevel returns the root of the git repository containing the
// current working directory.
func gitRevParseTopLevel() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
