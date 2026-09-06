package daemon

// The daemon half of the planned-restart contract. See
// session_shim_adoption_redial.go's "THE SECOND STRAND" for what it undoes: a
// startup composition that ran inside a relay's drain window quarantined every
// lineage it was composing at that instant, because a 503 that says "come back
// in five seconds" was classified exactly like a lineage that refused.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/sessionshim"
)

// drainingRelay is a fake relay in its planned-restart drain: it answers every
// attach dial with 503 + Retry-After until the replacement is up. It is a real
// HTTP server answering real responses, and the refusal the adoption callback
// returns is classified by the SAME production function the attach client's
// dial path runs — so this test cannot drift from what a real dial produces.
type drainingRelay struct {
	server      *httptest.Server
	redialAfter int

	mu     sync.Mutex
	dials  int
	drains int
}

func newDrainingRelay(t *testing.T, drains, redialAfter int) *drainingRelay {
	t.Helper()
	relay := &drainingRelay{drains: drains, redialAfter: redialAfter}
	relay.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		relay.mu.Lock()
		relay.dials++
		draining := relay.drains == 0 || relay.dials <= relay.drains
		relay.mu.Unlock()
		if !draining {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Retry-After", fmt.Sprint(relay.redialAfter))
		http.Error(w, "relay-restarting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(relay.server.Close)
	return relay
}

// dial performs one attach dial and returns the refusal a composing carrier
// would hand back, or nil once the replacement admits it.
func (r *drainingRelay) dial() error {
	resp, err := http.Get(r.server.URL) //nolint:noctx // loopback fixture
	if err != nil {
		return fmt.Errorf("dial the carrier: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if restart := attachclient.RelayRestartRefusal(resp); restart != nil {
		// Exactly the production shape: the attach client's typed refusal,
		// wrapped by the composing caller with %w.
		return fmt.Errorf("dial fresh v2 candidate: %w", restart)
	}
	return nil
}

func (r *drainingRelay) dialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dials
}

// restartingAdoption is a composing authority whose durable-adoption callback
// dials the draining relay each time it is asked.
type restartingAdoption struct {
	relay *drainingRelay

	mu    sync.Mutex
	asks  []preparedAsk
	dials int
}

func (a *restartingAdoption) prepare(_ context.Context, in SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = append(a.asks, preparedAsk{cause: in.Cause, attempt: in.Attempt, generation: in.CurrentControllerGeneration})
	return sessionshim.PreparedAdoption{Correlation: []byte("candidate-1")}, nil
}

func (a *restartingAdoption) adopt(_ context.Context, _ SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
	a.mu.Lock()
	a.dials++
	a.mu.Unlock()
	if err := a.relay.dial(); err != nil {
		return SessionShimAdoptionReceipt{}, err
	}
	return SessionShimAdoptionReceipt{DurableCorrelation: []byte("committed-after-the-restart")}, nil
}

func (a *restartingAdoption) snapshot() ([]preparedAsk, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]preparedAsk(nil), a.asks...), a.dials
}

func newRestartRedialDaemon(
	t *testing.T,
	registry, orgID string,
	adoption *restartingAdoption,
	batches *[]SessionShimAdoptionBatch,
	batchMu *sync.Mutex,
) *Daemon {
	t.Helper()
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:  true,
			RegistryDir:     registry,
			HostID:          "host-restart-redial",
			OrgID:           orgID,
			PrepareAdoption: adoption.prepare,
			OnAdoption:      adoption.adopt,
			// The bound this pass spends is the CONFIGURED re-adoption policy's,
			// so a fixture sets it here rather than reaching for a constant the
			// production path does not read. The local backoff is small; the
			// waits these tests actually spend are the relay's own 1s floor,
			// which is the point — the floor is honoured, not undercut.
			Readoption: SessionShimReadoptionPolicy{
				Mode: ReadoptionFixedAttempts, Attempts: 3, Backoff: 5 * time.Millisecond,
			},
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				batchMu.Lock()
				*batches = append(*batches, cloneSessionShimAdoptionBatch(batch))
				batchMu.Unlock()
				return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("batch-restart-redial")}, nil
			},
		},
	})
	t.Cleanup(d.ReleaseAdoptedSessionShims)
	return d
}

// TestStartupCompositionRedialsARestartingRelayBeforeQuarantining is the fix.
//
// A relay draining for a planned restart refuses the first dials with 503 +
// Retry-After and admits the next one. The lineage's harness is alive
// throughout and its carrier proof was never even read, so the pass must
// re-dial and adopt it — not condemn it to a quarantine that renews no orphan
// clock and ends in the shim reaping its own healthy harness.
//
// It also pins that a re-dial is a re-DIAL: the proof is NOT re-prepared, so
// the authority is asked exactly once and no reservation is superseded for a
// refusal nobody read.
func TestStartupCompositionRedialsARestartingRelayBeforeQuarantining(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-restart-redial"
	spec := launchOneAdoptableLineage(t, f, orgID, "lineage-restarting")

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	// Two refusals, then the replacement admits it — inside the three dials the
	// configured policy allows.
	adoption := &restartingAdoption{relay: newDrainingRelay(t, 2, 1)}
	replacement := newRestartRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup composition returned a host-wide failure: %v", err)
	}

	asks, dials := adoption.snapshot()
	if dials != 3 {
		t.Fatalf("durable adoption dials = %d, want 3 (two drain-window refusals, then the admitted dial)", dials)
	}
	if adoption.relay.dialCount() != 3 {
		t.Fatalf("relay saw %d dials, want 3", adoption.relay.dialCount())
	}
	if len(asks) != 1 || asks[0].cause != SessionShimPrepareCauseInitial {
		t.Fatalf("preparation asks = %+v, want exactly the initial one — a refusal nobody read supersedes no reservation", asks)
	}
	if _, err := replacement.adoptedShimEntry(orgID, spec.SessionID); err != nil {
		t.Fatalf("the lineage was quarantined over a relay that was restarting: %v", err)
	}
	for _, q := range replacement.QuarantinedSessions() {
		if q.SessionID == spec.SessionID {
			t.Fatalf("the re-dialled lineage is still surfaced as quarantined (%s: %s)", q.Reason, q.Detail)
		}
	}

	batchMu.Lock()
	defer batchMu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("adoption batches committed = %d, want exactly 1", len(batches))
	}
	batch := batches[0]
	if len(batch.Adopted) != 1 || batch.Adopted[0].Evidence.Identity.SessionID != spec.SessionID {
		t.Fatalf("batch.Adopted = %+v, want the re-dialled lineage", batch.Adopted)
	}
	if string(batch.Adopted[0].Receipt.DurableCorrelation) != "committed-after-the-restart" {
		t.Fatalf("batch carries receipt %q, want the one the admitted dial returned",
			batch.Adopted[0].Receipt.DurableCorrelation)
	}
	if len(batch.Quarantined) != 0 {
		t.Fatalf("batch.Quarantined = %+v, want empty", batch.Quarantined)
	}
}

// TestStartupCompositionQuarantinesOnlyAfterTheRedialBudget is the other half:
// the bound is real. A relay that never comes back does end in a quarantine —
// visibly degraded, after the budget, with a detail that says the relay was the
// thing that never answered and how many times this daemon waited for it.
func TestStartupCompositionQuarantinesOnlyAfterTheRedialBudget(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-restart-exhausted"
	spec := launchOneAdoptableLineage(t, f, orgID, "lineage-relay-gone")

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	adoption := &restartingAdoption{relay: newDrainingRelay(t, 0, 1)}
	replacement := newRestartRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("one lineage's refusal failed the whole composition: %v", err)
	}

	if _, dials := adoption.snapshot(); dials != 3 {
		t.Fatalf("durable adoption dials = %d, want the configured 3", dials)
	}
	if _, err := replacement.adoptedShimEntry(orgID, spec.SessionID); err == nil {
		t.Fatal("a lineage was adopted although every dial was refused")
	}
	found := false
	for _, q := range replacement.QuarantinedSessions() {
		if q.SessionID != spec.SessionID {
			continue
		}
		found = true
		if q.Reason != sessionshim.QuarantineAdoptionFailed {
			t.Fatalf("quarantine reason = %q, want %q", q.Reason, sessionshim.QuarantineAdoptionFailed)
		}
		for _, want := range []string{"restart", "still unavailable after 3 bounded re-dials"} {
			if !strings.Contains(q.Detail, want) {
				t.Fatalf("quarantine detail %q does not say the relay was the thing that refused (%q)", q.Detail, want)
			}
		}
	}
	if !found {
		t.Fatal("the lineage was not surfaced in the live quarantine projection")
	}
}

// TestUnavailableRedialDelayHonoursTheRelaysFloor pins the schedule exactly,
// rather than inferring it from a clock: the relay's own floor is a MINIMUM the
// local backoff cannot undercut, the local schedule still doubles when the
// relay names nothing, and the ceiling bounds both — so a relay that asks for
// longer than a startup pass can hold a host's capacity for gets dialled anyway
// and refused again, spending a bounded attempt instead of an unbounded wait.
func TestUnavailableRedialDelayHonoursTheRelaysFloor(t *testing.T) {
	t.Parallel()
	const (
		base    = 500 * time.Millisecond
		ceiling = 30 * time.Second
	)
	for _, test := range []struct {
		name    string
		dial    int
		hint    time.Duration
		base    time.Duration
		ceiling time.Duration
		want    time.Duration
	}{
		{name: "no floor named: the local schedule", dial: 2, base: base, ceiling: ceiling, want: base},
		{name: "no floor named: it doubles", dial: 3, base: base, ceiling: ceiling, want: 2 * base},
		{name: "no floor named: and again", dial: 4, base: base, ceiling: ceiling, want: 4 * base},
		{
			name: "the relay's floor wins when it is larger", dial: 2, hint: 5 * time.Second,
			base: base, ceiling: ceiling, want: 5 * time.Second,
		},
		{
			name: "a floor below the local schedule does not shorten it", dial: 4, hint: time.Second,
			base: base, ceiling: ceiling, want: 4 * base,
		},
		{
			name: "the ceiling bounds the local schedule", dial: 9, base: base, ceiling: 4 * time.Second,
			want: 4 * time.Second,
		},
		{
			name: "the ceiling bounds a floor the relay named too", dial: 2, hint: time.Hour,
			base: base, ceiling: ceiling, want: ceiling,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := sessionShimUnavailableRedialDelay(test.dial, test.hint, test.base, test.ceiling); got != test.want {
				t.Fatalf("delay(dial=%d, hint=%v) = %v, want %v", test.dial, test.hint, got, test.want)
			}
		})
	}
}

// TestUnavailableRedialBoundComesFromTheReadoptionPolicy pins that the startup
// pass spends the SAME bound the controller-loss path spends. Two private
// schedules for "how long do we chase a carrier that is not answering" is one
// more than a deployment can tune.
func TestUnavailableRedialBoundComesFromTheReadoptionPolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		policy      SessionShimReadoptionPolicy
		wantDials   int
		wantBase    time.Duration
		wantCeiling time.Duration
	}{
		{
			name:        "the default policy",
			wantDials:   defaultSessionShimReadoptionAttempts,
			wantBase:    defaultSessionShimReadoptionBackoff,
			wantCeiling: defaultSessionShimReadoptionBackoffCap,
		},
		{
			name: "a deployment's own numbers",
			policy: SessionShimReadoptionPolicy{
				Mode: ReadoptionFixedAttempts, Attempts: 7,
				Backoff: 2 * time.Second, BackoffCap: 9 * time.Second,
			},
			wantDials: 7, wantBase: 2 * time.Second, wantCeiling: 9 * time.Second,
		},
		{
			// Lineage-live bounds itself with a window and leaves Attempts zero,
			// so the fixed default stands in rather than a zero budget silently
			// restoring the single-attempt quarantine this whole path removes.
			name:      "lineage-live leaves the attempt count to the default",
			policy:    DefaultLineageLiveSessionShimReadoptionPolicy(),
			wantDials: defaultSessionShimReadoptionAttempts,
			wantBase:  defaultSessionShimReadoptionBackoff, wantCeiling: defaultSessionShimReadoptionBackoffCap,
		},
		{
			// Disabling controller-loss re-adoption answers a different
			// question, and must not quarantine every lineage on the host the
			// next time a deploy lands mid-composition.
			name:      "a disabled re-adoption policy still re-dials a restarting relay",
			policy:    SessionShimReadoptionPolicy{Disabled: true},
			wantDials: defaultSessionShimReadoptionAttempts,
			wantBase:  defaultSessionShimReadoptionBackoff, wantCeiling: defaultSessionShimReadoptionBackoffCap,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{Readoption: test.policy}})
			dials, base, ceiling := d.sessionShimUnavailableRedialBound()
			if dials != test.wantDials || base != test.wantBase || ceiling != test.wantCeiling {
				t.Fatalf("bound = %d dials, %v base, %v ceiling; want %d, %v, %v",
					dials, base, ceiling, test.wantDials, test.wantBase, test.wantCeiling)
			}
		})
	}
}
