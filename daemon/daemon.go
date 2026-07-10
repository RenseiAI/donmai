package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	internaldaemon "github.com/RenseiAI/donmai/internal/daemon"
	"github.com/RenseiAI/donmai/internal/statepath"
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
	// typed per-session map). It is sent verbatim as RegisterRequest.capabilities
	// and the platform persists it on workers.capabilities, where it gates the
	// capability-keyed claim lanes (KG-extraction, FD-4 landing's "merge-queue").
	//
	// Nil ⇒ the primary daemon falls back to the base substrate set
	// ({local,sandbox,workarea}) — byte-identical to today's behaviour — so a
	// pure-OSS embedder that never sets this is unaffected. Embedders that want
	// the worker to receive a capability-gated lane supply the full tag list
	// here (e.g. rensei-tui appends "merge-queue").
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

	// capabilitySet holds the substrate capabilities detected at startup.
	// It is populated before registration so the provides[] array can be
	// sent to the platform. Exposed via GET /api/daemon/capabilities.
	// (ADR-2026-05-12-capacity-pools-and-substrate-resolution.md §H.)
	capabilitySet *internaldaemon.CapabilitySet

	heartbeat *HeartbeatService
	poller    *PollService
	spawner   *WorkerSpawner

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

	stopOnce sync.Once
	doneCh   chan struct{}
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
	// Note: HTTPPort=0 is intentionally NOT auto-filled to
	// DefaultHTTPPort here — callers that want the well-known 7734
	// port (the cobra `donmai daemon run` entry point) substitute it
	// themselves before constructing Options. Leaving zero-as-
	// ephemeral here lets parallel tests bind 127.0.0.1:0 and have
	// the kernel pick free ports, eliminating the port-7734 bind
	// flake observed under -race when many tests share the default.
	d := &Daemon{
		opts:           opts,
		doneCh:         make(chan struct{}),
		sessionDetails: newSessionDetailStore(),
		routingTraces:  NewRoutingTraceStore(DefaultRoutingRingBufferSize),
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
func (d *Daemon) ActiveSessions() []SessionHandle {
	if d.spawner == nil {
		return nil
	}
	return d.spawner.ActiveSessions()
}

// Spawner returns the daemon's WorkerSpawner so callers can subscribe to
// session lifecycle events via WorkerSpawner.On without needing the spawner
// to be re-exposed through a higher-level hook. Returns nil before Start is
// called.
func (d *Daemon) Spawner() *WorkerSpawner {
	return d.spawner
}

// StopSession terminates a single in-flight session by id and frees its
// capacity slot. Returns false when the session is unknown (already exited
// or never spawned) or the spawner is not yet initialised. Wired to the
// POST /api/daemon/sessions/<id>/stop control-API route for the
// deterministic per-session cancel path (Guard 3 hard out-of-band leg).
func (d *Daemon) StopSession(id string) bool {
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
	if s := d.State(); s != StateStopped {
		return fmt.Errorf("cannot start — current state %q", s)
	}
	d.setState(StateStarting)

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
		// Registration capabilities: the embedder-supplied list when set
		// (rensei-tui appends "merge-queue" for the FD-4 landing lane), else the
		// base substrate set so a pure-OSS daemon is unchanged.
		regCaps := d.opts.RegistrationCapabilities
		if regCaps == nil {
			regCaps = []string{"local", "sandbox", "workarea"}
		}
		regOpts = RegistrationOptions{
			OrchestratorURL:         cfg.Orchestrator.URL,
			RegistrationToken:       token,
			MachineID:               cfg.Machine.ID,
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
			// Item 8 (fleet host-info): gather best-effort machine telemetry
			// once at startup and thread it onto RegisterRequest.HostInfo so
			// the platform populates the worker_hosts host-info columns. All
			// fields degrade to empty on an unsupported platform; gathering
			// never crashes registration.
			HostInfo: GatherHostInfo(d.EffectiveVersion(), d.StartedAt()),
		}
		var err error
		regResp, err = Register(ctx, regOpts)
		if err != nil {
			return fmt.Errorf("register: %w", err)
		}
		d.mu.Lock()
		d.workerID = regResp.WorkerID
		d.jwt = regResp.RuntimeToken
		d.mu.Unlock()
	}

	// Spawner — built before heartbeat/poll so the poll loop has a target for
	// AcceptWork dispatch on its very first tick.
	spawnerOpts := d.opts.SpawnerOptions
	spawnerOpts.Projects = cfg.EffectiveProjectConfigs()
	spawnerOpts.EnabledProjectIDs = cfg.EffectiveEnabledProjectIDs()
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
	d.spawner = NewWorkerSpawner(spawnerOpts)
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
		// Heartbeat + poll + the proactive token refresher share ONE refresh
		// implementation so a refresh on any path re-mints the runtime JWT
		// once and fans the fresh credentials out to every consumer.
		//
		// Token-refresh fix: route through RefreshRuntimeToken which probes a
		// real refresh endpoint first (preserving the workerId) and only
		// falls back to a full Register() — minting a fresh workerId — if
		// the platform side has not yet shipped the refresh handler. The
		// `[runtime-token]` log line attests which path was taken.
		refreshCreds := func(rctx context.Context, reason string) (*RefreshTokenResult, error) {
			d.mu.RLock()
			currentWorker := d.workerID
			d.mu.RUnlock()
			result, err := RefreshRuntimeToken(rctx, regOpts, currentWorker, reason)
			if err != nil {
				return nil, err
			}
			d.mu.Lock()
			d.workerID = result.WorkerID
			d.jwt = result.RuntimeToken
			d.mu.Unlock()
			if d.sessionDetails != nil {
				d.sessionDetails.UpdateRuntimeCredentials(result.WorkerID, result.RuntimeToken)
			}
			// Fan the fresh credentials out to both long-running loops. The
			// loop that triggered the refresh re-applies its own copy too —
			// harmlessly idempotent — but the OTHER loop now picks up the new
			// token without burning a 401 round-trip (and a duplicate refresh
			// + log cycle) of its own.
			if d.heartbeat != nil {
				d.heartbeat.SetCredentials(result.WorkerID, result.RuntimeToken)
			}
			if d.poller != nil {
				d.poller.SetCredentials(result.WorkerID, result.RuntimeToken)
			}
			// Persist the refreshed credentials to the on-disk cache so other
			// readers of daemon.jwt pick up the new token instead of the now-
			// stale one. Heartbeat/poll use the in-memory swap above, but the
			// per-session credential resolver and the runner's platform client
			// read the token from disk — without this write they keep using the
			// expired token (snapshot 307→HTML, status-update 401). Best-effort:
			// a cache-write failure must never abort the refresh.
			if regOpts.JWTPath != "" {
				if serr := persistRefreshedToken(regOpts.JWTPath, result, regOpts.Now); serr != nil {
					slog.Warn("[runtime-token] failed to persist refreshed token to cache",
						"event", "refresh.cache-write-failed",
						"workerId", result.WorkerID,
						"jwtPath", regOpts.JWTPath,
						"err", serr.Error(),
					)
				} else {
					slog.Info("[runtime-token]",
						"event", "refresh.cached",
						"workerId", result.WorkerID,
					)
				}
			}
			return result, nil
		}
		reregister := func(rctx context.Context, reason string) (string, string, error) {
			result, err := refreshCreds(rctx, reason)
			if err != nil {
				return "", "", err
			}
			return result.WorkerID, result.RuntimeToken, nil
		}

		// Heartbeat. OnReregister handles reactive runtime-token expiry (the
		// backstop behind the proactive refresher below): on a 401, or the
		// worker falling out of Redis after the 5-min heartbeat TTL (returned
		// as 404), we re-mint via RefreshRuntimeToken.
		d.heartbeat = NewHeartbeatService(HeartbeatOptions{
			WorkerID:        regResp.WorkerID,
			Hostname:        cfg.Machine.ID,
			OrchestratorURL: cfg.Orchestrator.URL,
			RuntimeJWT:      regResp.RuntimeToken,
			IntervalSeconds: regResp.HeartbeatIntervalSeconds(),
			GetActiveCount:  func() int { return d.spawnerActiveCount() },
			GetMaxCount:     func() int { return d.maxConcurrentSessions() },
			GetStatus:       d.RegistrationStatus,
			Region:          cfg.Machine.Region,
			// Item 8: per-beat CPU/mem load sample → last_cpu_pct/last_mem_pct.
			// Best-effort stdlib probe; omits the load key when it can't sample.
			GetLoad:      SampleLoad,
			OnReregister: reregister,
			LogWarn: func(format string, args ...any) {
				slog.Warn(fmt.Sprintf(format, args...))
			},
			LogInfo: func(format string, args ...any) {
				slog.Info(fmt.Sprintf(format, args...))
			},
			GetAllowlist: func() []ProjectAllowlistEntry {
				// Use d.spawner.AllProjects() so that satellite-org
				// projects registered via AddProjects are included in
				// the heartbeat's reported allowlist. d.spawner is set
				// before the heartbeat is constructed (see above).
				return AllowlistEntriesFromConfig(d.spawner.AllProjects())
			},
			// Phase 2c: handle platform-queued mutations.
			OnPendingMutations: d.applyPendingMutations,
			// Phase 2e: surface hostStatus signals (pool_deleted etc.)
			// to af daemon stats. The latest observed status is stored
			// in d.hostStatus; callers read via Daemon.HostStatus().
			OnHostStatus: d.setLastHostStatus,
		})
		d.heartbeat.Start()

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
			})
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

	d.setState(StateRunning)
	return nil
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
	}
}

// Stop performs a graceful shutdown: drain in-flight sessions, stop loops,
// and transition to stopped. The context is currently unused but is retained
// for future use (e.g. cancelling drain via ctx.Done).
// Stop drains spawned work, halts the heartbeat/poller loops, closes the
// yaml watcher, and transitions to StateStopped. Safe to call concurrently
// or repeatedly — the whole body is gated by stopOnce so a deferred
// Stop() in a test fixture racing with an HTTP /stop handler is benign.
func (d *Daemon) Stop(_ context.Context) error {
	d.stopOnce.Do(func() {
		if d.State() == StateStopped {
			return
		}
		d.setState(StateDraining)

		timeout := 30 * time.Second
		if cfg := d.Config(); cfg != nil && cfg.AutoUpdate.DrainTimeoutSeconds > 0 {
			timeout = time.Duration(cfg.AutoUpdate.DrainTimeoutSeconds) * time.Second
		}
		if d.spawner != nil {
			_ = d.spawner.Drain(timeout)
		}
		if d.heartbeat != nil {
			d.heartbeat.Stop()
		}
		if d.poller != nil {
			d.poller.Stop()
		}
		if d.tokenRefresher != nil {
			d.tokenRefresher.Stop()
		}
		if d.yamlWatcherStop != nil {
			d.yamlWatcherStop()
			d.yamlWatcherStop = nil
		}
		close(d.doneCh)
		d.setState(StateStopped)
	})
	return nil
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
			if lerr := onLanding(context.Background(), item); lerr != nil {
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

// Done returns a channel that is closed when the daemon has fully stopped.
func (d *Daemon) Done() <-chan struct{} {
	return d.doneCh
}

// Pause stops accepting new work without draining.
func (d *Daemon) Pause() {
	if d.spawner != nil {
		d.spawner.Pause()
	}
	d.setState(StatePaused)
}

// Resume re-enables accepting work.
func (d *Daemon) Resume() {
	if d.spawner != nil {
		d.spawner.Resume()
	}
	d.setState(StateRunning)
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
// Detail is stored before spawning and removed when the spawner emits
// the corresponding SessionEventEnded event so stale credentials do
// not linger in memory.
func (d *Daemon) AcceptWorkWithDetail(spec SessionSpec, detail *SessionDetail) (*SessionHandle, error) {
	if d.State() != StateRunning {
		return nil, fmt.Errorf("daemon is not running (state %q)", d.State())
	}
	if d.spawner == nil {
		return nil, errors.New("spawner not initialised")
	}
	if detail != nil && detail.SessionID == "" {
		detail.SessionID = spec.SessionID
	}
	if detail != nil && d.sessionDetails != nil {
		d.sessionDetails.Set(detail)
	}
	return d.spawner.AcceptWork(spec)
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

// Update triggers a manual auto-update check.
//
// Behavior: drain → fetch manifest → verify → swap → exit (3). If no update
// is available the call is idempotent and the daemon transitions back to
// running. If signature verification fails, the swap is aborted and an
// error is returned. The caller (HTTP handler) typically returns the
// outcome to the client and may then call Stop().
func (d *Daemon) Update(ctx context.Context) (*UpdateResult, error) {
	cfg := d.Config()
	if cfg == nil {
		return nil, errors.New("no config loaded")
	}
	d.setState(StateUpdating)
	defer func() {
		// Restore running state if we did not actually exit. The
		// spawner.Drain() below flips `accepting=false` directly without
		// going through Daemon.Pause(), so resuming the state alone leaves
		// the spawner stuck NACKing every claim with "not accepting new
		// work" while status reports `ready`. Re-resume the spawner so the
		// two stay in lockstep. Symptom (pre-fix): daemon uptime > drain
		// timeout, status=ready, every claim NACKs.
		if d.State() == StateUpdating {
			if d.spawner != nil {
				d.spawner.Resume()
			}
			d.setState(StateRunning)
		}
	}()

	timeout := time.Duration(cfg.AutoUpdate.DrainTimeoutSeconds) * time.Second
	if d.spawner != nil {
		_ = d.spawner.Drain(timeout)
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

func (d *Daemon) spawnerActiveCount() int {
	if d.spawner == nil {
		return 0
	}
	return d.spawner.ActiveCount()
}

// ActiveSessionCount returns the number of agent sessions currently running
// under the daemon's shared WorkerSpawner. Exported so embedders can wire this
// into a satellite heartbeat's GetActiveCount callback for a shared-spawner
// multi-identity configuration.
func (d *Daemon) ActiveSessionCount() int {
	return d.spawnerActiveCount()
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
	case StateDraining, StateUpdating:
		return RegistrationDraining
	case StateRunning:
		cfg := d.Config()
		if cfg == nil {
			return RegistrationIdle
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
