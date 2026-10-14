package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/internal/usageping"
)

func testUsagePingConfig(endpoint string) *Config {
	cfg := DefaultConfig()
	cfg.UsagePing = UsagePingConfig{
		Enabled:        true,
		InstallationID: "0123456789abcdef0123456789abcdef",
		Endpoint:       endpoint,
	}
	return cfg
}

func TestUsagePingLoop_DisabledSendsNothing(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	loop := &usagePingLoop{
		statePath: filepath.Join(t.TempDir(), "usage-ping.json"),
		version:   func() string { return "v0.0-test" },
		config:    DefaultConfig, // disabled
		sender:    func(endpoint string) *usageping.Sender { return &usageping.Sender{Endpoint: endpoint, AllowHTTP: true} },
		now:       time.Now,
	}
	loop.check(context.Background())
	if calls.Load() != 0 {
		t.Fatal("disabled ping sent a request")
	}
	// No state file written when nothing was due to send.
	if _, err := os.Stat(loop.statePath); !os.IsNotExist(err) {
		t.Fatal("disabled ping wrote state")
	}
}

func TestUsagePingLoop_SendsOncePerWeek(t *testing.T) {
	var calls atomic.Int32
	var bodies atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		if p, err := usageping.Decode(buf[:n]); err != nil || p.Version != "v0.0-test" {
			t.Errorf("bad payload: %+v %v", p, err)
		} else {
			bodies.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testUsagePingConfig(srv.URL)
	loop := &usagePingLoop{
		statePath: filepath.Join(t.TempDir(), "usage-ping.json"),
		version:   func() string { return "v0.0-test" },
		config:    func() *Config { return cfg },
		sender:    func(endpoint string) *usageping.Sender { return &usageping.Sender{Endpoint: endpoint, AllowHTTP: true} },
		now:       time.Now,
	}
	loop.check(context.Background())
	if calls.Load() != 1 || bodies.Load() != 1 {
		t.Fatalf("first check: calls=%d bodies=%d, want 1/1", calls.Load(), bodies.Load())
	}
	// Second check within the week sends nothing (weekly maximum).
	loop.check(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("second check sent again: calls=%d", calls.Load())
	}
	// State persisted before transmission: the file exists with a time.
	state, err := loop.loadState()
	if err != nil || state.LastAttempt.IsZero() {
		t.Fatalf("state not persisted: %+v %v", state, err)
	}
}

func TestUsagePingLoop_DisableCancelsQueuedWork(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	statePath := filepath.Join(t.TempDir(), "usage-ping.json")
	cfg := testUsagePingConfig(srv.URL)
	loop := &usagePingLoop{
		statePath: statePath,
		version:   func() string { return "v0.0-test" },
		config:    func() *Config { return cfg },
		sender:    func(endpoint string) *usageping.Sender { return &usageping.Sender{Endpoint: endpoint, AllowHTTP: true} },
		now:       time.Now,
	}
	loop.check(context.Background())
	if calls.Load() != 1 {
		t.Fatal("first check did not send")
	}
	// Operator disables: future checks send nothing even though the
	// weekly window has not elapsed, and cancel clears the record.
	cfg.UsagePing.Enabled = false
	cancelUsagePing(statePath)
	loop.check(context.Background())
	if calls.Load() != 1 {
		t.Fatal("disabled ping sent after opt-out")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("cancel did not clear state")
	}
}

func TestUsagePingLoop_FailureStillPacesRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := testUsagePingConfig(srv.URL)
	loop := &usagePingLoop{
		statePath: filepath.Join(t.TempDir(), "usage-ping.json"),
		version:   func() string { return "v0.0-test" },
		config:    func() *Config { return cfg },
		sender:    func(endpoint string) *usageping.Sender { return &usageping.Sender{Endpoint: endpoint, AllowHTTP: true} },
		now:       time.Now,
	}
	loop.check(context.Background())
	loop.check(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("failed attempt retried within the week: calls=%d", calls.Load())
	}
}

func TestUsagePingLoop_NoEndpointSendsNothing(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UsagePing = UsagePingConfig{Enabled: true, InstallationID: "0123456789abcdef0123456789abcdef"}
	loop := &usagePingLoop{
		statePath: filepath.Join(t.TempDir(), "usage-ping.json"),
		version:   func() string { return "v0.0-test" },
		config:    func() *Config { return cfg },
		now:       time.Now,
	}
	loop.check(context.Background())
	if _, err := os.Stat(loop.statePath); !os.IsNotExist(err) {
		t.Fatal("endpoint-less consent wrote state")
	}
}
