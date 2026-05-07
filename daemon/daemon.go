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

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

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
	HTTPPort int
	// PoolStatsProvider returns the current workarea pool snapshot. May be
	// nil — the /api/daemon/pool/stats endpoint will return an empty
	// snapshot in that case (acceptance criterion: pool integration is
	// optional in the runtime port; full WorkareaProvider wiring is REN-1280).
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
}

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

	heartbeat *HeartbeatService
	poller    *PollService
	spawner   *WorkerSpawner

	// sessionDetails stores the per-session payload the spawner
	// hands out to `af agent run` workers via the local control
	// HTTP API at /api/daemon/sessions/<id>. (REN-1461 / F.2.8.)
	sessionDetails *sessionDetailStore

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
	if opts.HTTPPort == 0 {
		opts.HTTPPort = DefaultHTTPPort
	}
	d := &Daemon{
		opts:           opts,
		doneCh:         make(chan struct{}),
		sessionDetails: newSessionDetailStore(),
	}
	d.state.Store(StateStopped)
	return d
}

// State returns the current lifecycle state.
func (d *Daemon) State() State {
	v, _ := d.state.Load().(State)
	return v
}

func (d *Daemon) setState(s State) {
	d.state.Store(s)
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

	var (
		regResp *RegisterResponse
		regOpts RegistrationOptions
	)
	if !d.opts.SkipRegistration {
		token := cfg.Orchestrator.AuthToken
		if token == "" {
			token = os.Getenv("RENSEI_DAEMON_TOKEN")
		}
		if token == "" {
			token = "local-stub-no-token"
		}
		regOpts = RegistrationOptions{
			OrchestratorURL:   cfg.Orchestrator.URL,
			RegistrationToken: token,
			MachineID:         cfg.Machine.ID,
			Hostname:          cfg.Machine.ID,
			Version:           Version,
			MaxAgents:         cfg.Capacity.MaxConcurrentSessions,
			Capabilities:      []string{"local", "sandbox", "workarea"},
			Region:            cfg.Machine.Region,
			JWTPath:           d.opts.JWTPath,
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
	spawnerOpts.Projects = cfg.Projects
	spawnerOpts.MaxConcurrentSessions = cfg.Capacity.MaxConcurrentSessions
	if spawnerOpts.BaseEnv == nil {
		spawnerOpts.BaseEnv = map[string]string{}
	}
	if d.workerID != "" {
		spawnerOpts.BaseEnv["RENSEI_WORKER_ID"] = d.workerID
	}
	spawnerOpts.BaseEnv["RENSEI_ORCHESTRATOR_URL"] = cfg.Orchestrator.URL
	// Default WorkerCommand: spawn `af agent run` from the same
	// binary as the running daemon process so session lifecycle is
	// owned in-tree. Operators can override via SpawnerOptions.
	// (REN-1461 / F.2.8 — daemon wire-up.)
	if len(spawnerOpts.WorkerCommand) == 0 {
		if cmd := defaultWorkerCommand(); cmd != nil {
			spawnerOpts.WorkerCommand = cmd
		}
	}
	// Default child stdout/stderr → slog so operators can see what the
	// spawned `af agent run` is doing without manually attaching a
	// debugger or rerunning under foreground. v0.5.0 had StdoutPrefixWriter
	// / StderrPrefixWriter nil by default, which the spawner translated to
	// drain-and-discard — leaving operators flying blind between
	// runner.Run() start and a `status=failed` post. Callers that already
	// supply their own writers via SpawnerOptions retain priority.
	// (REN-1463 / v0.5.1.)
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

	if regResp != nil {
		// Heartbeat + poll share an OnReregister implementation so a 401 on
		// either path re-mints the runtime JWT once and refreshes both
		// services with the new credentials.
		//
		// REN-1481 fix: route through RefreshRuntimeToken which probes a
		// real refresh endpoint first (preserving the workerId) and only
		// falls back to a full Register() — minting a fresh workerId — if
		// the platform side has not yet shipped the refresh handler. The
		// `[runtime-token]` log line attests which path was taken.
		reregister := func(rctx context.Context) (string, string, error) {
			d.mu.RLock()
			currentWorker := d.workerID
			d.mu.RUnlock()
			result, err := RefreshRuntimeToken(rctx, regOpts, currentWorker, "auth-failure")
			if err != nil {
				return "", "", err
			}
			d.mu.Lock()
			d.workerID = result.WorkerID
			d.jwt = result.RuntimeToken
			d.mu.Unlock()
			if d.sessionDetails != nil {
				d.sessionDetails.UpdateRuntimeCredentials(result.WorkerID, result.RuntimeToken)
			}
			return result.WorkerID, result.RuntimeToken, nil
		}

		// Heartbeat. OnReregister handles runtime-token expiry: the platform
		// runtime JWT TTL is 1h with no refresh endpoint, so on a 401 (or the
		// worker falling out of Redis after the 5-min heartbeat TTL — returned
		// as 404) we re-mint by calling Register() with ForceReregister=true.
		d.heartbeat = NewHeartbeatService(HeartbeatOptions{
			WorkerID:        regResp.WorkerID,
			Hostname:        cfg.Machine.ID,
			OrchestratorURL: cfg.Orchestrator.URL,
			RuntimeJWT:      regResp.RuntimeToken,
			IntervalSeconds: regResp.HeartbeatIntervalSeconds(),
			GetActiveCount:  func() int { return d.spawnerActiveCount() },
			GetMaxCount:     func() int { return d.maxConcurrentSessions() },
			GetStatus:       d.registrationStatus,
			Region:          cfg.Machine.Region,
			OnReregister:    reregister,
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
					spec := pollItemToSessionSpec(item, cfg.Projects)
					detail := pollItemToSessionDetail(
						item,
						cfg.Projects,
						cfg.Orchestrator.URL,
						d.runtimeJWT(),
						d.WorkerID(),
					)
					if _, err := d.AcceptWorkWithDetail(spec, detail); err != nil {
						return fmt.Errorf("accept work %s: %w", item.SessionID, err)
					}
					return nil
				},
				OnReregister: reregister,
			})
			d.poller.Start()
		}
	}

	d.setState(StateRunning)
	return nil
}

// Stop performs a graceful shutdown: drain in-flight sessions, stop loops,
// and transition to stopped. The context is currently unused but is retained
// for future use (e.g. cancelling drain via ctx.Done).
func (d *Daemon) Stop(_ context.Context) error {
	current := d.State()
	if current == StateStopped {
		return nil
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
	d.stopOnce.Do(func() { close(d.doneCh) })
	d.setState(StateStopped)
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
// records the per-session detail used by the spawned `af agent run`
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
		// Restore running state if we did not actually exit.
		if d.State() == StateUpdating {
			d.setState(StateRunning)
		}
	}()

	timeout := time.Duration(cfg.AutoUpdate.DrainTimeoutSeconds) * time.Second
	if d.spawner != nil {
		_ = d.spawner.Drain(timeout)
	}

	updater := NewUpdater(UpdaterOptions{
		CurrentVersion: Version,
		Config:         cfg.AutoUpdate,
		SkipExit:       true, // HTTP-driven update: caller decides to exit.
	})
	return updater.RunUpdate(ctx)
}

// ── internal helpers ──────────────────────────────────────────────────────

func (d *Daemon) spawnerActiveCount() int {
	if d.spawner == nil {
		return 0
	}
	return d.spawner.ActiveCount()
}

func (d *Daemon) registrationStatus() RegistrationStatus {
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
