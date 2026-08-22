package afclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
