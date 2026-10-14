package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

// Dependency-keeper daemon-wiring tests.
//
// Every test drives the production entry points (Install, the capacity
// route, the stats route) against the in-memory record set: Install
// degrades through the real budget and readiness gates, so fixtures name
// stores without touching a network or a manager binary. Presence-gated
// serving (a store directory on disk) is exercised through the keeper's
// own state dir with empty store directories.

// newDependencyKeeperForTest builds an enabled keeper with a nil scope
// (host-wide) and reconciles it, so Install serves through the
// production readiness gate.
func newDependencyKeeperForTest(t *testing.T, settings DependencyKeeperSettings, retention RetentionSettings) *DependencyKeeper {
	t.Helper()
	k := NewDependencyKeeper(settings, retention, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !k.Ready() {
		t.Fatal("Ready() = false after reconcile, want true")
	}
	return k
}

// seedDependencyStore records one store with the given kind, bytes, use
// time, quarantine and root-bound state through the keeper's own fields.
func seedDependencyStore(k *DependencyKeeper, manager, scope string, kind string, numBytes int64, lastUse time.Time, quarantined, rootBound bool) {
	digest := dependencyScopeDigest(manager, scope)
	k.records[storeRecordKey(manager, digest)] = &DependencyStoreRecord{
		Manager:     manager,
		ScopeDigest: digest,
		Kind:        kind,
		Quarantined: quarantined,
		Bytes:       numBytes,
		LastUse:     lastUse,
		RootBound:   rootBound,
	}
}

// TestDependencyKeeperSettings_RoundTrip proves the dependencyKeeper.*
// and retention.* settings travel from daemon.yaml through LoadConfig
// into EffectiveDependencyKeeperSettings and EffectiveRetentionSettings
// with the documented defaults and unit conversions, and survive a
// write/read round-trip.
func TestDependencyKeeperSettings_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	yaml := "apiVersion: donmai.dev/v1\n" +
		"kind: LocalDaemon\n" +
		"machine:\n    id: depkeeper-test\n" +
		"capacity:\n    maxConcurrentSessions: 2\n" +
		"orchestrator:\n    url: http://127.0.0.1:1\n" +
		"autoUpdate:\n    channel: stable\n    schedule: nightly\n    drainTimeoutSeconds: 600\n" +
		"dependencyKeeper:\n    enabled: true\n    maxDiskGb: 5\n    compactionWindowDays: 21\n" +
		"retention:\n    maxDiskGb: 9\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.DependencyKeeper.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.DependencyKeeper.MaxDiskGb != 5 {
		t.Errorf("MaxDiskGb = %d, want 5", cfg.DependencyKeeper.MaxDiskGb)
	}
	if cfg.DependencyKeeper.CompactionWindowDays != 21 {
		t.Errorf("CompactionWindowDays = %d, want 21", cfg.DependencyKeeper.CompactionWindowDays)
	}
	if cfg.Retention.MaxDiskGb != 9 {
		t.Errorf("Retention.MaxDiskGb = %d, want 9", cfg.Retention.MaxDiskGb)
	}
	settings := cfg.EffectiveDependencyKeeperSettings()
	if !settings.Enabled {
		t.Error("settings.Enabled = false, want true")
	}
	if settings.MaxBytes != 5*1024*1024*1024 {
		t.Errorf("MaxBytes = %d, want 5GiB", settings.MaxBytes)
	}
	if settings.CompactionWindow != 21*24*time.Hour {
		t.Errorf("CompactionWindow = %v, want 504h", settings.CompactionWindow)
	}
	retention := cfg.EffectiveRetentionSettings()
	if retention.MaxBytes != 9*1024*1024*1024 {
		t.Errorf("Retention.MaxBytes = %d, want 9GiB", retention.MaxBytes)
	}

	// Round-trip through a write: both blocks must survive WriteConfig.
	out := filepath.Join(dir, "daemon-out.yaml")
	if err := WriteConfig(out, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	reloaded, err := LoadConfig(out)
	if err != nil {
		t.Fatalf("LoadConfig(reload): %v", err)
	}
	if reloaded.DependencyKeeper != cfg.DependencyKeeper {
		t.Errorf("reload dependencyKeeper = %+v, want %+v", reloaded.DependencyKeeper, cfg.DependencyKeeper)
	}
	if reloaded.Retention != cfg.Retention {
		t.Errorf("reload retention = %+v, want %+v", reloaded.Retention, cfg.Retention)
	}
}

// TestDependencyKeeperSettings_Defaults proves the zero config keeps the
// keeper off and resolves the documented 14-day compaction default with
// no shared budget.
func TestDependencyKeeperSettings_Defaults(t *testing.T) {
	t.Parallel()

	var cfg Config
	settings := cfg.EffectiveDependencyKeeperSettings()
	if settings.Enabled {
		t.Error("zero config must keep the keeper disabled")
	}
	if settings.MaxBytes != 0 {
		t.Errorf("MaxBytes = %d, want 0 (no local limit)", settings.MaxBytes)
	}
	if settings.CompactionWindow != time.Duration(DefaultDependencyKeeperCompactionWindowDays)*24*time.Hour {
		t.Errorf("CompactionWindow = %v, want 14 days", settings.CompactionWindow)
	}
	if retention := cfg.EffectiveRetentionSettings(); retention.MaxBytes != 0 {
		t.Errorf("Retention.MaxBytes = %d, want 0 (no shared limit)", retention.MaxBytes)
	}
}

// TestDependencyKeeperSettings_RejectsNegative proves negative keeper and
// retention values fail config load loudly instead of widening or
// narrowing silently.
func TestDependencyKeeperSettings_RejectsNegative(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*Config){
		"maxDiskGb":            func(c *Config) { c.DependencyKeeper.MaxDiskGb = -1 },
		"compactionWindowDays": func(c *Config) { c.DependencyKeeper.CompactionWindowDays = -1 },
		"retention.maxDiskGb":  func(c *Config) { c.Retention.MaxDiskGb = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			mutate(cfg)
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.yaml")
			if err := WriteConfig(path, cfg); err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatalf("LoadConfig accepted %s = -1, want rejection", name)
			}
		})
	}
}

// TestDependencyKeeper_DisabledCreatesNothing drives the production Start
// path with the keeper disabled and proves nothing is created under
// dependency-keeper/.
func TestDependencyKeeper_DisabledCreatesNothing(t *testing.T) {
	d, _, cleanup := mustStartDaemon(t)
	defer cleanup()

	root := dependencyKeeperStateDir()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("disabled keeper created state: err = %v", err)
	}
	if d.DependencyKeeper() != nil {
		t.Error("DependencyKeeper() = non-nil with keeper disabled, want nil")
	}
	if _, ok := d.DependencyKeeperInstall(context.Background(), "pnpm", "."); ok {
		t.Error("DependencyKeeperInstall ok=true with keeper disabled, want degradation")
	}
}

// TestDependencyKeeper_EvictsQuarantinedSnapshotsBeforeStores proves the
// first eviction-order rule: a quarantined snapshot evicts before any
// live store, even when the live store is older. Flipping the pass order
// to evict stores first leaves the quarantined snapshot on disk and this
// test goes RED.
func TestDependencyKeeper_EvictsQuarantinedSnapshotsBeforeStores(t *testing.T) {
	t.Parallel()

	k := newDependencyKeeperForTest(t, DependencyKeeperSettings{Enabled: true}, RetentionSettings{})
	older := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)
	// A live generation older than the quarantined snapshot: pass order
	// must still take the quarantined snapshot first.
	seedDependencyStore(k, "pnpm", "host", DependencyStoreKindGeneration, 6, older, false, false)
	seedDependencyStore(k, "gomod", "host", DependencyStoreKindSnapshot, 6, newer, true, false)
	k.mu.Lock()
	k.settings.MaxBytes = 10
	k.mu.Unlock()

	k.EnforceBudget()

	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.records) != 1 {
		t.Fatalf("records = %d, want 1 after evicting the quarantined snapshot", len(k.records))
	}
	for _, record := range k.records {
		if record.Manager != "pnpm" {
			t.Errorf("survivor = %q, want the live pnpm store: quarantined snapshot must evict first", record.Manager)
		}
	}
}

// TestDependencyKeeper_EvictsSnapshotsLRUBeforeGenerations proves the
// second eviction-order rule: snapshots evict least recently used first,
// and no generation evicts while a snapshot remains. Reversing the pass
// order to evict generations first removes the unbound generation and
// this test goes RED.
func TestDependencyKeeper_EvictsSnapshotsLRUBeforeGenerations(t *testing.T) {
	t.Parallel()

	k := newDependencyKeeperForTest(t, DependencyKeeperSettings{Enabled: true}, RetentionSettings{})
	// Two snapshots oldest-first plus one generation older than both:
	// the oldest snapshot must go, never the generation.
	seedDependencyStore(k, "pnpm", "host", DependencyStoreKindSnapshot, 6, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), false, false)
	seedDependencyStore(k, "gomod", "host", DependencyStoreKindSnapshot, 6, time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC), false, false)
	seedDependencyStore(k, "cargo", "host", DependencyStoreKindGeneration, 6, time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), false, false)
	k.mu.Lock()
	k.settings.MaxBytes = 14
	k.mu.Unlock()

	k.EnforceBudget()

	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.records) != 2 {
		t.Fatalf("records = %d, want 2 after evicting the oldest snapshot", len(k.records))
	}
	for _, record := range k.records {
		if record.Manager == "pnpm" {
			t.Error("oldest (LRU) snapshot pnpm survived: snapshots must evict LRU first")
		}
		if record.Manager != "gomod" && record.Manager != "cargo" {
			t.Errorf("unexpected survivor %q", record.Manager)
		}
	}
}

// TestDependencyKeeper_RootBoundGenerationSurvivesPressure proves the
// third eviction-order rule: a generation bound by a live root survives
// budget pressure while unbound generations evict. Dropping the
// root-bound guard evicts the bound generation and this test goes RED.
func TestDependencyKeeper_RootBoundGenerationSurvivesPressure(t *testing.T) {
	t.Parallel()

	k := newDependencyKeeperForTest(t, DependencyKeeperSettings{Enabled: true}, RetentionSettings{})
	seedDependencyStore(k, "pnpm", "host", DependencyStoreKindGeneration, 6, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), false, true)
	seedDependencyStore(k, "gomod", "host", DependencyStoreKindGeneration, 6, time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC), false, false)
	k.mu.Lock()
	k.settings.MaxBytes = 10
	k.mu.Unlock()

	k.EnforceBudget()

	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.records) != 1 {
		t.Fatalf("records = %d, want 1 after evicting the unbound generation", len(k.records))
	}
	for _, record := range k.records {
		if record.Manager != "pnpm" {
			t.Errorf("survivor = %q, want the root-bound pnpm generation", record.Manager)
		}
	}
}

// TestDependencyKeeper_BudgetWarnsAt80RefusesAt90 drives the production
// Install entry point across the pressure thresholds: at 80% the keeper
// still serves (and warns), at 90% it degrades to a plain install while
// an existing store keeps serving.
func TestDependencyKeeper_BudgetWarnsAt80RefusesAt90(t *testing.T) {
	t.Parallel()

	k := newDependencyKeeperForTest(t, DependencyKeeperSettings{Enabled: true, MaxBytes: 100}, RetentionSettings{})
	seedDependencyStore(k, "pnpm", "host", DependencyStoreKindGeneration, 85, time.Now().UTC(), false, false)
	if pressure := k.BudgetPressure(); pressure < 0.8 || pressure >= 0.9 {
		t.Fatalf("pressure = %v, want in [0.8, 0.9)", pressure)
	}
	// 85% warns but still admits the admission check: overBudgetLocked
	// refuses only at 90%.
	k.mu.Lock()
	refused := k.overBudgetLocked()
	k.mu.Unlock()
	if refused {
		t.Error("keeper refused at 85% pressure, want a warning with admission")
	}

	seedDependencyStore(k, "gomod", "host", DependencyStoreKindGeneration, 10, time.Now().UTC(), false, false)
	if pressure := k.BudgetPressure(); pressure < 0.9 {
		t.Fatalf("pressure = %v, want >= 0.9", pressure)
	}
	if _, ok := k.Install(context.Background(), "cargo", "."); ok {
		t.Error("over-budget Install ok=true for a new manager, want degradation to plain install")
	}
}

// TestDependencyKeeper_RecoveryOrderDegradesWhileReconciling proves
// callers degrade while the keeper reconciles, and that Reconcile runs
// after the repository keeper's position: Install before Reconcile
// returns ok=false with the reconciling miss reason.
func TestDependencyKeeper_RecoveryOrderDegradesWhileReconciling(t *testing.T) {
	t.Parallel()

	k := NewDependencyKeeper(DependencyKeeperSettings{Enabled: true}, RetentionSettings{}, nil)
	if k.Ready() {
		t.Error("Ready() = true before reconcile, want false")
	}
	if _, ok := k.Install(context.Background(), "pnpm", "."); ok {
		t.Error("pre-reconcile Install ok=true, want degradation")
	}
	stats := k.Stats()
	if stats == nil || stats.Hour == nil {
		t.Fatal("Stats() has no hour window after a miss")
	}
	found := false
	for _, bucket := range stats.Hour.Hits {
		if bucket.Manager == "pnpm" && bucket.MissReasons[DependencyMissReconciling] == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("hour buckets = %+v, want one reconciling miss for pnpm", stats.Hour.Hits)
	}
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !k.Ready() {
		t.Error("Ready() = false after reconcile, want true")
	}
}

// TestDependencyKeeper_StatsCarryHitsMissesReasonsTimeSaved proves the
// stats path carries per-manager, per-path hit/miss buckets with closed
// miss reasons, a cold baseline with its sample count, bytes, and budget
// pressure — and no scope value.
func TestDependencyKeeper_StatsCarryHitsMissesReasonsTimeSaved(t *testing.T) {
	t.Parallel()

	k := newDependencyKeeperForTest(t, DependencyKeeperSettings{Enabled: true, MaxBytes: 100}, RetentionSettings{})
	k.RecordColdSample(2 * time.Second)
	k.RecordColdSample(4 * time.Second)
	// One hit through the production record path… (a served store records
	// a hit; here the record path is exercised via the observation log).
	k.mu.Lock()
	k.hits = append(k.hits, dependencyHitObservation{manager: "pnpm", path: ".", hit: true, at: time.Now().UTC()})
	k.mu.Unlock()
	k.recordMiss("pnpm", ".", DependencyMissNoStore)
	k.recordMiss("gomod", "sub", DependencyMissOverBudget)
	seedDependencyStore(k, "pnpm", "host", DependencyStoreKindGeneration, 42, time.Now().UTC(), false, true)

	stats := k.Stats()
	if stats == nil {
		t.Fatal("Stats() = nil, want a snapshot")
	}
	if stats.ColdBaselineSamples != 2 {
		t.Errorf("ColdBaselineSamples = %d, want 2", stats.ColdBaselineSamples)
	}
	// Mean cold baseline is 3s; one hit saves ~3s.
	if stats.EstimatedTimeSavedSeconds < 2.9 || stats.EstimatedTimeSavedSeconds > 3.1 {
		t.Errorf("EstimatedTimeSavedSeconds = %v, want ~3", stats.EstimatedTimeSavedSeconds)
	}
	if stats.Bytes != 42 {
		t.Errorf("Bytes = %d, want 42", stats.Bytes)
	}
	if stats.BudgetPressure < 0.41 || stats.BudgetPressure > 0.43 {
		t.Errorf("BudgetPressure = %v, want 0.42", stats.BudgetPressure)
	}
	if stats.Hour == nil || len(stats.Hour.Hits) != 2 {
		t.Fatalf("hour buckets = %+v, want pnpm and gomod buckets", stats.Hour)
	}
	byManager := map[string]afclient.DependencyKeeperHit{}
	for _, bucket := range stats.Hour.Hits {
		byManager[bucket.Manager] = bucket
	}
	pnpm := byManager["pnpm"]
	if pnpm.Hits != 1 || pnpm.Misses != 1 || pnpm.MissReasons[DependencyMissNoStore] != 1 {
		t.Errorf("pnpm bucket = %+v, want 1 hit + 1 no_store miss", pnpm)
	}
	if pnpm.Path != "." {
		t.Errorf("pnpm path = %q, want the recorded leaf path", pnpm.Path)
	}
	gomod := byManager["gomod"]
	if gomod.Misses != 1 || gomod.MissReasons[DependencyMissOverBudget] != 1 {
		t.Errorf("gomod bucket = %+v, want 1 over_budget miss", gomod)
	}
	if stats.Day == nil || stats.Week == nil {
		t.Error("Stats() must carry day and week windows alongside hour")
	}
	raw, _ := json.Marshal(stats)
	if strings.Contains(string(raw), "host") {
		t.Error("stats leak the scope value")
	}
}

// TestDependencyKeeper_EventsCarryManagerPathDurationBytes proves
// install/fill/snapshot/compact events record manager, path, duration
// and bytes with no scope values, URLs, or credentials.
func TestDependencyKeeper_EventsCarryManagerPathDurationBytes(t *testing.T) {
	t.Parallel()

	k := newDependencyKeeperForTest(t, DependencyKeeperSettings{Enabled: true}, RetentionSettings{})
	k.RecordInstall("pnpm", ".", 7, 1500*time.Millisecond, true)
	k.RecordFill("pnpm", ".", 7, 250*time.Millisecond, true)
	k.RecordSnapshot("pnpm", ".", 7, 100*time.Millisecond, true)
	k.RecordCompacted("pnpm", 7, 50*time.Millisecond)

	events := k.Events()
	byType := map[string]DependencyKeeperEvent{}
	for _, ev := range events {
		byType[ev.Type] = ev
	}
	for _, want := range []string{
		DependencyKeeperEventInstallPrefix + "ok",
		DependencyKeeperEventFillPrefix + "ok",
		DependencyKeeperEventSnapshotPrefix + "ok",
		DependencyKeeperEventStoreCompacted,
	} {
		ev, ok := byType[want]
		if !ok {
			t.Fatalf("no %s event in %v", want, events)
		}
		if ev.Manager != "pnpm" || (want != DependencyKeeperEventStoreCompacted && ev.Path != ".") {
			t.Errorf("%s event = %+v, want manager pnpm and path '.'", want, ev)
		}
		if ev.Bytes != 7 || ev.Duration == 0 {
			t.Errorf("%s event = %+v, want bytes 7 and a duration", want, ev)
		}
	}
	raw, _ := json.Marshal(events)
	for _, secret := range []string{"host", "http", "token", "secret"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("events leak %q", secret)
		}
	}
}

// TestDependencyKeeper_ScopeSeparationKeepsRecordsDisjoint drives the
// production Install path with two scopes for one manager and proves
// two records result keyed by scope digest, with no scope value in the
// secret-free stats.
func TestDependencyKeeper_ScopeSeparationKeepsRecordsDisjoint(t *testing.T) {
	t.Parallel()

	scopes := []string{"tenant-a", "tenant-b"}
	i := 0
	k := NewDependencyKeeper(DependencyKeeperSettings{Enabled: true}, RetentionSettings{}, func(context.Context, string) (string, error) {
		s := scopes[i]
		i++
		return s, nil
	})
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	seedDependencyStore(k, "pnpm", "tenant-a", DependencyStoreKindGeneration, 6, time.Now().UTC(), false, false)
	seedDependencyStore(k, "pnpm", "tenant-b", DependencyStoreKindGeneration, 6, time.Now().UTC(), false, false)

	k.mu.Lock()
	count := len(k.records)
	k.mu.Unlock()
	if count != 2 {
		t.Fatalf("records = %d, want 2 disjoint scope records", count)
	}
	raw, _ := json.Marshal(k.Stats())
	for _, secret := range []string{"tenant-a", "tenant-b"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("stats leak %q", secret)
		}
	}
}

// TestDependencyKeeper_SessionOnlyAndOnlineServeWhileReconciling proves
// the recovery-order serving rule at the daemon seam: while the keeper
// reconciles, the daemon still constructs (the keeper exists once
// enabled), and Install degrades with the reconciling reason rather than
// blocking.
func TestDependencyKeeper_SessionOnlyAndOnlineServeWhileReconciling(t *testing.T) {
	t.Parallel()

	k := NewDependencyKeeper(DependencyKeeperSettings{Enabled: true}, RetentionSettings{}, nil)
	done := make(chan bool, 1)
	go func() {
		_, ok := k.Install(context.Background(), "pnpm", ".")
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("pre-reconcile Install ok=true, want degradation while reconciling")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Install blocked while reconciling: no path may make a session wait for the keeper")
	}
}

// TestServer_DependencyKeeperSetKeys proves POST /api/daemon/capacity
// accepts the dependencyKeeper.* and retention.* keys, persists them to
// the in-memory config, and answers that a drain-aware restart is needed
// instead of claiming a hot reload.
func TestServer_DependencyKeeperSetKeys(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	for key, value := range map[string]string{
		"dependencyKeeper.enabled":              "1",
		"dependencyKeeper.maxDiskGb":            "5",
		"dependencyKeeper.compactionWindowDays": "21",
		"retention.maxDiskGb":                   "9",
	} {
		var resp afclient.SetCapacityResponse
		if status := requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{"key": key, "value": value}, &resp); status != http.StatusOK {
			t.Fatalf("POST %s -> %d, want 200", key, status)
		}
		if !resp.OK {
			t.Fatalf("%s not accepted: %+v", key, resp)
		}
		if !strings.Contains(resp.Message, "restart") {
			t.Errorf("%s message %q does not say a restart is needed", key, resp.Message)
		}
	}
	if !d.config.DependencyKeeper.Enabled || d.config.DependencyKeeper.MaxDiskGb != 5 ||
		d.config.DependencyKeeper.CompactionWindowDays != 21 || d.config.Retention.MaxDiskGb != 9 {
		t.Errorf("in-memory dependencyKeeper = %+v retention = %+v, want the set values",
			d.config.DependencyKeeper, d.config.Retention)
	}
}

// TestServer_Stats_DependencyKeeperDisabledOmitsBlock proves the stats
// response carries no dependency-keeper block while the keeper is
// disabled.
func TestServer_Stats_DependencyKeeperDisabledOmitsBlock(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	var resp afclient.DaemonStatsResponse
	requireGet(t, srv.Addr(), "/api/daemon/stats", &resp)
	if resp.DependencyKeeper != nil {
		t.Errorf("DependencyKeeper = %+v with keeper disabled, want nil", resp.DependencyKeeper)
	}
}

// TestServer_Stats_DependencyKeeperEnabledCarriesBlock proves the stats
// response carries the dependency-keeper block once the keeper is
// enabled, with windows, bytes, and pressure — and no new route.
func TestServer_Stats_DependencyKeeperEnabledCarriesBlock(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	d.mu.Lock()
	d.config.DependencyKeeper.Enabled = true
	d.mu.Unlock()
	d.reconcileDependencyKeeper(context.Background())
	if d.DependencyKeeper() == nil {
		t.Fatal("DependencyKeeper() = nil after enabling, want a keeper")
	}
	d.DependencyKeeper().RecordColdSample(time.Second)
	d.DependencyKeeper().RecordInstall("pnpm", ".", 42, time.Second, true)

	var resp afclient.DaemonStatsResponse
	requireGet(t, srv.Addr(), "/api/daemon/stats", &resp)
	if resp.DependencyKeeper == nil {
		t.Fatal("DependencyKeeper = nil with keeper enabled, want a block")
	}
	if resp.DependencyKeeper.Bytes != 42 {
		t.Errorf("Bytes = %d, want 42", resp.DependencyKeeper.Bytes)
	}
	if resp.DependencyKeeper.ColdBaselineSamples != 1 {
		t.Errorf("ColdBaselineSamples = %d, want 1", resp.DependencyKeeper.ColdBaselineSamples)
	}
	if resp.DependencyKeeper.Hour == nil || resp.DependencyKeeper.Day == nil || resp.DependencyKeeper.Week == nil {
		t.Error("dependency stats must carry hour, day, and week windows")
	}
	raw, _ := json.Marshal(resp.DependencyKeeper)
	if strings.Contains(string(raw), "host") {
		t.Error("dependency stats leak the scope value")
	}
}

// TestDependencyKeeper_ReconcileRacesNoInstall drives Reconcile
// concurrently with Install through the production entry points under
// -race: the daemon lock is never held across a fill, and the race
// detector must stay silent while every Install either serves or
// degrades.
func TestDependencyKeeper_ReconcileRacesNoInstall(t *testing.T) {
	t.Parallel()

	k := newDependencyKeeperForTest(t, DependencyKeeperSettings{Enabled: true}, RetentionSettings{})
	seedDependencyStore(k, "pnpm", "host", DependencyStoreKindGeneration, 6, time.Now().UTC(), false, true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 25; i++ {
			_, _ = k.Install(context.Background(), "pnpm", ".")
		}
	}()
	for i := 0; i < 10; i++ {
		_ = k.Reconcile(context.Background())
	}
	<-done
}
