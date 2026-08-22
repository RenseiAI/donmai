package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/sessionshim"
)

type restartExactStoreFunc func(context.Context, sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error)

func (f restartExactStoreFunc) AcknowledgeExact(ctx context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
	return f(ctx, request)
}

type echoLegacyRestartFenceStore struct{}

func (echoLegacyRestartFenceStore) Acknowledge(_ context.Context, fence sessionshim.Fence) (sessionshim.Fence, error) {
	return fence, nil
}

type normalizingLegacyRestartFenceStore struct{ now time.Time }

func (s normalizingLegacyRestartFenceStore) Acknowledge(_ context.Context, fence sessionshim.Fence) (sessionshim.Fence, error) {
	fence.Sessions = append([]sessionshim.FencedSession(nil), fence.Sessions...)
	fence.HostID = "normalized-host"
	fence.IssuedAtUnixNano = s.now.Add(time.Second).UnixNano()
	fence.HoldUntilUnixNano = s.now.Add(10 * time.Minute).UnixNano()
	for left, right := 0, len(fence.Sessions)-1; left < right; left, right = left+1, right-1 {
		fence.Sessions[left], fence.Sessions[right] = fence.Sessions[right], fence.Sessions[left]
	}
	return fence, nil
}

func newRestartTestDaemon(t *testing.T, store sessionshim.ExactFenceStore, ids ...sessionshim.Identity) *Daemon {
	t.Helper()
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			RegistryDir:     t.TempDir(),
			ExactFenceStore: store,
			HostIDForOrg: func(_ context.Context, orgID string) (string, error) {
				return "host-" + orgID, nil
			},
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	d.shims.restartStateWriter = func(restartPreparationAudit) error { return nil }
	d.shims.restartID = func() (string, error) { return "rp_test", nil }
	seedShimState(d, ids, nil)
	return d
}

func TestServerRestartPrepareReturnsClosedPermissionResponse(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	t.Cleanup(cleanup)

	res, err := http.Post("http://"+srv.Addr()+"/api/daemon/restart/prepare", "application/json", nil) //nolint:gosec
	if err != nil {
		t.Fatalf("POST restart prepare: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST restart prepare status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Protocol      string `json:"protocol"`
		State         string `json:"state"`
		PreparationID string `json:"preparationId"`
		ScopeCount    int    `json:"scopeCount"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode restart prepare: %v", err)
	}
	if body.Protocol != "session-shim-restart-preflight-v1" || body.State != "not_required" || body.PreparationID == "" || body.ScopeCount != 0 {
		t.Fatalf("restart prepare response = %+v, want closed not_required permission", body)
	}
}

func TestServerRestartPrepareRejectsCallerFenceIDBody(t *testing.T) {
	d := newRestartTestDaemon(t, nil)
	server := httptest.NewServer(NewServer(d).httpd.Handler)
	t.Cleanup(server.Close)
	res, err := http.Post(server.URL+"/api/daemon/restart/prepare", "application/json", strings.NewReader(`{"fenceId":"caller-owned"}`))
	if err != nil {
		t.Fatalf("POST restart prepare: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("caller fence body = %d %s, want 400", res.StatusCode, body)
	}
	if d.shims.restart != nil || d.State() != StateRunning {
		t.Fatalf("caller body reached preflight: restart=%+v state=%q", d.shims.restart, d.State())
	}
}

func TestPublicDaemonClientReceivesPreparedMultiScopePermission(t *testing.T) {
	store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		return sessionshim.FenceAcknowledgement{
			RequestBytes:    append([]byte(nil), request.RequestBytes...),
			DurableRevision: "revision-" + request.Fence.Sessions[0].OrgID,
		}, nil
	})
	d := newRestartTestDaemon(t, store,
		sessionshim.Identity{OrgID: "org-alpha", SessionID: "alpha"},
		sessionshim.Identity{OrgID: "org-beta", SessionID: "beta"},
	)
	server := httptest.NewServer(NewServer(d).httpd.Handler)
	t.Cleanup(server.Close)
	result, err := afclient.NewDaemonClientFromURL(server.URL).PrepareRestart()
	if err != nil {
		t.Fatalf("PrepareRestart client: %v", err)
	}
	if result.State != afclient.DaemonRestartPrepared || result.ScopeCount != 2 || result.PreparationID != "rp_test" {
		t.Fatalf("PrepareRestart client response = %+v", result)
	}
}

func TestRestartPreflightPartialRetryFreezesIdentitySnapshotAndBytes(t *testing.T) {
	store := &retryExactFenceStore{}
	d := newRestartTestDaemon(t, store,
		sessionshim.Identity{OrgID: "org-alpha", SessionID: "session-alpha"},
		sessionshim.Identity{OrgID: "org-beta", SessionID: "session-beta"},
	)

	first, err := d.PrepareRestart(context.Background())
	if !errors.Is(err, ErrRestartPreflightRefused) {
		t.Fatalf("first PrepareRestart = %+v, %v; want typed refusal", first, err)
	}
	if d.State() != StateDraining || d.spawner.IsAccepting() {
		t.Fatalf("first refusal state/admission = (%q,%v), want draining/false", d.State(), d.spawner.IsAccepting())
	}

	// Mutate the live registry projection after the partial acknowledgement. A
	// retry must not resample it or add a third scope under the same identity.
	d.shims.mu.Lock()
	d.shims.quarantined = append(d.shims.quarantined, sessionshim.QuarantinedSession{
		OrgID: "org-gamma", SessionID: "late", ShimID: "shim-late", ProcessEpoch: 9,
		Reason: sessionshim.QuarantineDuplicateIdentity, ConsumesCapacity: true,
	})
	d.shims.mu.Unlock()

	second, err := d.PrepareRestart(context.Background())
	if err != nil {
		t.Fatalf("retry PrepareRestart: %v", err)
	}
	if second.State != afclient.DaemonRestartPrepared || second.PreparationID != "rp_test" || second.ScopeCount != 2 {
		t.Fatalf("retry response = %+v, want same two-scope preparation", second)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := len(store.requests["org-alpha"]); got != 1 {
		t.Fatalf("acknowledged alpha calls = %d, want one", got)
	}
	beta := store.requests["org-beta"]
	if len(beta) != 2 || !bytes.Equal(beta[0], beta[1]) {
		t.Fatalf("beta retry bytes changed: %q then %q", beta[0], beta[1])
	}
	if bytes.Contains(beta[1], []byte("org-gamma")) {
		t.Fatalf("retry resampled late registry mutation: %s", beta[1])
	}
}

func TestCachedPreparedPermissionRefusesExpiredFenceWithoutRemintOrResnapshot(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	now := base
	var storeCalls atomic.Int32
	store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		storeCalls.Add(1)
		return sessionshim.FenceAcknowledgement{
			RequestBytes: append([]byte(nil), request.RequestBytes...), DurableRevision: "revision",
		}, nil
	})
	d := newRestartTestDaemon(t, store, sessionshim.Identity{OrgID: "org", SessionID: "session"})
	d.shims.restartNow = func() time.Time { return now }

	first, err := d.PrepareRestart(context.Background())
	if err != nil || first.State != afclient.DaemonRestartPrepared {
		t.Fatalf("initial PrepareRestart = %+v, %v", first, err)
	}
	preparation := d.shims.restart
	holdUntil := preparation.acked["org"].HoldUntilUnixNano
	now = time.Unix(0, holdUntil+1)

	second, err := d.PrepareRestart(context.Background())
	if !errors.Is(err, ErrRestartPreflightRefused) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired cached PrepareRestart = %+v, %v; want refusal", second, err)
	}
	if d.shims.restart != preparation || d.shims.restart.id != first.PreparationID || storeCalls.Load() != 1 {
		t.Fatalf("expired replay reminted/resnapshotted: prep=%p/%p id=%q/%q calls=%d",
			d.shims.restart, preparation, d.shims.restart.id, first.PreparationID, storeCalls.Load())
	}
	if d.State() != StateDraining || d.spawner.IsAccepting() {
		t.Fatalf("expired replay state/admission = (%q,%v), want draining/false", d.State(), d.spawner.IsAccepting())
	}
}

func TestInitialPreparedPermissionRevalidatesAfterPersistence(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	now := base
	store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		now = base.Add(24 * time.Hour)
		return sessionshim.FenceAcknowledgement{
			RequestBytes: append([]byte(nil), request.RequestBytes...), DurableRevision: "revision",
		}, nil
	})
	d := newRestartTestDaemon(t, store, sessionshim.Identity{OrgID: "org", SessionID: "session"})
	d.shims.restartNow = func() time.Time { return now }
	var persisted []restartPreparationState
	d.shims.restartStateWriter = func(record restartPreparationAudit) error {
		persisted = append(persisted, record.State)
		return nil
	}

	result, err := d.PrepareRestart(context.Background())
	if !errors.Is(err, ErrRestartPreflightRefused) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("slow initial PrepareRestart = %+v, %v; want post-persist expiry refusal", result, err)
	}
	if len(persisted) < 2 || persisted[len(persisted)-1] != restartPreparationPrepared {
		t.Fatalf("audit states = %v, want final prepared persistence before validation", persisted)
	}
	if d.shims.restart.state == restartPreparationPrepared {
		t.Fatal("expired initial acknowledgement published in-memory permission")
	}
}

func TestCachedPreparedPermissionRefusesMissingOrChangedLocalFence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Daemon)
	}{
		{"missing", func(d *Daemon) { delete(d.shims.fences, "org") }},
		{"revision empty", func(d *Daemon) {
			fence := d.shims.fences["org"]
			fence.DurableRevision = ""
			d.shims.fences["org"] = fence
		}},
		{"state changed", func(d *Daemon) {
			fence := d.shims.fences["org"]
			fence.State = sessionshim.FenceReconciliationRequired
			d.shims.fences["org"] = fence
		}},
		{"coverage changed", func(d *Daemon) {
			fence := d.shims.fences["org"]
			fence.Sessions = append(fence.Sessions, sessionshim.FencedSession{OrgID: "org", SessionID: "late"})
			d.shims.fences["org"] = fence
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
				return sessionshim.FenceAcknowledgement{
					RequestBytes: append([]byte(nil), request.RequestBytes...), DurableRevision: "revision",
				}, nil
			})
			d := newRestartTestDaemon(t, store, sessionshim.Identity{OrgID: "org", SessionID: "session"})
			first, err := d.PrepareRestart(context.Background())
			if err != nil {
				t.Fatalf("initial PrepareRestart: %v", err)
			}
			d.shims.mu.Lock()
			tc.mutate(d)
			d.shims.mu.Unlock()
			second, err := d.PrepareRestart(context.Background())
			if !errors.Is(err, ErrRestartPreflightRefused) {
				t.Fatalf("changed cached PrepareRestart = %+v, %v; want refusal", second, err)
			}
			if d.shims.restart.id != first.PreparationID {
				t.Fatalf("changed cached permission reminted %q -> %q", first.PreparationID, d.shims.restart.id)
			}
		})
	}
}

func TestLegacyFenceStoreEmptyRevisionRemainsReplayCompatible(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			RegistryDir: t.TempDir(), FenceStore: echoLegacyRestartFenceStore{},
			HostIDForOrg: func(_ context.Context, orgID string) (string, error) { return "host-" + orgID, nil },
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	d.shims.restartNow = func() time.Time { return base }
	d.shims.restartID = func() (string, error) { return "rp_legacy", nil }
	d.shims.restartStateWriter = func(restartPreparationAudit) error { return nil }
	seedShimState(d, []sessionshim.Identity{{OrgID: "org", SessionID: "session"}}, nil)

	first, err := d.PrepareRestart(context.Background())
	if err != nil || first.State != afclient.DaemonRestartPrepared {
		t.Fatalf("legacy initial PrepareRestart = %+v, %v", first, err)
	}
	if revision := d.shims.restart.acked["org"].DurableRevision; revision != "" {
		t.Fatalf("legacy callback revision = %q, want source-compatible empty", revision)
	}
	second, err := d.PrepareRestart(context.Background())
	if err != nil || second != first {
		t.Fatalf("legacy replay = %+v, %v; want %+v", second, err, first)
	}
}

func TestLegacyNormalizingFenceStoreReplayRemainsCompatible(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			RegistryDir: t.TempDir(), FenceStore: normalizingLegacyRestartFenceStore{now: base},
			HostIDForOrg: func(_ context.Context, orgID string) (string, error) { return "host-" + orgID, nil },
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	d.shims.restartNow = func() time.Time { return base }
	d.shims.restartID = func() (string, error) { return "rp_normalized", nil }
	d.shims.restartStateWriter = func(restartPreparationAudit) error { return nil }
	seedShimState(d, []sessionshim.Identity{
		{OrgID: "org", SessionID: "alpha"}, {OrgID: "org", SessionID: "beta"},
	}, nil)

	first, err := d.PrepareRestart(context.Background())
	if err != nil {
		t.Fatalf("normalizing legacy initial: %v", err)
	}
	ack := d.shims.restart.acked["org"]
	if ack.HostID != "normalized-host" || ack.IssuedAtUnixNano == d.shims.restart.requests["org"].Fence.IssuedAtUnixNano || ack.DurableRevision != "" {
		t.Fatalf("legacy normalization was not retained: %+v", ack)
	}
	second, err := d.PrepareRestart(context.Background())
	if err != nil || second != first {
		t.Fatalf("normalizing legacy replay = %+v, %v; want %+v", second, err, first)
	}
}

func TestStandalonePreparedReplayAcceptsHeldIntentAndRefusesExpiry(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	now := base
	d := newRestartTestDaemon(t, nil, sessionshim.Identity{OrgID: "local", SessionID: "session"})
	d.shims.restartNow = func() time.Time { return now }

	first, err := d.PrepareRestart(context.Background())
	if err != nil || first.State != afclient.DaemonRestartPrepared {
		t.Fatalf("standalone initial PrepareRestart = %+v, %v", first, err)
	}
	preparation := d.shims.restart
	if preparation.authorityMode != restartAuthorityStandalone || preparation.acked["local"].DurableRevision != "" {
		t.Fatalf("standalone authority/revision = %v/%q, want standalone/empty",
			preparation.authorityMode, preparation.acked["local"].DurableRevision)
	}
	second, err := d.PrepareRestart(context.Background())
	if err != nil || second != first {
		t.Fatalf("held standalone replay = %+v, %v; want %+v", second, err, first)
	}
	now = time.Unix(0, preparation.acked["local"].HoldUntilUnixNano+1)
	third, err := d.PrepareRestart(context.Background())
	if !errors.Is(err, ErrRestartPreflightRefused) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired standalone replay = %+v, %v; want refusal", third, err)
	}
	if d.shims.restart != preparation || d.shims.restart.id != first.PreparationID {
		t.Fatal("expired standalone replay resnapshotted or reminted")
	}
}

func TestCachedNotRequiredPermissionRevalidatesEmptyInvariant(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Daemon)
	}{
		{
			name: "direct session appears",
			mutate: func(_ *testing.T, d *Daemon) {
				d.spawner.mu.Lock()
				d.spawner.sessions["late-direct"] = &spawnedSession{
					handle: SessionHandle{SessionID: "late-direct"}, spec: SessionSpec{SessionID: "late-direct"},
				}
				d.spawner.mu.Unlock()
			},
		},
		{
			name: "shim correlation appears",
			mutate: func(_ *testing.T, d *Daemon) {
				d.shims.mu.Lock()
				d.shims.quarantined = append(d.shims.quarantined, sessionshim.QuarantinedSession{
					OrgID: "org", SessionID: "late-shim", ShimID: "shim-late", ProcessEpoch: 2,
					Reason: sessionshim.QuarantineDuplicateIdentity, ConsumesCapacity: true,
				})
				d.shims.mu.Unlock()
			},
		},
		{
			name: "unclassified registry record appears",
			mutate: func(t *testing.T, d *Daemon) {
				t.Helper()
				path := filepath.Join(d.sessionShimConfig().RegistryDir, "late.json")
				if err := os.WriteFile(path, []byte(`{}`), sessionshim.RecordFileMode); err != nil {
					t.Fatalf("write late registry record: %v", err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newRestartTestDaemon(t, nil)
			first, err := d.PrepareRestart(context.Background())
			if err != nil || first.State != afclient.DaemonRestartNotRequired {
				t.Fatalf("initial not-required = %+v, %v", first, err)
			}
			preparation := d.shims.restart
			tc.mutate(t, d)
			second, err := d.PrepareRestart(context.Background())
			if !errors.Is(err, ErrRestartPreflightRefused) {
				t.Fatalf("changed not-required = %+v, %v; want refusal", second, err)
			}
			if d.shims.restart != preparation || d.shims.restart.id != first.PreparationID {
				t.Fatalf("not-required replay reminted/resnapshotted: %+v", d.shims.restart)
			}
			if d.State() != StateDraining || d.spawner.IsAccepting() {
				t.Fatalf("not-required refusal state/admission = (%q,%v)", d.State(), d.spawner.IsAccepting())
			}
		})
	}
}

func TestRestartPreflightRefusesMissingOrChangedExactAcknowledgement(t *testing.T) {
	tests := []struct {
		name string
		ack  func(sessionshim.FenceRequest) sessionshim.FenceAcknowledgement
	}{
		{
			name: "missing revision",
			ack: func(request sessionshim.FenceRequest) sessionshim.FenceAcknowledgement {
				return sessionshim.FenceAcknowledgement{RequestBytes: request.RequestBytes}
			},
		},
		{
			name: "changed bytes",
			ack: func(request sessionshim.FenceRequest) sessionshim.FenceAcknowledgement {
				return sessionshim.FenceAcknowledgement{RequestBytes: append(append([]byte(nil), request.RequestBytes...), '\n'), DurableRevision: "revision"}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newRestartTestDaemon(t, restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
				return tc.ack(request), nil
			}), sessionshim.Identity{OrgID: "org", SessionID: "session"})
			got, err := d.PrepareRestart(context.Background())
			if !errors.Is(err, ErrRestartPreflightRefused) || got.State != "" {
				t.Fatalf("PrepareRestart = %+v, %v; want closed refusal", got, err)
			}
			if d.State() != StateDraining || d.spawner.IsAccepting() {
				t.Fatalf("refusal reopened daemon: state=%q accepting=%v", d.State(), d.spawner.IsAccepting())
			}
		})
	}
}

func TestRestartPreflightRefusesUncoveredDirectOwnedSession(t *testing.T) {
	d := newRestartTestDaemon(t, nil)
	d.spawner.mu.Lock()
	d.spawner.sessions["direct"] = &spawnedSession{
		handle: SessionHandle{SessionID: "direct"}, spec: SessionSpec{SessionID: "direct"},
	}
	d.spawner.mu.Unlock()

	got, err := d.PrepareRestart(context.Background())
	if !errors.Is(err, ErrRestartPreflightRefused) || !strings.Contains(err.Error(), "direct-owned") {
		t.Fatalf("PrepareRestart = %+v, %v; want uncovered-direct refusal", got, err)
	}
	if d.shims.restart != nil {
		t.Fatal("direct-owned refusal minted a stop authorization")
	}
	if d.State() != StateDraining || d.spawner.IsAccepting() {
		t.Fatalf("direct-owned refusal state/admission = (%q,%v)", d.State(), d.spawner.IsAccepting())
	}
}

func TestRestartPreflightAuditIsSecretFreeDurableAndExcludedFromRegistryScan(t *testing.T) {
	dir := t.TempDir()
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{RegistryDir: dir}})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.config = &Config{Orchestrator: OrchestratorConfig{AuthToken: "fixture-secret-never-persist"}}
	d.setState(StateRunning)
	d.shims.restartID = func() (string, error) { return "rp_audit", nil }

	result, err := d.PrepareRestart(context.Background())
	if err != nil || result.State != afclient.DaemonRestartNotRequired {
		t.Fatalf("PrepareRestart = %+v, %v", result, err)
	}
	info, err := os.Stat(filepath.Join(dir, restartPreparationStateName))
	if err != nil {
		t.Fatalf("stat audit: %v", err)
	}
	if info.Mode().Perm() != sessionshim.RecordFileMode {
		t.Fatalf("audit mode = %#o, want %#o", info.Mode().Perm(), sessionshim.RecordFileMode)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat audit directory: %v", err)
	}
	if dirInfo.Mode().Perm() != sessionshim.RegistryDirMode {
		t.Fatalf("audit directory = %#o, want %#o", dirInfo.Mode().Perm(), sessionshim.RegistryDirMode)
	}
	raw, err := os.ReadFile(filepath.Join(dir, restartPreparationStateName)) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("fixture-secret-never-persist")) || bytes.Contains(raw, []byte("host-org")) || bytes.Contains(raw, []byte("session-id")) {
		t.Fatalf("audit persisted forbidden correlation/secret material: %s", raw)
	}
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := registry.Scan()
	if err != nil || len(entries) != 0 {
		t.Fatalf("registry scan classified audit state: entries=%+v err=%v", entries, err)
	}
}

func TestFreshControllerIgnoresDeadPredecessorPreparedAudit(t *testing.T) {
	dir := t.TempDir()
	if err := writeRestartPreparationAudit(dir, restartPreparationAudit{
		SchemaVersion: 1, Protocol: afclient.DaemonRestartPreflightProtocol,
		PreparationID: "rp_dead_predecessor", State: restartPreparationPrepared,
		ScopeCount: 2, UpdatedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("seed stale audit: %v", err)
	}
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{RegistryDir: dir}})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	d.shims.restartID = func() (string, error) { return "rp_fresh_controller", nil }
	if d.shims.restart != nil {
		t.Fatal("fresh controller loaded predecessor stop authorization")
	}

	result, err := d.PrepareRestart(context.Background())
	if err != nil {
		t.Fatalf("fresh PrepareRestart: %v", err)
	}
	if result.PreparationID != "rp_fresh_controller" || result.PreparationID == "rp_dead_predecessor" {
		t.Fatalf("fresh result inherited predecessor authority: %+v", result)
	}
}

func TestResumePersistsAbandonmentBeforeAdmissionAndPreservesExternalHolds(t *testing.T) {
	d := newRestartTestDaemon(t, nil)
	d.setState(StateDraining)
	d.spawner.Pause()
	preparation := &restartPreparation{
		id: "rp_prepared", state: restartPreparationPrepared, scopeIDs: []string{"org"},
		covered: make(map[string][]sessionshim.FencedSession), requests: make(map[string]sessionshim.FenceRequest),
		acked: make(map[string]sessionshim.Fence), persisted: true,
	}
	d.shims.restart = preparation
	d.shims.fences["org"] = sessionshim.Fence{FenceID: "rp_prepared", State: sessionshim.FenceHeld}
	wantPersistErr := errors.New("audit unavailable")
	d.shims.restartStateWriter = func(record restartPreparationAudit) error {
		if record.State != restartPreparationAbandoned {
			t.Fatalf("resume persisted state %q, want abandoned", record.State)
		}
		if d.spawner.IsAccepting() {
			t.Fatal("admission reopened before durable abandonment")
		}
		return wantPersistErr
	}
	if err := d.ResumeContext(context.Background()); !errors.Is(err, wantPersistErr) {
		t.Fatalf("ResumeContext error = %v, want persist failure", err)
	}
	if d.State() != StateDraining || d.spawner.IsAccepting() || preparation.state != restartPreparationPrepared {
		t.Fatalf("failed resume changed authority/admission: state=%q accepting=%v prep=%q", d.State(), d.spawner.IsAccepting(), preparation.state)
	}
	if _, ok := d.shims.fences["org"]; !ok {
		t.Fatal("failed resume consumed external hold")
	}

	d.shims.restartStateWriter = func(record restartPreparationAudit) error {
		if record.State != restartPreparationAbandoned || d.spawner.IsAccepting() {
			t.Fatalf("abandonment ordering = state %q accepting %v", record.State, d.spawner.IsAccepting())
		}
		return nil
	}
	if err := d.ResumeContext(context.Background()); err != nil {
		t.Fatalf("ResumeContext: %v", err)
	}
	if d.State() != StateRunning || !d.spawner.IsAccepting() || preparation.state != restartPreparationAbandoned {
		t.Fatalf("successful resume = state %q accepting %v prep %q", d.State(), d.spawner.IsAccepting(), preparation.state)
	}
	if _, ok := d.shims.fences["org"]; !ok {
		t.Fatal("successful resume consumed external hold")
	}

	d.shims.restartID = func() (string, error) { return "rp_after_abandon", nil }
	d.shims.restartStateWriter = func(restartPreparationAudit) error { return nil }
	result, err := d.PrepareRestart(context.Background())
	if err != nil || result.PreparationID != "rp_after_abandon" {
		t.Fatalf("later PrepareRestart = %+v, %v; want new identity", result, err)
	}
}

func TestRestartPreflightBlocksDirectThenUpdatePreservesCoveredShim(t *testing.T) {
	var storeCalls atomic.Int32
	store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		storeCalls.Add(1)
		return sessionshim.FenceAcknowledgement{
			RequestBytes: append([]byte(nil), request.RequestBytes...), DurableRevision: "revision",
		}, nil
	})
	shimID := sessionshim.Identity{OrgID: "org", SessionID: "shim-session"}
	d := newRestartTestDaemon(t, store, shimID)
	d.config = &Config{}
	d.spawner.mu.Lock()
	d.spawner.sessions["direct"] = &spawnedSession{
		handle: SessionHandle{SessionID: "direct"}, spec: SessionSpec{SessionID: "direct"},
	}
	d.spawner.mu.Unlock()

	if _, err := d.PrepareRestart(context.Background()); !errors.Is(err, ErrRestartPreflightRefused) {
		t.Fatalf("mixed PrepareRestart error = %v, want direct-owned refusal", err)
	}
	if got := storeCalls.Load(); got != 0 {
		t.Fatalf("store calls while direct session remained = %d, want zero", got)
	}
	if got := d.AdoptedSessionShims(); len(got) != 1 || got[0] != shimID {
		t.Fatalf("direct refusal changed shim ownership: %+v", got)
	}

	// Ordinary terminal delivery removes the uncovered direct owner. The next
	// update can prepare the shim and must not drain or stop it afterward.
	d.spawner.mu.Lock()
	delete(d.spawner.sessions, "direct")
	d.spawner.mu.Unlock()
	var updateCalls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		updateCalls.Add(1)
		return &UpdateResult{Reason: "initiated"}, nil
	}
	if _, err := d.Update(context.Background()); err != nil {
		t.Fatalf("Update after direct terminal: %v", err)
	}
	if storeCalls.Load() != 1 || updateCalls.Load() != 1 {
		t.Fatalf("store/update calls = %d/%d, want 1/1", storeCalls.Load(), updateCalls.Load())
	}
	if got := d.AdoptedSessionShims(); len(got) != 1 || got[0] != shimID {
		t.Fatalf("successful update initiation stopped covered shim: %+v", got)
	}
}

func TestUpdateRefusesBeforeInitiationWhenRestartPreflightFails(t *testing.T) {
	store := restartExactStoreFunc(func(context.Context, sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		return sessionshim.FenceAcknowledgement{}, errors.New("durable store unavailable")
	})
	d := newRestartTestDaemon(t, store, sessionshim.Identity{OrgID: "org", SessionID: "session"})
	d.config = &Config{}
	var initiated atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		initiated.Add(1)
		return &UpdateResult{}, nil
	}

	server := httptest.NewServer(NewServer(d).httpd.Handler)
	t.Cleanup(server.Close)
	res, err := http.Post(server.URL+"/api/daemon/update", "application/json", nil)
	if err != nil {
		t.Fatalf("POST update: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("POST update = %d %s, want 409", res.StatusCode, body)
	}
	if got := initiated.Load(); got != 0 {
		t.Fatalf("update initiations = %d, want zero before durable preflight", got)
	}
}

func TestUpdateHandlerTransfersLifecycleLeaseBeforeSuccessResponse(t *testing.T) {
	d := newRestartTestDaemon(t, nil)
	d.config = &Config{}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		close(entered)
		<-release
		close(finished)
		return &UpdateResult{Reason: "test"}, nil
	}
	server := httptest.NewServer(NewServer(d).httpd.Handler)
	t.Cleanup(server.Close)

	res, err := http.Post(server.URL+"/api/daemon/update", "application/json", nil)
	if err != nil {
		t.Fatalf("POST update: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("POST update = %d %s", res.StatusCode, body)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("prepared update did not initiate")
	}
	d.lifecycleMu.Lock()
	owner := d.lifecycleOwner
	d.lifecycleMu.Unlock()
	if owner == nil || owner.kind != lifecycleUpdate {
		t.Fatalf("update response released lifecycle handoff: owner=%+v", owner)
	}
	if err := d.ResumeContext(context.Background()); err == nil {
		t.Fatal("Resume acquired lifecycle between preflight 2xx and update initiation")
	}
	if d.State() != StateUpdating || d.spawner.IsAccepting() {
		t.Fatalf("in-flight update state/admission = (%q,%v)", d.State(), d.spawner.IsAccepting())
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("prepared update did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for d.State() != StateDraining && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.State() != StateDraining || d.spawner.IsAccepting() {
		t.Fatalf("completed update state/admission = (%q,%v), want draining/false", d.State(), d.spawner.IsAccepting())
	}
}

func TestRestartPreflightSettlesSpawnReservationBeforeSnapshot(t *testing.T) {
	d := newRestartTestDaemon(t, nil)
	d.spawner.mu.Lock()
	d.spawner.spawnReservations["pending"] = struct{}{}
	d.spawner.mu.Unlock()
	done := make(chan struct{})
	go func() {
		_, _ = d.PrepareRestart(context.Background())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("preflight returned before admitted reservation settled")
	case <-time.After(25 * time.Millisecond):
	}
	d.spawner.mu.Lock()
	delete(d.spawner.spawnReservations, "pending")
	d.spawner.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("preflight did not continue after reservation settled")
	}
}

func TestRestartPreparationAuditWriteLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	record := restartPreparationAudit{
		SchemaVersion: 1, Protocol: afclient.DaemonRestartPreflightProtocol,
		PreparationID: "rp_atomic", State: restartPreparationPrepared, ScopeCount: 1,
		UpdatedAt: time.Now().UnixNano(),
	}
	if err := writeRestartPreparationAudit(dir, record); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != restartPreparationStateName {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("audit directory entries = %v, want only %s", names, restartPreparationStateName)
	}
}
