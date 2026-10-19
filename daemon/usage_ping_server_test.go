package daemon

import (
	"net/http"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

func TestServer_SetUsagePingEnabled(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp afclient.SetCapacityResponse
	status := requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "usagePing.enabled",
		"value": "1",
	}, &resp)
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("status=%d resp=%+v", status, resp)
	}
	if !d.Config().UsagePing.Enabled {
		t.Fatal("live config not updated")
	}
	// Disabling clears the block in memory.
	status = requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "usagePing.enabled",
		"value": "0",
	}, &resp)
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("status=%d resp=%+v", status, resp)
	}
	if got := d.Config().UsagePing; got != (UsagePingConfig{}) {
		t.Fatalf("disable kept state: %+v", got)
	}
}

func TestServer_SetUsagePingRejectsBadValue(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp afclient.SetCapacityResponse
	status := requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "usagePing.enabled",
		"value": "2",
	}, &resp)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	status = requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "usagePing.endpoint",
		"value": "http://example.invalid/usage",
	}, &resp)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	status = requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "usagePing.bogus",
		"value": "1",
	}, &resp)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestServer_SetUsagePingEndpoint(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp afclient.SetCapacityResponse
	status := requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "usagePing.endpoint",
		"value": "https://example.invalid/usage",
	}, &resp)
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("status=%d resp=%+v", status, resp)
	}
	if got := d.Config().UsagePing.Endpoint; got != "https://example.invalid/usage" {
		t.Fatalf("endpoint = %q", got)
	}
}
