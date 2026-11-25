package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestOutageLongerThanWindowSurvivesAndReattaches pins the interactive-outage
// fix: a control-plane outage longer than the ten-minute window does not
// release an interactive session — one the composing layer holds on terms the
// window does not bound (LineageLive and ReadoptionHeldPastWindow both true) —
// whose shim and harness are still alive. The daemon keeps the lineage
// adopted, retries with capped backoff (see
// DefaultSessionShimReadoptionBackoffCap) for as long as the shim is observable
// and held, and re-attaches when the carrier returns, with no lost transcript.
//
// Fake clock, no real sleeps: the virtual clock advances only on the backoffs
// the hold really asks for, so thirty simulated minutes cost milliseconds.
func TestOutageLongerThanWindowSurvivesAndReattaches(t *testing.T) {
	t.Parallel()
	const (
		window        = 10 * time.Minute
		outage        = 30 * time.Minute
		returnsOn     = 66 // backoffs 1,2,4,8,16,30,30,... cover >30min simulated
		backoff       = time.Second
		backoffCap    = DefaultSessionShimReadoptionBackoffCap
		keepaliveSkip = 1000 * time.Hour // keep the virtual waits to backoffs only
	)
	clock := newVirtualShimClock()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: backoff, BackoffCap: backoffCap,
			Window: window, KeepaliveInterval: keepaliveSkip,
		},
		clock:          clock,
		lineageLive:    alwaysLive,
		heldPastWindow: func(context.Context, sessionshim.Identity) bool { return true },
		adoption: func(_ context.Context, attempt int) error {
			if attempt < returnsOn {
				// The control plane is unreachable: timeouts and 5xx carry no
				// fact about the lineage, so the hold retries through them.
				return errors.New("injected carrier outage: POST adoption preparation timeout after 15s (503)")
			}
			return nil
		},
	})
	previousGeneration := f.controller.Generation()
	started := clock.Now()
	lost := f.lostEntry(t)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionSucceeded {
		t.Fatalf("disposition = %d, want readoptionSucceeded (%d) after the carrier returned past the old window",
			got, readoptionSucceeded)
	}
	if elapsed := clock.elapsed(started); elapsed < outage {
		t.Fatalf("the hold survived %s of simulated time, want at least the %s outage", elapsed, outage)
	}
	adoptions, batches := f.snapshot()
	if adoptions != returnsOn {
		t.Fatalf("durable adoption attempted %d times, want %d — the hold stopped at the old fixed cutoff", adoptions, returnsOn)
	}
	if len(batches) != 1 || len(batches[0].Adopted) != 1 || len(batches[0].Quarantined) != 0 {
		t.Fatalf("published batches = %+v, want exactly one carrying the lineage adopted", batches)
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("quarantine projection = %+v while the shim was still observable, want none", projected)
	}
	entry, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("the lineage left the adopted set: %v", err)
	}
	if entry.controller == f.controller || entry.controller.Generation() <= previousGeneration {
		t.Fatalf("adopted entry still names the lost controller (generation %d)", entry.controller.Generation())
	}
	// No terminal evidence: the shim never reaped, so the harness and its
	// buffered output survived the outage with no transcript loss.
	if _, err := f.registry.GetTombstoneIncarnation(f.id, lost.shimID, lost.controller.Hello().ProcessEpoch); err == nil {
		t.Fatal("a terminal tombstone exists for a lineage that re-attached alive")
	}
	if phase := f.recordPhase(t); phase == "exited" {
		t.Fatalf("discovery record phase = %q after a successful re-attachment", phase)
	}
	if live, err := f.daemon.sessionShimHarnessLive(f.id); err != nil || !live {
		t.Fatalf("harness live = %t, err = %v after re-attachment, want a live harness", live, err)
	}
	// The re-attached controller is usable: the carrier is back, not just the
	// adopted set.
	if err := entry.controller.Heartbeat(0); err != nil {
		t.Fatalf("re-attached controller heartbeat = %v, want nil", err)
	}
}

// TestOutageHoldBackoffCeilingIsConfigurable pins that the hold's backoff
// ceiling is a documented, configurable daemon setting with a visible default.
// The visible default is DefaultSessionShimReadoptionBackoffCap; a deployment
// that configures BackoffCap gets its own ceiling in both modes. A fixed policy
// that configures nothing still RESOLVES to a zero cap — its initial budget
// never reads one, so the resolved default fixed policy is unchanged — and
// the hold past the budget supplies the visible default itself.
func TestOutageHoldBackoffCeilingIsConfigurable(t *testing.T) {
	t.Parallel()
	if DefaultSessionShimReadoptionBackoffCap != 30*time.Second {
		t.Fatalf("DefaultSessionShimReadoptionBackoffCap = %s, want 30s", DefaultSessionShimReadoptionBackoffCap)
	}
	for _, tc := range []struct {
		name   string
		policy SessionShimReadoptionPolicy
		want   time.Duration
	}{
		{name: "lineage-live zero resolves to the visible default", policy: SessionShimReadoptionPolicy{Mode: ReadoptionLineageLive}, want: DefaultSessionShimReadoptionBackoffCap},
		{name: "fixed zero resolves to the visible default", policy: SessionShimReadoptionPolicy{}, want: DefaultSessionShimReadoptionBackoffCap},
		{name: "lineage-live configured wins", policy: SessionShimReadoptionPolicy{Mode: ReadoptionLineageLive, BackoffCap: 4 * time.Second}, want: 4 * time.Second},
		{name: "fixed configured wins", policy: SessionShimReadoptionPolicy{BackoffCap: 7 * time.Second}, want: 7 * time.Second},
	} {
		cfg := SessionShimConfig{Readoption: tc.policy}
		if got := sessionShimHoldBackoffCap(cfg.readoption()); got != tc.want {
			t.Errorf("%s: hold backoff cap = %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := (SessionShimConfig{}).readoption().BackoffCap; got != 0 {
		t.Fatalf("the default fixed policy resolved BackoffCap = %s, want 0 — a configuration that does not opt in must resolve as before", got)
	}
}

// TestOutageWithdrawalNamesHarnessLiveness pins that when a session does end
// on carrier loss, its terminal record says whether the harness was still alive.
// Ending the hold via the composing layer (LineageLive false) with the shim
// still observable withdraws with the harness-alive suffix, not as a death.
func TestOutageWithdrawalNamesHarnessLiveness(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy:   SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
		adoption: refusingCarrier(),
		lineageLive: func(context.Context, sessionshim.Identity) bool {
			return false
		},
	})
	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	projected := f.daemon.QuarantinedSessions()
	if len(projected) != 1 {
		t.Fatalf("quarantine projection = %+v, want exactly the lineage", projected)
	}
	detail := projected[0].Detail
	if detail == "" || !strings.Contains(detail, "harness alive at release:") {
		t.Fatalf("quarantine detail = %q, want it to say whether the harness was alive", detail)
	}
	if !strings.Contains(detail, "true") {
		t.Fatalf("quarantine detail = %q, want harness alive=true for an observable shim", detail)
	}
}
