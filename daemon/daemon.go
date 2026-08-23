package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/RenseiAI/donmai/gateway/costfeed"
	internaldaemon "github.com/RenseiAI/donmai/internal/daemon"
	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/rulesetsnapshot"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// LandingWorkType is the poll WorkType wire value for a landing-run trigger:
// a poll item that asks the worker to land a queued proposal (the concurrent
// landing serializer in landing/) rather than to spawn an agent session.
//
// A landing-run item is routed out-of-band by Options.OnLandingWork when that
// hook is wired; with no hook set, no producer emits this WorkType today, so
// the value is inert.
const LandingWorkType = "landing-run"

// Options configure a Daemon.
type Options struct {
	// ConfigPath is where to load / persist daemon.yaml. Defaults to
	// DefaultConfigPath().
	ConfigPath string
	// JWTPath is where to cache the runtime JWT. Defaults to
	// DefaultJWTPath().
	JWTPath string
	// SkipWizard, when true, prevents the interactive wizard from running
	// even when stdin is a TTY. The default config (or existing config) is
	// used instead.
	SkipWizard bool
	// SkipRegistration, when true, skips the registration call (used when
	// the daemon is being started in setup-only or config-only modes).
	SkipRegistration bool
	// SpawnerOptions overrides the default spawner options. The Projects
	// and MaxConcurrentSessions fields are populated automatically from
	// loaded config.
	SpawnerOptions SpawnerOptions
	// HTTPHost overrides the default control server bind address.
	HTTPHost string
	// HTTPPort overrides the default control server port.
	//
	// Zero means "ephemeral port": the listener binds 127.0.0.1:0 and
	// the kernel picks a free port. The effective bound port is then
	// available via Server.Addr() after Server.Start succeeds.
	// Production callers (afcli/daemon_run.go) substitute the
	// well-known DefaultHTTPPort (7734) themselves before constructing
	// Options so operator behaviour is preserved; the daemon library
	// itself does NOT auto-fill — leaving zero-as-ephemeral makes
	// parallel tests collision-free under -race.
	HTTPPort int
	// PoolStatsProvider returns the current workarea pool snapshot. May be
	// nil — the /api/daemon/pool/stats endpoint will return an empty
	// snapshot in that case (acceptance criterion: pool integration is
	// optional in the runtime port; full WorkareaProvider wiring is future work).
	PoolStatsProvider PoolStatsProvider
	// EvictHandler handles pool eviction requests. May be nil; the endpoint
	// returns 501 in that case.
	EvictHandler EvictHandler
	// ProviderRegistry exposes the daemon's locally-registered AgentRuntime
	// providers (claude/codex/ollama/opencode/gemini/amp/stub) to the
	// /api/daemon/providers* surface. May be nil — the endpoint will then
	// return an empty list with PartialCoverage=true, which is the correct
	// behaviour for a daemon that has not yet wired its runtime registry.
	// Wave 9 / ADR-2026-05-07-daemon-http-control-api.md §D4.
	ProviderRegistry ProviderRegistry
	// ExecutionPreflightStore durably records the initial ready receipt before
	// credential hooks or worker spawn. Receipt-bearing work fails closed when
	// either the compiler or this store is absent.
	ExecutionPreflightStore ExecutionPreflightStore

	// RulesetSnapshot, when non-nil, wires the daemon to a configured
	// ruleset-snapshot source: a signed, versioned bundle the daemon
	// polls, verifies (Ed25519 + content hash), and caches to disk,
	// consulted by the claim path when a live ClaimGateProvider is unset or
	// fails — fail-static within the client's configured TTL, a loud typed
	// refusal past it (see FailStaticClaimGateProvider). Nil (the default)
	// keeps every existing single-machine/self-hosted deployment
	// byte-identical: no snapshot source, no behaviour change. An
	// embedding platform constructs a *rulesetsnapshot.Client with its OWN
	// Endpoint URL / trusted key(s) / JWKS and injects it here — this
	// package never hardcodes or guesses a platform URL. The embedder also
	// owns keeping it warm (rulesetsnapshot.Client.Start or its own
	// periodic Refresh); the daemon only ever reads Current().
	RulesetSnapshot *rulesetsnapshot.Client

	// SessionShim configures per-session shim ownership and daemon adoption
	// (ADR-2026-08-17). The zero value keeps adoption DISABLED, which is §D11
	// step 1 of the ADR's own migration law — a daemon that never sets this
	// behaves exactly as it did before shim ownership existed.
	SessionShim SessionShimConfig

	// EnableGateway starts the translating-gateway loopback host (the
	// ModelEndpoint host kind "gateway", ADR-2026-07-24 / 08) alongside the
	// daemon. Off by default: the gateway is experimental at M1 and is only
	// reached once a resolver routes a gateway cell to it, so a pure-OSS daemon
	// that never sets this is byte-identical to today. When off,
	// GET /api/daemon/gateway reports enabled:false.
	EnableGateway bool

	// Version overrides the package-level `Version` for status reporting.
	// Empty falls back to the package var (which itself defaults to "dev"
	// unless the build injected via -ldflags). Downstream embedders that
	// ship their own binary (e.g. the rensei daemon) should set this to
	// their own version string so /api/daemon/status reports the
	// running binary, not whatever string donmai's vendored
	// source had at the time.
	Version string

	// WorkerCapabilitiesFunc returns the worker capability flags the daemon
	// advertises on each claimed session's SessionDetail (e.g.
	// "deterministic-landing"). It is consulted once per claimed poll item in
	// the primary poll loop and the result is threaded via
	// WithWorkerCapabilities so the runner can gate adapter-dependent
	// behaviour.
	//
	// Nil ⇒ no capabilities are advertised: the SessionDetail is built with
	// no capability option, which is byte-identical to today's behaviour
	// (Capabilities stays nil, every flag reads false). This keeps mixed
	// binary versions safe — an older embedder that never sets this is
	// unaffected. Embedders that want to advertise capabilities supply a
	// func that returns a non-empty map.
	WorkerCapabilitiesFunc func() map[string]bool

	// RegistrationCapabilities is the flat capability-tag list advertised at
	// REGISTRATION time (distinct from WorkerCapabilitiesFunc, which is the
	// typed per-session map). The coordinator persists it and gates its
	// capability-keyed claim lanes against it (kg-extraction, the landing
	// lane's "merge-queue").
	//
	// Nil ⇒ the base substrate set ({local,sandbox,workarea}). Embedders that
	// want the worker to receive another capability-gated lane supply the full
	// tag list here (e.g. a downstream embedder appends "merge-queue").
	//
	// Either way the tags for the lanes the poll service itself executes are
	// appended on top (see effectiveRegistrationCapabilities) — an embedder can
	// neither forget them nor advertise them without the executor, because the
	// executor is wired by the poll service, not by the embedder.
	RegistrationCapabilities []string

	// OnLandingWork handles a landing-run poll item (WorkType ==
	// LandingWorkType) out-of-band from the session-spawn path. The whole
	// PollWorkItem is passed so the handler can route by tenant
	// (OrganizationID, Repository) and read any trigger context it needs
	// without a separate fetch.
	//
	// When set, a landing-run item is dispatched to this hook and never
	// becomes a session or counts toward the concurrency quota; the handler's
	// error (if any) is logged best-effort and the poll item is reported
	// handled.
	//
	// Nil ⇒ landing-run items fall through to the normal session-spawn path
	// unchanged. No producer emits LandingWorkType today, so a nil hook leaves
	// the poll path byte-identical to current behaviour.
	OnLandingWork func(ctx context.Context, item PollWorkItem) error

	// GitAuth is the per-invocation git auth resolver an embedder supplies to
	// drive the credential-hardening seam. Given the repo URL a git invocation
	// is about to touch, it returns the HTTP authorization header to inject
	// (e.g. "Authorization: Bearer <token>" or "AUTHORIZATION: basic <base64>")
	// and whether the OS credential helper must be suppressed.
	//
	// Suppressing the credential helper resets git's credential.helper list to
	// empty so no keychain get/store runs — this is what avoids the blocking
	// GUI popup ("A keychain cannot be found to store …") that hangs a daemon
	// running under launchd with no logged-in keychain session. Injecting the
	// header lets auth travel per invocation instead of being baked into the
	// persisted remote URL, so the token never lands in .git/config.
	//
	// IMPORTANT — this field is a declarative contract surface, NOT an
	// auto-thread. The OSS daemon does not itself construct the per-session
	// worktree Manager: it spawns `donmai agent run` as a subprocess, and that
	// subprocess builds the runner + worktree.Manager (see afcli/agent_run.go).
	// Because the Manager lives in another process, the daemon cannot wire this
	// callback into it directly, and setting daemon.Options.GitAuth here has no
	// in-process effect on its own.
	//
	// The seam that actually applies hardening is runtime/worktree.Options.GitAuth
	// (applied via internal/gitexec.HardenedEnv): when set on the Manager, each
	// git invocation runs with the hardened env, and a clone of a URL carrying
	// embedded userinfo clones the userinfo-stripped URL and relies on the
	// injected http.extraHeader for auth. An embedder that wants hardening must
	// thread its resolver into the worktree Manager it constructs (or into the
	// spawn path it owns) — this daemon field exists so the embedder can carry
	// that resolver alongside the rest of its Options without restating the type.
	//
	// The OSS binary leaves this nil, the worktree seam stays inert, and
	// standalone behaviour is byte-identical to before.
	GitAuth GitAuth
}

// GitAuth is the per-invocation git auth resolver an embedder supplies via
// Options.GitAuth. See that field's documentation for the full contract. It
// mirrors runtime/worktree.GitAuth — the type is restated here so the daemon's
// public surface does not force a runtime/worktree import on consumers that
// only construct Options.
type GitAuth func(ctx context.Context, repoURL string) (authHeader string, suppressHelper bool, err error)

// PoolStatsProvider returns a workarea pool snapshot.
type PoolStatsProvider interface {
	Stats(ctx context.Context) (*afclient.WorkareaPoolStats, error)
}

// EvictHandler executes a pool eviction request and returns the response.
type EvictHandler interface {
	Evict(ctx context.Context, req afclient.EvictPoolRequest) (*afclient.EvictPoolResponse, error)
}

// ProviderRegistry is the minimal read-only view of the runner's in-process
// AgentRuntime registry the /api/daemon/providers handler consumes. The
// daemon imports a satisfying type from runner.Registry — the interface
// keeps this package free of a runner import cycle. (Wave 9 / A1.)
type ProviderRegistry interface {
	// Names returns the sorted list of registered provider name strings.
	// Each name is the canonical agent.ProviderName string (e.g. "claude",
	// "codex"). Order is stable across calls.
	Names() []string
	// Capabilities returns the typed capability struct serialised to a
	// flat map[string]any for the named provider. ok is false when the
	// provider is not registered. The map shape matches the JSON encoding
	// of agent.Capabilities so the wire shape on /api/daemon/providers
	// matches the contract.
	Capabilities(name string) (caps map[string]any, ok bool)
}

// WorkareaCapabilityProvider is the optional exact-executor registration
// surface. Registries that do not implement it advertise no session-root-v1
// capability and remain valid for singular legacy work.
type WorkareaCapabilityProvider interface {
	WorkareaExecutorCapabilities() []workarea.ExecutorCapabilityAttestation
}

// ExecutionPreflightProvider is optionally implemented by ProviderRegistry.
// The raw SessionDetail JSON lets the runner adapter consume the same immutable
// operational bytes without introducing a daemon -> runner dependency.
type ExecutionPreflightProvider interface {
	PreflightExecution(detailJSON json.RawMessage) (json.RawMessage, error)
}

// ClaimGateProvider is optionally implemented by ProviderRegistry. It lets the
// daemon re-run the execution-cell narrow-only claim gate
// (ADR-2026-08-05-versioned-execution-cell-and-session-reference.md D4)
// against THIS host's own local reality before accepting a claim-bound
// admission, instead of trusting an upstream-supplied ClaimReceipt at face
// value. cellJSON is the canonical bytes of the admitted, claim-bound
// ResolvedExecutionCell; the returned bytes must decode into an
// executioncell.ClaimLocalReality. The daemon — not the provider — runs
// executioncell.EvaluateClaim over the result, so the narrow-only invariant is
// enforced in exactly one place regardless of which provider is wired.
type ClaimGateProvider interface {
	ResolveClaimLocalReality(cellJSON json.RawMessage) (json.RawMessage, error)
}

// ExecutionPreflightStore durably persists immutable host adaptation receipts.
type ExecutionPreflightStore interface {
	Persist(sessionID string, receipt json.RawMessage) error
}

type lifecycleKind uint8

const (
	lifecycleStart lifecycleKind = iota + 1
	lifecycleRestartPrepare
	lifecycleUpdate
	lifecycleDrain
	lifecycleStop
	lifecyclePause
	lifecycleResume
)

// lifecycleLease identifies the one operation currently allowed to advance the
// daemon lifecycle. Its done channel lets later callers wait without holding
// lifecycleMu across any blocking work.
type lifecycleLease struct {
	id   uint64
	kind lifecycleKind
	done chan struct{}
}

// stopGeneration persists from the first Stop attempt until terminal
// completion. Incomplete attempt errors deliberately do not belong here: a
// later bounded Stop call may finish the same generation successfully.
type stopGeneration struct {
	id          uint64
	terminal    bool
	terminalErr error
}

// Daemon is the top-level supervisor. It owns the loaded Config, the
// HeartbeatService, the WorkerSpawner, and (optionally) the AutoUpdater.
type Daemon struct {
	opts Options

	mu        sync.RWMutex
	state     atomic.Value // State
	config    *Config
	workerID  string
	jwt       string
	startedAt time.Time
	// controllerID is resolved exactly once in New. It is immutable for this
	// process and independent of registration and credential refresh state.
	controllerIDValue string
	controllerIDErr   error
	// sessionShimAttestationValue is resolved, canonicalized, and frozen beside
	// controllerIDValue. Registration and every refresh reuse this exact tuple.
	sessionShimAttestationValue SessionShimHostAttestation
	sessionShimAttestationErr   error

	// capabilitySet holds the substrate capabilities detected at startup.
	// It is populated before registration so the provides[] array can be
	// sent to the platform. Exposed via GET /api/daemon/capabilities.
	// (ADR-2026-05-12-capacity-pools-and-substrate-resolution.md §H.)
	capabilitySet *internaldaemon.CapabilitySet

	heartbeat *HeartbeatService
	poller    *PollService
	spawner   *WorkerSpawner

	// shims is the daemon's live view of per-session shim ownership: which
	// shims it adopted at startup, which it quarantined, and the restart fence
	// it holds. Non-nil from New onward so every accessor is lock-safe without a
	// nil dance at each call site.
	shims *sessionShimState

	// gateway is the translating-gateway loopback host, started in Start when
	// Options.EnableGateway is set and torn down in Stop. Nil when disabled.
	// gatewayLedger is the cost-ledger path reported by /api/daemon/gateway.
	gateway       *gateway.Gateway
	gatewayLedger string

	// tokenRefresher proactively re-mints the runtime JWT before expiry
	// so the reactive 401 paths in heartbeat/poll stay quiet backstops.
	// Nil for stub registrations and when registration is skipped.
	tokenRefresher *tokenRefresher

	// lastHostStatus stores the most recent hostStatus the platform sent
	// in a heartbeat response. The pool-deleted / pool-disabled signals
	// surface here so af daemon stats can show "your pool was deleted —
	// re-register against pool X" without parsing daemon.log. Phase 2e.
	lastHostStatus *HostStatusDetail

	// yamlWatcherStop is the cancel function for the fsnotify goroutine
	// that hot-reloads daemon.yaml on direct edits. Phase 3b — nil when
	// either ConfigPath is empty or the watcher failed to start.
	yamlWatcherStop func()

	// sessionDetails stores the per-session payload the spawner
	// hands out to `donmai agent run` workers via the local control
	// HTTP API at /api/daemon/sessions/<id>.
	sessionDetails *sessionDetailStore

	// routingTraces is the in-process record of cross-provider
	// scheduler decisions. The /api/daemon/routing/* surface reads
	// this; future wave wires the scheduler's RecordDecision hook
	// into it. (Wave 9 / A4 — ADR-2026-05-07-daemon-http-control-api.md
	// §D4.)
	routingTraces *RoutingTraceStore

	// workareaArchive is the on-disk archive registry powering the
	// /api/daemon/workareas* surface. Lazily constructed on first
	// access; tests inject directly via SetWorkareaArchiveRegistry.
	// Wave 9 / Track A3.
	workareaArchive *WorkareaArchiveRegistry

	// lifecycleMu is a metadata-only ownership registry. It must never be held
	// across registration, draining, polling, callbacks, or update work: callers
	// that encounter an active generation wait on its lease with their own context.
	lifecycleMu     sync.Mutex
	lifecycleOwner  *lifecycleLease
	nextLifecycleID uint64
	stopGen         *stopGeneration
	doneCh          chan struct{}
	doneOnce        sync.Once

	// stopAttemptBeforeRelease is a package-private test seam. Production leaves
	// it nil; tests use it to prove a completed incomplete attempt cannot race a
	// later Stop attempt's terminal publication.
	stopAttemptBeforeRelease func(error)
	// runPreparedUpdate is a package-private test seam for the HTTP handoff from
	// synchronous restart preparation into asynchronous update work.
	runPreparedUpdate func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error)

	// Landing callbacks are independent of worker spawning, so they have their
	// own admission, cancellation, and completion ownership. landingDone is the
	// active callback generation's single completion signal; it starts closed and
	// is replaced only on a 0 -> 1 admission transition.
	landingMu       sync.Mutex
	landingCtx      context.Context
	landingCancel   context.CancelFunc
	landingStopping bool
	landingActive   int
	landingDone     chan struct{}
}

// New constructs a Daemon. Call Start() to bring it online.
func New(opts Options) *Daemon {
	if opts.ConfigPath == "" {
		opts.ConfigPath = DefaultConfigPath()
	}
	if opts.JWTPath == "" {
		opts.JWTPath = DefaultJWTPath()
	}
	if opts.HTTPHost == "" {
		opts.HTTPHost = DefaultHTTPHost
	}
	if opts.ExecutionPreflightStore == nil {
		opts.ExecutionPreflightStore = NewFileExecutionPreflightStore(
			statepath.Resolve("adaptation-receipts", "/tmp/.donmai/adaptation-receipts"),
		)
	}
	// Note: HTTPPort=0 is intentionally NOT auto-filled to
	// DefaultHTTPPort here — callers that want the well-known 7734
	// port (the cobra `donmai daemon run` entry point) substitute it
	// themselves before constructing Options. Leaving zero-as-
	// ephemeral here lets parallel tests bind 127.0.0.1:0 and have
	// the kernel pick free ports, eliminating the port-7734 bind
	// flake observed under -race when many tests share the default.
	landingCtx, landingCancel := context.WithCancel(context.Background())
	landingDone := make(chan struct{})
	close(landingDone)
	d := &Daemon{
		opts:           opts,
		doneCh:         make(chan struct{}),
		landingCtx:     landingCtx,
		landingCancel:  landingCancel,
		landingDone:    landingDone,
		sessionDetails: newSessionDetailStore(),
		routingTraces:  NewRoutingTraceStore(DefaultRoutingRingBufferSize),
		shims:          newSessionShimState(),
	}
	d.controllerIDValue, d.controllerIDErr = resolveControllerID(opts.SessionShim)
	d.sessionShimAttestationValue, d.sessionShimAttestationErr = resolveSessionShimHostAttestation(
		opts.SessionShim,
		d.controllerIDValue,
	)
	if opts.RulesetSnapshot != nil {
		snapshotClient := opts.RulesetSnapshot
		d.routingTraces.SetSnapshotStatusFunc(func() (afclient.RulesetSnapshotStatus, bool) {
			return rulesetSnapshotWireStatus(snapshotClient)
		})
	}
	d.state.Store(StateStopped)
	return d
}

// RoutingTraces returns the daemon's in-process routing trace store.
// The eventual cross-provider scheduler records its decisions here via
// store.RecordDecision; the /api/daemon/routing/* HTTP surface reads
// from it. Exposed so test harnesses (and a future scheduler wire-up)
// can drive recordings without reaching through internal fields.
// (Wave 9 / A4.)
func (d *Daemon) RoutingTraces() *RoutingTraceStore {
	return d.routingTraces
}

// State returns the current lifecycle state.
func (d *Daemon) State() State {
	v, _ := d.state.Load().(State)
	return v
}

// EffectiveVersion returns the version string the daemon should report
// in HTTP status / heartbeat / registration payloads. Resolution order:
// `Options.Version` (downstream embedder override) → package `Version`
// (which itself is "dev" unless overridden via `-ldflags
// -X .../daemon.Version=…`). Empty option = fall through.
func (d *Daemon) EffectiveVersion() string {
	if d.opts.Version != "" {
		return d.opts.Version
	}
	return Version
}

func (d *Daemon) setState(s State) {
	d.state.Store(s)
}

// claimLifecycle waits for the current lifecycle owner to release its lease,
// then installs a lease for kind. Context limits only waiting for an active
// owner: when no owner exists, even a canceled Stop may claim the slot and run
// its harmless zero-wait completion barriers.
func (d *Daemon) claimLifecycle(ctx context.Context, kind lifecycleKind) (*lifecycleLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		d.lifecycleMu.Lock()
		if d.lifecycleOwner == nil {
			d.nextLifecycleID++
			lease := &lifecycleLease{
				id:   d.nextLifecycleID,
				kind: kind,
				done: make(chan struct{}),
			}
			d.lifecycleOwner = lease
			d.lifecycleMu.Unlock()
			return lease, nil
		}
		done := d.lifecycleOwner.done
		d.lifecycleMu.Unlock()

		select {
		case <-done:
			// The owner released its exact lease. Retry so installation remains
			// serialized with every other contender.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// tryClaimLifecycle installs a lease only when no lifecycle operation is
// active. Pause and Resume deliberately use this nonblocking form because they
// have no caller context with which to bound a wait.
func (d *Daemon) tryClaimLifecycle(kind lifecycleKind) (*lifecycleLease, bool) {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.lifecycleOwner != nil {
		return nil, false
	}
	d.nextLifecycleID++
	lease := &lifecycleLease{
		id:   d.nextLifecycleID,
		kind: kind,
		done: make(chan struct{}),
	}
	d.lifecycleOwner = lease
	return lease, true
}

// releaseLifecycle clears and signals only the exact installed lease. It is
// safe for a deferred cleanup to run more than once and never wakes waiters for
// a newer lifecycle generation.
func (d *Daemon) releaseLifecycle(lease *lifecycleLease) {
	if lease == nil {
		return
	}
	d.lifecycleMu.Lock()
	if d.lifecycleOwner == lease {
		d.lifecycleOwner = nil
		close(lease.done)
	}
	d.lifecycleMu.Unlock()
}

func (d *Daemon) ownsLifecycleLocked(lease *lifecycleLease) bool {
	return d.lifecycleOwner == lease
}

// workerCapabilitiesFunc returns the configured capability-flag provider, or
// nil when none was supplied. Accessor so the poll closure (and tests) read
// the hook through one seam.
func (d *Daemon) workerCapabilitiesFunc() func() map[string]bool {
	return d.opts.WorkerCapabilitiesFunc
}

// onLandingWork returns the configured landing-run handler, or nil when none
// was supplied. Accessor so the poll closure (and tests) read the hook through
// one seam.
func (d *Daemon) onLandingWork() func(ctx context.Context, item PollWorkItem) error {
	return d.opts.OnLandingWork
}

// Config returns a copy of the loaded config (or nil if not started).
func (d *Daemon) Config() *Config {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.config == nil {
		return nil
	}
	c := *d.config
	return &c
}

// WorkerID returns the assigned worker ID (empty until registered).
func (d *Daemon) WorkerID() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.workerID
}

// RuntimeCredentials returns the daemon's current worker identity and ephemeral
// runtime token as one lock-consistent snapshot. Callers must use the token only
// for immediate authorization and must not persist it.
func (d *Daemon) RuntimeCredentials() (workerID, runtimeToken string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.workerID, d.jwt
}

// HostStatus returns the most recent hostStatus reported by the platform
// in a heartbeat response. nil until at least one beat has been ACK'd
// (or the platform predates Phase 2e). Phase 2e of
// 2026-05-18-daemon-config-sync-DESIGN.md.
//
// af daemon stats can surface this so an operator sees "your pool was
// deleted — re-register against pool X" without parsing daemon.log.
func (d *Daemon) HostStatus() *HostStatusDetail {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.lastHostStatus == nil {
		return nil
	}
	cp := *d.lastHostStatus
	return &cp
}

// claimSuspended is the PollOptions.ClaimSuspended callback: it translates the
// most recent host status the control plane reported into the poll loop's
// claim gate. True means "do not claim new work this tick"; the reason is used
// once, on the suspend transition, in the poll loop's log line.
//
// Absent status (no beat has carried one yet, or the server never sends the
// field) reads as NOT suspended — see HostStatusDetail.SuspendsClaiming.
//
// Locking: HostStatus() takes and releases d.mu (read side) and returns a copy,
// so nothing is held while the poll loop latches its own state, and the
// heartbeat goroutine's setLastHostStatus (write side) can always make
// progress. The two never nest.
func (d *Daemon) claimSuspended() (bool, string) {
	if d.sessionShimAttestationValue.enabled() &&
		(d.State() != StateRunning || !d.SessionShimAdoptionComplete() || !d.SessionShimCarrierActivationComplete()) {
		return true, "session-shim recovery is not ready"
	}
	status := d.HostStatus()
	if !status.SuspendsClaiming() {
		return false, ""
	}
	reason := "host status " + status.Status
	if status.RecommendedAction != "" {
		reason += ": " + status.RecommendedAction
	}
	return true, reason
}

// setLastHostStatus is the OnHostStatus callback wired into HeartbeatService.
// Called on every beat that carries a hostStatus payload (including
// status='ok', so we always reflect the platform's latest view).
func (d *Daemon) setLastHostStatus(detail HostStatusDetail) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := detail
	d.lastHostStatus = &cp
}

// runtimeJWT returns the cached runtime JWT (empty when registration
// was skipped). Internal helper for poll wiring.
func (d *Daemon) runtimeJWT() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.jwt
}

// ActiveSessions returns a snapshot of in-flight session handles.
//
// It is the union of two disjoint populations: sessions this daemon spawned as
// direct children, and per-session shims it launched, adopted, or quarantined
// (ADR-2026-08-17 §D7). Both are real work running on this machine. Listing only
// the direct children would make a freshly restarted daemon report an empty host
// while every pre-restart terminal is still live — the same lie the capacity
// path refuses to tell, told on the surface an operator actually reads.
func (d *Daemon) ActiveSessions() []SessionHandle {
	var out []SessionHandle
	if d.spawner != nil {
		out = d.spawner.ActiveSessions()
	}
	shims := d.sessionShimHandles()
	if len(shims) == 0 {
		return out
	}
	return append(out, shims...)
}

// Spawner returns the daemon's WorkerSpawner so callers can subscribe to
// session lifecycle events via WorkerSpawner.On without needing the spawner
// to be re-exposed through a higher-level hook. Returns nil before Start is
// called.
func (d *Daemon) Spawner() *WorkerSpawner {
	return d.spawner
}

// StopSession requests cooperative termination for one session id. A true
// result acknowledges an exact generation still owned by this daemon, including
// terminal or natural cleanup in progress; capacity is released asynchronously
// only after process-group reaping and synchronous Ended delivery. False means
// the spawner is uninitialised, the ID was never present, its generation was
// already released, or a bare id ambiguously names more than one organization-
// scoped shim identity. Ambiguity is a refusal and never falls through to a
// colliding direct child. Wired to the POST /api/daemon/sessions/<id>/stop
// control-API route for the deterministic per-session cancel path (Guard 3 hard
// out-of-band leg).
func (d *Daemon) StopSession(id string) bool {
	// A shim-owned session is reached by a generation-fenced Stop over shimwire,
	// never by signalling a process this daemon does not parent (§D4). Trying the
	// shim path first also means a stale spawner entry for the same id could not
	// win the race and terminate the wrong owner.
	switch d.stopSessionShimByID(id) {
	case sessionShimStopHandled:
		return true
	case sessionShimStopRefused:
		return false
	}
	if d.spawner == nil {
		return false
	}
	return d.spawner.StopSession(id)
}

// maxConcurrentSessions returns the current per-host capacity envelope under
// the read lock. Capacity can be mutated at runtime via the local control
// API (POST /api/daemon/capacity → handleSetCapacity), and the heartbeat
// loop reads it concurrently — without this lock the race detector flags
// the read in heartbeat.go vs the write in server.go.
func (d *Daemon) maxConcurrentSessions() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.config == nil {
		return 0
	}
	return d.config.Capacity.MaxConcurrentSessions
}

// Start brings the daemon online: load config (or wizard), register, start
// heartbeat, and start the spawner. The HTTP server is NOT started here;
// callers do that explicitly via Server.Start so they can pick the bind.
func (d *Daemon) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if d.controllerIDErr != nil {
		return d.controllerIDErr
	}
	if d.sessionShimAttestationErr != nil {
		return d.sessionShimAttestationErr
	}
	lease, err := d.claimLifecycle(ctx, lifecycleStart)
	if err != nil {
		return err
	}
	defer d.releaseLifecycle(lease)

	d.lifecycleMu.Lock()
	if d.stopGen != nil || d.State() != StateStopped {
		state := d.State()
		d.lifecycleMu.Unlock()
		return fmt.Errorf("cannot start — current state %q", state)
	}
	d.setState(StateStarting)
	d.lifecycleMu.Unlock()

	cfg, err := LoadConfig(d.opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil {
		// First run — wizard or default.
		cfg, err = RunSetupWizard(WizardOptions{
			ConfigPath: d.opts.ConfigPath,
			SkipWizard: d.opts.SkipWizard,
		})
		if err != nil {
			return fmt.Errorf("setup wizard: %w", err)
		}
	}

	d.mu.Lock()
	d.config = cfg
	d.startedAt = time.Now().UTC()
	d.mu.Unlock()

	// Detect substrate capabilities before registration so they can be
	// included in the provides[] array on POST /api/workers/register.
	// The result is cached for the worker lifetime and served via
	// GET /api/daemon/capabilities. (Stream H — pool awareness.)
	cs := internaldaemon.NewCapabilitySet()
	cs.Detect(internaldaemon.DefaultLookup)
	d.capabilitySet = cs
	slog.Info("daemon: detected substrate capabilities",
		"count", len(cs.Capabilities()))

	var (
		regResp *RegisterResponse
		regOpts RegistrationOptions
	)
	if !d.opts.SkipRegistration {
		token := cfg.Orchestrator.AuthToken
		if token == "" {
			token = os.Getenv("DONMAI_DAEMON_TOKEN")
		}
		if token == "" {
			token = "local-stub-no-token"
		}
		// Convert internal SubstrateCapability to wire ProvideCapability.
		detected := cs.Capabilities()
		provides := make([]ProvideCapability, len(detected))
		for i, c := range detected {
			provides[i] = ProvideCapability{Kind: string(c.Kind)}
		}
		regCaps := effectiveRegistrationCapabilities(d.opts.RegistrationCapabilities)
		var workareaExecutors []workarea.ExecutorCapabilityAttestation
		if provider, ok := d.opts.ProviderRegistry.(WorkareaCapabilityProvider); ok {
			workareaExecutors = provider.WorkareaExecutorCapabilities()
		}
		regOpts = RegistrationOptions{
			OrchestratorURL:   cfg.Orchestrator.URL,
			RegistrationToken: token,
			// MachineID is deliberately left empty so Register resolves the
			// STABLE machine identity (MachineID()). cfg.Machine.ID is a
			// hostname-derived label — keying host identity on it forked one
			// machine into one host per hostname form it had ever resolved
			// to. Operators who must pin identity set DONMAI_MACHINE_ID.
			Hostname:                cfg.Machine.ID,
			Version:                 d.EffectiveVersion(),
			MaxAgents:               cfg.Capacity.MaxConcurrentSessions,
			Capabilities:            regCaps,
			Region:                  cfg.Machine.Region,
			JWTPath:                 d.opts.JWTPath,
			Provides:                provides,
			DaemonProjects:          AllowlistEntriesFromConfig(cfg.EffectiveProjectConfigs()),
			ProjectIDs:              cfg.EffectiveEnabledProjectIDs(),
			ProjectAdmissionVersion: ProjectAdmissionVersionV2,
			ProjectAdmissionMode:    cfg.EffectiveProjectAdmissionMode(),
			// Item 8 (fleet host-info): gather best-effort machine telemetry
			// once at startup and thread it onto RegisterRequest.HostInfo so
			// the platform populates the worker_hosts host-info columns. All
			// fields degrade to empty on an unsupported platform; gathering
			// never crashes registration.
			HostInfo:            GatherHostInfo(d.EffectiveVersion(), d.StartedAt()),
			ValidateCredentials: d.validateControllerCredentials,
			SessionShim:         d.SessionShimHostAttestation(),
			WorkareaExecutors:   workareaExecutors,
			AuthOnly:            d.sessionShimAttestationValue.enabled(),
		}
	}

	register := func() error {
		if d.opts.SkipRegistration {
			return nil
		}
		var registerErr error
		regResp, registerErr = Register(ctx, regOpts)
		if registerErr != nil {
			return fmt.Errorf("register: %w", registerErr)
		}
		if d.sessionShimAttestationValue.enabled() {
			if receiptErr := d.acquireSessionShimRecoveryReceipts(ctx, regResp.SessionShim); receiptErr != nil {
				return receiptErr
			}
		}
		d.mu.Lock()
		d.workerID = regResp.WorkerID
		d.jwt = regResp.RuntimeToken
		d.mu.Unlock()
		return nil
	}

	// D12's hosted path is deliberately auth-only: acquire every scoped
	// credential/host/revision receipt first, while heartbeat, spawner, poll,
	// claim, and capacity publication do not yet exist. The zero-value legacy
	// path retains the established adopt-before-register order.
	if d.sessionShimAttestationValue.enabled() {
		if d.opts.SkipRegistration {
			return errors.New("session shim: attested recovery cannot skip registration")
		}
		d.setState(StateRecovering)
		if err := register(); err != nil {
			return err
		}
	}

	if err := d.adoptSessionShims(ctx); err != nil {
		return err
	}
	if !d.sessionShimAttestationValue.enabled() {
		if err := register(); err != nil {
			return err
		}
	}

	// Spawner — built before heartbeat/poll so the poll loop has a target for
	// AcceptWork dispatch on its very first tick.
	spawnerOpts := d.opts.SpawnerOptions
	spawnerOpts.Projects = cfg.EffectiveProjectConfigs()
	spawnerOpts.EnabledProjectIDs = cfg.EffectiveEnabledProjectIDs()
	spawnerOpts.ProjectAdmissionMode = cfg.EffectiveProjectAdmissionMode()
	spawnerOpts.MaxConcurrentSessions = cfg.Capacity.MaxConcurrentSessions
	if spawnerOpts.BaseEnv == nil {
		spawnerOpts.BaseEnv = map[string]string{}
	}
	if d.workerID != "" {
		spawnerOpts.BaseEnv["DONMAI_WORKER_ID"] = d.workerID
	}
	spawnerOpts.BaseEnv["DONMAI_ORCHESTRATOR_URL"] = cfg.Orchestrator.URL
	// Default WorktreeParentDir to the same statepath-resolved worktrees
	// directory the spawned `donmai agent run` worker uses when no
	// --worktree-dir override is passed (afcli/agent_run.go). Keeping the
	// two in sync lets the daemon publish each session's on-disk worktree
	// path on its SessionHandle (GET /api/daemon/sessions) so a local
	// reader (host-watch) can locate `.agent/events.jsonl` without a
	// per-session detail round-trip. Operators overriding the spawner can
	// pin a different value.
	if spawnerOpts.WorktreeParentDir == "" {
		spawnerOpts.WorktreeParentDir = statepath.Resolve("worktrees", "/tmp/.donmai/worktrees")
	}
	// Default WorkerCommand: spawn `donmai agent run` from the same
	// binary as the running daemon process so session lifecycle is
	// owned in-tree. Operators can override via SpawnerOptions.
	// (F.2.8 — daemon wire-up.)
	if len(spawnerOpts.WorkerCommand) == 0 {
		if cmd := defaultWorkerCommand(); cmd != nil {
			spawnerOpts.WorkerCommand = cmd
		}
	}
	// Default child stdout/stderr → slog so operators can see what the
	// spawned `donmai agent run` is doing without manually attaching a
	// debugger or rerunning under foreground. v0.5.0 had StdoutPrefixWriter
	// / StderrPrefixWriter nil by default, which the spawner translated to
	// drain-and-discard — leaving operators flying blind between
	// runner.Run() start and a `status=failed` post. Callers that already
	// supply their own writers via SpawnerOptions retain priority.
	if spawnerOpts.StdoutPrefixWriter == nil {
		spawnerOpts.StdoutPrefixWriter = newStdoutSlogWriter()
	}
	if spawnerOpts.StderrPrefixWriter == nil {
		spawnerOpts.StderrPrefixWriter = newStderrSlogWriter()
	}
	// §D1/§D11: select shim ownership before the credential-bearing pre-spawn
	// hook. Without this stable selector, a direct/default-off session first
	// acquires credentials for the shim offer and then acquires them again on the
	// direct fallback. The selector depends only on immutable daemon config and
	// the accepted spec; the launcher remains the fail-closed ownership move.
	// Embedders that supply their own launcher keep its original combined
	// decision semantics unless they also opt into ShimOwns explicitly.
	if spawnerOpts.ShimSpawn == nil {
		if spawnerOpts.ShimOwns == nil {
			spawnerOpts.ShimOwns = d.shimOwnsSession
		}
		spawnerOpts.ShimSpawn = d.launchSessionShim
	}
	// Admission must see shim-held slots too, or a restarted daemon would accept
	// against cores its surviving shims are already using.
	if spawnerOpts.ExternalOccupancy == nil {
		spawnerOpts.ExternalOccupancy = d.SessionShimOccupancy
	}
	d.spawner = NewWorkerSpawner(spawnerOpts)
	if d.sessionShimAttestationValue.enabled() {
		// D12: construction is not admission. Keep the spawner closed until the
		// first exact heartbeat response accepts the published recovery state.
		d.spawner.Pause()
	}
	// Cleanup the per-session detail store when sessions end so
	// stale auth tokens do not linger.
	d.spawner.On(func(ev SessionEvent) {
		if ev.Kind == SessionEventEnded && d.sessionDetails != nil {
			d.sessionDetails.Delete(ev.Spec.SessionID)
		}
	})
	// Record a routing decision for every session-start so the
	// /api/daemon/routing/explain/<sessionID> surface returns real
	// data for live sessions instead of always-404. The OSS daemon
	// ships a single sandbox provider (local), so the decision is
	// degenerate by construction; the recording exists primarily so
	// the operator surface is honest end-to-end. (Wave 11 / S6a;
	// ADR-2026-05-07-daemon-http-control-api.md §D4.)
	d.spawner.On(func(ev SessionEvent) {
		if ev.Kind != SessionEventStarted || d.routingTraces == nil {
			return
		}
		d.recordOSSRoutingDecision(ev.Spec.SessionID)
	})

	if regResp != nil {
		// Heartbeat, poll and the proactive refresher share ONE refresher, so a
		// refresh triggered anywhere re-mints the runtime credentials once and
		// every lane comes out of it on the same worker identity.
		//
		// Fan-out is the refresher's job, not this call site's: a lane left on a
		// superseded worker id is rejected on its next tick and re-registers,
		// which retires the registration the refresh just settled on. See
		// CredentialRefresher for what that cost when a call site had to
		// remember it by hand.
		credentials := NewCredentialRefresher(CredentialRefresherOptions{
			Registration: regOpts,
			WorkerID:     regResp.WorkerID,
			RuntimeJWT:   regResp.RuntimeToken,
			ValidateRefresh: func(result *RefreshTokenResult) error {
				if err := d.validateControllerCredentials(result.WorkerID, result.RuntimeToken); err != nil {
					return err
				}
				return d.validateAndRetainSessionShimRefreshReceipt(result)
			},
			OnRefreshed: func(result *RefreshTokenResult) {
				// Capture the identity being superseded under the same lock that
				// installs its replacement: it is the scope key for the session
				// re-stamp below, and reading it afterwards would race a
				// concurrent refresh into stamping the wrong generation.
				d.mu.Lock()
				prevWorkerID := d.workerID
				d.workerID = result.WorkerID
				d.jwt = result.RuntimeToken
				d.mu.Unlock()
				// Only the sessions claimed under THIS registration's identity.
				// A process serving several worker identities keeps each one's
				// children on their own bearer — see
				// sessionDetailStore.UpdateRuntimeCredentials for what an
				// unscoped sweep does to the identities this one does not own.
				if d.sessionDetails != nil {
					d.sessionDetails.UpdateRuntimeCredentials(prevWorkerID, result.WorkerID, result.RuntimeToken)
				}
			},
		})
		refreshCreds := credentials.Refresh
		reregister := credentials.OnReregister

		// Heartbeat. OnReregister handles reactive credential rejection (the
		// backstop behind the proactive refresher below): on a 401, or on a
		// 404 saying the orchestrator does not recognise this worker, we
		// re-mint via RefreshRuntimeToken — which re-presents the existing
		// registration wherever it still exists rather than replacing it.
		heartbeatOpts := HeartbeatOptions{
			WorkerID:        regResp.WorkerID,
			Hostname:        cfg.Machine.ID,
			OrchestratorURL: cfg.Orchestrator.URL,
			RuntimeJWT:      regResp.RuntimeToken,
			IntervalSeconds: regResp.HeartbeatIntervalSeconds(),
			GetActiveCount:  d.spawnerActiveCount,
			// Interactive-occupancy split: sample the unclassed total and the
			// interactive + legacy interview subset under one spawner lock so
			// lifecycle changes cannot serialize values from different instants.
			GetActiveSessionCounts: d.spawnerActiveSessionCounts,
			// §D7: quarantined per-session shims are visible capacity. They ride
			// every beat so a consumer can see occupied-but-unreachable load
			// instead of inferring an idle host from an unchanged session count.
			GetQuarantinedSessions: d.QuarantinedSessions,
			GetMaxCount:            func() int { return d.maxConcurrentSessions() },
			GetStatus:              d.RegistrationStatus,
			Region:                 cfg.Machine.Region,
			// Item 8: per-beat CPU/mem load sample → last_cpu_pct/last_mem_pct.
			// Best-effort stdlib probe; omits the load key when it can't sample.
			GetLoad:        SampleLoad,
			GetLoadAverage: SampleLoadAverage,
			OnReregister:   reregister,
			LogWarn: func(format string, args ...any) {
				slog.Warn(fmt.Sprintf(format, args...))
			},
			LogInfo: func(format string, args ...any) {
				slog.Info(fmt.Sprintf(format, args...))
			},
			GetProjectAdmission: func() ProjectAdmissionReport {
				// Read through the spawner, not the config snapshot: the
				// spawner is what the yaml watcher and the platform
				// mutations update, and it also carries satellite-org
				// identities registered via AddProjects/AddEnabledProjectIDs.
				// Reporting the LIVE set is what lets an admission edit reach
				// the platform on the next beat instead of the next restart.
				// d.spawner is set before the heartbeat is constructed.
				return ProjectAdmissionReport{
					Mode:              d.spawner.ProjectAdmissionMode(),
					EnabledProjectIDs: d.spawner.AllEnabledProjectIDs(),
					Entries:           AllowlistEntriesFromConfig(d.spawner.AllProjects()),
				}
			},
			// Phase 2c: handle platform-queued mutations.
			OnPendingMutations: d.applyPendingMutations,
			// Phase 2e: surface hostStatus signals (pool_deleted etc.)
			// to af daemon stats. The latest observed status is stored
			// in d.hostStatus; callers read via Daemon.HostStatus().
			OnHostStatus: d.setLastHostStatus,
		}
		if d.sessionShimAttestationValue.enabled() {
			primaryScope := d.sessionShimConfig().orgID()
			heartbeatOpts.GetSessionShim = func() (SessionShimHeartbeatProjection, error) {
				return d.SessionShimHeartbeatProjection(primaryScope)
			}
		}
		d.heartbeat = NewHeartbeatService(heartbeatOpts)
		credentials.Attach(d.heartbeat)
		if d.sessionShimAttestationValue.enabled() {
			if err := d.heartbeat.StartSynchronized(ctx); err != nil {
				return fmt.Errorf("session shim: first recovery heartbeat: %w", err)
			}
			d.spawner.Resume()
			d.lifecycleMu.Lock()
			if d.ownsLifecycleLocked(lease) && d.stopGen == nil {
				d.setState(StateRunning)
			}
			d.lifecycleMu.Unlock()
		} else {
			d.heartbeat.Start()
		}

		// Poll loop — the binding constraint that makes the daemon actually
		// receive work. Without this the platform's heartbeat-only sidecar
		// behaviour holds: the worker shows "active" but never picks up
		// queued sessions. (REN-v0.4.1.)
		//
		// Gated on real registration. Stub registrations have no platform poll
		// endpoint to call; starting the loop just floods logs with HTTP errors.
		if !strings.HasPrefix(regResp.RuntimeToken, "stub.") {
			interval := regResp.PollIntervalSeconds()
			if interval <= 0 {
				interval = 5
			}
			d.poller = NewPollService(PollOptions{
				WorkerID:        regResp.WorkerID,
				OrchestratorURL: cfg.Orchestrator.URL,
				RuntimeJWT:      regResp.RuntimeToken,
				IntervalSeconds: interval,
				// Stamps the non-agent lane executors' result/log context with
				// the running binary's version rather than the library default.
				WorkerVersion: d.EffectiveVersion(),
				LogWarn: func(format string, args ...any) {
					slog.Warn(fmt.Sprintf(format, args...))
				},
				LogInfo: func(format string, args ...any) {
					slog.Info(fmt.Sprintf(format, args...))
				},
				OnWork: func(item PollWorkItem) error {
					return d.handlePollWorkItem(item, cfg.Orchestrator.URL)
				},
				OnReregister: reregister,
				// Honour the host-status signal the heartbeat already
				// receives: when the bound capacity pool is deleted,
				// draining, or disabled, stop claiming new work while
				// leaving in-flight sessions alone.
				ClaimSuspended: d.claimSuspended,
			})
			credentials.Attach(d.poller)
			d.poller.Start()

			// Proactive token refresh — re-mint the runtime JWT shortly
			// BEFORE expiry so the steady state is one quiet scheduled
			// refresh per TTL window instead of the reactive hourly
			// 401→refresh log cycle on both the heartbeat and poll paths.
			// Gated like the poll loop: stub registrations have no platform
			// to refresh against.
			d.tokenRefresher = newTokenRefresher(tokenRefresherOptions{
				ExpiresAt: parseTokenExpiry(regResp.RuntimeTokenExpiresAt),
				Refresh: func(rctx context.Context) (time.Time, error) {
					result, err := refreshCreds(rctx, "proactive-expiry")
					if err != nil {
						return time.Time{}, err
					}
					return parseTokenExpiry(result.RuntimeTokenExpiresAt), nil
				},
			})
			d.tokenRefresher.Start()
		}
	}

	// Phase 3b: hot-reload daemon.yaml on direct edits. Without this,
	// `vim ~/.donmai/daemon.yaml` then :w leaves the running daemon with
	// stale in-memory state until restart — a real silent-staleness gap
	// the design doc flagged. fsnotify-driven reload makes operator edits
	// take effect within a coalesce window and pushes the new allowlist
	// into the spawner. The platform sees the changed hash on the next
	// heartbeat and emits daemon.allowlist.reported automatically.
	if d.opts.ConfigPath != "" {
		stop, err := startYamlWatcher(ctx, d.opts.ConfigPath, d.onYamlChanged)
		if err != nil {
			slog.Warn("daemon: yaml watcher disabled",
				"path", d.opts.ConfigPath, "err", err.Error())
		} else {
			d.yamlWatcherStop = stop
		}
	}

	// Translating-gateway loopback host (opt-in; ADR-2026-07-24 / 08). Started
	// after registration and the yaml watcher so a gateway failure never blocks
	// the daemon coming online — it is a best-effort side service at M1.
	if d.opts.EnableGateway {
		d.startGateway(ctx)
	}

	d.lifecycleMu.Lock()
	if d.ownsLifecycleLocked(lease) && d.stopGen == nil {
		d.setState(StateRunning)
	}
	d.lifecycleMu.Unlock()
	return nil
}

// startGateway constructs and starts the translating-gateway loopback host with
// a local JSONL cost ledger under the brand state dir. Best-effort: a ledger or
// listener failure logs a warning and leaves the gateway disabled rather than
// failing daemon startup.
func (d *Daemon) startGateway(ctx context.Context) {
	ledgerPath := statepath.Resolve("gateway/cost-events.jsonl", "")
	var sink costfeed.Sink
	if ledgerPath != "" {
		if l, err := costfeed.NewJSONLLedger(ledgerPath); err != nil {
			slog.Warn("daemon: gateway cost ledger unavailable; using in-memory sink", "err", err.Error())
			sink = &costfeed.MemorySink{}
		} else {
			sink = l
		}
	} else {
		sink = &costfeed.MemorySink{}
	}

	g := gateway.New(gateway.Options{Sink: sink})
	if err := g.Start(ctx); err != nil {
		slog.Warn("daemon: translating gateway failed to start", "err", err.Error())
		return
	}
	d.mu.Lock()
	d.gateway = g
	d.gatewayLedger = ledgerPath
	d.mu.Unlock()
	slog.Info("daemon: translating gateway started", "addr", g.Addr())
}

// GatewayStatus returns the current gateway status for the
// /api/daemon/gateway surface. When the gateway is disabled it reports
// enabled:false with the OSS-supported surface list.
func (d *Daemon) GatewayStatus() gateway.Status {
	d.mu.RLock()
	g := d.gateway
	ledger := d.gatewayLedger
	d.mu.RUnlock()
	if g == nil {
		return gateway.Status{Enabled: false, Surfaces: gateway.SupportedSurfaces()}
	}
	return g.Status(ledger)
}

// onYamlChanged is the fsnotify callback wired in Start(). Called whenever
// daemon.yaml is rewritten on disk (operator edit or our own mutation-apply
// path). Replaces the in-memory project list and pushes it into the
// spawner; the heartbeat goroutine's next beat will detect the new hash
// and report up to the platform.
//
// Defensive: only mutates state when projects[] actually differs from the
// in-memory copy. Other fields (capacity, orchestrator URL) are NOT
// hot-reloaded — those touch listeners we don't currently support
// re-binding live.
func (d *Daemon) onYamlChanged(cfg *Config) {
	d.mu.Lock()
	if d.config == nil {
		d.mu.Unlock()
		return
	}
	// Cheap equality check on the structured allowlist projection — same
	// shape the heartbeat reports, so this exactly matches "what the
	// platform would see change".
	before := AllowlistEntriesFromConfig(d.config.EffectiveProjectConfigs())
	after := AllowlistEntriesFromConfig(cfg.EffectiveProjectConfigs())
	beforeIDs := strings.Join(d.config.EffectiveEnabledProjectIDs(), "\x00")
	afterIDs := strings.Join(cfg.EffectiveEnabledProjectIDs(), "\x00")
	if allowlistHash(before) == allowlistHash(after) && beforeIDs == afterIDs {
		d.mu.Unlock()
		return
	}
	d.config.ProjectAdmissionVersion = cfg.ProjectAdmissionVersion
	d.config.EnabledProjectIDs = cfg.EffectiveEnabledProjectIDs()
	d.config.Repositories = cfg.Repositories
	d.config.Projects = cfg.Projects
	d.mu.Unlock()

	slog.Info("[yaml-watcher] reloaded projects",
		"beforeCount", len(before), "afterCount", len(after))
	if d.spawner != nil {
		d.spawner.SetProjectConfiguration(cfg.EffectiveProjectConfigs(), cfg.EffectiveEnabledProjectIDs())
		d.spawner.SetProjectAdmissionMode(cfg.EffectiveProjectAdmissionMode())
	}
}

// Stop begins a one-way terminal transition. An incomplete drain leaves the
// daemon in StateDraining with Done open so a later bounded Stop call can finish
// the same transition. Only a fully drained daemon stops its loops, reports
// StateStopped, and closes Done. No shutdown wait outlives ctx: an uncooperative
// poll callback or landing hook may delay completion, but cannot hold a caller
// past its own deadline.
func (d *Daemon) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// A completed generation has an immutable result, including for callers whose
	// own context has already expired.
	d.lifecycleMu.Lock()
	if gen := d.stopGen; gen != nil && gen.terminal {
		err := gen.terminalErr
		d.lifecycleMu.Unlock()
		return err
	}
	d.lifecycleMu.Unlock()

	lease, err := d.claimLifecycle(ctx, lifecycleStop)
	if err != nil {
		// Prefer a terminal result that raced the caller's wait over its now-stale
		// context error. This makes repeated Stop idempotent after completion.
		d.lifecycleMu.Lock()
		if gen := d.stopGen; gen != nil && gen.terminal {
			err = gen.terminalErr
		}
		d.lifecycleMu.Unlock()
		return err
	}
	defer d.releaseLifecycle(lease)

	d.lifecycleMu.Lock()
	if gen := d.stopGen; gen != nil && gen.terminal {
		err := gen.terminalErr
		d.lifecycleMu.Unlock()
		return err
	}
	if d.stopGen == nil {
		d.stopGen = &stopGeneration{id: lease.id}
	}
	gen := d.stopGen
	// Reassert draining on retries. Nothing may reopen admission once this
	// generation exists, even if a previous attempt was incomplete.
	d.setState(StateDraining)
	poller := d.poller
	spawner := d.spawner
	heartbeat := d.heartbeat
	refresher := d.tokenRefresher
	d.lifecycleMu.Unlock()

	// Start every shutdown barrier before waiting for any of them. In particular,
	// an unjoinable poll or landing callback must not suppress worker admission
	// closure or process-group termination.
	if spawner != nil {
		spawner.Pause()
	}
	var pollDone <-chan struct{}
	if poller != nil {
		pollDone = poller.beginStop()
	}
	landingDone := d.beginLandingStop()

	drainCtx, cancel := context.WithTimeout(ctx, d.drainTimeout())
	var drainErr error
	if spawner != nil {
		drainErr = spawner.DrainContext(drainCtx)
	}
	cancel()

	pollErr := waitCompletionContext(ctx, pollDone)
	landingErr := waitCompletionContext(ctx, landingDone)
	attemptErr := drainErr
	if attemptErr == nil {
		attemptErr = pollErr
	}
	if attemptErr == nil {
		attemptErr = landingErr
	}
	if attemptErr != nil {
		if hook := d.stopAttemptBeforeRelease; hook != nil {
			hook(attemptErr)
		}
		return attemptErr
	}

	// Only a fully joined attempt owns terminal publication. The loop stoppers
	// precede the terminal state and Done publication, but never run under the
	// lifecycle metadata lock.
	if heartbeat != nil {
		heartbeat.Stop()
	}
	if refresher != nil {
		refresher.Stop()
	}

	d.lifecycleMu.Lock()
	if !d.ownsLifecycleLocked(lease) || d.stopGen != gen || d.stopGen.id != gen.id || gen.terminal {
		err := gen.terminalErr
		d.lifecycleMu.Unlock()
		return err
	}
	watcherStop := d.yamlWatcherStop
	d.yamlWatcherStop = nil
	d.lifecycleMu.Unlock()
	if watcherStop != nil {
		watcherStop()
	}

	// Release adopted per-session shims WITHOUT stopping them (§D1/§D10). This
	// is the whole point of shim ownership: shutting the daemon down drops a
	// socket, and each shim keeps its harness and starts its bounded orphan clock
	// until the next daemon adopts. Stopping them here would make an ordinary
	// restart destructive again, which is the defect the ADR exists to remove.
	d.ReleaseAdoptedSessionShims()

	// Tear the translating gateway down (releases its loopback listener and
	// every session route). Best-effort, bounded by ctx.
	d.mu.Lock()
	gw := d.gateway
	d.gateway = nil
	d.mu.Unlock()
	if gw != nil {
		_ = gw.Stop(ctx)
	}

	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if !d.ownsLifecycleLocked(lease) || d.stopGen != gen || d.stopGen.id != gen.id {
		return nil
	}
	if gen.terminal {
		return gen.terminalErr
	}
	gen.terminal = true
	gen.terminalErr = nil
	// State is the published completion fact; close Done only after readers can
	// observe it, so a Done waiter never sees the stale StateDraining value.
	d.setState(StateStopped)
	d.doneOnce.Do(func() { close(d.doneCh) })
	return gen.terminalErr
}

func (d *Daemon) drainTimeout() time.Duration {
	timeout := 30 * time.Second
	if cfg := d.Config(); cfg != nil && cfg.AutoUpdate.DrainTimeoutSeconds > 0 {
		timeout = time.Duration(cfg.AutoUpdate.DrainTimeoutSeconds) * time.Second
	}
	return timeout
}

// beginLandingStop fences future callback admission, cancels entered callbacks,
// and returns the one completion channel for the active landing generation. Once
// stopping is true no callback can replace this channel.
func (d *Daemon) beginLandingStop() <-chan struct{} {
	d.landingMu.Lock()
	d.landingStopping = true
	if d.landingCancel != nil {
		d.landingCancel()
	}
	done := d.landingDone
	d.landingMu.Unlock()
	return done
}

func waitCompletionContext(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	// Prefer an already-complete barrier over an already-canceled caller. This
	// preserves idempotent Stop completion after the final worker/callback exits.
	select {
	case <-done:
		return nil
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Drain manually closes admission while retaining a resumable daemon. Unlike
// Stop it does not cancel poll/landing infrastructure or make the lifecycle
// terminal; Resume reopens the exact spawner once the requested drain completes.
func (d *Daemon) Drain(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	lease, err := d.claimLifecycle(ctx, lifecycleDrain)
	if err != nil {
		return err
	}
	defer d.releaseLifecycle(lease)

	d.lifecycleMu.Lock()
	if d.stopGen != nil {
		state := d.State()
		d.lifecycleMu.Unlock()
		return fmt.Errorf("cannot drain — terminal stop has begun (state %q)", state)
	}
	if d.State() != StateRunning && d.State() != StatePaused && d.State() != StateDraining {
		state := d.State()
		d.lifecycleMu.Unlock()
		return fmt.Errorf("cannot drain — current state %q", state)
	}
	spawner := d.spawner
	d.setState(StateDraining)
	d.lifecycleMu.Unlock()
	if spawner == nil {
		return nil
	}
	return spawner.DrainContext(ctx)
}

// handlePollWorkItem is the body of the primary poll loop's OnWork callback,
// extracted as a method so the landing-run routing and worker-capability
// threading are directly unit-testable without standing up a full PollService.
//
// orchestratorURL is the registration-time orchestrator URL captured by the
// poll loop; it is used for the best-effort NACK on a stale claim.
//
// Behaviour is byte-identical to the inlined closure for the existing
// (session-spawn) path. Two additive, nil-safe hooks layer on top:
//
//   - When item.WorkType == LandingWorkType AND OnLandingWork is wired, the
//     item is routed to that handler out-of-band and never becomes a session.
//     With no hook wired the branch is skipped entirely, so the item flows to
//     the unchanged session path exactly as before (no producer emits
//     LandingWorkType today).
//   - When WorkerCapabilitiesFunc is wired, its flags are advertised on the
//     built SessionDetail via WithWorkerCapabilities. With no func wired the
//     SessionDetail is built with no capability option, preserving the current
//     wire shape exactly.
func (d *Daemon) handlePollWorkItem(item PollWorkItem, orchestratorURL string) error {
	// Landing-run routing — INERT unless OnLandingWork is wired. A landing-run
	// trigger is not a session: route it out-of-band so it never spawns an
	// agent or counts toward the concurrency quota.
	if item.WorkType == LandingWorkType {
		if onLanding := d.onLandingWork(); onLanding != nil {
			if lerr := d.runLandingWork(onLanding, item); lerr != nil {
				slog.Warn("daemon poll: landing-run handler failed",
					"repository", item.Repository, "err", lerr)
			}
			return nil
		}
		// No handler wired: fall through to the unchanged session path.
	}

	// Use d.spawner.AllProjects() so that satellite-org projects registered
	// via AddProjects after daemon start are visible to the slug→URL
	// resolution performed by PollItemToSessionSpec / PollItemToSessionDetail.
	// cfg.Projects is the startup snapshot and would miss any extraProjects
	// appended later.
	projects := d.spawner.AllProjects()
	spec := PollItemToSessionSpec(item, projects)

	// Advertise worker capabilities only when a provider is wired, so the nil
	// case builds a byte-identical SessionDetail (no capability option).
	opts := []SessionDetailOption{}
	if capsFn := d.workerCapabilitiesFunc(); capsFn != nil {
		opts = append(opts, WithWorkerCapabilities(capsFn()))
	}
	// Per-item, per-org merge-queue landing flag from the coordinator wins over
	// the org-agnostic worker capability above. nil (older coordinator) is a
	// no-op, so the legacy WorkerCapabilitiesFunc value stands. Appended AFTER
	// WithWorkerCapabilities so the per-org flag is authoritative when present.
	opts = append(opts, WithMergeQueueLanding(item.MergeQueueLanding))
	detail := PollItemToSessionDetail(
		item,
		projects,
		orchestratorURL,
		d.runtimeJWT(),
		d.WorkerID(),
		opts...,
	)
	if _, err := d.AcceptWorkWithDetail(spec, detail); err != nil {
		// Local accept-work failure means the orchestrator's claim of this
		// session is stale on first contact — the session is in `claimed`
		// state with this worker, but no `donmai agent run` subprocess will
		// ever execute for it. NACK so the orchestrator releases the claim and
		// re-queues immediately, instead of waiting for the stale-claim sweep
		// (15min default) to reclaim. NACK is best-effort: failure to deliver
		// it only adds latency; the original AcceptWork error is what the
		// caller logs.
		item := item
		nackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		nackErr := callNackEndpoint(
			nackCtx,
			nil, // default 10s-timeout client
			orchestratorURL,
			item.SessionID,
			d.WorkerID(),
			d.runtimeJWT(),
			fmt.Sprintf("accept work failed: %v", err),
			&item,
		)
		if nackErr != nil {
			slog.Warn(
				"daemon poll: nack failed; orchestrator will reclaim via stale-claim sweep",
				"sessionId", item.SessionID,
				"acceptErr", err.Error(),
				"nackErr", nackErr.Error(),
			)
		} else {
			slog.Info(
				"daemon poll: nacked rejected session",
				"sessionId", item.SessionID,
				"reason", err.Error(),
			)
		}
		return fmt.Errorf("accept work %s: %w", item.SessionID, err)
	}
	return nil
}

// runLandingWork owns one landing callback from admission through completion.
// Stop fences additions under landingMu before canceling and waiting, so no
// callback can begin after the stop barrier and Done cannot outrun its return.
func (d *Daemon) runLandingWork(fn func(context.Context, PollWorkItem) error, item PollWorkItem) error {
	d.landingMu.Lock()
	if d.landingStopping {
		d.landingMu.Unlock()
		return errors.New("daemon is stopping; landing work rejected")
	}
	if d.landingActive == 0 {
		// The prior generation is closed. A fresh callback generation gets one
		// shared open completion channel, reclaimed when its final callback exits.
		d.landingDone = make(chan struct{})
	}
	ctx := d.landingCtx
	d.landingActive++
	d.landingMu.Unlock()
	defer func() {
		d.landingMu.Lock()
		d.landingActive--
		if d.landingActive == 0 {
			close(d.landingDone)
		}
		d.landingMu.Unlock()
	}()
	return fn(ctx, item)
}

// Done returns a channel that is closed when the daemon has fully stopped.
func (d *Daemon) Done() <-chan struct{} {
	return d.doneCh
}

// Pause stops accepting new work without draining. It reports whether it
// performed a transition, allowing control endpoints to avoid claiming a stale
// pause request succeeded.
func (d *Daemon) Pause() bool {
	lease, ok := d.tryClaimLifecycle(lifecyclePause)
	if !ok {
		return false
	}
	defer d.releaseLifecycle(lease)

	d.lifecycleMu.Lock()
	if d.stopGen != nil || d.State() != StateRunning {
		d.lifecycleMu.Unlock()
		return false
	}
	spawner := d.spawner
	d.lifecycleMu.Unlock()
	if spawner != nil {
		spawner.Pause()
	}
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if !d.ownsLifecycleLocked(lease) || d.stopGen != nil || d.State() != StateRunning {
		return false
	}
	d.setState(StatePaused)
	return true
}

// Resume re-enables accepting work after a completed manual pause or drain. It
// is retained for source compatibility; callers that need the durable
// restart-preparation abandonment error use ResumeContext.
func (d *Daemon) Resume() bool {
	return d.ResumeContext(context.Background()) == nil
}

// ResumeContext re-enables admission only after durably abandoning this
// controller's local planned-stop authorization. External fence holds are not
// consumed or deleted; a later restart takes a new snapshot and identity.
func (d *Daemon) ResumeContext(ctx context.Context) error {
	lease, ok := d.tryClaimLifecycle(lifecycleResume)
	if !ok {
		return errors.New("cannot resume while another lifecycle operation is active")
	}
	defer d.releaseLifecycle(lease)

	d.lifecycleMu.Lock()
	if d.stopGen != nil || (d.State() != StatePaused && d.State() != StateDraining) {
		state := d.State()
		d.lifecycleMu.Unlock()
		return fmt.Errorf("cannot resume while daemon is %s", state)
	}
	spawner := d.spawner
	d.lifecycleMu.Unlock()
	if err := d.abandonRestartPreparation(ctx); err != nil {
		return fmt.Errorf("abandon restart preparation: %w", err)
	}
	if spawner != nil {
		spawner.Resume()
	}
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if !d.ownsLifecycleLocked(lease) || d.stopGen != nil || (d.State() != StatePaused && d.State() != StateDraining) {
		return errors.New("cannot resume because lifecycle ownership changed")
	}
	d.setState(StateRunning)
	return nil
}

// AcceptWork dispatches a session spec to the spawner.
func (d *Daemon) AcceptWork(spec SessionSpec) (*SessionHandle, error) {
	return d.AcceptWorkWithDetail(spec, nil)
}

// AcceptWorkWithDetail dispatches a session spec to the spawner and
// records the per-session detail used by the spawned `donmai agent run`
// process. Pass nil detail when the caller does not have one (legacy
// tests); the spawner falls through to env-only inputs.
//
// Detail is stored before spawning so the child can fetch it immediately.
// The store rejects an already-owned session id before calling the spawner, and
// failed admission rolls back only the exact generation installed by this
// attempt. The detail is otherwise removed when the spawner emits the
// corresponding SessionEventEnded event, so stale credentials never linger in
// memory.
func (d *Daemon) AcceptWorkWithDetail(spec SessionSpec, detail *SessionDetail) (*SessionHandle, error) {
	if d.State() != StateRunning {
		return nil, fmt.Errorf("daemon is not running (state %q)", d.State())
	}
	if d.spawner == nil {
		return nil, errors.New("spawner not initialised")
	}
	if detail != nil {
		if detail.SessionID == "" {
			if spec.SessionID == "" {
				return nil, errors.New("session id is required when detail is provided")
			}
			detail.SessionID = spec.SessionID
		}
		if detail.SessionID != spec.SessionID {
			return nil, fmt.Errorf(
				"session detail id %q does not match spec session id %q",
				detail.SessionID,
				spec.SessionID,
			)
		}
		if spec.OrganizationID != "" && detail.OrganizationID != "" && spec.OrganizationID != detail.OrganizationID {
			return nil, fmt.Errorf(
				"session detail organization %q does not match spec organization %q",
				detail.OrganizationID,
				spec.OrganizationID,
			)
		}
		if spec.OrganizationID == "" {
			spec.OrganizationID = detail.OrganizationID
		}
		if len(detail.AdmissionReceipt) > 0 {
			// The narrow-only claim gate runs first: for a claim-bound
			// admission it either validates an already-supplied ClaimReceipt
			// or computes one from this host's local reality, populating
			// detail.ClaimReceipt/EffectiveCell before the binding
			// cross-check below reads them. For every other admission it is
			// a no-op.
			if err := d.evaluateNarrowOnlyClaim(detail); err != nil {
				return nil, err
			}
			if err := d.validateExecutionRuntimeBinding(detail); err != nil {
				return nil, err
			}
			compiler, ok := d.opts.ProviderRegistry.(ExecutionPreflightProvider)
			if !ok || d.opts.ExecutionPreflightStore == nil {
				return nil, errors.New("receipt-bearing work requires daemon execution preflight and durable receipt store")
			}
			preflightInput := struct {
				SessionID               string          `json:"sessionId"`
				WorkerID                string          `json:"workerId"`
				AdmissionReceipt        json.RawMessage `json:"admissionReceipt"`
				ClaimReceipt            json.RawMessage `json:"claimReceipt,omitempty"`
				EffectiveCell           json.RawMessage `json:"effectiveCell"`
				ExecutionRuntimeBinding json.RawMessage `json:"executionRuntimeBinding"`
				OperationalPayload      json.RawMessage `json:"operationalPayload"`
			}{
				SessionID: detail.SessionID, WorkerID: detail.WorkerID,
				AdmissionReceipt: detail.AdmissionReceipt, ClaimReceipt: detail.ClaimReceipt,
				EffectiveCell: detail.EffectiveCell, ExecutionRuntimeBinding: detail.ExecutionRuntimeBinding,
				OperationalPayload: detail.OperationalPayload,
			}
			detailJSON, err := json.Marshal(preflightInput)
			if err != nil {
				return nil, fmt.Errorf("marshal execution preflight detail: %w", err)
			}
			receipt, err := compiler.PreflightExecution(detailJSON)
			if len(receipt) == 0 {
				if err != nil {
					return nil, fmt.Errorf("execution adaptation preflight returned no receipt: %w", err)
				}
				return nil, errors.New("execution adaptation preflight returned no receipt")
			}
			hostReceipt, decodeErr := executioncell.DecodeHostAdaptationReceipt(receipt)
			if decodeErr != nil {
				return nil, fmt.Errorf("execution adaptation receipt: %w", decodeErr)
			}
			binding, _ := executioncell.DecodeRuntimeBinding(detail.ExecutionRuntimeBinding)
			if hostReceipt.RequestID != binding.RequestID || hostReceipt.WorkerID != binding.WorkerID ||
				hostReceipt.PlacementID != binding.PlacementID || hostReceipt.ClaimID != binding.ClaimID {
				return nil, errors.New("execution adaptation receipt does not match daemon runtime binding")
			}
			if (err == nil && hostReceipt.Decision != "ready") || (err != nil && hostReceipt.Decision != "denied") {
				return nil, errors.New("execution adaptation result and receipt decision disagree")
			}
			if err := d.opts.ExecutionPreflightStore.Persist(spec.SessionID, receipt); err != nil {
				return nil, fmt.Errorf("persist execution adaptation receipt: %w", err)
			}
			if err != nil {
				return nil, fmt.Errorf("execution adaptation preflight: %w", err)
			}
			detail.HostAdaptationReceipt = append(json.RawMessage(nil), receipt...)
		}
	}
	var detailLease sessionDetailLease
	if detail != nil && d.sessionDetails != nil {
		var stored bool
		detailLease, stored = d.sessionDetails.StoreIfAbsent(detail)
		if !stored {
			return nil, fmt.Errorf("session %q already has an active detail", spec.SessionID)
		}
	}
	handle, err := d.spawner.AcceptWork(spec)
	if err != nil {
		if detailLease.generation != 0 {
			d.sessionDetails.DeleteIfOwner(detailLease)
		}
		return nil, err
	}
	return handle, nil
}

func (d *Daemon) validateExecutionRuntimeBinding(detail *SessionDetail) error {
	binding, err := executioncell.DecodeRuntimeBinding(detail.ExecutionRuntimeBinding)
	if err != nil {
		return fmt.Errorf("execution runtime binding: %w", err)
	}
	if binding.RequestID != detail.SessionID || binding.WorkerID != detail.WorkerID {
		return errors.New("execution runtime binding is not owned by this request and worker")
	}
	if currentWorkerID := strings.TrimSpace(d.WorkerID()); currentWorkerID != "" && binding.WorkerID != currentWorkerID {
		return errors.New("execution runtime binding is not owned by the daemon's current worker registration")
	}
	effective, err := executioncell.DecodeResolvedExecutionCell(detail.EffectiveCell)
	if err != nil {
		return fmt.Errorf("execution effective cell: %w", err)
	}
	if binding.PlacementID != effective.Placement.ID {
		return errors.New("execution runtime placement does not match effective cell")
	}
	if len(detail.ClaimReceipt) == 0 {
		if binding.ClaimID != "" {
			return errors.New("exact execution runtime binding must not carry claim id")
		}
		return nil
	}
	claim, err := executioncell.DecodeClaimReceipt(detail.ClaimReceipt)
	if err != nil {
		return fmt.Errorf("execution claim receipt: %w", err)
	}
	if binding.ClaimID != claim.Value().ClaimID {
		return errors.New("execution claim receipt is not the active claim for this request and worker")
	}
	return nil
}

// evaluateNarrowOnlyClaim is the daemon claim path's narrow-only gate
// (ADR-2026-08-05-versioned-execution-cell-and-session-reference.md D4/D5).
// It is deliberately additive and best-effort at the shallow layer: the
// admission receipt's full semantic validity is the ExecutionPreflightProvider
// compiler's job (already wired below), so a receipt this function cannot even
// decode is left untouched here and denied downstream exactly as before this
// change. Two things it DOES enforce, both new:
//
//  1. A pre-supplied ClaimReceipt for a claim-bound admission must narrow the
//     admission (executioncell.AssertNarrowClaim) — previously this invariant
//     was only checked inside the runner process the compiler seam spawns, so
//     a daemon wired without a real compiler never checked it at all.
//  2. When no ClaimReceipt has been supplied yet and a ClaimGateProvider is
//     wired, the daemon computes one itself from this host's own local
//     reality (executioncell.EvaluateClaim) and attaches the result — never
//     widening the admitted cell, and never assembling a fallback when local
//     reality falls short (D3): a failed predicate is a typed refusal that
//     propagates as an error, which the existing poll-loop caller turns into a
//     NACK exactly like any other local accept-work failure.
//
// A nil-safe no-op when no ClaimGateProvider is wired and no ClaimReceipt was
// supplied keeps every existing deployment byte-identical.
func (d *Daemon) evaluateNarrowOnlyClaim(detail *SessionDetail) error {
	admission, err := executioncell.DecodeAdmissionReceipt(detail.AdmissionReceipt)
	if err != nil {
		// Not this function's concern: the compiler below is the authoritative
		// validator of admission-receipt content and already fails closed on a
		// malformed receipt.
		return nil
	}
	value := admission.Value()
	if value.Decision != executioncell.AdmissionAdmitted || value.Cell == nil {
		return nil
	}
	cell := *value.Cell
	if cell.Placement.Kind != executioncell.PlacementPool || cell.Placement.Resolution != executioncell.PlacementClaimBound {
		return nil
	}

	if len(detail.ClaimReceipt) != 0 {
		claim, err := executioncell.DecodeClaimReceipt(detail.ClaimReceipt)
		if err != nil {
			return fmt.Errorf("execution claim receipt: %w", err)
		}
		if err := executioncell.AssertNarrowClaim(admission, claim); err != nil {
			return fmt.Errorf("execution claim receipt does not narrow admission: %w", err)
		}
		return nil
	}

	provider, ok := d.claimGateProvider()
	if !ok {
		return nil
	}
	binding, err := executioncell.DecodeRuntimeBinding(detail.ExecutionRuntimeBinding)
	if err != nil {
		return fmt.Errorf("execution runtime binding: %w", err)
	}
	if strings.TrimSpace(binding.ClaimID) == "" {
		// No claim attempt has been assigned to this host yet — nothing to
		// gate. The existing downstream compiler already denies a claim-bound
		// admission with no claim receipt.
		return nil
	}
	cellJSON, err := executioncell.CanonicalJSON(cell)
	if err != nil {
		return fmt.Errorf("canonicalize admitted cell: %w", err)
	}
	realityJSON, err := provider.ResolveClaimLocalReality(cellJSON)
	// Record the cached ruleset-snapshot status this claim
	// decision was evaluated against, regardless of whether it succeeds or
	// is denied/refused below — a refusal is exactly the case
	// `routing explain` most needs to show rev/age/degraded for. A nil-safe
	// no-op when no snapshot source is configured (d.opts.RulesetSnapshot
	// == nil), preserving today's behaviour for every deployment that never
	// wires one.
	d.recordClaimGateSnapshotDecision(detail.SessionID)
	if err != nil {
		return fmt.Errorf("resolve claim local reality: %w", err)
	}
	var local executioncell.ClaimLocalReality
	if err := json.Unmarshal(realityJSON, &local); err != nil {
		return fmt.Errorf("decode claim local reality: %w", err)
	}
	claim, err := executioncell.EvaluateClaim(admission, binding.ClaimID, local, time.Now())
	if err != nil {
		return fmt.Errorf("evaluate narrow-only claim: %w", err)
	}
	claimValue := claim.Value()
	if claimValue.Decision == executioncell.ClaimDenied {
		return fmt.Errorf("execution claim denied: %s: %s", claimValue.DenialCode, claimValue.DenialDetail)
	}
	effectiveJSON, err := executioncell.CanonicalJSON(*claimValue.EffectiveCell)
	if err != nil {
		return fmt.Errorf("canonicalize claimed effective cell: %w", err)
	}
	detail.ClaimReceipt = claim.Bytes()
	detail.EffectiveCell = effectiveJSON
	return nil
}

// SessionDetail returns the stored per-session detail for the given
// session id, or (nil, false) if no detail is recorded. Used by the
// HTTP server's /api/daemon/sessions/<id> handler.
func (d *Daemon) SessionDetail(sessionID string) (*SessionDetail, bool) {
	if d.sessionDetails == nil {
		return nil, false
	}
	return d.sessionDetails.Get(sessionID)
}

// UpdateSessionRuntimeCredentials re-stamps the runtime credentials of the
// stored session details attributed to prevWorkerID, and reports how many it
// updated.
//
// This is the seam for an embedding binary that runs MORE than one worker
// identity on a single daemon process. A host admitted to several
// organisations holds a registration per organisation; each refreshes its own
// runtime token on its own schedule, while all of their sessions share this
// daemon's one session-detail store (they arrive through AcceptWorkWithDetail
// carrying their own WorkerID and AuthToken). Wire this into the per-identity
// re-registration path so a refresh reaches exactly that identity's children:
// pass the worker id its sessions were claimed under as prevWorkerID and the
// identity the refresh settled on as workerID. They are equal whenever the
// refresh preserved the identity; they differ when it fell back to a full
// re-registration, and passing both is what moves those sessions onto the new
// identity instead of orphaning them on a retired one.
//
// The daemon's own registration is already wired this way internally — this
// method exists for the identities the daemon does not own. Calling it for the
// daemon's own identity is unnecessary and would double-stamp a refresh the
// daemon has already applied.
func (d *Daemon) UpdateSessionRuntimeCredentials(prevWorkerID, workerID, authToken string) int {
	if d.sessionDetails == nil {
		return 0
	}
	return d.sessionDetails.UpdateRuntimeCredentials(prevWorkerID, workerID, authToken)
}

// Update triggers a manual auto-update check.
//
// Behavior: restart preflight (which refuses uncovered direct-owned work) →
// fetch manifest → verify → swap. Whether the update succeeds, fails, or finds
// no new version, the daemon remains draining until explicit ResumeContext
// durably abandons the controller-local stop authorization.
func (d *Daemon) Update(ctx context.Context) (*UpdateResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	attempt, err := d.beginUpdate(ctx)
	if err != nil {
		return nil, err
	}
	return attempt.run(ctx)
}

// updateAttempt transfers the one lifecycle lease from synchronous restart
// preparation into update execution. The HTTP handler may start run in a
// goroutine without opening a gap in which Resume could abandon the permission
// after the route already returned success.
type updateAttempt struct {
	daemon *Daemon
	lease  *lifecycleLease
}

func (d *Daemon) beginUpdate(ctx context.Context) (*updateAttempt, error) {
	lease, err := d.claimLifecycle(ctx, lifecycleUpdate)
	if err != nil {
		return nil, err
	}
	if _, err := d.prepareRestartWithLease(ctx, lease); err != nil {
		d.releaseLifecycle(lease)
		return nil, fmt.Errorf("prepare restart before update: %w", err)
	}
	d.lifecycleMu.Lock()
	if !d.ownsLifecycleLocked(lease) || d.stopGen != nil || d.State() != StateDraining {
		state := d.State()
		d.lifecycleMu.Unlock()
		d.releaseLifecycle(lease)
		return nil, fmt.Errorf("cannot update — current state %q", state)
	}
	d.setState(StateUpdating)
	d.lifecycleMu.Unlock()
	return &updateAttempt{daemon: d, lease: lease}, nil
}

func (a *updateAttempt) finish() {
	if a == nil || a.daemon == nil {
		return
	}
	d := a.daemon
	d.lifecycleMu.Lock()
	if d.ownsLifecycleLocked(a.lease) && d.stopGen == nil && d.State() == StateUpdating {
		d.setState(StateDraining)
	}
	d.lifecycleMu.Unlock()
	d.releaseLifecycle(a.lease)
}

func (a *updateAttempt) run(ctx context.Context) (*UpdateResult, error) {
	if a == nil || a.daemon == nil {
		return nil, errors.New("update attempt is not initialized")
	}
	d := a.daemon
	defer a.finish()
	if ctx == nil {
		ctx = context.Background()
	}
	// A failed, unavailable, or completed self-update does not silently abandon
	// the prepared stop authorization. finish leaves the daemon draining;
	// external holds remain in force until explicit resume.
	cfg := d.Config()
	if cfg == nil {
		return nil, errors.New("no config loaded")
	}
	if d.runPreparedUpdate != nil {
		return d.runPreparedUpdate(ctx, cfg.AutoUpdate, d.EffectiveVersion())
	}
	updater := NewUpdater(UpdaterOptions{
		CurrentVersion: d.EffectiveVersion(),
		Config:         cfg.AutoUpdate,
		SkipExit:       true, // HTTP-driven update: caller decides to exit.
	})
	return updater.RunUpdate(ctx)
}

// recordOSSRoutingDecision feeds the routing trace store with the
// degenerate decision that fits the OSS daemon's single-sandbox shape.
// Called from the spawner SessionEventStarted listener; the call site
// is a no-op when routingTraces is nil.
//
// The decision shape is locked by afclient.RoutingDecision (no free-form
// "reason" field exists on the wire); the human-readable rationale is
// surfaced via the trace step's Note instead. ChosenLLM resolves to the
// first registered AgentRuntime provider name when a registry is wired,
// or "stub" as a fallback for test/no-orchestrator paths where the
// registry is nil.
//
// Wave 11 / S6a — once a real cross-provider scheduler ships, this
// function gets retired in favour of scheduler.RecordDecision wired
// directly into the dispatch path.
func (d *Daemon) recordOSSRoutingDecision(sessionID string) {
	if d.routingTraces == nil {
		return
	}
	chosenLLM := "stub"
	if d.opts.ProviderRegistry != nil {
		if names := d.opts.ProviderRegistry.Names(); len(names) > 0 {
			chosenLLM = names[0]
		}
	}
	decision := afclient.RoutingDecision{
		SessionID:     sessionID,
		ChosenSandbox: "local",
		ChosenLLM:     chosenLLM,
		DecidedAt:     time.Now().UTC(),
	}
	trace := []afclient.RoutingTraceStep{{
		Step:      1,
		Phase:     "capability-filter",
		Dimension: "sandbox",
		Remaining: []string{"local"},
		Note:      "OSS daemon — only candidate is local",
	}}
	d.routingTraces.RecordDecision(decision, trace)
}

// ── internal helpers ──────────────────────────────────────────────────────

// spawnerActiveCount is the unclassed occupancy total.
//
// It routes through the paired snapshot rather than reading the spawner alone so
// it can never disagree with spawnerActiveSessionCounts about how full this host
// is — two occupancy answers that drift apart is how a host advertises capacity
// it does not have.
func (d *Daemon) spawnerActiveCount() int {
	active, _ := d.spawnerActiveSessionCounts()
	return active
}

func (d *Daemon) spawnerActiveInteractiveCount() int {
	_, interactive := d.spawnerActiveSessionCounts()
	return interactive
}

// spawnerActiveSessionCounts is the daemon's coherent occupancy snapshot.
//
// It is the sum of two DISJOINT populations: sessions this daemon spawned
// directly, and per-session shims it adopted or quarantined at startup. Both
// occupy real slots on this machine, and a shim's harness is running whether or
// not this daemon has authority over it (§D7), so both must be reported.
// Counting only the spawner's own children would advertise a restarted daemon as
// idle while every pre-restart terminal is still live.
//
// Adopted shims are interactive by construction — the first delivery of shim
// ownership is interactive-only (§D11) — so they count toward BOTH totals.
// Quarantined shims count toward the unclassed total only: this daemon could not
// negotiate with them, so classifying their run mode would be a guess.
func (d *Daemon) spawnerActiveSessionCounts() (active, activeInteractive int) {
	if d.spawner != nil {
		active, activeInteractive = d.spawner.ActiveSessionCounts()
	}
	adopted, quarantined := d.sessionShimCounts()
	return active + adopted + quarantined, activeInteractive + adopted
}

// sessionShimCounts returns the adopted and quarantined shim populations
// separately so the caller can decide how each contributes.
func (d *Daemon) sessionShimCounts() (adopted, quarantined int) {
	if d.shims == nil {
		return 0, 0
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return len(d.shims.adopted), len(d.shims.quarantined)
}

// ActiveSessionCount returns the number of agent sessions currently running
// under the daemon's shared WorkerSpawner. Exported for compatibility with
// embedders that wire a satellite heartbeat's GetActiveCount callback.
func (d *Daemon) ActiveSessionCount() int {
	return d.spawnerActiveCount()
}

// ActiveInteractiveSessionCount returns the interactive-occupancy subset under
// the daemon's shared WorkerSpawner: PTY "interactive" plus legacy "interview"
// run-mode sessions. Headless and unknown modes are excluded. Callers that need
// a value paired with total occupancy should use ActiveSessionCounts instead.
func (d *Daemon) ActiveInteractiveSessionCount() int {
	return d.spawnerActiveInteractiveCount()
}

// ActiveSessionCounts returns a coherent machine-wide occupancy snapshot for a
// shared-spawner multi-identity configuration. active includes every run mode;
// activeInteractive is the union of PTY "interactive" and legacy "interview"
// sessions. Embedders should wire this method to a satellite heartbeat's
// GetActiveSessionCounts callback so both fields are sampled under one spawner
// lock.
func (d *Daemon) ActiveSessionCounts() (active, activeInteractive int) {
	return d.spawnerActiveSessionCounts()
}

// MaxConcurrentSessions returns the per-host capacity ceiling configured for
// this daemon. Exported so embedders can wire this into a satellite heartbeat's
// GetMaxCount callback for a shared-spawner multi-identity configuration.
func (d *Daemon) MaxConcurrentSessions() int {
	return d.maxConcurrentSessions()
}

// RegistrationStatus returns the current registration state of the daemon
// (idle, busy, or draining). Exported so embedders can wire this into a
// satellite heartbeat's GetStatus callback for a shared-spawner multi-identity
// configuration.
func (d *Daemon) RegistrationStatus() RegistrationStatus {
	switch d.State() {
	case StateStarting, StateRecovering, StateDraining, StateUpdating:
		return RegistrationDraining
	case StateRunning:
		cfg := d.Config()
		if cfg == nil {
			return RegistrationIdle
		}
		// §D4: readiness is withheld until the adoption pass has finished. Until
		// then this daemon does not yet know what is occupied, and "idle" would be
		// a claim it cannot support.
		if !d.SessionShimAdoptionComplete() || !d.SessionShimCarrierActivationComplete() {
			return RegistrationDraining
		}
		active := d.spawnerActiveCount()
		if active >= cfg.Capacity.MaxConcurrentSessions {
			return RegistrationBusy
		}
		return RegistrationIdle
	default:
		return RegistrationIdle
	}
}

// StartedAt returns the daemon's UTC start time (zero before Start()).
func (d *Daemon) StartedAt() time.Time {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.startedAt
}

// SubstrateCapabilities returns the substrate capabilities detected at daemon
// startup. The slice is nil before Start() is called and non-nil afterwards
// (even when no optional toolchains were found — the always-present set is
// returned). The returned slice is a copy; callers may mutate it freely.
// (ADR-2026-05-12-capacity-pools-and-substrate-resolution.md §H.)
func (d *Daemon) SubstrateCapabilities() []internaldaemon.SubstrateCapability {
	if d.capabilitySet == nil {
		return nil
	}
	return d.capabilitySet.Capabilities()
}
