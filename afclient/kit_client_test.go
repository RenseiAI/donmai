package afclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// kitMockServer creates an httptest.Server that dispatches kit routes to a
// table of (method, path, status, body) entries. Returns the server and a
// captured-body pointer the caller can inspect for POST bodies.
type kitMockEntry struct {
	method   string
	path     string
	status   int
	respBody string
}

func newKitMockServer(t *testing.T, entries []kitMockEntry, capture *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, e := range entries {
		entry := e
		mux.HandleFunc(entry.path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != entry.method {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if capture != nil {
				body, _ := io.ReadAll(r.Body)
				*capture = string(body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(entry.status)
			_, _ = w.Write([]byte(entry.respBody))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDaemonClient_ListKits(t *testing.T) {
	body := `{"kits":[{"id":"spring/java","name":"Spring","version":"1.0","status":"active","source":"local","scope":"project","trust":"unsigned","providesCommands":true,"providesPrompts":false,"providesTools":false,"providesMcpServers":false,"providesSkills":false,"providesAgents":false,"providesA2aSkills":false,"providesExtractors":false}]}`
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodGet, "/api/daemon/kits", http.StatusOK, body},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)
	resp, err := c.ListKits()
	if err != nil {
		t.Fatalf("ListKits: %v", err)
	}
	if len(resp.Kits) != 1 {
		t.Fatalf("Kits: want 1, got %d", len(resp.Kits))
	}
	if resp.Kits[0].ID != "spring/java" {
		t.Errorf("Kits[0].ID: want spring/java, got %q", resp.Kits[0].ID)
	}
	if !resp.Kits[0].ProvidesCommands {
		t.Error("ProvidesCommands: want true")
	}
}

func TestDaemonClient_ListKits_NetworkError(t *testing.T) {
	c := NewDaemonClientFromURL("http://127.0.0.1:1") // unroutable
	if _, err := c.ListKits(); err == nil {
		t.Error("ListKits: want network error, got nil")
	}
}

func TestDaemonClient_GetKit_OK(t *testing.T) {
	body := `{"kit":{"id":"spring/java","name":"Spring","version":"1.0","status":"active","source":"local","scope":"project","trust":"unsigned","providesCommands":true,"providesPrompts":false,"providesTools":false,"providesMcpServers":false,"providesSkills":false,"providesAgents":false,"providesA2aSkills":false,"providesExtractors":false,"supportedOs":["linux","macos"]}}`
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodGet, "/api/daemon/kits/spring/java", http.StatusOK, body},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)
	resp, err := c.GetKit("spring/java")
	if err != nil {
		t.Fatalf("GetKit: %v", err)
	}
	if resp.Kit.ID != "spring/java" {
		t.Errorf("Kit.ID: want spring/java, got %q", resp.Kit.ID)
	}
	if len(resp.Kit.SupportedOS) != 2 {
		t.Errorf("SupportedOS: want 2, got %d", len(resp.Kit.SupportedOS))
	}
}

func TestDaemonClient_GetKit_NotFound(t *testing.T) {
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodGet, "/api/daemon/kits/nope", http.StatusNotFound, `{"error":"not found"}`},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)
	_, err := c.GetKit("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetKit nope: want ErrNotFound, got %v", err)
	}
}

func TestDaemonClient_VerifyKitSignature_OK(t *testing.T) {
	body := `{"kitId":"spring/java","trust":"unsigned","ok":true,"details":"Wave 9 caveat"}`
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodGet, "/api/daemon/kits/spring/java/verify-signature", http.StatusOK, body},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)
	resp, err := c.VerifyKitSignature("spring/java")
	if err != nil {
		t.Fatalf("VerifyKitSignature: %v", err)
	}
	if resp.Trust != KitTrustUnsigned {
		t.Errorf("Trust: want unsigned, got %q", resp.Trust)
	}
	if !resp.OK {
		t.Error("OK: want true")
	}
}

func TestDaemonClient_InstallKit_501(t *testing.T) {
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodPost, "/api/daemon/kits/spring/java/install", http.StatusNotImplemented, `{"error":"not implemented"}`},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)
	_, err := c.InstallKit("spring/java", KitInstallRequest{Version: "1.0"})
	if err == nil {
		t.Fatal("InstallKit: want error, got nil")
	}
	if !strings.Contains(err.Error(), "InstallKit") {
		t.Errorf("InstallKit error: want wrapped, got %v", err)
	}
}

func TestDaemonClient_InstallKit_BodyEncoded(t *testing.T) {
	var captured string
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodPost, "/api/daemon/kits/spring/java/install", http.StatusOK, `{"kit":{"id":"spring/java","name":"","version":"2.0","status":"active","source":"local","scope":"project","trust":"unsigned","providesCommands":false,"providesPrompts":false,"providesTools":false,"providesMcpServers":false,"providesSkills":false,"providesAgents":false,"providesA2aSkills":false,"providesExtractors":false},"message":"installed"}`},
	}, &captured)
	c := NewDaemonClientFromURL(srv.URL)
	resp, err := c.InstallKit("spring/java", KitInstallRequest{Version: "2.0"})
	if err != nil {
		t.Fatalf("InstallKit: %v", err)
	}
	if resp.Message != "installed" {
		t.Errorf("Message: want installed, got %q", resp.Message)
	}
	var got KitInstallRequest
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("decode captured: %v", err)
	}
	if got.Version != "2.0" {
		t.Errorf("captured Version: want 2.0, got %q", got.Version)
	}
}

func TestDaemonClient_EnableDisableKit(t *testing.T) {
	body := `{"id":"spring/java","name":"Spring","version":"1.0","status":"active","source":"local","scope":"project","trust":"unsigned","providesCommands":false,"providesPrompts":false,"providesTools":false,"providesMcpServers":false,"providesSkills":false,"providesAgents":false,"providesA2aSkills":false,"providesExtractors":false}`
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodPost, "/api/daemon/kits/spring/java/enable", http.StatusOK, body},
		{http.MethodPost, "/api/daemon/kits/spring/java/disable", http.StatusOK, strings.Replace(body, `"active"`, `"disabled"`, 1)},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)

	k, err := c.EnableKit("spring/java")
	if err != nil {
		t.Fatalf("EnableKit: %v", err)
	}
	if k.Status != KitStatusActive {
		t.Errorf("EnableKit Status: want active, got %q", k.Status)
	}

	k, err = c.DisableKit("spring/java")
	if err != nil {
		t.Fatalf("DisableKit: %v", err)
	}
	if k.Status != KitStatusDisabled {
		t.Errorf("DisableKit Status: want disabled, got %q", k.Status)
	}
}

func TestDaemonClient_EnableKit_NotFound(t *testing.T) {
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodPost, "/api/daemon/kits/nope/enable", http.StatusNotFound, `{"error":"not found"}`},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)
	if _, err := c.EnableKit("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("EnableKit nope: want ErrNotFound, got %v", err)
	}
}

func TestDaemonClient_ListKitSources(t *testing.T) {
	body := `{"sources":[{"name":"local","url":"~/.rensei/kits","enabled":true,"priority":1,"kind":"local"},{"name":"tessl","url":"https://registry.tessl.io","enabled":false,"priority":4,"kind":"tessl"}]}`
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodGet, "/api/daemon/kit-sources", http.StatusOK, body},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)
	resp, err := c.ListKitSources()
	if err != nil {
		t.Fatalf("ListKitSources: %v", err)
	}
	if len(resp.Sources) != 2 {
		t.Fatalf("Sources: want 2, got %d", len(resp.Sources))
	}
	if resp.Sources[0].Name != "local" {
		t.Errorf("Sources[0].Name: want local, got %q", resp.Sources[0].Name)
	}
}

func TestDaemonClient_EnableDisableSource(t *testing.T) {
	body := `{"source":{"name":"tessl","url":"https://registry.tessl.io","enabled":true,"priority":4,"kind":"tessl"},"message":"source tessl enabled"}`
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodPost, "/api/daemon/kit-sources/tessl/enable", http.StatusOK, body},
		{http.MethodPost, "/api/daemon/kit-sources/tessl/disable", http.StatusOK, strings.Replace(body, `"enabled":true`, `"enabled":false`, 1)},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)

	r, err := c.EnableKitSource("tessl")
	if err != nil {
		t.Fatalf("EnableKitSource: %v", err)
	}
	if !r.Source.Enabled {
		t.Error("EnableKitSource Enabled: want true")
	}

	r, err = c.DisableKitSource("tessl")
	if err != nil {
		t.Fatalf("DisableKitSource: %v", err)
	}
	if r.Source.Enabled {
		t.Error("DisableKitSource Enabled: want false")
	}
}

func TestDaemonClient_DisableSource_NotFound(t *testing.T) {
	srv := newKitMockServer(t, []kitMockEntry{
		{http.MethodPost, "/api/daemon/kit-sources/nope/disable", http.StatusNotFound, `{"error":"not found"}`},
	}, nil)
	c := NewDaemonClientFromURL(srv.URL)
	if _, err := c.DisableKitSource("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DisableKitSource nope: want ErrNotFound, got %v", err)
	}
}
