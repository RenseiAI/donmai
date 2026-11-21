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
	d := scanDaemon(map[string]quotaProbeFunc{"codex": codexFeed("acct-a", false)})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "m1", Op: observationsScanOp, Params: json.RawMessage(`{"kinds":["subscription-login"]}`)},
		{ID: "m2", Op: observationsScanOp},
		{ID: "m3", Op: observationsScanOp, Params: json.RawMessage(`{"kinds":["bogus"]}`)},
	})
	// m1 runs a scan and is acknowledged. m2 arrives inside the rate-limit
	// window: it is deferred, never acknowledged against m1's scan.
	if strings.Join(applied, ",") != "m1" || len(failures) != 1 || failures[0].ID != "m3" {
		t.Fatalf("applied = %v failures = %v, want m1 applied, m2 deferred and m3 failed", applied, failures)
	}
	if d.observationScan.lastAt.IsZero() || d.quota.snapshot() == nil {
		t.Fatal("observations.scan did not run a scan")
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

// waitDrained polls the deferred acknowledgements until one arrives.
func waitDrained(t *testing.T, d *Daemon) ([]string, []HeartbeatMutationFailure) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if applied, failures := d.drainDeferredScanAcks(); len(applied) > 0 || len(failures) > 0 {
			return applied, failures
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no deferred acknowledgement arrived")
	return nil, nil
}

func TestObservationsScanMutation_DeferredBehindAnInFlightScan(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = 20 * time.Millisecond
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = true
	d.observationScan.mu.Unlock()
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m1", Op: observationsScanOp}})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want the scan deferred while another is in flight", applied, failures)
	}
	if acks, _ := d.drainDeferredScanAcks(); len(acks) != 0 {
		t.Fatalf("acknowledged %v before any scan ran for the request", acks)
	}
	d.observationScan.mu.Lock()
	d.observationScan.inFlight = false
	d.observationScan.mu.Unlock()
	acks, fails := waitDrained(t, d)
	if strings.Join(acks, ",") != "m1" || len(fails) != 0 {
		t.Fatalf("drained %v %v, want m1 acknowledged after its own scan", acks, fails)
	}
	if calls.Load() != 1 {
		t.Fatalf("scans = %d, want one fresh scan run for the deferred request", calls.Load())
	}
}

func TestObservationsScanMutation_DeferredInsideTheRateWindow(t *testing.T) {
	var calls atomic.Int32
	d := scanDaemon(map[string]quotaProbeFunc{"codex": countingFeed(&calls)})
	d.observationScan.minInterval = 150 * time.Millisecond
	if _, err := d.ScanObservations(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{ID: "m2", Op: observationsScanOp}})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied = %v failures = %v, want the scan deferred inside the rate window", applied, failures)
	}
	acks, fails := waitDrained(t, d)
	if strings.Join(acks, ",") != "m2" || len(fails) != 0 {
		t.Fatalf("drained %v %v, want m2 acknowledged after its own scan", acks, fails)
	}
	if calls.Load() != 2 {
		t.Fatalf("scans = %d, want the earlier scan plus one fresh scan for m2", calls.Load())
	}
}

func TestHeartbeat_DrainsDeferredAcksBeforeBuildingObservations(t *testing.T) {
	var order []string
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		bodies <- buf
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr_obs", Hostname: "h", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
		GetStatus: func() RegistrationStatus { return RegistrationIdle },
		DrainDeferredAcks: func() ([]string, []HeartbeatMutationFailure) {
			order = append(order, "drain")
			return []string{"m9"}, nil
		},
		GetObservations: func() *observation.Report {
			order = append(order, "observations")
			return nil
		},
	})
	hs.sendOne(context.Background())
	var decoded struct {
		AppliedMutations []string `json:"appliedMutations"`
	}
	if err := json.Unmarshal(<-bodies, &decoded); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "drain,observations" {
		t.Fatalf("order = %v, want the deferred acks drained before the observations report is built", order)
	}
	if strings.Join(decoded.AppliedMutations, ",") != "m9" {
		t.Fatalf("appliedMutations = %v, want m9 in the same beat", decoded.AppliedMutations)
	}
}
