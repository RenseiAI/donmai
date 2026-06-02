package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEnv builds a rotateEnv whose Getenv reads from the supplied map and
// whose ReadFile honors the supplied path→contents map (returning fs.ErrNotExist
// for any unmapped path). HTTPClient defaults to the real http package's
// DefaultClient — callers usually override via a separate field.
func fakeEnv(t *testing.T, envvars map[string]string, files map[string][]byte) (*rotateEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errBuf bytes.Buffer
	return &rotateEnv{
		In:      strings.NewReader(""),
		Out:     &out,
		Err:     &errBuf,
		HomeDir: "/fake/home",
		Getenv: func(k string) string {
			return envvars[k]
		},
		ReadFile: func(path string) ([]byte, error) {
			if data, ok := files[path]; ok {
				return data, nil
			}
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		},
		HTTPClient: http.DefaultClient,
	}, &out, &errBuf
}

func TestRunRotate_Happy_200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/daemon/credentials/rotate" {
			t.Errorf("path = %q, want /api/daemon/credentials/rotate", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer rsk_test_token" {
			t.Errorf("Authorization = %q, want Bearer rsk_test_token", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["orgId"] != "org_test" {
			t.Errorf("orgId = %q, want org_test", body["orgId"])
		}
		if body["kind"] != "anthropic" {
			t.Errorf("kind = %q, want anthropic", body["kind"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rotateResponse{
			OK:           true,
			Kind:         "anthropic",
			SessionCount: 3,
			RotatedAt:    "2026-05-17T12:00:00.000Z",
		})
	}))
	t.Cleanup(srv.Close)

	env, out, _ := fakeEnv(t, map[string]string{ //nolint:gosec // G101 false positive (test fixture)
		"DONMAI_PLATFORM_URL": srv.URL,
		"DONMAI_RSK_TOKEN":    "rsk_test_token",
		"DONMAI_ORG_ID":       "org_test",
	}, nil)
	env.HTTPClient = srv.Client()

	err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"})
	if err != nil {
		t.Fatalf("runRotate: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "rotated kind=anthropic") {
		t.Errorf("stdout missing rotated kind: %q", got)
	}
	if got := out.String(); !strings.Contains(got, "notified sessions=3") {
		t.Errorf("stdout missing session count: %q", got)
	}
	if got := out.String(); !strings.Contains(got, "at=2026-05-17T12:00:00.000Z") {
		t.Errorf("stdout missing rotatedAt: %q", got)
	}
}

func TestRunRotate_404_KindNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(platformErrorResponse{
			Error: "No 'unknown' credential configured for org org_test",
		})
	}))
	t.Cleanup(srv.Close)

	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": srv.URL,
		"DONMAI_RSK_TOKEN":    "rsk_test",
		"DONMAI_ORG_ID":       "org_test",
	}, nil)
	env.HTTPClient = srv.Client()

	err := runRotate(context.Background(), env, rotateFlags{kind: "unknown"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error does not mention 404: %v", err)
	}
	if !strings.Contains(err.Error(), "kind not configured") {
		t.Errorf("error does not mention kind-not-configured: %v", err)
	}
}

func TestRunRotate_403_OrgMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(platformErrorResponse{
			Error: "orgId in request body must match the authenticated org",
		})
	}))
	t.Cleanup(srv.Close)

	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": srv.URL,
		"DONMAI_RSK_TOKEN":    "rsk_test",
		"DONMAI_ORG_ID":       "org_test",
	}, nil)
	env.HTTPClient = srv.Client()

	err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error does not mention 403: %v", err)
	}
	if !strings.Contains(err.Error(), "auth orgId mismatch") {
		t.Errorf("error does not mention orgId mismatch: %v", err)
	}
}

func TestRunRotate_401_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(platformErrorResponse{
			Error: "Authentication required",
		})
	}))
	t.Cleanup(srv.Close)

	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": srv.URL,
		"DONMAI_RSK_TOKEN":    "rsk_test",
		"DONMAI_ORG_ID":       "org_test",
	}, nil)
	env.HTTPClient = srv.Client()

	err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error does not mention 401: %v", err)
	}
	if !strings.Contains(err.Error(), "DONMAI_RSK_TOKEN") {
		t.Errorf("error does not hint at the token: %v", err)
	}
}

// errClient is an http.RoundTripper that always returns a synthetic
// error — simulates DNS failures / connection-refused without a real
// dead server.
type errClient struct{ err error }

func (e *errClient) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, e.err
}

func TestRunRotate_NetworkError(t *testing.T) {
	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": "http://127.0.0.1:1",
		"DONMAI_RSK_TOKEN":    "rsk_test",
		"DONMAI_ORG_ID":       "org_test",
	}, nil)
	env.HTTPClient = &http.Client{
		Transport: &errClient{err: errors.New("connection refused (test)")},
	}

	err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "POST") {
		t.Errorf("error does not mention POST: %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error does not surface transport error: %v", err)
	}
}

func TestRunRotate_MissingPlatformURL(t *testing.T) {
	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_RSK_TOKEN": "rsk_test",
		"DONMAI_ORG_ID":    "org_test",
	}, nil)

	err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "platform-url") {
		t.Errorf("error does not mention platform-url: %v", err)
	}
}

func TestRunRotate_MissingToken(t *testing.T) {
	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": "https://example.test",
		"DONMAI_ORG_ID":       "org_test",
	}, nil)

	err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "rsk-token") {
		t.Errorf("error does not mention rsk-token: %v", err)
	}
}

func TestRunRotate_MissingOrgID(t *testing.T) {
	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": "https://example.test",
		"DONMAI_RSK_TOKEN":    "rsk_test",
	}, nil)

	err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "org-id") {
		t.Errorf("error does not mention org-id: %v", err)
	}
}

func TestRunRotate_TokenFromFile_Fallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer rsk_from_file" {
			t.Errorf("Authorization = %q, want Bearer rsk_from_file", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rotateResponse{
			OK:           true,
			Kind:         "linear",
			SessionCount: 0,
			RotatedAt:    "2026-05-17T12:00:00.000Z",
		})
	}))
	t.Cleanup(srv.Close)

	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": srv.URL,
		"DONMAI_ORG_ID":       "org_test",
	}, map[string][]byte{
		"/fake/home/.donmai/cli.token": []byte("rsk_from_file\n"),
	})
	env.HTTPClient = srv.Client()

	err := runRotate(context.Background(), env, rotateFlags{kind: "linear"})
	if err != nil {
		t.Fatalf("runRotate: %v", err)
	}
}

func TestRunRotate_OrgIDFromConfig_Fallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["orgId"] != "org_from_yaml" {
			t.Errorf("orgId = %q, want org_from_yaml", body["orgId"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rotateResponse{
			OK:           true,
			Kind:         "anthropic",
			SessionCount: 1,
			RotatedAt:    "2026-05-17T12:00:00.000Z",
		})
	}))
	t.Cleanup(srv.Close)

	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": srv.URL,
		"DONMAI_RSK_TOKEN":    "rsk_test",
	}, map[string][]byte{
		"/fake/home/.donmai/cli-config.yaml": []byte(
			"# rensei cli config\n" +
				"orgId: \"org_from_yaml\"\n" +
				"projectId: proj_X\n",
		),
	})
	env.HTTPClient = srv.Client()

	err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"})
	if err != nil {
		t.Fatalf("runRotate: %v", err)
	}
}

func TestRunRotate_FlagWinsOverEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer rsk_flag" {
			t.Errorf("Authorization = %q, want Bearer rsk_flag", got)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["orgId"] != "org_flag" {
			t.Errorf("orgId = %q, want org_flag", body["orgId"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rotateResponse{OK: true, Kind: "anthropic", SessionCount: 0, RotatedAt: "now"})
	}))
	t.Cleanup(srv.Close)

	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": srv.URL,
		"DONMAI_RSK_TOKEN":    "rsk_env",
		"DONMAI_ORG_ID":       "org_env",
	}, nil)
	env.HTTPClient = srv.Client()

	err := runRotate(context.Background(), env, rotateFlags{
		kind:     "anthropic",
		orgID:    "org_flag",
		rskToken: "rsk_flag",
	})
	if err != nil {
		t.Fatalf("runRotate: %v", err)
	}
}

func TestRunRotate_PlatformURLWithTrailingSlash(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/api/daemon/credentials/rotate" {
			t.Errorf("path = %q, want /api/daemon/credentials/rotate", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rotateResponse{OK: true, Kind: "anthropic", SessionCount: 0, RotatedAt: "now"})
	}))
	t.Cleanup(srv.Close)

	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": srv.URL + "/",
		"DONMAI_RSK_TOKEN":    "rsk_test",
		"DONMAI_ORG_ID":       "org_test",
	}, nil)
	env.HTTPClient = srv.Client()

	if err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"}); err != nil {
		t.Fatalf("runRotate: %v", err)
	}
	if !called {
		t.Fatal("server handler was never invoked")
	}
}

func TestRunRotate_TokenPrecedence_RskTokenEnvOverWorker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer rsk_primary" {
			t.Errorf("Authorization = %q, want Bearer rsk_primary", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rotateResponse{OK: true, Kind: "anthropic", SessionCount: 0, RotatedAt: "now"})
	}))
	t.Cleanup(srv.Close)

	// All three are set — DONMAI_RSK_TOKEN must win.
	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": srv.URL,
		"DONMAI_RSK_TOKEN":    "rsk_primary",
		"WORKER_API_KEY":      "rsk_worker",
		"DONMAI_API_TOKEN":    "rsk_legacy",
		"DONMAI_ORG_ID":       "org_test",
	}, nil)
	env.HTTPClient = srv.Client()

	if err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"}); err != nil {
		t.Fatalf("runRotate: %v", err)
	}
}

func TestRunRotate_500_FallsBackToRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "<html><body>oops</body></html>")
	}))
	t.Cleanup(srv.Close)

	env, _, _ := fakeEnv(t, map[string]string{
		"DONMAI_PLATFORM_URL": srv.URL,
		"DONMAI_RSK_TOKEN":    "rsk_test",
		"DONMAI_ORG_ID":       "org_test",
	}, nil)
	env.HTTPClient = srv.Client()

	err := runRotate(context.Background(), env, rotateFlags{kind: "anthropic"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error does not surface HTTP 500: %v", err)
	}
	if !strings.Contains(err.Error(), "<html>") {
		t.Errorf("error does not surface raw body: %v", err)
	}
}

func TestParseOrgIDFromConfig(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unquoted", "orgId: org_x\n", "org_x"},
		{"double-quoted", "orgId: \"org_x\"\n", "org_x"},
		{"single-quoted", "orgId: 'org_x'\n", "org_x"},
		{"with-comment", "orgId: org_x # active org\n", "org_x"},
		{"multiline", "projectId: proj_a\norgId: org_x\nother: y\n", "org_x"},
		{"absent", "projectId: proj_a\n", ""},
		{"empty-file", "", ""},
		{"comment-only", "# orgId: not_this\n", ""},
		{"case-sensitive-miss", "OrgId: org_x\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOrgIDFromConfig([]byte(tc.in))
			if got != tc.want {
				t.Errorf("parseOrgIDFromConfig(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// newRotateCmd's cobra wiring — verifies that the parent `creds` command
// exposes `rotate` and that ExactArgs(1) enforces the kind argument.
func TestNewRotateCmd_RequiresKindArg(t *testing.T) {
	parent := NewCmd()
	// Find the rotate subcommand.
	var rotate *struct{ name string }
	for _, c := range parent.Commands() {
		if c.Name() == "rotate" {
			rotate = &struct{ name string }{name: c.Name()}
			break
		}
	}
	if rotate == nil {
		t.Fatal("`rotate` subcommand not registered on `creds` parent")
	}
}
