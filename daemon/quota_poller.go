package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	providermuse "github.com/RenseiAI/donmai/provider/harness/muse"
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
		var extras []string
		if cfg := d.Config(); cfg != nil {
			extras = cfg.QuotaProbeHomes[agent.UsageHarnessCodex]
		}
		probes[agent.UsageHarnessCodex] = codexQuotaProbe(bin, true, extras)
	}
	if bin := d.opts.QuotaProbeBinaries[agent.UsageHarnessClaude]; bin != "" {
		var extras []string
		if cfg := d.Config(); cfg != nil {
			extras = cfg.QuotaProbeHomes[agent.UsageHarnessClaude]
		}
		probes[agent.UsageHarnessClaude] = claudeQuotaProbe(bin, extras)
	}
	if bin := d.opts.QuotaProbeBinaries[agent.UsageHarnessMuse]; bin != "" {
		probes[agent.UsageHarnessMuse] = museQuotaProbe(bin)
	}
	return probes
}

// defaultQuotaProbes builds the on-demand observation probes: the default
// home of each configured harness only. Extras are never published, so the
// observation scan must not wait for them; a slow extra would otherwise
// downgrade an already-verified default to unknown on timeout.
func (d *Daemon) defaultQuotaProbes() map[string]quotaProbeFunc {
	probes := map[string]quotaProbeFunc{}
	if d == nil {
		return probes
	}
	if bin := d.opts.QuotaProbeBinaries[agent.UsageHarnessCodex]; bin != "" {
		probes[agent.UsageHarnessCodex] = codexQuotaProbe(bin, true, nil)
	}
	if bin := d.opts.QuotaProbeBinaries[agent.UsageHarnessClaude]; bin != "" {
		probes[agent.UsageHarnessClaude] = claudeQuotaProbe(bin, nil)
	}
	if bin := d.opts.QuotaProbeBinaries[agent.UsageHarnessMuse]; bin != "" {
		probes[agent.UsageHarnessMuse] = museQuotaProbe(bin)
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
	case agent.UsageHarnessClaude:
		last = q.claudeAt
	case agent.UsageHarnessMuse:
		last = q.museAt
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
func codexQuotaProbe(codexBin string, hostAuth bool, extraHomes []string) quotaProbeFunc {
	homes := append([]string(nil), extraHomes...)
	return func(ctx context.Context, q *quotaState) {
		if ctx == nil {
			ctx = context.Background()
		}
		if codexBin == "" {
			at := time.Now()
			plan := buildCodexScanPlan(homes)
			defKey := ""
			if len(plan) > 0 {
				defKey = plan[0].key
			}
			q.recordCodexSlotAttempt(defKey, "", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request."), at, true)
			return
		}
		for _, slot := range buildCodexScanPlan(homes) {
			if ctx.Err() != nil {
				return
			}
			slotCtx, cancel := context.WithTimeout(ctx, quotaProbeTimeout)
			at := time.Now()
			accountID, plan, probed := probeCodexSlot(slotCtx, codexBin, hostAuth, slot.dir)
			cancel()
			q.recordCodexSlotAttempt(slot.key, accountID, plan, probed, at, slot.isDefault)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// probeCodexSlot runs one codex quota read against an explicit config home.
// An empty homeDir reads the default login; a non-empty value must name an
// absolute config directory whose auth.json is projected into the probe's
// isolated home. Unreadable homes yield unanswered failures, never verdicts.
func probeCodexSlot(ctx context.Context, codexBin string, hostAuth bool, homeDir string) (string, string, agent.UsageLimits) {
	at := time.Now()
	failed := func() (string, string, agent.UsageLimits) {
		return "", "", agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	if homeDir != "" {
		if !filepath.IsAbs(homeDir) {
			return failed()
		}
		if info, err := os.Stat(homeDir); err != nil || !info.IsDir() {
			return failed()
		}
	}
	provider, err := providercodex.New(providercodex.Options{
		CodexBin:         codexBin,
		HostSessionAuth:  hostAuth,
		HostHome:         homeDir,
		HandshakeTimeout: quotaProbeTimeout / 2,
		RPCTimeout:       providercodex.RateLimitsProbeTimeout,
	})
	if err != nil {
		return failed()
	}
	accountID, plan, probed := provider.ProbeQuota(ctx)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = provider.Shutdown(shutdownCtx)
	}()
	return accountID, plan, probed
}

type codexScanSlot struct {
	key       string
	dir       string
	isDefault bool
}

// probeParentDir returns the parent of an absolute path without cleaning
// it first: filepath.Dir cleans, which collapses a ".." through a symlink
// and resolves to an undeclared sibling. Trimming only trailing slashes
// preserves the symlink component for EvalSymlinks to resolve as the OS
// would open it.
func probeParentDir(p string) string {
	q := strings.TrimRight(p, "/")
	if q == "" {
		return "/"
	}
	if idx := strings.LastIndex(q, "/"); idx >= 0 {
		if idx == 0 {
			return "/"
		}
		return q[:idx]
	}
	return "."
}

// probeBase returns the final element without cleaning the path first,
// so a name ending in a space keeps it.
func probeBase(p string) string {
	q := strings.TrimRight(p, "/")
	if idx := strings.LastIndex(q, "/"); idx >= 0 {
		return q[idx+1:]
	}
	return q
}

func sameProbeFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	return os.SameFile(a, b)
}

func normalizeCodexProbeHome(dir string) string {
	// Resolve the declared path as the OS opens it: symlinks first, clean
	// only the resolved result. Cleaning before resolving would collapse
	// a ".." through a symlink and probe an undeclared home.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != "" {
		return filepath.Clean(resolved)
	}
	parent, base := probeParentDir(dir), probeBase(dir)
	if parent != "" && base != "" && base != "." && base != ".." {
		if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil && resolvedParent != "" {
			return filepath.Join(filepath.Clean(resolvedParent), base)
		}
	}
	return filepath.Clean(dir)
}

func buildCodexScanPlan(extraHomes []string) []codexScanSlot {
	defDir := providercodex.DefaultCodexHome()
	defKey := ""
	var defInfo os.FileInfo
	if defDir != "" {
		defKey = normalizeCodexProbeHome(defDir)
		defInfo, _ = os.Stat(defDir)
	}
	seen := map[string]struct{}{defKey: {}}
	var seenInfos []os.FileInfo
	if defInfo != nil {
		seenInfos = append(seenInfos, defInfo)
	}
	var extras []string
	for _, raw := range extraHomes {
		if strings.TrimSpace(raw) == "" || !filepath.IsAbs(raw) {
			continue
		}
		key := normalizeCodexProbeHome(raw)
		if _, dup := seen[key]; dup {
			continue
		}
		if info, err := os.Stat(raw); err == nil {
			dup := false
			for _, s := range seenInfos {
				if sameProbeFile(s, info) {
					dup = true
					break
				}
			}
			// A different spelling that opens the same directory
			// (case alias, symlink) probes once under the first key.
			if dup {
				continue
			}
			seen[key] = struct{}{}
			seenInfos = append(seenInfos, info)
			extras = append(extras, key)
			continue
		}
		// Unstatable paths dedup by string only; the probe records an
		// unanswered attempt for the missing home.
		seen[key] = struct{}{}
		extras = append(extras, key)
	}
	sort.Strings(extras)
	plan := make([]codexScanSlot, 0, 1+len(extras))
	plan = append(plan, codexScanSlot{key: defKey, dir: "", isDefault: true})
	for _, key := range extras {
		plan = append(plan, codexScanSlot{key: key, dir: key})
	}
	return plan
}

type claudeScanSlot struct {
	key       string
	dir       string
	isDefault bool
}

func normalizeClaudeProbeHome(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != "" {
		return filepath.Clean(resolved)
	}
	parent, base := probeParentDir(dir), probeBase(dir)
	if parent != "" && base != "" && base != "." && base != ".." {
		if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil && resolvedParent != "" {
			return filepath.Join(filepath.Clean(resolvedParent), base)
		}
	}
	return filepath.Clean(dir)
}

func buildClaudeScanPlan(extraHomes []string) []claudeScanSlot {
	defDir := providerclaude.DefaultProbeConfigDir()
	defKey := ""
	var defInfo os.FileInfo
	if defDir != "" {
		defKey = normalizeClaudeProbeHome(defDir)
		defInfo, _ = os.Stat(defDir)
	}
	seen := map[string]struct{}{defKey: {}}
	var seenInfos []os.FileInfo
	if defInfo != nil {
		seenInfos = append(seenInfos, defInfo)
	}
	var extras []string
	for _, raw := range extraHomes {
		if strings.TrimSpace(raw) == "" || !filepath.IsAbs(raw) {
			continue
		}
		key := normalizeClaudeProbeHome(raw)
		if _, dup := seen[key]; dup {
			continue
		}
		if info, err := os.Stat(raw); err == nil {
			dup := false
			for _, s := range seenInfos {
				if sameProbeFile(s, info) {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			seen[key] = struct{}{}
			seenInfos = append(seenInfos, info)
			extras = append(extras, key)
			continue
		}
		seen[key] = struct{}{}
		extras = append(extras, key)
	}
	sort.Strings(extras)
	plan := make([]claudeScanSlot, 0, 1+len(extras))
	plan = append(plan, claudeScanSlot{key: defKey, dir: "", isDefault: true})
	for _, key := range extras {
		plan = append(plan, claudeScanSlot{key: key, dir: key})
	}
	return plan
}

func claudeQuotaProbe(binary string, extraHomes []string) quotaProbeFunc {
	homes := append([]string(nil), extraHomes...)
	return func(ctx context.Context, q *quotaState) {
		if ctx == nil {
			ctx = context.Background()
		}
		if binary == "" {
			at := time.Now()
			plan := buildClaudeScanPlan(homes)
			defKey := ""
			if len(plan) > 0 {
				defKey = plan[0].key
			}
			q.recordClaudeSlotAttempt(defKey, "", agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Claude did not answer the usage request."), at, true)
			return
		}
		for _, slot := range buildClaudeScanPlan(homes) {
			if ctx.Err() != nil {
				return
			}
			slotCtx, cancel := context.WithTimeout(ctx, quotaProbeTimeout)
			at := time.Now()
			accountID, probed, _ := providerclaude.ProbeUsageWithConfigDir(slotCtx, binary, slot.dir)
			cancel()
			q.recordClaudeSlotAttempt(slot.key, accountID, probed, at, slot.isDefault)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// museHostSessionAccountID is the opaque account identity the muse
// login check publishes. The check proves a supported CLI is
// installed, never who signed in, so the id names the host session
// slot rather than an account.
const museHostSessionAccountID = "muse-host-session"

// museQuotaProbe checks the host muse CLI. Cancellation is unanswered.
func museQuotaProbe(binary string) quotaProbeFunc {
	return func(ctx context.Context, q *quotaState) {
		if ctx == nil {
			ctx = context.Background()
		}
		at := time.Now()
		if binary == "" {
			q.noteMuseProbe("", agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Muse did not answer the usage request."), at)
			return
		}
		probed := agent.MakeUsageLimits(agent.ISOTime(at), nil)
		if err := providermuse.CheckHostSessionLogin(ctx, binary); err != nil {
			out := agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Muse did not answer the usage request.")
			// Cancellation never answers: a stopping poller must not
			// publish its own shutdown as a login refusal.
			out.Unavailable.Answered = ctx.Err() == nil
			probed = out
		}
		q.noteMuseProbe(museHostSessionAccountID, probed, at)
	}
}

// quotaPoller runs the daemon's live quota reads: one codex
// `account/rateLimits/read`, one claude usage read and one muse login
// check per probe interval, each fed into the quota cache behind the
// heartbeat quota field. Lifecycle mirrors the other daemon services:
// Start is idempotent, Stop cancels the goroutine.
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

// probeDue runs one attempt per due harness. Claude and codex get a fresh
// per-slot timeout from the scan context; others share one per-attempt timeout.
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
		case agent.UsageHarnessMuse:
			due = p.quota.museProbeDue(now)
		default:
			continue
		}
		if !due {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if harness == agent.UsageHarnessClaude || harness == agent.UsageHarnessCodex {
			probe(ctx, p.quota)
		} else {
			attemptCtx, cancel := context.WithTimeout(ctx, quotaProbeTimeout)
			probe(attemptCtx, p.quota)
			cancel()
		}
		if ctx.Err() != nil {
			return
		}
		slog.Debug("daemon: quota probe attempted", "harness", harness)
	}
}
