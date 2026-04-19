package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sampleRegisterRequest returns a populated RegisterRequest used by the
// register tests so field assertions have something non-zero to compare.
func sampleRegisterRequest() RegisterRequest {
	return RegisterRequest{
		Hostname:     "host-1",
		PID:          4242,
		Version:      "v0.1.0",
		Capabilities: []string{"claude", "codex"},
		MaxAgents:    8,
	}
}

// assertStateUnchanged fails the test if the Client's WorkerID or
// RuntimeJWT is non-empty. Used after every failure-path Register call.
func assertStateUnchanged(t *testing.T, c *Client) {
	t.Helper()
	if c.WorkerID != "" {
		t.Errorf("WorkerID = %q, want empty after failed Register", c.WorkerID)
	}
	if c.RuntimeJWT != "" {
		t.Errorf("RuntimeJWT = %q, want empty after failed Register", c.RuntimeJWT)
	}
}

func TestRegister_Success(t *testing.T) {
	var (
		gotAuth, gotMethod, gotPath, gotCT string
		gotBody                            RegisterRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("server decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"worker_id": "w_abc123",
			"runtime_jwt": "rjwt_xyz",
			"heartbeat_interval_seconds": 15
		}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "rsp_live_test")
	req := sampleRegisterRequest()

	resp, err := c.Register(context.Background(), req)
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}

	// Returned response.
	if resp.WorkerID != "w_abc123" {
		t.Errorf("resp.WorkerID = %q, want w_abc123", resp.WorkerID)
	}
	if resp.RuntimeJWT != "rjwt_xyz" {
		t.Errorf("resp.RuntimeJWT = %q, want rjwt_xyz", resp.RuntimeJWT)
	}
	if resp.HeartbeatIntervalSeconds != 15 {
		t.Errorf("resp.HeartbeatIntervalSeconds = %d, want 15", resp.HeartbeatIntervalSeconds)
	}
	if got, want := resp.HeartbeatInterval(), 15*time.Second; got != want {
		t.Errorf("HeartbeatInterval() = %v, want %v", got, want)
	}

	// Client mutated.
	if c.WorkerID != "w_abc123" {
		t.Errorf("c.WorkerID = %q, want w_abc123", c.WorkerID)
	}
	if c.RuntimeJWT != "rjwt_xyz" {
		t.Errorf("c.RuntimeJWT = %q, want rjwt_xyz", c.RuntimeJWT)
	}

	// Request inspection.
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/workers/register" {
		t.Errorf("path = %s, want /api/workers/register", gotPath)
	}
	if gotAuth != "Bearer rsp_live_test" {
		t.Errorf("Authorization = %q, want Bearer rsp_live_test", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody.Hostname != req.Hostname {
		t.Errorf("body.Hostname = %q, want %q", gotBody.Hostname, req.Hostname)
	}
	if gotBody.PID != req.PID {
		t.Errorf("body.PID = %d, want %d", gotBody.PID, req.PID)
	}
	if gotBody.Version != req.Version {
		t.Errorf("body.Version = %q, want %q", gotBody.Version, req.Version)
	}
	if gotBody.MaxAgents != req.MaxAgents {
		t.Errorf("body.MaxAgents = %d, want %d", gotBody.MaxAgents, req.MaxAgents)
	}
	if len(gotBody.Capabilities) != len(req.Capabilities) {
		t.Fatalf("body.Capabilities = %v, want %v", gotBody.Capabilities, req.Capabilities)
	}
	for i, cap := range req.Capabilities {
		if gotBody.Capabilities[i] != cap {
			t.Errorf("body.Capabilities[%d] = %q, want %q", i, gotBody.Capabilities[i], cap)
		}
	}
}

func TestRegister_StatusErrorsTable(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"401 bad provisioning token", http.StatusUnauthorized, ErrInvalidProvisioningToken},
		{"429 rate limited", http.StatusTooManyRequests, ErrRateLimited},
		{"500 server error", http.StatusInternalServerError, ErrRegistrationFailed},
		{"503 service unavailable", http.StatusServiceUnavailable, ErrRegistrationFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(srv.URL, "rsp_live_test")
			resp, err := c.Register(context.Background(), sampleRegisterRequest())
			if err == nil {
				t.Fatalf("expected error, got nil resp=%+v", resp)
			}
			if resp != nil {
				t.Errorf("resp = %+v, want nil", resp)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false (err = %v)", tc.wantErr, err)
			}
			// 5xx must NOT leak the underlying ErrServerError to callers.
			if tc.wantErr == ErrRegistrationFailed && errors.Is(err, ErrServerError) {
				t.Errorf("5xx error should be remapped away from ErrServerError, got %v", err)
			}
			assertStateUnchanged(t, c)
		})
	}
}

func TestRegister_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"worker_id": "w_abc"`)) // truncated, invalid JSON
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "rsp_live_test")
	resp, err := c.Register(context.Background(), sampleRegisterRequest())
	if err == nil {
		t.Fatalf("expected decode error, got nil resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("resp = %+v, want nil", resp)
	}
	assertStateUnchanged(t, c)
}

func TestRegister_NetworkError(t *testing.T) {
	// Start then immediately close the server so the next request hits a
	// closed listener and the transport reports a connection error.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	addr := srv.URL
	srv.Close()

	c := NewClient(addr, "rsp_live_test")
	resp, err := c.Register(context.Background(), sampleRegisterRequest())
	if err == nil {
		t.Fatalf("expected transport error, got nil resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("resp = %+v, want nil", resp)
	}
	// Ensure the error is wrapped with the register prefix.
	if !containsSubstr(err.Error(), "register:") {
		t.Errorf("err = %v, want to contain 'register:' prefix", err)
	}
	assertStateUnchanged(t, c)
}

func TestRegister_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client cancels so we can observe ctx propagation.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "rsp_live_test")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Do

	resp, err := c.Register(ctx, sampleRegisterRequest())
	if err == nil {
		t.Fatalf("expected context error, got nil resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("resp = %+v, want nil", resp)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false (err = %v)", err)
	}
	assertStateUnchanged(t, c)
}

// containsSubstr is a tiny helper so the register tests do not pull in
// strings just to assert an error prefix.
func containsSubstr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
