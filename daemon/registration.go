package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RegistrationOptions configure a single Register call.
type RegistrationOptions struct {
	OrchestratorURL   string
	RegistrationToken string
	MachineID         string
	Hostname          string
	Version           string
	MaxAgents         int
	Capabilities      []string
	Region            string
	JWTPath           string
	ForceReregister   bool

	// HTTPClient is the client used when the real (non-stub) path is taken.
	// Defaults to http.DefaultClient with a 10s timeout.
	HTTPClient *http.Client
	// Now lets tests deterministically clock the cached-at timestamp.
	Now func() time.Time
}

// RegisterRequest is the JSON body sent on POST /api/workers/register.
//
// The platform contract (see platform/src/app/api/workers/register/route.ts):
//
//	{ machineId?: string, hostname: string, capacity: number, version?: string, projects?: string[] }
//
// The registration token is sent in the Authorization: Bearer header, NOT in
// the body. Status / region / capabilities / activeAgentCount are not part of
// the platform contract — they live in the heartbeat payload, or are inferred
// from the project's Linear tracker bindings on the server side.
type RegisterRequest struct {
	MachineID string   `json:"machineId,omitempty"`
	Hostname  string   `json:"hostname"`
	Capacity  int      `json:"capacity"`
	Version   string   `json:"version,omitempty"`
	Projects  []string `json:"projects,omitempty"`
}

// RegisterResponse is the JSON response from POST /api/workers/register.
//
// Platform contract:
//
//	{ workerId, heartbeatInterval (ms), pollInterval (ms),
//	  runtimeToken, runtimeTokenExpiresAt }
//
// Field names mirror the wire shape; helper methods provide seconds-based
// accessors used by the heartbeat scheduler.
type RegisterResponse struct {
	WorkerID              string `json:"workerId"`
	HeartbeatInterval     int    `json:"heartbeatInterval"` // ms
	PollInterval          int    `json:"pollInterval"`      // ms
	RuntimeToken          string `json:"runtimeToken"`
	RuntimeTokenExpiresAt string `json:"runtimeTokenExpiresAt,omitempty"`
}

// HeartbeatIntervalSeconds returns the heartbeat cadence in seconds (rounded
// up). The platform reports the cadence in milliseconds; daemon code that
// schedules tickers historically worked in seconds.
func (r *RegisterResponse) HeartbeatIntervalSeconds() int {
	if r.HeartbeatInterval <= 0 {
		return 0
	}
	// Round up so 30000 ms doesn't truncate to 29s.
	return (r.HeartbeatInterval + 999) / 1000
}

// PollIntervalSeconds returns the poll cadence in seconds (rounded up).
func (r *RegisterResponse) PollIntervalSeconds() int {
	if r.PollInterval <= 0 {
		return 0
	}
	return (r.PollInterval + 999) / 1000
}

// RegisterEndpoint is the relative path on the platform.
const RegisterEndpoint = "/api/workers/register"

// CachedJWT is the on-disk cache entry. We persist this between daemon runs
// so re-registration is skipped while the runtime token is fresh.
type CachedJWT struct {
	WorkerID              string `json:"workerId"`
	RuntimeToken          string `json:"runtimeToken"`
	HeartbeatInterval     int    `json:"heartbeatInterval"` // ms
	PollInterval          int    `json:"pollInterval"`      // ms
	RuntimeTokenExpiresAt string `json:"runtimeTokenExpiresAt,omitempty"`
	CachedAt              string `json:"cachedAt"`

	// Legacy fields retained so old cache files written before REN-1422
	// still load successfully. Newer writes only populate the canonical
	// platform-named fields above.
	LegacyRuntimeJWT               string `json:"runtimeJwt,omitempty"`
	LegacyHeartbeatIntervalSeconds int    `json:"heartbeatIntervalSeconds,omitempty"`
	LegacyPollIntervalSeconds      int    `json:"pollIntervalSeconds,omitempty"`
}

// LoadCachedJWT reads ~/.rensei/daemon.jwt. Returns (nil, nil) when the file
// does not exist or cannot be parsed.
func LoadCachedJWT(jwtPath string) (*CachedJWT, error) {
	data, err := os.ReadFile(jwtPath) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read jwt cache: %w", err)
	}
	var c CachedJWT
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, nil //nolint:nilerr // corrupt cache → re-register
	}
	// Migrate legacy fields written by daemons pre-REN-1422.
	if c.RuntimeToken == "" && c.LegacyRuntimeJWT != "" {
		c.RuntimeToken = c.LegacyRuntimeJWT
	}
	if c.HeartbeatInterval == 0 && c.LegacyHeartbeatIntervalSeconds > 0 {
		c.HeartbeatInterval = c.LegacyHeartbeatIntervalSeconds * 1000
	}
	if c.PollInterval == 0 && c.LegacyPollIntervalSeconds > 0 {
		c.PollInterval = c.LegacyPollIntervalSeconds * 1000
	}
	if c.RuntimeToken == "" || c.WorkerID == "" {
		return nil, nil
	}
	return &c, nil
}

// SaveCachedJWT atomically writes the response to jwtPath with 0o600 perms.
func SaveCachedJWT(jwtPath string, resp *RegisterResponse, now time.Time) error {
	dir := filepath.Dir(jwtPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create jwt dir: %w", err)
	}
	entry := CachedJWT{
		WorkerID:              resp.WorkerID,
		RuntimeToken:          resp.RuntimeToken,
		HeartbeatInterval:     resp.HeartbeatInterval,
		PollInterval:          resp.PollInterval,
		RuntimeTokenExpiresAt: resp.RuntimeTokenExpiresAt,
		CachedAt:              now.UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal jwt cache: %w", err)
	}
	tmp := jwtPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write jwt cache: %w", err)
	}
	if err := os.Rename(tmp, jwtPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename jwt cache: %w", err)
	}
	return nil
}

// looksLikeRegistrationToken returns true if the input has one of the
// platform's accepted registration-token prefixes.
//
// The platform's worker-protocol/auth.ts (via the unified validateApiKey hot
// path landed in REN-1351 / REN-1353) accepts both:
//   - rsp_live_*   — legacy worker_registration tokens
//   - rsk_live_*   — the unified API-key prefix that REN-1351 made the new
//     mint format. Tokens minted via /api/projects/<id>/runtime-tokens or
//     /api/org/<id>/keys today come back as rsk_live_*.
//
// Anything else falls through to the stub path so local dev / tests don't
// accidentally reach prod.
func looksLikeRegistrationToken(token string) bool {
	return strings.HasPrefix(token, "rsp_live_") || strings.HasPrefix(token, "rsk_live_")
}

// Register dials the platform (or the stub path) and returns a
// RegisterResponse. The cache at jwtPath is consulted first unless
// opts.ForceReregister is set.
//
// Real-platform registration is the default. The stub path is taken when:
//   - RENSEI_DAEMON_FORCE_STUB env is set (e.g. =1), OR
//   - the orchestrator URL is "file://...", OR
//   - the registration token does not start with rsp_live_ or rsk_live_.
//
// REN-1444 (v0.4.1) inverted the env-gate from opt-in to opt-out. The
// previous default required RENSEI_DAEMON_REAL_REGISTRATION=1 in the
// launchd plist; with that env unset, a daemon configured with a real
// rsk_live_* token would silently fall back to stub mode and never
// register against the platform.
func Register(ctx context.Context, opts RegistrationOptions) (*RegisterResponse, error) {
	if opts.JWTPath == "" {
		opts.JWTPath = DefaultJWTPath()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if !opts.ForceReregister {
		if cached, _ := LoadCachedJWT(opts.JWTPath); cached != nil {
			return &RegisterResponse{
				WorkerID:              cached.WorkerID,
				RuntimeToken:          cached.RuntimeToken,
				HeartbeatInterval:     cached.HeartbeatInterval,
				PollInterval:          cached.PollInterval,
				RuntimeTokenExpiresAt: cached.RuntimeTokenExpiresAt,
			}, nil
		}
	}

	// Capacity is derived from MaxAgents. Platform requires capacity > 0.
	capacity := opts.MaxAgents
	if capacity <= 0 {
		capacity = 1
	}
	req := RegisterRequest{
		MachineID: opts.MachineID,
		Hostname:  opts.Hostname,
		Capacity:  capacity,
		Version:   opts.Version,
	}
	if req.MachineID == "" {
		req.MachineID = opts.Hostname
	}

	useStub := stubModeRequested() ||
		strings.HasPrefix(opts.OrchestratorURL, "file://") ||
		!looksLikeRegistrationToken(opts.RegistrationToken)

	var resp *RegisterResponse
	if useStub {
		resp = buildStubResponse(opts.Hostname)
	} else {
		var err error
		resp, err = callRegisterEndpoint(ctx, opts, &req)
		if err != nil {
			return nil, err
		}
	}

	if err := SaveCachedJWT(opts.JWTPath, resp, opts.Now()); err != nil {
		// Cache write failure is non-fatal — the daemon can still run.
		// Return the response so the caller can proceed.
		return resp, nil
	}
	return resp, nil
}

// callRegisterEndpoint calls the real platform endpoint.
//
// The registration token is sent in the Authorization: Bearer header (per
// platform contract — the token is NOT in the request body).
func callRegisterEndpoint(ctx context.Context, opts RegistrationOptions, body *RegisterRequest) (*RegisterResponse, error) {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	url := strings.TrimRight(opts.OrchestratorURL, "/") + RegisterEndpoint
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+opts.RegistrationToken)
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register call: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		// Read up to 2 KiB of the error body so failures are diagnosable.
		errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		snippet := strings.TrimSpace(string(errBuf))
		if snippet != "" {
			return nil, fmt.Errorf("registration failed: HTTP %d: %s", res.StatusCode, snippet)
		}
		return nil, fmt.Errorf("registration failed: HTTP %d", res.StatusCode)
	}
	var resp RegisterResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.WorkerID == "" {
		return nil, fmt.Errorf("registration response missing workerId")
	}
	return &resp, nil
}

// buildStubResponse returns a deterministic local-only response. The runtime
// token is intentionally not a real JWT — the `stub.` prefix lets servers
// reject it if accidentally used.
func buildStubResponse(hostname string) *RegisterResponse {
	workerID := fmt.Sprintf("worker-%s-stub", hostname)
	return &RegisterResponse{
		WorkerID:          workerID,
		RuntimeToken:      makeStubJWT(workerID, hostname),
		HeartbeatInterval: 30000, // ms — same wire shape as platform
		PollInterval:      10000,
	}
}

// stubModeRequested returns true when the operator has explicitly opted into
// stub registration via RENSEI_DAEMON_FORCE_STUB. The legacy
// RENSEI_DAEMON_REAL_REGISTRATION env (REN-1422) is also honoured: setting
// it to "0" / "false" / "off" / "no" forces stub mode for back-compat with
// existing test harnesses; any other non-empty value is treated as a no-op
// (real path, the new default).
//
// REN-1444 (v0.4.1): real registration is now the default. Previously the
// daemon required RENSEI_DAEMON_REAL_REGISTRATION=1 in the launchd plist;
// without it, a fully-configured daemon silently fell back to stub mode.
func stubModeRequested() bool {
	if v := os.Getenv("RENSEI_DAEMON_FORCE_STUB"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "off", "no", "":
			// Explicit opt-out of stub mode; fall through.
		default:
			return true
		}
	}
	if v := os.Getenv("RENSEI_DAEMON_REAL_REGISTRATION"); v != "" {
		// Honour explicit "0" / "false" so the legacy plist export of
		// =0 still forces stub mode.
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "off", "no":
			return true
		}
	}
	return false
}

func makeStubJWT(workerID, hostname string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"stub","typ":"JWT"}`))
	payload := map[string]any{
		"sub":      workerID,
		"iss":      "rensei-daemon-stub",
		"iat":      time.Now().Unix(),
		"hostname": hostname,
		"stub":     true,
	}
	pbytes, _ := json.Marshal(payload)
	return "stub." + header + "." + base64.RawURLEncoding.EncodeToString(pbytes) + ".stub-signature"
}
