package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// RegisterRequest is the body sent on POST /v1/daemon/register.
type RegisterRequest struct {
	Hostname          string             `json:"hostname"`
	Version           string             `json:"version"`
	MaxAgents         int                `json:"maxAgents"`
	Capabilities      []string           `json:"capabilities"`
	ActiveAgentCount  int                `json:"activeAgentCount"`
	Status            RegistrationStatus `json:"status"`
	Region            string             `json:"region,omitempty"`
	RegistrationToken string             `json:"registrationToken,omitempty"`
}

// RegisterResponse is the response from POST /v1/daemon/register.
type RegisterResponse struct {
	WorkerID                 string `json:"workerId"`
	RuntimeJWT               string `json:"runtimeJwt"`
	HeartbeatIntervalSeconds int    `json:"heartbeatIntervalSeconds"`
	PollIntervalSeconds      int    `json:"pollIntervalSeconds"`
}

// RegisterEndpoint is the relative path on the orchestrator.
const RegisterEndpoint = "/v1/daemon/register"

// CachedJWT is the on-disk cache entry. We persist this between daemon runs
// so re-registration is skipped while the JWT is fresh.
type CachedJWT struct {
	WorkerID                 string `json:"workerId"`
	RuntimeJWT               string `json:"runtimeJwt"`
	HeartbeatIntervalSeconds int    `json:"heartbeatIntervalSeconds"`
	PollIntervalSeconds      int    `json:"pollIntervalSeconds"`
	CachedAt                 string `json:"cachedAt"`
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
	if c.RuntimeJWT == "" || c.WorkerID == "" {
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
		WorkerID:                 resp.WorkerID,
		RuntimeJWT:               resp.RuntimeJWT,
		HeartbeatIntervalSeconds: resp.HeartbeatIntervalSeconds,
		PollIntervalSeconds:      resp.PollIntervalSeconds,
		CachedAt:                 now.UTC().Format(time.RFC3339),
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

// Register dials the orchestrator (or the stub path) and returns a
// RegisterResponse. The cache at jwtPath is consulted first unless
// opts.ForceReregister is set.
//
// Stub path is taken when:
//   - RENSEI_DAEMON_REAL_REGISTRATION env is unset, OR
//   - the orchestrator URL is "file://...", OR
//   - the registration token does not have an rsp_live_ prefix.
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
				WorkerID:                 cached.WorkerID,
				RuntimeJWT:               cached.RuntimeJWT,
				HeartbeatIntervalSeconds: cached.HeartbeatIntervalSeconds,
				PollIntervalSeconds:      cached.PollIntervalSeconds,
			}, nil
		}
	}

	caps := opts.Capabilities
	if len(caps) == 0 {
		caps = []string{"local", "sandbox", "workarea"}
	}
	req := RegisterRequest{
		Hostname:          opts.Hostname,
		Version:           opts.Version,
		MaxAgents:         opts.MaxAgents,
		Capabilities:      caps,
		ActiveAgentCount:  0,
		Status:            RegistrationIdle,
		Region:            opts.Region,
		RegistrationToken: opts.RegistrationToken,
	}

	useStub := os.Getenv("RENSEI_DAEMON_REAL_REGISTRATION") == "" ||
		strings.HasPrefix(opts.OrchestratorURL, "file://") ||
		!strings.HasPrefix(opts.RegistrationToken, "rsp_live_")

	var resp *RegisterResponse
	if useStub {
		resp = buildStubResponse(&req)
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

// callRegisterEndpoint calls the real orchestrator endpoint.
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
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register call: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("registration failed: HTTP %d", res.StatusCode)
	}
	var resp RegisterResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
}

// buildStubResponse mirrors the TS stub path. The JWT is intentionally not a
// real JWT — the `stub.` prefix lets servers reject it if accidentally used.
func buildStubResponse(req *RegisterRequest) *RegisterResponse {
	workerID := fmt.Sprintf("worker-%s-stub", req.Hostname)
	return &RegisterResponse{
		WorkerID:                 workerID,
		RuntimeJWT:               makeStubJWT(workerID, req.Hostname),
		HeartbeatIntervalSeconds: 30,
		PollIntervalSeconds:      10,
	}
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
