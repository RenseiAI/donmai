package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
