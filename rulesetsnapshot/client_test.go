package rulesetsnapshot

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestServer(t *testing.T, body func() []byte) *httptest.Server {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClient_Unconfigured(t *testing.T) {
	t.Parallel()
	c, err := NewClient(Config{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Configured() {
		t.Fatal("Configured() = true for a zero-value Config, want false")
	}
	if err := c.Refresh(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Refresh on an unconfigured client = %v, want ErrNotConfigured", err)
	}
	if _, _, ok := c.Current(); ok {
		t.Fatal("Current() ok=true on an unconfigured, never-refreshed client")
	}
}

func TestNewClient_RejectsUnverifiableEndpoint(t *testing.T) {
	t.Parallel()
	_, err := NewClient(Config{Endpoint: "https://example.invalid/snapshot"})
	if err == nil {
		t.Fatal("NewClient accepted an Endpoint with no TrustedKeys and no JWKSURL")
	}
}

func TestNewClient_RejectsRefuseAfterBelowDegradedAfter(t *testing.T) {
	t.Parallel()
	pub, _ := mustGenerateKey(t)
	_, err := NewClient(Config{
		Endpoint:      "https://example.invalid/snapshot",
		TrustedKeys:   map[string]ed25519.PublicKey{"k": pub},
		DegradedAfter: time.Hour,
		RefuseAfter:   time.Minute,
	})
	if err == nil {
		t.Fatal("NewClient accepted RefuseAfter < DegradedAfter")
	}
}

func TestClient_Refresh_AdoptsValidSnapshot(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	srv := newTestServer(t, func() []byte {
		return buildSignedSnapshot(t, priv, signedSnapshotOpts{})
	})

	c, err := NewClient(Config{
		Endpoint:    srv.URL,
		TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:   filepath.Join(t.TempDir(), "snap.json"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, status, ok := c.Current()
	if !ok {
		t.Fatal("Current() ok=false after a successful Refresh")
	}
	if snap.RulesetRev != "org1@1" {
		t.Fatalf("RulesetRev = %q, want org1@1", snap.RulesetRev)
	}
	if status.Degraded {
		t.Fatal("a freshly compiled snapshot reported degraded=true")
	}
	if status.Age < 0 {
		t.Fatalf("Age = %v, want >= 0", status.Age)
	}
}

// TestClient_Refresh_RejectsTamperedSnapshotKeepsPrevious is the direct
// evidence for "reject on verify failure keeps the previous snapshot": a
// first good Refresh, then a second Refresh whose
// server response is tampered, must leave Current() unchanged from the
// first — and must record the rejection as a typed, retrievable error
// rather than silently doing nothing.
func TestClient_Refresh_RejectsTamperedSnapshotKeepsPrevious(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	var serveTampered atomic.Bool
	srv := newTestServer(t, func() []byte {
		if serveTampered.Load() {
			return buildSignedSnapshot(t, priv, signedSnapshotOpts{revision: 2, corruptSignature: true})
		}
		return buildSignedSnapshot(t, priv, signedSnapshotOpts{revision: 1})
	})

	c, err := NewClient(Config{
		Endpoint:    srv.URL,
		TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:   filepath.Join(t.TempDir(), "snap.json"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	snapBefore, _, _ := c.Current()

	serveTampered.Store(true)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("second Refresh accepted a tampered (bad-signature) snapshot")
	} else if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("second Refresh error = %v, want ErrVerificationFailed", err)
	}

	snapAfter, _, ok := c.Current()
	if !ok {
		t.Fatal("Current() ok=false after a rejected refresh — previous snapshot was dropped")
	}
	if snapAfter.Revision != snapBefore.Revision {
		t.Fatalf("Current() adopted the tampered snapshot: revision = %d, want %d (the pre-tamper revision)", snapAfter.Revision, snapBefore.Revision)
	}
	if c.LastError() == nil {
		t.Fatal("LastError() is nil after a rejected refresh — the rejection was silent")
	}
}

func TestClient_Refresh_TransportErrorKeepsPrevious(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	srv := newTestServer(t, func() []byte {
		return buildSignedSnapshot(t, priv, signedSnapshotOpts{})
	})

	c, err := NewClient(Config{
		Endpoint:    srv.URL,
		TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:   filepath.Join(t.TempDir(), "snap.json"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	srv.Close() // simulate the platform becoming unreachable

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh against a closed server returned nil error")
	}
	if _, _, ok := c.Current(); !ok {
		t.Fatal("Current() ok=false after a transport error — fail-static must keep serving the last-known-good snapshot")
	}
}

// TestClient_PersistenceSurvivesRestart is the red-first evidence for "on
// disk (survives daemon restart)": a second, independent Client instance —
// pointed at the SAME StatePath but with NO reachable server at all — must
// still answer Current() from the first Client's persisted snapshot.
func TestClient_PersistenceSurvivesRestart(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	srv := newTestServer(t, func() []byte {
		return buildSignedSnapshot(t, priv, signedSnapshotOpts{revision: 7})
	})
	statePath := filepath.Join(t.TempDir(), "snap.json")

	first, err := NewClient(Config{
		Endpoint:    srv.URL,
		TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:   statePath,
	})
	if err != nil {
		t.Fatalf("NewClient(first): %v", err)
	}
	if err := first.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(first): %v", err)
	}

	// A fresh Client — the "restarted daemon" — pointed at an UNREACHABLE
	// endpoint but the same on-disk StatePath. If persistence did not run,
	// this MUST fail to find a cached snapshot at all (proving the test can
	// actually go red): construct it with an endpoint that refuses
	// connections rather than calling Refresh, so only loadPersisted (run
	// inside NewClient) can possibly populate Current().
	second, err := NewClient(Config{
		Endpoint:    "http://127.0.0.1:1", // reserved/unroutable — never reachable
		TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:   statePath,
	})
	if err != nil {
		t.Fatalf("NewClient(second): %v", err)
	}
	snap, _, ok := second.Current()
	if !ok {
		t.Fatal("a fresh Client over the same StatePath found no persisted snapshot — restart survival is broken")
	}
	if snap.Revision != 7 {
		t.Fatalf("persisted snapshot revision = %d, want 7", snap.Revision)
	}
}

func TestClient_PersistedSnapshotIsReVerified(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	statePath := filepath.Join(t.TempDir(), "snap.json")

	// Write a tampered payload directly to the state path — as if the
	// on-disk cache had been altered while the daemon was stopped.
	tampered := buildSignedSnapshot(t, priv, signedSnapshotOpts{corruptSignature: true})
	c := &Client{cfg: Config{StatePath: statePath, TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub}}}
	if err := c.persist(tampered); err != nil {
		t.Fatalf("persist: %v", err)
	}

	loaded, err := NewClient(Config{
		StatePath:   statePath,
		TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, _, ok := loaded.Current(); ok {
		t.Fatal("NewClient adopted a tampered on-disk snapshot without re-verifying it")
	}
	if loaded.LastError() == nil {
		t.Fatal("LastError() is nil after loading a tampered on-disk snapshot")
	}
}

func TestClient_DegradedAndExpired(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	compiledAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := newTestServer(t, func() []byte {
		return buildSignedSnapshot(t, priv, signedSnapshotOpts{compiledAt: compiledAt})
	})

	now := compiledAt
	c, err := NewClient(Config{
		Endpoint:      srv.URL,
		TrustedKeys:   map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:     filepath.Join(t.TempDir(), "snap.json"),
		DegradedAfter: time.Minute,
		RefuseAfter:   5 * time.Minute,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	_, status, _ := c.Current()
	if status.Degraded {
		t.Fatal("Degraded=true immediately after compile, want false")
	}

	now = compiledAt.Add(2 * time.Minute) // past DegradedAfter, before RefuseAfter
	_, status, _ = c.Current()
	if !status.Degraded {
		t.Fatal("Degraded=false past DegradedAfter, want true")
	}
	if status.Age > c.RefuseAfter() {
		t.Fatal("test setup error: Age already exceeds RefuseAfter")
	}

	now = compiledAt.Add(10 * time.Minute) // past RefuseAfter
	_, status, ok := c.Current()
	if !ok {
		t.Fatal("Current() ok=false past RefuseAfter — Current() itself must keep serving; refusal is the CALLER's job (see daemon.FailStaticClaimGateProvider)")
	}
	if status.Age <= c.RefuseAfter() {
		t.Fatalf("Age = %v, want > RefuseAfter (%v)", status.Age, c.RefuseAfter())
	}
}

// TestClient_ForwardDiscontinuityKicksSingleRefresh proves a forward
// wall-clock jump greater than the refresh interval triggers exactly one
// background fetch at the next Current boundary, without waiting another
// full interval.
func TestClient_ForwardDiscontinuityKicksSingleRefresh(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	var hits atomic.Int64
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildSignedSnapshot(t, priv, signedSnapshotOpts{compiledAt: base}))
	}))
	t.Cleanup(srv.Close)

	now := base
	c, err := NewClient(Config{
		Endpoint:      srv.URL,
		TrustedKeys:   map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:     filepath.Join(t.TempDir(), "snap.json"),
		DegradedAfter: 5 * time.Minute,
		RefuseAfter:   30 * time.Minute,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("setup Refresh hits = %d, want 1", got)
	}
	// Wire the refresh interval the way Start would, without starting the
	// ticker's own async fetch.
	c.pollMu.Lock()
	c.pollInterval = time.Minute
	c.pollMu.Unlock()

	if _, _, ok := c.Current(); !ok {
		t.Fatal("Current() ok=false after Refresh")
	}
	if _, _, ok := c.Current(); !ok {
		t.Fatal("second Current() ok=false")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("poll without discontinuity fetched: hits = %d, want 1", got)
	}

	now = base.Add(5 * time.Minute) // forward jump greater than the interval
	if _, _, ok := c.Current(); !ok {
		t.Fatal("Current() ok=false after wall jump")
	}
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("forward wall jump did not kick a refresh: hits = %d, want 2", hits.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A further poll at the same jumped time must not start a second fetch.
	if _, _, ok := c.Current(); !ok {
		t.Fatal("Current() ok=false on repeat poll")
	}
	time.Sleep(200 * time.Millisecond)
	if got := hits.Load(); got != 2 {
		t.Fatalf("wall jump kicked %d fetches, want exactly 2 total (1 setup + 1 kick)", got)
	}
}

// TestClient_ConcurrentKicksCoalesceToOneFetch proves concurrent background
// kicks share a single in-flight fetch.
func TestClient_ConcurrentKicksCoalesceToOneFetch(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	var hits atomic.Int64
	release := make(chan struct{})
	var block atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if block.Load() {
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildSignedSnapshot(t, priv, signedSnapshotOpts{}))
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(Config{
		Endpoint:    srv.URL,
		TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:   filepath.Join(t.TempDir(), "snap.json"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("setup Refresh: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("setup hits = %d, want 1", got)
	}

	block.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.RequestRefresh()
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("concurrent kicks did not start a fetch: hits = %d, want 2", hits.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Further kicks while the first is still in flight must coalesce.
	for i := 0; i < 10; i++ {
		c.RequestRefresh()
	}
	time.Sleep(100 * time.Millisecond)
	if got := hits.Load(); got != 2 {
		t.Fatalf("concurrent kicks started %d fetches, want exactly 2 total (1 setup + 1 shared)", got)
	}
	close(release)
	deadline = time.Now().Add(5 * time.Second)
	for {
		c.refreshMu.Lock()
		inFlight := c.refreshInFlight
		c.refreshMu.Unlock()
		if !inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("kicked refresh did not finish after release")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestClient_BackgroundKickAdoptsVerifiedSnapshot proves a kicked refresh
// adopts through the usual verified path: after the background fetch
// succeeds, Current serves the newer snapshot.
func TestClient_BackgroundKickAdoptsVerifiedSnapshot(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	var hits atomic.Int64
	var revision atomic.Int64
	revision.Store(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildSignedSnapshot(t, priv, signedSnapshotOpts{revision: int(revision.Load())}))
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(Config{
		Endpoint:    srv.URL,
		TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:   filepath.Join(t.TempDir(), "snap.json"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("setup Refresh: %v", err)
	}
	snap, _, _ := c.Current()
	if snap.Revision != 1 {
		t.Fatalf("setup revision = %d, want 1", snap.Revision)
	}

	revision.Store(2)
	c.RequestRefresh()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, _, ok := c.Current()
		if ok && cur.Revision == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background kick did not adopt revision 2 (hits=%d)", hits.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Adoption lands in memory before the durable write completes: drain the
	// kicked refresh (same as the coalescing test above) so TempDir cleanup
	// cannot race its persist.
	deadline = time.Now().Add(5 * time.Second)
	for {
		c.refreshMu.Lock()
		inFlight := c.refreshInFlight
		c.refreshMu.Unlock()
		if !inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("kicked refresh did not finish after adopting")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestClient_PollObservationStoresWallOnlyTime pins the production clock-jump
// path: with Config.Now nil (the default every non-test caller uses),
// Current must record a wall-only lastWall with no monotonic reading, so the
// next poll compares a pure wall-clock delta against the real monotonic
// delta. Without the strip, both readings would carry monotonic components
// and their difference would stay near zero after a suspend or operator step.
func TestClient_PollObservationStoresWallOnlyTime(t *testing.T) {
	t.Parallel()
	pub, _ := mustGenerateKey(t)
	c, err := NewClient(Config{
		Endpoint:    "https://example.invalid/snapshot",
		TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:   filepath.Join(t.TempDir(), "snap.json"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.cfg.Now != nil {
		t.Fatal("test setup error: Config.Now must be nil to exercise the production path")
	}
	c.pollMu.Lock()
	c.pollInterval = time.Minute
	c.pollMu.Unlock()
	// Drive the production entry point, not the helper directly.
	_, _, _ = c.Current()
	c.pollMu.Lock()
	storedWall := c.lastWall
	storedMono := c.lastMono
	c.pollMu.Unlock()
	if storedWall.IsZero() {
		t.Fatal("observePoll did not record lastWall on the production path")
	}
	if strings.Contains(storedWall.String(), "m=") {
		t.Fatalf("lastWall carries a monotonic reading on the production path: %q — wall-clock jumps would be invisible", storedWall.String())
	}
	if !strings.Contains(storedMono.String(), "m=") {
		t.Fatalf("lastMono lost its monotonic reading: %q — discontinuity detection needs a real monotonic baseline", storedMono.String())
	}
}
