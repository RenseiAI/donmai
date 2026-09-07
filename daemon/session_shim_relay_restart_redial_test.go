package daemon

// The daemon half of the planned-restart contract. See
// session_shim_adoption_redial.go's "THE SECOND STRAND" for what it undoes: a
// startup composition that ran inside a relay's drain window quarantined every
// lineage it was composing at that instant, because a 503 that says "come back
// in five seconds" was classified exactly like a lineage that refused.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/sessionshim"
)

// relayRestartFixtureWindow is the drain window every fixture in this file
// composes with. At 1.2s it buys a 300ms per-wait ceiling (a quarter of the
// window) and a 2.4s pass budget, which is what makes the dial counts below
// exact arithmetic rather than a race with the clock.
const relayRestartFixtureWindow = 1200 * time.Millisecond

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
	relay.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relay.mu.Lock()
		relay.dials++
		draining := relay.drains == 0 || relay.dials <= relay.drains
		relay.mu.Unlock()
		// /refuse and /admit let a fixture drive a pattern the dial COUNT
		// cannot express — a relay that flaps per lineage rather than per dial.
		switch r.URL.Path {
		case "/refuse":
			draining = true
		case "/admit":
			draining = false
		}
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

// dialPath performs one attach dial and returns the refusal a composing carrier
// would hand back, or nil once the replacement admits it. An empty path lets
// the relay's own dial counter decide; /refuse and /admit override it.
func (r *drainingRelay) dialPath(path string) error {
	resp, err := http.Get(r.server.URL + path) //nolint:noctx // loopback fixture
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
	// refuseEachLineageFor, when positive, makes the relay refuse every lineage
	// for that long from its FIRST dial and admit it after — a relay that flaps
	// rather than one that drains once. It is an interval and not a dial count
	// because that is what separates the two dispositions under test: a re-dial
	// fired in the same instant lands inside the refusal, while one spaced by
	// the announced floor lands after it.
	refuseEachLineageFor time.Duration
	// refuseFirstLineageFor makes the FIRST lineage the pass reaches meet a
	// longer outage than the ones after it — one real restart, then brief
	// blips. It is what drives the pass's shared budget to exhaustion inside a
	// fixture small enough to run in seconds, so the lineages composed after it
	// are the ones that must NOT be condemned by a budget somebody else spent.
	refuseFirstLineageFor time.Duration

	mu           sync.Mutex
	asks         []preparedAsk
	dials        int
	perShim      map[sessionshim.Identity]time.Time
	firstLineage sessionshim.Identity
}

func (a *restartingAdoption) prepare(_ context.Context, in SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = append(a.asks, preparedAsk{cause: in.Cause, attempt: in.Attempt, generation: in.CurrentControllerGeneration})
	return sessionshim.PreparedAdoption{Correlation: []byte("candidate-1")}, nil
}

func (a *restartingAdoption) adopt(_ context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
	a.mu.Lock()
	a.dials++
	path := ""
	if a.refuseEachLineageFor > 0 {
		if a.perShim == nil {
			a.perShim = map[sessionshim.Identity]time.Time{}
		}
		first, seen := a.perShim[evidence.Identity]
		outage := a.refuseEachLineageFor
		if !seen {
			first = time.Now()
			a.perShim[evidence.Identity] = first
			if len(a.perShim) == 1 && a.refuseFirstLineageFor > 0 {
				a.firstLineage = evidence.Identity
			}
		}
		if evidence.Identity == a.firstLineage {
			outage = a.refuseFirstLineageFor
		}
		path = "/admit"
		if time.Since(first) < outage {
			path = "/refuse"
		}
	}
	a.mu.Unlock()
	if err := a.relay.dialPath(path); err != nil {
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
			// A 1.5s window against the relay fixtures' 1s announced floor buys
			// two waits — one full floor, then the remainder of the window — so
			// every dial count below is exact arithmetic rather than a race with
			// the clock. The ladder comes from the re-adoption policy; the
			// window is its own knob, because how long a relay restart takes is
			// not a fact about this daemon's re-adoption appetite.
			StartupRelayDrainWindow: relayRestartFixtureWindow,
			Readoption: SessionShimReadoptionPolicy{
				Mode: ReadoptionFixedAttempts, Attempts: 3,
				Backoff: 100 * time.Millisecond, AttemptTimeout: 400 * time.Millisecond,
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

	// The refusal that opens the window, plus every re-dial the 1.5s window
	// pays for at the 375ms per-wait ceiling.
	if _, dials := adoption.snapshot(); dials != 5 {
		t.Fatalf("durable adoption dials = %d, want 5 — the first, plus the four the pass's window paid for", dials)
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
		for _, want := range []string{"restart", "still unavailable after 4 re-dial(s)", "drain window"} {
			if !strings.Contains(q.Detail, want) {
				t.Fatalf("quarantine detail %q does not say the relay was the thing that refused (%q)", q.Detail, want)
			}
		}
	}
	if !found {
		t.Fatal("the lineage was not surfaced in the live quarantine projection")
	}
}

// TestCompositionWaitsOneDrainWindowForEveryLineage is the pass-wide half.
//
// A relay outage is one fact about one process, and composition is serial. A
// ladder restarted from zero per lineage would make every lineage re-learn that
// fact, pay for it out of its own quarantine budget, and multiply a single
// outage by the lineage count — with the composition ORDER deciding who
// survived it. Three lineages, one drain window: the pass waits once, and the
// lineages composed after the window was opened dial straight through.
func TestCompositionWaitsOneDrainWindowForEveryLineage(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-shared-drain"
	specs := []SessionSpec{
		launchOneAdoptableLineage(t, f, orgID, "lineage-drain-a"),
		launchOneAdoptableLineage(t, f, orgID, "lineage-drain-b"),
		launchOneAdoptableLineage(t, f, orgID, "lineage-drain-c"),
	}

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	// The drain covers the first two dials — spent by whichever lineage the pass
	// composes first — and the replacement admits everything after.
	adoption := &restartingAdoption{relay: newDrainingRelay(t, 2, 1)}
	replacement := newRestartRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup composition returned a host-wide failure: %v", err)
	}

	if _, dials := adoption.snapshot(); dials != 5 {
		t.Fatalf("durable adoption dials = %d, want 5 — two refusals plus one admitted dial each; "+
			"a per-lineage ladder would re-learn the drain and dial more", dials)
	}
	for _, spec := range specs {
		if _, err := replacement.adoptedShimEntry(orgID, spec.SessionID); err != nil {
			t.Fatalf("%s was quarantined over a relay that was restarting: %v", spec.SessionID, err)
		}
	}
	if q := replacement.QuarantinedSessions(); len(q) != 0 {
		t.Fatalf("quarantined = %+v, want none", q)
	}
}

// TestCompositionReachesOneVerdictWhenTheRelayNeverReturns is the other half of
// the same property, and the one the adverse ordering used to break: when the
// drain outlasts the window, every lineage must reach the SAME answer, and the
// pass must spend ONE window rather than one per lineage.
//
// The lineage that opens the window spends its waits; the ones composed after
// it still get their single free re-dial — no lineage is ever quarantined on
// one unavailable refusal — but they do not re-open a window the pass has
// already spent.
func TestCompositionReachesOneVerdictWhenTheRelayNeverReturns(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-shared-drain-forever"
	specs := []SessionSpec{
		launchOneAdoptableLineage(t, f, orgID, "lineage-gone-a"),
		launchOneAdoptableLineage(t, f, orgID, "lineage-gone-b"),
		launchOneAdoptableLineage(t, f, orgID, "lineage-gone-c"),
	}

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	adoption := &restartingAdoption{relay: newDrainingRelay(t, 0, 1)}
	replacement := newRestartRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("a relay that never returned failed the whole composition: %v", err)
	}

	// 5 + 2 + 2: one lineage spends the shared window, and the two composed
	// after it get the free re-dial that keeps a single refusal from ever being
	// terminal. A per-lineage ladder is 5 dials EACH — fifteen — and three
	// separate windows of waiting.
	if _, dials := adoption.snapshot(); dials != 9 {
		t.Fatalf("durable adoption dials = %d, want 9 (5 + 2 + 2): one shared window, "+
			"then one free re-dial per later lineage", dials)
	}
	quarantined := map[string]sessionshim.QuarantinedSession{}
	for _, q := range replacement.QuarantinedSessions() {
		quarantined[q.SessionID] = q
	}
	for _, spec := range specs {
		q, found := quarantined[spec.SessionID]
		if !found {
			t.Fatalf("%s reached a different verdict from its siblings against one fleet-wide outage", spec.SessionID)
		}
		if q.Reason != sessionshim.QuarantineAdoptionFailed {
			t.Fatalf("%s quarantine reason = %q, want %q", spec.SessionID, q.Reason, sessionshim.QuarantineAdoptionFailed)
		}
		if !strings.Contains(q.Detail, "drain window") {
			t.Fatalf("%s quarantine detail %q does not name the composition's drain window", spec.SessionID, q.Detail)
		}
	}
}

// TestRelayDrainBoundResolvesEachNumberFromWhoeverKnowsIt pins where every
// field of the bound comes from, and — the recurring defect this closes — that
// the number the code DERIVES is the number the shipped default SPENDS.
//
// Two earlier rounds sized the window off the re-adoption policy, which
// answers a different question; the derived constant then governed nothing and
// the default fell short of the relay's own worst case. The ladder still comes
// from that policy, in the shape its own validator accepts, because retry
// cadence genuinely is its question.
func TestRelayDrainBoundResolvesEachNumberFromWhoeverKnowsIt(t *testing.T) {
	t.Parallel()
	quarter := func(window time.Duration) time.Duration {
		return window / sessionShimStartupRelayDrainWaitDivisor
	}
	for _, test := range []struct {
		name   string
		window time.Duration
		policy SessionShimReadoptionPolicy
		want   sessionShimRelayDrainBound
	}{
		{
			// The shipped default: the derived window, spent.
			name: "the shipped default spends the derived window",
			want: sessionShimRelayDrainBound{
				base:      defaultSessionShimReadoptionBackoff,
				ceiling:   quarter(sessionShimStartupRelayDrainWindow),
				window:    sessionShimStartupRelayDrainWindow,
				passTotal: 2 * sessionShimStartupRelayDrainWindow,
			},
		},
		{
			// A deployment names its own window; the ladder still comes from the
			// re-adoption policy, in the fixed-attempts vocabulary the validator
			// accepts (no BackoffCap, which it would reject).
			name:   "a deployment's own window and the policy's ladder",
			window: 40 * time.Second,
			policy: SessionShimReadoptionPolicy{
				Mode: ReadoptionFixedAttempts, Attempts: 4,
				Backoff: 2 * time.Second, AttemptTimeout: time.Second,
			},
			want: sessionShimRelayDrainBound{
				base:      2 * time.Second,
				ceiling:   quarter(40 * time.Second),
				window:    40 * time.Second,
				passTotal: 80 * time.Second,
			},
		},
		{
			// Lineage-live is the one mode that expresses a per-wait cap, and it
			// is honoured — while it is below the fraction-of-window bound every
			// mode gets. Its default 30s is not, against a 90s window.
			name:   "the fraction of the window bounds even a mode that names its own cap",
			policy: DefaultLineageLiveSessionShimReadoptionPolicy(),
			want: sessionShimRelayDrainBound{
				base:      defaultSessionShimReadoptionBackoff,
				ceiling:   quarter(sessionShimStartupRelayDrainWindow),
				window:    sessionShimStartupRelayDrainWindow,
				passTotal: 2 * sessionShimStartupRelayDrainWindow,
			},
		},
		{
			name: "a per-wait cap below that fraction is honoured",
			policy: func() SessionShimReadoptionPolicy {
				policy := DefaultLineageLiveSessionShimReadoptionPolicy()
				policy.BackoffCap = 10 * time.Second
				return policy
			}(),
			want: sessionShimRelayDrainBound{
				base:      defaultSessionShimReadoptionBackoff,
				ceiling:   10 * time.Second,
				window:    sessionShimStartupRelayDrainWindow,
				passTotal: 2 * sessionShimStartupRelayDrainWindow,
			},
		},
		{
			// However long a deployment's appetite, boot may not block past the
			// cap.
			name:   "the cap bounds a window boot cannot afford",
			window: time.Hour,
			want: sessionShimRelayDrainBound{
				base:      defaultSessionShimReadoptionBackoff,
				ceiling:   quarter(sessionShimStartupRelayDrainCap),
				window:    sessionShimStartupRelayDrainCap,
				passTotal: 2 * sessionShimStartupRelayDrainCap,
			},
		},
		{
			// Disabling controller-loss re-adoption answers a different
			// question, and must not quarantine every lineage on the host the
			// next time a deploy lands mid-composition.
			name:   "a disabled re-adoption policy still re-dials a restarting relay",
			policy: SessionShimReadoptionPolicy{Disabled: true},
			want: sessionShimRelayDrainBound{
				base:      defaultSessionShimReadoptionBackoff,
				ceiling:   quarter(sessionShimStartupRelayDrainWindow),
				window:    sessionShimStartupRelayDrainWindow,
				passTotal: 2 * sessionShimStartupRelayDrainWindow,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// The ladder's shape must be one a daemon can actually boot with.
			if err := test.policy.Validate(); err != nil {
				t.Fatalf("the policy this case pins is not one any daemon can boot with: %v", err)
			}
			d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
				StartupRelayDrainWindow: test.window, Readoption: test.policy,
			}})
			if got := d.sessionShimRelayDrainBound(); got != test.want {
				t.Fatalf("bound = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestOneLargeAnnouncedFloorCannotCollapseTheLadder pins the per-wait ceiling.
//
// The floor a relay names is honoured as a MINIMUM, so without a ceiling that
// is independent of the window a single large Retry-After reduced a whole
// outage to two dials — the refusal that opened it and one at the deadline —
// which is the fewest possible chances to notice the relay's return.
func TestOneLargeAnnouncedFloorCannotCollapseTheLadder(t *testing.T) {
	t.Parallel()
	d := New(Options{SkipRegistration: true})
	bound := d.sessionShimRelayDrainBound()
	for _, floor := range []time.Duration{5 * time.Second, 30 * time.Second, bound.window, time.Hour} {
		var barrier sessionShimRelayDrainBarrier
		start := time.Now()
		now := start
		dials := 1
		for {
			wait, stop := barrier.reserve(floor, bound, now)
			if stop != sessionShimDrainLive {
				break
			}
			now = now.Add(wait)
			dials++
		}
		if dials < sessionShimStartupRelayDrainWaitDivisor {
			t.Fatalf("an announced floor of %s left the outage %d dials in its %s window; "+
				"a single Retry-After must not collapse the ladder", floor, dials, bound.window)
		}
		if spent := now.Sub(start); spent > bound.window {
			t.Fatalf("an announced floor of %s spent %s of a %s window", floor, spent, bound.window)
		}
	}
}

// TestTheShippedDefaultOutlastsARelayRestart is the bound that matters, stated
// as the question an operator actually has: does a host booting into a relay's
// planned restart still have a lineage when the replacement comes up?
//
// A relay's planned restart is its drain timeout (15s by that contract's
// default), then shutdown, then a journal flush, then a single-machine
// replacement boot — comfortably longer than the drain alone. A startup pass
// that stops waiting first quarantines the lineage, closes its controller, and
// leaves it with NO recovery path: the controller-loss re-adoption path only
// fires for a lineage that was adopted, and this one never was. The shim keeps
// its harness and reaps it on its own orphan deadline.
//
// This walks the real barrier with a synthetic clock — no sleeping — and
// requires the shipped default to still be granting dials well past the drain.
func TestTheShippedDefaultOutlastsARelayRestart(t *testing.T) {
	t.Parallel()
	// What the pass must survive: the relay's own default drain, plus the
	// shutdown, flush and replacement boot that follow it.
	const observedRestart = 45 * time.Second
	// The floor the relay announces is a re-dial SPACING, deliberately much
	// shorter than the restart it announces — it can never extend the ladder to
	// cover one.
	const announcedFloor = 5 * time.Second

	d := New(Options{SkipRegistration: true})
	bound := d.sessionShimRelayDrainBound()
	if bound.window <= observedRestart {
		t.Fatalf("the shipped default gives the pass a %s window, which does not outlast a %s relay restart",
			bound.window, observedRestart)
	}

	var barrier sessionShimRelayDrainBarrier
	start := time.Now()
	now := start
	dials := 1 // the refusal that opens the window
	for {
		wait, stop := barrier.reserve(announcedFloor, bound, now)
		if stop != sessionShimDrainLive {
			break
		}
		now = now.Add(wait)
		dials++
		if now.Sub(start) > 10*time.Minute {
			t.Fatal("the barrier never stopped granting waits")
		}
	}
	if now.Sub(start) <= observedRestart {
		t.Fatalf("the pass stopped re-dialling at %s, inside a %s relay restart — "+
			"a lineage refused for the whole restart is quarantined with no recovery path",
			now.Sub(start), observedRestart)
	}
	if dials < 4 {
		t.Fatalf("the pass spent only %d dials across %s; the ladder is not re-dialling through the drain",
			dials, now.Sub(start))
	}
}

// TestRelayDrainDelayHonoursTheRelaysFloor pins the schedule exactly, rather
// than inferring it from a clock: the relay's own floor is a MINIMUM the local
// backoff cannot undercut, the local schedule still doubles when the relay
// names nothing, and the ceiling bounds both — so a relay that asks for longer
// than a boot can wait gets dialled anyway and refused again, spending a
// bounded wait instead of an unbounded one.
func TestRelayDrainDelayHonoursTheRelaysFloor(t *testing.T) {
	t.Parallel()
	const (
		base    = 500 * time.Millisecond
		ceiling = 30 * time.Second
	)
	for _, test := range []struct {
		name  string
		wait  int
		hint  time.Duration
		bound sessionShimRelayDrainBound
		want  time.Duration
	}{
		{
			name: "no floor named: the local schedule", wait: 2,
			bound: sessionShimRelayDrainBound{base: base, ceiling: ceiling}, want: base,
		},
		{
			name: "no floor named: it doubles", wait: 3,
			bound: sessionShimRelayDrainBound{base: base, ceiling: ceiling}, want: 2 * base,
		},
		{
			name: "no floor named: and again", wait: 4,
			bound: sessionShimRelayDrainBound{base: base, ceiling: ceiling}, want: 4 * base,
		},
		{
			name: "the relay's floor wins when it is larger", wait: 2, hint: 5 * time.Second,
			bound: sessionShimRelayDrainBound{base: base, ceiling: ceiling}, want: 5 * time.Second,
		},
		{
			name: "a floor below the local schedule does not shorten it", wait: 4, hint: time.Second,
			bound: sessionShimRelayDrainBound{base: base, ceiling: ceiling}, want: 4 * base,
		},
		{
			name: "the ceiling bounds the local schedule", wait: 9,
			bound: sessionShimRelayDrainBound{base: base, ceiling: 4 * time.Second}, want: 4 * time.Second,
		},
		{
			name: "the ceiling bounds a floor the relay named too", wait: 2, hint: time.Hour,
			bound: sessionShimRelayDrainBound{base: base, ceiling: ceiling}, want: ceiling,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := sessionShimRelayDrainDelay(test.wait, test.hint, test.bound); got != test.want {
				t.Fatalf("delay(wait=%d, hint=%v) = %v, want %v", test.wait, test.hint, got, test.want)
			}
		})
	}
}

// TestAFlappingRelayCannotSpendOneWindowPerLineage pins BOTH halves of what the
// pass-total budget is for, and the one thing it must never do.
//
// Clearing the window on a dial that gets through is deliberate — it is what
// lets a later, unrelated outage be waited out properly — but on its own it
// hands a relay that ALTERNATES a fresh window per lineage, which is the
// lineage-count multiplication the shared window exists to remove, re-entering
// through the success door. The pass-total budget is not refunded by a
// successful dial, so the waiting has one ceiling however the relay behaves.
//
// What that budget must NOT do is condemn anything. It is shared with every
// other lineage, so a lineage arriving after some OTHER lineage spent it has
// evidence of nothing — and a startup quarantine is terminal for this daemon's
// lifetime, with no reconciliation pass and no controller-loss re-adoption to
// follow it. So a lineage that meets an exhausted budget still gets a dial
// spaced by the relay's own floor, served outside the budget, and every one of
// these eight lineages ends up ADOPTED.
//
// The relay here refuses each lineage for a real interval rather than for a
// dial count, which is what separates the two dispositions: a re-dial fired in
// the same instant lands inside the refusal and fails, while one spaced by the
// announced floor lands after it and is admitted.
func TestAFlappingRelayCannotSpendOneWindowPerLineage(t *testing.T) {
	// The pass below deliberately spends seconds, so the launching fixture's
	// two-second orphan deadline would reap the shims it is composing — they
	// would be tombstoned rather than adopted, and this test would be measuring
	// the fixture instead of the budget.
	f := newShimSpawnFixture(t, func(cfg *SessionShimConfig) {
		cfg.Orphan.Deadline = 5 * time.Minute
	})
	const orgID = "org-flapping-relay"
	specs := make([]SessionSpec, 0, 8)
	for i := range 8 {
		specs = append(specs, launchOneAdoptableLineage(t, f, orgID, fmt.Sprintf("lineage-flap-%d", i)))
	}

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	// One real restart, then brief blips: the first lineage's outage is what
	// drives the pass's shared budget to exhaustion, and the lineages composed
	// after it are the ones a budget somebody else spent must not condemn.
	adoption := &restartingAdoption{
		relay:                 newDrainingRelay(t, 0, 1),
		refuseFirstLineageFor: time.Second,
		refuseEachLineageFor:  150 * time.Millisecond,
	}
	replacement := newRestartRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	started := time.Now()
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("a flapping relay failed the whole composition: %v", err)
	}
	elapsed := time.Since(started)

	bound := replacement.sessionShimRelayDrainBound()
	adopted := 0
	for _, spec := range specs {
		if _, err := replacement.adoptedShimEntry(orgID, spec.SessionID); err == nil {
			adopted++
		}
	}
	if adopted != len(specs) {
		t.Fatalf("adopted %d of %d lineages; want all of them — a shared budget is a fairness "+
			"device and must never be the stop that condemns a live lineage (quarantined: %+v)",
			adopted, len(specs), replacement.QuarantinedSessions())
	}
	// The ceiling that must hold: the pass's own budget, plus at most one
	// floor-spaced dial per lineage once it is spent. An unbounded pass would
	// spend one whole window per lineage — eight of them.
	worstCase := bound.passTotal + time.Duration(len(specs))*bound.ceiling
	if elapsed > worstCase+2*time.Second {
		t.Fatalf("the composition blocked for %s against a %s budget and a %s per-wait ceiling "+
			"(worst case %s) — the pass-total budget did not hold",
			elapsed, bound.passTotal, bound.ceiling, worstCase)
	}
	if elapsed >= time.Duration(len(specs))*bound.window {
		t.Fatalf("the composition blocked for %s, which is one whole %s window per lineage — "+
			"a flapping relay bought a fresh window each time", elapsed, bound.window)
	}
}

// TestAdoptionHooksDocumentTheWrappingContract is a doc-line control for the
// one dependency this daemon cannot test behaviourally: there is no implementor
// of OnAdoption/OnAdoptionV2 in this repo. The composing layer supplies it, and
// if it renders a transport refusal with %v instead of %w, every re-dial in
// this file silently reverts to a single-attempt quarantine and nothing here
// goes red.
//
// So the contract is written on the hooks, and this asserts it is still there
// and still names the predicate an embedder can assert against.
func TestAdoptionHooksDocumentTheWrappingContract(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("session_shim.go")
	if err != nil {
		t.Fatalf("read the hook declarations: %v", err)
	}
	text := string(source)
	for _, hook := range []string{
		"OnAdoption func(context.Context, SessionShimAdoptionEvidence)",
		"OnAdoptionV2 func(context.Context, SessionShimAdoptionEvidenceV2)",
	} {
		at := strings.Index(text, hook)
		if at < 0 {
			t.Fatalf("hook %q is no longer declared where this control looks for it", hook)
		}
		// The field's own doc comment is the contiguous run of comment lines
		// immediately above the declaration.
		lines := strings.Split(text[:at], "\n")
		var doc []string
		for i := len(lines) - 2; i >= 0; i-- {
			trimmed := strings.TrimSpace(lines[i])
			if !strings.HasPrefix(trimmed, "//") {
				break
			}
			doc = append(doc, trimmed)
		}
		joined := strings.Join(doc, " ")
		for _, want := range []string{"%w", "IsSessionShimRelayUnavailable"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("the doc on %q no longer states the wrapping contract (missing %q). "+
					"Without it an embedder that renders a transport refusal with %%v silently "+
					"restores single-attempt quarantine, and no test in this repo would notice.",
					hook, want)
			}
		}
	}
	// The predicate the doc names must exist and answer for a real refusal.
	refusal := fmt.Errorf("dial fresh v2 candidate: %w",
		&attachclient.RelayRestartingError{RedialAfter: time.Second})
	if !IsSessionShimRelayUnavailable(refusal) {
		t.Fatal("IsSessionShimRelayUnavailable does not answer for a wrapped transport refusal")
	}
	if IsSessionShimRelayUnavailable(errors.New(refusal.Error())) {
		t.Fatal("IsSessionShimRelayUnavailable answers for a refusal that lost its type — " +
			"the predicate cannot detect the very mistake it exists to catch")
	}
}
