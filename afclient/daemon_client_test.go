package afclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDaemonClientPrepareRestartClosedPermissionAndRefusal(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantState  DaemonRestartPreflightState
		wantErr    error
		wantRefuse bool
	}{
		{
			name:      "prepared",
			body:      `{"protocol":"session-shim-restart-preflight-v1","state":"prepared","preparationId":"rp_server","scopeCount":2}`,
			wantState: DaemonRestartPrepared,
		},
		{
			name:      "not required",
			body:      `{"protocol":"session-shim-restart-preflight-v1","state":"not_required","preparationId":"rp_empty","scopeCount":0}`,
			wantState: DaemonRestartNotRequired,
		},
		{
			name:    "unknown success field is refusal",
			body:    `{"protocol":"session-shim-restart-preflight-v1","state":"not_required","preparationId":"rp_empty","scopeCount":0,"permission":true}`,
			wantErr: ErrInvalidRestartPreflightResponse,
		},
		{
			name:    "empty success is refusal",
			body:    `{}`,
			wantErr: ErrInvalidRestartPreflightResponse,
		},
		{
			name:    "second JSON value is refusal",
			body:    `{"protocol":"session-shim-restart-preflight-v1","state":"not_required","preparationId":"rp_empty","scopeCount":0} {"permission":true}`,
			wantErr: ErrInvalidRestartPreflightResponse,
		},
		{
			name:    "oversized success is refusal",
			body:    `{"protocol":"session-shim-restart-preflight-v1","state":"not_required","preparationId":"rp_empty","scopeCount":0}` + strings.Repeat(" ", 8193),
			wantErr: ErrInvalidRestartPreflightResponse,
		},
		{
			name:       "typed conflict",
			status:     http.StatusConflict,
			body:       `{"error":"durable acknowledgement missing","code":"restart_preflight_refused"}`,
			wantErr:    ErrRestartPreflightRefused,
			wantRefuse: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/daemon/restart/prepare" || r.Method != http.MethodPost {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "" {
					t.Errorf("Authorization = %q, want empty localhost request", got)
				}
				payload, readErr := io.ReadAll(r.Body)
				if readErr != nil || len(payload) != 0 {
					t.Errorf("caller supplied restart body = %q, %v; want none", payload, readErr)
				}
				status := tc.status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			got, err := NewDaemonClientFromURL(srv.URL).PrepareRestart()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("PrepareRestart error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantRefuse {
				var refusal *DaemonRestartPreflightRefusalError
				if !errors.As(err, &refusal) || refusal.Code != DaemonRestartPreflightRefusalCode || !errors.Is(err, ErrConflict) {
					t.Fatalf("typed refusal = %#v, %v", refusal, err)
				}
			}
			if tc.wantErr == nil && (got == nil || got.State != tc.wantState) {
				t.Fatalf("PrepareRestart = %+v, want state %q", got, tc.wantState)
			}
		})
	}
}

func TestDaemonClientPrepareRestartUsesDefaultTransportTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	client := NewDaemonClientFromURL(srv.URL)
	client.httpClient.Timeout = 20 * time.Millisecond
	if _, err := client.PrepareRestart(); err == nil {
		t.Fatal("PrepareRestart ignored the configured transport timeout")
	}
}

func TestDaemonClientDrainContextAllowsLongerDrain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/drain" {
			http.NotFound(w, r)
			return
		}
		var req DaemonDrainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode drain request: %v", err)
			return
		}
		if req.TimeoutSeconds != 1 {
			t.Errorf("timeoutSeconds = %d, want 1", req.TimeoutSeconds)
			return
		}
		time.Sleep(75 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(DaemonActionResponse{OK: true, Message: "drained"})
	}))
	t.Cleanup(srv.Close)

	client := NewDaemonClientFromURL(srv.URL)
	client.httpClient.Timeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	resp, err := client.DrainContext(ctx, 1)
	if err != nil {
		t.Fatalf("DrainContext: %v", err)
	}
	if resp == nil || !resp.OK {
		t.Fatalf("DrainContext response = %#v, want successful response", resp)
	}
}

func TestDaemonClientDrainContextCancelsPromptly(t *testing.T) {
	started := make(chan struct{})
	handlerCanceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(handlerCanceled)
	}))
	t.Cleanup(srv.Close)

	client := NewDaemonClientFromURL(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.DrainContext(ctx, 1)
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("drain handler did not start")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DrainContext error = %v, want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("DrainContext did not return after cancellation")
	}
	select {
	case <-handlerCanceled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("drain handler did not observe cancellation")
	}
}

// TestDaemonClientLifecycleContextDoors pins the host-watch lifecycle
// surface: pause, resume and per-session stop each send one authenticated
// POST, the session id travels URL-escaped in the stop path, and an empty
// id refuses without a request.
func TestDaemonClientLifecycleContextDoors(t *testing.T) {
	const token = "lifecycle-test-token"
	var (
		mu     sync.Mutex
		calls  []string
		auths  []string
		bodies []string
		paths  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		auths = append(auths, r.Header.Get("Authorization"))
		bodies = append(bodies, strings.TrimSpace(string(body)))
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"message":"done"}`)
	}))
	t.Cleanup(srv.Close)

	client := NewDaemonClientFromURL(srv.URL)
	client.SetControlToken(token)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.PauseContext(ctx); err != nil {
		t.Fatalf("PauseContext: %v", err)
	}
	if _, err := client.ResumeContext(ctx); err != nil {
		t.Fatalf("ResumeContext: %v", err)
	}
	if _, err := client.StopSessionContext(ctx, "sess-1"); err != nil {
		t.Fatalf("StopSessionContext: %v", err)
	}
	// An id hostile to path routing travels as one escaped segment.
	if _, err := client.StopSessionContext(ctx, "a/b c"); err != nil {
		t.Fatalf("StopSessionContext escaped: %v", err)
	}
	if _, err := client.DrainContext(ctx, 7); err != nil {
		t.Fatalf("DrainContext: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantCalls := []string{
		"POST /api/daemon/pause",
		"POST /api/daemon/resume",
		"POST /api/daemon/sessions/sess-1/stop",
		"POST /api/daemon/sessions/a%2Fb%20c/stop",
		"POST /api/daemon/drain",
	}
	if len(calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %d requests", calls, len(wantCalls))
	}
	for i, want := range wantCalls {
		if calls[i] != want {
			t.Errorf("call %d = %q, want %q", i, calls[i], want)
		}
		if auths[i] != "Bearer "+token {
			t.Errorf("call %d Authorization = %q, want the operator bearer", i, auths[i])
		}
	}
	// Drain carries the explicit deadline in its JSON body.
	var drainReq DaemonDrainRequest
	if err := json.Unmarshal([]byte(bodies[4]), &drainReq); err != nil || drainReq.TimeoutSeconds != 7 {
		t.Errorf("drain body = %q (decode %v), want timeoutSeconds 7", bodies[4], err)
	}
	_ = paths
}

// TestDaemonClientStopSessionContextEmptyID refuses without a request.
func TestDaemonClientStopSessionContextEmptyID(t *testing.T) {
	requested := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	client := NewDaemonClientFromURL(srv.URL)
	for _, id := range []string{"", "   "} {
		if _, err := client.StopSessionContext(context.Background(), id); !errors.Is(err, ErrBadRequest) {
			t.Errorf("StopSessionContext(%q) error = %v, want ErrBadRequest", id, err)
		}
	}
	if requested {
		t.Error("empty session id must not reach the daemon")
	}
}
