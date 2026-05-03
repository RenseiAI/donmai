package afcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/daemon"
	"github.com/RenseiAI/agentfactory-tui/prompt"
	providerclaude "github.com/RenseiAI/agentfactory-tui/provider/claude"
	providercodex "github.com/RenseiAI/agentfactory-tui/provider/codex"
	providerstub "github.com/RenseiAI/agentfactory-tui/provider/stub"
	"github.com/RenseiAI/agentfactory-tui/result"
	"github.com/RenseiAI/agentfactory-tui/runner"
	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
)

// DefaultAgentRunDaemonURL is the local control HTTP address the
// daemon binds to (127.0.0.1:7734). The `af agent run` subcommand
// fetches its session detail from <DefaultAgentRunDaemonURL>/api/daemon/sessions/<id>.
const DefaultAgentRunDaemonURL = "http://127.0.0.1:7734"

// agentRunOpts collects the per-invocation flags the runner consumes.
// Pulled out so tests can drive newAgentRunCmd's RunE directly without
// going through cobra's flag-parsing layer.
type agentRunOpts struct {
	sessionID  string
	daemonURL  string
	worktree   string
	preserveWT bool
	jsonOut    bool
}

// newAgentRunCmd constructs the `agent run` subcommand. This is the
// long-running entry point the daemon spawns for every claimed
// session: it reads the session detail from the daemon's local HTTP
// API, builds a runner.Registry with the providers compiled into the
// binary, and invokes runner.Run.
//
// The subcommand is intentionally headless — it expects RENSEI_SESSION_ID
// in env (set by the spawner) or --session-id on the command line.
// Stdout receives a single line of machine-readable JSON describing
// the terminal Result; stderr receives slog output.
//
// Exit codes:
//
//   - 0  — runner.Run returned a Result with Status="completed" and
//     poster.Post succeeded. Soft warnings (failed teardown,
//     retried result post) do not change the exit code.
//   - 1  — runner.Run failed; Result.Status != "completed".
//   - 2  — pre-flight failure (no session id, daemon unreachable,
//     session not found, registry construction failed).
//
// (REN-1461 / F.2.8 — daemon wire-up.)
func newAgentRunCmd() *cobra.Command {
	opts := &agentRunOpts{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a single agent session (invoked by the daemon spawner).",
		Long: "Run a single agent session end-to-end.\n\n" +
			"This subcommand is the worker the local daemon spawns for every\n" +
			"claimed session. It reads the session detail from the daemon's\n" +
			"local HTTP control API at 127.0.0.1:7734/api/daemon/sessions/<id>,\n" +
			"selects the provider implementation indicated by the session's\n" +
			"resolved profile (claude / codex / stub), runs the orchestrator\n" +
			"loop in runner.Runner, and posts the terminal Result back to the\n" +
			"platform.\n\n" +
			"The session id is read from --session-id or the\n" +
			"RENSEI_SESSION_ID environment variable (set automatically by\n" +
			"the daemon spawner).\n\n" +
			"Operators rarely invoke this directly — `af daemon run` spawns it\n" +
			"on every accepted session. To debug a session locally, set\n" +
			"RENSEI_SESSION_ID and invoke this command against a running\n" +
			"daemon.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentRun(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.sessionID, "session-id", "",
		"Session ID to run (default: $RENSEI_SESSION_ID)")
	cmd.Flags().StringVar(&opts.daemonURL, "daemon-url", "",
		"Daemon control URL (default: $RENSEI_DAEMON_URL or http://127.0.0.1:7734)")
	cmd.Flags().StringVar(&opts.worktree, "worktree-dir", "",
		"Per-session worktree parent directory (default: ~/.rensei/worktrees)")
	cmd.Flags().BoolVar(&opts.preserveWT, "preserve-worktree", true,
		"Preserve the worktree on disk after the session ends (debugging)")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", true,
		"Emit a single JSON line describing the terminal Result (default true)")
	return cmd
}

// runAgentRun is the testable entry point for the `agent run` command.
// Cobra-free; takes opts directly so tests can drive it with a fake
// daemon HTTP server.
func runAgentRun(ctx context.Context, cmd *cobra.Command, opts *agentRunOpts) error {
	// 1. Resolve the session id.
	sessionID := strings.TrimSpace(opts.sessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(os.Getenv("RENSEI_SESSION_ID"))
	}
	if sessionID == "" {
		return preflightErr("missing session id: pass --session-id or set RENSEI_SESSION_ID (the daemon spawner sets this automatically)")
	}

	// 2. Resolve the daemon URL.
	daemonURL := strings.TrimSpace(opts.daemonURL)
	if daemonURL == "" {
		daemonURL = strings.TrimSpace(os.Getenv("RENSEI_DAEMON_URL"))
	}
	if daemonURL == "" {
		daemonURL = DefaultAgentRunDaemonURL
	}

	// 3. Set up signal handling so SIGTERM/SIGINT translates into a
	// clean ctx cancellation through the runner.
	runCtx, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	logger := slog.Default()
	logger.Info("af agent run: starting",
		"sessionId", sessionID,
		"daemonUrl", daemonURL,
	)

	// 4. Fetch session detail from the daemon (3-attempt exp backoff).
	detail, err := fetchSessionDetail(runCtx, &http.Client{Timeout: 10 * time.Second}, daemonURL, sessionID)
	if err != nil {
		return preflightErr(fmt.Sprintf("fetch session detail: %v", err))
	}
	logger.Info("af agent run: session detail fetched",
		"sessionId", detail.SessionID,
		"identifier", detail.IssueIdentifier,
		"provider", providerNameFromDetail(detail),
		"workType", detail.WorkType,
	)

	// 5. Construct registry, runner, and run.
	reg := buildAgentRunRegistry(logger)
	logger.Info("af agent run: registry built", "providers", reg.Names())

	wtParent := opts.worktree
	if wtParent == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return preflightErr(fmt.Sprintf("resolve home dir for worktree parent: %v", herr))
		}
		wtParent = filepath.Join(home, ".rensei", "worktrees")
	}
	wm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent, Logger: logger})
	if err != nil {
		return preflightErr(fmt.Sprintf("worktree manager: %v", err))
	}

	poster, err := result.NewPoster(result.Options{
		PlatformURL: detail.PlatformURL,
		AuthToken:   detail.AuthToken,
		WorkerID:    detail.WorkerID,
	})
	if err != nil {
		// PlatformURL missing is a soft pre-flight failure; surface a
		// clear error so the daemon log shows the misconfiguration.
		return preflightErr(fmt.Sprintf("result poster: %v", err))
	}

	r, err := runner.New(runner.Options{
		Registry:                  reg,
		WorktreeManager:           wm,
		Poster:                    poster,
		Logger:                    logger,
		PreserveWorktreeOnFailure: opts.preserveWT,
		// Backstop runs by default — the daemon-spawned worker is
		// the production code path; tests use the in-process entry.
	})
	if err != nil {
		return preflightErr(fmt.Sprintf("runner: %v", err))
	}

	qw := detailToQueuedWork(detail)

	logger.Info("af agent run: invoking runner.Run", "sessionId", qw.SessionID)
	res, runErr := r.Run(runCtx, qw)

	out := cmd.OutOrStdout()
	if opts.jsonOut && res != nil {
		if err := emitResultJSON(out, res); err != nil {
			logger.Warn("af agent run: emit result json failed", "err", err)
		}
	}

	// Shutdown providers (codex app-server is the load-bearing one;
	// claude + stub are no-ops). Best-effort.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if shutErr := reg.Shutdown(shutCtx); shutErr != nil {
		logger.Warn("af agent run: registry shutdown returned errors", "err", shutErr)
	}

	if runErr != nil {
		return fmt.Errorf("runner.Run: %w", runErr)
	}
	if res != nil && res.Status != "completed" {
		// Honor the runner's failure classification with a non-zero
		// exit so the daemon's spawn-event observer records a failure.
		return fmt.Errorf("session %s ended with status %q (failureMode=%s)", sessionID, res.Status, res.FailureMode)
	}
	return nil
}

// fetchSessionDetail retrieves the per-session payload from the
// daemon's local HTTP control API. Retries up to 3 times with
// 200ms / 400ms / 800ms exponential backoff on transient failures (5xx,
// network) — 4xx responses (404 session not found) short-circuit.
func fetchSessionDetail(ctx context.Context, client *http.Client, baseURL, sessionID string) (*daemon.SessionDetail, error) {
	if client == nil {
		client = http.DefaultClient
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/api/daemon/sessions/" + sessionID

	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		detail, err := fetchSessionDetailOnce(ctx, client, endpoint)
		if err == nil {
			return detail, nil
		}
		lastErr = err
		// 4xx — permanent.
		var perm *permanentFetchError
		if errors.As(err, &perm) {
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if attempt < maxAttempts {
			delay := time.Duration(200*(1<<(attempt-1))) * time.Millisecond
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
		}
	}
	return nil, fmt.Errorf("fetch session detail: after %d attempts: %w", maxAttempts, lastErr)
}

// permanentFetchError signals a 4xx response from the daemon — no
// amount of retrying will help.
type permanentFetchError struct {
	StatusCode int
	Body       string
}

func (e *permanentFetchError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

func fetchSessionDetailOnce(ctx context.Context, client *http.Client, endpoint string) (*daemon.SessionDetail, error) {
	// nolint:gosec // G107: endpoint is the operator-supplied daemon URL,
	// defaulting to 127.0.0.1:7734 — not user-tainted SSRF.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req) // nolint:gosec // see above
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, &permanentFetchError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var detail daemon.SessionDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	return &detail, nil
}

// providerCtor is a (name, constructor) tuple consumed by
// [buildRegistryFromCtors]. Pulled out so unit tests can drive the
// failure-aggregation + zero-providers branches without depending on
// the real claude / codex / stub probe behaviour.
type providerCtor struct {
	name string
	new  func() (agent.Provider, error)
}

// buildAgentRunRegistry constructs the runner.Registry for one
// `af agent run` invocation. Stub is always registered; claude + codex
// register on best-effort (their probes return errors when the
// underlying CLI / app-server is missing — we log + skip rather than
// fail the whole worker so a misconfigured host does not silently lose
// stub-mode smoke runs).
//
// Each spawned `af agent run` builds its own Registry — providers are
// stateless modulo codex's app-server, and that app-server is a
// per-process singleton that gets a fresh start on every spawn. Sharing
// a single registry across daemon-life sessions would force lifecycle
// coupling we explicitly want to avoid (per F.1.1 §7 + the F.2.8 task
// guidance).
//
// Probe-failure visibility (REN-1462 / v0.5.1): every provider
// construction or registration failure logs at WARN with the provider
// name and underlying error so operators can see at a glance which
// providers are available on this host. If the resulting registry has
// zero providers, an ERROR-level log fires — that is a fatal
// misconfiguration and any subsequent runner.Run will fail because
// no provider can resolve.
func buildAgentRunRegistry(logger *slog.Logger) *runner.Registry {
	return buildRegistryFromCtors(logger, []providerCtor{
		{name: "stub", new: func() (agent.Provider, error) { return providerstub.New() }},
		{name: "claude", new: func() (agent.Provider, error) { return providerclaude.New(providerclaude.Options{}) }},
		{name: "codex", new: func() (agent.Provider, error) { return providercodex.New(providercodex.Options{}) }},
	})
}

// buildRegistryFromCtors is the testable core of [buildAgentRunRegistry].
// It walks the provided ctors, logs WARN per-provider failure, and
// emits an ERROR record when the resulting registry has zero
// successful registrations. Returns the (possibly-empty) Registry.
func buildRegistryFromCtors(logger *slog.Logger, ctors []providerCtor) *runner.Registry {
	reg := runner.NewRegistry()
	for _, c := range ctors {
		p, err := c.new()
		if err != nil {
			logger.Warn("af agent run: provider probe failed",
				"provider", c.name, "err", err)
			continue
		}
		if regErr := reg.Register(p); regErr != nil {
			logger.Warn("af agent run: provider register failed",
				"provider", c.name, "err", regErr)
		}
	}
	if len(reg.Names()) == 0 {
		logger.Error("af agent run: no providers available — every provider probe failed; the worker cannot resolve any session. Check claude/codex install on PATH or run `af doctor`. (REN-1462)")
	}
	return reg
}

// detailToQueuedWork translates the daemon's SessionDetail wire shape
// into the runner's QueuedWork. Pure function; no I/O.
func detailToQueuedWork(d *daemon.SessionDetail) runner.QueuedWork {
	qw := runner.QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID:          d.SessionID,
			IssueID:            d.IssueID,
			IssueIdentifier:    d.IssueIdentifier,
			LinearSessionID:    d.LinearSessionID,
			ProviderSessionID:  d.ProviderSessionID,
			ProjectName:        d.ProjectName,
			OrganizationID:     d.OrganizationID,
			Repository:         d.Repository,
			Ref:                d.Ref,
			WorkType:           d.WorkType,
			PromptContext:      d.PromptContext,
			Body:               d.Body,
			Title:              d.Title,
			MentionContext:     d.MentionContext,
			ParentContext:      d.ParentContext,
			StagePrompt:        d.StagePrompt,
			StageID:            d.StageID,
			StageLifecycle:     d.StageLifecycle,
			StageSourceEventID: d.StageSourceEventID,
		},
		Branch:      d.Branch,
		WorkerID:    d.WorkerID,
		AuthToken:   d.AuthToken,
		PlatformURL: d.PlatformURL,
	}
	if d.StageBudget != nil {
		qw.StageBudget = &prompt.StageBudget{
			MaxDurationSeconds: d.StageBudget.MaxDurationSeconds,
			MaxSubAgents:       d.StageBudget.MaxSubAgents,
			MaxTokens:          d.StageBudget.MaxTokens,
		}
	}
	if d.ResolvedProfile != nil {
		qw.ResolvedProfile = runner.ResolvedProfile{
			Provider:       agent.ProviderName(d.ResolvedProfile.Provider),
			Runner:         d.ResolvedProfile.Runner,
			Model:          d.ResolvedProfile.Model,
			Effort:         agent.EffortLevel(d.ResolvedProfile.Effort),
			CredentialID:   d.ResolvedProfile.CredentialID,
			ProviderConfig: d.ResolvedProfile.ProviderConfig,
		}
	}
	return qw
}

// providerNameFromDetail returns the provider name the runner will
// resolve for this detail, falling back through the same chain
// runner.QueuedWork.resolvedProvider uses. Only used for log lines —
// the runner does the authoritative resolution itself.
func providerNameFromDetail(d *daemon.SessionDetail) string {
	if d.ResolvedProfile == nil {
		return string(agent.ProviderClaude)
	}
	if d.ResolvedProfile.Provider != "" {
		return d.ResolvedProfile.Provider
	}
	if d.ResolvedProfile.Runner != "" {
		return d.ResolvedProfile.Runner
	}
	return string(agent.ProviderClaude)
}

// emitResultJSON writes the runner.Result as a single newline-
// terminated JSON line to w. Errors are non-fatal; the caller logs
// them and proceeds. The line shape mirrors result.Post's wire body
// so external dashboards can ingest stdout directly.
func emitResultJSON(w io.Writer, res *runner.Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

// preflightErr wraps a setup-time failure (no session id, daemon
// unreachable, etc) so the caller can distinguish from a runner.Run
// failure.
func preflightErr(msg string) error { return fmt.Errorf("preflight: %s", msg) }
