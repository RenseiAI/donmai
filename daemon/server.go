package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

const drainResponseWriteGrace = time.Second

// Server is the daemon's HTTP control API. It wraps a Daemon and exposes
// the endpoints consumed by `donmai daemon …` and `rensei daemon …`.
type Server struct {
	daemon *Daemon
	httpd  *http.Server

	mu      sync.Mutex
	started bool
	addr    string

	// stopAttemptTimeout and stopRetryDelay keep endpoint retries bounded. They
	// are test seams; zero values use the production defaults in stopTimeouts.
	stopAttemptTimeout time.Duration
	stopRetryDelay     time.Duration

	// stopCompletionActive fences the one asynchronous completion owner for this
	// daemon's single terminal generation. Concurrent /stop requests acknowledge
	// the already-owned transition instead of starting competing retry loops.
	stopCompletionActive bool
	stopCompletionStarts uint64       // guarded by mu; lifecycle observability/test seam
	stopAttemptResults   chan<- error // test seam; receives each completed attempt

	// kitReg is the in-process Kit registry serving /api/daemon/kits*
	// (Wave 9 A2). Lazily constructed via kitRegistryOrEmpty so test
	// servers built with NewServer get a default scan path automatically.
	// Tests inject a fake by assigning the field directly before serving.
	kitReg kitRegistryDoer

	// agentReg is the in-process AgentCard registry serving
	// /api/daemon/agents* (GO-3 / Wave 5). Lazily constructed via
	// agentCardRegistryOrDefault using the daemon's sessionDetailStore.
	// Tests inject a fake by assigning the field directly before serving.
	agentReg agentCardRegistry
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
	host, port, err := ResolveControlBind(s.daemon.opts.HTTPHost, s.daemon.opts.HTTPPort, s.daemon.opts.HTTPPort != 0)
	if err != nil {
		return nil, fmt.Errorf("control listener configuration: %w", err)
	}
	s.httpd.Addr = net.JoinHostPort(host, strconv.Itoa(port))
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
	// Workarea cache control surface. `pool` named four different things
	// across this codebase; it now names only the org-owned capacity pool, so
	// the daemon's warm-workarea surface moved to the `workarea` noun.
	mux.HandleFunc("/api/daemon/workarea/stats", s.method(http.MethodGet, s.handleWorkareaStats))
	mux.HandleFunc("/api/daemon/workarea/evict", s.method(http.MethodPost, s.handleWorkareaEvict))
	// Deprecated aliases of the two lines above, removed in
	// afclient.WorkareaAliasRemovalVersion. They serve the identical handler
	// and additionally announce themselves (Deprecation/Warning headers plus a
	// daemon log line) so a caller pinned to an older release keeps working and
	// finds out why it should not stay there.
	mux.HandleFunc("/api/daemon/pool/stats", s.method(http.MethodGet,
		deprecatedSurface("GET /api/daemon/pool/stats", "GET /api/daemon/workarea/stats", s.handleWorkareaStats)))
	mux.HandleFunc("/api/daemon/pool/evict", s.method(http.MethodPost,
		deprecatedSurface("POST /api/daemon/pool/evict", "POST /api/daemon/workarea/evict", s.handleWorkareaEvict)))
	mux.HandleFunc("/api/daemon/sessions", s.handleSessions) // GET=list, POST=accept
	// Per-session sub-routes. Spawned `donmai agent run` processes fetch
	// their full QueuedWork shape via GET <id>; the deterministic cancel
	// wire posts to <id>/stop to kill exactly one session + free its slot.
	// The path-pattern dispatch is custom because the stdlib mux only
	// supports prefix matching pre-Go 1.22 in this codebase, so the single
	// prefix handler multiplexes both shapes.
	mux.HandleFunc("/api/daemon/sessions/", s.handleSessionSubroute)
	mux.HandleFunc("/api/daemon/heartbeat", s.method(http.MethodGet, s.handleHeartbeat))
	mux.HandleFunc("/api/daemon/doctor", s.method(http.MethodGet, s.handleDoctor))
	// providers (Wave 9)
	mux.HandleFunc("/api/daemon/gateway", s.method(http.MethodGet, s.handleGateway))

	mux.HandleFunc("/api/daemon/providers", s.method(http.MethodGet, s.handleListProviders))
	mux.HandleFunc("/api/daemon/providers/", s.handleGetProvider) // trailing slash → prefix matcher
	// routing (Wave 9)
	mux.HandleFunc("/api/daemon/routing/config", s.method(http.MethodGet, s.handleGetRoutingConfig))
	mux.HandleFunc("/api/daemon/routing/explain/", s.handleExplainRouting) // trailing slash → prefix matcher
	// kits (Wave 9)
	mux.HandleFunc("/api/daemon/kits", s.handleKitsCollection)
	mux.HandleFunc(kitRoutePrefix, s.handleKitDetail)
	mux.HandleFunc("/api/daemon/kit-sources", s.handleKitSourcesCollection)
	mux.HandleFunc(kitSourceRoutePrefix, s.handleKitSourceDetail)
	// workareas (Wave 9) — list, inspect, restore, and diff over the
	// on-disk archive registry plus active pool members.
	mux.HandleFunc("/api/daemon/workareas", s.handleWorkareasRoot)
	mux.HandleFunc("/api/daemon/workareas/", s.handleWorkareaItem)
	// capabilities (Stream H — pool-aware daemon advertises substrate).
	// GET /api/daemon/capabilities returns the provides[] set detected
	// at startup and sent to POST /api/workers/register.
	mux.HandleFunc("/api/daemon/capabilities", s.method(http.MethodGet, s.handleCapabilities))
	// agents (GO-3 / Wave 5) — AgentCard stubs synthesised from active
	// session-detail store entries. GET list + GET detail + 404 on miss.
	mux.HandleFunc("/api/daemon/agents", s.method(http.MethodGet, s.handleListAgents))
	mux.HandleFunc("/api/daemon/agents/", s.handleGetAgent) // trailing slash → prefix matcher
	mux.HandleFunc("/healthz", s.method(http.MethodGet, s.handleHealthz))
}

// method wraps a handler with a method check.
// deprecatedSurface wraps next so every response served through a deprecated
// alias carries the standard deprecation signal before the handler writes its
// status line. Behaviour is otherwise identical — the alias is a second door
// onto the same handler, never a reimplementation of it.
func deprecatedSurface(alias, replacement string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		announceDeprecatedSurface(w, alias, replacement)
		next(w, r)
	}
}

// announceDeprecatedSurface marks a response as served by a deprecated alias.
// It MUST run before the handler writes a status line, because headers set
// after WriteHeader are discarded.
//
// The signal is threefold: RFC 8594 `Deprecation`, a `Link` to the successor,
// and an RFC 7234 `Warning: 299` carrying the human sentence — none of which
// disturb a client that only decodes the JSON body.
func announceDeprecatedSurface(w http.ResponseWriter, alias, replacement string) {
	notice := afclient.DeprecatedSurfaceNotice(alias, replacement)
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", `<`+replacement+`>; rel="successor-version"`)
	w.Header().Set("Warning", `299 - "`+notice+`"`)
	slog.Warn("deprecated daemon control surface used",
		"alias", alias,
		"replacement", replacement,
		"removedIn", afclient.WorkareaAliasRemovalVersion,
	)
}

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
	statusName := daemonStatus(s.daemon)
	enabledProjectIDs := safeEnabledProjectIDs(cfg)
	appliedIDs := appliedProjectIDs(s.daemon, cfg)
	resp := afclient.DaemonStatusResponse{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		Status:                  statusName,
		Version:                 s.daemon.EffectiveVersion(),
		MachineID:               safeMachineID(cfg),
		PID:                     os.Getpid(),
		UptimeSeconds:           int64(time.Since(s.daemon.StartedAt()).Seconds()),
		ActiveSessions:          countActive(s.daemon),
		MaxSessions:             safeMaxSessions(cfg),
		ProjectsAllowed:         len(appliedIDs),
		EnabledProjectIDs:       enabledProjectIDs,
		AppliedProjectIDs:       appliedIDs,
		Projects:                buildProjectStatusRows(s.daemon, cfg, enabledProjectIDs, appliedIDs),
		Timestamp:               time.Now().UTC().Format(time.RFC3339),
	}
	writeJSON(w, http.StatusOK, &resp)
}

func buildProjectStatusRows(d *Daemon, cfg *Config, enabledIDs, appliedIDs []string) []afclient.DaemonProjectStatus {
	enabled := make(map[string]struct{}, len(enabledIDs))
	applied := make(map[string]struct{}, len(appliedIDs))
	all := make(map[string]struct{}, len(enabledIDs)+len(appliedIDs))
	for _, id := range enabledIDs {
		enabled[id] = struct{}{}
		all[id] = struct{}{}
	}
	for _, id := range appliedIDs {
		applied[id] = struct{}{}
		all[id] = struct{}{}
	}
	repositoryCount := make(map[string]int)
	primaryRepository := make(map[string]string)
	if cfg != nil {
		for _, repository := range cfg.Repositories {
			all[repository.ProjectID] = struct{}{}
			repositoryCount[repository.ProjectID]++
			if repository.Primary {
				primaryRepository[repository.ProjectID] = repository.ID
			}
		}
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]afclient.DaemonProjectStatus, 0, len(ids))
	for _, id := range ids {
		row := afclient.DaemonProjectStatus{
			ProjectID:           id,
			Desired:             "disabled",
			Applied:             "absent",
			Connection:          "pending",
			RepositoryCount:     repositoryCount[id],
			PrimaryRepositoryID: primaryRepository[id],
		}
		if _, ok := enabled[id]; ok {
			row.Desired = "enabled"
		}
		if _, ok := applied[id]; ok {
			row.Applied = "ready"
			row.Connection = "healthy"
		}
		if _, ok := enabled[id]; ok && row.RepositoryCount == 0 {
			row.Warnings = append(row.Warnings, "no repository resources configured")
		}
		if d != nil && d.State() == StateDraining {
			row.Connection = "draining"
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	cfg := s.daemon.Config()
	enabledProjectIDs := safeEnabledProjectIDs(cfg)
	appliedIDs := appliedProjectIDs(s.daemon, cfg)
	withWorkarea := r.URL.Query().Get("workarea") == "true"
	if !withWorkarea && r.URL.Query().Get("pool") == "true" {
		// Deprecated alias of ?workarea=true, removed in
		// afclient.WorkareaAliasRemovalVersion. Without it a CLI pinned to an
		// older release would silently receive no workarea section rather than
		// an error — the worst failure shape a rename can have.
		withWorkarea = true
		announceDeprecatedSurface(w, "GET /api/daemon/stats?pool=true", "GET /api/daemon/stats?workarea=true")
	}
	byMachine := r.URL.Query().Get("byMachine") == "true"

	resp := afclient.DaemonStatsResponse{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		Capacity: afclient.MachineCapacity{
			MaxConcurrentSessions: safeMaxSessions(cfg),
			MaxVCpuPerSession:     safeMaxVCPU(cfg),
			MaxMemoryMbPerSession: safeMaxMem(cfg),
			ReservedVCpu:          safeReservedVCPU(cfg),
			ReservedMemoryMb:      safeReservedMem(cfg),
		},
		ActiveSessions:    countActive(s.daemon),
		QueueDepth:        0,
		Timestamp:         time.Now().UTC().Format(time.RFC3339),
		WorkerID:          s.daemon.WorkerID(),
		Registration:      buildRegistrationStats(s.daemon),
		AllowedProjects:   safeProjectRepos(cfg),
		EnabledProjectIDs: enabledProjectIDs,
		AppliedProjectIDs: appliedIDs,
	}
	if withWorkarea {
		stats, err := s.workareaStats(r.Context())
		if err == nil {
			// Emits both `workarea` and the deprecated `pool` key; see
			// afclient.DaemonStatsResponse.SetWorkareaStats.
			resp.SetWorkareaStats(stats)
		}
	}
	if byMachine {
		// Single-machine fleet — emit just our own machine entry.
		resp.ByMachine = []afclient.MachineStats{{
			ID:             safeMachineID(cfg),
			Region:         safeRegion(cfg),
			Status:         daemonStatus(s.daemon),
			Version:        s.daemon.EffectiveVersion(),
			ActiveSessions: countActive(s.daemon),
			Capacity:       resp.Capacity,
			UptimeSeconds:  int64(time.Since(s.daemon.StartedAt()).Seconds()),
			LastSeenAt:     time.Now().UTC().Format(time.RFC3339),
		}}
	}
	writeJSON(w, http.StatusOK, &resp)
}

func (s *Server) handlePause(w http.ResponseWriter, _ *http.Request) {
	if !s.daemon.Pause() {
		writeJSON(w, http.StatusConflict, &afclient.DaemonActionResponse{OK: false, Message: fmt.Sprintf("cannot pause while daemon is %s", s.daemon.State())})
		return
	}
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "paused"})
}

func (s *Server) handleResume(w http.ResponseWriter, _ *http.Request) {
	if !s.daemon.Resume() {
		writeJSON(w, http.StatusConflict, &afclient.DaemonActionResponse{OK: false, Message: fmt.Sprintf("cannot resume while daemon is %s", s.daemon.State())})
		return
	}
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "resumed"})
}

func (s *Server) handleStop(w http.ResponseWriter, _ *http.Request) {
	// "stopping" acknowledges transition ownership, not completed shutdown.
	// There is exactly one completion goroutine per daemon terminal generation;
	// duplicate requests observe that owner rather than starting racing retries.
	s.mu.Lock()
	if s.daemon.State() == StateStopped {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "stopped"})
		return
	}
	if s.stopCompletionActive {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "stopping"})
		return
	}
	s.stopCompletionActive = true
	s.stopCompletionStarts++
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "stopping"})
	attemptTimeout, retryDelay := s.stopTimeouts()
	go s.completeStop(attemptTimeout, retryDelay)
}

func (s *Server) completeStop(attemptTimeout, retryDelay time.Duration) {
	// stopCompletionActive intentionally remains set after this routine returns.
	// A daemon has one terminal stop generation; even an unrecoverable attempt
	// must not let a later request create a second completion owner for it.
	for {
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		err := s.daemon.Stop(ctx)
		cancel()
		if s.stopAttemptResults != nil {
			select {
			case s.stopAttemptResults <- err:
			default:
			}
		}
		if err == nil {
			return
		}
		var incomplete *DrainIncompleteError
		switch {
		case errors.As(err, &incomplete):
			slog.Warn("daemon stop incomplete; retaining completion owner for retry", "activeSessions", incomplete.ActiveSessions, "spawnReservations", incomplete.SpawnReservations)
		case !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled):
			slog.Warn("daemon stop failed; daemon remains draining", "err", err)
			return
		default:
			slog.Warn("daemon stop phase exceeded attempt deadline; retaining completion owner for retry", "err", err)
		}
		time.Sleep(retryDelay)
	}
}

func (s *Server) stopTimeouts() (attempt, retry time.Duration) {
	attempt, retry = s.stopAttemptTimeout, s.stopRetryDelay
	if attempt <= 0 {
		attempt = 60 * time.Second
	}
	if retry <= 0 {
		retry = time.Second
	}
	return attempt, retry
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	var body afclient.DaemonDrainRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, &afclient.DaemonActionResponse{OK: false, Message: "invalid drain request: " + err.Error()})
		return
	}
	timeout := time.Duration(body.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		if cfg := s.daemon.Config(); cfg != nil && cfg.AutoUpdate.DrainTimeoutSeconds > 0 {
			timeout = time.Duration(cfg.AutoUpdate.DrainTimeoutSeconds) * time.Second
		} else {
			timeout = 30 * time.Second
		}
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + drainResponseWriteGrace))
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	if err := s.daemon.Drain(ctx); err != nil {
		writeJSON(w, http.StatusConflict, &afclient.DaemonActionResponse{OK: false, Message: "drain incomplete: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: fmt.Sprintf("drained (timeout %s)", timeout)})
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
	n, err := strconv.Atoi(body.Value)
	if err != nil || n < 0 {
		writeJSON(w, http.StatusBadRequest, &afclient.SetCapacityResponse{
			OK: false, Key: body.Key, Value: body.Value, Message: "value must be non-negative integer",
		})
		return
	}

	key := body.Key
	if key == afclient.LegacyWorkareaMaxDiskGbKey {
		// Deprecated alias of capacity.workareaMaxDiskGb, removed in
		// afclient.WorkareaAliasRemovalVersion.
		announceDeprecatedSurface(w, afclient.LegacyWorkareaMaxDiskGbKey, afclient.WorkareaMaxDiskGbKey)
		key = afclient.WorkareaMaxDiskGbKey
	}

	switch key {
	case afclient.WorkareaMaxDiskGbKey:
		// Persist to in-memory config; the CLI also writes daemon.yaml directly.
		s.daemon.mu.Lock()
		if s.daemon.config != nil {
			s.daemon.config.Capacity.PoolMaxDiskGb = n
		}
		s.daemon.mu.Unlock()
	case "capacity.maxConcurrentSessions":
		s.daemon.mu.Lock()
		if s.daemon.config != nil {
			s.daemon.config.Capacity.MaxConcurrentSessions = n
		}
		spawner := s.daemon.spawner
		s.daemon.mu.Unlock()
		if spawner != nil {
			if err := spawner.SetMaxConcurrentSessions(n); err != nil {
				writeJSON(w, http.StatusBadRequest, &afclient.SetCapacityResponse{
					OK: false, Key: body.Key, Value: body.Value, Message: err.Error(),
				})
				return
			}
		}
	default:
		writeJSON(w, http.StatusBadRequest, &afclient.SetCapacityResponse{
			OK: false, Key: body.Key, Value: body.Value, Message: "unknown key",
		})
		return
	}

	writeJSON(w, http.StatusOK, &afclient.SetCapacityResponse{
		OK: true, Key: body.Key, Value: body.Value, Message: "applied",
	})
}

func (s *Server) handleWorkareaStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.workareaStats(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, &afclient.WorkareaPoolStats{
			Members:   []afclient.WorkareaPoolMember{},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleWorkareaEvict(w http.ResponseWriter, r *http.Request) {
	if s.daemon.opts.EvictHandler == nil {
		writeJSON(w, http.StatusNotImplemented, &afclient.EvictPoolResponse{
			Evicted: 0,
			Message: "workarea eviction handler not wired (WorkareaProvider wiring pending)",
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

// handleSessionSubroute multiplexes the per-session paths under the
// /api/daemon/sessions/ prefix:
//
//	GET  /api/daemon/sessions/<id>       → handleSessionDetail
//	POST /api/daemon/sessions/<id>/stop  → handleSessionStop
//
// A single prefix handler is used because the stdlib mux only supports
// prefix matching pre-Go 1.22 in this codebase. The path tail is parsed
// here so each leaf handler sees a clean session id.
func (s *Server) handleSessionSubroute(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/daemon/sessions/"
	tail := strings.TrimPrefix(r.URL.Path, prefix)
	if id, ok := strings.CutSuffix(tail, "/stop"); ok {
		s.handleSessionStop(w, r, id)
		return
	}
	s.handleSessionDetail(w, r, tail)
}

// handleSessionDetail handles GET /api/daemon/sessions/<id> — the
// detail endpoint a spawned `donmai agent run` process reads on startup
// to recover its full QueuedWork shape. Localhost-only (the daemon
// binds to 127.0.0.1); 404s on unknown ids; 405s on non-GET methods.
//
// (F.2.8 — daemon wire-up.)
func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	detail, ok := s.daemon.SessionDetail(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":     "session not found",
			"sessionId": id,
		})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleSessionStop handles POST /api/daemon/sessions/<id>/stop — the
// deterministic per-session cancel route (Guard 3 hard out-of-band leg). A 200
// acknowledges an exact generation still owned by the daemon, including cleanup
// in progress; capacity is released asynchronously only after process-group
// reaping and synchronous Ended delivery. A 404 means the spawner is
// uninitialised, the ID was never present, or its generation was already
// released. The established JSON response shape and "stopped" message remain
// unchanged for wire compatibility. Returns 405 on non-POST methods.
// Localhost-only (the daemon binds to 127.0.0.1).
func (s *Server) handleSessionStop(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if !s.daemon.StopSession(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":     "session not found",
			"sessionId": id,
		})
		return
	}
	writeJSON(w, http.StatusOK, &afclient.DaemonActionResponse{OK: true, Message: "stopped"})
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

func (s *Server) workareaStats(ctx context.Context) (*afclient.WorkareaPoolStats, error) {
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

// daemonStatus is the source of truth for status reporting. It composes
// `mapState(d.State())` with the spawner's `IsAccepting()` flag so the
// reported status can never claim `ready` while the spawner is silently
// NACKing every claim with "not accepting new work (paused or draining)".
//
// The two sources can diverge because Drain() flips spawner.accepting
// directly without going through Daemon.Pause(); a paused-then-resumed
// state restoration that forgets to resume the spawner used to leave
// `d.state == Running` and `spawner.accepting == false` indefinitely
// (precedent: post-auto-update on a 24h-uptime daemon, May 2026).
func daemonStatus(d *Daemon) afclient.DaemonStatus {
	state := mapState(d.State())
	if state != afclient.DaemonReady {
		return state
	}
	if d != nil && d.spawner != nil && !d.spawner.IsAccepting() {
		// Spawner won't take work — the closest honest state is "paused".
		// Don't synthesise a new status code; reuse the existing one
		// callers already know how to render.
		return afclient.DaemonPaused
	}
	return state
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
	return len(c.EffectiveEnabledProjectIDs())
}

func safeEnabledProjectIDs(c *Config) []string {
	if c == nil {
		return nil
	}
	return c.EffectiveEnabledProjectIDs()
}

func appliedProjectIDs(d *Daemon, c *Config) []string {
	if d != nil && d.spawner != nil {
		return d.spawner.AllEnabledProjectIDs()
	}
	if c == nil {
		return nil
	}
	return c.EffectiveEnabledProjectIDs()
}

func safeOrchestratorURL(c *Config) string {
	if c == nil {
		return ""
	}
	return c.Orchestrator.URL
}

// safeProjectRepos returns the list of repository URLs in the project
// allowlist for inclusion in DaemonStatsResponse.AllowedProjects.
func safeProjectRepos(c *Config) []string {
	if c == nil {
		return nil
	}
	projects := c.EffectiveProjectConfigs()
	if len(projects) == 0 {
		return nil
	}
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		out = append(out, p.Repository)
	}
	return out
}

// buildRegistrationStats summarises the daemon's registration / heartbeat /
// poll subsystem state for DaemonStatsResponse. Returns nil when no
// heartbeat has been started (e.g. SkipRegistration mode).
func buildRegistrationStats(d *Daemon) *afclient.DaemonRegistrationStats {
	stats := &afclient.DaemonRegistrationStats{}
	if d == nil {
		return stats
	}
	if d.heartbeat != nil {
		stats.HeartbeatRunning = d.heartbeat.IsRunning()
		last := d.heartbeat.LastPayload()
		stats.LastHeartbeatAt = last.SentAt
		if last.Status != "" {
			stats.Status = string(last.Status)
		}
	}
	if d.poller != nil {
		stats.PollRunning = d.poller.IsRunning()
	}
	if stats.Status == "" {
		// Derive a reasonable fallback so consumers always see something.
		switch d.State() {
		case StateRunning:
			if d.WorkerID() == "" {
				stats.Status = "unregistered"
			} else {
				stats.Status = "idle"
			}
		case StateDraining:
			stats.Status = "draining"
		case StatePaused:
			stats.Status = "paused"
		default:
			stats.Status = string(d.State())
		}
	}
	// Mirror `daemonStatus()`: when the spawner won't accept work, surface
	// "paused" regardless of where the upstream status came from
	// (heartbeat-reported `idle` or `running`, or the fallback above).
	// Same divergence root cause — Drain() flips spawner.accepting
	// directly without going through Daemon.Pause(), and a forgotten
	// Resume after restore-to-Running used to leave operators staring at
	// `idle` while every claim NACKed.
	if d.spawner != nil && !d.spawner.IsAccepting() {
		switch stats.Status {
		case "idle", "busy", "running", string(RegistrationDraining):
			// keep "draining" — it's a more specific true statement.
			if stats.Status != string(RegistrationDraining) {
				stats.Status = "paused"
			}
		}
	}
	return stats
}
