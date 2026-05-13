package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	internaldaemon "github.com/RenseiAI/agentfactory-tui/internal/daemon"
)

// newCapabilitiesTestServer builds a minimal test Server backed by a Daemon
// that has a pre-populated capabilitySet. The http.ServeMux registers only
// the capabilities endpoint so tests are isolated from unrelated handlers.
func newCapabilitiesTestServer(t *testing.T, caps []internaldaemon.SubstrateCapability) *httptest.Server {
	t.Helper()
	d := &Daemon{}
	if caps != nil {
		cs := internaldaemon.NewCapabilitySet()
		cs.Detect(func(name string) (string, error) {
			// Unused — we set capabilitySet directly below.
			return "", nil
		})
		// Bypass Detect() to inject a specific capability set for tests.
		// We expose the CapabilitySet through the public Capabilities() method;
		// to seed it deterministically we run a real Detect() with a controlled
		// lookup, then swap in a pre-built set via direct field assignment.
		d.capabilitySet = buildCapabilitySetFromSlice(caps)
	}
	s := &Server{daemon: d}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/capabilities", s.method(http.MethodGet, s.handleCapabilities))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// buildCapabilitySetFromSlice returns a CapabilitySet pre-loaded with the
// supplied capabilities without hitting the real filesystem probe.
func buildCapabilitySetFromSlice(caps []internaldaemon.SubstrateCapability) *internaldaemon.CapabilitySet {
	// Build a lookup that claims to find every binary needed to produce
	// exactly the given capability set. We achieve this by running Detect
	// with an injected lookup that routes the probe deterministically.
	kinds := make(map[internaldaemon.SubstrateKind]bool, len(caps))
	for _, c := range caps {
		kinds[c.Kind] = true
	}
	lookup := func(name string) (string, error) {
		switch name {
		case "node":
			if kinds[internaldaemon.KindNPM] {
				return "/usr/local/bin/node", nil
			}
		case "python3":
			if kinds[internaldaemon.KindPythonPip] {
				return "/usr/bin/python3", nil
			}
		}
		return "", errNotFound
	}
	cs := internaldaemon.NewCapabilitySet()
	cs.Detect(lookup)
	return cs
}

var errNotFound = &notFoundError{}

type notFoundError struct{}

func (e *notFoundError) Error() string { return "not found" }

func TestHandleCapabilities_NoCapabilitiesSet(t *testing.T) {
	// capabilitySet is nil (before daemon.Start).
	srv := newCapabilitiesTestServer(t, nil)
	res, err := http.Get(srv.URL + "/api/daemon/capabilities")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	var resp CapabilitiesResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Provides == nil {
		t.Error("Provides must not be nil — want empty slice")
	}
	if len(resp.Provides) != 0 {
		t.Errorf("Provides = %+v, want empty", resp.Provides)
	}
	if resp.Timestamp == "" {
		t.Error("Timestamp must not be empty")
	}
}

func TestHandleCapabilities_WithDetectedCapabilities(t *testing.T) {
	// Seed a capability set with node on PATH.
	caps := []internaldaemon.SubstrateCapability{
		{Kind: internaldaemon.KindNative},
		{Kind: internaldaemon.KindNPM},
		{Kind: internaldaemon.KindHTTP},
		{Kind: internaldaemon.KindHostBinary},
		{Kind: internaldaemon.KindWorkarea},
		{Kind: internaldaemon.KindMCPServer},
	}
	srv := newCapabilitiesTestServer(t, caps)
	res, err := http.Get(srv.URL + "/api/daemon/capabilities")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	var resp CapabilitiesResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Provides) == 0 {
		t.Fatal("Provides is empty, expected capabilities")
	}
	kinds := make(map[string]bool, len(resp.Provides))
	for _, p := range resp.Provides {
		kinds[p.Kind] = true
	}
	for _, want := range []string{"native", "npm", "http", "host-binary", "workarea", "mcp-server"} {
		if !kinds[want] {
			t.Errorf("expected kind %q in Provides; got %v", want, resp.Provides)
		}
	}
}

func TestHandleCapabilities_RejectsNonGet(t *testing.T) {
	srv := newCapabilitiesTestServer(t, nil)
	res, err := http.Post(srv.URL+"/api/daemon/capabilities", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}

func TestHandleCapabilities_ResponseIsValidJSON(t *testing.T) {
	srv := newCapabilitiesTestServer(t, nil)
	res, err := http.Get(srv.URL + "/api/daemon/capabilities")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	ct := res.Header.Get("Content-Type")
	if ct == "" {
		t.Errorf("Content-Type header missing")
	}
	var raw map[string]any
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if _, ok := raw["provides"]; !ok {
		t.Error("JSON response missing 'provides' key")
	}
	if _, ok := raw["timestamp"]; !ok {
		t.Error("JSON response missing 'timestamp' key")
	}
}

func TestHandleCapabilities_ProvideKindStringsMatchConstant(t *testing.T) {
	// Ensure the wire shape of kind strings matches the platform v1 enum.
	expectedKinds := map[string]internaldaemon.SubstrateKind{
		"native":      internaldaemon.KindNative,
		"npm":         internaldaemon.KindNPM,
		"python-pip":  internaldaemon.KindPythonPip,
		"http":        internaldaemon.KindHTTP,
		"mcp-server":  internaldaemon.KindMCPServer,
		"host-binary": internaldaemon.KindHostBinary,
		"workarea":    internaldaemon.KindWorkarea,
	}
	for wire, constant := range expectedKinds {
		if string(constant) != wire {
			t.Errorf("SubstrateKind constant %q has wire value %q, want %q",
				constant, string(constant), wire)
		}
	}
}
