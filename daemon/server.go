package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// Server is the daemon's HTTP control API. It wraps a Daemon and exposes
// the endpoints consumed by `af daemon …` and `rensei daemon …`.
type Server struct {
	daemon *Daemon
	httpd  *http.Server

	mu      sync.Mutex
	started bool
	addr    string
}

// NewServer builds an HTTP server for d. The handler is registered but the
// server is not yet listening — call Start to bind.
func NewServer(d *Daemon) *Server {
	s := &Server{daemon: d}
	mux := http.NewServeMux()
	s.register(mux)
	addr := fmt.Sprintf("%s:%d", d.opts.HTTPHost, d.opts.HTTPPort)
	s.httpd = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	s.addr = addr
	return s
}

// Addr returns the address the server is bound to (after Start succeeds).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Start binds the listener and serves in a goroutine. Errors during accept
// are reported via the returned channel — callers should select on it
// alongside their own shutdown signal.
func (s *Server) Start() (<-chan error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil, errors.New("server already started")
	}
	listener, err := net.Listen("tcp", s.httpd.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", s.httpd.Addr, err)
	}
	s.addr = listener.Addr().String()
	errCh := make(chan error, 1)
	go func() {
		if err := s.httpd.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		close(errCh)
	}()
	s.started = true
	return errCh, nil
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return nil
	}
	return s.httpd.Shutdown(ctx)
}

// register wires endpoint handlers. The 14 endpoints from the acceptance
// criteria correspond to the daemonDoer methods in afcli/daemon.go plus
// the accept-work and pool/eviction endpoints.
func (s *Server) register(mux *http.ServeMux) {
	mux.HandleFunc("/api/daemon/status", s.method(http.MethodGet, s.handleStatus))
	mux.HandleFunc("/api/daemon/stats", s.method(http.MethodGet, s.handleStats))
	mux.HandleFunc("/api/daemon/pause", s.method(http.MethodPost, s.handlePause))
	mux.HandleFunc("/api/daemon/resume", s.method(http.MethodPost, s.handleResume))
	mux.HandleFunc("/api/daemon/stop", s.method(http.MethodPost, s.handleStop))
	mux.HandleFunc("/api/daemon/drain", s.method(http.MethodPost, s.handleDrain))
	mux.HandleFunc("/api/daemon/update", s.method(http.MethodPost, s.handleUpdate))
	mux.HandleFunc("/api/daemon/capacity", s.method(http.MethodPost, s.handleSetCapacity))
	mux.HandleFunc("/api/daemon/pool/stats", s.method(http.MethodGet, s.handlePoolStats))
	mux.HandleFunc("/api/daemon/pool/evict", s.method(http.MethodPost, s.handlePoolEvict))
	mux.HandleFunc("/api/daemon/sessions", s.handleSessions) // GET=list, POST=accept
	mux.HandleFunc("/api/daemon/heartbeat", s.method(http.MethodGet, s.handleHeartbeat))
	mux.HandleFunc("/api/daemon/doctor", s.method(http.MethodGet, s.handleDoctor))
	mux.HandleFunc("/healthz", s.method(http.MethodGet, s.handleHealthz))
}

// method wraps a handler with a method check.
func (s *Server) method(want string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != want {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fn(w, r)
	}
}

// ── handlers ──────────────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := s.daemon.Config()
	statusName := mapState(s.daemon.State())
	resp := afclient.DaemonStatusResponse{
		Status:          statusName,
		Version:         Version,
		MachineID:       safeMachineID(cfg),
		PID:             os.Getpid(),
		UptimeSeconds:   int64(time.Since(s.daemon.StartedAt()).Seconds()),
		ActiveSessions:  countActive(s.daemon),
		MaxSessions:     safeMaxSessions(cfg),
		ProjectsAllowed: safeProjectsLen(cfg),
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
	}
	writeJSON(w, http.StatusOK, &resp)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	cfg := s.daemon.Config()
	withPool := r.URL.Query().Get("pool") == "true"
	byMachine := r.URL.Query().Get("byMachine") == "true"

	resp := afclient.DaemonStatsResponse{
		Capacity: afclient.MachineCapacity{
			MaxConcurrentSessions: safeMaxSessions(cfg),
			MaxVCpuPerSession:     safeMaxVCPU(cfg),
			MaxMemoryMbPerSession: safeMaxMem(cfg),
			ReservedVCpu:          safeReservedVCPU(cfg),
			ReservedMemoryMb:      safeReservedMem(cfg),
		},
		ActiveSessions: countActive(s.daemon),
		QueueDepth:     0,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
	}
	if withPool {
		stats, err := s.poolStats(r.Context())
		if err == nil {
			resp.Pool = stats
		}
	}
	if byMachine {
		// Single-machine fleet — emit just our own machine entry.
		resp.ByMachine = []afclient.MachineStats{{
			ID:             safeMachineID(cfg),
			Region:         safeRegion(cfg),
			Status:         mapState(s.daemon.State()),
			Version:        Version,
			ActiveSessions: countActive(s.daemon),
			Capacity:       resp.Capacity,
			UptimeSeconds:  int64(time.Since(s.daemon.StartedAt()).Seconds()),
			LastSeenAt:     time.Now().UTC().Format(time.RFC3339),
		}}
	}
	writeJSON(w, http.StatusOK, &resp)
}

func (s *Server) handlePause(w http.ResponseWriter, _ *http.Request) {
	s.daemon.Pause()
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "paused"})
}

func (s *Server) handleResume(w http.ResponseWriter, _ *http.Request) {
	s.daemon.Resume()
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "resumed"})
}

func (s *Server) handleStop(w http.ResponseWriter, _ *http.Request) {
	// Respond first so the client gets the 200, then schedule the stop.
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "stopping"})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = s.daemon.Stop(ctx)
	}()
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	var body afclient.DaemonDrainRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	timeout := time.Duration(body.TimeoutSeconds) * time.Second
	if timeout == 0 {
		cfg := s.daemon.Config()
		if cfg != nil {
			timeout = time.Duration(cfg.AutoUpdate.DrainTimeoutSeconds) * time.Second
		}
	}
	go func() {
		if s.daemon.spawner != nil {
			_ = s.daemon.spawner.Drain(timeout)
		}
	}()
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: fmt.Sprintf("drain initiated (timeout %s)", timeout)})
}

func (s *Server) handleUpdate(w http.ResponseWriter, _ *http.Request) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_, _ = s.daemon.Update(ctx)
	}()
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "update initiated"})
}

func (s *Server) handleSetCapacity(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, &afclient.SetCapacityResponse{
			OK: false, Message: "invalid body: " + err.Error(),
		})
		return
	}
	if body.Key != "capacity.poolMaxDiskGb" {
		writeJSON(w, http.StatusBadRequest, &afclient.SetCapacityResponse{
			OK: false, Key: body.Key, Value: body.Value, Message: "unknown key",
		})
		return
	}
	n, err := strconv.Atoi(body.Value)
	if err != nil || n < 0 {
		writeJSON(w, http.StatusBadRequest, &afclient.SetCapacityResponse{
			OK: false, Key: body.Key, Value: body.Value, Message: "value must be non-negative integer",
		})
		return
	}
	// Persist to in-memory config; the CLI also writes daemon.yaml directly.
	s.daemon.mu.Lock()
	if s.daemon.config != nil {
		s.daemon.config.Capacity.PoolMaxDiskGb = n
	}
	s.daemon.mu.Unlock()
	writeJSON(w, http.StatusOK, &afclient.SetCapacityResponse{
		OK: true, Key: body.Key, Value: body.Value, Message: "applied",
	})
}

func (s *Server) handlePoolStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.poolStats(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, &afclient.WorkareaPoolStats{
			Members:   []afclient.WorkareaPoolMember{},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handlePoolEvict(w http.ResponseWriter, r *http.Request) {
	if s.daemon.opts.EvictHandler == nil {
		writeJSON(w, http.StatusNotImplemented, &afclient.EvictPoolResponse{
			Evicted: 0,
			Message: "pool eviction handler not wired (REN-1280 WorkareaProvider)",
		})
		return
	}
	var req afclient.EvictPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepoURL == "" {
		http.Error(w, "repoUrl is required", http.StatusBadRequest)
		return
	}
	resp, err := s.daemon.opts.EvictHandler.Evict(r.Context(), req)
	if err != nil {
		http.Error(w, "evict failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSessions multiplexes GET (list active sessions) and POST (accept work).
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.daemon.ActiveSessions())
	case http.MethodPost:
		var spec SessionSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		handle, err := s.daemon.AcceptWork(spec)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, handle)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, _ *http.Request) {
	if s.daemon.heartbeat == nil {
		writeJSON(w, http.StatusOK, &HeartbeatPayload{})
		return
	}
	last := s.daemon.heartbeat.LastPayload()
	writeJSON(w, http.StatusOK, &last)
}

func (s *Server) handleDoctor(w http.ResponseWriter, _ *http.Request) {
	cfg := s.daemon.Config()
	report := map[string]any{
		"state":           string(s.daemon.State()),
		"version":         Version,
		"configLoaded":    cfg != nil,
		"machineId":       safeMachineID(cfg),
		"workerId":        s.daemon.WorkerID(),
		"projectCount":    safeProjectsLen(cfg),
		"orchestratorUrl": safeOrchestratorURL(cfg),
		"heartbeat":       s.daemon.heartbeat != nil && s.daemon.heartbeat.IsRunning(),
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) poolStats(ctx context.Context) (*afclient.WorkareaPoolStats, error) {
	if s.daemon.opts.PoolStatsProvider == nil {
		return &afclient.WorkareaPoolStats{
			Members:   []afclient.WorkareaPoolMember{},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}, nil
	}
	return s.daemon.opts.PoolStatsProvider.Stats(ctx)
}

// ── helpers ───────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func mapState(s State) afclient.DaemonStatus {
	switch s {
	case StateRunning:
		return afclient.DaemonReady
	case StatePaused:
		return afclient.DaemonPaused
	case StateDraining:
		return afclient.DaemonDraining
	case StateUpdating:
		return afclient.DaemonUpdating
	case StateStopped:
		return afclient.DaemonStopped
	default:
		return afclient.DaemonStopped
	}
}

func countActive(d *Daemon) int { return d.spawnerActiveCount() }

func safeMachineID(c *Config) string {
	if c == nil {
		return ""
	}
	return c.Machine.ID
}

func safeRegion(c *Config) string {
	if c == nil {
		return ""
	}
	return c.Machine.Region
}

func safeMaxSessions(c *Config) int {
	if c == nil {
		return 0
	}
	return c.Capacity.MaxConcurrentSessions
}

func safeMaxVCPU(c *Config) int {
	if c == nil {
		return 0
	}
	return c.Capacity.MaxVCpuPerSession
}

func safeMaxMem(c *Config) int {
	if c == nil {
		return 0
	}
	return c.Capacity.MaxMemoryMbPerSession
}

func safeReservedVCPU(c *Config) int {
	if c == nil {
		return 0
	}
	return c.Capacity.ReservedForSystem.VCpu
}

func safeReservedMem(c *Config) int {
	if c == nil {
		return 0
	}
	return c.Capacity.ReservedForSystem.MemoryMb
}

func safeProjectsLen(c *Config) int {
	if c == nil {
		return 0
	}
	return len(c.Projects)
}

func safeOrchestratorURL(c *Config) string {
	if c == nil {
		return ""
	}
	return c.Orchestrator.URL
}
