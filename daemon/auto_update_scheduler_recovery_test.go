package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// expiringVerifier models a bundle download that outlives the restart hold:
// the first verification advances the daemon clock past the fence expiry,
// later verifications leave the clock alone so a retry can succeed.
type expiringVerifier struct {
	advance func()
	fired   atomic.Bool
}

func (v *expiringVerifier) Verify(_ context.Context, _, _ string) (bool, string) {
	if v.advance != nil && v.fired.CompareAndSwap(false, true) {
		v.advance()
	}
	return true, "test"
}

// An expired post-swap permission must not strand the host: the first tick
// reuses a cached preparation with two seconds left, the production download
// (modelled as a clock advance inside verification) expires the hold before
// the swap, and the handoff must refuse the restart while leaving a renewal
// path. Drives the production entry point (scheduler tick → updateWithConfig
// → RunUpdate with a real HTTP swap to a temp binary), not a stub.
func TestAutoUpdateSchedulerExpiredHandoffRecoversThroughProductionSwap(t *testing.T) {
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
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "test-bundle")
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, Scheduled: true, DrainTimeoutSeconds: 1,
		CDNBase: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	current := filepath.Join(t.TempDir(), "donmai-daemon")
	if err := os.WriteFile(current, []byte("old-binary"), 0o600); err != nil {
		t.Fatalf("write current binary: %v", err)
	}
	d.autoUpdateBinaryPath = current
	if d.runPreparedUpdate != nil {
		t.Fatal("runPreparedUpdate must stay nil to exercise the production updater branch")
	}
	if _, err := d.PrepareRestart(context.Background()); err != nil {
		t.Fatalf("initial PrepareRestart: %v", err)
	}
	holdUntil := time.Unix(0, d.shims.restart.acked["org"].HoldUntilUnixNano)
	// Reuse the cached preparation with two seconds of hold left.
	now = holdUntil.Add(-2 * time.Second)
	verifier := &expiringVerifier{}
	verifier.advance = func() {
		// The binary download outlives the remaining hold.
		now = holdUntil.Add(time.Second)
	}
	d.autoUpdateVerifier = verifier
	var restarts atomic.Int32
	d.autoUpdateRestartHook = func() { restarts.Add(1) }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	s.ctx = ctx
	s.attemptOnce()
	if got := restarts.Load(); got != 0 {
		t.Fatalf("first tick restart requests = %d, want 0 (expired handoff must not restart)", got)
	}
	if s.applied.Load() {
		t.Fatal("applied latched after expired handoff: a later tick could never retry")
	}
	if d.updateSwapApplied.Load() {
		t.Fatal("swap fence latched after expired handoff: Resume would refuse with no restart owner")
	}
	d.shims.mu.RLock()
	prep := d.shims.restart
	d.shims.mu.RUnlock()
	if prep == nil || prep.state != restartPreparationAbandoned {
		state := "<nil>"
		if prep != nil {
			state = string(prep.state)
		}
		t.Fatalf("stale preparation state = %q, want abandoned so a later tick mints a fresh permission", state)
	}
	if got := d.State(); got != StateDraining {
		t.Fatalf("state after expired handoff = %q, want draining (fail closed)", got)
	}
	swapped, err := os.ReadFile(current) //nolint:gosec
	if err != nil {
		t.Fatalf("read swapped binary: %v", err)
	}
	if string(swapped) != "new-binary" {
		t.Fatalf("swapped binary = %q, want probe binary (production swap never applied)", swapped)
	}
	// Automatic retry with a fresh permission requests exactly one restart.
	// The clock already reads past the old hold, so the next tick mints a
	// fresh preparation; the verifier fires only once, so the retry holds.
	s.attemptOnce()
	if got := restarts.Load(); got != 1 {
		t.Fatalf("second tick restart requests = %d, want 1 (fresh permission must recover)", got)
	}
	if !s.applied.Load() {
		t.Fatal("applied not latched after recovered retry: second tick would re-apply")
	}
	if !d.updateSwapApplied.Load() {
		t.Fatal("swap fence not latched after recovered retry: Resume must refuse once a restart is owned")
	}
	if err := d.ResumeContext(context.Background()); err == nil {
		t.Fatal("resume accepted after recovered swap: restart owns completion, not Resume")
	}
}

// The same expired handoff must also leave explicit recovery usable: after
// the production swap abandons the stale preparation and clears the fence,
// a fresh PrepareRestart mints a new permission and Resume reopens admission.
func TestAutoUpdateSchedulerExpiredHandoffLeavesExplicitRecovery(t *testing.T) {
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
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "test-bundle")
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, Scheduled: true, DrainTimeoutSeconds: 1,
		CDNBase: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	current := filepath.Join(t.TempDir(), "donmai-daemon")
	if err := os.WriteFile(current, []byte("old-binary"), 0o600); err != nil {
		t.Fatalf("write current binary: %v", err)
	}
	d.autoUpdateBinaryPath = current
	if d.runPreparedUpdate != nil {
		t.Fatal("runPreparedUpdate must stay nil to exercise the production updater branch")
	}
	if _, err := d.PrepareRestart(context.Background()); err != nil {
		t.Fatalf("initial PrepareRestart: %v", err)
	}
	holdUntil := time.Unix(0, d.shims.restart.acked["org"].HoldUntilUnixNano)
	now = holdUntil.Add(-2 * time.Second)
	verifier := &expiringVerifier{}
	verifier.advance = func() { now = holdUntil.Add(time.Second) }
	d.autoUpdateVerifier = verifier
	var restarts atomic.Int32
	d.autoUpdateRestartHook = func() { restarts.Add(1) }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	s.ctx = ctx
	s.attemptOnce()
	if got := restarts.Load(); got != 0 {
		t.Fatalf("first tick restart requests = %d, want 0", got)
	}
	if d.updateSwapApplied.Load() {
		t.Fatal("swap fence latched after expired handoff: explicit Resume could never proceed")
	}
	if _, err := d.PrepareRestart(context.Background()); err != nil {
		t.Fatalf("explicit PrepareRestart after expired handoff: %v (want a fresh permission)", err)
	}
	if err := d.ResumeContext(context.Background()); err != nil {
		t.Fatalf("explicit Resume after expired handoff: %v (want reopened admission)", err)
	}
	if got := d.State(); got != StateRunning {
		t.Fatalf("state after explicit resume = %q, want running", got)
	}
}
