package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// quotaReportTimeout bounds one quota-update POST to the daemon. The
// report rides beside the event stream, never in it: a slow daemon
// must not stall the session.
const quotaReportTimeout = 5 * time.Second

// QuotaReporter forwards the sparse quota updates a session's harness
// stream carries (a codex `account/rateLimits/updated` notification,
// a claude `rate_limit_event`) to the daemon that admitted the
// session, where they merge by window id onto the probe snapshot
// behind the heartbeat quota field.
//
// A nil reporter is valid and drops every report: sessions admitted
// without a reachable daemon (tests, standalone runs) keep their
// streams untouched.
type QuotaReporter struct {
	client    *http.Client
	daemonURL string
	sessionID string
	harness   string
	token     string
	logger    *slog.Logger

	mu   sync.Mutex
	last []byte
}

// NewQuotaReporter builds the reporter for one session. daemonURL is
// the admitting daemon's control base URL (empty disables reporting);
// harness names the quota account the updates merge into ("codex" or
// "claude"); token is the optional daemon-control bearer, sent only
// when set. client defaults to a timeout-bound default client.
func NewQuotaReporter(client *http.Client, daemonURL, sessionID, harness, token string, logger *slog.Logger) *QuotaReporter {
	if client == nil {
		client = &http.Client{Timeout: quotaReportTimeout}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &QuotaReporter{
		client:    client,
		daemonURL: strings.TrimRight(daemonURL, "/"),
		sessionID: sessionID,
		harness:   harness,
		token:     token,
		logger:    logger,
	}
}

// Enabled reports whether reports can reach a daemon.
func (r *QuotaReporter) Enabled() bool {
	return r != nil && r.daemonURL != "" && r.sessionID != "" && r.harness != ""
}

// registerQuotaReporter builds the session's reporter through the
// configured factory and holds it for the run. Only the quota
// harnesses ("codex", "claude") report; any other harness name
// registers nothing. The caller releases the registration when the
// run ends.
func (r *Runner) registerQuotaReporter(sessionID string, provider agent.Provider) {
	if r == nil || r.quotaReporterForSession == nil || provider == nil {
		return
	}
	var harness string
	switch provider.Name() {
	case agent.ProviderCodex:
		harness = agent.UsageHarnessCodex
	case agent.ProviderClaude:
		harness = agent.UsageHarnessClaude
	default:
		return
	}
	reporter := r.quotaReporterForSession(sessionID, harness)
	if reporter == nil || !reporter.Enabled() {
		return
	}
	r.quotaReportersMu.Lock()
	r.quotaReporters[sessionID] = reporter
	r.quotaReportersMu.Unlock()
}

// releaseQuotaReporter drops the session's reporter. Called when the
// run ends so reporters never outlive their session.
func (r *Runner) releaseQuotaReporter(sessionID string) {
	if r == nil {
		return
	}
	r.quotaReportersMu.Lock()
	delete(r.quotaReporters, sessionID)
	r.quotaReportersMu.Unlock()
}

// reportQuotaEvent forwards one observed event's sparse quota update
// when the session registered a reporter. Nil-safe: sessions without
// a reporter (unconfigured factory, non-quota harness) skip.
func (r *Runner) reportQuotaEvent(ctx context.Context, sessionID string, ev agent.Event) {
	if r == nil {
		return
	}
	r.quotaReportersMu.Lock()
	reporter := r.quotaReporters[sessionID]
	r.quotaReportersMu.Unlock()
	if reporter == nil {
		return
	}
	reporter.ReportUsageEvent(ctx, ev)
}

// ReportUsageEvent forwards one harness event's sparse quota update
// when it carries one. Consecutive identical updates are reported
// once: several live sessions on one host observe the same account
// notification, and the daemon merge is idempotent, so only the first
// copy travels. Delivery is best-effort and synchronous under the
// report timeout; a failure logs and drops, never failing the
// session.
func (r *QuotaReporter) ReportUsageEvent(ctx context.Context, ev agent.Event) {
	if !r.Enabled() {
		return
	}
	usage, ok := ev.(agent.UsageEvent)
	if !ok || usage.Usage == nil || len(usage.Usage.Windows) == 0 {
		return
	}
	body, err := json.Marshal(map[string]any{
		"harness": r.harness,
		"update":  usage.Usage,
	})
	if err != nil {
		return
	}
	r.mu.Lock()
	if bytes.Equal(body, r.last) {
		r.mu.Unlock()
		return
	}
	r.last = body
	r.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(ctx, quotaReportTimeout)
	defer cancel()
	endpoint := r.daemonURL + "/api/daemon/sessions/" + r.sessionID + "/usage"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		r.logger.Debug("quota report build failed", "sessionId", r.sessionID, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.logger.Debug("quota report dropped", "sessionId", r.sessionID, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		r.logger.Debug("quota report refused", "sessionId", r.sessionID, "status", resp.StatusCode)
	}
}
