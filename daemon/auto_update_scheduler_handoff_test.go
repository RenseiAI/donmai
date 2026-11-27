package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// The scheduled restart handoff must revalidate the retained restart
// permission after the bundle download: a preparation that was valid at
// tick start can expire while the download runs. Requesting process exit
// on that expired permission would restart without authorization.
func TestAutoUpdateSchedulerHandoffRefusesExpiredFreshPermission(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	now := base
	store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		return sessionshim.FenceAcknowledgement{
			RequestBytes:    append([]byte(nil), request.RequestBytes...),
			DurableRevision: "revision",
		}, nil
	})
	d := newRestartTestDaemon(t, store, sessionshim.Identity{OrgID: "org", SessionID: "session"})
	d.shims.restartNow = func() time.Time { return now }
	d.opts.Version = "0.1.0"
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, Scheduled: true, DrainTimeoutSeconds: 1,
		CDNBase: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	var restarts atomic.Int32
	d.autoUpdateRestartHook = func() { restarts.Add(1) }
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		// Simulate a bundle download that outlives the 2-minute hold
		// window: the handoff below must see the expiry.
		now = base.Add(24 * time.Hour)
		return &UpdateResult{Updated: true, Version: "9.9.9", Reason: "update-applied"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	s.ctx = ctx
	s.attemptOnce()
	if got := restarts.Load(); got != 0 {
		t.Fatalf("restart requests after expired handoff = %d, want 0 (no exit without valid permission)", got)
	}
	if s.applied.Load() {
		t.Fatal("applied latched after expired handoff: a later tick could never retry with a fresh permission")
	}
	if got := d.State(); got != StateDraining {
		t.Fatalf("state after expired handoff = %q, want draining (fail closed, no resume)", got)
	}
	if err := d.validateAutoUpdateRestartHandoff(); err == nil {
		t.Fatal("handoff revalidation after expiry = nil, want expired-hold error")
	}
}

// A cached preparation reused by a later tick must face the same handoff
// check: an operator leaves the host prepared and draining, the next tick
// reuses that preparation just before its fence expires, and the download
// outlives the remaining hold. The handoff must still refuse the restart.
func TestAutoUpdateSchedulerHandoffRefusesExpiredCachedPermission(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	now := base
	store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		return sessionshim.FenceAcknowledgement{
			RequestBytes:    append([]byte(nil), request.RequestBytes...),
			DurableRevision: "revision",
		}, nil
	})
	d := newRestartTestDaemon(t, store, sessionshim.Identity{OrgID: "org", SessionID: "session"})
	d.shims.restartNow = func() time.Time { return now }
	d.opts.Version = "0.1.0"
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, Scheduled: true, DrainTimeoutSeconds: 1,
		CDNBase: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	first, err := d.PrepareRestart(context.Background())
	if err != nil {
		t.Fatalf("initial PrepareRestart: %v", err)
	}
	holdUntil := time.Unix(0, d.shims.restart.acked["org"].HoldUntilUnixNano)
	// Reuse the cached preparation shortly before its fence expires.
	now = holdUntil.Add(-10 * time.Second)
	var restarts atomic.Int32
	d.autoUpdateRestartHook = func() { restarts.Add(1) }
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		// The download outlives the remaining 10-second hold.
		now = holdUntil.Add(time.Second)
		return &UpdateResult{Updated: true, Version: "9.9.9", Reason: "update-applied"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	s.ctx = ctx
	s.attemptOnce()
	if got := restarts.Load(); got != 0 {
		t.Fatalf("restart requests after expired cached handoff = %d, want 0 (preparation %q expired during download)", got, first.PreparationID)
	}
	if s.applied.Load() {
		t.Fatal("applied latched after expired cached handoff: retry with a fresh permission is blocked")
	}
	if got := d.State(); got != StateDraining {
		t.Fatalf("state after expired cached handoff = %q, want draining", got)
	}
}

// A held permission still authorizes the handoff: the same path that
// refuses an expired hold must request exactly one restart while the hold
// remains valid, or the revalidation over-blocks ordinary short downloads.
func TestAutoUpdateSchedulerHandoffRestartsWhileHoldRemainsValid(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	now := base
	store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		return sessionshim.FenceAcknowledgement{
			RequestBytes:    append([]byte(nil), request.RequestBytes...),
			DurableRevision: "revision",
		}, nil
	})
	d := newRestartTestDaemon(t, store, sessionshim.Identity{OrgID: "org", SessionID: "session"})
	d.shims.restartNow = func() time.Time { return now }
	d.opts.Version = "0.1.0"
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, Scheduled: true, DrainTimeoutSeconds: 1,
		CDNBase: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	var restarts atomic.Int32
	d.autoUpdateRestartHook = func() { restarts.Add(1) }
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		// A fast download well inside the hold window.
		now = base.Add(time.Second)
		return &UpdateResult{Updated: true, Version: "9.9.9", Reason: "update-applied"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	s.ctx = ctx
	s.attemptOnce()
	if got := restarts.Load(); got != 1 {
		t.Fatalf("restart requests with valid hold = %d, want exactly 1", got)
	}
	if !s.applied.Load() {
		t.Fatal("applied not latched after valid handoff: second tick would re-apply the same release")
	}
}
