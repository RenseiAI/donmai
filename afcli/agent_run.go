package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
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
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/worktree"
	"github.com/RenseiAI/donmai/sessionshim"
)

// DefaultAgentRunDaemonURL is the local control HTTP address the default
// daemon binds to (127.0.0.1:7734). The `donmai agent run` subcommand
// fetches its session detail from <DefaultAgentRunDaemonURL>/api/daemon/sessions/<id>.
//
// It is a LAST resort, for a worker started by hand against the default
// daemon. A daemon-spawned worker is told its parent's real address in
// daemon.EnvDaemonControlURL and never reaches this constant — which matters
// because a named daemon instance binds a port that is deliberately not this
// one, and a worker that falls back here would dial a port nothing serves.
const DefaultAgentRunDaemonURL = "http://127.0.0.1:7734"

// agentRunOpts collects the per-invocation flags the runner consumes.
// Pulled out so tests can drive newAgentRunCmd's RunE directly without
// going through cobra's flag-parsing layer.
type agentRunOpts struct {
	localRuntime         bool
	localRuntimeContract string
	sessionID            string
	daemonURL            string
	worktree             string
	preserveWT           bool
	jsonOut              bool
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
	specDecorator                          agent.ExtensionDecorator
	capabilityRealizations                 *agent.CapabilityRealizationRegistry
	protectedRuntimeMCPSelector            runner.ProtectedRuntimeMCPSelector
	protectedRuntimeMCPV2Selector          runner.ProtectedRuntimeMCPV2Selector
	protectedRuntimeMCPDualSelectionPolicy runner.ProtectedRuntimeMCPDualSelectionPolicy
	piTrustedExtensions                    []providerpi.TrustedExtensionIdentity
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

var buildRegistryForAgentRun = func(logger *slog.Logger, hints agentRunCtorHints, agentBin string) *runner.Registry {
	return buildRegistryFromCtors(logger, agentRunProviderCtors(hints), agentBin)
}

// shimSeatFromEnv reports this worker's shim-owned seat, or nil when this
// process was not launched under the per-session shim launch contract. It is
// a var so tests can drive the shim-owned runner path without mutating the
// process environment.
var shimSeatFromEnv = func() *runner.ShimSeatConfig {
	launch, err := sessionshim.LaunchFromEnv(os.Getenv)
	if err != nil {
		return nil
	}
	return &runner.ShimSeatConfig{Attempt: launch.ProcessEpoch}
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
// the terminal Result; stderr receives slog output. Shim-owned processes may
// then remain alive for the bounded final-screen service window.
//
// Exit codes:
//
//   - 0  — runner.Run returned a Result with Status="completed" and
//     poster.Post succeeded. Soft warnings (failed teardown,
//     retried result post) do not change the exit code.
//   - 1  — runner.Run failed, Result.Status != "completed", or the owned
//     shim could not durably finalize.
//   - 2  — pre-flight failure (no session id, daemon unreachable,
//     session not found, registry construction failed).
//
// (F.2.8 — daemon wire-up.)
func newAgentRunCmd(cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	opts := agentRunOptions(cfg, bin)
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
			"Shim-owned workers can remain alive for the final-screen window\n" +
			"(60 seconds by default) after posting the result. Controller detach\n" +
			"or an explicit interrupt ends that transport lifetime.\n\n" +
			"Operators rarely invoke this directly. `" + bin + " host run` spawns it\n" +
			"on every accepted session. To debug a session locally, set\n" +
			"DONMAI_SESSION_ID and invoke this command against a running\n" +
			"daemon.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentRun(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.localRuntime, "local-runtime", false, "Use the trusted local runtime receiver")
	_ = cmd.Flags().MarkHidden("local-runtime")
	cmd.Flags().StringVar(&opts.localRuntimeContract, "local-runtime-contract", "", "Required local worker transport contract")
	_ = cmd.Flags().MarkHidden("local-runtime-contract")
	cmd.Flags().StringVar(&opts.sessionID, "session-id", "",
		"Session ID to run (default: $DONMAI_SESSION_ID)")
	cmd.Flags().StringVar(&opts.daemonURL, "daemon-url", "",
		"Daemon control URL (default: $DONMAI_DAEMON_URL, which the spawning daemon sets, or http://127.0.0.1:7734)")
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

func agentRunOptions(cfg Config, bin string) *agentRunOpts {
	return &agentRunOpts{
		bin: bin, specDecorator: cfg.AgentSpecExtensionDecorator,
		capabilityRealizations:                 cfg.CapabilityRealizations,
		protectedRuntimeMCPSelector:            cfg.ProtectedRuntimeMCPSelector,
		protectedRuntimeMCPV2Selector:          cfg.ProtectedRuntimeMCPV2Selector,
		protectedRuntimeMCPDualSelectionPolicy: cfg.ProtectedRuntimeMCPDualSelectionPolicy,
		piTrustedExtensions:                    append([]providerpi.TrustedExtensionIdentity(nil), cfg.PiTrustedExtensions...),
	}
}

func applyAgentRunCapabilityOptions(dst *runner.Options, src *agentRunOpts) {
	dst.CapabilityRealizations = src.capabilityRealizations
	dst.ProtectedRuntimeMCPSelector = src.protectedRuntimeMCPSelector
	dst.ProtectedRuntimeMCPV2Selector = src.protectedRuntimeMCPV2Selector
	dst.ProtectedRuntimeMCPDualSelectionPolicy = src.protectedRuntimeMCPDualSelectionPolicy
}

// agentRunMaxSessionDuration returns the runner timeout override for a
// daemon-spawned agent session. Three answers, in precedence order:
//
//   - Interactive sessions are human-driven and may remain attached beyond any
//     budget the dispatcher sized, so a negative duration disables the
//     runner-side cap entirely. Unchanged.
//   - A dispatched stage budget carrying a positive maxDurationSeconds IS the
//     session's maximum duration. The platform sized the stage; the runner's
//     two-hour default must not silently truncate it. It would: the value
//     returned here becomes runner.Options.MaxSessionDuration, which is the
//     timeout the runner wraps around the whole run, and the budget enforcer's
//     own duration cap is a context.WithDeadline derived FROM that ctx — it can
//     only ever pull the deadline in, never push it out. Before this, a
//     four-hour budget therefore died at two hours with the session classified
//     as a timeout.
//   - Everything else leaves the option at zero and retains
//     runner.DefaultMaxSessionDuration — the fallback for work dispatched with
//     no duration of its own.
//
// The dispatched value is clamped to runner.MaxSessionDurationCeiling. It
// arrives over the wire, and the comparison is made in SECONDS on purpose: a
// nonsense maxDurationSeconds large enough to overflow time.Duration would
// wrap to a negative value, and a negative return here does not mean "very
// long", it means "no runner-side cap at all". Clamping first is what keeps a
// malformed budget from producing an unbounded session.
func agentRunMaxSessionDuration(detail *daemon.SessionDetail) time.Duration {
	if detail == nil {
		return 0
	}
	if detail.Mode == prompt.InteractiveRunMode {
		return -1
	}
	budget := detail.StageBudget
	if budget == nil || budget.MaxDurationSeconds <= 0 {
		return 0
	}
	const ceilingSeconds = int64(runner.MaxSessionDurationCeiling / time.Second)
	if int64(budget.MaxDurationSeconds) >= ceilingSeconds {
		return runner.MaxSessionDurationCeiling
	}
	return time.Duration(budget.MaxDurationSeconds) * time.Second
}

// Daemon-URL provenance strings. They are part of the preflight error a
// failed worker leaves behind, so they name the SOURCE, not the value: an
// address printed on its own cannot tell whoever reads the log whether this
// process was pointed at it or fell back to it.
const (
	daemonURLSourceFlag           = "from --daemon-url"
	daemonURLSourceEnv            = "from $" + daemon.EnvDaemonControlURL
	daemonURLSourceBuiltinDefault = "built-in default; the spawning daemon set no $" +
		daemon.EnvDaemonControlURL + ", so a daemon on any other port is unreachable from here"
)

// resolveAgentRunDaemonURL picks the control address this worker dials and
// reports where it came from.
//
// Precedence is unchanged — flag, then environment, then the well-known
// default. What is new is the third answer being labelled as the guess it is.
// A worker spawned by a daemon should never reach it: the daemon states its own
// address (daemon.EnvDaemonControlURL). When it does reach it, the fallback is
// only ever right for a daemon on the well-known port, and the failure that
// follows on any other one is a connection refused with no hint as to why —
// which is exactly the silence this label exists to break.
func resolveAgentRunDaemonURL(flagValue string, lookupEnv func(string) string) (url, source string) {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v, daemonURLSourceFlag
	}
	if lookupEnv != nil {
		if v := strings.TrimSpace(lookupEnv(daemon.EnvDaemonControlURL)); v != "" {
			return v, daemonURLSourceEnv
		}
	}
	return DefaultAgentRunDaemonURL, daemonURLSourceBuiltinDefault
}

// runAgentRun is the testable entry point for the `agent run` command.
// Cobra-free; takes opts directly so tests can drive it with a fake
// daemon HTTP server.
func runAgentRun(ctx context.Context, cmd *cobra.Command, opts *agentRunOpts) error {
	if opts.localRuntimeContract != "" && (!opts.localRuntime || opts.localRuntimeContract != string(runner.RuntimeTransportLocalV2)) {
		return preflightErr("unsupported local worker transport contract")
	}
	// 1. Resolve the session id.
	sessionID := strings.TrimSpace(opts.sessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(os.Getenv("DONMAI_SESSION_ID"))
	}
	if sessionID == "" {
		return preflightErr("missing session id: pass --session-id or set DONMAI_SESSION_ID (the daemon spawner sets this automatically)")
	}

	// 2. Resolve the daemon URL, keeping the provenance: which of the three
	// sources answered decides whether a connection failure below is an
	// operator mistake or this process guessing at an address nobody gave it.
	daemonURL, daemonURLSource := resolveAgentRunDaemonURL(opts.daemonURL, os.Getenv)

	// 2b. Resolve the bearer token for the session-detail read. Precedence:
	// the per-session read credential the spawning daemon stated in this
	// worker's environment (authorizes exactly this session's detail read
	// and nothing else), then the sandbox provisioner's endpoint token
	// (an authenticated remote endpoint reached via DONMAI_DAEMON_URL
	// with the token in DONMAI_RUNTIME_JWT). When neither is set (the
	// default localhost loopback) the request carries no Authorization
	// header and the daemon answers with the credential-redacted shape.
	//
	// A local-runtime worker presents its attempt credential only. Its
	// spawn environment states both, but the local receiver authenticates
	// the attempt credential on the detail read and on every callback
	// (the credential cache below reuses this token) and never consults
	// the read credential.
	daemonToken := strings.TrimSpace(os.Getenv("DONMAI_RUNTIME_JWT"))
	if readToken := strings.TrimSpace(os.Getenv(runtimeenv.SessionReadTokenEnv)); readToken != "" && !opts.localRuntime {
		daemonToken = readToken
	}
	if opts.localRuntime && (daemonToken == "" || daemonURLSource == daemonURLSourceBuiltinDefault) {
		return preflightErr("local worker requires its explicit daemon origin and attempt credential")
	}

	if opts.localRuntime {
		if err := validateLocalAgentOrigin(daemonURL); err != nil {
			return preflightErr(err.Error())
		}
	}

	// 3. Set up signal handling so SIGTERM/SIGINT translates into a
	// clean ctx cancellation through the runner.
	// Keep OS shutdown distinct from the caller/run deadline from the start.
	// A signal arriving while terminal publication finishes must remain visible
	// to the later owner drain even if the business deadline already expired.
	ownerCtx, stopOwnerSignals := signal.NotifyContext(context.WithoutCancel(ctx), syscall.SIGTERM, syscall.SIGINT)
	defer stopOwnerSignals()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopShutdown := context.AfterFunc(ownerCtx, cancel)
	defer stopShutdown()
	runCtx, ownerLifetime := sessionshim.WithOwnerLifetime(runCtx)

	logger := slog.Default()
	logger.Info(
		"agent run: starting",
		"sessionId", sessionID,
		"daemonUrl", daemonURL,
		"daemonUrlSource", daemonURLSource,
	)

	// 4. Fetch session detail from the daemon (3-attempt exp backoff).
	detailClient := &http.Client{Timeout: 10 * time.Second}
	if opts.localRuntime {
		detailClient = localCallbackClient(10 * time.Second)
	}
	detail, err := fetchSessionDetail(runCtx, detailClient, daemonURL, sessionID, daemonToken)
	if err != nil {
		return preflightErr(fmt.Sprintf(
			"fetch session detail from %s (%s): %v", daemonURL, daemonURLSource, err))
	}
	// The bootstrap read consumed the per-session read credential: drop it
	// from this process's environment before anything it spawns can
	// inherit it. The credential cache below already captured the token
	// string it needs for refreshes; every harness and agent child this
	// worker spawns from here on inherits a credential-free environment.
	// A local-runtime worker never used it (it presents its attempt
	// credential only) — dropping the unused copy is the same hygiene.
	if err := os.Unsetenv(runtimeenv.SessionReadTokenEnv); err != nil {
		return preflightErr(fmt.Sprintf("drop session read credential after bootstrap: %v", err))
	}
	if opts.localRuntime {
		if err := validateLocalAgentDetail(daemonURL, detail); err != nil {
			return preflightErr(err.Error())
		}
		if detail.SessionID != sessionID {
			return preflightErr("local detail session identity mismatch")
		}
	}
	logger.Info(
		"agent run: session detail fetched",
		"sessionId", detail.SessionID,
		"identifier", detail.IssueIdentifier,
		"provider", providerNameFromDetail(detail),
		"workType", detail.WorkType,
	)
	// A detail read that crossed no credential boundary answers with the
	// credential fields cleared. A worker that bootstraps from such an
	// answer cannot talk to the platform it was claimed for, so log the
	// shortfall loudly: production daemons always state the session's
	// own read credential in the spawn environment, and its absence here
	// means this worker was started by hand (or by a daemon that
	// predates the credential) rather than by its own session's spawn.
	if detail.AuthToken == "" {
		logger.Warn(
			"agent run: session detail carries no runtime credential; platform calls will fail unless the daemon refreshes it",
			"sessionId", sessionID,
			"daemonUrlSource", daemonURLSource,
		)
	}
	if len(detail.AdmissionReceipt) > 0 {
		hostReceipt, err := executioncell.DecodeHostAdaptationReceipt(detail.HostAdaptationReceipt)
		if err != nil || hostReceipt.RequestID != detail.SessionID ||
			hostReceipt.WorkerID != detail.WorkerID || hostReceipt.Decision != "ready" {
			return fmt.Errorf("receipt-bearing session has no valid daemon adaptation-ready receipt")
		}
	}
	credentialClient := &http.Client{Timeout: 5 * time.Second}
	if opts.localRuntime {
		credentialClient = localCallbackClient(5 * time.Second)
	}
	credentialCache := newAgentRunCredentialCache(
		credentialClient,
		daemonURL,
		sessionID,
		daemonToken,
		detail,
	)

	if opts.localRuntime {
		credentialCache.localRuntime = true
		credentialCache.authToken = daemonToken
	}

	// 5. Construct registry, runner, and run.
	agentBin := opts.bin
	if agentBin == "" {
		agentBin = "donmai"
	}
	hints := agentRunHints(detail)
	selectedLocalHarness := ""
	if opts.localRuntime {
		// Only local/v2 replaces the existing constructor hint with an exact
		// admitted binding. Local/v1 and controller behavior stay unchanged.
		if opts.localRuntimeContract == string(runner.RuntimeTransportLocalV2) {
			hints.CodexHostSessionAuth, err = localCodexHostSessionHint(detail)
			if err != nil {
				return preflightErr(fmt.Sprintf("local host authentication binding: %v", err))
			}
			selectedLocalHarness = detail.ResolvedProfile.Harness
		}
	}
	hints.PiTrustedExtensions = append([]providerpi.TrustedExtensionIdentity(nil), opts.piTrustedExtensions...)
	var reg *runner.Registry
	if opts.localRuntime {
		reg, err = localAgentRegistry(logger, hints, agentBin, localWorkerTransport(opts.localRuntimeContract), selectedLocalHarness)
		if err != nil {
			return preflightErr(err.Error())
		}
	} else {
		reg = buildRegistryForAgentRun(logger, hints, agentBin)
	}
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
	qw, admission, admissionErr := reg.PreflightHarnessWithProtectedRuntimeMCPSelection(qw, opts.capabilityRealizations, opts.protectedRuntimeMCPSelector, opts.protectedRuntimeMCPV2Selector, opts.protectedRuntimeMCPDualSelectionPolicy)
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

	var callbackClient *http.Client
	if opts.localRuntime {
		callbackClient = localCallbackClient(30 * time.Second)
	}
	poster, err := result.NewPoster(result.Options{
		HTTPClient:         callbackClient,
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

	runnerOptions := runner.Options{
		HTTPClient:                callbackClient,
		Registry:                  reg,
		WorktreeManager:           wm,
		Poster:                    poster,
		CredentialProvider:        credentialCache.runnerCredentials,
		Logger:                    logger,
		MaxSessionDuration:        agentRunMaxSessionDuration(detail),
		PreserveWorktreeOnFailure: opts.preserveWT,
		// Tracker transitions and diagnostic comments are workflow actions. The
		// production worker publishes the terminal result and leaves those
		// optional mutations to visible, authored post-session nodes.
		SkipPostSession: true,
		// The library stays env-free; this binary is the operator boundary.
		// Dispatch capability `llm-span-ingest` can also enable the pipeline
		// per session once a compatible server advertises it.
		SpanEmissionEnabled: !opts.localRuntime && donmaiSpanTracingEnabled(),
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
		// AdditionalExtensionDecorator mirrors opts.specDecorator into
		// Runner's own prepared-source authority self-check (runLoop, via
		// buildPreparedSourceSpec) — the SAME decorator
		// decorateRegistryProviders already wraps this registry's providers
		// with above, so both computations this process performs agree with
		// each other and with the daemon's preflight compiler (see
		// runner.ReconcileAdditionalExtensions).
		AdditionalExtensionDecorator: opts.specDecorator,
		// A seat launched under per-session shim ownership persists every
		// terminal status body in the standalone outbox before the first
		// send, so a runner killed after persist but before send has its
		// exact bytes replayed once by the daemon. Detection mirrors the
		// harness's own launch-contract read: the contract in this
		// process's environment is what makes this seat shim-owned, and
		// its monotonic process epoch is the outbox attempt. Absent the
		// contract this stays nil and the runner behaves exactly as before.
		ShimSeat: shimSeatFromEnv(),
		// Runtime memory-inject (v2) needs NO worker config: the runner always
		// wires the inject handler when the provider supports injection, and the
		// PLATFORM decides per-session whether to deliver (per-project memory
		// config). No env var. Providers without injection support fall back to
		// the dispatch-time fold (v1).
		// Backstop runs by default — the daemon-spawned worker is
		// the production code path; tests use the in-process entry.
		// Live quota updates ride back to the admitting daemon: the
		// sparse windows the session's harness stream carries (a codex
		// `account/rateLimits/updated` notification, a claude
		// `rate_limit_event`) merge there by window id onto the probe
		// snapshot behind the heartbeat quota field. Best-effort and
		// bounded; an unreachable daemon never stalls the session.
		QuotaReporterForSession: func(_, harness string) *runner.QuotaReporter {
			return runner.NewQuotaReporter(callbackClient, daemonURL, sessionID, harness, daemonToken, logger)
		},
	}
	applyAgentRunCapabilityOptions(&runnerOptions, opts)
	r, err := runner.New(runnerOptions)
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
	if admissionErr == nil && !opts.localRuntime {
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
	// appearance as a stuck spawn. Posting running at spawn makes the claim
	// observably alive from the first seconds of the run, so a no-activity
	// failure is a distinct reap signal.
	//
	// The eager post races the later maybePostRunning, and that race is
	// safe but NOT a no-op: the platform answers a repeated running
	// transition with 409 Conflict rather than ignoring it. The loser of the
	// race learns the transition already happened (maybePostRunning
	// discards the conflict at debug; postSessionRunning below treats a 409
	// on its own retries as "an earlier attempt landed" for the same
	// reason).
	if admissionErr == nil {
		runningToken := detail.AuthToken
		if opts.localRuntime {
			_, runningToken, err = credentialCache.current(runCtx)
			if err != nil {
				return preflightErr("local result transport authentication is unavailable")
			}
		}
		postSessionRunning(runCtx, credentialClient, logger,
			detail.PlatformURL, sessionID, detail.WorkerID, runningToken)
	}

	logger.Info("donmai agent run: invoking runner.RunAdmitted", "sessionId", qw.SessionID)
	res, runErr := r.RunAdmitted(runCtx, qw, admission)

	out := cmd.OutOrStdout()
	if opts.jsonOut && res != nil {
		if err := emitResultJSON(out, res); err != nil {
			logger.Warn("donmai agent run: emit result json failed", "err", err)
		}
	}

	// Business completion (including its terminal post/JSON) has already happened.
	// Keep only this command's shim transport owners alive for the final screen;
	// the runner's canceled stage context must not turn success into budget failure.
	if ownerErr := ownerLifetime.Wait(ownerCtx); ownerErr != nil {
		logger.Error("agent run: session shim owner drain failed", "err", ownerErr)
		return errors.Join(runErr, fmt.Errorf("session shim owner drain: %w", ownerErr))
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

// postSessionRunningMaxAttempts bounds the eager running-post retry loop so a
// slow or loaded host gets a few chances without delaying the run
// indefinitely. Three attempts cover the "fails twice then returns 200"
// case; with the delay schedule below the loop spans at most 3 s of backoff
// sleep (plus per-request client timeouts), staying well inside the
// claimed-stale window the nudge exists to beat. The literal count is pinned
// by the exhausted-retry test — do not raise it without re-checking that
// budget.
const postSessionRunningMaxAttempts = 3

// postSessionRunningRetryDelay is the backoff between running-post attempts,
// indexed by the attempt that just failed (1-based). It follows the
// heartbeat's 1s/2s/4s exponential convention (runtime/heartbeat's
// DefaultMaxAttemptsPerTick) with equal jitter in [base/2, base] — the same
// shape attachclient/backoff.go applies to reconnects — so a platform
// returning fast 5xx errors cannot burn all attempts in under a second, and
// many workers retrying at once do not stampede in lock-step. Worst case the
// two sleeps total 3 s (typical ~2.25 s). A var so tests can shrink it to
// zero without waiting out real backoff.
var postSessionRunningRetryDelay = func(failedAttempt int) time.Duration {
	return jitteredRunningRetryDelay(time.Duration(1<<(failedAttempt-1)) * time.Second)
}

// runningRetryJitterIntn is the randomness seam behind the retry backoff's
// equal jitter. A var so tests can pin both jitter bounds deterministically.
var runningRetryJitterIntn = rand.Int64N

// jitteredRunningRetryDelay applies the repo's equal-jitter convention to a
// base backoff: a delay in [base/2, base], never zero and never above base
// (attachclient/backoff.go uses the identical shape for reconnects).
func jitteredRunningRetryDelay(base time.Duration) time.Duration {
	half := base / 2
	if half <= 0 {
		return base
	}
	return half + time.Duration(runningRetryJitterIntn(int64(half)+1)) //nolint:gosec // G404: jitter, not crypto
}

// runningPostFailedMessage is the final-failure log line for the eager
// running post. "after N attempts" is only honest wording when N > 1 — a
// first-attempt 4xx fails fast with zero retries behind it, and claiming
// "after retries" there misleads whoever reads the log.
func runningPostFailedMessage(attempts int) string {
	if attempts > 1 {
		return fmt.Sprintf("agent run: status=running post failed after %d attempts", attempts)
	}
	return "agent run: status=running post failed on the first attempt (no retries)"
}

// postSessionRunning fires an eager POST
// /api/sessions/<id>/status with {"status":"running","workerId":"..."}
// against the PLATFORM (not the local daemon) before the runner spawns the
// provider. It mirrors the wire shape of runtime/activity's maybePostRunning
// so the two are interchangeable — but they are NOT idempotent on the
// platform: a repeated running transition is answered with 409 Conflict, not
// ignored. Two consequences:
//
//   - A 409 arriving on attempt 2 or later most likely means an earlier
//     attempt landed (its response was lost client-side) and the session is
//     already running. That is logged at info as success, not as the
//     fail-fast warn every other 4xx gets. A 409 on the first attempt has no
//     earlier attempt to credit and stays a permanent client error.
//   - Racing the later maybePostRunning is safe only in the sense that the
//     loser's conflict is discarded — never because the platform no-ops the
//     repeat.
//
// The post is retried with the jittered backoff above up to
// postSessionRunningMaxAttempts on transient failures (transport errors and
// 5xx); every other 4xx response is permanent and fails fast. All failure
// paths are best-effort — the running nudge is pure observability (it makes
// the claim observably alive before any activity arrives); it must never
// fail the worker. The final failure logs at warn so a lost nudge is
// visible; earlier attempts log at debug, and a cancelled context aborts the
// backoff silently (the run is already shutting down). A no-op when
// platformURL is empty (standalone / no-platform mode, where there is no
// platform status endpoint to hit).
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
	for attempt := 1; attempt <= postSessionRunningMaxAttempts; attempt++ {
		last := attempt == postSessionRunningMaxAttempts
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body)) //nolint:gosec // G704: platformURL is the operator-configured platform base URL (trusted daemon/session config, not request-derived input)
		if err != nil {
			if last {
				logger.Warn(runningPostFailedMessage(attempt), "sessionId", sessionID, "attempts", attempt, "err", err)
			} else {
				logger.Debug("agent run: status=running new request failed", "sessionId", sessionID, "attempt", attempt, "err", err)
			}
			if !last && !sleepForRunningRetry(ctx, attempt) {
				return
			}
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}
		resp, err := client.Do(req) //nolint:gosec // G704: same trusted operator-configured URL as above
		if err != nil {
			if last {
				logger.Warn(runningPostFailedMessage(attempt), "sessionId", sessionID, "attempts", attempt, "err", err)
			} else {
				logger.Debug("agent run: status=running post failed", "sessionId", sessionID, "attempt", attempt, "err", err)
			}
			if !last && !sleepForRunningRetry(ctx, attempt) {
				return
			}
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			logger.Info("agent run: session flipped to running (pre-spawn)",
				"sessionId", sessionID, "workerId", workerID)
			return
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// A 409 on a retry most likely means an earlier attempt landed:
			// its response was lost client-side, the session is already
			// running, and the platform answers the repeat with a conflict.
			// That is success for this nudge — log it at info, not as the
			// fail-fast warn every other 4xx gets. A 409 on the first attempt
			// has no earlier attempt to credit, so it stays a permanent
			// client error.
			if resp.StatusCode == http.StatusConflict && attempt > 1 {
				logger.Info("agent run: session already running — earlier running post likely landed",
					"sessionId", sessionID, "attempt", attempt, "status", resp.StatusCode)
				return
			}
			logger.Warn(runningPostFailedMessage(attempt), "sessionId", sessionID, "attempts", attempt, "status", resp.StatusCode)
			return
		}
		if last {
			logger.Warn(runningPostFailedMessage(attempt), "sessionId", sessionID, "attempts", attempt, "status", resp.StatusCode)
			return
		}
		logger.Debug("agent run: status=running non-2xx", "sessionId", sessionID, "attempt", attempt, "status", resp.StatusCode)
		if !sleepForRunningRetry(ctx, attempt) {
			return
		}
	}
}

// sleepForRunningRetry waits out the backoff after a failed running-post
// attempt. It reports false when the context expired first, in which case
// the caller gives up without logging a retry-exhausted warning — the run
// is already shutting down.
func sleepForRunningRetry(ctx context.Context, failedAttempt int) bool {
	delay := postSessionRunningRetryDelay(failedAttempt)
	if delay <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
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
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
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
	localRuntime bool
	mu           sync.Mutex
	client       *http.Client
	daemonURL    string
	sessionID    string
	daemonToken  string
	workerID     string
	authToken    string
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
	if c.localRuntime {
		if fetchErr != nil {
			return c.workerID, c.authToken, fetchErr
		}
		if err := validateLocalAgentDetail(c.daemonURL, detail); err != nil {
			return c.workerID, c.authToken, err
		}
		if detail.SessionID != c.sessionID || detail.WorkerID != c.workerID {
			return c.workerID, c.authToken, errors.New("local runtime credential binding changed")
		}
		return c.workerID, c.daemonToken, nil
	}
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
// Foundation-runtime-stubs adds two more probe-and-skip entries
// (gemini, opencode). Each follows
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
	return BuildDecoratedAgentRunRegistry(logger, nil)
}

// BuildDecoratedAgentRunRegistry is [BuildAgentRunRegistry] plus one more
// step: when decorate is non-nil, every registered provider is wrapped via
// agent.DecorateProvider(p, decorate) — the SAME wrapping runAgentRun applies
// to its own per-session registry (decorateRegistryProviders below) when
// Config.AgentSpecExtensionDecorator is set.
//
// Every entry point that can reach agent.ApplyPreparedHarness for a
// receipt-bearing session — the daemon's own preflight compiler
// (afcli/daemon_run.go's newDaemonRunCmd, via runner.NewProviderView) and the
// per-session `agent run` registry (runAgentRun) alike — MUST build its
// registry through this one function (or BuildAgentRunRegistry, its
// decorate==nil case) with the SAME decorate value, or the two can silently
// diverge again: before this function existed, only runAgentRun's registry
// carried the wrapping, so a provider's decorator-appended
// Spec.AdditionalExtensions were invisible to the daemon's persisted
// ToolLifecycleReceipt but present in the real spawn's recompute — an
// *agent.ToolLifecycleDriftError{Fields:["entries"]} at spawn instead of a
// receipt that already told the truth. See runner.ReconcileAdditionalExtensions
// for the sibling half of this fix (the daemon's compile-time Spec itself
// must also apply decorate before persisting a plan — wrapping the Provider
// alone does nothing for a compile site that never calls Provider.Spawn).
func BuildDecoratedAgentRunRegistry(logger *slog.Logger, decorate agent.ExtensionDecorator) *runner.Registry {
	reg := buildRegistryFromCtors(logger, agentRunProviderCtors(), "donmai")
	if decorate != nil {
		decorateRegistryProviders(reg, decorate)
	}
	return reg
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

	// PiTrustedExtensions comes only from afcli.Config in the compiled
	// embedder. It is never derived from SessionDetail or ProviderConfig.
	PiTrustedExtensions []providerpi.TrustedExtensionIdentity
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

// localCodexHostSessionHint uses admitted local/v2 bytes instead of the
// controller's optional AuthMode mirror. It runs before provider construction
// so an unselected or forged profile cannot cause a host credential lookup.
func localCodexHostSessionHint(detail *daemon.SessionDetail) (bool, error) {
	if detail == nil || detail.ResolvedProfile == nil || detail.SessionID == "" || detail.WorkerID == "" {
		return false, errors.New("local admission has no complete worker identity")
	}
	receipt, err := executioncell.DecodeAdmissionReceipt(detail.AdmissionReceipt)
	if err != nil {
		return false, fmt.Errorf("decode local admission receipt: %w", err)
	}
	cell, err := executioncell.DecodeResolvedExecutionCell(detail.EffectiveCell)
	if err != nil {
		return false, fmt.Errorf("decode local execution cell: %w", err)
	}
	binding, err := executioncell.DecodeRuntimeBinding(detail.ExecutionRuntimeBinding)
	if err != nil {
		return false, fmt.Errorf("decode local runtime binding: %w", err)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(detail.HostAdaptationReceipt)
	if err != nil {
		return false, fmt.Errorf("decode local host adaptation: %w", err)
	}
	payloadDigest, err := executioncell.DigestOperationalPayload(detail.OperationalPayload)
	if err != nil {
		return false, fmt.Errorf("digest local operational payload: %w", err)
	}
	admitted := receipt.Value()
	if admitted.Decision != executioncell.AdmissionAdmitted || admitted.Cell == nil ||
		admitted.RequestID != detail.SessionID || admitted.OperationalPayloadDigest != payloadDigest ||
		!reflect.DeepEqual(*admitted.Cell, cell) ||
		binding.ContractVersion != executioncell.RuntimeBindingV2ContractVersion ||
		binding.RequestID != detail.SessionID || binding.WorkerID != detail.WorkerID ||
		binding.PlacementID != cell.Placement.ID || binding.ClaimID != "" ||
		binding.PreflightRegistration == nil || !binding.PreflightRegistration.Required ||
		host.Decision != "ready" || host.RequestID != detail.SessionID ||
		host.WorkerID != detail.WorkerID || host.PlacementID != cell.Placement.ID || host.ClaimID != "" ||
		cell.Placement.Kind != executioncell.PlacementHost || cell.Placement.Resolution != executioncell.PlacementExact ||
		cell.SessionMode != executioncell.SessionAutonomous ||
		detail.Harness != cell.Harness.ID || detail.ResolvedProfile.Harness != cell.Harness.ID ||
		detail.ResolvedProfile.Model != cell.Model.ID || detail.ModelProfile != nil {
		return false, errors.New("local worker detail disagrees with exact admitted host binding")
	}
	endpoint := detail.ResolvedProfile.Endpoint
	if endpoint == nil || endpoint.EndpointID != cell.Endpoint.ID ||
		endpoint.EndpointRevision != cell.Endpoint.Revision || endpoint.Protocol != cell.Endpoint.Protocol ||
		endpoint.EndpointOperator != cell.Endpoint.Operator || endpoint.Model != cell.Model.ID ||
		endpoint.ModelAuthor != cell.Model.Author || endpoint.AuthBindingID != cell.AuthBinding.ID ||
		endpoint.Mechanism != string(cell.AuthBinding.Mechanism) ||
		endpoint.AuthAuthority != cell.AuthBinding.Authority ||
		endpoint.AuthCommercialMode != string(cell.AuthBinding.CommercialMode) ||
		endpoint.AuthBindingScope != string(cell.AuthBinding.BindingScope) ||
		endpoint.AuthPortability != string(cell.AuthBinding.Portability) ||
		endpoint.AuthDelivery != string(cell.AuthBinding.Delivery) {
		return false, errors.New("local worker profile differs from admitted endpoint and auth binding")
	}
	if cell.Harness.ID == string(agent.HarnessClaudeCode) {
		return false, nil
	}
	if cell.Harness.ID != string(agent.HarnessCodex) || cell.Model.Author != "openai" ||
		cell.Endpoint.Operator != "openai" || cell.AuthBinding.Authority != "openai" ||
		cell.AuthBinding.Mechanism != executioncell.AuthCLISession ||
		cell.AuthBinding.CommercialMode != executioncell.CommercialSubscription ||
		cell.AuthBinding.BindingScope != executioncell.ScopeHost ||
		cell.AuthBinding.Portability != executioncell.HostBound ||
		cell.AuthBinding.Delivery != executioncell.DeliveryHostCLIHomeReference ||
		endpoint.Company != "openai" || endpoint.Host != string(agent.HostOAuthCLI) ||
		(detail.ResolvedProfile.AuthMode != "" && detail.ResolvedProfile.AuthMode != string(agent.AuthHostSession)) {
		return false, errors.New("local Codex host login lacks exact admitted authority")
	}
	return true, nil
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

func piCtorOptions(h agentRunCtorHints) providerpi.Options {
	return providerpi.Options{TrustedExtensions: append([]providerpi.TrustedExtensionIdentity(nil), h.PiTrustedExtensions...)}
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
		// OpenCode (registered below) ships two real managed-spawn lanes
		// (CLI one-shot, serve/HTTP with real tool/MCP policy delivery)
		// plus a fail-closed external-attach posture — see its own ctor
		// comment for how PreferServer routes between them. Gemini is a
		// full streaming impl against generativelanguage.googleapis.com.
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
		// pi is registration-only today, mirroring opencode: the
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
		{name: "pi", new: func() (agent.Provider, error) { return providerpi.New(piCtorOptions(h)) }},
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
		PullRequest:             d.PullRequest,
		QueuedWork: prompt.QueuedWork{
			SessionID:            d.SessionID,
			SessionName:          d.SessionName,
			AgentCardID:          d.AgentCardID,
			AgentCardName:        d.AgentCardName,
			IssueID:              d.IssueID,
			IssueIdentifier:      d.IssueIdentifier,
			LinearSessionID:      d.LinearSessionID,
			ProviderSessionID:    d.ProviderSessionID,
			ProjectName:          d.ProjectName,
			OrganizationID:       d.OrganizationID,
			Repository:           d.Repository,
			Ref:                  d.Ref,
			BaseRef:              d.BaseRef,
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
			ContinuePullRequest:  detailContinuePullRequest(d.ContinuePullRequest),
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
		SeatBudget:            detailSeatBudget(d.SeatBudget),
	}
	if len(d.OperationalPayload) > 0 {
		// Decode into a zero value: absent receipted fields must stay absent rather
		// than inheriting an unreceipted compatibility mirror.
		if err := runner.ValidateBaseRefMember(d.OperationalPayload); err != nil {
			return runner.QueuedWork{}, fmt.Errorf("operational payload base branch: %w", err)
		}
		if err := runner.ValidateExecutionSecurityMember(d.OperationalPayload); err != nil {
			return runner.QueuedWork{}, fmt.Errorf("operational payload: %w", err)
		}
		var admitted runner.QueuedWork
		if err := json.Unmarshal(d.OperationalPayload, &admitted); err != nil {
			return runner.QueuedWork{}, fmt.Errorf("operational payload: %w", err)
		}
		// The dispatched pull request joins this guard because it decides
		// WHICH COMMITS the workarea materializes — exactly the class of
		// intent the receipted payload is authoritative for. A mirror that
		// disagreed would otherwise silently lose the record and provision a
		// plain branch clone, which is the failure this change exists to
		// prevent.
		if d.BaseRef != admitted.BaseRef || !reflect.DeepEqual(d.RepositoryDeclaration, admitted.RepositoryDeclaration) ||
			d.WorkareaMode != admitted.WorkareaMode || d.ParentWorkareaID != admitted.ParentWorkareaID ||
			!reflect.DeepEqual(d.RepositoryFilter, admitted.RepositoryFilter) || d.CacheSeedID != admitted.CacheSeedID ||
			!reflect.DeepEqual(d.PullRequest, admitted.PullRequest) ||
			!reflect.DeepEqual(detailContinuePullRequest(d.ContinuePullRequest), admitted.ContinuePullRequest) {
			return runner.QueuedWork{}, errors.New("operational payload workarea intent differs from compatibility mirror")
		}
		if d.AgentCardID != admitted.AgentCardID || d.AgentCardName != admitted.AgentCardName {
			return runner.QueuedWork{}, errors.New("operational payload agent card annotation differs from compatibility mirror")
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
	stageBudgetJSON, err := marshalOptional(d.StageBudget)
	if err != nil {
		return runner.QueuedWork{}, fmt.Errorf("marshal stage budget: %w", err)
	}
	qw, err = runner.ReconcileStageBudget(qw, stageBudgetJSON)
	if err != nil {
		return runner.QueuedWork{}, err
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

// The resolvedProfile limit folding (contextWindow, maxOutputTokens) and
// detailEndpointBinding used to live here; both moved to
// runner.ReconcileResolvedProfile (runner/
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

// detailContinuePullRequest re-types the daemon's PollContinuePullRequest
// mirror into the runner-consumable prompt.ContinuePullRequest. Nil in
// returns nil so the omitempty round-trip is faithful.
func detailContinuePullRequest(in *daemon.PollContinuePullRequest) *prompt.ContinuePullRequest {
	if in == nil {
		return nil
	}
	return &prompt.ContinuePullRequest{
		Number:  in.Number,
		HeadRef: in.HeadRef,
		HeadSha: in.HeadSha,
	}
}

// detailSeatBudget re-types the daemon's per-seat budget mirror into the
// runner-consumable shape. Nil stays nil: budgeting off means the seat
// spawns exactly as before.
func detailSeatBudget(in *daemon.SessionSeatBudget) *runner.SeatBudget {
	if in == nil {
		return nil
	}
	return &runner.SeatBudget{
		Mode:     in.Mode,
		CPUs:     in.CPUs,
		MemoryMB: in.MemoryMB,
		Detail:   in.Detail,
	}
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

func localWorkerTransport(contract string) runner.RuntimeTransportMode {
	if contract == string(runner.RuntimeTransportLocalV2) {
		return runner.RuntimeTransportLocalV2
	}
	return runner.RuntimeTransportLocal
}
