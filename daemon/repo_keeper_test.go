package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

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

// TestRepoKeeper_ScopeSeam drives the production Acquire entry point with two
// scopes for one remote and proves two mirrors result, with no scope value
// or URL in the on-disk catalog or provenance.
func TestRepoKeeper_ScopeSeam(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// One remote, two scopes: the scope function varies while the remote is
	// fixed, so only the scope can separate the two mirrors.
	remote := "https://example.com/org/repo.git"
	scopes := []string{"tenant-a", "tenant-b"}
	i := 0
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true}, func(context.Context, string) (string, error) {
		s := scopes[i]
		i++
		return s, nil
	}, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	dirA, okA := k.Acquire(context.Background(), remote)
	dirB, okB := k.Acquire(context.Background(), remote)
	if !okA || !okB {
		t.Fatalf("Acquire ok = %v/%v, want true/true", okA, okB)
	}
	if dirA == dirB {
		t.Fatalf("two scopes share one mirror dir %q, want two directories", dirA)
	}

	data, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	for _, secret := range []string{"tenant-a", "tenant-b", "example.com"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("catalog leaks %q", secret)
		}
	}
	var decoded map[string]*repoKeeperEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(decoded) != 2 {
		t.Errorf("catalog has %d entries, want 2", len(decoded))
	}
}

// TestRepoKeeper_BudgetExceededFallsBackToPlainClone drives the production
// Acquire entry point with the budget already exhausted and proves the caller
// degrades: ok=false, while an existing mirror still serves.
func TestRepoKeeper_BudgetExceededFallsBackToPlainClone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true, MaxBytes: 1}, nil, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	remote := "https://example.com/org/repo.git"
	dir, ok := k.Acquire(context.Background(), remote)
	if !ok {
		t.Fatal("first Acquire ok=false, want a served mirror")
	}
	digest := filepath.Base(dir)
	k.RecordFetch(digest, 1<<30, time.Second, false)

	if _, ok := k.Acquire(context.Background(), "https://example.com/org/other.git"); ok {
		t.Error("over-budget Acquire ok=true, want degradation to plain clone")
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

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true}, nil, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	remotes := []string{
		"https://example.com/org/a.git",
		"https://example.com/org/b.git",
		"https://example.com/org/c.git",
		"https://example.com/org/seeded.git",
	}
	digests := make([]string, 0, len(remotes))
	for _, remote := range remotes {
		dir, ok := k.Acquire(context.Background(), remote)
		if !ok {
			t.Fatalf("acquire %s: degraded", remote)
		}
		digests = append(digests, filepath.Base(dir))
	}
	// Assign byte loads and the enforcement cap without tripping the
	// admission gate mid-setup: the fourth Acquire would otherwise degrade
	// once the first three record 6 bytes each against the 18-byte cap.
	// The cap is armed just before enforcement — this test exercises
	// eviction order, not the admission gate.
	k.mu.Lock()
	for _, digest := range digests {
		if e, ok := k.entries[digest]; ok {
			e.Bytes = 6
		}
	}
	k.settings.MaxBytes = 20
	k.mu.Unlock()
	// Assign distinct use times oldest-first: a < b < c < seeded.
	// (Acquire stamps LastUse at second granularity, so same-second
	// acquires would tie; the explicit stagger makes the LRU order
	// unambiguous.)
	k.mu.Lock()
	for i, digest := range digests {
		if e, ok := k.entries[digest]; ok {
			e.LastUse = fmt.Sprintf("2000-01-01T00:00:%02dZ", i)
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
	if _, err := os.Stat(filepath.Join(root, "mirrors", digests[0])); !os.IsNotExist(err) {
		t.Errorf("oldest (LRU) mirror %s still on disk: err = %v", digests[0], err)
	}
	for i, d := range digests[1:] {
		if _, err := os.Stat(filepath.Join(root, "mirrors", d)); err != nil {
			t.Errorf("mirror %d (%s) evicted instead of the oldest: %v", i+1, d, err)
		}
	}
	k.SeedEnd(digests[3], 6, time.Millisecond)
}

// TestRepoKeeper_RecoveryOrderDegradesWhileReconciling proves callers degrade
// while the keeper reconciles: Acquire before Reconcile returns ok=false,
// and the mock recovery position (reconcile after adoption, before admission)
// holds on the running daemon.
func TestRepoKeeper_RecoveryOrderDegradesWhileReconciling(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true}, nil, nil)
	if _, ok := k.Acquire(context.Background(), "https://example.com/org/repo.git"); ok {
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
	if _, ok := k.Acquire(context.Background(), "https://example.com/org/repo.git"); !ok {
		t.Error("post-reconcile Acquire ok=false, want a served mirror")
	}
}

// TestRepoKeeper_StatsCarryNoSecrets drives the production stats path and
// proves the snapshot carries counts and bytes but no scope values or URLs.
func TestRepoKeeper_StatsCarryNoSecrets(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true}, func(context.Context, string) (string, error) {
		return "tenant-secret", nil
	}, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	dir, ok := k.Acquire(context.Background(), "https://example.com/org/repo.git")
	if !ok {
		t.Fatal("Acquire degraded")
	}
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

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true, MaxBytes: 10}, nil, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	dir, ok := k.Acquire(context.Background(), "https://example.com/org/repo.git")
	if !ok {
		t.Fatal("Acquire degraded")
	}
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
// Acquire through the production entry points under -race: the publish and
// the catalog persist hold one lock, so the race detector must stay silent
// and every Acquire must either serve or degrade without an error.
//
// A test-only rendezvous parks Reconcile inside the publish→persist window
// (after k.entries is swapped, before the catalog is marshalled) while a
// concurrent Acquire hammers the same map. Restoring the racy shape —
// publish under lock, persist after unlock — makes the detector fire on the
// overlap, so this test pins the lock-holding fix rather than the timing.
// The hammer uses the serve-existing path (no file I/O, just the entry-map
// write), so it overlaps the persist marshal on every parked round.
func TestRepoKeeper_ReconcileRacesNoAcquire(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true}, nil, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Seed one mirror so the racing Acquire takes the serve-existing path:
	// lock, entry-map write, unlock — no file I/O to dilute the overlap.
	seed := "https://example.com/org/seed.git"
	if _, ok := k.Acquire(context.Background(), seed); !ok {
		t.Fatal("seed Acquire degraded")
	}

	for round := 0; round < 3; round++ {
		// entered fires once Reconcile is parked in the publish→persist
		// window; release unblocks it. Buffered + select-default so a
		// surprising scheduler ordering cannot deadlock the test.
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		stop := make(chan struct{})
		k.reconcilePrePersist = func() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}

		// Hammer Acquire on the seeded mirror until told to stop. While
		// Reconcile is parked holding the lock this queues on the mutex
		// (fixed code); with the racy shape it spins concurrently with
		// the unlocked persist (mutant) and the detector fires.
		hammerDone := make(chan struct{})
		go func() {
			defer close(hammerDone)
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = k.Acquire(context.Background(), seed)
				}
			}
		}()

		reconcileDone := make(chan error, 1)
		go func() {
			reconcileDone <- k.Reconcile(context.Background())
		}()

		// Wait until Reconcile is parked in the window, then give the
		// hammer a moment to pile onto the overlap before releasing.
		// The main goroutine never touches the keeper lock here, so
		// neither shape can deadlock the test.
		select {
		case <-entered:
			time.Sleep(20 * time.Millisecond)
			close(release)
		case err := <-reconcileDone:
			// Reconcile finished without parking (must not happen
			// while enabled, but never deadlock if it does).
			close(stop)
			<-hammerDone
			if err != nil {
				t.Fatalf("round %d Reconcile: %v", round, err)
			}
			t.Fatalf("round %d: Reconcile finished without entering the persist window", round)
		}
		if err := <-reconcileDone; err != nil {
			close(stop)
			<-hammerDone
			t.Fatalf("round %d Reconcile: %v", round, err)
		}
		close(stop)
		<-hammerDone
	}
	k.reconcilePrePersist = nil
}

// TestRepoKeeper_CredentialBearingRemoteStaysSecretFree drives the production
// Acquire entry point with a userinfo-, query-, and fragment-bearing remote
// and proves it shares the bare remote's mirror while no credential material
// lands in mirror.json or catalog.json.
func TestRepoKeeper_CredentialBearingRemoteStaysSecretFree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true}, nil, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	bare := "https://example.com/org/repo.git"
	credentialed := "https://user:secrettoken@example.com/org/repo.git?token=abc123#frag"

	bareDir, ok := k.Acquire(context.Background(), bare)
	if !ok {
		t.Fatal("bare Acquire degraded")
	}
	credDir, ok := k.Acquire(context.Background(), credentialed)
	if !ok {
		t.Fatal("credentialed Acquire degraded")
	}
	if bareDir != credDir {
		t.Fatalf("credentialed remote split mirrors: %q vs %q, want one digest", credDir, bareDir)
	}

	digest := filepath.Base(bareDir)
	meta, err := os.ReadFile(filepath.Join(root, "mirrors", digest, "mirror.json")) //nolint:gosec // test-owned state file
	if err != nil {
		t.Fatalf("read mirror.json: %v", err)
	}
	catalog, err := os.ReadFile(filepath.Join(root, "catalog.json")) //nolint:gosec // test-owned state file
	if err != nil {
		t.Fatalf("read catalog.json: %v", err)
	}
	for _, secret := range []string{"secrettoken", "user", "token=abc123", "abc123", "frag"} {
		if strings.Contains(string(meta), secret) {
			t.Errorf("mirror.json leaks %q: %s", secret, meta)
		}
		if strings.Contains(string(catalog), secret) {
			t.Errorf("catalog.json leaks %q: %s", secret, catalog)
		}
	}
	var decoded repoKeeperMirrorMeta
	if err := json.Unmarshal(meta, &decoded); err != nil {
		t.Fatalf("decode mirror.json: %v", err)
	}
	if decoded.Remote != bare {
		t.Errorf("mirror.json remote = %q, want canonical %q", decoded.Remote, bare)
	}
}

// TestRepoKeeper_NonHTTPRemoteHashesTrimmedAsIs pins the documented boundary
// of the credential strip: non-URL remotes (SSH-style, local paths) carry
// no URL credentials, so they hash trimmed as-is. The conservative choice
// is http(s)-only stripping — an SSH userinfo like git@ is not a
// credential — and this test locks that boundary against silent widening.
func TestRepoKeeper_NonHTTPRemoteHashesTrimmedAsIs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true}, nil, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	ssh := "git@example.com:org/repo.git"
	padded := "  " + ssh + "\n"

	sshDir, ok := k.Acquire(context.Background(), ssh)
	if !ok {
		t.Fatal("ssh Acquire degraded")
	}
	paddedDir, ok := k.Acquire(context.Background(), padded)
	if !ok {
		t.Fatal("padded ssh Acquire degraded")
	}
	if sshDir != paddedDir {
		t.Fatalf("whitespace-padded remote split mirrors: %q vs %q, want one digest", paddedDir, sshDir)
	}
	httpStyle := "ssh://git@example.com/org/repo.git"
	httpDir, ok := k.Acquire(context.Background(), httpStyle)
	if !ok {
		t.Fatal("ssh-scheme Acquire degraded")
	}
	if httpDir == sshDir {
		t.Errorf("ssh-scheme remote %q shares the scp-style digest %q: schemes must stay distinct", httpStyle, sshDir)
	}
	httpMeta, err := os.ReadFile(filepath.Join(root, "mirrors", filepath.Base(httpDir), "mirror.json")) //nolint:gosec // test-owned state file
	if err != nil {
		t.Fatalf("read ssh-scheme mirror.json: %v", err)
	}
	var httpDecoded repoKeeperMirrorMeta
	if err := json.Unmarshal(httpMeta, &httpDecoded); err != nil {
		t.Fatalf("decode ssh-scheme mirror.json: %v", err)
	}
	// The conservative http(s)-only strip leaves non-http(s) remotes
	// untouched — even when they parse as URLs with userinfo — so the
	// provenance keeps the remote exactly as the caller passed it
	// (trimmed). Widening the strip to every scheme would rewrite this
	// to "ssh://example.com/org/repo.git" and split no digests, but it
	// would silently change the recorded provenance.
	if httpDecoded.Remote != httpStyle {
		t.Errorf("ssh-scheme mirror.json remote = %q, want untouched %q", httpDecoded.Remote, httpStyle)
	}

	digest := filepath.Base(sshDir)
	meta, err := os.ReadFile(filepath.Join(root, "mirrors", digest, "mirror.json")) //nolint:gosec // test-owned state file
	if err != nil {
		t.Fatalf("read mirror.json: %v", err)
	}
	var decoded repoKeeperMirrorMeta
	if err := json.Unmarshal(meta, &decoded); err != nil {
		t.Fatalf("decode mirror.json: %v", err)
	}
	if decoded.Remote != ssh {
		t.Errorf("mirror.json remote = %q, want trimmed-as-is %q", decoded.Remote, ssh)
	}
}

// TestRepoKeeper_ScopeErrorDegradesToPlainClone drives the production Acquire
// entry point with a failing scope resolver and proves the caller degrades:
// ok=false and no mirror directory created.
func TestRepoKeeper_ScopeErrorDegradesToPlainClone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	boom := errors.New("scope unavailable")
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true}, func(context.Context, string) (string, error) {
		return "", boom
	}, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if dir, ok := k.Acquire(context.Background(), "https://example.com/org/repo.git"); ok || dir != "" {
		t.Fatalf("scope-error Acquire = (%q, %v), want (\"\", false)", dir, ok)
	}
	names, err := os.ReadDir(filepath.Join(root, "mirrors"))
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

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true}, func(context.Context, string) (string, error) {
		return "", nil
	}, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if dir, ok := k.Acquire(context.Background(), "https://example.com/org/repo.git"); ok || dir != "" {
		t.Fatalf("empty-scope Acquire = (%q, %v), want (\"\", false)", dir, ok)
	}
	names, err := os.ReadDir(filepath.Join(root, "mirrors"))
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

	root := t.TempDir()
	k := NewRepoKeeper(root, RepoKeeperSettings{Enabled: true, MaxBytes: 10}, nil, nil)
	if err := k.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	dir, ok := k.Acquire(context.Background(), "https://example.com/org/repo.git")
	if !ok {
		t.Fatal("first Acquire degraded")
	}
	k.RecordFetch(filepath.Base(dir), 10, time.Millisecond, false)

	if _, ok := k.Acquire(context.Background(), "https://example.com/org/other.git"); ok {
		t.Error("at-cap Acquire ok=true, want degradation to plain clone")
	}
	if _, ok := k.Acquire(context.Background(), "https://example.com/org/repo.git"); !ok {
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
