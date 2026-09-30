package afclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureControlToken_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-token")
	first, err := EnsureControlToken(path)
	if err != nil {
		t.Fatalf("EnsureControlToken: %v", err)
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("expected non-empty minted token")
	}
	second, err := EnsureControlToken(path)
	if err != nil {
		t.Fatalf("EnsureControlToken again: %v", err)
	}
	if second != first {
		t.Errorf("second ensure = %q, want stable %q", second, first)
	}
	loaded, err := LoadControlToken(path)
	if err != nil {
		t.Fatalf("LoadControlToken: %v", err)
	}
	if loaded != first {
		t.Errorf("loaded = %q, want %q", loaded, first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
}

func TestDaemonClient_AttachesControlTokenOnMutatingRequests(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"message":"paused"}`)
	}))
	t.Cleanup(srv.Close)
	u := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, ok := strings.Cut(u, ":")
	if !ok {
		t.Fatalf("test server URL = %q", srv.URL)
	}
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	cfg := DefaultDaemonConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.ControlToken = "opaque-operator-token"
	c := NewDaemonClient(cfg)
	if _, err := c.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if gotAuth != "Bearer opaque-operator-token" {
		t.Errorf("Authorization = %q, want bearer token", gotAuth)
	}
	if gotMethod != "POST" || gotPath != "/api/daemon/pause" {
		t.Errorf("request = %s %s, want POST /api/daemon/pause", gotMethod, gotPath)
	}
}
