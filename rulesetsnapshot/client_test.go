package rulesetsnapshot

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
