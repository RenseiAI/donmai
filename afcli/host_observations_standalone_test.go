package afcli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon"
)

const standaloneControlToken = "standalone-smoke-token"

// startLiveObservationDaemon starts a real daemon control listener on an
// ephemeral port with no quota probes and no credentials. The caller owns
// cleanup. It returns the server address and the control token.
func startLiveObservationDaemon(t *testing.T, host string) (string, func()) {
	t.Helper()
	cfg := daemon.DefaultConfig()
	cfg.Machine.ID = "standalone-smoke"
	cfg.Capacity.MaxConcurrentSessions = 1
	cfg.Projects = []daemon.ProjectConfig{{ID: "demo", Repository: "github.com/foo/bar"}}
	cfg.Orchestrator.URL = "http://127.0.0.1:1"
	cfg.Orchestrator.AuthToken = "local-stub-no-token"
	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	if err := daemon.WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	d := daemon.New(daemon.Options{
		ConfigPath: configPath, JWTPath: filepath.Join(dir, "daemon.jwt"),
		HTTPHost: host, HTTPPort: 0, SkipWizard: true, SkipRegistration: true,
		RequireControlToken: true, ControlToken: standaloneControlToken,
		SpawnerOptions: daemon.SpawnerOptions{WorktreeParentDir: t.TempDir()},
	})
	if err := d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	srv := daemon.NewServer(d)
	if _, err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_ = srv.Shutdown(context.Background())
		_ = d.Stop(context.Background())
	}
	return srv.Addr(), cleanup
}

// TestHostObservationsLiveDaemon drives the shipped CLI surface against a
// real daemon listener: the cached read, an authenticated rescan, and the
// immediate second rescan the one-a-minute gate refuses.
func TestHostObservationsLiveDaemon(t *testing.T) {
	addr, cleanup := startLiveObservationDaemon(t, "127.0.0.1")
	defer cleanup()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	factory := func(cfg afclient.DaemonConfig) hostObservationsClient {
		cfg.ControlToken = standaloneControlToken
		return afclient.NewDaemonClient(cfg)
	}

	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := newHostObservationsCmd(factory)
		var out bytes.Buffer
		cmd.SetOut(&out)
		// Dial the ephemeral listener explicitly: the defaults would hit
		// the well-known port, never this test's daemon.
		full := append([]string{"--host", host, "--port", portStr}, args...)
		cmd.SetArgs(full)
		err := cmd.Execute()
		return out.String(), err
	}

	if out, err := run("--json"); err != nil {
		t.Fatalf("cached read: %v", err)
	} else if !strings.Contains(out, `"full": true`) || !strings.Contains(out, "subscription-login") {
		t.Fatalf("cached read lacks a full subscription-login report:\n%s", out)
	}

	if out, err := run("--rescan", "--json"); err != nil {
		t.Fatalf("rescan: %v", err)
	} else if !strings.Contains(out, `"full": true`) {
		t.Fatalf("rescan lacks a full report:\n%s", out)
	}

	if _, err := run("--rescan"); err == nil {
		t.Fatal("second rescan succeeded, want the scan_rate_limited refusal")
	} else if !errors.Is(err, afclient.ErrRateLimited) && !strings.Contains(err.Error(), "rate limited") && !strings.Contains(err.Error(), "scan_rate_limited") {
		t.Fatalf("second rescan err = %v, want the rate-limited refusal", err)
	}
}

// TestHostObservationsLiveDaemonIPv6 proves a raw `--host ::1` reaches a
// daemon bound to the IPv6 loopback, end to end through BaseURL and HTTP.
func TestHostObservationsLiveDaemonIPv6(t *testing.T) {
	addr, cleanup := startLiveObservationDaemon(t, "::1")
	defer cleanup()
	if !strings.HasPrefix(addr, "[::1]:") {
		t.Fatalf("addr = %q, want an IPv6 loopback bind", addr)
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	factory := func(cfg afclient.DaemonConfig) hostObservationsClient {
		cfg.ControlToken = standaloneControlToken
		return afclient.NewDaemonClient(cfg)
	}
	cmd := newHostObservationsCmd(factory)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--host", "::1", "--port", portStr, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--host ::1: %v", err)
	}
	if !strings.Contains(out.String(), `"full": true`) {
		t.Fatalf("IPv6 read lacks a full report:\n%s", out.String())
	}
}
