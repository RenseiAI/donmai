package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

// compositionHarness is one fake control plane plus the daemon that talks to
// it, wired the way a deferred composition actually runs: the daemon registers
// standing down, and the composed configuration is installed afterwards.
type compositionHarness struct {
	t *testing.T

	daemon      *Daemon
	attestation SessionShimHostAttestation
	orgID       string
	registryDir string
	server      *httptest.Server

	mu               sync.Mutex
	registerBodies   [][]byte
	refreshBodies    [][]byte
	heartbeatBodies  []heartbeatRequestBody
	registerBlockFor time.Duration
}

// heartbeats returns every heartbeat body the control plane has received.
func (h *compositionHarness) heartbeats() []heartbeatRequestBody {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]heartbeatRequestBody(nil), h.heartbeatBodies...)
}

// lastHeartbeat returns the most recent beat, or false when none has landed.
func (h *compositionHarness) lastHeartbeat() (heartbeatRequestBody, bool) {
	beats := h.heartbeats()
	if len(beats) == 0 {
		return heartbeatRequestBody{}, false
	}
	return beats[len(beats)-1], true
}

func (h *compositionHarness) registrations() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.registerBodies...)
}

func (h *compositionHarness) refreshes() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.refreshBodies...)
}

// shimKeysIn reports the flat session-shim attestation keys present in one
// captured request body. "Explicit absence" is a claim about what is NOT sent
// as much as about what is, so the tests assert on the exact set.
func shimKeysIn(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	return shimKeysPresent(t, raw)
}

// newCompositionHarness stands up a control plane that answers registration,
// refresh, heartbeat and poll, and a stand-down daemon pointed at it. The
// daemon is NOT started; callers do that so they can observe the timing.
func newCompositionHarness(t *testing.T) *compositionHarness {
	t.Helper()
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	dir := t.TempDir()
	h := &compositionHarness{
		t:           t,
		attestation: activationTestAttestation(),
		orgID:       "org-composition",
		registryDir: filepath.Join(dir, "registry"),
	}
	const workerID = "worker-composition"

	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case RegisterEndpoint:
			raw, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			h.registerBodies = append(h.registerBodies, append([]byte(nil), raw...))
			block := h.registerBlockFor
			h.mu.Unlock()
			if block > 0 {
				time.Sleep(block)
			}
			var presented SessionShimHostAttestation
			_ = json.Unmarshal(raw, &presented)
			resp := RegisterResponse{
				WorkerID: workerID, RuntimeToken: "runtime-composition",
				HeartbeatInterval: 3_600_000, PollInterval: 3_600_000,
			}
			if presented.Supports() {
				resp.SessionShim = activationTestCredentialReceipt(
					presented, SessionShimCredentialStateRecovering,
					"stable-host-composition", "revision-declared",
				)
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/api/workers/" + workerID + "/refresh-token":
			raw, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			h.refreshBodies = append(h.refreshBodies, append([]byte(nil), raw...))
			h.mu.Unlock()
			var presented SessionShimHostAttestation
			_ = json.Unmarshal(raw, &presented)
			resp := refreshResponse{RuntimeToken: "runtime-composition-refreshed"}
			if presented.Supports() {
				resp.SessionShim = activationTestCredentialReceipt(
					presented, SessionShimCredentialStateRecovering,
					"stable-host-composition", "revision-declared",
				)
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/api/workers/" + workerID + "/heartbeat":
			var body heartbeatRequestBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode heartbeat: %v", err)
			}
			h.mu.Lock()
			h.heartbeatBodies = append(h.heartbeatBodies, body)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(heartbeatResponseBody{
				Acknowledged: true, SessionShim: body.SessionShim,
			})
		case "/api/workers/" + workerID + "/poll":
			_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.server.Close)

	configPath := filepath.Join(dir, "daemon.yaml")
	cfg := DefaultConfig()
	cfg.Machine.ID = "composition-test"
	cfg.Orchestrator.URL = h.server.URL
	cfg.Orchestrator.AuthToken = "rsp_live_composition"
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h.daemon = New(Options{
		ConfigPath: configPath, JWTPath: filepath.Join(dir, "daemon.jwt"),
		HTTPHost: "127.0.0.1", HTTPPort: 0, SkipWizard: true,
		SessionShimStandDown: true,
	})
	return h
}

// composedConfig is the configuration an embedder would hand the seam once its
// own control-plane work finished. onBatch is the one hook the tests drive.
func (h *compositionHarness) composedConfig(
	onBatch func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error),
) SessionShimConfig {
	return SessionShimConfig{
		EnableAdoption:               true,
		EnableOwnership:              true,
		RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness:   testSessionShimProofV2Readiness,
		ControllerID:                 h.attestation.ControllerID,
		AttestationCapabilities:      h.attestation.Capabilities,
		OrgID:                        h.orgID,
		RegistryDir:                  h.registryDir,
		OnAdoptionBatch:              onBatch,
		OnCarrierActivationAcknowledged: func(SessionShimPublishedBatchReceipt) {
		},
	}
}

func (h *compositionHarness) start(ctx context.Context) {
	h.t.Helper()
	if err := h.daemon.Start(ctx); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	h.t.Cleanup(func() { _ = h.daemon.Stop(context.Background()) })
}

// TestControlListenerAnswersWhileTheDaemonIsStillStarting pins the other half
// of the change. "Connection refused" and "still coming up" are the same signal
// to every caller and mean opposite things; the listener binds first so the
// wait stops being indistinguishable from a failure.
func TestControlListenerAnswersWhileTheDaemonIsStillStarting(t *testing.T) {
	h := newCompositionHarness(t)
	h.mu.Lock()
	h.registerBlockFor = 300 * time.Millisecond
	h.mu.Unlock()

	srv := NewServer(h.daemon)
	errCh, err := srv.StartBeforeDaemon()
	if err != nil {
		t.Fatalf("control server start: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		<-errCh
	})

	// The port answers before Start has even been called, which is the point:
	// a caller can tell "starting" from "not there".
	if _, err := net.DialTimeout("tcp", srv.Addr(), time.Second); err != nil {
		t.Fatalf("control listener refused a connection before the daemon started: %v", err)
	}
	status, body := getStatus(t, "http://"+srv.Addr()+"/api/daemon/status")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("pre-start control response = %d %s, want 503 with a starting notice", status, body)
	}
	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode pre-start body %q: %v", body, err)
	}
	if payload["status"] != "starting" {
		t.Fatalf("pre-start control body = %#v, want status=starting", payload)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)
	srv.DaemonStarted()

	served := getJSON(t, "http://"+srv.Addr()+"/api/daemon/status")
	if served["status"] != string(afclient.DaemonReady) {
		t.Fatalf("post-start control status = %#v, want %q", served["status"], afclient.DaemonReady)
	}
}

func getStatus(t *testing.T, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, body
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	status, body := getStatus(t, url)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v (%s)", url, err, body)
	}
	return out
}
