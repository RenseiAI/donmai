package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/repokeeper"
)

// keeper test helpers: local git fixtures and store-backed keepers.
//
// Every keeper test drives the production entry points (Acquire, the
// capacity route, the stats route) against real local git repositories:
// Acquire clones through the store, so fixtures must be cloneable without
// a network. runKeeperGitT is independent of the production git path, so
// these tests exercise the keeper as a black box.

func runKeeperGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git")
	cmd.Args = append(cmd.Args, args...)
	cmd.Dir = dir
	cmd.Env = append(
		os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newKeeperSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runKeeperGitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	runKeeperGitT(t, dir, "add", "a.go")
	runKeeperGitT(t, dir, "commit", "-q", "-m", "first")
	return dir
}

// newKeeperForTest builds a store-backed keeper over a fresh state dir with
// the worktree root beside it on the same filesystem.
func newKeeperForTest(t *testing.T, settings RepoKeeperSettings, scope RepoKeeperScopeFunc, auth RepoKeeperGitAuth) *RepoKeeper {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	store, err := repokeeper.New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("repokeeper.New() error = %v", err)
	}
	k := NewRepoKeeper(store, settings, scope, auth)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return k
}

func keeperDigestFor(t *testing.T, source, scope string) string {
	t.Helper()
	digest, err := repokeeper.MirrorDigest(source, scope)
	if err != nil {
		t.Fatalf("MirrorDigest() error = %v", err)
	}
	return digest
}

// TestRepoKeeperSettings_RoundTrip proves the repoKeeper.* settings travel
// from daemon.yaml through LoadConfig into EffectiveRepoKeeperSettings with
// the documented defaults and unit conversions.
func TestRepoKeeperSettings_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	yaml := "apiVersion: donmai.dev/v1\n" +
		"kind: LocalDaemon\n" +
		"machine:\n    id: keeper-test\n" +
		"capacity:\n    maxConcurrentSessions: 2\n" +
		"orchestrator:\n    url: http://127.0.0.1:1\n" +
		"autoUpdate:\n    channel: stable\n    schedule: nightly\n    drainTimeoutSeconds: 600\n" +
		"repoKeeper:\n    enabled: true\n    maxDiskGb: 5\n    fetchIntervalSeconds: 45\n    authorizationWindowSeconds: 120\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.RepoKeeper.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.RepoKeeper.MaxDiskGb != 5 {
		t.Errorf("MaxDiskGb = %d, want 5", cfg.RepoKeeper.MaxDiskGb)
	}
	if cfg.RepoKeeper.FetchIntervalSeconds != 45 {
		t.Errorf("FetchIntervalSeconds = %d, want 45", cfg.RepoKeeper.FetchIntervalSeconds)
	}
	if cfg.RepoKeeper.AuthorizationWindowSeconds != 120 {
		t.Errorf("AuthorizationWindowSeconds = %d, want 120", cfg.RepoKeeper.AuthorizationWindowSeconds)
	}
	settings := cfg.EffectiveRepoKeeperSettings()
	if !settings.Enabled {
		t.Error("settings.Enabled = false, want true")
	}
	if settings.MaxBytes != 5*1024*1024*1024 {
		t.Errorf("MaxBytes = %d, want 5GiB", settings.MaxBytes)
	}
	if settings.FetchInterval != 45*time.Second {
		t.Errorf("FetchInterval = %v, want 45s", settings.FetchInterval)
	}
	if settings.AuthorizationWindow != 120*time.Second {
		t.Errorf("AuthorizationWindow = %v, want 120s", settings.AuthorizationWindow)
	}

	// Round-trip through a write: the block must survive WriteConfig.
	out := filepath.Join(dir, "daemon-out.yaml")
	if err := WriteConfig(out, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	reloaded, err := LoadConfig(out)
	if err != nil {
		t.Fatalf("LoadConfig(reload): %v", err)
	}
	if reloaded.RepoKeeper != cfg.RepoKeeper {
		t.Errorf("reload repoKeeper = %+v, want %+v", reloaded.RepoKeeper, cfg.RepoKeeper)
	}
}

// TestRepoKeeperSettings_Defaults proves the zero config keeps the keeper off
// and resolves the documented default intervals.
func TestRepoKeeperSettings_Defaults(t *testing.T) {
	t.Parallel()

	var cfg Config
	settings := cfg.EffectiveRepoKeeperSettings()
	if settings.Enabled {
		t.Error("zero config must keep the keeper disabled")
	}
	if settings.MaxBytes != 0 {
		t.Errorf("MaxBytes = %d, want 0 (no limit)", settings.MaxBytes)
	}
	if settings.FetchInterval != time.Duration(DefaultRepoKeeperFetchIntervalSeconds)*time.Second {
		t.Errorf("FetchInterval = %v, want default", settings.FetchInterval)
	}
	if settings.AuthorizationWindow != time.Duration(DefaultRepoKeeperAuthorizationWindowSeconds)*time.Second {
		t.Errorf("AuthorizationWindow = %v, want default", settings.AuthorizationWindow)
	}
}

// TestRepoKeeperSettings_RejectsNegative proves negative keeper values fail
// config load loudly instead of widening or narrowing silently.
func TestRepoKeeperSettings_RejectsNegative(t *testing.T) {
	t.Parallel()

	cases := map[string]RepoKeeperConfig{
		"maxDiskGb":            {MaxDiskGb: -1},
		"fetchIntervalSeconds": {FetchIntervalSeconds: -1},
		"authorizationWindow":  {AuthorizationWindowSeconds: -1},
	}
	for name, rk := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.RepoKeeper = rk
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

// TestRepoKeeper_DisabledCreatesNothing drives the production Start path with
// the keeper disabled and proves nothing is created under repo-keeper/.
func TestRepoKeeper_DisabledCreatesNothing(t *testing.T) {
	d, _, cleanup := mustStartDaemon(t)
	defer cleanup()

	root := repoKeeperRoot()
	if _, err := os.Stat(filepath.Join(root, "mirrors")); !os.IsNotExist(err) {
		t.Errorf("disabled keeper created mirrors state: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "catalog.json")); !os.IsNotExist(err) {
		t.Errorf("disabled keeper created catalog state: err = %v", err)
	}
	if d.RepoKeeper() != nil {
		t.Error("RepoKeeper() = non-nil with keeper disabled, want nil")
	}
	if _, ok := d.RepoKeeperMirror(context.Background(), "https://example.com/a/b.git"); ok {
		t.Error("RepoKeeperMirror ok=true with keeper disabled, want degradation")
	}
}

// TestRepoKeeper_ProductionLayoutUnderStateHome builds the keeper the way
// Start does and proves its mirrors and catalog land directly under
// <state-home>/repo-keeper/ (the root the disabled-keeper test watches),
// never one level deeper in a nested repo-keeper/repo-keeper/.
func TestRepoKeeper_ProductionLayoutUnderStateHome(t *testing.T) {
	root := repoKeeperRoot()
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	cfg := DefaultConfig()
	cfg.RepoKeeper.Enabled = true
	d := New(Options{})
	d.config = cfg
	k := d.repoKeeperFor(filepath.Join(filepath.Dir(root), "worktrees"))
	if k == nil {
		t.Fatal("repoKeeperFor() = nil with the keeper enabled, want a keeper")
	}
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	dirLease, ok := k.Acquire(context.Background(), newKeeperSourceRepo(t))
	dir := dirLease.Dir()
	dirLease.Release()
	if !ok {
		t.Fatal("Acquire degraded, want a served mirror")
	}
	if got, want := filepath.Dir(dir), filepath.Join(root, "mirrors"); got != want {
		t.Errorf("mirror parent = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, "catalog.json")); err != nil {
		t.Errorf("catalog not at the keeper root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "repo-keeper")); !os.IsNotExist(err) {
		t.Errorf("nested repo-keeper/ under the keeper root: err = %v, want not-exist", err)
	}
}

// TestRepoKeeper_ScopeSeam drives the production Acquire entry point with two
// scopes for one remote and proves two store-backed mirrors result, with no
// scope value or URL in the secret-free catalog.
func TestRepoKeeper_ScopeSeam(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	scopes := []string{"tenant-a", "tenant-b"}
	i := 0
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, func(context.Context, string) (string, error) {
		s := scopes[i]
		i++
		return s, nil
	}, nil)
	dirALease, okA := k.Acquire(context.Background(), remote)
	dirA := dirALease.Dir()
	dirALease.Release()
	dirBLease, okB := k.Acquire(context.Background(), remote)
	dirB := dirBLease.Dir()
	dirBLease.Release()
	if !okA || !okB {
		t.Fatalf("Acquire ok = %v/%v, want true/true", okA, okB)
	}
	if dirA == dirB {
		t.Fatalf("two scopes share one mirror dir %q, want two directories", dirA)
	}
	if want := keeperDigestFor(t, remote, "tenant-a"); filepath.Base(dirA) != want {
		t.Errorf("scope-a dir = %q, want digest %q", filepath.Base(dirA), want)
	}
	if want := keeperDigestFor(t, remote, "tenant-b"); filepath.Base(dirB) != want {
		t.Errorf("scope-b dir = %q, want digest %q", filepath.Base(dirB), want)
	}

	entries, err := k.store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("catalog has %d entries, want 2", len(entries))
	}
	raw, _ := json.Marshal(entries)
	for _, secret := range []string{"tenant-a", "tenant-b"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("catalog leaks %q", secret)
		}
	}
}

// TestRepoKeeper_BudgetExceededFallsBackToPlainClone drives the production
// Acquire entry point with the budget already exhausted and proves the caller
// degrades: ok=false, while an existing mirror still serves.
func TestRepoKeeper_BudgetExceededFallsBackToPlainClone(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	other := newKeeperSourceRepo(t)
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true, MaxBytes: 1}, nil, nil)
	dirLease, ok := k.Acquire(context.Background(), remote)
	if !ok {
		t.Fatal("first Acquire ok=false, want a served mirror")
	}
	dir := dirLease.Dir()
	digest := filepath.Base(dir)
	dirLease.Release()
	k.RecordFetch(digest, 1<<30, time.Second, false)

	if dir2Lease, ok := k.Acquire(context.Background(), other); ok {
		t.Errorf("over-budget Acquire ok=true (dir %v), want degradation to plain clone", dir2Lease)
	}
	if _, ok := k.Acquire(context.Background(), remote); !ok {
		t.Error("existing mirror must still serve while over budget")
	}
}

// TestRepoKeeper_EvictsLeastRecentlyUsedWithoutInflightSeed proves the
// minimal budget rule: eviction takes mirrors with no in-flight seed, least
// recently used first, and never evicts a seeded mirror.
//
// Three unpinned mirrors carry distinct use times (oldest → newest: a, b,
// c). The budget admits exactly two, so one eviction must take the oldest
// digest. Flipping the victim order to most-recently-used evicts c instead
// and this test goes RED, pinning "least recently used first". A seeded
// fourth mirror proves in-flight seeds are never evicted.
func TestRepoKeeper_EvictsLeastRecentlyUsedWithoutInflightSeed(t *testing.T) {
	t.Parallel()

	remotes := []string{
		newKeeperSourceRepo(t),
		newKeeperSourceRepo(t),
		newKeeperSourceRepo(t),
		newKeeperSourceRepo(t),
	}
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, nil, nil)
	digests := make([]string, 0, len(remotes))
	leases := make([]*repokeeper.Lease, 0, len(remotes))
	for _, remote := range remotes {
		dirLease, ok := k.Acquire(context.Background(), remote)
		if !ok {
			t.Fatalf("acquire %s: degraded", remote)
		}
		digests = append(digests, filepath.Base(dirLease.Dir()))
		leases = append(leases, dirLease)
	}
	// Eviction takes the exclusive maintenance lock, which waits for
	// outstanding leases: release every acquired lease before enforcing
	// the budget, as a seeding caller would after its clone finishes.
	for _, lease := range leases {
		lease.Release()
	}
	// Record byte loads through the production RecordFetch path and arm the
	// cap just before enforcement, so the admission gate stays out of the
	// setup: this test exercises eviction order, not the admission gate.
	for _, digest := range digests {
		k.RecordFetch(digest, 6, time.Millisecond, false)
	}
	k.mu.Lock()
	k.settings.MaxBytes = 20
	// Assign distinct use times oldest-first: a < b < c < seeded. The
	// in-memory markers set here ride alongside the store rows; the
	// assertion below reads the store's directory truth.
	for i, digest := range digests {
		k.lastUse[digest] = time.Date(2000, 1, 1, 0, 0, i, 0, time.UTC)
		if err := k.store.TouchUse(digest, time.Date(2000, 1, 1, 0, 0, i, 0, time.UTC)); err != nil {
			t.Fatalf("TouchUse: %v", err)
		}
	}
	k.mu.Unlock()

	// Pin the newest mirror with an in-flight seed, then force the
	// budget: 24 bytes against a 20-byte cap evicts exactly one mirror,
	// and it must be the oldest unpinned one (a).
	k.SeedBegin(digests[3])
	k.EnforceBudget()

	stats := k.Stats()
	if stats.MirrorCount != 3 {
		t.Fatalf("MirrorCount = %d, want 3 after evicting the LRU mirror", stats.MirrorCount)
	}
	if dir := k.store.MirrorDir(digests[0]); dir == "" {
		t.Fatalf("MirrorDir(%q) empty, want the store path", digests[0])
	} else if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("oldest (LRU) mirror %s still on disk: err = %v", digests[0], err)
	}
	for i, d := range digests[1:] {
		if _, err := os.Stat(k.store.MirrorDir(d)); err != nil {
			t.Errorf("mirror %d (%s) evicted instead of the oldest: %v", i+1, d, err)
		}
	}
	k.SeedEnd(digests[3], 6, time.Millisecond)
}

// TestRepoKeeper_RecoveryOrderDegradesWhileReconciling proves callers degrade
// while the keeper reconciles: Acquire before Reconcile returns ok=false.
func TestRepoKeeper_RecoveryOrderDegradesWhileReconciling(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	store, err := repokeeper.New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("repokeeper.New() error = %v", err)
	}
	k := NewRepoKeeper(store, RepoKeeperSettings{Enabled: true}, nil, nil)
	if _, ok := k.Acquire(context.Background(), remote); ok {
		t.Error("pre-reconcile Acquire ok=true, want degradation")
	}
	if k.Ready() {
		t.Error("Ready() = true before reconcile, want false")
	}
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !k.Ready() {
		t.Error("Ready() = false after reconcile, want true")
	}
	if _, ok := k.Acquire(context.Background(), remote); !ok {
		t.Error("post-reconcile Acquire ok=false, want a served mirror")
	}
}

// TestRepoKeeper_StatsCarryNoSecrets drives the production stats path and
// proves the snapshot carries counts and bytes but no scope values or URLs.
func TestRepoKeeper_StatsCarryNoSecrets(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, func(context.Context, string) (string, error) {
		return "tenant-secret", nil
	}, nil)
	dirLease, ok := k.Acquire(context.Background(), remote)
	if !ok {
		t.Fatal("Acquire degraded")
	}
	dir := dirLease.Dir()
	dirLease.Release()
	k.RecordFetch(filepath.Base(dir), 42, 2*time.Second, true)

	stats := k.Stats()
	if stats.MirrorCount != 1 {
		t.Errorf("MirrorCount = %d, want 1", stats.MirrorCount)
	}
	if stats.RevokedCount != 1 {
		t.Errorf("RevokedCount = %d, want 1", stats.RevokedCount)
	}
	if stats.Bytes != 42 {
		t.Errorf("Bytes = %d, want 42", stats.Bytes)
	}
	if stats.LastFetchAt == "" {
		t.Error("LastFetchAt empty, want the recorded fetch time")
	}
	raw, _ := json.Marshal(stats)
	for _, secret := range []string{"tenant-secret", "example.com"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("stats leak %q", secret)
		}
	}
}

// TestRepoKeeper_EventsCarryDurationAndBytes proves fetch/seed/evict events
// record duration and bytes with no scope values or URLs.
func TestRepoKeeper_EventsCarryDurationAndBytes(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true, MaxBytes: 10}, nil, nil)
	dirLease, ok := k.Acquire(context.Background(), remote)
	if !ok {
		t.Fatal("Acquire degraded")
	}
	dir := dirLease.Dir()
	dirLease.Release()
	digest := filepath.Base(dir)
	k.RecordFetch(digest, 7, 1500*time.Millisecond, false)
	k.SeedBegin(digest)
	k.SeedEnd(digest, 7, 250*time.Millisecond)

	events := k.Events()
	byType := map[string]RepoKeeperEvent{}
	for _, ev := range events {
		byType[ev.Type] = ev
	}
	fetch, ok := byType[RepoKeeperEventFetch]
	if !ok {
		t.Fatalf("no %s event in %v", RepoKeeperEventFetch, events)
	}
	if fetch.Bytes != 7 || fetch.Duration != 1500*time.Millisecond {
		t.Errorf("fetch event = %+v, want bytes 7 duration 1.5s", fetch)
	}
	seed, ok := byType[RepoKeeperEventSeed]
	if !ok {
		t.Fatalf("no %s event in %v", RepoKeeperEventSeed, events)
	}
	if seed.Bytes != 7 || seed.Duration != 250*time.Millisecond {
		t.Errorf("seed event = %+v, want bytes 7 duration 250ms", seed)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "example.com") {
		t.Error("events leak the remote URL")
	}
}

// TestServer_RepoKeeperSetKeys proves POST /api/daemon/capacity accepts the
// repoKeeper.* keys, persists them to the in-memory config, and answers that
// a drain-aware restart is needed instead of claiming a hot reload.
func TestServer_RepoKeeperSetKeys(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	for key, value := range map[string]string{
		"repoKeeper.enabled":                    "1",
		"repoKeeper.maxDiskGb":                  "5",
		"repoKeeper.fetchIntervalSeconds":       "45",
		"repoKeeper.authorizationWindowSeconds": "120",
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
	if !d.config.RepoKeeper.Enabled || d.config.RepoKeeper.MaxDiskGb != 5 ||
		d.config.RepoKeeper.FetchIntervalSeconds != 45 || d.config.RepoKeeper.AuthorizationWindowSeconds != 120 {
		t.Errorf("in-memory repoKeeper = %+v, want the set values", d.config.RepoKeeper)
	}
}

// TestServer_Stats_RepoKeeperDisabledOmitsBlock proves the stats response
// carries no keeper block while the keeper is disabled.
func TestServer_Stats_RepoKeeperDisabledOmitsBlock(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	var resp afclient.DaemonStatsResponse
	requireGet(t, srv.Addr(), "/api/daemon/stats", &resp)
	if resp.RepoKeeper != nil {
		t.Errorf("RepoKeeper = %+v with keeper disabled, want nil", resp.RepoKeeper)
	}
}

// TestRepoKeeper_ReconcileRacesNoAcquire drives Reconcile concurrently with
// Acquire through the production entry points under -race: the catalog
// lives in the store (which serializes per-mirror work), the daemon lock
// is never held across git, and the race detector must stay silent while
// every Acquire either serves or degrades.
func TestRepoKeeper_ReconcileRacesNoAcquire(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, nil, nil)
	if _, ok := k.Acquire(context.Background(), remote); !ok {
		t.Fatal("seed Acquire degraded")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_, _ = k.Acquire(context.Background(), remote)
		}
	}()
	for i := 0; i < 5; i++ {
		if err := k.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	<-done
}

// TestRepoKeeper_CredentialBearingRemoteStaysSecretFree drives the production
// Acquire entry point with a userinfo-, query-, and fragment-bearing remote
// and proves the credentialed form degrades (a credential in the URL would
// reach git's argv and the persisted origin) while the bare remote serves
// and no credential material reaches the mirror config, the sidecar, or
// the catalog. The query/fragment-bearing bare form still shares the bare
// remote's mirror.
func TestRepoKeeper_CredentialBearingRemoteStaysSecretFree(t *testing.T) {
	t.Parallel()

	srcDir := newKeeperSourceRepo(t)
	bare := "file://localhost" + srcDir
	const secret = "secrettoken"
	credentialed := "file://user:" + secret + "@localhost" + srcDir + "?token=abc123#frag"

	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, nil, nil)
	bareLease, ok := k.Acquire(context.Background(), bare)
	if !ok {
		t.Fatal("bare Acquire degraded")
	}
	bareDir := bareLease.Dir()
	bareLease.Release()
	decoratedLease, ok := k.Acquire(context.Background(), bare+"?token=abc123#frag")
	if !ok {
		t.Fatal("query/fragment Acquire degraded")
	}
	decoratedDir := decoratedLease.Dir()
	decoratedLease.Release()
	if decoratedDir != bareDir {
		t.Fatalf("query/fragment remote split mirrors: %q vs %q, want one digest", decoratedDir, bareDir)
	}
	if credLease, ok := k.Acquire(context.Background(), credentialed); ok || credLease != nil {
		t.Fatalf("credentialed Acquire = (%v, %v), want (nil, false)", credLease, ok)
	}

	var sweep strings.Builder
	config, err := os.ReadFile(filepath.Join(bareDir, "config")) //nolint:gosec // test-owned mirror dir
	if err != nil {
		t.Fatalf("read mirror config: %v", err)
	}
	sweep.Write(config)
	sweep.WriteByte('\n')
	sidecar, err := os.ReadFile(filepath.Join(bareDir, "keeper.json")) //nolint:gosec // test-owned mirror dir
	if err != nil {
		t.Fatalf("read keeper.json: %v", err)
	}
	sweep.Write(sidecar)
	sweep.WriteByte('\n')
	entries, err := k.store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	raw, _ := json.Marshal(entries)
	sweep.Write(raw)
	sweep.WriteByte('\n')
	for _, leak := range []string{secret, "user", "token=abc123", "abc123", "frag"} {
		if strings.Contains(sweep.String(), leak) {
			t.Errorf("keeper state leaks %q", leak)
		}
	}
}

// TestRepoKeeper_NonHTTPRemoteHashesTrimmedAsIs pins the store's canonical
// boundary for non-URL remotes: scp-style and local-path sources hash
// trimmed as-is, and distinct sources never share a digest.
func TestRepoKeeper_NonHTTPRemoteHashesTrimmedAsIs(t *testing.T) {
	t.Parallel()

	ssh := "git@example.com:org/repo.git"
	padded := "  " + ssh + "\n"

	sshDigest, err := repokeeper.MirrorDigest(ssh, "host")
	if err != nil {
		t.Fatalf("MirrorDigest(ssh) error = %v", err)
	}
	paddedDigest, err := repokeeper.MirrorDigest(padded, "host")
	if err != nil {
		t.Fatalf("MirrorDigest(padded) error = %v", err)
	}
	if sshDigest != paddedDigest {
		t.Fatalf("whitespace-padded remote split digests: %q vs %q, want one digest", paddedDigest, sshDigest)
	}
	// An ssh-scheme source keeps its login user: a later fetch must
	// connect as the recorded user, not as the daemon's OS user.
	canonical, _, err := repokeeper.CanonicalRemote("ssh://git@example.com/org/repo.git")
	if err != nil {
		t.Fatalf("CanonicalRemote error = %v", err)
	}
	if canonical != "ssh://git@example.com/org/repo.git" {
		t.Errorf("canonical = %q, want the login-user-preserving form", canonical)
	}
}

// TestRepoKeeper_ScopeErrorDegradesToPlainClone drives the production Acquire
// entry point with a failing scope resolver and proves the caller degrades:
// ok=false and no mirror directory created.
func TestRepoKeeper_ScopeErrorDegradesToPlainClone(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	boom := errors.New("scope unavailable")
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, func(context.Context, string) (string, error) {
		return "", boom
	}, nil)
	if dirLease, ok := k.Acquire(context.Background(), remote); ok || dirLease != nil {
		t.Fatalf("scope-error Acquire = (%v, %v), want (nil, false)", dirLease, ok)
	}
	names, err := os.ReadDir(filepath.Join(k.store.StateDir(), "repo-keeper", "mirrors"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("scan mirrors: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("scope-error Acquire created %d mirror dirs, want none", len(names))
	}
}

// TestRepoKeeper_EmptyScopeDegradesToPlainClone drives the production Acquire
// entry point with an empty-scope resolver and proves the caller degrades:
// ok=false and no mirror directory created.
func TestRepoKeeper_EmptyScopeDegradesToPlainClone(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, func(context.Context, string) (string, error) {
		return "", nil
	}, nil)
	if dirLease, ok := k.Acquire(context.Background(), remote); ok || dirLease != nil {
		t.Fatalf("empty-scope Acquire = (%v, %v), want (nil, false)", dirLease, ok)
	}
	names, err := os.ReadDir(filepath.Join(k.store.StateDir(), "repo-keeper", "mirrors"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("scan mirrors: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("empty-scope Acquire created %d mirror dirs, want none", len(names))
	}
}

// TestRepoKeeper_AtExactlyBudgetRefusesNewMirror pins the at-cap boundary:
// recorded bytes of exactly MaxBytes refuse the next creation, so the caller
// degrades to a plain clone while existing mirrors keep serving.
func TestRepoKeeper_AtExactlyBudgetRefusesNewMirror(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	other := newKeeperSourceRepo(t)
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true, MaxBytes: 10}, nil, nil)
	dirLease, ok := k.Acquire(context.Background(), remote)
	if !ok {
		t.Fatal("first Acquire degraded")
	}
	dir := dirLease.Dir()
	dirLease.Release()
	k.RecordFetch(filepath.Base(dir), 10, time.Millisecond, false)

	if _, ok := k.Acquire(context.Background(), other); ok {
		t.Error("at-cap Acquire ok=true, want degradation to plain clone")
	}
	if _, ok := k.Acquire(context.Background(), remote); !ok {
		t.Error("existing mirror must still serve at exactly the budget")
	}
}

// TestServer_RepoKeeperEnabledRejectsOutOfRange drives the production
// POST /api/daemon/capacity route with repoKeeper.enabled=2 and proves the
// server refuses it with 400 instead of persisting it as enabled.
func TestServer_RepoKeeperEnabledRejectsOutOfRange(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	var resp afclient.SetCapacityResponse
	status := requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "repoKeeper.enabled",
		"value": "2",
	}, &resp)
	if status != http.StatusBadRequest {
		t.Fatalf("POST repoKeeper.enabled=2 -> %d, want 400", status)
	}
	if d.config.RepoKeeper.Enabled {
		t.Error("repoKeeper.enabled=2 persisted as enabled, want refusal")
	}
}

// TestRepoKeeper_RevokedMirrorDegradesToPlainClone drives the production
// Acquire entry point against a revoked catalog row and proves the caller
// degrades instead of serving the revoked mirror.
func TestRepoKeeper_RevokedMirrorDegradesToPlainClone(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, nil, nil)
	dirLease, ok := k.Acquire(context.Background(), remote)
	if !ok {
		t.Fatal("Acquire degraded")
	}
	dir := dirLease.Dir()
	digest := filepath.Base(dir)
	dirLease.Release()
	k.RecordFetch(digest, 42, time.Millisecond, true)

	if dir2Lease, ok := k.Acquire(context.Background(), remote); ok || dir2Lease != nil {
		t.Fatalf("revoked Acquire = (%v, %v), want (nil, false)", dir2Lease, ok)
	}
	stats := k.Stats()
	if stats.RevokedCount != 1 {
		t.Errorf("RevokedCount = %d, want 1", stats.RevokedCount)
	}
}

// TestRepoKeeper_UnreadableCatalogDegradesToPlainClone proves a corrupt
// catalog degrades every Acquire: revocation state is unknown, so no
// mirror is served until a fetch re-establishes state.
func TestRepoKeeper_UnreadableCatalogDegradesToPlainClone(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, nil, nil)
	dirLease, ok := k.Acquire(context.Background(), remote)
	if !ok {
		t.Fatal("Acquire degraded")
	}
	dir := dirLease.Dir()
	dirLease.Release()
	catalogPath := filepath.Join(k.store.StateDir(), "repo-keeper", "catalog.json")
	if err := os.WriteFile(catalogPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt catalog: %v", err)
	}
	if dir2Lease, ok := k.Acquire(context.Background(), remote); ok || dir2Lease != nil {
		t.Fatalf("corrupt-catalog Acquire = (%v, %v), want (nil, false)", dir2Lease, ok)
	}
	if err := k.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile(corrupt catalog) error = nil, want the decode failure")
	}
	if k.Ready() {
		t.Error("Ready() = true after a failed reconcile, want false")
	}
	_ = dir
}

// TestRepoKeeper_ForeignMirrorFailsClosed plants a foreign repository at a
// remote's digest and proves Acquire refuses it: the store's identity check
// fails closed instead of serving another remote's content.
func TestRepoKeeper_ForeignMirrorFailsClosed(t *testing.T) {
	t.Parallel()

	genuine := newKeeperSourceRepo(t)
	foreign := newKeeperSourceRepo(t)
	if err := os.WriteFile(filepath.Join(foreign, "foreign-marker"), []byte("foreign"), 0o600); err != nil {
		t.Fatalf("write foreign marker: %v", err)
	}
	runKeeperGitT(t, foreign, "add", "foreign-marker")
	runKeeperGitT(t, foreign, "commit", "-q", "-m", "foreign")

	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, nil, nil)
	// Publish the genuine mirror, then swap its content for the foreign
	// repository while keeping the genuine identity sidecar: the live
	// origin check must fail closed on the desync.
	genuineDirLease, ok := k.Acquire(context.Background(), genuine)
	genuineDir := ""
	if ok {
		genuineDir = genuineDirLease.Dir()
		genuineDirLease.Release()
	}
	if !ok {
		t.Fatal("genuine Acquire degraded")
	}
	foreignDirLease, ok := k.Acquire(context.Background(), foreign)
	foreignDir := ""
	if ok {
		foreignDir = foreignDirLease.Dir()
		foreignDirLease.Release()
	}
	if !ok {
		t.Fatal("foreign Acquire degraded")
	}
	_ = foreignDir
	runKeeperGitT(t, genuineDir, "remote", "set-url", "origin", foreign)

	if dir2Lease, ok := k.Acquire(context.Background(), genuine); ok || dir2Lease != nil {
		t.Fatalf("foreign-content Acquire = (%v, %v), want (nil, false)", dir2Lease, ok)
	}
}

// TestRepoKeeper_SymlinkMirrorFailsClosed plants a symlink at a remote's
// digest and proves Acquire refuses it: the store serves only real
// directories, so nothing is written through the link.
func TestRepoKeeper_SymlinkMirrorFailsClosed(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	outside := t.TempDir()
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, nil, nil)
	digest := keeperDigestFor(t, remote, "host")
	link := filepath.Join(k.store.StateDir(), "repo-keeper", "mirrors", digest)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("create mirrors dir: %v", err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if dir2Lease, ok := k.Acquire(context.Background(), remote); ok || dir2Lease != nil {
		t.Fatalf("symlink Acquire = (%v, %v), want (nil, false)", dir2Lease, ok)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("read outside dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("symlink target gained %d entries, want none", len(entries))
	}
}

// TestRepoKeeper_AuthResolverFailureDegrades proves a failing GitAuth
// resolver degrades a fresh clone to ok=false instead of failing loudly.
func TestRepoKeeper_AuthResolverFailureDegrades(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	boom := errors.New("auth unavailable")
	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, nil, func(context.Context, string) (string, bool, error) {
		return "", false, boom
	})
	if dirLease, ok := k.Acquire(context.Background(), remote); ok || dirLease != nil {
		t.Fatalf("auth-error Acquire = (%v, %v), want (nil, false)", dirLease, ok)
	}
}

// TestRepoKeeper_AuthHeaderReachesCloneProvenance proves the GitAuth seam
// threads into the clone: a resolver that mints a header for an http(s)
// remote still clones through the production path, and the published
// mirror's config carries no trace of the header.

// TestRepoKeeper_OpaqueCredentialRemoteDegradesToPlainClone drives the
// production Acquire entry point with an scp-like remote carrying
// password-shaped userinfo and proves the caller degrades: ok=false and
// no mirror directory created, so the token never reaches keeper state.
func TestRepoKeeper_OpaqueCredentialRemoteDegradesToPlainClone(t *testing.T) {
	t.Parallel()

	k := newKeeperForTest(t, RepoKeeperSettings{Enabled: true}, nil, nil)
	if dirLease, ok := k.Acquire(context.Background(), "deploy:s3cret-token@forge.example:org/repo.git"); ok || dirLease != nil {
		t.Fatalf("opaque-credential Acquire = (%v, %v), want (nil, false)", dirLease, ok)
	}
	names, err := os.ReadDir(filepath.Join(k.store.StateDir(), "repo-keeper", "mirrors"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("scan mirrors: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("opaque-credential Acquire created %d mirror dirs, want none", len(names))
	}
}

// TestRepoKeeper_RestartSweepsCrashStaging is the keeper-start crash
// acceptance case: a credentialed clone staging sibling left between
// clone and publish is swept when the next keeper starts over the same
// state dir, so no credential bytes remain on disk after the restart.
func TestRepoKeeper_RestartSweepsCrashStaging(t *testing.T) {
	t.Parallel()

	const secret = "restart-sweep-secret"
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	first, err := repokeeper.New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("repokeeper.New(first) error = %v", err)
	}
	keeper := NewRepoKeeper(first, RepoKeeperSettings{Enabled: true}, nil, nil)
	if err := keeper.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Simulate the kill between clone and publish: a credentialed
	// staging sibling under the mirrors directory.
	mirrors := filepath.Join(stateDir, "repo-keeper", "mirrors")
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		t.Fatalf("create mirrors dir: %v", err)
	}
	staging, err := os.MkdirTemp(mirrors, ".clone-*.tmp")
	if err != nil {
		t.Fatalf("plant crash staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "config"), []byte("[remote \"origin\"]\n\turl = https://user:"+secret+"@example.com/org/repo.git\n"), 0o600); err != nil {
		t.Fatalf("plant crash credential: %v", err)
	}

	// The restart builds a fresh store over the same state dir and
	// reconciles before serving: the sweep runs inside New.
	second, err := repokeeper.New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("repokeeper.New(second) error = %v", err)
	}
	restarted := NewRepoKeeper(second, RepoKeeperSettings{Enabled: true}, nil, nil)
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("crash staging survived the restart: err = %v", err)
	}
	var leaked []string
	_ = filepath.Walk(stateDir, func(path string, info os.FileInfo, err error) error { //nolint:gosec // G122: secret sweep over the test's own TempDir; no writes.
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: files enumerated from the test's own TempDir walk.
		if err == nil && strings.Contains(string(data), secret) {
			leaked = append(leaked, path)
		}
		return nil
	})
	if len(leaked) > 0 {
		t.Errorf("crash credential survived the restart: %v", leaked)
	}
	// The restarted keeper still serves: the sweep removed staging
	// only, never live state.
	if _, ok := restarted.Acquire(context.Background(), newKeeperSourceRepo(t)); !ok {
		t.Error("restarted keeper Acquire degraded, want a served mirror")
	}
}

// TestRepoKeeper_CombinedRootProbe is the cross-store regression test: the
// daemon keeper and a second store over the same state dir share one
// catalog format, so neither corrupts the other's state — a revocation
// recorded through one is visible through the other, and both address one
// remote at the same directory.
func TestRepoKeeper_CombinedRootProbe(t *testing.T) {
	t.Parallel()

	remote := newKeeperSourceRepo(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	first, err := repokeeper.New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("repokeeper.New(first) error = %v", err)
	}
	second, err := repokeeper.New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("repokeeper.New(second) error = %v", err)
	}
	keeper := NewRepoKeeper(first, RepoKeeperSettings{Enabled: true}, nil, nil)
	if err := keeper.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	dirLease, ok := keeper.Acquire(context.Background(), remote)
	if !ok {
		t.Fatal("keeper Acquire degraded")
	}
	dir := dirLease.Dir()
	digest := filepath.Base(dir)
	dirLease.Release()
	secondLease, err := second.Ensure(context.Background(), remote, "host")
	secondDir := ""
	if err == nil {
		secondDir = secondLease.Dir()
		secondLease.Release()
	}
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if secondDir != dir {
		t.Fatalf("two stores address one remote at %q vs %q, want one directory", secondDir, dir)
	}
	keeper.RecordFetch(digest, 42, time.Millisecond, true)
	rows, err := second.List()
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.Digest != digest {
			continue
		}
		found = true
		if !row.Revoked {
			t.Error("revocation recorded through the keeper is not visible through the second store")
		}
		if row.Bytes != 42 {
			t.Errorf("row bytes = %d, want 42", row.Bytes)
		}
	}
	if !found {
		t.Fatalf("second store has no row for digest %q", digest)
	}
	// Eviction through the keeper removes the directory the second store
	// serves: both agree on the layout, so no orphan survives.
	keeper.mu.Lock()
	keeper.settings.MaxBytes = 1
	keeper.mu.Unlock()
	keeper.EnforceBudget()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("evicted mirror still on disk: err = %v", err)
	}
	if _, err := os.Stat(secondDir); !os.IsNotExist(err) {
		t.Errorf("evicted mirror still served at the second store's path: err = %v", err)
	}
	// The store renames aside before deleting: a removal that drops the
	// row but skips the delete would leave a trash sibling behind.
	leftovers, err := os.ReadDir(filepath.Join(stateDir, "repo-keeper", "mirrors"))
	if err != nil {
		t.Fatalf("read mirrors dir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("eviction left %d entries behind the mirrors dir, want none", len(leftovers))
	}
	if rows, err := second.List(); err != nil {
		t.Fatalf("second List after evict: %v", err)
	} else {
		for _, row := range rows {
			if row.Digest == digest {
				t.Errorf("evicted digest %q still listed by the second store", digest)
			}
		}
	}
}
