package attachclient

import (
	"context"
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
	defaultEpochStaleRetries = 8
	defaultEpochStaleWindow  = 2 * time.Minute
	defaultEpochStableWindow = 30 * time.Second
	defaultRingMissCeiling   = 60 * time.Second
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

	// InitialAuthorityToken is the immutable bearer supplied when this local
	// PTY process was spawned. It is parsed only to bind the expected session
	// and process epoch; reconnect transport still uses TokenSource. It is
	// required when Session.Snapshot reports the valid legacy epoch zero, where
	// the session surface alone cannot distinguish unstamped compatibility from
	// the exact spawn authority.
	InitialAuthorityToken string

	// TokenSource yields the current bearer JWT. It is resolved before each
	// top-level carrier attempt and may also be called concurrently by degraded-
	// lane 401 recovery and the background WSS upgrade probe (§ 14/§ 15). Every
	// token epoch is checked against Session.Snapshot's immutable local PTY
	// epoch and InitialAuthorityToken before use. A later higher token belongs to
	// a successor process and is never applied to this Session.
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

	// EpochStaleMaxRetries and EpochStaleRetryWindow jointly bound recovery
	// when the relay rejects the current PTY epoch while its previous carrier
	// may still be half-open. Exhausting either returns ErrEpochStale. Defaults
	// to 8 retries within 2 minutes.
	EpochStaleMaxRetries  int
	EpochStaleRetryWindow time.Duration

	// EpochStaleStableWindow is the minimum duration of a subsequently admitted
	// carrier attempt before an older stale-retry budget is cleared. Short
	// interleaved network failures do not reset the budget to its floor. Default
	// 30 seconds.
	EpochStaleStableWindow time.Duration

	// RingMissRetryCeiling bounds the reconnect backoff used after a §13
	// ring-miss reset (the relay — or our own retained ring — lost history,
	// most commonly a relay restart). It shares BackoffMin as its floor but
	// deliberately gets its own, slower ceiling: unlike a transient dial/
	// network failure, this loop NEVER gives up (see the ring-miss case in
	// host.run) — it just settles into this steady cadence and keeps trying,
	// because a relay restart is rare but multi-bounce windows happen, and the
	// session should reacquire its view whenever the relay returns however
	// late. Default 60s.
	RingMissRetryCeiling time.Duration

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
	// epochStaleSleep observes stale-retry delays in package tests. Production
	// leaves it nil and uses sleepCtx.
	epochStaleSleep func(context.Context, time.Duration) error
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
	if c.EpochStaleMaxRetries <= 0 {
		c.EpochStaleMaxRetries = defaultEpochStaleRetries
	}
	if c.EpochStaleRetryWindow <= 0 {
		c.EpochStaleRetryWindow = defaultEpochStaleWindow
	}
	if c.EpochStaleStableWindow <= 0 {
		c.EpochStaleStableWindow = defaultEpochStableWindow
	}
	if c.RingMissRetryCeiling < c.BackoffMin {
		c.RingMissRetryCeiling = defaultRingMissCeiling
		if c.RingMissRetryCeiling < c.BackoffMin {
			c.RingMissRetryCeiling = c.BackoffMin
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
