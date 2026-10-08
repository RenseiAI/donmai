package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// quotaReportCapture is a fake daemon usage route: it records every
// POST body the reporter delivers.
type quotaReportCapture struct {
	mu     sync.Mutex
	bodies [][]byte
	paths  []string
	status int
}

func (c *quotaReportCapture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, append([]byte(nil), body...))
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.WriteHeader(c.status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func (c *quotaReportCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func quotaTestUpdate(percent float64) agent.UsageEvent {
	return agent.UsageEvent{Usage: &agent.UsageLimitsUpdate{
		CheckedAt: "2026-10-06T12:00:00Z",
		Windows:   []agent.UsageWindow{{ID: "primary", Kind: agent.UsageWindowWeekly, Label: "Weekly", UsedPercent: percent}},
	}}
}

// TestQuotaReporter_ForwardsUsageEvent drives the production report
// path: one UsageEvent becomes one POST to the session's usage route
// carrying the harness and the sparse update.
func TestQuotaReporter_ForwardsUsageEvent(t *testing.T) {
	t.Parallel()

	capture := &quotaReportCapture{status: http.StatusOK}
	srv := httptest.NewServer(capture.handler())
	t.Cleanup(srv.Close)

	reporter := NewQuotaReporter(srv.Client(), srv.URL, "sess-1", agent.UsageHarnessCodex, "", nil)
	if !reporter.Enabled() {
		t.Fatal("reporter with daemon URL, session and harness must be enabled")
	}
	reporter.ReportUsageEvent(context.Background(), quotaTestUpdate(42))

	if got := capture.count(); got != 1 {
		t.Fatalf("reports = %d, want 1", got)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.paths[0] != "/api/daemon/sessions/sess-1/usage" {
		t.Errorf("path = %q, want the session usage route", capture.paths[0])
	}
	var decoded struct {
		Harness string                  `json:"harness"`
		Update  agent.UsageLimitsUpdate `json:"update"`
	}
	if err := json.Unmarshal(capture.bodies[0], &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if decoded.Harness != agent.UsageHarnessCodex {
		t.Errorf("harness = %q, want codex", decoded.Harness)
	}
	if len(decoded.Update.Windows) != 1 || decoded.Update.Windows[0].UsedPercent != 42 {
		t.Errorf("update = %+v, want the primary row at 42", decoded.Update)
	}
}

// TestQuotaReporter_DedupsConsecutiveDuplicates pins the fan-in
// guard: several live sessions observe the same account
// notification, so consecutive identical updates travel once.
func TestQuotaReporter_DedupsConsecutiveDuplicates(t *testing.T) {
	t.Parallel()

	capture := &quotaReportCapture{status: http.StatusOK}
	srv := httptest.NewServer(capture.handler())
	t.Cleanup(srv.Close)

	reporter := NewQuotaReporter(srv.Client(), srv.URL, "sess-1", agent.UsageHarnessClaude, "", nil)
	reporter.ReportUsageEvent(context.Background(), quotaTestUpdate(7))
	reporter.ReportUsageEvent(context.Background(), quotaTestUpdate(7))
	reporter.ReportUsageEvent(context.Background(), quotaTestUpdate(8))
	reporter.ReportUsageEvent(context.Background(), quotaTestUpdate(8))
	if got := capture.count(); got != 2 {
		t.Errorf("reports = %d, want 2 (one per distinct update)", got)
	}
}

// TestQuotaReporter_DropsNonUsageEvents covers the filter: transcript
// events never reach the daemon, and a disabled reporter sends
// nothing at all.
func TestQuotaReporter_DropsNonUsageEvents(t *testing.T) {
	t.Parallel()

	capture := &quotaReportCapture{status: http.StatusOK}
	srv := httptest.NewServer(capture.handler())
	t.Cleanup(srv.Close)

	reporter := NewQuotaReporter(srv.Client(), srv.URL, "sess-1", agent.UsageHarnessCodex, "", nil)
	reporter.ReportUsageEvent(context.Background(), agent.AssistantTextEvent{Text: "hi"})
	reporter.ReportUsageEvent(context.Background(), agent.UsageEvent{})
	if got := capture.count(); got != 0 {
		t.Errorf("reports = %d, want none for non-quota events", got)
	}
	for _, disabled := range []*QuotaReporter{
		nil,
		NewQuotaReporter(srv.Client(), "", "sess-1", agent.UsageHarnessCodex, "", nil),
		NewQuotaReporter(srv.Client(), srv.URL, "", agent.UsageHarnessCodex, "", nil),
		NewQuotaReporter(srv.Client(), srv.URL, "sess-1", "", "", nil),
	} {
		if disabled.Enabled() {
			t.Error("reporter missing daemon URL, session or harness must be disabled")
		}
		disabled.ReportUsageEvent(context.Background(), quotaTestUpdate(1))
	}
	if got := capture.count(); got != 0 {
		t.Errorf("reports = %d, want none from disabled reporters", got)
	}
}

// quotaTestProvider is a minimal agent.Provider naming one harness,
// for the registration tests below.
type quotaTestProvider struct{ name agent.ProviderName }

func (p quotaTestProvider) Name() agent.ProviderName { return p.name }
func (p quotaTestProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}

func (p quotaTestProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

func (p quotaTestProvider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (p quotaTestProvider) Shutdown(_ context.Context) error { return nil }

// TestRunner_QuotaReporterRegistration pins the per-session wiring:
// the quota harnesses register a reporter for the run, any other
// harness registers nothing, and the release at run end drops it.
func TestRunner_QuotaReporterRegistration(t *testing.T) {
	t.Parallel()

	r := minimalRunner(t)
	var factoryCalls atomic.Int32
	r.quotaReporterForSession = func(sessionID, harness string) *QuotaReporter {
		factoryCalls.Add(1)
		return NewQuotaReporter(nil, "http://127.0.0.1:1", sessionID, harness, "", nil)
	}

	r.registerQuotaReporter("sess-codex", quotaTestProvider{name: agent.ProviderCodex})
	r.registerQuotaReporter("sess-claude", quotaTestProvider{name: agent.ProviderClaude})
	r.registerQuotaReporter("sess-stub", quotaTestProvider{name: "stub"})
	if got := factoryCalls.Load(); got != 2 {
		t.Errorf("factory calls = %d, want 2 (stub registers nothing)", got)
	}
	r.quotaReportersMu.Lock()
	_, codexOK := r.quotaReporters["sess-codex"]
	_, claudeOK := r.quotaReporters["sess-claude"]
	_, stubOK := r.quotaReporters["sess-stub"]
	r.quotaReportersMu.Unlock()
	if !codexOK || !claudeOK || stubOK {
		t.Errorf("reporters: codex=%v claude=%v stub=%v, want true/true/false", codexOK, claudeOK, stubOK)
	}

	r.releaseQuotaReporter("sess-codex")
	r.quotaReportersMu.Lock()
	_, stillThere := r.quotaReporters["sess-codex"]
	r.quotaReportersMu.Unlock()
	if stillThere {
		t.Error("released reporter still registered; reporters must not outlive their run")
	}
}

// TestRunner_ReportQuotaEventReachesSessionReporter is the
// hook-point proof: an event observed mid-stream reaches the
// session's registered reporter exactly once per distinct update.
func TestRunner_ReportQuotaEventReachesSessionReporter(t *testing.T) {
	t.Parallel()

	capture := &quotaReportCapture{status: http.StatusOK}
	srv := httptest.NewServer(capture.handler())
	t.Cleanup(srv.Close)

	r := minimalRunner(t)
	r.quotaReporterForSession = func(sessionID, harness string) *QuotaReporter {
		return NewQuotaReporter(srv.Client(), srv.URL, sessionID, harness, "", nil)
	}
	r.registerQuotaReporter("sess-9", quotaTestProvider{name: agent.ProviderClaude})
	defer r.releaseQuotaReporter("sess-9")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.reportQuotaEvent(ctx, "sess-9", quotaTestUpdate(3))
	r.reportQuotaEvent(ctx, "sess-9", agent.AssistantTextEvent{Text: "nope"})
	r.reportQuotaEvent(ctx, "sess-unknown", quotaTestUpdate(3))
	if got := capture.count(); got != 1 {
		t.Errorf("reports = %d, want 1 (one distinct update on a registered session)", got)
	}
}
