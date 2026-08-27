package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// standDownShimKeys is every flat key the attestation can contribute to a
// request. The tests below assert on the exact set present, not merely on the
// one key under test, because "explicit absence" is a claim about what is NOT
// sent as much as about what is.
var standDownShimKeys = []string{
	"sessionShimSupported",
	"sessionShimControllerId",
	"sessionShimProtocolMin",
	"sessionShimProtocolMax",
	"sessionShimCapabilities",
}

func shimKeysPresent(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request: %v (raw=%s)", err, raw)
	}
	present := make(map[string]any)
	for _, key := range standDownShimKeys {
		if value, ok := body[key]; ok {
			present[key] = value
		}
	}
	return present
}

// TestSessionShimSupportStatesEachHaveOneExactWireForm pins the whole point of
// the three-state encoding: absent, stand-down, and supported must be three
// distinguishable things on the wire, and absent must still be byte-identical
// to a request from a daemon that predates the attestation.
func TestSessionShimSupportStatesEachHaveOneExactWireForm(t *testing.T) {
	t.Parallel()
	base := RegisterRequest{MachineID: "machine-wire", Hostname: "host-wire", Capacity: 3}
	supported := SessionShimHostAttestation{
		Supported:    SessionShimSupported,
		ControllerID: "ctl-wire",
		ProtocolMin:  1,
		ProtocolMax:  3,
		Capabilities: RequiredSessionShimHostCapabilities(),
	}

	legacyBytes, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal legacy request: %v", err)
	}
	if strings.Contains(string(legacyBytes), "sessionShim") {
		t.Fatalf("a daemon presenting no attestation emitted a session-shim key: %s", legacyBytes)
	}
	if want := `{"machineId":"machine-wire","hostname":"host-wire","capacity":3}`; string(legacyBytes) != want {
		t.Fatalf("legacy request bytes = %s, want %s", legacyBytes, want)
	}

	standDown := base
	standDown.SessionShimHostAttestation = SessionShimStandDownAttestation()
	standDownBytes, err := json.Marshal(standDown)
	if err != nil {
		t.Fatalf("marshal stand-down request: %v", err)
	}
	present := shimKeysPresent(t, standDownBytes)
	if len(present) != 1 {
		t.Fatalf("stand-down emitted %d session-shim keys, want exactly one: %s", len(present), standDownBytes)
	}
	if value, ok := present["sessionShimSupported"]; !ok || value != false {
		t.Fatalf("stand-down sessionShimSupported = %#v, want false: %s", value, standDownBytes)
	}

	full := base
	full.SessionShimHostAttestation = supported
	fullBytes, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal supported request: %v", err)
	}
	present = shimKeysPresent(t, fullBytes)
	if len(present) != len(standDownShimKeys) {
		t.Fatalf("supported emitted %d session-shim keys, want %d: %s",
			len(present), len(standDownShimKeys), fullBytes)
	}
	if value := present["sessionShimSupported"]; value != true {
		t.Fatalf("supported sessionShimSupported = %#v, want true: %s", value, fullBytes)
	}
	if value := present["sessionShimControllerId"]; value != "ctl-wire" {
		t.Fatalf("supported sessionShimControllerId = %#v: %s", value, fullBytes)
	}
	if value := present["sessionShimProtocolMax"]; value != float64(3) {
		t.Fatalf("supported sessionShimProtocolMax = %#v: %s", value, fullBytes)
	}

	// Decode is the exact inverse of encode across all three states, so a
	// consumer reading these bytes back recovers the state that was meant.
	for name, tc := range map[string]struct {
		raw  []byte
		want SessionShimSupportState
	}{
		"absent":     {raw: legacyBytes, want: SessionShimSupportAbsent},
		"stand down": {raw: standDownBytes, want: SessionShimStandDown},
		"supported":  {raw: fullBytes, want: SessionShimSupported},
	} {
		var decoded RegisterRequest
		if err := json.Unmarshal(tc.raw, &decoded); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if decoded.Supported != tc.want {
			t.Errorf("%s: decoded support state = %d, want %d", name, decoded.Supported, tc.want)
		}
	}
}

// TestStandDownAttestationValidatesAsAClaimlessDeclaration keeps the stand-down
// honest: it declares an absence and may carry nothing else, while the absent
// state stays valid and disabled.
func TestStandDownAttestationValidatesAsAClaimlessDeclaration(t *testing.T) {
	t.Parallel()
	standDown := SessionShimStandDownAttestation()
	if err := standDown.validate(); err != nil {
		t.Fatalf("stand-down attestation rejected: %v", err)
	}
	if standDown.Supports() || standDown.enabled() {
		t.Fatal("stand-down attestation reported session-shim support")
	}
	if !standDown.StandsDown() {
		t.Fatal("stand-down attestation did not report standing down")
	}
	if (SessionShimHostAttestation{}).StandsDown() {
		t.Fatal("an absent attestation claimed to stand down")
	}
	if (SessionShimHostAttestation{}).exactEqual(standDown) {
		t.Fatal("absent and stand-down compared equal; they are different declarations")
	}

	smuggled := standDown
	smuggled.ControllerID = "ctl-smuggled"
	if err := smuggled.validate(); err == nil {
		t.Fatal("a stand-down carrying a controller id was accepted")
	}
}

func registrationCaptureServer(t *testing.T, captured *[]byte, response RegisterResponse, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RegisterEndpoint {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		*captured = append([]byte(nil), raw...)
		if calls != nil {
			calls.Add(1)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestRegisterPresentsExplicitAbsenceForAStandDownHost is the wire fact the
// whole change exists for: a daemon with no shim composition can now SAY so on
// its ordinary registration, and it says nothing else.
func TestRegisterPresentsExplicitAbsenceForAStandDownHost(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	var captured []byte
	server := registrationCaptureServer(t, &captured, RegisterResponse{
		WorkerID: "worker-stand-down", RuntimeToken: "runtime-stand-down",
		HeartbeatInterval: 30_000, PollInterval: 10_000,
	}, nil)

	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL: server.URL, RegistrationToken: "rsp_live_stand_down",
		Hostname: "host-stand-down", MaxAgents: 2,
		JWTPath:     filepath.Join(t.TempDir(), "daemon.jwt"),
		SessionShim: SessionShimStandDownAttestation(),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.WorkerID != "worker-stand-down" {
		t.Fatalf("worker id = %q", resp.WorkerID)
	}
	present := shimKeysPresent(t, captured)
	if len(present) != 1 || present["sessionShimSupported"] != false {
		t.Fatalf("stand-down registration presented %#v, want exactly sessionShimSupported=false: %s",
			present, captured)
	}
	// A stand-down is not a recovery registration: it publishes real capacity
	// and needs no receipt, which is what separates it from the auth-only lane.
	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["capacity"] != float64(2) {
		t.Fatalf("stand-down registration capacity = %#v, want 2: %s", body["capacity"], captured)
	}
}

// TestRegisterKeepsLegacyRequestBytesForAnAbsentAttestation is the other half of
// the contract: a daemon that never composes a shim must keep sending exactly
// what it sent before the attestation existed.
func TestRegisterKeepsLegacyRequestBytesForAnAbsentAttestation(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	var captured []byte
	server := registrationCaptureServer(t, &captured, RegisterResponse{
		WorkerID: "worker-legacy", RuntimeToken: "runtime-legacy",
		HeartbeatInterval: 30_000, PollInterval: 10_000,
	}, nil)

	if _, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL: server.URL, RegistrationToken: "rsp_live_legacy",
		Hostname: "host-legacy", MaxAgents: 1,
		JWTPath: filepath.Join(t.TempDir(), "daemon.jwt"),
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if strings.Contains(string(captured), "sessionShim") {
		t.Fatalf("legacy registration emitted a session-shim key: %s", captured)
	}
}

// TestRefreshPresentsExplicitAbsenceForAStandDownHost covers the second lane
// with the same rule — the refresh endpoint reads the same attestation, so a
// host that stood down at registration must not fall silent an hour later.
func TestRefreshPresentsExplicitAbsenceForAStandDownHost(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/worker-refresh/refresh-token" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		captured = append([]byte(nil), raw...)
		_ = json.NewEncoder(w).Encode(refreshResponse{RuntimeToken: "runtime-refreshed"})
	}))
	t.Cleanup(server.Close)

	if _, err := callRefreshEndpoint(context.Background(), RegistrationOptions{
		OrchestratorURL: server.URL, RegistrationToken: "rsp_live_refresh",
		SessionShim: SessionShimStandDownAttestation(),
	}, "worker-refresh"); err != nil {
		t.Fatalf("callRefreshEndpoint: %v", err)
	}
	present := shimKeysPresent(t, captured)
	if len(present) != 1 || present["sessionShimSupported"] != false {
		t.Fatalf("stand-down refresh presented %#v, want exactly sessionShimSupported=false: %s",
			present, captured)
	}
}

// TestStandDownDropsACacheMintedUnderShimSupport proves the declaration is
// actually delivered. A host degrading from a shim-enabled run holds a cached
// credential whose receipt still says "enabled"; reusing it would keep the
// stand-down bottled up in the daemon forever.
func TestStandDownDropsACacheMintedUnderShimSupport(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	for name, tc := range map[string]struct {
		cachedShim  *SessionShimCredentialReceipt
		wantCalls   int32
		wantWorker  string
		description string
	}{
		"cache minted under shim support": {
			cachedShim: &SessionShimCredentialReceipt{
				Enabled: true, State: SessionShimCredentialStateReady,
				WorkerHostID: "stable-host", AdoptionRevision: "7",
				ControllerID: "ctl-cached", ProtocolMin: 1, ProtocolMax: 3,
				Capabilities: RequiredSessionShimHostCapabilities(),
			},
			wantCalls:  1,
			wantWorker: "worker-fresh",
		},
		"ordinary cache": {
			cachedShim: nil,
			wantCalls:  0,
			wantWorker: "worker-cached",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var captured []byte
			var calls atomic.Int32
			server := registrationCaptureServer(t, &captured, RegisterResponse{
				WorkerID: "worker-fresh", RuntimeToken: "runtime-fresh",
				HeartbeatInterval: 30_000, PollInterval: 10_000,
			}, &calls)

			jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
			if err := SaveCachedJWT(jwtPath, &RegisterResponse{
				WorkerID: "worker-cached", RuntimeToken: "runtime-cached",
				HeartbeatInterval: 30_000, PollInterval: 10_000,
				SessionShim: tc.cachedShim,
			}, time.Now()); err != nil {
				t.Fatalf("seed cache: %v", err)
			}

			resp, err := Register(context.Background(), RegistrationOptions{
				OrchestratorURL: server.URL, RegistrationToken: "rsp_live_cache",
				Hostname: "host-cache", MaxAgents: 1, JWTPath: jwtPath,
				SessionShim: SessionShimStandDownAttestation(),
			})
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("registration calls = %d, want %d", got, tc.wantCalls)
			}
			if resp.WorkerID != tc.wantWorker {
				t.Fatalf("worker id = %q, want %q", resp.WorkerID, tc.wantWorker)
			}
			if tc.wantCalls > 0 {
				present := shimKeysPresent(t, captured)
				if len(present) != 1 || present["sessionShimSupported"] != false {
					t.Fatalf("presented %#v, want exactly sessionShimSupported=false: %s", present, captured)
				}
			}
		})
	}
}

// TestDaemonStandDownOptionDeclaresAbsenceAndRefusesContradiction covers the
// embedder seam: the option turns silence into a declaration, and asking for
// both a composed attestation and a stand-down fails at startup rather than
// letting the wire disagree with the configuration.
func TestDaemonStandDownOptionDeclaresAbsenceAndRefusesContradiction(t *testing.T) {
	t.Parallel()
	standDown := New(Options{SessionShimStandDown: true})
	if got := standDown.SessionShimHostAttestation(); !got.StandsDown() || got.Supports() {
		t.Fatalf("stand-down daemon attestation = %#v, want an explicit stand-down", got)
	}
	if standDown.sessionShimAttestationError() != nil {
		t.Fatalf("stand-down daemon failed to compose: %v", standDown.sessionShimAttestationError())
	}

	silent := New(Options{})
	if got := silent.SessionShimHostAttestation(); got.StandsDown() {
		t.Fatalf("a daemon that never asked for the shim declared a stand-down: %#v", got)
	}

	contradiction := New(Options{
		SessionShimStandDown: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:               true,
			RequireCredentialAttestation: true,
			AttestationCapabilities:      RequiredSessionShimHostCapabilities(),
		},
	})
	err := contradiction.Start(context.Background())
	if err == nil {
		t.Fatal("a daemon composing both an attestation and a stand-down started")
	}
	if !strings.Contains(err.Error(), "stand-down contradicts") {
		t.Fatalf("Start error = %v, want the stand-down contradiction refusal", err)
	}
}

// TestDaemonNormalRegistrationCarriesTheStandDown drives the real Start path:
// the option has to reach the ordinary worker registration, not merely sit in
// the daemon. This is the lane a degraded host actually crashes on.
func TestDaemonNormalRegistrationCarriesTheStandDown(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	var captured atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == RegisterEndpoint:
			raw, _ := io.ReadAll(r.Body)
			captured.CompareAndSwap(nil, append([]byte(nil), raw...))
			_ = json.NewEncoder(w).Encode(RegisterResponse{
				WorkerID: "worker-stand-down-daemon", RuntimeToken: "runtime.stand.down",
				HeartbeatInterval: 60_000, PollInterval: 60_000,
			})
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
		case strings.HasSuffix(r.URL.Path, "/poll"):
			_ = json.NewEncoder(w).Encode(PollResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	cfg := DefaultConfig()
	cfg.Machine.ID = "stand-down-daemon-test"
	cfg.Orchestrator.URL = server.URL
	cfg.Orchestrator.AuthToken = "rsp_live_stand_down_daemon"
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	d := New(Options{
		ConfigPath: configPath, JWTPath: filepath.Join(dir, "daemon.jwt"),
		SkipWizard: true, SessionShimStandDown: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })

	raw, _ := captured.Load().([]byte)
	if len(raw) == 0 {
		t.Fatal("daemon start performed no registration")
	}
	present := shimKeysPresent(t, raw)
	if len(present) != 1 || present["sessionShimSupported"] != false {
		t.Fatalf("daemon registration presented %#v, want exactly sessionShimSupported=false: %s", present, raw)
	}
}
