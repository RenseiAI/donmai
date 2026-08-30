package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/matrix"
	"github.com/RenseiAI/donmai/prompt"
	provideragycli "github.com/RenseiAI/donmai/provider/harness/agycli"
	provideramp "github.com/RenseiAI/donmai/provider/harness/amp"
	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	providergemini "github.com/RenseiAI/donmai/provider/harness/gemini"
	providerollama "github.com/RenseiAI/donmai/provider/harness/ollama"
	provideropencode "github.com/RenseiAI/donmai/provider/harness/opencode"
	providerpi "github.com/RenseiAI/donmai/provider/harness/pi"
	providershell "github.com/RenseiAI/donmai/provider/harness/shell"
	providerstub "github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// DefaultAgentRunDaemonURL is the local control HTTP address the
// daemon binds to (127.0.0.1:7734). The `donmai agent run` subcommand
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
	// keepRecording is the standalone --keep-recording flag: a LOCAL OPERATOR
	// decision to suppress the runner's end-of-session deletion of an
	// interactive session's on-disk asciinema-v2 cast. It sets
	// runner.QueuedWork.RetainRecording, which never rides the wire and has
	// no platform-side counterpart — see that field's doc comment.
	keepRecording bool
	// bin is the host binary name (from binaryName(cfg)) used in error hints.
	// Defaults to "donmai" when empty.
	bin string
	// specDecorator is cfg.AgentSpecExtensionDecorator, threaded through from
	// newAgentRunCmd exactly like bin above. nil preserves historical
	// behavior (no provider wrapping).
	specDecorator agent.ExtensionDecorator
}

// bindWorkerGatewayForAgentRun is the production gateway-binding seam. Tests
// replace it to prove harness preflight denial precedes gateway side effects.
var bindWorkerGatewayForAgentRun = func(
	ctx context.Context,
	logger *slog.Logger,
	detail *daemon.SessionDetail,
	work *runner.QueuedWork,
	harnessID string,
) (*workerGateway, error) {
	return bindWorkerGateway(ctx, logger, detail, work, harnessID)
}

// gatewayHarnessIdentity projects the canonical loop-driver identity already
// fixed by successful explicit admission. Absent-harness work has no preflight
// admission, so it projects the legacy provider through the generated matrix
// alias. This keeps gateway and cost attribution on canonical harness ids even
// while the legacy/posterior admission path still runs later in Runner.
func gatewayHarnessIdentity(detail *daemon.SessionDetail, admission *runner.HarnessAdmission) string {
	if ref, ok := admission.CanonicalHarnessRef(); ok {
		return ref.ID
	}
	legacyProvider := agent.ProviderName(providerNameFromDetail(detail))
	if cell, ok := matrix.LegacyCell(legacyProvider); ok {
		return string(cell.Harness)
	}
	return string(legacyProvider)
}

// newAgentRunCmd constructs the `agent run` subcommand. This is the
// long-running entry point the daemon spawns for every claimed
// session: it reads the session detail from the daemon's local HTTP
// API, builds a runner.Registry with the providers compiled into the
// binary, and invokes runner.Run.
//
// The subcommand is intentionally headless — it expects DONMAI_SESSION_ID
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
// (F.2.8 — daemon wire-up.)
func newAgentRunCmd(cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	opts := &agentRunOpts{bin: bin, specDecorator: cfg.AgentSpecExtensionDecorator}
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
			"DONMAI_SESSION_ID environment variable (set automatically by\n" +
			"the daemon spawner).\n\n" +
			"Operators rarely invoke this directly. `" + bin + " host run` spawns it\n" +
			"on every accepted session. To debug a session locally, set\n" +
			"DONMAI_SESSION_ID and invoke this command against a running\n" +
			"daemon.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentRun(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.sessionID, "session-id", "",
		"Session ID to run (default: $DONMAI_SESSION_ID)")
	cmd.Flags().StringVar(&opts.daemonURL, "daemon-url", "",
		"Daemon control URL (default: $DONMAI_DAEMON_URL or http://127.0.0.1:7734)")
	cmd.Flags().StringVar(&opts.worktree, "worktree-dir", "",
		"Per-session worktree parent directory (default: ~/.donmai/worktrees)")
	cmd.Flags().BoolVar(&opts.preserveWT, "preserve-worktree", true,
		"Preserve the worktree on disk after the session ends (debugging)")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", true,
		"Emit a single JSON line describing the terminal Result (default true)")
	cmd.Flags().BoolVar(&opts.keepRecording, "keep-recording", false,
		"Keep the interactive session's on-disk asciinema-v2 cast after the session ends (default: deleted)")
	return cmd
}

// agentRunMaxSessionDuration returns the runner timeout override for a
// daemon-spawned agent session. Interactive sessions are human-driven and may
// remain attached beyond the runner's two-hour default, so a negative duration
// disables that runner-side cap. All other modes leave the option at zero and
// retain runner.DefaultMaxSessionDuration.
func agentRunMaxSessionDuration(detail *daemon.SessionDetail) time.Duration {
	if detail != nil && detail.Mode == prompt.InteractiveRunMode {
		return -1
	}
	return 0
}

// runAgentRun is the testable entry point for the `agent run` command.
// Cobra-free; takes opts directly so tests can drive it with a fake
// daemon HTTP server.
func runAgentRun(ctx context.Context, cmd *cobra.Command, opts *agentRunOpts) error {
	// 1. Resolve the session id.
	sessionID := strings.TrimSpace(opts.sessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(os.Getenv("DONMAI_SESSION_ID"))
	}
	if sessionID == "" {
		return preflightErr("missing session id: pass --session-id or set DONMAI_SESSION_ID (the daemon spawner sets this automatically)")
	}

	// 2. Resolve the daemon URL.
	daemonURL := strings.TrimSpace(opts.daemonURL)
	if daemonURL == "" {
		daemonURL = strings.TrimSpace(os.Getenv("DONMAI_DAEMON_URL"))
	}
	if daemonURL == "" {
		daemonURL = DefaultAgentRunDaemonURL
	}

	// 2b. Resolve the optional daemon-control bearer token. In a cloud
	// sandbox the provisioner points DONMAI_DAEMON_URL at an authenticated
	// remote endpoint and sets DONMAI_RUNTIME_JWT to the token that
	// endpoint expects; the token is attached as `Authorization: Bearer
	// <token>` on daemon-control requests. When unset (the default
	// localhost loopback at 127.0.0.1:7734) no Authorization header is
	// sent, preserving the unauthenticated loopback behavior.
	daemonToken := strings.TrimSpace(os.Getenv("DONMAI_RUNTIME_JWT"))

	// 3. Set up signal handling so SIGTERM/SIGINT translates into a
	// clean ctx cancellation through the runner.
	runCtx, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	logger := slog.Default()
	logger.Info(
		"agent run: starting",
		"sessionId", sessionID,
		"daemonUrl", daemonURL,
	)

	// 4. Fetch session detail from the daemon (3-attempt exp backoff).
	detail, err := fetchSessionDetail(runCtx, &http.Client{Timeout: 10 * time.Second}, daemonURL, sessionID, daemonToken)
	if err != nil {
		return preflightErr(fmt.Sprintf("fetch session detail: %v", err))
	}
	logger.Info(
		"agent run: session detail fetched",
		"sessionId", detail.SessionID,
		"identifier", detail.IssueIdentifier,
		"provider", providerNameFromDetail(detail),
		"workType", detail.WorkType,
	)
	if len(detail.AdmissionReceipt) > 0 {
		hostReceipt, err := executioncell.DecodeHostAdaptationReceipt(detail.HostAdaptationReceipt)
		if err != nil || hostReceipt.RequestID != detail.SessionID ||
			hostReceipt.WorkerID != detail.WorkerID || hostReceipt.Decision != "ready" {
			return fmt.Errorf("receipt-bearing session has no valid daemon adaptation-ready receipt")
		}
	}
	credentialCache := newAgentRunCredentialCache(
		&http.Client{Timeout: 5 * time.Second},
		daemonURL,
		sessionID,
		daemonToken,
		detail,
	)

	// 5. Construct registry, runner, and run.
	agentBin := opts.bin
	if agentBin == "" {
		agentBin = "donmai"
	}
	reg := buildRegistryFromCtors(logger, agentRunProviderCtors(agentRunHints(detail)), agentBin)
	logger.Info("agent run: registry built", "providers", reg.Names())
	if opts.specDecorator != nil {
		decorateRegistryProviders(reg, opts.specDecorator)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if shutErr := reg.Shutdown(shutCtx); shutErr != nil {
			logger.Warn("donmai agent run: registry shutdown returned errors", "err", shutErr)
		}
	}()

	qw, err := detailToQueuedWork(detail)
	if err != nil {
		return preflightErr(fmt.Sprintf("endpoint binding: %v", err))
	}
	// RetainRecording is a LOCAL OPERATOR decision only (never a platform
	// one — see the field's doc comment on runner.QueuedWork): it rides in
	// from --keep-recording, not from the daemon's SessionDetail.
	qw.RetainRecording = opts.keepRecording
	admission, admissionErr := reg.PreflightHarness(qw)
	if admissionErr != nil {
		logger.Warn("donmai agent run: explicit harness denied before gateway/status side effects",
			"sessionId", qw.SessionID, "err", admissionErr)
	}

	wtParent := opts.worktree
	if wtParent == "" {
		wtParent = statepath.Resolve("worktrees", "/tmp/.donmai/worktrees")
	}
	wm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent, Logger: logger, RestoreSessionID: sessionID})
	if err != nil {
		return preflightErr(fmt.Sprintf("worktree manager: %v", err))
	}

	poster, err := result.NewPoster(result.Options{
		PlatformURL:        detail.PlatformURL,
		AuthToken:          detail.AuthToken,
		WorkerID:           detail.WorkerID,
		CredentialProvider: credentialCache.resultCredentials,
	})
	if err != nil {
		// PlatformURL missing is a soft pre-flight failure; surface a
		// clear error so the daemon log shows the misconfiguration.
		return preflightErr(fmt.Sprintf("result poster: %v", err))
	}

	// Activate kit toolchain provisioning (KITS PIVOT #3). This is the
	// production runner-construction site (`donmai agent run` is what the
	// daemon spawns per session), so it is where runner.Options' kit knobs
	// must be set — K1 added the knobs but left them unset, so step 2b was
	// inert. We arm:
	//   - KitDetector: the registry's repo-detection fallback. When the
	//     platform threads no explicit demand on the work item, the runner
	//     detects kits from the cloned worktree (OD-1 fallback).
	//   - KitSkillDetector: resolves skill sources POST-CLONE against the
	//     real worktree path (closes the stale-CWD bug). Replaces the prior
	//     KitSkillSources pre-compute-at-daemon-CWD approach.
	//   - KitPromptFragmentDetector: resolves prompt-fragment sources
	//     POST-CLONE so [provide.prompt_fragments] bodies are injected at
	//     step 5a filtered by the session's workType.
	//   - KitTargetOS: "linux" for cloud sandboxes (a cloud sandbox is Linux
	//     even when this binary runs on a macOS host, OD-2); the host GOOS
	//     for local execution. The daemon-spawned worker for a cloud
	//     sandbox runs INSIDE that Linux sandbox, so runtime.GOOS is already
	//     "linux" there; for the local path runtime.GOOS is the host. Using
	//     runtime.GOOS therefore yields the correct target in both modes.
	kitReg := daemon.NewKitRegistry(kitScanPaths())
	kitTargetOS, _ := kit.ResolveOS(runtime.GOOS)
	if kitTargetOS == "" {
		kitTargetOS = kit.OSLinux
	}

	r, err := runner.New(runner.Options{
		Registry:                  reg,
		WorktreeManager:           wm,
		Poster:                    poster,
		CredentialProvider:        credentialCache.runnerCredentials,
		Logger:                    logger,
		MaxSessionDuration:        agentRunMaxSessionDuration(detail),
		PreserveWorktreeOnFailure: opts.preserveWT,
		// The library stays env-free; this binary is the operator boundary.
		// Dispatch capability `llm-span-ingest` can also enable the pipeline
		// per session once a compatible server advertises it.
		SpanEmissionEnabled: donmaiSpanTracingEnabled(),
		// KITS PIVOT #3 — arm runner/loop.go step 2b so kit toolchain
		// (toolchain_install + post_acquire) runs AFTER the repo is cloned.
		// The platform-supplied demand on the work item (qw.Kits) overrides
		// detection; KitDetector is the fallback.
		KitDetector: kitReg.DetectForRepo,
		KitComposer: kitReg.ComposeForRepo,
		KitTargetOS: kitTargetOS,
		// KIT BOOTSTRAP — wire post-clone skill + prompt-fragment detectors
		// so the runner re-detects against the REAL worktree (step 2c in
		// loop.go) rather than relying on the pre-computed daemon-CWD sources.
		// KitSkillSources is intentionally left nil — KitSkillDetector takes
		// precedence when set (see runner/runner.go field docs).
		KitSkillDetector:          kitReg.SkillSourcesForRepo,
		KitPromptFragmentDetector: kitReg.PromptFragmentSourcesForRepo,
		// Runtime memory-inject (v2) needs NO worker config: the runner always
		// wires the inject handler when the provider supports injection, and the
		// PLATFORM decides per-session whether to deliver (per-project memory
		// config). No env var. Providers without injection support fall back to
		// the dispatch-time fold (v1).
		// Backstop runs by default — the daemon-spawned worker is
		// the production code path; tests use the in-process entry.
	})
	if err != nil {
		return preflightErr(fmt.Sprintf("runner: %v", err))
	}

	// 5b. Worker-local gateway binding (08 §5/§9 M1). When the resolved cell is
	// served by the translating-gateway host, start the gateway in THIS process,
	// bind this session, and stamp the resulting EndpointBinding onto the
	// resolved profile so the harness drives the loopback surface with a
	// per-session bearer while the upstream credential stays here. A no-op for
	// every non-gateway cell; a hard preflight failure when the cell IS
	// gateway-served but the worker cannot honor it (never a silent fallback to
	// some other endpoint — see afcli/gateway_bind.go).
	var gwSession *workerGateway
	if admissionErr == nil {
		gwSession, err = bindWorkerGatewayForAgentRun(
			runCtx, logger, detail, &qw, gatewayHarnessIdentity(detail, admission),
		)
		if err != nil {
			return preflightErr(fmt.Sprintf("gateway binding: %v", err))
		}
	}
	defer gwSession.Close(logger)

	// Flip the session to 'running' eagerly, BEFORE runner.Run spawns the
	// provider. The activity-gated maybePostRunning
	// (runtime/activity/poster.go) only fires after the first successful
	// activity POST, so a credential-blocked or slow-booting agent sits
	// indistinguishably in 'pending' with no activity — the same terminal
	// appearance as a stuck spawn. Posting running at spawn makes a
	// no-activity failure a distinct reap signal and unblocks the
	// platform-side lock re-acquire path, which only passes once the
	// session is 'running'. Best-effort + idempotent with the later
	// maybePostRunning: the platform treats a repeated running transition
	// as a no-op, so racing the two posts is safe.
	if admissionErr == nil {
		postSessionRunning(runCtx, &http.Client{Timeout: 5 * time.Second}, logger,
			detail.PlatformURL, sessionID, detail.WorkerID, detail.AuthToken)
	}

	logger.Info("donmai agent run: invoking runner.RunAdmitted", "sessionId", qw.SessionID)
	res, runErr := r.RunAdmitted(runCtx, qw, admission)

	out := cmd.OutOrStdout()
	if opts.jsonOut && res != nil {
		if err := emitResultJSON(out, res); err != nil {
			logger.Warn("donmai agent run: emit result json failed", "err", err)
		}
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

func donmaiSpanTracingEnabled() bool {
	v := strings.TrimSpace(os.Getenv("DONMAI_OTEL_TRACES"))
	return v == "1" || strings.EqualFold(v, "true")
}

// postSessionRunning fires an eager, best-effort POST
// /api/sessions/<id>/status with {"status":"running","workerId":"..."}
// against the PLATFORM (not the local daemon) before the runner spawns the
// provider. It mirrors the wire shape of runtime/activity's maybePostRunning
// so the two are interchangeable and idempotent: the platform treats a
// repeated running transition as a no-op.
//
// All failures are logged at debug and discarded — the running nudge is
// pure observability + it unblocks the platform-side lock re-acquire path
// (which only passes once the session is 'running'); it must never fail the
// worker. A no-op when platformURL is empty (standalone / no-platform mode,
// where there is no platform status endpoint to hit).
func postSessionRunning(ctx context.Context, client *http.Client, logger *slog.Logger, platformURL, sessionID, workerID, authToken string) {
	platformURL = strings.TrimSpace(platformURL)
	if platformURL == "" {
		return
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	body, err := json.Marshal(map[string]string{
		"status":   "running",
		"workerId": workerID,
	})
	if err != nil {
		logger.Debug("agent run: status=running marshal failed", "sessionId", sessionID, "err", err)
		return
	}
	url := strings.TrimRight(platformURL, "/") + "/api/sessions/" + sessionID + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body)) //nolint:gosec // G704: platformURL is the operator-configured platform base URL (trusted daemon/session config, not request-derived input)
	if err != nil {
		logger.Debug("agent run: status=running new request failed", "sessionId", sessionID, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := client.Do(req) //nolint:gosec // G704: same trusted operator-configured URL as above
	if err != nil {
		logger.Debug("agent run: status=running post failed", "sessionId", sessionID, "err", err)
		return
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Debug("agent run: status=running non-2xx", "sessionId", sessionID, "status", resp.StatusCode)
		return
	}
	logger.Info("agent run: session flipped to running (pre-spawn)",
		"sessionId", sessionID, "workerId", workerID)
}

// fetchSessionDetail retrieves the per-session payload from the
// daemon's local HTTP control API. Retries up to 3 times with
// 200ms / 400ms / 800ms exponential backoff on transient failures (5xx,
// network) — 4xx responses (404 session not found) short-circuit.
//
// token is the optional daemon-control bearer token. When non-empty it is
// attached as `Authorization: Bearer <token>`; when empty (the localhost
// loopback default) no Authorization header is sent.
func fetchSessionDetail(ctx context.Context, client *http.Client, baseURL, sessionID, token string) (*daemon.SessionDetail, error) {
	if client == nil {
		client = http.DefaultClient
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/api/daemon/sessions/" + sessionID

	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		detail, err := fetchSessionDetailOnce(ctx, client, endpoint, token)
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

func fetchSessionDetailOnce(ctx context.Context, client *http.Client, endpoint, token string) (*daemon.SessionDetail, error) {
	// nolint:gosec // G107: endpoint is the operator-supplied daemon URL,
	// defaulting to 127.0.0.1:7734 — not user-tainted SSRF.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Attach the bearer token only when one is configured. The default
	// localhost loopback endpoint is unauthenticated, so an empty token
	// means no Authorization header is sent.
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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

type agentRunCredentialCache struct {
	mu          sync.Mutex
	client      *http.Client
	daemonURL   string
	sessionID   string
	daemonToken string
	workerID    string
	authToken   string
}

func newAgentRunCredentialCache(client *http.Client, daemonURL, sessionID, daemonToken string, initial *daemon.SessionDetail) *agentRunCredentialCache {
	c := &agentRunCredentialCache{
		client:      client,
		daemonURL:   daemonURL,
		sessionID:   sessionID,
		daemonToken: daemonToken,
	}
	if initial != nil {
		c.workerID = initial.WorkerID
		c.authToken = initial.AuthToken
	}
	return c
}

func (c *agentRunCredentialCache) current(ctx context.Context) (workerID, authToken string, err error) {
	detail, fetchErr := fetchSessionDetail(ctx, c.client, c.daemonURL, c.sessionID, c.daemonToken)

	c.mu.Lock()
	defer c.mu.Unlock()
	if fetchErr == nil && detail != nil {
		if detail.WorkerID != "" {
			c.workerID = detail.WorkerID
		}
		if detail.AuthToken != "" {
			c.authToken = detail.AuthToken
		}
	}
	return c.workerID, c.authToken, fetchErr
}

func (c *agentRunCredentialCache) runnerCredentials(ctx context.Context) (runner.RuntimeCredentials, error) {
	workerID, authToken, err := c.current(ctx)
	return runner.RuntimeCredentials{WorkerID: workerID, AuthToken: authToken}, err
}

func (c *agentRunCredentialCache) resultCredentials(ctx context.Context) (result.RuntimeCredentials, error) {
	workerID, authToken, err := c.current(ctx)
	return result.RuntimeCredentials{WorkerID: workerID, AuthToken: authToken}, err
}

// providerCtor is a (name, constructor) tuple consumed by
// [buildRegistryFromCtors]. Pulled out so unit tests can drive the
// failure-aggregation + zero-providers branches without depending on
// the real claude / codex / stub probe behaviour.
type providerCtor struct {
	name string
	new  func() (agent.Provider, error)
}

// BuildAgentRunRegistry constructs the runner.Registry of the providers
// compiled into this binary — the SINGLE SOURCE for the agent-run provider
// set. It is the public, importable entry point downstream Go binaries call so
// they do NOT have to fork the hand-authored ctor list; calling this builder
// keeps every embedder on the exact same eight providers donmai resolves,
// eliminating constructor-list drift between donmai and downstream CLI
// consumers.
//
// Stub is always registered; the others register on best-effort (their probes
// return errors when the underlying CLI / app-server / API key is missing — we
// log + skip rather than fail the whole worker so a misconfigured host does
// not silently lose stub-mode smoke runs).
//
// Each spawned `donmai agent run` builds its own Registry — providers are
// stateless modulo codex's app-server, and that app-server is a
// per-process singleton that gets a fresh start on every spawn. Sharing
// a single registry across daemon-life sessions would force lifecycle
// coupling we explicitly want to avoid (per F.1.1 §7 + the F.2.8 task
// guidance).
//
// Probe-failure visibility: every provider
// construction or registration failure logs at WARN with the provider
// name and underlying error so operators can see at a glance which
// providers are available on this host. If the resulting registry has
// zero providers, an ERROR-level log fires — that is a fatal
// misconfiguration and any subsequent runner.Run will fail because
// no provider can resolve.
//
// Foundation-runtime-stubs adds three more probe-and-skip entries
// (amp, gemini, opencode). Each follows
// the same warn-and-skip contract as claude / codex: if the
// constructor returns ErrProviderUnavailable (no API key, server
// unreachable) the registry build logs WARN and proceeds without
// that provider, identical to the existing probe-failure path.
//
// The ctor list below is the single hand-authored source of the agent-run
// provider set. It is deliberately NOT matrix-generated: each provider's
// New constructor takes a distinct, package-local Options type (and stub is
// variadic), so a generated closure could only re-emit these same per-package
// New(Options{}) call sites verbatim — adding codegen surface for zero
// single-sourcing gain. Keeping it here, behind a public builder, is the clean
// realization of "single source + no fork".
func BuildAgentRunRegistry(logger *slog.Logger) *runner.Registry {
	return buildRegistryFromCtors(logger, agentRunProviderCtors(), "donmai")
}

// agentRunCtorHints carries per-session signals, derived from the fetched
// SessionDetail's resolved profile, that alter how a hand-authored ctor
// below constructs its provider Options. It is a struct (not a bare bool
// parameter) so a future second signal doesn't force another positional-
// argument change at every call site. The zero value reproduces the exact
// historical, no-session-context construction every ctor used before this
// existed.
type agentRunCtorHints struct {
	// PreferOpenCodeServer forces the opencode ctor onto Lane B (opencode
	// serve + REST/SSE) for this session rather than the Lane-A one-shot
	// CLI default. Derived by opencodeCtorHints from
	// ResolvedProfile.ProviderConfig[opencodeCtorHintKey] — see there.
	PreferOpenCodeServer bool

	// CodexHostSessionAuth tells the codex ctor to project the host's existing
	// CLI login into its isolated config home. It is true only when this exact
	// session selected the codex provider and resolved authMode=host-session.
	CodexHostSessionAuth bool
}

// agentRunHints collects every per-session constructor signal in one pass.
// BuildAgentRunRegistry deliberately does not call this: its zero-context
// introspection registry must retain the historical zero-value behavior.
func agentRunHints(d *daemon.SessionDetail) agentRunCtorHints {
	h := opencodeCtorHints(d)
	h.CodexHostSessionAuth = codexHostSessionCtorHint(d)
	return h
}

func codexHostSessionCtorHint(d *daemon.SessionDetail) bool {
	if d == nil || d.ResolvedProfile == nil ||
		d.ResolvedProfile.AuthMode != string(agent.AuthHostSession) {
		return false
	}

	// Mirror the runner's authoritative selector order without using
	// providerNameFromDetail: that helper is display-only and deliberately
	// falls through an unknown explicit harness to the legacy provider. Secret
	// projection must instead fail closed whenever explicit harness intent is
	// not exactly Codex.
	if d.ModelProfile != nil {
		if d.ModelProfile.Harness != "" {
			return d.ModelProfile.Harness == string(agent.HarnessCodex)
		}
		return d.ModelProfile.ProviderID == string(agent.ProviderCodex)
	}
	if d.ResolvedProfile.Harness != "" {
		return d.ResolvedProfile.Harness == string(agent.HarnessCodex)
	}
	if d.ResolvedProfile.Provider != "" {
		return d.ResolvedProfile.Provider == string(agent.ProviderCodex)
	}
	return d.ResolvedProfile.Runner == string(agent.ProviderCodex)
}

// opencodeCtorHintKey is the typed ResolvedProfile.ProviderConfig knob that
// requests the opencode Lane-B (serve/HTTP) adapter for a session — e.g.
// because it needs live permission mediation, MCP wiring, or resume/inject
// support once those Spec-level triggers are wired (07 §2). It follows the
// same opaque-ProviderConfig-typed-key pattern already used by
// provider/harness/stub ("stub.behavior") and provider/harness/gemini
// ("thinkingLevel"/"thinkingBudget"): namespaced by provider, round-trips
// unmodified through daemon.SessionResolvedProfile.ProviderConfig's
// map[string]any JSON wire shape.
const opencodeCtorHintKey = "opencode.preferServer"

// opencodeCtorHints derives agentRunCtorHints from a fetched SessionDetail.
// A nil detail/ResolvedProfile, a missing key, or a non-bool value all
// resolve to the zero value (Lane-A default, unchanged historical
// behavior) — this is intentionally lenient rather than an error path: an
// agent-run session must never fail preflight over an optional routing
// hint.
func opencodeCtorHints(d *daemon.SessionDetail) agentRunCtorHints {
	var h agentRunCtorHints
	if d == nil || d.ResolvedProfile == nil {
		return h
	}
	if v, ok := d.ResolvedProfile.ProviderConfig[opencodeCtorHintKey]; ok {
		if b, ok := v.(bool); ok {
			h.PreferOpenCodeServer = b
		}
	}
	return h
}

// opencodeCtorOptions builds the opencode provider's construction Options
// from this call's agentRunCtorHints. Split out from the ctor closure in
// agentRunProviderCtors so tests can assert the threaded PreferServer value
// directly, without exercising provideropencode.New's real binary/version
// probe (which is host-dependent).
func opencodeCtorOptions(h agentRunCtorHints) provideropencode.Options {
	return provideropencode.Options{PreferServer: h.PreferOpenCodeServer}
}

func codexCtorOptions(h agentRunCtorHints) providercodex.Options {
	return providercodex.Options{HostSessionAuth: h.CodexHostSessionAuth}
}

// agentRunProviderCtors returns the single hand-authored ctor list — the SoT
// for the agent-run provider set. Pulled into its own function (returning a
// fresh slice on each call) so [BuildAgentRunRegistry] and the no-behavior-
// change parity test enumerate the SAME provider set without the test having
// to re-declare it (which would itself become a fork). Order matches the
// historical slice exactly; behaviour is unchanged.
//
// hints is variadic and optional: [BuildAgentRunRegistry] and
// [buildAgentRunRegistry] (the daemon-startup introspection registry, built
// with no session context) call this with zero arguments, which yields the
// zero-value agentRunCtorHints and therefore byte-for-byte the historical
// per-provider Options{} construction. runAgentRun (the per-session `donmai
// agent run` entry point) is the one call site that has a fetched
// SessionDetail in hand and passes its derived hints.
func agentRunProviderCtors(hints ...agentRunCtorHints) []providerCtor {
	var h agentRunCtorHints
	if len(hints) > 0 {
		h = hints[0]
	}
	return []providerCtor{
		{name: "stub", new: func() (agent.Provider, error) { return providerstub.New() }},
		{name: "claude", new: func() (agent.Provider, error) { return providerclaude.New(providerclaude.Options{}) }},
		{name: "codex", new: func() (agent.Provider, error) { return providercodex.New(codexCtorOptions(h)) }},
		// Ollama is local-first: probe is a quick GET /api/tags against
		// http://localhost:11434. If `ollama serve` is not running on
		// this host the probe wraps agent.ErrProviderUnavailable and
		// the registry skips it (operator-visible WARN log). Sessions
		// that resolved to provider="ollama" then fail at
		// runner.Resolve with agent.ErrNoProvider — which is the
		// correct loud failure when the local runtime is missing.
		{name: "ollama", new: func() (agent.Provider, error) { return providerollama.New(providerollama.Options{}) }},
		// Amp is registration-only today (no public stable runner API); the
		// constructor probes env vars / endpoints and warns-and-skips when
		// missing. OpenCode (registered below) no longer belongs in this
		// bucket: it ships two real managed-spawn lanes (CLI one-shot,
		// serve/HTTP with real tool/MCP policy delivery) plus a fail-closed
		// external-attach posture — see its own ctor comment for how
		// PreferServer routes between them. Gemini is a full streaming impl
		// against generativelanguage.googleapis.com.
		{name: "amp", new: func() (agent.Provider, error) { return provideramp.New(provideramp.Options{}) }},
		{name: "gemini", new: func() (agent.Provider, error) { return providergemini.New(providergemini.Options{}) }},
		// agy-cli is a LOCAL/HOST-SESSION/OAUTH provider wrapping the Antigravity `agy` CLI under a pty.
		// It is the SUBSCRIPTION/no-key local-Gemini path (the user's own OAuth-authed agy on the user's
		// own machine). Distinct from the API-direct "gemini" provider. Requires `agy` installed AND
		// logged in on the host PATH. NOT for cloud sandboxes.
		{name: "agy-cli", new: func() (agent.Provider, error) { return provideragycli.New(provideragycli.Options{}) }},
		// opencode's PreferServer threads the resolved profile's Lane-B
		// signal (opencodeCtorHints above) so a `donmai agent run` session
		// can select the serve/HTTP adapter (07 §2 Lane B) instead of
		// always defaulting to the Lane-A one-shot CLI. Every other call
		// site (daemon-startup introspection, tests) gets h's zero value,
		// i.e. PreferServer: false — unchanged historical behavior.
		{name: "opencode", new: func() (agent.Provider, error) {
			return provideropencode.New(opencodeCtorOptions(h))
		}},
		// pi is registration-only today, mirroring amp/opencode: the
		// constructor probes the binary + version pin and warns-and-skips
		// when absent/below-pin (provider/harness/pi/probe.go). Greenfield
		// harness (09-design-pi-adapter.md); real-binary smoke coverage is
		// donmai-smokes step20 (12-work-breakdown.md W2b). Registering the
		// ctor here is what lets a `donmai agent run` session (and the
		// step20 black-box smoke, which only ever drives the compiled
		// binary's CLI surface) reach pi.New()/Spawn() at all — it does not
		// itself change matrix-level tier gating (cells stay
		// experimental/untested/smoked:false until step20 proves a real
		// run, DEC-2/DEC-3).
		{name: "pi", new: func() (agent.Provider, error) { return providerpi.New(providerpi.Options{}) }},
		// shell is the interactive-only PTY harness (W4 interactive
		// sessions): spawns ${SHELL:-/bin/sh} under ptyhost. Headless
		// Spawn (Spec.Interactive == nil) fails loudly by design.
		{name: "shell", new: func() (agent.Provider, error) { return providershell.New() }},
	}
}

// buildAgentRunRegistry is a thin internal alias of [BuildAgentRunRegistry],
// retained so the package's existing call sites and tests keep their
// short, unexported name. Behaviour is identical — it just delegates.
func buildAgentRunRegistry(logger *slog.Logger) *runner.Registry {
	return buildRegistryFromCtors(logger, agentRunProviderCtors(), "donmai")
}

// buildRegistryFromCtors is the testable core of [BuildAgentRunRegistry].
// It walks the provided ctors, logs WARN per-provider failure, and
// emits an ERROR record when the resulting registry has zero
// successful registrations. Returns the (possibly-empty) Registry.
// bin is the host binary name (from binaryName(cfg)) used in the error hint.
func buildRegistryFromCtors(logger *slog.Logger, ctors []providerCtor, bin string) *runner.Registry {
	reg := runner.NewRegistry()
	for _, c := range ctors {
		p, err := c.new()
		if err != nil {
			logger.Warn("agent run: provider probe failed",
				"provider", c.name, "err", err)
			continue
		}
		if regErr := reg.Register(p); regErr != nil {
			logger.Warn("agent run: provider register failed",
				"provider", c.name, "err", regErr)
			continue
		}
		assertLegacyAlias(logger, p)
	}
	if len(reg.Names()) == 0 {
		logger.Error("agent run: no providers available. Every provider probe failed; the worker cannot resolve any session. Check claude/codex install on PATH or run `" + bin + " host doctor`.")
	}
	return reg
}

// decorateRegistryProviders re-registers every provider currently in reg,
// each wrapped via agent.DecorateProvider(p, decorate) — the embedder
// registration hook for the additional-extension delivery seam (Config.
// AgentSpecExtensionDecorator's doc comment). Registry.Register documents
// that registering under an existing name overwrites the earlier entry, so
// this mutates reg's contents in place without a second registry.
//
// Called once, immediately after buildRegistryFromCtors, so every provider
// this `agent run` invocation could dispatch to — not just the one the
// session's resolved profile happens to select — carries the decorator.
// decorate is guaranteed non-nil by the caller (runAgentRun checks
// opts.specDecorator != nil before calling this), matching
// agent.DecorateProvider's own nil-decorate passthrough contract.
func decorateRegistryProviders(reg *runner.Registry, decorate agent.ExtensionDecorator) {
	for _, name := range reg.Names() {
		p, err := reg.Resolve(name)
		if err != nil {
			// Names() only returns names Resolve can look up; a failure here
			// would mean a concurrent mutation this single-goroutine
			// construction path never performs. Skip defensively rather than
			// panic on an invariant violation that isn't this function's to
			// diagnose.
			continue
		}
		_ = reg.Register(agent.DecorateProvider(p, decorate))
	}
}

// assertLegacyAlias consumes the generated matrix.LegacyAliasMap as a
// defense-in-depth invariant: a registered provider's harness identity
// (Manifest().Name) MUST match the harness the matrix says its
// ProviderName resolves to. This makes the alias map a real reader (P1
// generated it but left it unconsumed) WITHOUT changing which concrete
// provider answers any name — the registry stays ProviderName-keyed.
//
// A mismatch is logged at WARN (never fatal): it would mean the
// hand-authored cell anchors in matrix/cells.go drifted from the live
// manifests, a build-time bug to fix at the source, not a runtime path
// to fail. A provider without a Manifest() (no HarnessProvider) or a
// ProviderName with no legacy alias is skipped silently — neither is a
// drift signal.
func assertLegacyAlias(logger *slog.Logger, p agent.Provider) {
	name := p.Name()
	cell, ok := matrix.LegacyCell(name)
	if !ok {
		return
	}
	hp, ok := p.(agent.HarnessProvider)
	if !ok {
		return
	}
	if got := hp.Manifest().Name; got != cell.Harness {
		logger.Warn(
			"donmai agent run: legacy-alias harness mismatch",
			"provider", name,
			"manifestHarness", got,
			"matrixHarness", cell.Harness,
		)
	}
}

// kitScanPaths returns the kit registry scan paths the runner should use,
// read from the daemon config's optional `kit.scanPaths` block. Falls back
// to the default scan path when the config is absent or unreadable — the
// runner is spawned per session and must never fail to construct just
// because daemon.yaml is missing (a standalone-mode invocation). The
// resolved paths feed daemon.NewKitRegistry so the runner's KitDetector
// fallback sees the same installed kits the daemon's operator surface does.
func kitScanPaths() []string {
	cfg, err := daemon.LoadConfig(daemon.DefaultConfigPath())
	if err == nil && cfg != nil && len(cfg.Kit.ScanPaths) > 0 {
		return cfg.Kit.ScanPaths
	}
	return []string{daemon.DefaultKitScanPath()}
}

// detailToQueuedWork translates the daemon's SessionDetail wire shape
// into the runner's QueuedWork. Pure function; no I/O. This is the seam
// where a dispatched endpoint binding enters the runner path — a malformed
// BaseURL is rejected here (see agent.ValidateEndpointBindingBaseURL, run via
// runner.ReconcileResolvedProfile below) rather than reaching the runner's
// Spec.
func detailToQueuedWork(d *daemon.SessionDetail) (runner.QueuedWork, error) {
	qw := runner.QueuedWork{
		AdmissionReceipt:        bytes.Clone(d.AdmissionReceipt),
		ClaimReceipt:            bytes.Clone(d.ClaimReceipt),
		EffectiveCell:           bytes.Clone(d.EffectiveCell),
		ExecutionRuntimeBinding: bytes.Clone(d.ExecutionRuntimeBinding),
		OperationalPayload:      bytes.Clone(d.OperationalPayload),
		HostAdaptationReceipt:   bytes.Clone(d.HostAdaptationReceipt),
		RepositoryDeclaration:   d.RepositoryDeclaration,
		WorkareaMode:            d.WorkareaMode,
		ParentWorkareaID:        d.ParentWorkareaID,
		RepositoryFilter:        d.RepositoryFilter,
		CacheSeedID:             d.CacheSeedID,
		QueuedWork: prompt.QueuedWork{
			SessionID:            d.SessionID,
			IssueID:              d.IssueID,
			IssueIdentifier:      d.IssueIdentifier,
			LinearSessionID:      d.LinearSessionID,
			ProviderSessionID:    d.ProviderSessionID,
			ProjectName:          d.ProjectName,
			OrganizationID:       d.OrganizationID,
			Repository:           d.Repository,
			Ref:                  d.Ref,
			WorkType:             d.WorkType,
			PromptContext:        d.PromptContext,
			Body:                 d.Body,
			Title:                d.Title,
			MentionContext:       d.MentionContext,
			ParentContext:        d.ParentContext,
			StagePrompt:          d.StagePrompt,
			StageID:              d.StageID,
			StageLifecycle:       d.StageLifecycle,
			StageSourceEventID:   d.StageSourceEventID,
			SystemPromptOverride: d.SystemPromptOverride,
			Kits:                 d.Kits,
			DisallowedTools:      d.DisallowedTools,
			AllowedTools:         d.AllowedTools,
			McpServers:           detailMCPServers(d.McpServers),
			Skills:               detailSkills(d.Skills),
			MemoryBlock:          d.MemoryBlock,
			Mode:                 d.Mode,
			InitialPrompt:        d.InitialPrompt,
			RecordingEnabled:     d.RecordingEnabled,
			InterviewDefinition:  d.InterviewDefinition,
			Traceparent:          d.Traceparent,
			Tracestate:           d.Tracestate,
			SessionStorageID:     d.SessionStorageID,
			SessionPublicID:      d.SessionPublicID,
			TrackerSessionID:     d.TrackerSessionID,
		},
		Branch:                d.Branch,
		WorkerID:              d.WorkerID,
		AuthToken:             d.AuthToken,
		McpAuthToken:          d.McpAuthToken,
		McpAuthTokenExpiresAt: d.McpAuthTokenExpiresAt,
		PlatformURL:           d.PlatformURL,
		TerminalWorkareaLease: d.TerminalWorkareaLease,
		Capabilities:          d.Capabilities,
	}
	if len(d.OperationalPayload) > 0 {
		// Decode into a zero value: absent receipted fields must stay absent rather
		// than inheriting an unreceipted compatibility mirror.
		var admitted runner.QueuedWork
		if err := json.Unmarshal(d.OperationalPayload, &admitted); err != nil {
			return runner.QueuedWork{}, fmt.Errorf("operational payload: %w", err)
		}
		if !reflect.DeepEqual(d.RepositoryDeclaration, admitted.RepositoryDeclaration) ||
			d.WorkareaMode != admitted.WorkareaMode || d.ParentWorkareaID != admitted.ParentWorkareaID ||
			!reflect.DeepEqual(d.RepositoryFilter, admitted.RepositoryFilter) || d.CacheSeedID != admitted.CacheSeedID {
			return runner.QueuedWork{}, errors.New("operational payload workarea intent differs from compatibility mirror")
		}
		if err := applyResolvedRepositoryCompatibility(d, &admitted); err != nil {
			return runner.QueuedWork{}, err
		}
		qw = admitted
		qw.AdmissionReceipt = bytes.Clone(d.AdmissionReceipt)
		qw.ClaimReceipt = bytes.Clone(d.ClaimReceipt)
		qw.EffectiveCell = bytes.Clone(d.EffectiveCell)
		qw.ExecutionRuntimeBinding = bytes.Clone(d.ExecutionRuntimeBinding)
		qw.OperationalPayload = bytes.Clone(d.OperationalPayload)
		qw.HostAdaptationReceipt = bytes.Clone(d.HostAdaptationReceipt)
		qw.WorkerID, qw.AuthToken, qw.PlatformURL = d.WorkerID, d.AuthToken, d.PlatformURL
		// Restored beside the worker credentials for the same reason they are:
		// the detail is authoritative for runtime credentials, so whatever the
		// payload projection did to these fields must not survive. Today the
		// `json:"-"` tags already keep the decoder off them, which makes this
		// line a no-op — it is the tag change, not the decoder, that this
		// guards against, exactly as for AuthToken/PlatformURL above.
		qw.McpAuthToken, qw.McpAuthTokenExpiresAt = d.McpAuthToken, d.McpAuthTokenExpiresAt
		qw.Capabilities = d.Capabilities
	}
	if d.StageBudget != nil {
		qw.StageBudget = &prompt.StageBudget{
			MaxDurationSeconds: d.StageBudget.MaxDurationSeconds,
			MaxSubAgents:       d.StageBudget.MaxSubAgents,
			MaxTokens:          d.StageBudget.MaxTokens,
		}
	}
	if d.InterviewBudget != nil {
		qw.InterviewBudget = &prompt.InterviewBudget{
			MaxWallClockSeconds: d.InterviewBudget.MaxWallClockSeconds,
			IdleGraceSeconds:    d.InterviewBudget.IdleGraceSeconds,
		}
	}
	// Honor dispatch.modelProfile (richer platform-resolved profile per
	// ADR-2026-05-12-worktype-and-model-profile-routing) when present.
	// It supersedes ResolvedProfile.Provider / Model / Effort so the
	// runner uses the exact model the platform chose rather than the
	// local-config fallback. Falls back to ResolvedProfile → default
	// provider chain for backwards compat.
	//
	// This reconciliation is delegated to runner.ReconcileResolvedProfile
	// (raw JSON in, not the typed daemon.SessionModelProfile/
	// SessionResolvedProfile shapes — see that function's doc comment for
	// why) so the daemon's preflight compiler applies the IDENTICAL logic
	// over the IDENTICAL ModelProfile/ResolvedProfile the platform sent:
	// before this was shared, preflight never saw these two SessionDetail
	// fields at all, so a receipt-bearing session whose authority depended
	// on either — Model, ProviderConfig, Endpoint — could never pass
	// ApplyPreparedHarness's authority digest.
	modelProfileJSON, err := marshalOptional(d.ModelProfile)
	if err != nil {
		return runner.QueuedWork{}, fmt.Errorf("marshal model profile: %w", err)
	}
	resolvedProfileJSON, err := marshalOptional(d.ResolvedProfile)
	if err != nil {
		return runner.QueuedWork{}, fmt.Errorf("marshal resolved profile: %w", err)
	}
	qw, err = runner.ReconcileResolvedProfile(qw, modelProfileJSON, resolvedProfileJSON)
	if err != nil {
		return runner.QueuedWork{}, err
	}
	return qw, nil
}

// marshalOptional returns nil (never the 4-byte JSON literal "null") for a
// nil pointer, so callers that gate reconciliation on len(raw) > 0 — see
// runner.ReconcileResolvedProfile — correctly treat an absent profile as
// absent rather than as a present-but-null one.
func marshalOptional[T any](v *T) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func applyResolvedRepositoryCompatibility(d *daemon.SessionDetail, admitted *runner.QueuedWork) error {
	if d.Repository == admitted.Repository {
		return nil
	}
	deny := func() error {
		return errors.New("operational payload repository differs from compatibility mirror without an exact authoritative project/resource resolution")
	}
	if admitted.Repository != "" || d.Repository == "" || admitted.RepositoryDeclaration != nil {
		return deny()
	}
	var identity struct {
		ProjectID          string `json:"projectId,omitempty"`
		RepositoryID       string `json:"repositoryId,omitempty"`
		ProjectName        string `json:"projectName,omitempty"`
		Repository         string `json:"repository,omitempty"`
		RequiresRepository bool   `json:"requiresRepository,omitempty"`
	}
	if err := json.Unmarshal(d.OperationalPayload, &identity); err != nil || identity.Repository != admitted.Repository {
		return deny()
	}
	authorized := false
	switch {
	case identity.ProjectID == "" && identity.RepositoryID == "" && !identity.RequiresRepository:
		// Legacy project-name-only dispatch. Its exact project selector must
		// survive unchanged across the immutable payload and daemon mirror.
		authorized = identity.ProjectName != "" && identity.ProjectName == admitted.ProjectName && identity.ProjectName == d.ProjectName
	case identity.ProjectID != "" && identity.RepositoryID != "":
		// Explicit repository-resource dispatch. Both authoritative identities
		// must equal the daemon's allowlist resolution keys.
		authorized = identity.ProjectID == d.ProjectID && identity.RepositoryID == d.RepositoryID
	case identity.ProjectID != "" && identity.RepositoryID == "" && identity.RequiresRepository:
		// Project-primary dispatch. Absence of a repository id is significant:
		// neither the payload nor compatibility mirror may invent one.
		authorized = identity.ProjectID == d.ProjectID && d.RepositoryID == ""
	}
	if !authorized {
		return deny()
	}
	// Preserve the host-local allowlist result without changing the receipted
	// operational payload bytes or their digest.
	admitted.Repository = d.Repository
	return nil
}

// providerConfigWithContextWindow and detailEndpointBinding used to live
// here; both moved to runner.ReconcileResolvedProfile (runner/
// resolved_profile_reconcile.go) so the daemon's preflight compiler
// (runner.ProviderView.PreflightExecution) applies the identical
// reconciliation this function delegates to above — see that function's doc
// comment.

// detailMCPServers re-types the daemon's PollMCPServer mirror slice into the
// runner-consumable agent.MCPServerConfig slice (WS5 agent-card MCP set). The
// daemon carries the agent-card MCP servers as PollMCPServer so it stays free
// of the agent package; this is the bridge. Field-for-field copy; nil/empty
// returns nil so the omitempty round-trip is faithful.
func detailMCPServers(in []daemon.PollMCPServer) []agent.MCPServerConfig {
	if len(in) == 0 {
		return nil
	}
	out := make([]agent.MCPServerConfig, len(in))
	for i, s := range in {
		out[i] = agent.MCPServerConfig{
			Name:    s.Name,
			Type:    s.Type,
			Command: s.Command,
			Args:    s.Args,
			Env:     s.Env,
			URL:     s.URL,
			Headers: s.Headers,
		}
	}
	return out
}

// detailSkills re-types the daemon's PollSkill mirror slice into the
// runner-consumable prompt.SkillSpec slice (WS5 agent-card inline skills).
// Field-for-field copy; nil/empty returns nil.
func detailSkills(in []daemon.PollSkill) []prompt.SkillSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]prompt.SkillSpec, len(in))
	for i, s := range in {
		out[i] = prompt.SkillSpec{
			ID:              s.ID,
			Body:            s.Body,
			DisallowedTools: s.DisallowedTools,
		}
	}
	return out
}

// providerNameFromDetail projects a non-authoritative compatibility name for
// pre-run display and gateway metadata. The runner's harness admission remains
// the only authoritative runtime selection.
//
// Display order: ModelProfile.ProviderID → the historical `agy` projection →
// ResolvedProfile.Provider → ResolvedProfile.Runner → default claude. Unknown
// explicit harnesses may fall through here for display only; runner admission
// still denies them and never follows this compatibility chain.
func providerNameFromDetail(d *daemon.SessionDetail) string {
	if d.ModelProfile != nil && d.ModelProfile.ProviderID != "" {
		return d.ModelProfile.ProviderID
	}
	if d.ResolvedProfile == nil {
		return string(agent.ProviderClaude)
	}
	if name, ok := harnessToProviderName(d.ResolvedProfile.Harness); ok {
		return name
	}
	if d.ResolvedProfile.Provider != "" {
		return d.ResolvedProfile.Provider
	}
	if d.ResolvedProfile.Runner != "" {
		return d.ResolvedProfile.Runner
	}
	return string(agent.ProviderClaude)
}

// harnessToProviderName handles the one historical pre-run projection needed
// for Antigravity logs/gateway metadata. It is not an admission selector: an
// unrecognized token returns ("", false), while the runner independently
// denies unknown explicit harness intent instead of following this fallback.
func harnessToProviderName(harness string) (string, bool) {
	switch harness {
	case "agy":
		return string(agent.ProviderAGYCLI), true
	default:
		return "", false
	}
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
