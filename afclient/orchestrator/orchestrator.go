// Package orchestrator implements the local orchestrator entrypoint for OSS
// users who do not run the daemon.  It is the Go port of the legacy TS
// af-orchestrator process (packages/cli/src/orchestrator.ts +
// packages/cli/src/lib/orchestrator-runner.ts).
//
// The orchestrator:
//  1. Loads .donmai/config.yaml and enforces allowedProjects /
//     projectPaths.
//  2. Validates git remote get-url origin against the repository: field at
//     startup and before each agent spawn.
//  3. Picks Linear backlog issues and dispatches them to provider processes.
//  4. Tracks agent processes and reports results.
//
// Provider dispatch (Claude / Codex) uses the existing native harness
// provider implementations via an injected ProviderFactory — never a
// provider-argv adapter. Inject a Dispatcher to override in tests
// (e.g. dry-run or mock).
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

	"github.com/RenseiAI/donmai/afclient/repoconfig"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/linear"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/templates"
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
	// Identifier is the human-readable issue identifier (e.g. "ENG-42").
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
// implementation spawns the selected native harness provider and awaits
// its terminal outcome.  Tests inject a mock.
type Dispatcher interface {
	Dispatch(ctx context.Context, issue linear.Issue, cfg Config) (*AgentDispatch, error)
}

// Supported Harness values for Config.Harness.
const (
	// HarnessAuto probes the supported harnesses in documented order
	// (claude, then codex) and uses the first whose constructor
	// succeeds.  Probe failures name the missing binary.
	HarnessAuto = "auto"
	// HarnessClaude selects the claude harness provider.
	HarnessClaude = "claude"
	// HarnessCodex selects the codex harness provider (app-server JSON-RPC).
	HarnessCodex = "codex"
)

// ProviderFactory constructs a native harness provider for one dispatch.
// The orchestrator owns the returned provider's lifecycle: it spawns one
// session, awaits the terminal event, then shuts the provider down.
// Implementations must be safe for concurrent use — backlog runs call
// NewProvider from multiple workers.
type ProviderFactory interface {
	NewProvider(ctx context.Context, harness string) (agent.Provider, error)
}

// defaultProviderFactory builds the real harness providers compiled into
// this binary.  "auto" tries claude first, then codex, mirroring the
// historical provider preference without passing one harness's argv to
// another: each provider owns its native spawn protocol.
type defaultProviderFactory struct{}

// NewProvider constructs the named harness provider, probing fail-fast
// (a missing binary returns an error wrapping agent.ErrProviderUnavailable).
func (defaultProviderFactory) NewProvider(_ context.Context, harness string) (agent.Provider, error) {
	switch harness {
	case HarnessClaude:
		return claude.New(claude.Options{})
	case HarnessCodex:
		return codex.New(codex.Options{})
	default:
		return nil, fmt.Errorf("orchestrator: unsupported harness %q (want %q or %q)", harness, HarnessClaude, HarnessCodex)
	}
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
	// When set, the directory must exist and load as a template registry;
	// template load failure fails the dispatch before any provider spawns.
	TemplateDir string
	// Harness selects the native harness provider ("auto", "claude" or
	// "codex").  Empty means "auto".  Explicit values fail closed when
	// the selected provider is unavailable; there is no silent fallback
	// to the other harness.
	Harness string
	// ProviderFactory constructs the harness provider for each dispatch.
	// Nil uses the default factory (real claude/codex constructors).
	// Tests inject a stub factory via NewForTest or WithProviderFactory.
	ProviderFactory ProviderFactory
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

// nativeDispatcher is the production Dispatcher implementation.  It
// resolves the harness (explicit or auto), constructs the native harness
// provider via the configured ProviderFactory, builds an agent.Spec from
// the issue plus the rendered prompt templates, spawns one session,
// awaits its terminal ResultEvent/ErrorEvent (or stream closure), maps
// the outcome onto the AgentDispatch, and shuts the owned provider down.
// Dispatch is synchronous: the returned dispatch is always terminal.
type nativeDispatcher struct {
	factory ProviderFactory
}

// Dispatch runs one issue on the selected native harness provider and
// awaits its terminal outcome before returning. Cleanup failures join
// the returned error: a successful terminal result followed by a
// Shutdown failure still reports failure to the caller, and a terminal
// error is never lost to (or masked by) a Shutdown failure.
func (d *nativeDispatcher) Dispatch(ctx context.Context, issue linear.Issue, cfg Config) (ad *AgentDispatch, retErr error) {
	ad = &AgentDispatch{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Title:      issue.Title,
		Project:    issue.Project.Name,
		Status:     DispatchStarting,
		StartedAt:  time.Now(),
	}

	harness, err := resolveHarness(cfg.Harness)
	if err != nil {
		return failDispatch(ad, err), err
	}

	spec, buildErr := buildAgentSpec(issue, cfg)
	if buildErr != nil {
		return failDispatch(ad, buildErr), buildErr
	}

	factory := d.factory
	if factory == nil {
		factory = defaultProviderFactory{}
	}

	provider, perr := acquireProvider(ctx, factory, harness)
	if perr != nil {
		return failDispatch(ad, perr), perr
	}

	ad.Status = DispatchRunning
	// The deferred cleanup mutates the named return: plain `return ad,
	// ad.Error` values are evaluated BEFORE deferred calls run, so
	// without the retErr assignment below a Shutdown failure after a
	// successful terminal result would label the dispatch failed while
	// the caller still received a nil error.
	defer func() {
		ad.CompletedAt = time.Now()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if serr := provider.Shutdown(shutdownCtx); serr != nil {
			serr = fmt.Errorf("orchestrator: shutdown %s provider: %w", harness, serr)
			ad.Status = DispatchFailed
			if ad.Error == nil {
				ad.Error = serr
			} else {
				ad.Error = errors.Join(ad.Error, serr)
			}
			retErr = ad.Error
		}
	}()

	handle, serr := provider.Spawn(ctx, spec)
	if serr != nil {
		return failDispatch(ad, fmt.Errorf("orchestrator: spawn %s: %w", harness, serr)), ad.Error
	}
	stopOnCancel := context.AfterFunc(ctx, func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = handle.Stop(stopCtx)
	})
	defer stopOnCancel()

	if terr := awaitTerminal(ctx, handle, ad); terr != nil {
		return ad, terr
	}
	return ad, ad.Error
}

// failDispatch marks a dispatch failed and records the error.
func failDispatch(ad *AgentDispatch, err error) *AgentDispatch {
	ad.Status = DispatchFailed
	ad.CompletedAt = time.Now()
	ad.Error = err
	return ad
}

// resolveHarness normalises the configured harness name.  Empty means
// auto.  Unknown names fail closed.
func resolveHarness(harness string) (string, error) {
	switch strings.TrimSpace(harness) {
	case "", HarnessAuto:
		return HarnessAuto, nil
	case HarnessClaude:
		return HarnessClaude, nil
	case HarnessCodex:
		return HarnessCodex, nil
	default:
		return "", fmt.Errorf("orchestrator: unsupported harness %q (want %q, %q or %q)", harness, HarnessAuto, HarnessClaude, HarnessCodex)
	}
}

// acquireProvider constructs the provider for one dispatch.  "auto"
// tries claude first, then codex, and joins both probe errors when
// neither is available.  Explicit harnesses fail closed with the single
// probe error — there is no silent fallback to the other harness.
func acquireProvider(ctx context.Context, factory ProviderFactory, harness string) (agent.Provider, error) {
	if harness != HarnessAuto {
		p, err := factory.NewProvider(ctx, harness)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: %s provider unavailable: %w", harness, err)
		}
		return p, nil
	}
	claudeProvider, claudeErr := factory.NewProvider(ctx, HarnessClaude)
	if claudeErr == nil {
		return claudeProvider, nil
	}
	codexProvider, codexErr := factory.NewProvider(ctx, HarnessCodex)
	if codexErr == nil {
		return codexProvider, nil
	}
	return nil, fmt.Errorf("orchestrator: no provider available (tried: %s, %s): %w; %w", HarnessClaude, HarnessCodex, claudeErr, codexErr)
}

// buildAgentSpec renders the issue prompt (custom templates when
// configured, built-in registry otherwise) and projects it onto an
// agent.Spec.  No template path is ever forwarded as provider argv —
// each harness owns its native argument translation.
func buildAgentSpec(issue linear.Issue, cfg Config) (agent.Spec, error) {
	userPrompt, systemAppend, err := renderIssuePrompt(issue, cfg)
	if err != nil {
		return agent.Spec{}, err
	}
	cwd := cfg.GitRoot
	env := map[string]string{
		"LINEAR_ISSUE_ID":         issue.ID,
		"LINEAR_ISSUE_IDENTIFIER": issue.Identifier,
	}
	return agent.Spec{
		Prompt:             userPrompt,
		SystemPromptAppend: systemAppend,
		Cwd:                cwd,
		Env:                env,
		Autonomous:         true,
	}, nil
}

// renderIssuePrompt renders the (system, user) prompt pair for an issue.
// A configured TemplateDir must exist and load; failure fails the
// dispatch before any provider spawns.  The built-in registry always
// renders the development template.
func renderIssuePrompt(issue linear.Issue, cfg Config) (user, systemAppend string, err error) {
	qw := prompt.QueuedWork{
		SessionID:       issue.ID,
		IssueID:         issue.ID,
		IssueIdentifier: issue.Identifier,
		Title:           issue.Title,
		Body:            issue.Description,
		ProjectName:     issue.Project.Name,
		Repository:      cfg.Repository,
		WorkType:        "development",
	}
	if cfg.TemplateDir != "" {
		info, statErr := os.Stat(cfg.TemplateDir)
		if statErr != nil || !info.IsDir() {
			return "", "", fmt.Errorf("orchestrator: template dir %q: %w", cfg.TemplateDir, statErrOrNotDir(statErr))
		}
		reg, loadErr := templates.NewFromFS(os.DirFS(cfg.TemplateDir), ".")
		if loadErr != nil {
			return "", "", fmt.Errorf("orchestrator: load templates from %q: %w", cfg.TemplateDir, loadErr)
		}
		builder := &prompt.Builder{Registry: reg}
		system, userPrompt, buildErr := builder.Build(qw)
		if buildErr != nil {
			return "", "", fmt.Errorf("orchestrator: render custom template: %w", buildErr)
		}
		return userPrompt, system, nil
	}
	builder := prompt.NewBuilder()
	system, userPrompt, buildErr := builder.Build(qw)
	if buildErr != nil {
		return "", "", fmt.Errorf("orchestrator: render prompt: %w", buildErr)
	}
	return userPrompt, system, nil
}

func statErrOrNotDir(err error) error {
	if err != nil {
		return err
	}
	return errors.New("not a directory")
}

// awaitTerminal consumes the handle's event stream until the terminal
// ResultEvent/ErrorEvent arrives or the stream closes.  A ResultEvent
// with Success=false, an ErrorEvent, a premature stream closure, or
// context cancellation all map to DispatchFailed — none can appear
// successful.
func awaitTerminal(ctx context.Context, handle agent.Handle, ad *AgentDispatch) error {
	events := handle.Events()
	if events == nil {
		return failDispatchErr(ad, errors.New("orchestrator: provider returned nil event stream"))
	}
	for {
		select {
		case <-ctx.Done():
			stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			stopErr := handle.Stop(stopCtx)
			cancel()
			if stopErr != nil {
				return failDispatchErr(ad, fmt.Errorf("orchestrator: stop after cancel: %w", context.Cause(ctx)))
			}
			return failDispatchErr(ad, fmt.Errorf("orchestrator: dispatch canceled: %w", context.Cause(ctx)))
		case ev, ok := <-events:
			if !ok {
				// The provider closed the stream without a terminal
				// event.  That is a failure — never success.
				if ad.Status != DispatchFailed && ad.Status != DispatchCompleted {
					return failDispatchErr(ad, errors.New("orchestrator: provider stream closed before terminal event"))
				}
				return ad.Error
			}
			switch e := ev.(type) {
			case agent.ResultEvent:
				if e.Success {
					ad.Status = DispatchCompleted
					ad.ExitCode = 0
					ad.Error = nil
				} else {
					msg := strings.Join(e.Errors, "; ")
					if msg == "" {
						msg = e.Message
					}
					if msg == "" {
						msg = "agent reported failure"
					}
					if e.ErrorSubtype != "" {
						msg += " (" + e.ErrorSubtype + ")"
					}
					return failDispatchErr(ad, errors.New(msg)) //nolint:goerr113
				}
				return nil
			case agent.ErrorEvent:
				msg := e.Message
				if msg == "" {
					msg = "agent reported error"
				}
				if e.Code != "" {
					msg += " (" + e.Code + ")"
				}
				return failDispatchErr(ad, errors.New(msg)) //nolint:goerr113
			default:
				// Non-terminal events (assistant text, tool use, system)
				// carry progress only; keep awaiting the terminal.
			}
		}
	}
}

// failDispatchErr marks a running dispatch failed and returns the error.
func failDispatchErr(ad *AgentDispatch, err error) error {
	ad.Status = DispatchFailed
	ad.CompletedAt = time.Now()
	ad.Error = err
	return err
}

// New creates an Orchestrator with a real Linear client and provider dispatcher.
// It validates and loads .donmai/config.yaml when present.
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
		dispatcher: &nativeDispatcher{factory: cfg.ProviderFactory},
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

	// Load .donmai/config.yaml (optional — not an error when absent).
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
		dispatcher: &nativeDispatcher{factory: cfg.ProviderFactory},
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

// WithProviderFactory replaces the harness provider factory — primarily
// used in tests to inject stub providers.
func (o *Orchestrator) WithProviderFactory(f ProviderFactory) {
	o.dispatcher = &nativeDispatcher{factory: f}
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
	if ad != nil {
		result.Dispatched = append(result.Dispatched, ad)
	}
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

	// Validate the harness selection once, before listing, so a bad
	// --harness value fails fast instead of once per issue.
	if _, herr := resolveHarness(o.cfg.Harness); herr != nil {
		return nil, herr
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

			// Acquire a semaphore slot, or stop when cancelled. Only this
			// loop sends on sem; workers only release.
			select {
			case <-ctx.Done():
				goto done
			case sem <- struct{}{}:
			}
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

				// Dispatch is synchronous in the native implementation: the
				// returned dispatch is already terminal, so no polling loop
				// runs here. The semaphore slot is held for the whole
				// Dispatch call, which bounds concurrency.
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

	return nil, errors.New("orchestrator: no project specified (use --project or set allowedProjects in .donmai/config.yaml)")
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
