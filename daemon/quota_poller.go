package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
)

// quotaProbes builds the live probes for the harnesses the operator
// configured. Binaries come from Options.QuotaProbeBinaries — empty
// by default, so tests and embedders never shell out to a real
// harness login; only the production entry point resolves them from
// PATH. The codex probe projects the host's existing CLI login
// (host-session): without one its construction fails and the harness
// stays unprobed, so an API-key-only host records no codex verdict
// instead of a false one.
func (d *Daemon) quotaProbes() map[string]quotaProbeFunc {
	probes := map[string]quotaProbeFunc{}
	if d == nil {
		return probes
	}
	if bin := d.opts.QuotaProbeBinaries[agent.UsageHarnessCodex]; bin != "" {
		probes[agent.UsageHarnessCodex] = codexQuotaProbe(bin, true)
	}
	if bin := d.opts.QuotaProbeBinaries[agent.UsageHarnessClaude]; bin != "" {
		probes[agent.UsageHarnessClaude] = claudeQuotaProbe(bin)
	}
	return probes
}

// quotaProbeTimeout bounds one quota probe attempt: the codex read and
// the claude usage read each run under it. It stays above the slowest
// per-read ceiling (the claude usage session) and below the 5-minute
// probe interval, so a stuck harness child can never hold the quota
// loop past its per-harness budget.
const quotaProbeTimeout = 20 * time.Second

// quotaConsumerFreshnessTTL is the freshness bound the quota consumer
// relies on: a login verdict older than this no longer verifies the
// host. The probe interval stays below it, so a fresh ok:true always
// lands before the previous verdict ages out.
const quotaConsumerFreshnessTTL = 10 * time.Minute

// quotaProbeDueAt returns when the named harness's next probe may
// fire: the last attempt plus the probe interval. Zero when the probe
// is already due.
func (q *quotaState) quotaProbeDueAt(harness string, now time.Time) time.Time {
	if q == nil {
		return now
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var last time.Time
	switch harness {
	case agent.UsageHarnessCodex:
		last = q.codexAt
	default:
		last = q.claudeAt
	}
	due := last.Add(providercodex.RateLimitsProbeInterval)
	if last.IsZero() || !now.Before(due) {
		return time.Time{}
	}
	return due
}

// quotaProbeFunc performs one live quota read for a harness and feeds
// it into the quota cache. Production constructors live beside the
// poller; tests substitute recorded fixtures. Every invocation must
// feed exactly one note*Probe call — even when the harness is
// unreachable — because the feed is what advances the probe clock;
// a func that returns without feeding leaves its harness due and
// the loop re-fires it on the minimum wait.
type quotaProbeFunc func(ctx context.Context, q *quotaState)

// codexQuotaProbe returns the probe that reads the host's codex login
// through a short-lived app-server: construct, read, shut down. The
// read runs under the shared probe mapper, so its rows carry the same
// window ids the streamed updates merge onto. codexBin is the resolved
// codex binary; hostAuth selects whether the host's existing CLI
// login is projected into the probe's isolated home. An empty binary
// (codex not installed) disables the probe: the attempt records
// nothing and the heartbeat keeps omitting the codex entry.
func codexQuotaProbe(codexBin string, hostAuth bool) quotaProbeFunc {
	return func(ctx context.Context, q *quotaState) {
		at := time.Now()
		feed := func(accountID, plan string, probed agent.UsageLimits) {
			q.noteCodexProbe(accountID, plan, probed, at)
		}
		if codexBin == "" {
			feed("", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request."))
			return
		}
		provider, err := providercodex.New(providercodex.Options{
			CodexBin:         codexBin,
			HostSessionAuth:  hostAuth,
			HandshakeTimeout: quotaProbeTimeout / 2,
			RPCTimeout:       providercodex.RateLimitsProbeTimeout,
		})
		if err != nil {
			// No host login, no isolated home: the attempt never
			// reached the provider, so it advances the clock but
			// records no verdict.
			feed("", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request."))
			return
		}
		accountID, plan, probed := provider.ProbeQuota(ctx)
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = provider.Shutdown(shutdownCtx)
		}()
		feed(accountID, plan, probed)
	}
}

// claudeQuotaProbe returns the probe that reads the host's claude
// login through a short-lived CLI usage session. binary is the
// resolved claude binary; empty disables the probe like the codex
// one above.
func claudeQuotaProbe(binary string) quotaProbeFunc {
	return func(ctx context.Context, q *quotaState) {
		at := time.Now()
		if binary == "" {
			q.noteClaudeProbe("", agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Claude did not answer the usage request."), at)
			return
		}
		accountID, probed, _ := providerclaude.ProbeUsage(ctx, binary)
		q.noteClaudeProbe(accountID, probed, at)
	}
}

// quotaPoller runs the daemon's live quota reads: one codex
// `account/rateLimits/read` and one claude usage read per probe
// interval, each fed into the quota cache behind the heartbeat quota
// field. Lifecycle mirrors the other daemon services: Start is
// idempotent, Stop cancels the goroutine.
type quotaPoller struct {
	quota  *quotaState
	probes map[string]quotaProbeFunc

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
}

// newQuotaPoller builds a poller over q. probes maps harness name to
// its live read; a missing entry means that harness is not probed
// (its cache entry, if any, only moves on streamed updates).
func newQuotaPoller(q *quotaState, probes map[string]quotaProbeFunc) *quotaPoller {
	return &quotaPoller{quota: q, probes: probes}
}

// Start launches the probe goroutine. Probes that are due fire
// within about a second of Start (the loop floors its first wait),
// so a freshly started daemon reports quota on its first beats, then
// sleeps until the next harness comes due. Subsequent calls are
// no-ops.
func (p *quotaPoller) Start() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.running || p.quota == nil || len(p.probes) == 0 {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.running = true
	p.mu.Unlock()

	go p.loop(ctx)
}

// Stop terminates the probe goroutine. Safe to call multiple times.
func (p *quotaPoller) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return
	}
	p.cancel()
	p.running = false
}

// loop probes what is due, then sleeps until the earliest next due
// time across the configured harnesses. A probe runs under the
// per-attempt timeout; a slow or stuck harness therefore delays only
// its own next attempt, never the other harness's. Waits floor at
// one second so a probe that somehow leaves its harness due cannot
// spin the loop.
func (p *quotaPoller) loop(ctx context.Context) {
	for {
		wait := p.untilNextDue(time.Now())
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		p.probeDue(ctx)
		if ctx.Err() != nil {
			return
		}
	}
}

// untilNextDue sleeps until the earliest due probe. A due-now harness
// yields a zero wait; an empty or all-far-future set waits one full
// probe interval and rechecks.
func (p *quotaPoller) untilNextDue(now time.Time) time.Duration {
	earliest := time.Time{}
	for harness := range p.probes {
		if due := p.quota.quotaProbeDueAt(harness, now); due.IsZero() {
			return 0
		} else if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	if earliest.IsZero() {
		return providercodex.RateLimitsProbeInterval
	}
	wait := time.Until(earliest)
	if wait < 0 {
		return 0
	}
	return wait
}

// probeDue runs one attempt for every harness whose probe is due. Each
// attempt carries the per-attempt timeout and feeds the cache even
// when the read fails: every attempt advances the probe clock (so
// retries stay on the interval cadence) and an answered refusal
// refreshes the login verdict.
func (p *quotaPoller) probeDue(ctx context.Context) {
	now := time.Now()
	for harness, probe := range p.probes {
		if probe == nil {
			continue
		}
		due := false
		switch harness {
		case agent.UsageHarnessCodex:
			due = p.quota.codexProbeDue(now)
		case agent.UsageHarnessClaude:
			due = p.quota.claudeProbeDue(now)
		default:
			continue
		}
		if !due {
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, quotaProbeTimeout)
		probe(attemptCtx, p.quota)
		cancel()
		if ctx.Err() != nil {
			return
		}
		slog.Debug("daemon: quota probe attempted", "harness", harness)
	}
}
