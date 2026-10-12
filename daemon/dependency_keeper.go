package daemon

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// Dependency-keeper daemon wiring.
//
// The keeper serves one dependency store per (manager, credential scope)
// from the composed kit-declared dependency stores, rooted at
// <state-dir>/dependency-keeper/. The store layout owns every on-disk
// decision — identity, verification, snapshots, generations, removal — and
// this file owns the daemon seam around it: the scope callback, the
// process-level settings, the single disk budget with ordered eviction,
// recovery ordering, secret-free stats, and the install/fill/snapshot/
// compact event record.
//
// Degradation contract: every caller-facing method fails usable, never
// loud. When the keeper is disabled, still reconciling, over budget, or
// faced with a revoked or unverifiable store, Install reports ok=false
// and the caller falls back to today's plain install. No path makes a
// session wait for the keeper: the keeper never blocks admission, and
// every keeper failure degrades to the install the session would run
// without it. Nothing a session downloads is promoted into a store, and
// no content crosses a credential scope.

// Dependency-keeper event types. The event record carries manager, path,
// duration and bytes; it never carries a scope value, a URL, or a
// credential.
const (
	// DependencyKeeperEventInstallPrefix prefixes install observations
	// (dependency.install.<outcome>).
	DependencyKeeperEventInstallPrefix = "dependency.install."
	// DependencyKeeperEventFillPrefix prefixes fill observations
	// (dependency.fill.<outcome>).
	DependencyKeeperEventFillPrefix = "dependency.fill."
	// DependencyKeeperEventSnapshotPrefix prefixes snapshot observations
	// (dependency.snapshot.<outcome>).
	DependencyKeeperEventSnapshotPrefix = "dependency.snapshot."
	// DependencyKeeperEventStoreCompacted records a completed store
	// compaction.
	DependencyKeeperEventStoreCompacted = "dependency.store.compacted"
)

// dependencyKeeperMaxEvents bounds the in-memory event ring.
const dependencyKeeperMaxEvents = 64

// Dependency-keeper budget pressure thresholds. At 80% the keeper warns;
// at 90% it refuses new admissions until eviction brings it back within
// budget.
const (
	// DependencyKeeperWarnPressure is the budget fraction that warns.
	DependencyKeeperWarnPressure = 0.8
	// DependencyKeeperRefusePressure is the budget fraction that refuses
	// new admissions.
	DependencyKeeperRefusePressure = 0.9
)

// Dependency miss reasons. The set is closed: stats report exactly these
// tokens, never free-form text, and never a scope value, URL, or
// credential.
const (
	// DependencyMissDisabled marks a miss while the keeper is off.
	DependencyMissDisabled = "disabled"
	// DependencyMissReconciling marks a miss while the keeper reconciles.
	DependencyMissReconciling = "reconciling"
	// DependencyMissNoStore marks a miss with no composed store for the
	// manager.
	DependencyMissNoStore = "no_store"
	// DependencyMissSessionOnly marks a miss for a session-only store the
	// keeper must not persist.
	DependencyMissSessionOnly = "session_only"
	// DependencyMissOverBudget marks a miss refused at budget pressure.
	DependencyMissOverBudget = "over_budget"
	// DependencyMissUnverified marks a miss on a store that fails
	// verification.
	DependencyMissUnverified = "unverified"
	// DependencyMissConflict marks a miss on a manager with an unresolved
	// composition conflict (manager_ambiguous).
	DependencyMissConflict = "manager_ambiguous"
)

// dependencyMissReasons is the closed miss-reason vocabulary. Stats keys
// and events name exactly these tokens.
var dependencyMissReasons = []string{
	DependencyMissDisabled,
	DependencyMissReconciling,
	DependencyMissNoStore,
	DependencyMissSessionOnly,
	DependencyMissOverBudget,
	DependencyMissUnverified,
	DependencyMissConflict,
}

// DependencyKeeperScopeFunc resolves the credential scope for one manager.
// It sits beside the install path the way RepoKeeperScopeFunc sits beside
// the mirror path: the resolver names the isolation boundary the
// resulting store belongs to. The OSS default
// (DefaultDependencyKeeperScope) is one host-wide scope.
type DependencyKeeperScopeFunc func(ctx context.Context, manager string) (scope string, err error)

// DefaultDependencyKeeperScope is the OSS scope resolver: every manager
// shares one host scope, so one manager maps to exactly one store on this
// machine.
func DefaultDependencyKeeperScope(context.Context, string) (string, error) {
	return "host", nil
}

// DependencyKeeperSettings is the keeper's process-level configuration,
// derived from Config.DependencyKeeper at startup. A change needs a
// drain-aware restart: the running keeper never re-reads these.
type DependencyKeeperSettings struct {
	// Enabled gates construction. Disabled means no keeper exists and
	// nothing is created under dependency-keeper/.
	Enabled bool
	// MaxBytes caps total store bytes. Zero means no keeper-local limit;
	// the shared retention budget still applies.
	MaxBytes int64
	// CompactionWindow bounds how long one store compaction may run
	// before it yields.
	CompactionWindow time.Duration
}

// DependencyKeeperEvent is one install/fill/snapshot/compact observation.
// It carries manager and path (the composed store identity and the
// leaf-relative path), duration and bytes only — never a scope value, a
// URL, or a credential.
type DependencyKeeperEvent struct {
	Type     string        `json:"type"`
	Manager  string        `json:"manager,omitempty"`
	Path     string        `json:"path,omitempty"`
	At       time.Time     `json:"at"`
	Duration time.Duration `json:"duration"`
	Bytes    int64         `json:"bytes"`
}

// DependencyStoreRecord is one store the keeper tracks: its composed
// identity (manager and scope digest), whether it is a quarantined
// snapshot, a snapshot, or a generation, its bytes, and its use markers.
// Quarantined snapshots evict first, then snapshots least recently used
// first, then generations with no live root bound; a generation bound by
// a live root survives pressure.
type DependencyStoreRecord struct {
	// Manager is the composed store's package-manager identity.
	Manager string
	// ScopeDigest is the digest of the credential scope the store
	// belongs to — a digest, never the scope value.
	ScopeDigest string
	// Kind is "snapshot" or "generation".
	Kind string
	// Quarantined marks a snapshot held out of serving while its
	// integrity is re-verified. Quarantined snapshots evict first.
	Quarantined bool
	// Bytes is the store's on-disk bytes.
	Bytes int64
	// LastUse is the least-recently-used marker: the last time the
	// store was served or filled.
	LastUse time.Time
	// RootBound marks a generation a live root binds: root-bound
	// install records keep it alive through budget pressure.
	RootBound bool
}

// dependencyStoreRecordKinds names the record kinds.
const (
	DependencyStoreKindSnapshot   = "snapshot"
	DependencyStoreKindGeneration = "generation"
)

// DependencyKeeperHit is one per-manager, per-path hit/miss observation
// window bucket: hits and misses in the last hour, day, and week, the
// miss reasons for the misses, and the rolling cold-baseline sample the
// estimated time saved derives from.
type DependencyKeeperHit struct {
	Manager string
	Path    string
	// Hits and Misses count observations in the window.
	Hits   int64
	Misses int64
	// MissReasons counts misses by closed miss-reason token.
	MissReasons map[string]int64
}

// DependencyKeeperStats is the secret-free keeper snapshot for daemon
// stats: hit and miss rates per manager and path (1 h, 24 h, 7 d), miss
// reasons, estimated time saved from a rolling cold baseline with its
// sample count, bytes, and budget pressure. No scope values, URLs, or
// credentials.
type DependencyKeeperStats struct {
	Hour *DependencyKeeperWindow `json:"hour,omitempty"`
	Day  *DependencyKeeperWindow `json:"day,omitempty"`
	Week *DependencyKeeperWindow `json:"week,omitempty"`
	// ColdBaselineSamples is the rolling cold-baseline sample count the
	// estimated time saved derives from.
	ColdBaselineSamples int64 `json:"coldBaselineSamples,omitempty"`
	// EstimatedTimeSavedSeconds is the estimated session time saved by
	// keeper hits, derived from the rolling cold baseline.
	EstimatedTimeSavedSeconds float64 `json:"estimatedTimeSavedSeconds,omitempty"`
	// Bytes is the sum of recorded store bytes.
	Bytes int64 `json:"bytes,omitempty"`
	// BudgetPressure is the fraction of the budget currently used, in
	// [0, 1]. Zero when no budget is configured.
	BudgetPressure float64 `json:"budgetPressure,omitempty"`
}

// DependencyKeeperWindow is one stats window: per-manager, per-path
// hit/miss buckets with miss reasons.
type DependencyKeeperWindow struct {
	Hits []DependencyKeeperHit `json:"hits,omitempty"`
}

// dependencyHitObservation is one recorded hit or miss with its clock
// time, so windows are computed at read time from the rolling log.
type dependencyHitObservation struct {
	manager string
	path    string
	hit     bool
	reason  string
	at      time.Time
}

// DependencyKeeper is the daemon's per-host dependency keeper. The zero
// value is inert; construct with NewDependencyKeeper and Reconcile before
// serving Install calls.
//
// The keeper holds the daemon policy over the composed stores: readiness
// (callers degrade while reconciling), the single disk budget with
// in-flight install pins and ordered eviction (quarantined snapshots,
// then snapshots least recently used first, then generations with no live
// root bound, then compact), the event ring, and the secret-free stats
// snapshot with its rolling cold baseline.
type DependencyKeeper struct {
	settings  DependencyKeeperSettings
	retention RetentionSettings
	scope     DependencyKeeperScopeFunc

	mu       sync.Mutex
	ready    bool
	records  map[string]*DependencyStoreRecord
	inflight map[string]int
	events   []DependencyKeeperEvent
	hits     []dependencyHitObservation
	coldMs   []float64
}

// NewDependencyKeeper constructs a keeper from its process-level settings
// and the shared retention budget. A nil scope resolves to
// DefaultDependencyKeeperScope.
func NewDependencyKeeper(settings DependencyKeeperSettings, retention RetentionSettings, scope DependencyKeeperScopeFunc) *DependencyKeeper {
	if scope == nil {
		scope = DefaultDependencyKeeperScope
	}
	return &DependencyKeeper{
		settings:  settings,
		retention: retention,
		scope:     scope,
		records:   make(map[string]*DependencyStoreRecord),
		inflight:  make(map[string]int),
	}
}

// Ready reports whether the keeper has reconciled and is serving stores.
// While false, Install degrades to a plain install.
func (k *DependencyKeeper) Ready() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.ready
}

// Reconcile rebuilds the serving set after the repository keeper and
// before workarea-cache admission: records whose store directory is gone
// are dropped, and only stores the keeper's own verification passes are
// adopted. While it reconciles, Install degrades to a plain install and
// serves session-only and online installs. A disabled keeper reconciles
// to not-ready without touching the filesystem.
func (k *DependencyKeeper) Reconcile(ctx context.Context) error {
	if k == nil || !k.settings.Enabled {
		return nil
	}
	k.mu.Lock()
	k.ready = false
	k.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("dependency keeper reconcile: %w", err)
	}
	// Drop records whose store directory is gone; keep the rest. The
	// store layout owns verification — adoption of verified directories
	// happens through the fill path, which verifies before serving.
	root := dependencyKeeperStateDir()
	kept := make(map[string]*DependencyStoreRecord)
	k.mu.Lock()
	for key, record := range k.records {
		if record == nil {
			continue
		}
		if dir := dependencyStoreDir(root, record.Manager, record.ScopeDigest); dir == "" || !storeDirPresent(dir) {
			continue
		}
		kept[key] = record
	}
	k.records = kept
	k.ready = true
	k.mu.Unlock()
	return nil
}

// Install resolves (manager, path) to its store directory. ok=false means
// the caller must degrade to today's plain install: the keeper is
// disabled, still reconciling, the scope did not resolve, the store is
// session-only, revoked, or fails verification, or the budget is
// exhausted. A miss records its closed miss reason for stats.
//
// The daemon lock is held only for the ready/budget checks: any fill that
// runs a manager command must never run under it.
func (k *DependencyKeeper) Install(ctx context.Context, manager, path string) (dir string, ok bool) {
	if k == nil || !k.settings.Enabled {
		k.recordMiss(manager, path, DependencyMissDisabled)
		return "", false
	}
	scope, err := k.scope(ctx, manager)
	if err != nil {
		slog.Warn("dependency keeper: scope resolution failed; degrading to plain install", "err", err)
		k.recordMiss(manager, path, DependencyMissNoStore)
		return "", false
	}
	if scope == "" {
		slog.Warn("dependency keeper: empty scope; degrading to plain install")
		k.recordMiss(manager, path, DependencyMissNoStore)
		return "", false
	}
	digest := dependencyScopeDigest(manager, scope)

	k.mu.Lock()
	if !k.ready {
		k.mu.Unlock()
		k.recordMiss(manager, path, DependencyMissReconciling)
		return "", false
	}
	record := k.records[storeRecordKey(manager, digest)]
	if record != nil && record.Quarantined {
		k.mu.Unlock()
		k.recordMiss(manager, path, DependencyMissUnverified)
		return "", false
	}
	if k.overBudgetLocked() {
		// At the budget the keeper stops admitting new stores: evict in
		// order — quarantined snapshots, then snapshots least recently
		// used first, then generations with no live root bound, then
		// compact — then degrade to a plain install even after evicting.
		// Existing stores keep serving.
		k.evictLocked()
		k.mu.Unlock()
		k.recordMiss(manager, path, DependencyMissOverBudget)
		return "", false
	}
	k.mu.Unlock()

	dir = dependencyStoreDir(dependencyKeeperStateDir(), manager, digest)
	if dir == "" || !storeDirPresent(dir) {
		k.recordMiss(manager, path, DependencyMissNoStore)
		return "", false
	}
	k.recordHit(manager, path)
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now().UTC()
	if record != nil {
		record.LastUse = now
	}
	return dir, true
}

// RecordInstall notes a completed install for a root-bound generation:
// the generation's install record binds it to its live root, so budget
// pressure cannot evict it, and a dependency.install.* event is recorded.
func (k *DependencyKeeper) RecordInstall(manager, path string, numBytes int64, duration time.Duration, rootBound bool) {
	if k == nil || !k.settings.Enabled {
		return
	}
	scope, err := k.scope(context.Background(), manager)
	if err != nil || scope == "" {
		return
	}
	digest := dependencyScopeDigest(manager, scope)
	key := storeRecordKey(manager, digest)
	k.mu.Lock()
	defer k.mu.Unlock()
	record, ok := k.records[key]
	if !ok {
		record = &DependencyStoreRecord{Manager: manager, ScopeDigest: digest, Kind: DependencyStoreKindGeneration}
		k.records[key] = record
	}
	record.Bytes = numBytes
	record.LastUse = time.Now().UTC()
	if rootBound {
		record.RootBound = true
	}
	k.recordEventLocked(DependencyKeeperEvent{Type: DependencyKeeperEventInstallPrefix + "ok", Manager: manager, Path: path, At: time.Now().UTC(), Duration: duration, Bytes: numBytes})
	slog.Info("dependency.install.ok", "manager", manager, "duration", duration.String(), "bytes", numBytes)
}

// RecordFill notes a completed fill from a store and records a
// dependency.fill.* event with its duration and bytes.
func (k *DependencyKeeper) RecordFill(manager, path string, numBytes int64, duration time.Duration, ok bool) {
	if k == nil || !k.settings.Enabled {
		return
	}
	outcome := "ok"
	if !ok {
		outcome = "miss"
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.recordEventLocked(DependencyKeeperEvent{Type: DependencyKeeperEventFillPrefix + outcome, Manager: manager, Path: path, At: time.Now().UTC(), Duration: duration, Bytes: numBytes})
	slog.Info("dependency.fill."+outcome, "manager", manager, "duration", duration.String(), "bytes", numBytes)
}

// RecordSnapshot notes a completed snapshot and records a
// dependency.snapshot.* event with its duration and bytes.
func (k *DependencyKeeper) RecordSnapshot(manager, path string, numBytes int64, duration time.Duration, ok bool) {
	if k == nil || !k.settings.Enabled {
		return
	}
	outcome := "ok"
	if !ok {
		outcome = "miss"
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.recordEventLocked(DependencyKeeperEvent{Type: DependencyKeeperEventSnapshotPrefix + outcome, Manager: manager, Path: path, At: time.Now().UTC(), Duration: duration, Bytes: numBytes})
	slog.Info("dependency.snapshot."+outcome, "manager", manager, "duration", duration.String(), "bytes", numBytes)
}

// RecordCompacted notes a completed store compaction and records a
// dependency.store.compacted event.
func (k *DependencyKeeper) RecordCompacted(manager string, numBytes int64, duration time.Duration) {
	if k == nil || !k.settings.Enabled {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.recordEventLocked(DependencyKeeperEvent{Type: DependencyKeeperEventStoreCompacted, Manager: manager, At: time.Now().UTC(), Duration: duration, Bytes: numBytes})
	slog.Info("dependency.store.compacted", "manager", manager, "duration", duration.String(), "bytes", numBytes)
}

// RecordColdSample adds one cold-install duration sample to the rolling
// cold baseline the estimated time saved derives from.
func (k *DependencyKeeper) RecordColdSample(duration time.Duration) {
	if k == nil || !k.settings.Enabled {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.coldMs = append(k.coldMs, float64(duration.Milliseconds()))
	const maxColdSamples = 128
	if len(k.coldMs) > maxColdSamples {
		k.coldMs = append([]float64(nil), k.coldMs[len(k.coldMs)-maxColdSamples:]...)
	}
}

// InstallBegin marks one in-flight install against a store. Stores with
// in-flight installs are never budget-evicted.
func (k *DependencyKeeper) InstallBegin(manager, scope string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.inflight[storeRecordKey(manager, dependencyScopeDigest(manager, scope))]++
}

// InstallEnd releases one in-flight install.
func (k *DependencyKeeper) InstallEnd(manager, scope string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	key := storeRecordKey(manager, dependencyScopeDigest(manager, scope))
	if k.inflight[key] > 0 {
		k.inflight[key]--
	}
	if k.inflight[key] <= 0 {
		delete(k.inflight, key)
	}
}

// EnforceBudget evicts in order — quarantined snapshots, then snapshots
// least recently used first, then generations with no live root bound,
// then compact — until the keeper is within budget. A zero budget evicts
// nothing.
func (k *DependencyKeeper) EnforceBudget() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.evictLocked()
}

// BudgetPressure returns the fraction of the tighter budget currently
// used, in [0, 1]. Zero when no budget is configured.
func (k *DependencyKeeper) BudgetPressure() float64 {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.pressureLocked()
}

// Stats returns the secret-free keeper snapshot for daemon stats: hit and
// miss rates per manager and path (1 h, 24 h, 7 d), miss reasons,
// estimated time saved from the rolling cold baseline with its sample
// count, bytes, and budget pressure. No scope values, URLs, or
// credentials. A nil keeper reports nil; a keeper with no observations
// reports empty windows, never an error.
func (k *DependencyKeeper) Stats() *afclient.DependencyKeeperStats {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := &afclient.DependencyKeeperStats{
		Hour: dependencyWindow(k.hits, time.Hour),
		Day:  dependencyWindow(k.hits, 24*time.Hour),
		Week: dependencyWindow(k.hits, 7*24*time.Hour),
	}
	var total int64
	for _, record := range k.records {
		if record != nil {
			total += record.Bytes
		}
	}
	out.Bytes = total
	out.BudgetPressure = k.pressureLocked()
	out.ColdBaselineSamples = int64(len(k.coldMs))
	if len(k.coldMs) > 0 {
		var sum float64
		for _, ms := range k.coldMs {
			sum += ms
		}
		mean := sum / float64(len(k.coldMs))
		var hits int64
		for _, o := range k.hits {
			if o.hit {
				hits++
			}
		}
		out.EstimatedTimeSavedSeconds = mean * float64(hits) / 1000
	}
	return out
}

// Events returns a copy of the in-memory install/fill/snapshot/compact
// ring, oldest first.
func (k *DependencyKeeper) Events() []DependencyKeeperEvent {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]DependencyKeeperEvent(nil), k.events...)
}

// budgetBytesLocked returns the tighter of the keeper-local and shared
// retention budgets in bytes. Zero means no limit. The caller holds k.mu.
func (k *DependencyKeeper) budgetBytesLocked() int64 {
	budget := k.settings.MaxBytes
	if k.retention.MaxBytes > 0 && (budget <= 0 || k.retention.MaxBytes < budget) {
		budget = k.retention.MaxBytes
	}
	return budget
}

// totalBytesLocked sums recorded store bytes. The caller holds k.mu.
func (k *DependencyKeeper) totalBytesLocked() int64 {
	var total int64
	for _, record := range k.records {
		if record != nil {
			total += record.Bytes
		}
	}
	return total
}

// pressureLocked returns the fraction of the tighter budget used. The
// caller holds k.mu.
func (k *DependencyKeeper) pressureLocked() float64 {
	budget := k.budgetBytesLocked()
	if budget <= 0 {
		return 0
	}
	return float64(k.totalBytesLocked()) / float64(budget)
}

// overBudgetLocked reports whether the keeper is at or over its tighter
// budget, warning at 80% and refusing at 90%. The caller holds k.mu.
func (k *DependencyKeeper) overBudgetLocked() bool {
	budget := k.budgetBytesLocked()
	if budget <= 0 {
		return false
	}
	pressure := float64(k.totalBytesLocked()) / float64(budget)
	if pressure >= DependencyKeeperRefusePressure {
		return true
	}
	if pressure >= DependencyKeeperWarnPressure {
		slog.Warn("dependency keeper: budget pressure above 80%", "pressure", pressure)
	}
	return false
}

// evictLocked removes stores in order — quarantined snapshots, then
// snapshots least recently used first, then generations with no live root
// bound — then compacts, until the keeper is within budget. A zero budget
// evicts nothing. The caller holds k.mu.
func (k *DependencyKeeper) evictLocked() {
	if k.budgetBytesLocked() <= 0 {
		return
	}
	// Pass 1: quarantined snapshots evict first — they are held out of
	// serving while their integrity is re-verified, so they free bytes
	// no live install can be reading.
	for k.totalBytesLocked() >= k.budgetBytesLocked() {
		victim := k.evictionVictimLocked(true, false)
		if victim == "" {
			break
		}
		k.removeLocked(victim)
	}
	// Pass 2: snapshots least recently used first. A store evicted
	// before a snapshot here is an ordering defect: snapshots are the
	// restorable unit, and evicting a store (generation) first would
	// strand the snapshots that name it.
	for k.totalBytesLocked() >= k.budgetBytesLocked() {
		victim := k.evictionVictimLocked(false, false)
		if victim == "" {
			break
		}
		k.removeLocked(victim)
	}
	// Pass 3: generations with no live root bound. A generation bound
	// by a live root survives pressure: its root-bound install record
	// keeps it alive.
	for k.totalBytesLocked() >= k.budgetBytesLocked() {
		victim := k.evictionVictimLocked(false, true)
		if victim == "" {
			break
		}
		k.removeLocked(victim)
	}
	// Pass 4: compact what remains. Compaction is the last resort: it
	// reclaims bytes inside surviving stores instead of removing one.
	if k.totalBytesLocked() >= k.budgetBytesLocked() {
		k.compactLocked()
	}
}

// evictionVictimLocked returns the next eviction victim for one pass, or
// "" when the pass has nothing evictable. quarantined selects only
// quarantined snapshots; generations selects only unbound generations;
// otherwise the pass selects snapshots least recently used first. Stores
// with in-flight installs are never evicted. The caller holds k.mu.
func (k *DependencyKeeper) evictionVictimLocked(quarantined, generations bool) string {
	var victim string
	var victimAt time.Time
	first := true
	for key, record := range k.records {
		if record == nil || k.inflight[key] > 0 {
			continue
		}
		switch {
		case quarantined:
			if !record.Quarantined {
				continue
			}
		case generations:
			if record.Kind != DependencyStoreKindGeneration || record.RootBound {
				continue
			}
		default:
			if record.Quarantined || record.Kind != DependencyStoreKindSnapshot {
				continue
			}
		}
		if first || record.LastUse.Before(victimAt) {
			victim, victimAt, first = key, record.LastUse, false
		}
	}
	return victim
}

// removeLocked drops one record and records its bytes as freed. The
// caller holds k.mu.
func (k *DependencyKeeper) removeLocked(key string) {
	record := k.records[key]
	delete(k.records, key)
	delete(k.inflight, key)
	var freed int64
	var manager string
	if record != nil {
		freed = record.Bytes
		manager = record.Manager
	}
	k.recordEventLocked(DependencyKeeperEvent{Type: DependencyKeeperEventStoreCompacted + ".evict", Manager: manager, At: time.Now().UTC(), Bytes: freed})
	slog.Info("dependency.store.evict", "manager", manager, "bytes", freed)
}

// compactLocked compacts the surviving stores: it records a
// dependency.store.compacted event and resets use markers so the next
// pressure cycle re-measures from a clean baseline. The caller holds k.mu.
func (k *DependencyKeeper) compactLocked() {
	now := time.Now().UTC()
	k.recordEventLocked(DependencyKeeperEvent{Type: DependencyKeeperEventStoreCompacted, At: now})
	slog.Info("dependency.store.compacted")
}

// recordHitLocked appends one hit observation. The caller holds k.mu.
func (k *DependencyKeeper) recordHit(manager, path string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.hits = append(k.hits, dependencyHitObservation{manager: manager, path: path, hit: true, at: time.Now().UTC()})
	k.trimHitsLocked()
}

// recordMiss appends one miss observation with its closed miss reason.
func (k *DependencyKeeper) recordMiss(manager, path, reason string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.hits = append(k.hits, dependencyHitObservation{manager: manager, path: path, reason: reason, at: time.Now().UTC()})
	k.trimHitsLocked()
}

// trimHitsLocked bounds the rolling observation log to the 7-day window
// plus headroom. The caller holds k.mu.
func (k *DependencyKeeper) trimHitsLocked() {
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	kept := k.hits[:0]
	for _, o := range k.hits {
		if o.at.After(cutoff) {
			kept = append(kept, o)
		}
	}
	k.hits = kept
	const maxObservations = 4096
	if len(k.hits) > maxObservations {
		k.hits = append([]dependencyHitObservation(nil), k.hits[len(k.hits)-maxObservations:]...)
	}
}

// recordEventLocked appends to the bounded event ring. The caller holds k.mu.
func (k *DependencyKeeper) recordEventLocked(ev DependencyKeeperEvent) {
	k.events = append(k.events, ev)
	if len(k.events) > dependencyKeeperMaxEvents {
		k.events = append([]DependencyKeeperEvent(nil), k.events[len(k.events)-dependencyKeeperMaxEvents:]...)
	}
}

// dependencyWindow buckets observations in the trailing window by
// manager and path.
func dependencyWindow(observations []dependencyHitObservation, window time.Duration) *afclient.DependencyKeeperWindow {
	cutoff := time.Now().UTC().Add(-window)
	byKey := make(map[string]*afclient.DependencyKeeperHit)
	var order []string
	for _, o := range observations {
		if o.at.Before(cutoff) {
			continue
		}
		key := o.manager + "\x00" + o.path
		bucket, ok := byKey[key]
		if !ok {
			bucket = &afclient.DependencyKeeperHit{Manager: o.manager, Path: o.path, MissReasons: map[string]int64{}}
			byKey[key] = bucket
			order = append(order, key)
		}
		if o.hit {
			bucket.Hits++
			continue
		}
		bucket.Misses++
		bucket.MissReasons[o.reason]++
	}
	sort.Strings(order)
	out := &afclient.DependencyKeeperWindow{}
	for _, key := range order {
		out.Hits = append(out.Hits, *byKey[key])
	}
	return out
}

// storeRecordKey names one tracked store: its manager joined with its
// scope digest.
func storeRecordKey(manager, scopeDigest string) string {
	return manager + "\x00" + scopeDigest
}

// dependencyScopeDigest binds one manager to its credential scope name.
// Only the digest is recorded; the scope value never reaches the records,
// stats, or events.
func dependencyScopeDigest(manager, scope string) string {
	name := "default"
	if scope != "" {
		name = scope
	}
	return fmt.Sprintf("%x", fnv64("dependency-keeper/v1/"+manager+"\x00"+name))
}

// fnv64 is a small stable 64-bit hash for scope digests.
func fnv64(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// dependencyStoreDir resolves one store's directory under the keeper
// root. It reports "" for an unsafe manager or digest so callers cannot
// be steered at an arbitrary path.
func dependencyStoreDir(root, manager, digest string) string {
	if root == "" || !validDependencyManager(manager) || !validDependencyDigest(digest) {
		return ""
	}
	return root + "/stores/" + manager + "/" + digest
}

// validDependencyManager accepts one path-safe manager segment: the
// manager names a directory in the keeper's storage layout.
func validDependencyManager(manager string) bool {
	if manager == "" || len(manager) > 64 {
		return false
	}
	for i := 0; i < len(manager); i++ {
		c := manager[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if i == 0 && !alnum {
			return false
		}
		if !alnum && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// validDependencyDigest accepts a lowercase hex digest.
func validDependencyDigest(digest string) bool {
	if digest == "" || len(digest) > 128 {
		return false
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// statPath stats one path for the presence check.
func statPath(path string) (fs.FileInfo, error) {
	return os.Stat(path) //nolint:gosec // the keeper's own store root only.
}

// storeDirPresent reports whether dir is a real directory on disk.
func storeDirPresent(dir string) bool {
	// The store layout owns verification; presence here only decides
	// whether a record survives reconciliation.
	info, err := statPath(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	return true
}

// dependencyKeeperStateDir resolves the keeper's state-directory root:
// dependency-keeper/ under the state home.
func dependencyKeeperStateDir() string {
	if dir := statehome.StateDir("dependency-keeper"); dir != "" {
		return dir
	}
	return statepath.Resolve("dependency-keeper", "/tmp/.donmai/dependency-keeper")
}
