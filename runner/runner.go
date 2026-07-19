package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/mcp"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// activitySink is the per-session seam that pushes runner-observed
// events to the platform's /api/sessions/<id>/activity buffer. The
// runner builds one [activitySink] per [Runner.Run] (today via
// [activitySinkFromConfig], which constructs a
// [github.com/RenseiAI/donmai/runtime/activity.Poster]).
//
// Defined as an interface so the runner test suite can substitute a
// recording fake without spinning up an HTTP server. Send is
// non-blocking and best-effort; implementations must drop events
// rather than block the runner on platform I/O.
type activitySink interface {
	Send(ctx context.Context, ev agent.Event)
}

// spanEventProcessor is the per-session correlation seam. Process may prepend
// a synthetic aggregate LlmCallEvent before a terminal ResultEvent; callers
// persist and observe every returned event in order. Finish emits the session
// root after the runner has finalized its result.
type spanEventProcessor interface {
	Process(agent.Event) []agent.Event
	Finish(status, message string)
}

type noopSpanProcessor struct{}

func (noopSpanProcessor) Process(ev agent.Event) []agent.Event { return []agent.Event{ev} }
func (noopSpanProcessor) Finish(string, string)                {}

// noopSink is the default sink used when no real poster has been
// constructed (e.g. unit tests, or a runner running offline). All
// methods are no-ops.
type noopSink struct{}

func (noopSink) Send(context.Context, agent.Event) {}

// Default values for [Options]. Exposed for tests and operator
// debugging; production daemons override via [Options].
const (
	// DefaultMaxSessionDuration is the upper-bound timeout the runner
	// applies to ctx when [Options.MaxSessionDuration] is zero. Two
	// hours matches the legacy TS MAX_SESSION_DURATION constant.
	DefaultMaxSessionDuration = 2 * time.Hour

	// DefaultEventBufferSize is the buffered-channel size used to
	// decouple event mirroring from the provider goroutine. A small
	// number keeps memory bounded; spikes block the provider for at
	// most one event before backpressure kicks in.
	DefaultEventBufferSize = 64

	// DefaultIdleTimeout is the no-progress watchdog window applied to
	// the event stream when [Options.IdleTimeout] is zero. The timer
	// resets on every agent.Event; when it expires with no event the
	// runner cancels the stream and classifies the session as
	// FailureNoProgress. Twelve minutes is comfortably longer than a
	// normal think/tool-call gap but short enough to free a wedged slot
	// well before the 2h MaxSessionDuration. A NEGATIVE
	// Options.IdleTimeout disables the watchdog.
	DefaultIdleTimeout = 12 * time.Minute

	// terminalResultPostTimeout bounds the detached cleanup context used for
	// terminal result delivery after the run context has expired or been
	// cancelled. Posting remains ahead of worktree teardown.
	terminalResultPostTimeout = 30 * time.Second
)

// Options carries the long-lived configuration a Runner needs.
//
// Required fields: Registry, WorktreeManager, Poster. The remaining
// fields have sensible defaults so simple consumers can call
// New(Options{Registry: r, WorktreeManager: m, Poster: p}) without
// having to plumb every collaborator.
type Options struct {
	// Registry is the provider registry the runner consults on each
	// Run to resolve QueuedWork.ResolvedProfile.Provider. Required.
	Registry *Registry

	// WorktreeManager owns clone/teardown of per-session worktrees.
	// Required.
	WorktreeManager *worktree.Manager

	// Poster posts the terminal Result back to the platform.
	// Required.
	Poster *result.Poster

	// CredentialProvider supplies the freshest platform worker credentials
	// for long-running child sessions. Heartbeats call it before every tick so
	// they can pick up daemon-side runtime-token refreshes.
	CredentialProvider CredentialProvider

	// EnvComposer builds the agent subprocess env. Defaults to
	// env.NewComposer().
	EnvComposer *env.Composer

	// MCPBuilder builds the per-session MCP stdio config tmpfile.
	// Defaults to mcp.NewBuilder().
	MCPBuilder *mcp.Builder

	// StateStore writes .agent/state.json snapshots. Defaults to
	// state.NewStore().
	StateStore *state.Store

	// PromptBuilder renders the (system, user) prompt pair.
	// Defaults to &prompt.Builder{}.
	PromptBuilder *prompt.Builder

	// HTTPClient is forwarded to the heartbeat pulser for
	// /api/sessions/<id>/lock-refresh calls. Defaults to a 30s-timeout
	// http.Client.
	HTTPClient *http.Client

	// Logger receives Debug/Info/Warn lines describing each step.
	// Defaults to slog.Default().
	Logger *slog.Logger

	// Now is injected for deterministic tests. Defaults to time.Now.
	Now func() time.Time

	// MaxSessionDuration is the per-Run upper-bound on ctx. Zero
	// falls back to DefaultMaxSessionDuration. Negative disables the
	// runner-side timeout (caller is responsible for ctx expiry).
	MaxSessionDuration time.Duration

	// IdleTimeout is the no-progress watchdog window applied to the
	// event stream. A resettable timer is armed in consumeEvents and
	// reset on every agent.Event; when it expires with no event in the
	// window the runner cancels the stream and surfaces
	// FailureNoProgress — catching the wedged-but-channel-alive class
	// the MaxSessionDuration ctx misses (a session whose events channel
	// is still open but which has stopped making forward progress).
	//
	// Zero falls back to DefaultIdleTimeout (watchdog ON by default).
	// NEGATIVE disables the watchdog entirely (caller relies solely on
	// MaxSessionDuration / external ctx for liveness).
	IdleTimeout time.Duration

	// PreserveWorktreeOnFailure keeps the worktree on disk after a
	// failed Run for debugging. Defaults to true in v0.5.0 per F.1.1
	// §10 Q7 — flip to false after smoke-harness confidence is high.
	PreserveWorktreeOnFailure bool

	// PreserveWorktreeAlways keeps the worktree on disk after every
	// Run regardless of outcome. Used by tests that need to inspect
	// .agent/events.jsonl or state.json post-Run, and by debug
	// builds that want zero auto-cleanup. Defaults to false.
	PreserveWorktreeAlways bool

	// SkipBackstop disables the deterministic backstop entirely.
	// Used by tests that don't have a real git worktree.
	SkipBackstop bool

	// SkipSteering disables the steering stage of tail recovery.
	// Used by tests that need a deterministic recovery flow.
	SkipSteering bool

	// SkipPostSession disables the post-session Linear state-transition
	// block (loop.go step 11b). Tests that don't have a
	// platform mock with /api/issue-tracker-proxy support, or that
	// want to assert on the pre-transition Result envelope, set this
	// to skip the block entirely. Production daemons leave it false.
	SkipPostSession bool

	// HeartbeatInterval overrides the per-session heartbeat cadence.
	// Zero falls back to runtime/heartbeat.DefaultInterval.
	HeartbeatInterval time.Duration

	// SpanEmissionEnabled explicitly enables the additive per-call span
	// pipeline. Platform dispatchers may instead advertise
	// CapabilitySpanIngest per session; either signal enables it. Default false
	// is mixed-version safe while the ingest route is unavailable.
	SpanEmissionEnabled bool

	// SpanEndpointPath overrides runtime/span.DefaultEndpointPath. Empty uses
	// the accepted /api/daemon/traces path. This exists for OSS embedders and
	// contract tests; no hosted address is baked into the library.
	SpanEndpointPath string

	// KitSkillSources is the optional slice of Kit skill contributions to
	// inject into each dispatched agent's system prompt and tool surface.
	// Populated by the daemon at runner construction time from the active
	// Kits in the KitRegistry (via internal/kit.LoadSkills). When nil or
	// empty no Kit skill injection occurs — the runner behaves as before
	// this feature was added (additive/cardinal rule 1 compliant).
	//
	// DEPRECATED path: this field pre-computes skill sources at daemon CWD,
	// which races against kit detect on the real worktree. Prefer wiring
	// KitSkillDetector instead (see below) which resolves against the cloned
	// worktree AFTER clone (step 2b). Both fields are supported: if
	// KitSkillDetector is set it takes precedence over KitSkillSources.
	//
	// Each element describes one active Kit's skill file list (paths
	// relative to the Kit's manifest directory) and priority for
	// ordering the merged system-prompt append block.
	KitSkillSources []kit.KitSkillSource

	// KitSkillDetector resolves kit skill sources against the cloned worktree
	// path (post-clone, step 2b) so detection runs against the REAL repo
	// contents rather than the daemon's CWD. Implemented by
	// KitRegistry.SkillSourcesForRepo. When non-nil it replaces KitSkillSources
	// for the lifetime of a single Run (the daemon-CWD pre-compute is stale
	// once the repo is cloned to wpath).
	//
	// Additive/cardinal rule 1: nil keeps KitSkillSources behaviour intact.
	KitSkillDetector KitSkillDetector

	// KitPromptFragmentDetector resolves kit prompt-fragment sources against
	// the cloned worktree path (post-clone, step 2b). Implemented by
	// KitRegistry.PromptFragmentSourcesForRepo. When non-nil the runner injects
	// workType-filtered fragment bodies into the system prompt at step 5a
	// alongside skill bodies. nil = no fragment injection (additive).
	KitPromptFragmentDetector KitPromptFragmentDetector

	// KitDetector resolves the kit manifests that apply to a provisioned
	// worktree, ordered foundation → framework → project. The daemon
	// wires this to KitRegistry.DetectForRepo over its active kits; nil
	// disables kit toolchain provisioning entirely (the runner behaves
	// exactly as before K1). Computed inside the runner because the runner
	// owns the worktree path (K1.4); mirrors how KitSkillSources is
	// daemon-populated.
	KitDetector KitDetector

	// KitComposer performs target-aware detection plus owner-qualified command
	// composition. When set it takes precedence over KitDetector so the exact
	// work type/path scope and operator lock participate in preflight.
	KitComposer KitComposer

	// KitTargetOS overrides the OS the kit toolchain demand is composed
	// for. Empty falls back to the host OS (kit.MustResolveOS). The daemon
	// sets this to the SANDBOX OS ("linux") for cloud-targeted sessions so
	// install scripts match the sandbox, not the dispatching host (OD-2).
	KitTargetOS string
}

// KitDetector resolves the ordered kit manifests that apply to a worktree
// at repoRoot for targetOS (foundation → framework → project). Returns an
// empty slice when no kit applies. Implemented by KitRegistry.DetectForRepo.
type KitDetector func(repoRoot, targetOS string) ([]kit.ManifestView, error)

// KitComposer resolves target-aware kit lifecycle and command demand.
type KitComposer func(repoRoot string, target kit.CompositionTarget, selected []kit.Selection) (*kit.ToolchainDemand, error)

// KitSkillDetector resolves kit skill sources against the cloned worktree
// path so skill injection uses the actual repo's detected kits. Implemented
// by KitRegistry.SkillSourcesForRepo.
type KitSkillDetector func(repoRoot, targetOS string) ([]kit.KitSkillSource, error)

// KitPromptFragmentDetector resolves kit prompt-fragment sources against
// the cloned worktree path for workType-filtered injection. Implemented
// by KitRegistry.PromptFragmentSourcesForRepo.
type KitPromptFragmentDetector func(repoRoot, targetOS string) ([]kit.KitPromptFragmentSource, error)

// Runner is the long-lived per-daemon orchestrator. Build one via
// [New] at daemon startup and call [Runner.Run] for every claimed
// QueuedWork.
//
// Runner is safe for concurrent use across sessions: every method
// holds only per-Run state via locals; collaborators (Registry,
// WorktreeManager, etc.) are documented as concurrency-safe by their
// own packages.
type Runner struct {
	registry              *Registry
	wt                    *worktree.Manager
	poster                *result.Poster
	credentialProvider    CredentialProvider
	envc                  *env.Composer
	mcpb                  *mcp.Builder
	store                 *state.Store
	promptBuilder         *prompt.Builder
	httpClient            *http.Client
	logger                *slog.Logger
	now                   func() time.Time
	maxDuration           time.Duration
	idleTimeout           time.Duration
	preserveOnFail        bool
	preserveAlways        bool
	skipBackstop          bool
	skipSteering          bool
	skipPostSession       bool
	hbInterval            time.Duration
	spanEmissionEnabled   bool
	spanEndpointPath      string
	kitSkillSources       []kit.KitSkillSource
	kitSkillDetector      KitSkillDetector
	kitPromptFragDetector KitPromptFragmentDetector
	kitDetector           KitDetector
	kitComposer           KitComposer
	kitTargetOS           string
}

// RuntimeCredentials are the bearer-token credentials needed for session
// heartbeats and status posts.
type RuntimeCredentials struct {
	WorkerID  string
	AuthToken string
}

// CredentialProvider returns the freshest worker runtime credentials available
// to the caller. Implementations should be cheap and concurrency-safe.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

// New constructs a Runner from opts. Returns an error when any
// required collaborator is missing.
func New(opts Options) (*Runner, error) {
	if opts.Registry == nil {
		return nil, errors.New("runner: Registry is required")
	}
	if opts.WorktreeManager == nil {
		return nil, errors.New("runner: WorktreeManager is required")
	}
	if opts.Poster == nil {
		return nil, errors.New("runner: Poster is required")
	}
	r := &Runner{
		registry:              opts.Registry,
		wt:                    opts.WorktreeManager,
		poster:                opts.Poster,
		credentialProvider:    opts.CredentialProvider,
		envc:                  opts.EnvComposer,
		mcpb:                  opts.MCPBuilder,
		store:                 opts.StateStore,
		promptBuilder:         opts.PromptBuilder,
		httpClient:            opts.HTTPClient,
		logger:                opts.Logger,
		now:                   opts.Now,
		maxDuration:           opts.MaxSessionDuration,
		idleTimeout:           opts.IdleTimeout,
		preserveOnFail:        opts.PreserveWorktreeOnFailure,
		preserveAlways:        opts.PreserveWorktreeAlways,
		skipBackstop:          opts.SkipBackstop,
		skipSteering:          opts.SkipSteering,
		skipPostSession:       opts.SkipPostSession,
		hbInterval:            opts.HeartbeatInterval,
		spanEmissionEnabled:   opts.SpanEmissionEnabled,
		spanEndpointPath:      opts.SpanEndpointPath,
		kitSkillSources:       opts.KitSkillSources,
		kitSkillDetector:      opts.KitSkillDetector,
		kitPromptFragDetector: opts.KitPromptFragmentDetector,
		kitDetector:           opts.KitDetector,
		kitComposer:           opts.KitComposer,
		kitTargetOS:           opts.KitTargetOS,
	}
	if r.envc == nil {
		r.envc = env.NewComposer()
	}
	if r.mcpb == nil {
		r.mcpb = mcp.NewBuilder()
	}
	if r.store == nil {
		r.store = state.NewStore()
	}
	if r.promptBuilder == nil {
		r.promptBuilder = &prompt.Builder{}
	}
	if r.httpClient == nil {
		r.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.maxDuration == 0 {
		r.maxDuration = DefaultMaxSessionDuration
	}
	if r.idleTimeout == 0 {
		r.idleTimeout = DefaultIdleTimeout
	}
	return r, nil
}

// terminalResultPostContext preserves parent values while detaching terminal
// result delivery from the run's cancellation/deadline. The replacement
// deadline keeps cleanup bounded even when the platform is unavailable.
func terminalResultPostContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), terminalResultPostTimeout)
}

// Run orchestrates one session end-to-end. It does not return until
// the session has reached a terminal state (success, failure, or
// cancellation) and the result has been posted.
//
// The returned Result is always non-nil — on a fatal error the
// Result.Status is "failed" and Result.FailureMode classifies the
// reason. Callers should log err and inspect Result for the
// platform-relevant fields (PR URL, cost, etc).
//
// Cancellation: ctx is honored at every step. A cancelled ctx
// short-circuits the loop with FailureMode "timeout"; the runner
// still attempts result.Post + teardown so platform state stays
// consistent.
func (r *Runner) Run(ctx context.Context, qw QueuedWork) (*Result, error) {
	startedAt := r.now().UnixMilli()

	// Apply the runner-side upper-bound timeout if requested.
	runCtx := ctx
	var runCancel context.CancelFunc
	if r.maxDuration > 0 {
		runCtx, runCancel = context.WithTimeout(ctx, r.maxDuration)
		defer runCancel()
	}

	// Validate the QueuedWork early so callers get a clear error.
	if err := validateQueuedWork(qw); err != nil {
		res := &Result{
			SessionID:       qw.SessionID,
			IssueIdentifier: qw.IssueIdentifier,
			StartedAt:       startedAt,
			FinishedAt:      r.now().UnixMilli(),
		}
		res.Status = "failed"
		res.FailureMode = FailurePromptRender
		res.Error = err.Error()
		// Best-effort post; ignore errors on the failure path. Validation may
		// observe an already-cancelled caller, so terminal delivery gets the same
		// detached, bounded cleanup context as the normal post-run path.
		postCtx, postCancel := terminalResultPostContext(runCtx)
		_ = r.poster.Post(postCtx, qw.SessionID, res.Result)
		postCancel()
		return res, fmt.Errorf("runner: invalid QueuedWork: %w", err)
	}

	// Drive the loop. loop.go owns the step sequence; the helpers
	// here own the result envelope + post-Run teardown.
	res, runErr := r.runLoop(runCtx, qw, startedAt)

	// Post the terminal Result. We do this before teardown so a
	// teardown error does not lose the platform-side update. The run context is
	// often the reason the session ended, so detach cancellation while preserving
	// its values and apply a fresh cleanup bound.
	postCtx, postCancel := terminalResultPostContext(runCtx)
	postErr := r.poster.Post(postCtx, qw.SessionID, res.Result)
	postCancel()
	if postErr != nil {
		// Log + record the post failure on the result. The Run
		// itself does not fail because of a post failure — the
		// platform has its own retry/poller for stale sessions.
		r.logger.Warn("result post failed",
			"sessionId", qw.SessionID,
			"err", postErr,
		)
		if res.Error == "" {
			res.Error = fmt.Sprintf("result post failed: %v", postErr)
		}
	}

	// Teardown worktree (unless preserving).
	if shouldTeardown(res, r.preserveOnFail, r.preserveAlways) {
		if err := r.wt.Teardown(context.Background(), qw.SessionID); err != nil {
			r.logger.Warn("worktree teardown failed",
				"sessionId", qw.SessionID,
				"err", err,
			)
		}
	}

	res.FinishedAt = r.now().UnixMilli()
	return res, runErr
}

// validateQueuedWork checks the minimum field set required for Run.
// Returns nil when the work is dispatchable.
func validateQueuedWork(qw QueuedWork) error {
	switch {
	case qw.SessionID == "":
		return errors.New("SessionID is required")
	case qw.IssueIdentifier == "" && qw.PromptContext == "" && qw.Body == "":
		return errors.New("issue context required (PromptContext, Body, or IssueIdentifier)")
	case qw.PlatformURL == "":
		return errors.New("PlatformURL is required (for heartbeat refresh)")
	case qw.WorkerID == "":
		return errors.New("WorkerID is required")
	}
	return nil
}

// shouldTeardown decides whether the worktree should be removed after
// Run returns. PreserveAlways short-circuits to keep the worktree;
// otherwise successful runs are torn down and failed runs respect
// preserveOnFail.
func shouldTeardown(res *Result, preserveOnFail, preserveAlways bool) bool {
	if preserveAlways {
		return false
	}
	if res == nil || res.Status == "completed" {
		return true
	}
	if preserveOnFail {
		return false
	}
	return true
}

// hostEnv returns os.Environ() parsed into a key→value map. Used by
// runLoop to seed the env composer with the daemon's host env.
func hostEnv() map[string]string {
	out := make(map[string]string, 64)
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}
