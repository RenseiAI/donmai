package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RenseiAI/donmai/gateway"
)

func newGatewayTestServer(t *testing.T, d *Daemon) *httptest.Server {
	t.Helper()
	s := &Server{daemon: d}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/gateway", s.method(http.MethodGet, s.handleGateway))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func getGatewayStatus(t *testing.T, url string) gateway.Status {
	t.Helper()
	res, err := http.Get(url + "/api/daemon/gateway")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var st gateway.Status
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return st
}

func TestHandleGateway_DisabledReportsHonestly(t *testing.T) {
	// No gateway wired — the surface still answers with enabled:false and the
	// supported-surface list (the honesty-marker posture).
	d := &Daemon{opts: Options{}}
	srv := newGatewayTestServer(t, d)
	st := getGatewayStatus(t, srv.URL)
	if st.Enabled {
		t.Error("disabled gateway should report enabled:false")
	}
	if len(st.Surfaces) == 0 {
		t.Error("disabled gateway should still list supported surfaces")
	}
}

func TestHandleGateway_EnabledReportsRunning(t *testing.T) {
	g := gateway.New(gateway.Options{})
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	t.Cleanup(func() { _ = g.Stop(context.Background()) })

	d := &Daemon{opts: Options{}, gateway: g, gatewayLedger: "/x/.donmai/gateway/cost-events.jsonl"}
	srv := newGatewayTestServer(t, d)
	st := getGatewayStatus(t, srv.URL)
	if !st.Enabled {
		t.Fatal("running gateway should report enabled:true")
	}
	if st.Addr == "" {
		t.Error("running gateway should report its loopback addr")
	}
	if st.LedgerPath == "" {
		t.Error("running gateway should report the cost-ledger path")
	}
}

func TestHandleGateway_MethodNotAllowed(t *testing.T) {
	d := &Daemon{opts: Options{}}
	srv := newGatewayTestServer(t, d)
	res, err := http.Post(srv.URL+"/api/daemon/gateway", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", res.StatusCode)
	}
}
