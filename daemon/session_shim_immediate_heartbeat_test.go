package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newImmediateBeatRecorder is a control plane that acknowledges every beat by
// echoing the exact session-shim projection back, and remembers what it saw.
// Echoing is not a convenience: the acknowledgement edge refuses anything that
// is not byte-for-byte the projection it was sent.
type immediateBeatRecorder struct {
	mu          sync.Mutex
	beats       int
	projections []SessionShimHeartbeatProjection
}

func (r *immediateBeatRecorder) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, req *http.Request) {
		var body heartbeatRequestBody
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("decode heartbeat body: %v", err)
			return
		}
		r.mu.Lock()
		r.beats++
		if body.SessionShim != nil {
			r.projections = append(r.projections, *body.SessionShim)
		}
		r.mu.Unlock()
		_ = json.NewEncoder(w).Encode(heartbeatResponseBody{Acknowledged: true, SessionShim: body.SessionShim})
	}
}

func (r *immediateBeatRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.beats
}

func (r *immediateBeatRecorder) adoptionCompleteRevisions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.projections))
	for _, projection := range r.projections {
		if projection.AdoptionComplete {
			out = append(out, projection.AdoptionRevision)
		}
	}
	return out
}

// TestDynamicAdoptionRingsAnImmediatePostActivationHeartbeat is the defect this
// change closes.
//
// A session-shim adoption published after startup raises the recovery barrier,
// publishes, and completes carrier activation — and then, before this change,
// waited for the periodic ticker. For up to one whole heartbeat interval a host
// that was completely ready claimed no new work and did not read as
// adoption-complete to its control plane.
//
// The interval here is the production default, so a beat carrying the NEW
// adoption revision inside two seconds of the launch cannot have come from the
// ticker: it can only be the immediate one.
func TestDynamicAdoptionRingsAnImmediatePostActivationHeartbeat(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.opts.SessionShim.HostID = "host-immediate-beat"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, f.orgID)
	probe := &dynamicPublicationProbe{}
	probe.carrierEpoch.Store(70)
	configureDynamicPublicationProbe(t, d, probe)

	var activatedMu sync.Mutex
	type activatedScope struct {
		scope                     string
		revision                  string
		carrierActivationComplete bool
		beatsAlreadySent          int
	}
	var activated []activatedScope
	recorder := &immediateBeatRecorder{}
	d.opts.SessionShim.OnAdoptionActivated = func(_ context.Context, scope, revision string) {
		activatedMu.Lock()
		activated = append(activated, activatedScope{
			scope:                     scope,
			revision:                  revision,
			carrierActivationComplete: d.SessionShimCarrierActivationComplete(),
			beatsAlreadySent:          recorder.count(),
		})
		activatedMu.Unlock()
	}
	activatedSnapshot := func() []activatedScope {
		activatedMu.Lock()
		defer activatedMu.Unlock()
		return append([]activatedScope(nil), activated...)
	}

	server := httptest.NewServer(recorder.handler(t))
	t.Cleanup(server.Close)
	// Every option the beat goroutine reads is settled BEFORE the service
	// starts; mutating them afterwards would race the beat that reads them.
	service := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker-immediate-beat", OrchestratorURL: server.URL,
		RuntimeJWT:      "runtime-immediate-beat",
		IntervalSeconds: int(HeartbeatDefaultInterval / time.Second),
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     d.heartbeatMaxConcurrentSessions,
		GetStatus:       d.RegistrationStatus,
		GetSessionShim: func() (SessionShimHeartbeatProjection, error) {
			return d.SessionShimHeartbeatProjection(f.orgID)
		},
		OnSessionShimAcknowledged: func(projection SessionShimHeartbeatProjection) {
			d.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, projection)
		},
		HTTPClient: server.Client(),
	})
	d.lifecycleMu.Lock()
	d.heartbeat = service
	d.lifecycleMu.Unlock()
	service.Start()
	t.Cleanup(service.Stop)
	waitFor(t, 5*time.Second, "the periodic lane's first beat", func() bool {
		return recorder.count() >= 1
	})
	baselineBeats := recorder.count()

	launchedAt := time.Now()
	if _, err := d.spawner.AcceptWork(f.interactiveSpec("immediate-beat")); err != nil {
		t.Fatalf("dynamic launch: %v", err)
	}
	publications, _ := probe.snapshot()
	if len(publications) != 1 {
		t.Fatalf("dynamic publications = %d, want 1: %+v", len(publications), publications)
	}
	wantRevision := "dynamic-revision-1"

	waitFor(t, 2*time.Second, "the immediate post-activation heartbeat to clear recovery", func() bool {
		return !d.sessionShimReadinessWithdrawn.Load()
	})
	if elapsed := time.Since(launchedAt); elapsed >= HeartbeatDefaultInterval {
		t.Fatalf("recovery cleared after %s, which is one whole heartbeat interval — the ticker, not an immediate beat", elapsed)
	}
	if got := recorder.count(); got != baselineBeats+1 {
		t.Fatalf("beats after the dynamic launch = %d, want exactly one more than the %d already sent", got, baselineBeats)
	}
	revisions := recorder.adoptionCompleteRevisions()
	if len(revisions) == 0 || revisions[len(revisions)-1] != wantRevision {
		t.Fatalf("adoption-complete revisions on the wire = %v, want the last to be %q", revisions, wantRevision)
	}
	if d.State() != StateRunning || !d.spawner.IsAccepting() {
		t.Fatalf("recovery did not reopen admission: state=%s accepting=%v", d.State(), d.spawner.IsAccepting())
	}
	if suspended, _ := d.PollClaimGate()(); suspended {
		t.Fatal("poll/claim admission is still suspended after the acknowledged immediate beat")
	}

	got := activatedSnapshot()
	if len(got) != 1 {
		t.Fatalf("OnAdoptionActivated calls = %+v, want exactly one", got)
	}
	if got[0].scope != f.orgID || got[0].revision != wantRevision {
		t.Fatalf("OnAdoptionActivated scope/revision = %q/%q, want %q/%q",
			got[0].scope, got[0].revision, f.orgID, wantRevision)
	}
	if !got[0].carrierActivationComplete {
		t.Fatal("OnAdoptionActivated ran before carrier activation completed")
	}
	if got[0].beatsAlreadySent != baselineBeats+1 {
		t.Fatalf("OnAdoptionActivated saw %d beats, want %d — it must fire AFTER the lane donmai owns has already beaten",
			got[0].beatsAlreadySent, baselineBeats+1)
	}
}

// TestHeartbeatSendNowIsInertWhileTheLoopIsNotRunning pins the half of the
// contract that keeps SendNow from becoming a second way to start beating: with
// no loop there is no registered lane to ride, so the call is a no-op — and it
// must not quietly start one.
func TestHeartbeatSendNowIsInertWhileTheLoopIsNotRunning(t *testing.T) {
	tests := []struct {
		name  string
		setUp func(t *testing.T, hs *HeartbeatService, hits func() int)
	}{
		{
			name:  "never started",
			setUp: func(*testing.T, *HeartbeatService, func() int) {},
		},
		{
			name: "stopped after a start",
			setUp: func(t *testing.T, hs *HeartbeatService, hits func() int) {
				t.Helper()
				hs.Start()
				waitFor(t, 5*time.Second, "the loop's immediate first beat", func() bool { return hits() >= 1 })
				hs.Stop()
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &immediateBeatRecorder{}
			server := httptest.NewServer(recorder.handler(t))
			t.Cleanup(server.Close)
			hs := NewHeartbeatService(HeartbeatOptions{
				WorkerID: "worker-inert", OrchestratorURL: server.URL, RuntimeJWT: "runtime-inert",
				IntervalSeconds: 3600,
				GetActiveCount:  func() int { return 0 },
				GetMaxCount:     func() int { return 1 },
				GetStatus:       func() RegistrationStatus { return RegistrationIdle },
				HTTPClient:      server.Client(),
			})
			t.Cleanup(hs.Stop)
			tc.setUp(t, hs, recorder.count)
			before := recorder.count()

			if err := hs.SendNow(context.Background()); err != nil {
				t.Fatalf("SendNow on a stopped service = %v, want nil", err)
			}
			if got := recorder.count(); got != before {
				t.Fatalf("beats after SendNow = %d, want %d — SendNow beat with no loop running", got, before)
			}
			if hs.IsRunning() {
				t.Fatal("SendNow started the periodic loop")
			}
		})
	}
}

// TestHeartbeatSendNowAddsExactlyOneBeat pins the other half: one call, one
// beat, out of band with a ticker that is deliberately an hour away.
func TestHeartbeatSendNowAddsExactlyOneBeat(t *testing.T) {
	recorder := &immediateBeatRecorder{}
	server := httptest.NewServer(recorder.handler(t))
	t.Cleanup(server.Close)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker-send-now", OrchestratorURL: server.URL, RuntimeJWT: "runtime-send-now",
		IntervalSeconds: 3600,
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     func() int { return 1 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		HTTPClient:      server.Client(),
	})
	hs.Start()
	t.Cleanup(hs.Stop)
	waitFor(t, 5*time.Second, "the loop's immediate first beat", func() bool { return recorder.count() >= 1 })
	before := recorder.count()

	if err := hs.SendNow(context.Background()); err != nil {
		t.Fatalf("SendNow while running = %v, want nil", err)
	}
	if got := recorder.count(); got != before+1 {
		t.Fatalf("beats after SendNow = %d, want exactly %d", got, before+1)
	}
	if !hs.IsRunning() {
		t.Fatal("SendNow stopped the periodic loop")
	}
	// A second observation, after the send has settled, catches a SendNow that
	// started its own loop: an extra lane would keep adding beats.
	time.Sleep(100 * time.Millisecond)
	if got := recorder.count(); got != before+1 {
		t.Fatalf("beats after settling = %d, want %d — SendNow left a second lane beating", got, before+1)
	}
}

// TestOnAdoptionActivatedFiresOncePerPendingScope covers the fan-out on its
// own: one call per scope still awaiting acknowledgement, in scope order, with
// that scope's exact revision — and nothing at all until carrier activation is
// actually complete, because the projection a beat would carry is refused until
// then.
func TestOnAdoptionActivatedFiresOncePerPendingScope(t *testing.T) {
	t.Parallel()

	type call struct {
		scope    string
		revision string
	}
	tests := []struct {
		name                      string
		pending                   map[string]string
		carrierActivationComplete bool
		hooked                    bool
		want                      []call
	}{
		{
			name:                      "one pending scope",
			pending:                   map[string]string{"org-a": "rev-a"},
			carrierActivationComplete: true,
			hooked:                    true,
			want:                      []call{{"org-a", "rev-a"}},
		},
		{
			name:                      "every pending scope in scope order",
			pending:                   map[string]string{"org-c": "rev-c", "org-a": "rev-a", "org-b": "rev-b"},
			carrierActivationComplete: true,
			hooked:                    true,
			want:                      []call{{"org-a", "rev-a"}, {"org-b", "rev-b"}, {"org-c", "rev-c"}},
		},
		{
			name:                      "carrier activation incomplete announces nothing",
			pending:                   map[string]string{"org-a": "rev-a"},
			carrierActivationComplete: false,
			hooked:                    true,
			want:                      nil,
		},
		{
			name:                      "no pending scope announces nothing",
			pending:                   nil,
			carrierActivationComplete: true,
			hooked:                    true,
			want:                      nil,
		},
		{
			name:                      "unhooked embedder is untouched",
			pending:                   map[string]string{"org-a": "rev-a"},
			carrierActivationComplete: true,
			hooked:                    false,
			want:                      nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var (
				mu    sync.Mutex
				calls []call
			)
			opts := Options{SkipRegistration: true, SessionShim: SessionShimConfig{
				EnableAdoption: true, OrgID: "org-a",
			}}
			if tc.hooked {
				opts.SessionShim.OnAdoptionActivated = func(ctx context.Context, scope, revision string) {
					if deadline, ok := ctx.Deadline(); !ok || !deadline.After(time.Now().Add(-time.Second)) {
						t.Errorf("OnAdoptionActivated context is unbounded for scope %q", scope)
					}
					mu.Lock()
					calls = append(calls, call{scope, revision})
					mu.Unlock()
				}
			}
			d := New(opts)
			d.shims.mu.Lock()
			d.shims.carrierActivationComplete = tc.carrierActivationComplete
			for scope, revision := range tc.pending {
				d.shims.pendingHeartbeatAcks[scope] = revision
			}
			d.shims.mu.Unlock()

			d.notifySessionShimAdoptionActivated(context.Background(), d.sessionShimActivatedScopes())

			mu.Lock()
			got := append([]call(nil), calls...)
			mu.Unlock()
			if len(got) != len(tc.want) {
				t.Fatalf("OnAdoptionActivated calls = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("OnAdoptionActivated call %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestHeartbeatSendNowSerializesWithTheTicker proves the claim SendNow's
// documentation makes: an out-of-band beat and a periodic one cannot interleave.
// A handler that fails the moment two beats overlap turns any interleaving into
// a test failure rather than a rare production ACK duplication.
func TestHeartbeatSendNowSerializesWithTheTicker(t *testing.T) {
	var (
		inFlight atomic.Int32
		overlaps atomic.Int32
		beats    atomic.Int32
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if inFlight.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		beats.Add(1)
		_ = json.NewEncoder(w).Encode(heartbeatResponseBody{Acknowledged: true})
	}))
	t.Cleanup(server.Close)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker-serialized", OrchestratorURL: server.URL, RuntimeJWT: "runtime-serialized",
		// A 1s ticker cannot deliver enough ticks to collide on its own, so the
		// concurrency below comes from the out-of-band senders.
		IntervalSeconds: 1,
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     func() int { return 1 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		HTTPClient:      server.Client(),
	})
	hs.Start()
	t.Cleanup(hs.Stop)
	waitFor(t, 5*time.Second, "the loop's immediate first beat", func() bool { return beats.Load() >= 1 })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := hs.SendNow(context.Background()); err != nil {
				t.Errorf("concurrent SendNow = %v, want nil", err)
			}
		}()
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Fatalf("overlapping beats = %d, want 0 — SendNow raced another sender", overlaps.Load())
	}
}
