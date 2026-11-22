package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/observation"
)

func codexFeed(id string, block bool) quotaProbeFunc {
	return func(ctx context.Context, q *quotaState) {
		q.noteCodexProbe(id, "pro", agent.MakeUsageLimits(agent.ISOTime(time.Now()), []agent.UsageWindow{
			{ID: "primary", Kind: agent.UsageWindowSession, UsedPercent: 5},
		}), time.Now())
		if block {
			<-ctx.Done()
		}
	}
}

func claudeFeed(_ context.Context, q *quotaState) {
	q.noteClaudeProbe("", agent.MakeUsageLimits(agent.ISOTime(time.Now()), []agent.UsageWindow{
		{ID: "five_hour", Kind: agent.UsageWindowSession, UsedPercent: 5},
	}), time.Now())
}

func scanDaemon(probes map[string]quotaProbeFunc) *Daemon {
	d := &Daemon{quota: &quotaState{}}
	d.observationScan.probes = probes
	return d
}

// marksByKey indexes a result's marks by itemKey.
func marksByKey(r *ObservationScanResult) map[string]ObservationMark {
	out := map[string]ObservationMark{}
	for _, m := range r.Marks {
		out[m.ItemKey] = m
	}
	return out
}

var (
	codexKey  = observation.SubscriptionLoginItemKey(observation.ProviderOpenAIChatGPT, "acct-a", "")
	claudeKey = observation.HostScopedItemKey(observation.KindSubscriptionLogin, "claude", observation.DefaultSlot)
)

func TestScanObservations_NewMarksAndRateLimit(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	first, err := d.ScanObservations(context.Background(), nil)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if m := marksByKey(first); !m[codexKey].New || !m[codexKey].Enabled || !first.Report.Full {
		t.Fatalf("first scan marks = %+v, want codex new and enabled in a full report", m)
	}
	var limited *observationScanLimitedError
	if _, err := d.ScanObservations(context.Background(), nil); !errors.As(err, &limited) || limited.reason != observationScanRateLimited {
		t.Fatalf("second scan err = %v, want %s", err, observationScanRateLimited)
	}
	d.observationScan.lastAt = time.Now().Add(-2 * observationScanMinInterval)
	d.observationScan.probes["claude"] = claudeFeed
	third, err := d.ScanObservations(context.Background(), nil)
	if err != nil {
		t.Fatalf("third scan: %v", err)
	}
	if m := marksByKey(third); len(m) != 2 || m[codexKey].New || !m[claudeKey].New {
		t.Fatalf("third scan marks = %+v, want only claude new", m)
	}
}

func TestScanObservations_InFlightAndTimeout(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", true)})
	d.observationScan.inFlight = true
	var limited *observationScanLimitedError
	if _, err := d.ScanObservations(context.Background(), nil); !errors.As(err, &limited) || limited.reason != observationScanInFlight {
		t.Fatalf("scan during a scan err = %v, want %s", err, observationScanInFlight)
	}
	d.observationScan.inFlight = false
	d.observationScan.timeout = 20 * time.Millisecond
	result, err := d.ScanObservations(context.Background(), nil)
	if err != nil {
		t.Fatalf("timed-out scan: %v", err)
	}
	raw, _ := json.Marshal(result.Report)
	if !strings.Contains(string(raw), `"state":"unknown","reasons":["scan_timeout"]`) {
		t.Fatalf("timed-out scan report = %s, want readiness unknown with scan_timeout", raw)
	}
}

func TestObservationScanEndpoint(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	s := &Server{daemon: d}
	tests := []struct {
		name, body string
		wantStatus int
		wantIn     string
	}{
		{"unknown kind", `{"kinds":["bogus"]}`, http.StatusBadRequest, "unknown observation kind"},
		{"scan", `{"kinds":["subscription-login"]}`, http.StatusOK, `"marks":[{"kind":"subscription-login"`},
		{"rate limited", "", http.StatusTooManyRequests, `"reason":"scan_rate_limited"`},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		s.handleObservationScan(rec, httptest.NewRequest(http.MethodPost, "/api/daemon/observations/scan", strings.NewReader(tt.body)))
		if rec.Code != tt.wantStatus || !strings.Contains(rec.Body.String(), tt.wantIn) {
			t.Errorf("%s: status %d body %s, want %d containing %s", tt.name, rec.Code, rec.Body, tt.wantStatus, tt.wantIn)
		}
	}
}

func TestApplyObservationsScanMutation(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = 30 * time.Millisecond
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "m1", Op: observationsScanOp, Params: json.RawMessage(`{"kinds":["subscription-login"]}`)},
		{ID: "m2", Op: observationsScanOp},
		{ID: "m3", Op: observationsScanOp, Params: json.RawMessage(`{"kinds":["bogus"]}`)},
	})
	// Every valid request is queued for a scan of its own; an invalid one
	// fails at once. Nothing is acknowledged in the delivering beat.
	if len(applied) != 0 || len(failures) != 1 || failures[0].ID != "m3" {
		t.Fatalf("applied = %v failures = %v, want m1 and m2 queued and m3 failed", applied, failures)
	}
	acks := drainUntil(t, d, 2)
	if strings.Join(acks, ",") != "m1,m2" {
		t.Fatalf("acknowledged %v, want m1 then m2 in arrival order", acks)
	}
	if calls.Load() != 2 {
		t.Fatalf("scans = %d, want one fresh scan per request", calls.Load())
	}
}

// countingFeed counts the scans that probed.
func countingFeed(calls *atomic.Int32) quotaProbeFunc {
	feed := codexFeed("acct-a", false)
	return func(ctx context.Context, q *quotaState) {
		calls.Add(1)
		feed(ctx, q)
	}
}

// drainUntil drains deferred acknowledgements until want ids have arrived.
func drainUntil(t *testing.T, d *Daemon, want int) []string {
	t.Helper()
	var acks []string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(acks) < want {
		applied, failures, _ := d.drainDeferredScanAcks()
		if len(failures) > 0 {
			t.Fatalf("unexpected scan failures %v", failures)
		}
		acks = append(acks, applied...)
		time.Sleep(10 * time.Millisecond)
	}
	if len(acks) < want {
		t.Fatalf("acknowledged %v, want %d acknowledgements", acks, want)
	}
	return acks
}

func TestObservationsScanMutation_WaitsOutALongInFlightScan(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = true
	d.observationScan.mu.Unlock()
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want the request queued", applied, failures)
	}
	// An operator scan that probes for longer than any retry budget: the
	// queued request keeps waiting, it never fails while the slot is busy.
	time.Sleep(600 * time.Millisecond)
	if applied, failures, _ := d.drainDeferredScanAcks(); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("drained %v %v while the other scan was still running", applied, failures)
	}
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = false
	d.observationScan.mu.Unlock()
	if acks := drainUntil(t, d, 1); strings.Join(acks, ",") != "m1" || calls.Load() != 1 {
		t.Fatalf("acknowledged %v after %d scans, want m1 after its own scan", acks, calls.Load())
	}
}

func TestObservationsScanMutation_RedeliveryStartsNoSecondScan(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = 150 * time.Millisecond
	if _, err := d.ScanObservations(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "same-id", Op: observationsScanOp}})
	}
	acks := drainUntil(t, d, 1)
	time.Sleep(300 * time.Millisecond)
	more, _, _ := d.drainDeferredScanAcks()
	if strings.Join(append(acks, more...), ",") != "same-id" || calls.Load() != 2 {
		t.Fatalf("acknowledged %v after %d scans, want one acknowledgement and one scan beyond the operator's", append(acks, more...), calls.Load())
	}
	// A redelivery after the acknowledgement starts no scan either.
	d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "same-id", Op: observationsScanOp}})
	time.Sleep(300 * time.Millisecond)
	if late, _, _ := d.drainDeferredScanAcks(); len(late) != 0 || calls.Load() != 2 {
		t.Fatalf("late redelivery acknowledged %v after %d scans, want none", late, calls.Load())
	}
}

func TestObservationsScanMutation_AcknowledgesWithItsOwnEmptyReport(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{})
	d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "cold", Op: observationsScanOp}})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		applied, _, report := d.drainDeferredScanAcks()
		if len(applied) == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if strings.Join(applied, ",") != "cold" || report == nil || !report.Full || len(report.Items) != 0 {
			t.Fatalf("drained %v with report %+v, want cold with the full empty report of its scan", applied, report)
		}
		return
	}
	t.Fatal("no acknowledgement arrived")
}

func TestObservationScanEndpoint_RefusesTrailingJSON(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	s := &Server{daemon: d}
	post := func(body string) int {
		rec := httptest.NewRecorder()
		s.handleObservationScan(rec, httptest.NewRequest(http.MethodPost, "/api/daemon/observations/scan", strings.NewReader(body)))
		return rec.Code
	}
	if code := post(`{"kinds":[]}{"kinds":["unsupported"]}`); code != http.StatusBadRequest {
		t.Fatalf("concatenated objects: status %d, want 400", code)
	}
	if code := post(`{"kinds":["subscription-login"]}`); code != http.StatusOK {
		t.Fatalf("valid request after a refused one: status %d, want 200 (the refusal took no scan slot)", code)
	}
}

func heartbeatCapture(t *testing.T, statuses ...int) (*httptest.Server, chan []byte) {
	t.Helper()
	bodies := make(chan []byte, 4)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		bodies <- buf
		if i := int(n.Add(1)) - 1; i < len(statuses) && statuses[i] != http.StatusOK {
			w.WriteHeader(statuses[i])
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

type capturedBeat struct {
	AppliedMutations []string `json:"appliedMutations"`
	Observations     *struct {
		Full  bool             `json:"full"`
		Items []map[string]any `json:"items"`
	} `json:"observations"`
}

func TestHeartbeat_DrainsDeferredAcksBeforeBuildingObservations(t *testing.T) {
	var order []string
	srv, bodies := heartbeatCapture(t)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr_obs", Hostname: "h", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
		GetStatus: func() RegistrationStatus { return RegistrationIdle },
		DrainDeferredAcks: func() ([]string, []HeartbeatMutationFailure, *observation.Report) {
			order = append(order, "drain")
			return []string{"m9"}, nil, nil
		},
		GetObservations: func() *observation.Report {
			order = append(order, "observations")
			return nil
		},
	})
	hs.sendOne(context.Background())
	var beat capturedBeat
	if err := json.Unmarshal(<-bodies, &beat); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "drain,observations" {
		t.Fatalf("order = %v, want the deferred acks drained before the observations report is built", order)
	}
	if strings.Join(beat.AppliedMutations, ",") != "m9" {
		t.Fatalf("appliedMutations = %v, want m9 in the same beat", beat.AppliedMutations)
	}
}

func TestHeartbeat_SendsTheScanReportWithItsAcknowledgementUntilAccepted(t *testing.T) {
	empty, err := observation.NewReport(time.Now(), true, observationKinds, nil)
	if err != nil {
		t.Fatal(err)
	}
	drained := false
	srv, bodies := heartbeatCapture(t, http.StatusInternalServerError, http.StatusOK)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr_obs", Hostname: "h", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
		GetStatus: func() RegistrationStatus { return RegistrationIdle },
		DrainDeferredAcks: func() ([]string, []HeartbeatMutationFailure, *observation.Report) {
			if drained {
				return nil, nil, nil
			}
			drained = true
			return []string{"cold"}, nil, empty
		},
		// The generic report omits observations on a host with no logins.
		GetObservations: func() *observation.Report { return nil },
	})
	for attempt := 0; attempt < 2; attempt++ {
		hs.sendOne(context.Background())
		var beat capturedBeat
		if err := json.Unmarshal(<-bodies, &beat); err != nil {
			t.Fatal(err)
		}
		if strings.Join(beat.AppliedMutations, ",") != "cold" || beat.Observations == nil || !beat.Observations.Full || len(beat.Observations.Items) != 0 {
			t.Fatalf("beat %d: applied %v observations %+v, want cold with its full empty scan report (retried after a failed POST)", attempt, beat.AppliedMutations, beat.Observations)
		}
	}
	hs.sendOne(context.Background())
	var after capturedBeat
	if err := json.Unmarshal(<-bodies, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.AppliedMutations) != 0 || after.Observations != nil {
		t.Fatalf("after acceptance: applied %v observations %+v, want neither", after.AppliedMutations, after.Observations)
	}
}

func TestObservationsReadEndpointDoesNotScan(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	rec := httptest.NewRecorder()
	(&Server{daemon: d}).handleObservations(rec, httptest.NewRequest(http.MethodGet, "/api/daemon/observations", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) || !d.observationScan.lastAt.IsZero() {
		t.Fatalf("read = %d %s (scanned %v), want 200 with an empty full report and no scan", rec.Code, rec.Body, !d.observationScan.lastAt.IsZero())
	}
}

// scanBatch is one drained observations.scan batch: the ids one scan
// answered plus that scan's exact report.
type scanBatch struct {
	applied  []string
	failures []HeartbeatMutationFailure
	report   *observation.Report
}

// drainOneScanBatch drains the oldest completed scan batch through the
// production heartbeat handoff.
func drainOneScanBatch(t *testing.T, d *Daemon) scanBatch {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		applied, failures, report := d.drainDeferredScanAcks()
		if len(applied) == 0 && len(failures) == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return scanBatch{applied: applied, failures: failures, report: report}
	}
	t.Fatal("no scan batch drained")
	return scanBatch{}
}

func TestObservationsScanMutation_HoldsLaterAckUntilItsOwnScan(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = time.Millisecond
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "m1", Op: observationsScanOp},
		{ID: "m2", Op: observationsScanOp},
	}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want both requests queued", applied, failures)
	}
	first := drainOneScanBatch(t, d)
	if strings.Join(first.applied, ",") != "m1" || first.report == nil {
		t.Fatalf("first batch = %v report %v, want [m1] with its own scan report", first.applied, first.report != nil)
	}
	second := drainOneScanBatch(t, d)
	if strings.Join(second.applied, ",") != "m2" || second.report == nil {
		t.Fatalf("second batch = %v report %v, want [m2] with its own scan report", second.applied, second.report != nil)
	}
	if first.report == second.report {
		t.Fatal("both batches share one report pointer, want one exact report per scan")
	}
	if calls.Load() != 2 {
		t.Fatalf("scans = %d, want one fresh scan per request", calls.Load())
	}
}

func TestObservationsScanMutation_StopCancelsQueuedScan(t *testing.T) {
	d := New(Options{})
	d.observationScan.probes = map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)}
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = true
	d.observationScan.mu.Unlock()
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want the request queued", applied, failures)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, failures, _ := d.drainDeferredScanAcks()
		if len(failures) == 1 && failures[0].ID == "m1" {
			if !strings.Contains(failures[0].Error, "stopping") {
				t.Fatalf("failure = %q, want a stop-cancelled failure", failures[0].Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no cancelled failure drained, failures = %v", failures)
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.observationScan.mu.Lock()
	working, queued := d.observationScan.working, len(d.observationScan.queue)
	d.observationScan.mu.Unlock()
	if working || queued != 0 {
		t.Fatalf("working = %v queued = %d, want the worker exited with an empty queue", working, queued)
	}
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m2", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 1 || failures[0].ID != "m2" {
		t.Fatalf("post-stop apply = %v %v, want m2 refused fail-closed", applied, failures)
	}
}

func TestObservationsScanMutation_QueueFullFailsTyped(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	d.observationScan.maxQueue = 2
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = true
	d.observationScan.mu.Unlock()
	mk := func(id string) PendingMutation { return PendingMutation{ID: id, Op: observationsScanOp} }
	if _, failures := d.applyPendingMutations(context.Background(), []PendingMutation{mk("m1")}); len(failures) != 0 {
		t.Fatalf("m1 failures = %v, want queued", failures)
	}
	if _, failures := d.applyPendingMutations(context.Background(), []PendingMutation{mk("m2")}); len(failures) != 0 {
		t.Fatalf("m2 failures = %v, want queued", failures)
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{mk("m3")})
	if len(applied) != 0 || len(failures) != 1 || failures[0].ID != "m3" || !strings.Contains(failures[0].Error, "queue full") {
		t.Fatalf("m3 applied = %v failures = %v, want a typed queue-full failure", applied, failures)
	}
	d.observationScan.minInterval = time.Millisecond
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = false
	d.observationScan.mu.Unlock()
	if acks := drainUntil(t, d, 2); strings.Join(acks, ",") != "m1,m2" {
		t.Fatalf("acknowledged %v, want m1 then m2", acks)
	}
}

func TestObservationsScanMutation_RedeliveryReplaysOutcomeAfterHandoff(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = time.Millisecond
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want the request queued", applied, failures)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.observationScan.mu.Lock()
		_, ok := d.observationScan.done[observationScanKey{id: "m1"}]
		d.observationScan.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The scan completed long ago but the heartbeat only just took the
	// acknowledgement: age the completion past the window, then drain.
	d.observationScan.mu.Lock()
	outcome := d.observationScan.done[observationScanKey{id: "m1"}]
	outcome.at = time.Now().Add(-(observationScanRecentTTL + time.Minute))
	d.observationScan.done[observationScanKey{id: "m1"}] = outcome
	d.observationScan.mu.Unlock()
	batch := drainOneScanBatch(t, d)
	if strings.Join(batch.applied, ",") != "m1" || len(batch.failures) != 0 || batch.report == nil {
		t.Fatalf("drained %v %v, want [m1] with its scan report", batch.applied, batch.failures)
	}
	// A redelivery after the handoff absorbs instead of starting a second
	// scan, because the window runs from the handoff. It absorbs as
	// deferred rather than replaying a bare applied: the acknowledgement
	// already rides its own batch's drain with its own report, and a bare
	// replay would merge with the next scan's report on a later beat.
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("redelivery applied = %v failures = %v, want absorbed after the handoff", applied, failures)
	}
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("scans = %d, want no second scan for the redelivery", calls.Load())
	}
}

func TestObservationsScanMutation_ValidatesAfterDedupeAndRejectsEmptyID(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "", Op: observationsScanOp}})
	if len(applied) != 0 || len(failures) != 1 || !strings.Contains(failures[0].Error, "requires an id") {
		t.Fatalf("empty id applied = %v failures = %v, want one requires-an-id failure", applied, failures)
	}
	d.observationScan.mu.Lock()
	queued := len(d.observationScan.queue)
	d.observationScan.mu.Unlock()
	if queued != 0 {
		t.Fatalf("queued = %d after an empty id, want nothing queued", queued)
	}
	// Hold the worker in the slot so the queued request stays queued.
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = true
	d.observationScan.mu.Unlock()
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("m1 applied = %v failures = %v, want queued", applied, failures)
	}
	// A redelivery carrying invalid params must not re-report validation:
	// the dedupe check runs first, so it stays deferred.
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp, Params: json.RawMessage(`{"kinds":["bogus"]}`)}})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("redelivery applied = %v failures = %v, want absorbed without re-validation", applied, failures)
	}
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = false
	d.observationScan.mu.Unlock()
	if acks := drainUntil(t, d, 1); strings.Join(acks, ",") != "m1" {
		t.Fatalf("acknowledged %v, want m1", acks)
	}
}

func TestObservationsScanMutation_ValidationFailureReplaysIdentically(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	// An invalid kind fails at once and records its exact failure text.
	_, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "m1", Op: observationsScanOp, Params: json.RawMessage(`{"kinds":["bogus"]}`)},
	})
	if len(failures) != 1 || failures[0].ID != "m1" || !strings.Contains(failures[0].Error, `unknown observation kind "bogus"`) {
		t.Fatalf("first failures = %v, want m1 failing on bogus", failures)
	}
	if calls.Load() != 0 {
		t.Fatalf("scans = %d after a validation failure, want none", calls.Load())
	}
	want := failures[0].Error
	// A redelivery with valid params replays the original failure instead of
	// starting a scan.
	_, failures = d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}})
	if len(failures) != 1 || failures[0].ID != "m1" || failures[0].Error != want {
		t.Fatalf("valid redelivery failures = %v, want replay of %q", failures, want)
	}
	// A redelivery with a different invalid kind still replays the original
	// failure: the dedupe check runs before validation, so it never
	// re-reports validation differently.
	_, failures = d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "m1", Op: observationsScanOp, Params: json.RawMessage(`{"kinds":["other-bogus"]}`)},
	})
	if len(failures) != 1 || failures[0].ID != "m1" || failures[0].Error != want {
		t.Fatalf("other-bogus redelivery failures = %v, want replay of %q", failures, want)
	}
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("scans = %d after redeliveries, want no second scan", calls.Load())
	}
	d.observationScan.mu.Lock()
	queued := len(d.observationScan.queue)
	d.observationScan.mu.Unlock()
	if queued != 0 {
		t.Fatalf("queued = %d after validation redeliveries, want nothing queued", queued)
	}
}

func TestObservationsScanMutation_StopCancelsLongSlotWait(t *testing.T) {
	d := New(Options{})
	d.observationScan.probes = map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)}
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	// Park the next scan behind the rate-limit window, not just a short
	// in-flight probe: the slot wait must observe the stop instead of
	// sleeping out the window.
	d.observationScan.mu.Lock()
	d.observationScan.minInterval = time.Minute
	d.observationScan.lastAt = time.Now()
	d.observationScan.mu.Unlock()
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want the request queued behind the rate window", applied, failures)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- d.Stop(context.Background()) }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s while a scan waited out the rate window")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, failures, _ := d.drainDeferredScanAcks()
		if len(failures) == 1 && failures[0].ID == "m1" {
			if !strings.Contains(failures[0].Error, "stopping") {
				t.Fatalf("failure = %q, want a stop-cancelled failure", failures[0].Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no cancelled failure drained, failures = %v", failures)
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.observationScan.mu.Lock()
	working, queued := d.observationScan.working, len(d.observationScan.queue)
	d.observationScan.mu.Unlock()
	if working || queued != 0 {
		t.Fatalf("working = %v queued = %d, want the worker exited with an empty queue", working, queued)
	}
}

func TestHeartbeat_HoldsNextScanBatchUntilPriorBeatAccepted(t *testing.T) {
	r1, err := observation.NewReport(time.Now().Add(-time.Hour).Truncate(time.Second), true, observationKinds, nil)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := observation.NewReport(time.Now().Truncate(time.Second), true, observationKinds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.ScannedAt == r2.ScannedAt {
		t.Fatal("test reports share a scan time, want distinct reports")
	}
	var drains atomic.Int32
	srv, bodies := heartbeatCapture(t, http.StatusInternalServerError, http.StatusOK, http.StatusOK)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr_obs", Hostname: "h", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
		GetStatus: func() RegistrationStatus { return RegistrationIdle },
		DrainDeferredAcks: func() ([]string, []HeartbeatMutationFailure, *observation.Report) {
			switch drains.Add(1) {
			case 1:
				return []string{"m1"}, nil, r1
			case 2:
				return []string{"m2"}, nil, r2
			default:
				return nil, nil, nil
			}
		},
		GetObservations: func() *observation.Report { return nil },
	})
	type beatObs struct {
		ScannedAt string `json:"scannedAt"`
	}
	type beat struct {
		AppliedMutations []string `json:"appliedMutations"`
		Observations     *beatObs `json:"observations"`
	}
	next := func() beat {
		t.Helper()
		select {
		case buf := <-bodies:
			var b beat
			if err := json.Unmarshal(buf, &b); err != nil {
				t.Fatal(err)
			}
			return b
		case <-time.After(3 * time.Second):
			t.Fatal("no heartbeat arrived")
			return beat{}
		}
	}
	// First beat fails to POST: m1 with its own report stays pending.
	hs.sendOne(context.Background())
	b1 := next()
	if strings.Join(b1.AppliedMutations, ",") != "m1" || b1.Observations == nil || b1.Observations.ScannedAt != r1.ScannedAt {
		t.Fatalf("beat1 = %v %v, want m1 with its own report", b1.AppliedMutations, b1.Observations)
	}
	// Second beat retries m1 with r1 instead of merging m2 with r2.
	hs.sendOne(context.Background())
	b2 := next()
	if strings.Join(b2.AppliedMutations, ",") != "m1" || b2.Observations == nil || b2.Observations.ScannedAt != r1.ScannedAt {
		t.Fatalf("beat2 = %v %v, want a retry of m1 with its own report", b2.AppliedMutations, b2.Observations)
	}
	// After the retry is accepted the held batch drains with its own report.
	hs.sendOne(context.Background())
	b3 := next()
	if strings.Join(b3.AppliedMutations, ",") != "m2" || b3.Observations == nil || b3.Observations.ScannedAt != r2.ScannedAt {
		t.Fatalf("beat3 = %v %v, want m2 with its own report", b3.AppliedMutations, b3.Observations)
	}
}

func TestApplyObservationMutations_AbsorbsDeferredScan(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = true
	d.observationScan.mu.Unlock()
	apply := d.ApplyObservationMutations("")
	applied, failures := apply(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want a queued scan absorbed with no acknowledgement yet", applied, failures)
	}
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = false
	d.observationScan.mu.Unlock()
	if acks := drainUntil(t, d, 1); strings.Join(acks, ",") != "m1" {
		t.Fatalf("acknowledged %v, want m1 once its scan completes", acks)
	}
}

func TestObservationsScanMutation_RejectsWhitespaceID(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "   ", Op: observationsScanOp}})
	if len(applied) != 0 || len(failures) != 1 || !strings.Contains(failures[0].Error, "requires an id") {
		t.Fatalf("whitespace id applied = %v failures = %v, want one requires-an-id failure", applied, failures)
	}
	d.observationScan.mu.Lock()
	queued := len(d.observationScan.queue)
	d.observationScan.mu.Unlock()
	if queued != 0 {
		t.Fatalf("queued = %d after a whitespace id, want nothing queued", queued)
	}
}

func TestObservationsScanMutation_BoundsRetainedOutcomes(t *testing.T) {
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	for i := 0; i < observationScanMaxRetainedOutcomes+32; i++ {
		mid := fmt.Sprintf("bound-m-%04d", i)
		_, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
			{ID: mid, Op: observationsScanOp, Params: json.RawMessage(`{"kinds":["bogus"]}`)},
		})
		if len(failures) != 1 {
			t.Fatalf("seed %d failures = %v, want one validation failure", i, failures)
		}
	}
	d.observationScan.mu.Lock()
	n := len(d.observationScan.done)
	d.observationScan.mu.Unlock()
	if n > observationScanMaxRetainedOutcomes {
		t.Fatalf("retained outcomes = %d, want at most %d", n, observationScanMaxRetainedOutcomes)
	}
}

func TestObservationsScanMutation_FailedStopLeavesScansEnabled(t *testing.T) {
	d := New(Options{})
	d.observationScan.probes = map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)}
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	// Hold the slot behind a running scan so the queued request is still
	// pending when Stop runs.
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = true
	d.observationScan.mu.Unlock()
	// Hold the lifecycle so Stop cannot acquire it.
	lease, err := d.claimLifecycle(context.Background(), lifecyclePause)
	if err != nil {
		t.Fatalf("claim lifecycle: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	stopErr := d.Stop(ctx)
	cancel()
	if stopErr == nil {
		d.releaseLifecycle(lease)
		t.Fatal("Stop without the lease: want an error")
	}
	// A Stop that never acquired the lease must leave scans enabled.
	d.observationScan.mu.Lock()
	stopped := d.observationScan.stopped
	d.observationScan.mu.Unlock()
	if stopped {
		d.releaseLifecycle(lease)
		t.Fatal("failed Stop disabled scans, want the scanner still enabled")
	}
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		d.releaseLifecycle(lease)
		t.Fatalf("post-failure apply = %v %v, want the request queued", applied, failures)
	}
	d.releaseLifecycle(lease)
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, failures, _ := d.drainDeferredScanAcks()
		if len(failures) == 1 && failures[0].ID == "m1" {
			if !strings.Contains(failures[0].Error, "stopping") {
				t.Fatalf("failure = %q, want a stop-cancelled failure", failures[0].Error)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no cancelled failure drained, failures = %v", failures)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestObservationsScanMutation_UndrainedBatchAbsorbsRedelivery(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = time.Millisecond
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want the request queued", applied, failures)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.observationScan.mu.Lock()
		_, ok := d.observationScan.done[observationScanKey{id: "m1"}]
		d.observationScan.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Age the outcome past the window without draining: the completed
	// batch still awaits its heartbeat handoff.
	d.observationScan.mu.Lock()
	outcome := d.observationScan.done[observationScanKey{id: "m1"}]
	outcome.at = time.Now().Add(-(observationScanRecentTTL + time.Minute))
	d.observationScan.done[observationScanKey{id: "m1"}] = outcome
	d.observationScan.mu.Unlock()
	// A redelivery before the handoff must be absorbed, not rescanned.
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("redelivery applied = %v failures = %v, want absorbed while its batch awaits drain", applied, failures)
	}
	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("scans = %d, want no second scan for an undrained id", calls.Load())
	}
	batch := drainOneScanBatch(t, d)
	if strings.Join(batch.applied, ",") != "m1" || batch.report == nil {
		t.Fatalf("drained %v with report %v, want the single [m1] batch with its scan report", batch.applied, batch.report != nil)
	}
}

func TestObservationsScanMutation_DrainRerecordsPrunedOutcome(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = time.Millisecond
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want the request queued", applied, failures)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.observationScan.mu.Lock()
		_, ok := d.observationScan.done[observationScanKey{id: "m1"}]
		d.observationScan.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Age the outcome past the window, then prune it with an unrelated
	// request while the batch still awaits its handoff.
	d.observationScan.mu.Lock()
	outcome := d.observationScan.done[observationScanKey{id: "m1"}]
	outcome.at = time.Now().Add(-(observationScanRecentTTL + time.Minute))
	d.observationScan.done[observationScanKey{id: "m1"}] = outcome
	d.observationScan.mu.Unlock()
	if _, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "other", Op: observationsScanOp, Params: json.RawMessage(`{"kinds":["bogus"]}`)},
	}); len(failures) != 1 {
		t.Fatalf("other failures = %v, want one validation failure", failures)
	}
	// The handoff re-records the pruned outcome, so a later redelivery
	// absorbs instead of starting a second scan.
	batch := drainOneScanBatch(t, d)
	if strings.Join(batch.applied, ",") != "m1" || batch.report == nil {
		t.Fatalf("drained %v with report %v, want [m1] with its scan report", batch.applied, batch.report != nil)
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("redelivery applied = %v failures = %v, want absorbed after the handoff", applied, failures)
	}
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("scans = %d, want no second scan after the handoff", calls.Load())
	}
}

func TestHeartbeat_ScanFailureAckCarriesNoForeignReport(t *testing.T) {
	generic, err := observation.NewReport(time.Now(), true, observationKinds, nil)
	if err != nil {
		t.Fatal(err)
	}
	drained := false
	srv, bodies := heartbeatCapture(t)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr_obs", Hostname: "h", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
		GetStatus: func() RegistrationStatus { return RegistrationIdle },
		DrainDeferredAcks: func() ([]string, []HeartbeatMutationFailure, *observation.Report) {
			if drained {
				return nil, nil, nil
			}
			drained = true
			return nil, []HeartbeatMutationFailure{{ID: "m1", Error: "observations.scan: boom"}}, nil
		},
		GetObservations: func() *observation.Report { return generic },
	})
	type failBeat struct {
		AppliedMutations []string                   `json:"appliedMutations"`
		MutationFailures []HeartbeatMutationFailure `json:"mutationFailures"`
		Observations     *struct {
			Full bool `json:"full"`
		} `json:"observations"`
	}
	next := func() failBeat {
		t.Helper()
		select {
		case buf := <-bodies:
			var b failBeat
			if err := json.Unmarshal(buf, &b); err != nil {
				t.Fatal(err)
			}
			return b
		case <-time.After(3 * time.Second):
			t.Fatal("no heartbeat arrived")
			return failBeat{}
		}
	}
	hs.sendOne(context.Background())
	first := next()
	if len(first.MutationFailures) != 1 || first.MutationFailures[0].ID != "m1" {
		t.Fatalf("beat1 failures = %v, want [m1]", first.MutationFailures)
	}
	if first.Observations != nil {
		t.Fatalf("beat1 observations = %+v, want none: a scan failure carries no report of its own", first.Observations)
	}
	// Once the failure batch is confirmed the generic report rides again.
	hs.sendOne(context.Background())
	second := next()
	if len(second.MutationFailures) != 0 {
		t.Fatalf("beat2 failures = %v, want none after confirmation", second.MutationFailures)
	}
	if second.Observations == nil {
		t.Fatal("beat2 observations = nil, want the generic report after confirmation")
	}
}

func TestObservationsScanMutation_SatelliteCompletesOnlyOnOwningRegistration(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = time.Millisecond
	satellite := d.ApplyObservationMutations("satellite")
	if applied, failures := satellite(context.Background(), []PendingMutation{{ID: "sat-scan", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("satellite apply = %v %v, want queued with no acknowledgement", applied, failures)
	}
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "pri-scan", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("primary apply = %v %v, want queued with no acknowledgement", applied, failures)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.observationScan.mu.Lock()
		_, satOK := d.observationScan.done[observationScanKey{registration: "satellite", id: "sat-scan"}]
		_, priOK := d.observationScan.done[observationScanKey{id: "pri-scan"}]
		d.observationScan.mu.Unlock()
		if satOK && priOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("both scans did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The primary lane drains first: it must receive only its own batch,
	// never the satellite's. A shared drain would hand sat-scan to the
	// primary beat under the primary registration.
	priApplied, _, priReport := d.drainDeferredScanAcks()
	if strings.Join(priApplied, ",") != "pri-scan" || priReport == nil {
		t.Fatalf("primary drain = %v report %v, want [pri-scan] with its own report", priApplied, priReport != nil)
	}
	satApplied, _, satReport := d.DrainDeferredScanAcksFor("satellite")
	if strings.Join(satApplied, ",") != "sat-scan" || satReport == nil {
		t.Fatalf("satellite drain = %v report %v, want [sat-scan] with its own report", satApplied, satReport != nil)
	}
	if satReport == priReport {
		t.Fatal("both registrations share one report pointer, want one exact report per scan")
	}
	if applied, _, _ := d.drainDeferredScanAcks(); len(applied) != 0 {
		t.Fatalf("primary redrain = %v, want nothing left", applied)
	}
	if applied, _, _ := d.DrainDeferredScanAcksFor("satellite"); len(applied) != 0 {
		t.Fatalf("satellite redrain = %v, want nothing left", applied)
	}
	// The same id on two registrations is two independent requests.
	if applied, failures := satellite(context.Background(), []PendingMutation{{ID: "dup", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("satellite dup apply = %v %v, want queued", applied, failures)
	}
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "dup", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("primary dup apply = %v %v, want queued independently", applied, failures)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		d.observationScan.mu.Lock()
		_, satOK := d.observationScan.done[observationScanKey{registration: "satellite", id: "dup"}]
		_, priOK := d.observationScan.done[observationScanKey{id: "dup"}]
		d.observationScan.mu.Unlock()
		if satOK && priOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("both dup scans did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	satApplied, _, _ = d.DrainDeferredScanAcksFor("satellite")
	priApplied, _, _ = d.drainDeferredScanAcks()
	if strings.Join(satApplied, ",") != "dup" || strings.Join(priApplied, ",") != "dup" {
		t.Fatalf("dup drains = satellite %v primary %v, want [dup] on each", satApplied, priApplied)
	}
	if calls.Load() != 4 {
		t.Fatalf("scans = %d, want one fresh scan per request across registrations", calls.Load())
	}
}

func TestObservationsScanMutation_HandedOffSuccessAbsorbsToPreserveReportFidelity(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = time.Millisecond
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "first-scan", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want the request queued", applied, failures)
	}
	first := drainOneScanBatch(t, d)
	if strings.Join(first.applied, ",") != "first-scan" || first.report == nil {
		t.Fatalf("first batch = %v report %v, want [first-scan] with its report", first.applied, first.report != nil)
	}
	// The handoff already acknowledged first-scan on its own beat with its
	// own report. A redelivery alongside the next scan must absorb, not
	// buffer a bare acknowledgement that would ride the next scan's report.
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "first-scan", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("redelivery applied = %v failures = %v, want absorbed after the handoff", applied, failures)
	}
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "second-scan", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("second apply = %v %v, want queued", applied, failures)
	}
	second := drainOneScanBatch(t, d)
	if strings.Join(second.applied, ",") != "second-scan" || second.report == nil {
		t.Fatalf("second batch = %v report %v, want only [second-scan] with its own report", second.applied, second.report != nil)
	}
	if first.report == second.report {
		t.Fatal("both batches share one report pointer, want one exact report per scan")
	}
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 2 {
		t.Fatalf("scans = %d, want no second scan for the redelivery", calls.Load())
	}
}

// waitScanOutcome waits until the scan for id has recorded its outcome
// without draining its acknowledgement batch.
func waitScanOutcome(t *testing.T, d *Daemon, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.observationScan.mu.Lock()
		_, ok := d.observationScan.done[observationScanKey{id: id}]
		d.observationScan.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan for %s did not complete", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestObservationsScanMutation_StopRefusesOnlyNewIDs pins that daemon stop
// refuses only ids it has never seen: a redelivered id that is queued or
// completed-but-undrained keeps its single decided outcome instead of
// gaining a second, contradicting cancellation failure.
func TestObservationsScanMutation_StopRefusesOnlyNewIDs(t *testing.T) {
	tests := []struct {
		name string
		// queued keeps the scan slot busy so the request is still queued
		// when the daemon stops; otherwise the scan completes first.
		queued       bool
		wantApplied  []string
		wantFailures int
	}{
		{name: "completed undrained id is absorbed", wantApplied: []string{"m1"}},
		{name: "queued id gets exactly one cancellation", queued: true, wantFailures: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
			d.observationScan.minInterval = time.Millisecond
			if tt.queued {
				d.observationScan.mu.Lock()
				d.observationScan.inFlight = true
				d.observationScan.mu.Unlock()
			}
			if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}}); len(applied) != 0 || len(failures) != 0 {
				t.Fatalf("applied = %v failures = %v, want the request queued", applied, failures)
			}
			if !tt.queued {
				waitScanOutcome(t, d, "m1")
			}
			d.cancelObservationScans()
			var gotApplied []string
			var gotFailures []HeartbeatMutationFailure
			applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}})
			gotApplied = append(gotApplied, applied...)
			gotFailures = append(gotFailures, failures...)
			batch := drainOneScanBatch(t, d)
			gotApplied = append(gotApplied, batch.applied...)
			gotFailures = append(gotFailures, batch.failures...)
			// Let the worker finish, then confirm nothing else is owed.
			deadline := time.Now().Add(3 * time.Second)
			for {
				d.observationScan.mu.Lock()
				working := d.observationScan.working
				d.observationScan.mu.Unlock()
				if !working {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("scan worker did not exit after stop")
				}
				time.Sleep(10 * time.Millisecond)
			}
			for {
				applied, failures, _ := d.drainDeferredScanAcks()
				if len(applied) == 0 && len(failures) == 0 {
					break
				}
				gotApplied = append(gotApplied, applied...)
				gotFailures = append(gotFailures, failures...)
			}
			if strings.Join(gotApplied, ",") != strings.Join(tt.wantApplied, ",") {
				t.Fatalf("applied = %v, want %v", gotApplied, tt.wantApplied)
			}
			if len(gotFailures) != tt.wantFailures {
				t.Fatalf("failures = %v, want %d", gotFailures, tt.wantFailures)
			}
			for _, f := range gotFailures {
				if f.ID != "m1" || !strings.Contains(f.Error, "stopping") {
					t.Fatalf("failure = %+v, want a stop cancellation for m1", f)
				}
			}
			if !tt.queued && calls.Load() != 1 {
				t.Fatalf("scans = %d, want the single original scan", calls.Load())
			}
		})
	}
}

// TestHeartbeat_StopNeverReportsAScanBothAppliedAndFailed drives the real
// heartbeat wire across daemon stop: two scans complete, the daemon stops
// before the second batch is handed off, and the control plane redelivers
// the second id. No beat may list one id as both applied and failed, and
// the redelivered id is acknowledged exactly once, as applied.
func TestHeartbeat_StopNeverReportsAScanBothAppliedAndFailed(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = time.Millisecond
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "scan-0", Op: observationsScanOp},
		{ID: "scan-1", Op: observationsScanOp},
	}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want both requests queued", applied, failures)
	}
	waitScanOutcome(t, d, "scan-0")
	waitScanOutcome(t, d, "scan-1")
	// What Daemon.Stop runs before the heartbeat stops.
	d.cancelObservationScans()

	bodies := make(chan []byte, 8)
	var beats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		bodies <- buf
		resp := map[string]any{"acknowledged": true}
		if beats.Add(1) == 1 {
			// scan-1 is not yet acknowledged, so the control plane
			// redelivers it.
			resp["pendingMutations"] = []PendingMutation{{ID: "scan-1", Op: observationsScanOp}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr_obs", Hostname: "h", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
		GetStatus:          func() RegistrationStatus { return RegistrationIdle },
		DrainDeferredAcks:  d.drainDeferredScanAcks,
		OnPendingMutations: d.applyPendingMutations,
		GetObservations:    func() *observation.Report { return nil },
	})
	type wireBeat struct {
		AppliedMutations []string                   `json:"appliedMutations"`
		MutationFailures []HeartbeatMutationFailure `json:"mutationFailures"`
	}
	applied := map[string]int{}
	failed := map[string]int{}
	for i := 1; i <= 3; i++ {
		hs.sendOne(context.Background())
		var beat wireBeat
		select {
		case buf := <-bodies:
			if err := json.Unmarshal(buf, &beat); err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("beat %d: no heartbeat arrived", i)
		}
		inBeat := map[string]bool{}
		for _, id := range beat.AppliedMutations {
			inBeat[id] = true
			applied[id]++
		}
		for _, f := range beat.MutationFailures {
			if inBeat[f.ID] {
				t.Fatalf("beat %d lists %s as both applied and failed: applied=%v failures=%v", i, f.ID, beat.AppliedMutations, beat.MutationFailures)
			}
			failed[f.ID]++
		}
	}
	for _, id := range []string{"scan-0", "scan-1"} {
		if applied[id] != 1 || failed[id] != 0 {
			t.Fatalf("%s: applied %d failed %d times, want exactly one applied acknowledgement", id, applied[id], failed[id])
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("scans = %d, want one scan per request", calls.Load())
	}
}
