package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
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
}

// PoolStatsProvider returns a workarea pool snapshot.
type PoolStatsProvider interface {
	Stats(ctx context.Context) (*afclient.WorkareaPoolStats, error)
}

// EvictHandler executes a pool eviction request and returns the response.
type EvictHandler interface {
	Evict(ctx context.Context, req afclient.EvictPoolRequest) (*afclient.EvictPoolResponse, error)
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
	spawner   *WorkerSpawner

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
		opts:   opts,
		doneCh: make(chan struct{}),
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

// ActiveSessions returns a snapshot of in-flight session handles.
func (d *Daemon) ActiveSessions() []SessionHandle {
	if d.spawner == nil {
		return nil
	}
	return d.spawner.ActiveSessions()
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

	if !d.opts.SkipRegistration {
		token := cfg.Orchestrator.AuthToken
		if token == "" {
			token = os.Getenv("RENSEI_DAEMON_TOKEN")
		}
		if token == "" {
			token = "local-stub-no-token"
		}
		regResp, err := Register(ctx, RegistrationOptions{
			OrchestratorURL:   cfg.Orchestrator.URL,
			RegistrationToken: token,
			Hostname:          cfg.Machine.ID,
			Version:           Version,
			MaxAgents:         cfg.Capacity.MaxConcurrentSessions,
			Capabilities:      []string{"local", "sandbox", "workarea"},
			Region:            cfg.Machine.Region,
			JWTPath:           d.opts.JWTPath,
		})
		if err != nil {
			return fmt.Errorf("register: %w", err)
		}
		d.mu.Lock()
		d.workerID = regResp.WorkerID
		d.jwt = regResp.RuntimeJWT
		d.mu.Unlock()

		// Heartbeat
		d.heartbeat = NewHeartbeatService(HeartbeatOptions{
			WorkerID:        regResp.WorkerID,
			Hostname:        cfg.Machine.ID,
			OrchestratorURL: cfg.Orchestrator.URL,
			RuntimeJWT:      regResp.RuntimeJWT,
			IntervalSeconds: regResp.HeartbeatIntervalSeconds,
			GetActiveCount:  func() int { return d.spawnerActiveCount() },
			GetMaxCount:     func() int { return cfg.Capacity.MaxConcurrentSessions },
			GetStatus:       d.registrationStatus,
			Region:          cfg.Machine.Region,
		})
		d.heartbeat.Start()
	}

	// Spawner
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
	d.spawner = NewWorkerSpawner(spawnerOpts)

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
	if d.State() != StateRunning {
		return nil, fmt.Errorf("daemon is not running (state %q)", d.State())
	}
	if d.spawner == nil {
		return nil, errors.New("spawner not initialised")
	}
	return d.spawner.AcceptWork(spec)
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
