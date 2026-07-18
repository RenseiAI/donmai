package attachclient

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Defaults for the optional HostConfig knobs.
const (
	defaultBackoffFloor      = 250 * time.Millisecond
	defaultBackoffCeiling    = 30 * time.Second
	defaultFallbackAfterN    = 3
	defaultFinalScreenWindow = 60 * time.Second
	defaultUpgradeProbeEvery = 30 * time.Second
	defaultReadLimitBytes    = 16 << 20 // 16 MiB — Output/Snapshot frames exceed the 32 KiB ws default
	defaultDedupWindow       = 4096
	defaultDialTimeout       = 15 * time.Second
)

// HostConfig configures RunHost. AttachURL, TokenSource and Session are
// required; every other field defaults when zero.
type HostConfig struct {
	// AttachURL is the relay attach endpoint, ending in the versioned room path
	// (…/v1/rooms/<roomId>). wss:// is normal; ws:// is allowed for local dev and
	// tests. The degraded-lane HTTPS endpoints are derived mechanically from it
	// (§ 14): wss→https / ws→http, plus the /host/sse and /host/output suffixes.
	AttachURL string

	// TokenSource yields the current bearer JWT, called once per dial attempt
	// (§ 15).
	TokenSource TokenSource

	// Session is the live PTY surface (structurally == agent.InteractiveSession).
	Session Session

	// Logger receives structured logs. Optional; nil is quiet (discard).
	Logger *slog.Logger

	// Kill is the process-group termination hook (see KillFunc). Optional: a nil
	// Kill logs the relay's kill request and no-ops; the Session's own teardown
	// still drives Exit if the process ends by other means.
	Kill KillFunc

	// HTTPClient dials the WSS handshake and the degraded lane. Optional; nil
	// uses http.DefaultClient. Do NOT set a client-level Timeout — the SSE-down
	// leg is long-lived; per-attempt bounds come from context instead.
	HTTPClient *http.Client

	// BackoffMin/BackoffMax bound the reconnect backoff (equal-jitter, reset on
	// success). Defaults 250ms / 30s.
	BackoffMin time.Duration
	BackoffMax time.Duration

	// DisableDegraded turns off the § 14 degraded lane entirely (WSS-only). When
	// false (default) the client falls back to the degraded lane after
	// FallbackAfterN consecutive dial failures.
	DisableDegraded bool

	// FallbackAfterN is the consecutive dial-failure count that triggers the
	// degraded lane (§ 14 trigger-down). Default 3.
	FallbackAfterN int

	// FinalScreenWindow is how long the host leg stays attached after Exit to
	// answer snapshot_request with the post-Exit final screen (§ 12.2). Default
	// 60s.
	FinalScreenWindow time.Duration

	// UpgradeProbeInterval is the degraded→WSS re-dial cadence (§ 14
	// upgrade-back). Default 30s.
	UpgradeProbeInterval time.Duration

	// ReadLimitBytes is the WebSocket per-message read limit. Default 16 MiB.
	ReadLimitBytes int64

	// DedupWindow bounds the degraded-lane at-least-once Input dedup recency set
	// (§ 14). Default 4096 keys.
	DedupWindow int

	// DialTimeout bounds a single WSS handshake / degraded-lane HTTP attempt.
	// Default 15s.
	DialTimeout time.Duration

	// now is an injectable clock seam for tests. nil uses time.Now.
	now func() time.Time
}

func (c *HostConfig) withDefaults() error {
	if c.AttachURL == "" {
		return fmt.Errorf("attachclient: HostConfig.AttachURL is required")
	}
	if c.TokenSource == nil {
		return fmt.Errorf("attachclient: HostConfig.TokenSource is required")
	}
	if c.Session == nil {
		return fmt.Errorf("attachclient: HostConfig.Session is required")
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = defaultBackoffFloor
	}
	if c.BackoffMax < c.BackoffMin {
		c.BackoffMax = defaultBackoffCeiling
		if c.BackoffMax < c.BackoffMin {
			c.BackoffMax = c.BackoffMin
		}
	}
	if c.FallbackAfterN <= 0 {
		c.FallbackAfterN = defaultFallbackAfterN
	}
	if c.FinalScreenWindow <= 0 {
		c.FinalScreenWindow = defaultFinalScreenWindow
	}
	if c.UpgradeProbeInterval <= 0 {
		c.UpgradeProbeInterval = defaultUpgradeProbeEvery
	}
	if c.ReadLimitBytes <= 0 {
		c.ReadLimitBytes = defaultReadLimitBytes
	}
	if c.DedupWindow <= 0 {
		c.DedupWindow = defaultDedupWindow
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = defaultDialTimeout
	}
	return nil
}
