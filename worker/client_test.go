package worker

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

// newTestClient returns a Client pointed at srv with both tokens
// pre-populated so either POST/GET helper can be exercised.
func newTestClient(srv *httptest.Server) *Client {
	c := NewClient(srv.URL, "rsp_live_test")
	c.RuntimeJWT = "rjwt_test"
	c.WorkerID = "w_test"
	return c
}

func TestNewClient_TrimsTrailingSlashAndSetsTimeout(t *testing.T) {
	c := NewClient("https://example.test/", "rsp_live_x")
	if c.BaseURL != "https://example.test" {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, "https://example.test")
	}
	if c.ProvisioningToken != "rsp_live_x" {
		t.Errorf("ProvisioningToken = %q", c.ProvisioningToken)
	}
	if c.HTTPClient == nil {
		t.Fatal("HTTPClient is nil")
	}
	if c.HTTPClient.Timeout != defaultHTTPTimeout {
		t.Errorf("timeout = %v, want %v", c.HTTPClient.Timeout, defaultHTTPTimeout)
	}
}

func TestPostWithProvisioning_SuccessSendsProvisioningBearer(t *testing.T) {
	var gotAuth, gotCT, gotMethod, gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	var resp struct {
		OK bool `json:"ok"`
	}
	err := c.postWithProvisioning(context.Background(), "/reg", map[string]string{"hello": "world"}, &resp)
	if err != nil {
		t.Fatalf("postWithProvisioning error: %v", err)
	}
	if !resp.OK {
		t.Errorf("resp.OK = false, want true")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/reg" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer rsp_live_test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody["hello"] != "world" {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestPostWithRuntime_SuccessSendsRuntimeBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	if err := c.postWithRuntime(context.Background(), "/hb", nil, nil); err != nil {
		t.Fatalf("postWithRuntime error: %v", err)
	}
	if gotAuth != "Bearer rjwt_test" {
		t.Errorf("Authorization = %q, want Bearer rjwt_test", gotAuth)
	}
}

func TestGetWithRuntime_SuccessDecodes(t *testing.T) {
	var gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		_, _ = w.Write([]byte(`{"work_items":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	var resp PollResponse
	if err := c.getWithRuntime(context.Background(), "/poll", &resp); err != nil {
		t.Fatalf("getWithRuntime error: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s", gotMethod)
	}
	if gotAuth != "Bearer rjwt_test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if resp.WorkItems == nil {
		// Not fatal: caller may treat nil and empty as equivalent.
		t.Logf("WorkItems decoded as nil (expected empty slice)")
	}
}

func TestStatusErrorMappings(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		runtime bool
		want    error
	}{
		{"provisioning 401", http.StatusUnauthorized, false, ErrInvalidProvisioningToken},
		{"runtime 401", http.StatusUnauthorized, true, ErrRuntimeJWTExpired},
		{"403 runtime", http.StatusForbidden, true, ErrRuntimeJWTInvalid},
		{"403 provisioning", http.StatusForbidden, false, ErrRuntimeJWTInvalid},
		{"404", http.StatusNotFound, true, ErrNotFound},
		{"429", http.StatusTooManyRequests, true, ErrRateLimited},
		{"500", http.StatusInternalServerError, true, ErrServerError},
		{"503", http.StatusServiceUnavailable, false, ErrServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := statusToError(tc.status, "/x", tc.runtime)
			if got == nil {
				t.Fatalf("nil error for status %d", tc.status)
			}
			if !errors.Is(got, tc.want) {
				t.Errorf("errors.Is(%v, %v) = false", got, tc.want)
			}
		})
	}
}

func TestStatusToError_SuccessIsNil(t *testing.T) {
	for _, code := range []int{200, 201, 204, 299} {
		if err := statusToError(code, "/x", true); err != nil {
			t.Errorf("status %d returned %v", code, err)
		}
	}
}

func TestStatusToError_UnexpectedStatus(t *testing.T) {
	err := statusToError(http.StatusTeapot, "/x", true)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Must not match a sentinel.
	for _, s := range []error{
		ErrNotFound, ErrRateLimited, ErrServerError,
		ErrRuntimeJWTExpired, ErrRuntimeJWTInvalid, ErrInvalidProvisioningToken,
	} {
		if errors.Is(err, s) {
			t.Errorf("unexpected-status err matched sentinel %v", s)
		}
	}
}

func TestPost_401ProvisioningMapsToInvalidProvisioningToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	err := c.postWithProvisioning(context.Background(), "/reg", nil, nil)
	if !errors.Is(err, ErrInvalidProvisioningToken) {
		t.Fatalf("err = %v, want ErrInvalidProvisioningToken", err)
	}
}

func TestPost_401RuntimeMapsToRuntimeJWTExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	err := c.postWithRuntime(context.Background(), "/hb", nil, nil)
	if !errors.Is(err, ErrRuntimeJWTExpired) {
		t.Fatalf("err = %v, want ErrRuntimeJWTExpired", err)
	}
}

func TestGet_401RuntimeMapsToRuntimeJWTExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	err := c.getWithRuntime(context.Background(), "/poll", nil)
	if !errors.Is(err, ErrRuntimeJWTExpired) {
		t.Fatalf("err = %v, want ErrRuntimeJWTExpired", err)
	}
}

func TestPost_403MapsToRuntimeJWTInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	err := c.postWithRuntime(context.Background(), "/hb", nil, nil)
	if !errors.Is(err, ErrRuntimeJWTInvalid) {
		t.Fatalf("err = %v, want ErrRuntimeJWTInvalid", err)
	}
}

func TestGet_404MapsToNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	err := c.getWithRuntime(context.Background(), "/poll", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPost_429MapsToRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	err := c.postWithRuntime(context.Background(), "/hb", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestPost_500MapsToServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	err := c.postWithRuntime(context.Background(), "/hb", nil, nil)
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("err = %v, want ErrServerError", err)
	}
}

func TestPost_MalformedJSONReturnsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"not-json`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	var out map[string]any
	err := c.postWithProvisioning(context.Background(), "/reg", nil, &out)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want to mention decode", err)
	}
}

func TestGet_MalformedJSONReturnsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"broken`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	var out PollResponse
	err := c.getWithRuntime(context.Background(), "/poll", &out)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want to mention decode", err)
	}
}

func TestPost_ContextCancellationPropagates(t *testing.T) {
	// Server sleeps long enough for us to cancel.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Do

	err := c.postWithProvisioning(ctx, "/reg", nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(context.Canceled) = false, err = %v", err)
	}
}

func TestGet_ContextCancellationPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.getWithRuntime(ctx, "/poll", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(context.Canceled) = false, err = %v", err)
	}
}

func TestPost_MarshalFailureReturnsError(t *testing.T) {
	// chan values cannot be marshaled to JSON.
	c := NewClient("http://example.test", "rsp_live_x")
	err := c.postWithProvisioning(context.Background(), "/reg", make(chan int), nil)
	if err == nil {
		t.Fatal("expected marshal error, got nil")
	}
	if !strings.Contains(err.Error(), "marshal") {
		t.Errorf("err = %v, want to mention marshal", err)
	}
}

func TestPost_NilBodySendsEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if len(b) != 0 {
			t.Errorf("body = %q, want empty", string(b))
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	if err := c.postWithRuntime(context.Background(), "/hb", nil, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestPost_NilTargetSkipsDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Return invalid JSON; if client tried to decode this it would fail.
		_, _ = w.Write([]byte(`not json at all`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	if err := c.postWithRuntime(context.Background(), "/hb", nil, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestGet_NilTargetSkipsDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv)
	if err := c.getWithRuntime(context.Background(), "/poll", nil); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestPost_RequestFailureWrappedError(t *testing.T) {
	// Use a URL pointing at a closed listener to force a transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	addr := srv.URL
	srv.Close()

	c := NewClient(addr, "rsp_live_x")
	err := c.postWithProvisioning(context.Background(), "/reg", nil, nil)
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "worker post:") {
		t.Errorf("err = %v, want worker post: prefix", err)
	}
}

func TestGet_RequestFailureWrappedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	addr := srv.URL
	srv.Close()

	c := NewClient(addr, "rsp_live_x")
	c.RuntimeJWT = "rjwt"
	err := c.getWithRuntime(context.Background(), "/poll", nil)
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "worker get:") {
		t.Errorf("err = %v, want worker get: prefix", err)
	}
}

func TestPost_MissingTokenSendsNoAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Empty runtime JWT and empty provisioning token on the same client.
	c := NewClient(srv.URL, "")
	if err := c.postWithProvisioning(context.Background(), "/x", nil, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty", gotAuth)
	}
}
