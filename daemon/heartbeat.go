package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// HeartbeatOptions configure a HeartbeatService.
type HeartbeatOptions struct {
	WorkerID        string
	Hostname        string
	OrchestratorURL string
	RuntimeJWT      string
	IntervalSeconds int
	GetActiveCount  func() int
	GetMaxCount     func() int
	GetStatus       func() RegistrationStatus
	Region          string

	// HTTPClient is the client used for the optional real-endpoint call.
	HTTPClient *http.Client
	// LogWarn is called when the optional real-endpoint call fails. The
	// daemon's slog handler is the typical wiring; in tests it is a no-op.
	LogWarn func(format string, args ...any)
	// Now provides the heartbeat sentAt timestamp.
	Now func() time.Time
	// OnHeartbeat is invoked after each heartbeat is composed (whether or
	// not the network call succeeded). Used by tests and observability.
	OnHeartbeat func(payload HeartbeatPayload)
}

// HeartbeatService manages the periodic heartbeat goroutine. It is safe to
// Start / Stop multiple times; consecutive Starts are idempotent.
type HeartbeatService struct {
	opts HeartbeatOptions

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	last    HeartbeatPayload
}

// NewHeartbeatService constructs a HeartbeatService from opts. Required
// callbacks are GetActiveCount, GetMaxCount, and GetStatus.
func NewHeartbeatService(opts HeartbeatOptions) *HeartbeatService {
	if opts.IntervalSeconds <= 0 {
		opts.IntervalSeconds = int(HeartbeatDefaultInterval / time.Second)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LogWarn == nil {
		opts.LogWarn = func(string, ...any) {}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &HeartbeatService{opts: opts}
}

// Start launches the heartbeat goroutine. It sends an immediate heartbeat,
// then continues at IntervalSeconds. Subsequent calls are no-ops.
func (h *HeartbeatService) Start() {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.running = true
	h.mu.Unlock()

	go h.loop(ctx)
}

// Stop terminates the heartbeat goroutine. Safe to call multiple times.
func (h *HeartbeatService) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.running {
		return
	}
	if h.cancel != nil {
		h.cancel()
	}
	h.running = false
}

// IsRunning reports whether the heartbeat goroutine is active.
func (h *HeartbeatService) IsRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

// LastPayload returns the most recently composed heartbeat payload (for
// debugging / status surfaces).
func (h *HeartbeatService) LastPayload() HeartbeatPayload {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

func (h *HeartbeatService) loop(ctx context.Context) {
	// Immediate first heartbeat.
	h.sendOne(ctx)

	tick := time.NewTicker(time.Duration(h.opts.IntervalSeconds) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.sendOne(ctx)
		}
	}
}

func (h *HeartbeatService) sendOne(ctx context.Context) {
	payload := HeartbeatPayload{
		WorkerID:       h.opts.WorkerID,
		Hostname:       h.opts.Hostname,
		Status:         h.opts.GetStatus(),
		ActiveSessions: h.opts.GetActiveCount(),
		MaxSessions:    h.opts.GetMaxCount(),
		Region:         h.opts.Region,
		SentAt:         h.opts.Now().UTC().Format(time.RFC3339),
	}

	h.mu.Lock()
	h.last = payload
	h.mu.Unlock()

	if h.opts.OnHeartbeat != nil {
		h.opts.OnHeartbeat(payload)
	}

	// Optional real-endpoint call.
	if os.Getenv("RENSEI_DAEMON_REAL_REGISTRATION") == "" {
		return
	}
	if err := h.callEndpoint(ctx, payload); err != nil {
		h.opts.LogWarn("daemon heartbeat HTTP call failed: %v — orchestrator will detect via missed heartbeats", err)
	}
}

func (h *HeartbeatService) callEndpoint(ctx context.Context, payload HeartbeatPayload) error {
	url := strings.TrimRight(h.opts.OrchestratorURL, "/") + "/v1/daemon/heartbeat"
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.opts.RuntimeJWT)
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)
	res, err := h.opts.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}
