package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

// Repository-keeper daemon wiring.
//
// The keeper maintains one bare-mirror directory per (canonical remote,
// credential scope) under <state-dir>/repo-keeper/mirrors/<digest>, with a
// secret-free catalog at <state-dir>/repo-keeper/catalog.json. Actual network
// fetch, cross-process locking, and seed-from-mirror mechanics belong to other
// slices; this file owns the daemon seam: the scope callback, the process-level
// settings, budget-gated creation with LRU eviction, recovery ordering,
// secret-free stats, and the fetch/seed/evict event record.
//
// Degradation contract: every caller-facing method fails usable, never loud.
// When the keeper is disabled, still reconciling, or over budget, Acquire
// reports ok=false and the caller falls back to a plain clone.

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

// repoKeeperCatalogName is the secret-free catalog file under the keeper root.
const repoKeeperCatalogName = "catalog.json"

// repoKeeperMirrorsName is the mirror-directory parent under the keeper root.
const repoKeeperMirrorsName = "mirrors"

// repoKeeperMirrorMetaName is the per-mirror provenance file. It records the
// canonical (credential-stripped) remote and the scope digest, never a
// credential.
const repoKeeperMirrorMetaName = "mirror.json"

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

// repoKeeperEntry is one secret-free catalog record, keyed by mirror digest.
type repoKeeperEntry struct {
	Digest    string `json:"digest"`
	LastFetch string `json:"lastFetch,omitempty"`
	LastUse   string `json:"lastUse,omitempty"`
	Revoked   bool   `json:"revoked,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
}

// repoKeeperMirrorMeta is the per-mirror provenance record. Remote is the
// canonical credential-stripped remote; ScopeDigest is the hex digest of the
// scope, never the scope itself.
type repoKeeperMirrorMeta struct {
	Remote      string `json:"remote"`
	ScopeDigest string `json:"scopeDigest"`
}

// RepoKeeper is the daemon's per-host mirror keeper. The zero value is inert;
// construct with NewRepoKeeper and Reconcile before serving Acquire calls.
type RepoKeeper struct {
	root     string
	settings RepoKeeperSettings
	scope    RepoKeeperScopeFunc
	auth     RepoKeeperGitAuth

	mu       sync.Mutex
	ready    bool
	entries  map[string]*repoKeeperEntry
	inflight map[string]int
	events   []RepoKeeperEvent
	// reconcilePrePersist is a test-only rendezvous invoked between the
	// catalog publish and the catalog persist in Reconcile. It lets the
	// race test park Reconcile inside the publish→persist window while a
	// concurrent Acquire hammers the entry map. Nil in production.
	reconcilePrePersist func()
}

// NewRepoKeeper constructs a keeper rooted at root. A nil scope resolves to
// DefaultRepoKeeperScope. Nothing is created on disk until Reconcile or the
// first Acquire; a disabled keeper never touches the filesystem at all.
func NewRepoKeeper(root string, settings RepoKeeperSettings, scope RepoKeeperScopeFunc, auth RepoKeeperGitAuth) *RepoKeeper {
	if scope == nil {
		scope = DefaultRepoKeeperScope
	}
	return &RepoKeeper{
		root:     root,
		settings: settings,
		scope:    scope,
		auth:     auth,
		entries:  make(map[string]*repoKeeperEntry),
		inflight: make(map[string]int),
	}
}

// Root returns the keeper's state-directory root.
func (k *RepoKeeper) Root() string {
	return k.root
}

// Ready reports whether the catalog has reconciled and the keeper is serving
// mirrors. While false, Acquire degrades to a plain clone.
func (k *RepoKeeper) Ready() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.ready
}

// Reconcile rebuilds the catalog after session and catalog reconciliation and
// before workarea-cache admission: entries whose mirror directory is gone are
// dropped, directories with no entry are adopted, and only then is the keeper
// marked ready. While it runs, Acquire degrades. A disabled keeper reconciles
// to not-ready without touching the filesystem.
func (k *RepoKeeper) Reconcile(ctx context.Context) error {
	if k == nil || !k.settings.Enabled {
		return nil
	}
	k.mu.Lock()
	k.ready = false
	k.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("repo keeper reconcile: %w", err)
	}
	catalog := k.loadCatalog()
	dirs, err := k.scanMirrorDirs()
	if err != nil {
		return err
	}
	merged := make(map[string]*repoKeeperEntry, len(dirs))
	for _, digest := range dirs {
		if e, ok := catalog[digest]; ok {
			merged[digest] = e
			continue
		}
		meta, metaErr := k.readMirrorMeta(digest)
		if metaErr != nil {
			slog.Warn("repo keeper: adopting mirror without provenance; recording digest only", "digest", digest)
		}
		_ = meta
		merged[digest] = &repoKeeperEntry{Digest: digest}
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	k.entries = merged
	// Publish and persist under one hold of k.mu: persistCatalogLocked
	// marshals k.entries, and a concurrent Acquire writes the same map, so
	// releasing the lock between publish and persist races. A persist
	// failure leaves the keeper not-ready; callers degrade until the next
	// successful reconcile.
	if k.reconcilePrePersist != nil {
		k.reconcilePrePersist()
	}
	if err := k.persistCatalogLocked(); err != nil {
		k.ready = false
		return err
	}
	k.ready = true
	return nil
}

// Acquire resolves remote to its mirror directory. ok=false means the caller
// must degrade to a plain clone: the keeper is disabled, still reconciling,
// the scope did not resolve, or the budget is exhausted. An existing mirror
// is always served; the budget gates creation of new mirrors only.
func (k *RepoKeeper) Acquire(ctx context.Context, remote string) (dir string, ok bool) {
	if k == nil || !k.settings.Enabled {
		return "", false
	}
	scope, err := k.scope(ctx, remote)
	if err != nil {
		slog.Warn("repo keeper: scope resolution failed; degrading to plain clone", "err", err)
		return "", false
	}
	if scope == "" {
		slog.Warn("repo keeper: empty scope; degrading to plain clone")
		return "", false
	}
	digest := repoKeeperDigest(canonicalRepoRemote(remote), scope)
	dir = filepath.Join(k.root, repoKeeperMirrorsName, digest)

	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.ready {
		return "", false
	}
	if e, exists := k.entries[digest]; exists {
		e.LastUse = time.Now().UTC().Format(time.RFC3339)
		return dir, true
	}
	if k.overBudgetLocked() {
		k.evictLocked()
		// At the budget the keeper stops creating mirrors even after
		// evicting: eviction reclaims for existing demand, and callers
		// degrade to a plain clone.
		return "", false
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("repo keeper: create mirror failed; degrading to plain clone", "err", err)
		return "", false
	}
	if err := k.writeMirrorMetaLocked(digest, canonicalRepoRemote(remote), scope); err != nil {
		_ = os.RemoveAll(dir)
		slog.Warn("repo keeper: record mirror provenance failed; degrading to plain clone", "err", err)
		return "", false
	}
	now := time.Now().UTC().Format(time.RFC3339)
	k.entries[digest] = &repoKeeperEntry{Digest: digest, LastUse: now}
	if err := k.persistCatalogLocked(); err != nil {
		slog.Warn("repo keeper: persist catalog failed", "err", err)
	}
	return dir, true
}

// RecordFetch notes a completed mirror fetch: last-fetch advances, bytes and
// revoked state are replaced, and a repo-keeper.fetch event is recorded.
func (k *RepoKeeper) RecordFetch(digest string, bytes int64, duration time.Duration, revoked bool) {
	if k == nil || !k.settings.Enabled {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if e, ok := k.entries[digest]; ok {
		e.LastFetch = time.Now().UTC().Format(time.RFC3339)
		e.Bytes = bytes
		e.Revoked = revoked
		if err := k.persistCatalogLocked(); err != nil {
			slog.Warn("repo keeper: persist catalog failed", "err", err)
		}
	}
	k.recordEventLocked(RepoKeeperEvent{Type: RepoKeeperEventFetch, At: time.Now().UTC(), Duration: duration, Bytes: bytes})
	slog.Info("repo-keeper.fetch", "duration", duration.String(), "bytes", bytes, "revoked", revoked)
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
func (k *RepoKeeper) SeedEnd(digest string, bytes int64, duration time.Duration) {
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
	k.recordEventLocked(RepoKeeperEvent{Type: RepoKeeperEventSeed, At: time.Now().UTC(), Duration: duration, Bytes: bytes})
	slog.Info("repo-keeper.seed", "duration", duration.String(), "bytes", bytes)
}

// EnforceBudget evicts mirrors with no in-flight seed, least recently used
// first, until the keeper is within budget. A zero MaxBytes means no limit
// and evicts nothing.
func (k *RepoKeeper) EnforceBudget() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.evictLocked()
}

// Stats returns the secret-free keeper snapshot for daemon stats: mirror
// count, last fetch, revoked count, and bytes. No scope values or URLs.
func (k *RepoKeeper) Stats() *afclient.RepoKeeperStats {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := &afclient.RepoKeeperStats{MirrorCount: len(k.entries)}
	var total int64
	for _, e := range k.entries {
		total += e.Bytes
		if e.Revoked {
			out.RevokedCount++
		}
		if e.LastFetch > out.LastFetchAt {
			out.LastFetchAt = e.LastFetch
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

// overBudgetLocked reports whether recorded mirror bytes meet or exceed
// the cap. A zero MaxBytes means no limit and never refuses.
func (k *RepoKeeper) overBudgetLocked() bool {
	if k.settings.MaxBytes <= 0 {
		return false
	}
	var total int64
	for _, e := range k.entries {
		total += e.Bytes
	}
	return total >= k.settings.MaxBytes
}

// evictLocked removes mirrors with no in-flight seed, least recently used
// first, until the keeper is within budget. A zero MaxBytes evicts nothing.
func (k *RepoKeeper) evictLocked() {
	if k.settings.MaxBytes <= 0 {
		return
	}
	for {
		var total int64
		for _, e := range k.entries {
			total += e.Bytes
		}
		if total < k.settings.MaxBytes {
			return
		}
		victim := k.oldestEvictableLocked()
		if victim == "" {
			return
		}
		bytes := k.entries[victim].Bytes
		_ = os.RemoveAll(filepath.Join(k.root, repoKeeperMirrorsName, victim))
		delete(k.entries, victim)
		k.recordEventLocked(RepoKeeperEvent{Type: RepoKeeperEventEvict, At: time.Now().UTC(), Bytes: bytes})
		slog.Info("repo-keeper.evict", "bytes", bytes)
		if err := k.persistCatalogLocked(); err != nil {
			slog.Warn("repo keeper: persist catalog failed", "err", err)
		}
	}
}

// oldestEvictableLocked returns the least-recently-used digest with no
// in-flight seed, or "" when every mirror is pinned by a seed.
func (k *RepoKeeper) oldestEvictableLocked() string {
	digests := make([]string, 0, len(k.entries))
	for digest := range k.entries {
		if k.inflight[digest] > 0 {
			continue
		}
		digests = append(digests, digest)
	}
	if len(digests) == 0 {
		return ""
	}
	sort.Slice(digests, func(i, j int) bool {
		return k.entries[digests[i]].LastUse < k.entries[digests[j]].LastUse
	})
	return digests[0]
}

// recordEventLocked appends to the bounded event ring.
func (k *RepoKeeper) recordEventLocked(ev RepoKeeperEvent) {
	k.events = append(k.events, ev)
	if len(k.events) > repoKeeperMaxEvents {
		k.events = append([]RepoKeeperEvent(nil), k.events[len(k.events)-repoKeeperMaxEvents:]...)
	}
}

// catalogPath returns the catalog file path.
func (k *RepoKeeper) catalogPath() string {
	return filepath.Join(k.root, repoKeeperCatalogName)
}

// loadCatalog reads the secret-free catalog, returning an empty map when the
// file is absent or undecodable. A corrupt catalog reconciles to the on-disk
// directories rather than failing the daemon.
func (k *RepoKeeper) loadCatalog() map[string]*repoKeeperEntry {
	data, err := os.ReadFile(k.catalogPath()) //nolint:gosec // keeper-owned state file
	if err != nil {
		return make(map[string]*repoKeeperEntry)
	}
	var decoded map[string]*repoKeeperEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		slog.Warn("repo keeper: catalog undecodable; reconciling from disk", "err", err)
		return make(map[string]*repoKeeperEntry)
	}
	out := make(map[string]*repoKeeperEntry, len(decoded))
	for digest, e := range decoded {
		if e == nil {
			continue
		}
		e.Digest = digest
		out[digest] = e
	}
	return out
}

// persistCatalogLocked writes the secret-free catalog atomically.
func (k *RepoKeeper) persistCatalogLocked() error {
	data, err := json.Marshal(k.entries)
	if err != nil {
		return fmt.Errorf("repo keeper: encode catalog: %w", err)
	}
	if err := os.MkdirAll(k.root, 0o700); err != nil {
		return fmt.Errorf("repo keeper: create root: %w", err)
	}
	tmp := k.catalogPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("repo keeper: write catalog: %w", err)
	}
	if err := os.Rename(tmp, k.catalogPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("repo keeper: publish catalog: %w", err)
	}
	return nil
}

// scanMirrorDirs lists the digest directories under mirrors/.
func (k *RepoKeeper) scanMirrorDirs() ([]string, error) {
	dir := filepath.Join(k.root, repoKeeperMirrorsName)
	names, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("repo keeper: scan mirrors: %w", err)
	}
	var out []string
	for _, n := range names {
		if n.IsDir() {
			out = append(out, n.Name())
		}
	}
	return out, nil
}

// readMirrorMeta reads one mirror's provenance record.
func (k *RepoKeeper) readMirrorMeta(digest string) (repoKeeperMirrorMeta, error) {
	var meta repoKeeperMirrorMeta
	data, err := os.ReadFile(filepath.Join(k.root, repoKeeperMirrorsName, digest, repoKeeperMirrorMetaName)) //nolint:gosec // keeper-owned state file
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// writeMirrorMetaLocked records the canonical remote and scope digest for a
// newly created mirror. The caller holds k.mu.
func (k *RepoKeeper) writeMirrorMetaLocked(digest, remote, scope string) error {
	sum := sha256.Sum256([]byte(scope))
	meta := repoKeeperMirrorMeta{Remote: remote, ScopeDigest: hex.EncodeToString(sum[:])}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("repo keeper: encode mirror provenance: %w", err)
	}
	if err := os.WriteFile(filepath.Join(k.root, repoKeeperMirrorsName, digest, repoKeeperMirrorMetaName), data, 0o600); err != nil {
		return fmt.Errorf("repo keeper: write mirror provenance: %w", err)
	}
	return nil
}

// repoKeeperDigest binds a mirror directory to one (canonical remote, scope)
// pair: two scopes for one remote produce two directories, and two remotes
// never share one.
func repoKeeperDigest(canonicalRemote, scope string) string {
	sum := sha256.Sum256([]byte(canonicalRemote + "\x00" + scope))
	return hex.EncodeToString(sum[:])
}

// canonicalRepoRemote strips credential-bearing components (userinfo, query,
// fragment) from an http(s) remote before it is hashed or recorded, so the
// catalog and provenance stay secret-free. Non-URL remotes (SSH-style, local
// paths) carry no URL credentials and hash trimmed as-is.
func canonicalRepoRemote(remote string) string {
	trimmed := strings.TrimSpace(remote)
	if parsed, err := url.Parse(trimmed); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	}
	return trimmed
}
