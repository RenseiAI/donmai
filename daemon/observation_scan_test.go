package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if strings.Join(applied, ",") != "m1,m2" || len(failures) != 1 || failures[0].ID != "m3" {
		t.Fatalf("applied = %v failures = %v, want m1,m2 applied and m3 failed", applied, failures)
	}
	if d.observationScan.lastAt.IsZero() || d.quota.snapshot() == nil {
		t.Fatal("observations.scan did not run a scan")
	}
}
