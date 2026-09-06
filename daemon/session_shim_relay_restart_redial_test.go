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
		t.Fatalf("durable adoption dials = %d, want 3 — the first, plus the two the pass's window paid for", dials)
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
		for _, want := range []string{"restart", "still unavailable after 2 re-dial(s)", "drain window"} {
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

	// One lineage spends the window (1 dial + 2 waited re-dials); the other two
	// spend one dial and the free re-dial each. A per-lineage ladder is 3 dials
	// EACH — nine — and three separate windows of waiting.
	if _, dials := adoption.snapshot(); dials != 7 {
		t.Fatalf("durable adoption dials = %d, want 7 (3 + 2 + 2): one shared window, "+
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

// TestRelayDrainBoundComesFromAValidatedPolicy pins that the bound is read from
// the deployment's own re-adoption policy in the shape that policy's OWN
// VALIDATOR accepts.
//
// Every case runs Validate() first, and that is the point: the validator
// refuses BackoffCap in fixed-attempts mode and Attempts in lineage-live mode,
// so a bound assembled from all four fields would pin a configuration no daemon
// can boot with — the "third policy nobody can tune" this is supposed to avoid.
func TestRelayDrainBoundComesFromAValidatedPolicy(t *testing.T) {
	t.Parallel()
	fixedWorstCase := func(attempts int, backoff, attemptTimeout time.Duration) time.Duration {
		return SessionShimReadoptionPolicy{
			Mode: ReadoptionFixedAttempts, Attempts: attempts,
			Backoff: backoff, AttemptTimeout: attemptTimeout,
		}.WorstCaseWindow()
	}
	for _, test := range []struct {
		name   string
		policy SessionShimReadoptionPolicy
		want   sessionShimRelayDrainBound
	}{
		{
			// Default fixed-attempts: 3 dials, so 2 waits, on a 5s ladder. Its
			// own worst case is 60s, which the startup cap trims to 30s.
			name: "the default policy",
			want: sessionShimRelayDrainBound{
				waits: defaultSessionShimReadoptionAttempts - 1,
				base:  defaultSessionShimReadoptionBackoff,
				// The policy expresses no per-wait cap in this mode, so the
				// window caps a single wait too.
				ceiling: sessionShimStartupRelayDrainCap,
				window:  sessionShimStartupRelayDrainCap,
			},
		},
		{
			// A deployment's own numbers, in the fixed-attempts vocabulary the
			// validator accepts — no BackoffCap, which it would reject.
			name: "a deployment's own fixed-attempt numbers",
			policy: SessionShimReadoptionPolicy{
				Mode: ReadoptionFixedAttempts, Attempts: 4,
				Backoff: 2 * time.Second, AttemptTimeout: time.Second,
			},
			want: sessionShimRelayDrainBound{
				waits: 3, base: 2 * time.Second,
				ceiling: fixedWorstCase(4, 2*time.Second, time.Second),
				window:  fixedWorstCase(4, 2*time.Second, time.Second),
			},
		},
		{
			// Lineage-live expresses no attempt count — the window is its bound,
			// and BackoffCap is the per-wait cap it does express. Its ten-minute
			// window is a legitimate answer to "how patient about a live carrier
			// fault" and never to "how long may boot block", so the startup cap
			// trims it.
			name:   "lineage-live is read in its own vocabulary",
			policy: DefaultLineageLiveSessionShimReadoptionPolicy(),
			want: sessionShimRelayDrainBound{
				waits: 0, base: defaultSessionShimReadoptionBackoff,
				ceiling: defaultSessionShimReadoptionBackoffCap,
				window:  sessionShimStartupRelayDrainCap,
			},
		},
		{
			// Disabling controller-loss re-adoption answers a different
			// question, and must not quarantine every lineage on the host the
			// next time a deploy lands mid-composition.
			name:   "a disabled re-adoption policy still re-dials a restarting relay",
			policy: SessionShimReadoptionPolicy{Disabled: true},
			want: sessionShimRelayDrainBound{
				waits:   defaultSessionShimReadoptionAttempts - 1,
				base:    defaultSessionShimReadoptionBackoff,
				ceiling: sessionShimStartupRelayDrainCap,
				window:  sessionShimStartupRelayDrainCap,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// The shape must be one a daemon can actually boot with.
			if err := test.policy.Validate(); err != nil {
				t.Fatalf("the policy this case pins is not one any daemon can boot with: %v", err)
			}
			d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{Readoption: test.policy}})
			if got := d.sessionShimRelayDrainBound(); got != test.want {
				t.Fatalf("bound = %+v, want %+v", got, test.want)
			}
		})
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
