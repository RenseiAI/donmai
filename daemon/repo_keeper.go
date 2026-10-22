package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runtime/repokeeper"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// Repository-keeper daemon wiring.
//
// The keeper serves one bare-mirror directory per (canonical remote,
// credential scope) from the shared store in runtime/repokeeper, rooted at
// <state-dir>/repo-keeper/. The store owns every on-disk decision —
// identity, verification, catalog, removal — and this file owns the daemon
// seam around it: the scope callback, the process-level settings,
// budget-gated creation with least-recently-used eviction, recovery
// ordering, secret-free stats, and the fetch/seed/evict event record.
//
// Degradation contract: every caller-facing method fails usable, never loud.
// When the keeper is disabled, still reconciling, over budget, or faced
// with a revoked or unverifiable mirror, Acquire reports ok=false and the
// caller falls back to a plain clone.

// Repo-keeper event types. The event record carries duration and bytes; it
// never carries a scope value or a remote URL.
const (
	// RepoKeeperEventFetch records a completed mirror fetch.
	RepoKeeperEventFetch = "repo-keeper.fetch"
	// RepoKeeperEventSeed records a completed seed from a mirror.
	RepoKeeperEventSeed = "repo-keeper.seed"
	// RepoKeeperEventEvict records a budget eviction of a mirror.
	RepoKeeperEventEvict = "repo-keeper.evict"
)

// repoKeeperMaxEvents bounds the in-memory event ring.
const repoKeeperMaxEvents = 64

// RepoKeeperScopeFunc resolves the credential scope for one remote. It sits
// beside Options.GitAuth: GitAuth mints the per-invocation credential, this
// seam names the isolation boundary the resulting mirror belongs to. The OSS
// default (DefaultRepoKeeperScope) is one host-wide scope.
type RepoKeeperScopeFunc func(ctx context.Context, remote string) (scope string, err error)

// DefaultRepoKeeperScope is the OSS scope resolver: every remote shares one
// host scope, so one remote maps to exactly one mirror on this machine.
func DefaultRepoKeeperScope(context.Context, string) (string, error) {
	return "host", nil
}

// RepoKeeperGitAuth mirrors the daemon's GitAuth seam so the keeper can carry
// the resolver without importing it through Options at every call site.
type RepoKeeperGitAuth func(ctx context.Context, repoURL string) (authHeader string, suppressHelper bool, err error)

// RepoKeeperSettings is the keeper's process-level configuration, derived
// from Config.RepoKeeper at startup. A change needs a drain-aware restart:
// the running keeper never re-reads these.
type RepoKeeperSettings struct {
	// Enabled gates construction. Disabled means no keeper exists and nothing
	// is created under repo-keeper/.
	Enabled bool
	// MaxBytes caps total mirror bytes. Zero means no limit.
	MaxBytes int64
	// FetchInterval bounds how often a mirror is re-fetched.
	FetchInterval time.Duration
	// AuthorizationWindow bounds how long a resolved authorization is honored.
	AuthorizationWindow time.Duration
}

// RepoKeeperEvent is one fetch/seed/evict observation. It carries duration
// and bytes only — never a scope value or a remote URL.
type RepoKeeperEvent struct {
	Type     string        `json:"type"`
	At       time.Time     `json:"at"`
	Duration time.Duration `json:"duration"`
	Bytes    int64         `json:"bytes"`
}

// RepoKeeper is the daemon's per-host mirror keeper. The zero value is inert;
// construct with NewRepoKeeper and Reconcile before serving Acquire calls.
//
// The store owns mirrors and catalog; the keeper adds the daemon policy:
// readiness (callers degrade while reconciling), the disk budget with
// in-flight seed pins and least-recently-used eviction, the event ring, and
// the secret-free stats snapshot.
type RepoKeeper struct {
	store    *repokeeper.Store
	settings RepoKeeperSettings
	scope    RepoKeeperScopeFunc
	auth     RepoKeeperGitAuth

	mu       sync.Mutex
	ready    bool
	inflight map[string]int
	lastUse  map[string]time.Time
	events   []RepoKeeperEvent
}

// NewRepoKeeper constructs a keeper over store. A nil scope resolves to
// DefaultRepoKeeperScope. A nil store (the store refused to start —
// including a cross-filesystem state/worktree layout) builds an inert
// keeper: Reconcile leaves it not-ready and Acquire always degrades.
func NewRepoKeeper(store *repokeeper.Store, settings RepoKeeperSettings, scope RepoKeeperScopeFunc, auth RepoKeeperGitAuth) *RepoKeeper {
	if scope == nil {
		scope = DefaultRepoKeeperScope
	}
	return &RepoKeeper{
		store:    store,
		settings: settings,
		scope:    scope,
		auth:     auth,
		inflight: make(map[string]int),
		lastUse:  make(map[string]time.Time),
	}
}

// Root returns the keeper's state-directory root.
func (k *RepoKeeper) Root() string {
	if k == nil || k.store == nil {
		return ""
	}
	return k.store.StateDir()
}

// Close releases the keeper's store root handle. Outstanding leases stay
// valid until their own Release; Acquire after Close degrades to a plain
// clone. The daemon calls it when a reconcile replaces the keeper and at
// shutdown, so each rebuilt store does not leak one root fd.
func (k *RepoKeeper) Close() error {
	if k == nil || k.store == nil {
		return nil
	}
	return k.store.Close()
}

// Ready reports whether the catalog has reconciled and the keeper is serving
// mirrors. While false, Acquire degrades to a plain clone.
func (k *RepoKeeper) Ready() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.ready
}

// Reconcile rebuilds the serving set after session and catalog
// reconciliation and before workarea-cache admission: catalog rows whose
// mirror directory is gone are dropped, and only mirrors the store's own
// verification passes are adopted — never an unverified directory. While
// it runs, Acquire degrades. A disabled keeper, or one whose store refused
// to start, reconciles to not-ready without touching the filesystem.
func (k *RepoKeeper) Reconcile(ctx context.Context) error {
	if k == nil || !k.settings.Enabled || k.store == nil {
		return nil
	}
	k.mu.Lock()
	k.ready = false
	k.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("repo keeper reconcile: %w", err)
	}
	// The store verifies candidates itself; the daemon passes none, so
	// Reconcile only drops rows whose directory is gone and re-reads the
	// surviving catalog. Adoption of verified directories happens through
	// Acquire's Ensure path, which verifies before serving.
	verified, _, err := k.store.Reconcile(nil)
	if err != nil {
		// Revocation state is unknown when the catalog is unreadable, so
		// the keeper stays not-ready and callers degrade until a fetch
		// re-establishes state — never serve a possibly-revoked mirror.
		slog.Warn("repo keeper: catalog unreadable; degrading to plain clone", "err", err)
		return err
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	// Drop in-memory use markers for mirrors the reconcile dropped, so a
	// later mirror at a recycled digest cannot inherit a stale recency.
	keep := make(map[string]time.Time, len(verified))
	for _, entry := range verified {
		if at, ok := k.lastUse[entry.Digest]; ok {
			keep[entry.Digest] = at
		} else if !entry.LastUse.IsZero() {
			keep[entry.Digest] = entry.LastUse
		}
	}
	k.lastUse = keep
	k.ready = true
	return nil
}

// Acquire resolves remote to a lease on its mirror. ok=false means the
// caller must degrade to a plain clone: the keeper is disabled, still
// reconciling, the scope did not resolve, the mirror is revoked or fails
// verification, or the budget is exhausted. An existing verified mirror is
// always served; the budget gates creation of new mirrors only.
//
// The lease holds the mirror's shared maintenance lock until Release, so
// the verify-then-use window stays closed while the caller seeds from the
// directory. The caller must Release; a nil lease degrades like ok=false.
//
// The daemon lock is held only for the ready/budget/revoked checks: the
// store call below may clone over the network and must never run under it.
func (k *RepoKeeper) Acquire(ctx context.Context, remote string) (lease *repokeeper.Lease, ok bool) {
	if k == nil || !k.settings.Enabled || k.store == nil {
		return nil, false
	}
	scope, err := k.scope(ctx, remote)
	if err != nil {
		slog.Warn("repo keeper: scope resolution failed; degrading to plain clone", "err", err)
		return nil, false
	}
	if scope == "" {
		slog.Warn("repo keeper: empty scope; degrading to plain clone")
		return nil, false
	}
	digest, err := repokeeper.MirrorDigest(remote, scope)
	if err != nil {
		return nil, false
	}

	k.mu.Lock()
	if !k.ready {
		k.mu.Unlock()
		return nil, false
	}
	// One catalog snapshot drives every gate below: a revoked row, an
	// unreadable catalog, or an over-budget new mirror all degrade. The
	// snapshot is taken under k.mu but the store call below runs without
	// it: the store serializes per-mirror work itself.
	entries, err := k.store.List()
	if err != nil {
		// Revocation and usage are unknown: degrade rather than serve a
		// possibly-revoked mirror or admit past an unknown budget.
		k.mu.Unlock()
		return nil, false
	}
	var self *repokeeper.MirrorEntry
	var total int64
	for i := range entries {
		// Revoked rows never count toward the budget: tombstones carry
		// zero bytes, and a revoked live mirror's bytes stay pinned
		// until the flag is explicitly cleared, so counting them would
		// only evict serving mirrors for no reclaimable gain.
		if entries[i].Revoked {
			if entries[i].Digest == digest {
				self = &entries[i]
			}
			continue
		}
		total += entries[i].Bytes
		if entries[i].Digest == digest {
			self = &entries[i]
		}
	}
	if self != nil && self.Revoked {
		k.mu.Unlock()
		return nil, false
	}
	if self == nil && k.settings.MaxBytes > 0 && total >= k.settings.MaxBytes {
		// At the budget the keeper stops creating mirrors: evict the
		// least recently used unpinned mirrors, then degrade to a plain
		// clone even after evicting. Existing mirrors keep serving.
		k.evictLocked(entries)
		k.mu.Unlock()
		return nil, false
	}
	k.mu.Unlock()

	// A revoked row or an unreadable catalog degrades above; the store
	// re-verifies identity, scope, and live origin before serving, and
	// refuses symlinks, foreign content, and empty non-repositories. The
	// returned lease holds the shared maintenance lock until the caller
	// releases it.
	var storeAuth repokeeper.GitAuth
	if k.auth != nil {
		storeAuth = repokeeper.GitAuth(k.auth)
	}
	lease, err = k.store.EnsureWithAuth(ctx, remote, scope, storeAuth)
	if err != nil {
		return nil, false
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now().UTC()
	k.touchLocked(digest, now)
	if err := k.store.TouchUse(digest, now); err != nil {
		slog.Warn("repo keeper: record mirror use failed", "err", err)
	}
	return lease, true
}

// RecordFetch notes a completed mirror fetch: last-fetch advances, bytes and
// revoked state are replaced, and a repo-keeper.fetch event is recorded.
func (k *RepoKeeper) RecordFetch(digest string, numBytes int64, duration time.Duration, revoked bool) {
	if k == nil || !k.settings.Enabled || k.store == nil {
		return
	}
	if err := k.store.RecordFetch(digest, time.Now().UTC(), numBytes, revoked); err != nil {
		slog.Warn("repo keeper: record fetch failed", "err", err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.touchLocked(digest, time.Now().UTC())
	k.recordEventLocked(RepoKeeperEvent{Type: RepoKeeperEventFetch, At: time.Now().UTC(), Duration: duration, Bytes: numBytes})
	slog.Info("repo-keeper.fetch", "duration", duration.String(), "bytes", numBytes, "revoked", revoked)
}

// SeedBegin marks one in-flight seed against a mirror. Mirrors with in-flight
// seeds are never budget-evicted.
func (k *RepoKeeper) SeedBegin(digest string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.inflight[digest]++
}

// SeedEnd releases one in-flight seed and records a repo-keeper.seed event
// with its duration and bytes.
func (k *RepoKeeper) SeedEnd(digest string, numBytes int64, duration time.Duration) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.inflight[digest] > 0 {
		k.inflight[digest]--
	}
	if k.inflight[digest] <= 0 {
		delete(k.inflight, digest)
	}
	k.recordEventLocked(RepoKeeperEvent{Type: RepoKeeperEventSeed, At: time.Now().UTC(), Duration: duration, Bytes: numBytes})
	slog.Info("repo-keeper.seed", "duration", duration.String(), "bytes", numBytes)
}

// EnforceBudget evicts mirrors with no in-flight seed, least recently used
// first, until the keeper is within budget. A zero MaxBytes means no limit
// and evicts nothing.
func (k *RepoKeeper) EnforceBudget() {
	if k == nil || k.store == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	entries, err := k.store.List()
	if err != nil {
		return
	}
	k.evictLocked(entries)
}

// Stats returns the secret-free keeper snapshot for daemon stats: mirror
// count, last fetch, revoked count, and bytes. No scope values or URLs.
// An unreadable catalog degrades to an empty snapshot, never an error.
func (k *RepoKeeper) Stats() *afclient.RepoKeeperStats {
	if k == nil {
		return nil
	}
	out := &afclient.RepoKeeperStats{}
	if k.store == nil {
		return out
	}
	entries, err := k.store.List()
	if err != nil {
		return out
	}
	out.MirrorCount = len(entries)
	var total int64
	for _, e := range entries {
		total += e.Bytes
		if e.Revoked {
			out.RevokedCount++
		}
		if !e.LastFetch.IsZero() {
			if at := e.LastFetch.UTC().Format(time.RFC3339); at > out.LastFetchAt {
				out.LastFetchAt = at
			}
		}
	}
	out.Bytes = total
	return out
}

// Events returns a copy of the in-memory fetch/seed/evict ring, oldest first.
func (k *RepoKeeper) Events() []RepoKeeperEvent {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]RepoKeeperEvent(nil), k.events...)
}

// evictLocked removes mirrors with no in-flight seed, least recently used
// first, until the keeper is within budget. A zero MaxBytes evicts nothing.
// Revoked rows are never victims: a tombstone holds no bytes to reclaim
// and dropping it would lose the sticky revocation gate, so eviction skips
// every revoked entry and its bytes stay out of the budget total.
// The caller holds k.mu and passes its catalog snapshot. Removal is
// non-blocking: a mirror whose lease is held (in this or another process
// sharing the state dir) is skipped as a victim for this pass, so
// eviction never waits on a lease while holding k.mu and never stalls
// serving mirrors. The recursive delete runs outside the per-mirror lock.
func (k *RepoKeeper) evictLocked(entries []repokeeper.MirrorEntry) {
	if k.settings.MaxBytes <= 0 || k.store == nil {
		return
	}
	// Victims skipped as lease-held this pass: without the set the
	// picker would return the same oldest victim forever.
	skipped := make(map[string]bool)
	for {
		var total int64
		for _, e := range entries {
			if e.Revoked {
				continue
			}
			total += e.Bytes
		}
		if total < k.settings.MaxBytes {
			return
		}
		victim := k.oldestEvictableLocked(entries, skipped)
		if victim == "" {
			return
		}
		var victimBytes int64
		for _, e := range entries {
			if e.Digest == victim {
				victimBytes = e.Bytes
			}
		}
		removed, err := k.store.TryRemove(victim)
		if err != nil {
			slog.Warn("repo keeper: evict mirror failed", "err", err)
			return
		}
		if !removed {
			// A lease is outstanding: treat the victim as in-flight
			// for this pass and try the next-oldest mirror.
			skipped[victim] = true
			continue
		}
		delete(k.lastUse, victim)
		kept := entries[:0]
		for _, e := range entries {
			if e.Digest != victim {
				kept = append(kept, e)
			}
		}
		entries = kept
		k.recordEventLocked(RepoKeeperEvent{Type: RepoKeeperEventEvict, At: time.Now().UTC(), Bytes: victimBytes})
		slog.Info("repo-keeper.evict", "bytes", victimBytes)
	}
}

// oldestEvictableLocked returns the least-recently-used digest with no
// in-flight seed, not revoked, and not in skipped (lease-held victims from
// this eviction pass), or "" when every mirror is pinned. Revoked rows are
// never victims: evicting a tombstone reaps zero bytes and deletes the
// sticky revocation gate. The caller holds k.mu.
func (k *RepoKeeper) oldestEvictableLocked(entries []repokeeper.MirrorEntry, skipped map[string]bool) string {
	var victim string
	var victimAt time.Time
	first := true
	for _, e := range entries {
		if e.Revoked || k.inflight[e.Digest] > 0 || skipped[e.Digest] {
			continue
		}
		at := e.LastUse
		if marked, ok := k.lastUse[e.Digest]; ok && marked.After(at) {
			at = marked
		}
		if first || at.Before(victimAt) {
			victim, victimAt, first = e.Digest, at, false
		}
	}
	return victim
}

// touchLocked advances one mirror's in-memory use marker. The caller holds
// k.mu.
func (k *RepoKeeper) touchLocked(digest string, at time.Time) {
	if k.lastUse == nil {
		k.lastUse = make(map[string]time.Time)
	}
	k.lastUse[digest] = at
}

// recordEventLocked appends to the bounded event ring. The caller holds k.mu.
func (k *RepoKeeper) recordEventLocked(ev RepoKeeperEvent) {
	k.events = append(k.events, ev)
	if len(k.events) > repoKeeperMaxEvents {
		k.events = append([]RepoKeeperEvent(nil), k.events[len(k.events)-repoKeeperMaxEvents:]...)
	}
}

// repoKeeperStateDir resolves the keeper's state-directory root:
// repo-keeper/ under the state home. It is a function so tests can point a
// Daemon at a temp dir by swapping the config-independent state home.
func repoKeeperStateDir() string {
	if dir := statehome.StateDir("repo-keeper"); dir != "" {
		return dir
	}
	return statepath.Resolve("repo-keeper", "/tmp/.donmai/repo-keeper")
}
