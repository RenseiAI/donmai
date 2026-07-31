package afcli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/runtime/codeintelhost"
	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
)

// ── newCodeHostCmd wiring and flag defaults ─────────────────────────────────

// TestCodeHostCmdWiredUnderCode confirms `donmai code host` surfaces as a
// subcommand of the existing `code` group (afcli/code.go's newCodeCmd),
// rather than as a new top-level command.
func TestCodeHostCmdWiredUnderCode(t *testing.T) {
	t.Parallel()
	code := newCodeCmd(Config{})
	host, _, err := code.Find([]string{"host"})
	if err != nil {
		t.Fatalf("code.Find(host) error = %v", err)
	}
	if host.Use != "host" {
		t.Errorf("found command Use = %q, want %q", host.Use, "host")
	}
}

// TestNewCodeHostCmdFlagDefaults pins the non-required flags' default
// values so a future edit can't silently change host behavior.
func TestNewCodeHostCmdFlagDefaults(t *testing.T) {
	t.Parallel()
	cmd := newCodeHostCmd(Config{})
	cases := map[string]string{
		"listen":               "127.0.0.1:8085",
		"max-workareas":        "8",
		"max-concurrent-calls": "16",
		"idle-ttl":             "30m0s",
		"request-timeout":      "2m0s",
		"shutdown-timeout":     "1m0s",
		"warm-timeout":         "5m0s",
	}
	for name, want := range cases {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("flag %q not registered", name)
			continue
		}
		if f.DefValue != want {
			t.Errorf("flag %q default = %q, want %q", name, f.DefValue, want)
		}
	}
}

// TestNewCodeHostCmdNoDefaultForIdentityFlags confirms --issuer and
// --audience carry no default: this is a generic, deployment-configured
// host, not one hardcoded to any specific platform identity.
func TestNewCodeHostCmdNoDefaultForIdentityFlags(t *testing.T) {
	t.Parallel()
	cmd := newCodeHostCmd(Config{})
	for _, name := range []string{"issuer", "audience", "catalog", "state-dir"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("flag %q not registered", name)
		}
		if f.DefValue != "" {
			t.Errorf("flag %q default = %q, want empty (no default)", name, f.DefValue)
		}
	}
}

// TestNewCodeHostCmdRequiredFlags exercises cobra's MarkFlagRequired wiring:
// omitting any one of catalog/state-dir/issuer/audience must fail
// validation before RunE ever runs (so it fails long before touching the
// filesystem or network).
func TestNewCodeHostCmdRequiredFlags(t *testing.T) {
	t.Parallel()
	full := map[string]string{
		"catalog":   "/tmp/does-not-matter.yaml",
		"state-dir": "/tmp/does-not-matter-state",
		"issuer":    "rensei-platform",
		"audience":  "rensei-code-intel-host",
	}
	cases := []struct {
		omit string
	}{
		{"catalog"},
		{"state-dir"},
		{"issuer"},
		{"audience"},
	}
	for _, tc := range cases {
		t.Run("missing "+tc.omit, func(t *testing.T) {
			t.Parallel()
			var args []string
			for name, val := range full {
				if name == tc.omit {
					continue
				}
				args = append(args, "--"+name, val)
			}
			cmd := newCodeHostCmd(Config{})
			cmd.SetArgs(args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("Execute() with %s omitted: error = nil, want required-flag error", tc.omit)
			}
			if !strings.Contains(err.Error(), tc.omit) {
				t.Errorf("Execute() error = %q, want it to mention missing flag %q", err.Error(), tc.omit)
			}
		})
	}
}

// ── lookupCodeHostJWTSecret ──────────────────────────────────────────────────

func TestLookupCodeHostJWTSecret(t *testing.T) {
	cases := []struct {
		name       string
		primary    string
		fallback   string
		wantSecret string
		wantErr    bool
	}{
		{"primary set", "prod-secret", "", "prod-secret", false},
		{"fallback used when primary unset", "", "dev-secret", "dev-secret", false},
		{"primary wins over fallback", "prod-secret", "dev-secret", "prod-secret", false},
		{"whitespace-only primary treated as unset", "   ", "dev-secret", "dev-secret", false},
		{"both unset is an error", "", "", "", true},
		{"both whitespace-only is an error", "  ", "\t", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv forbids t.Parallel in the same subtest.
			t.Setenv("CODE_INTEL_HOST_JWT_SECRET", tc.primary)
			t.Setenv("M2M_JWT_SECRET", tc.fallback)

			got, err := lookupCodeHostJWTSecret()
			if (err != nil) != tc.wantErr {
				t.Fatalf("lookupCodeHostJWTSecret() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.wantSecret {
				t.Errorf("lookupCodeHostJWTSecret() = %q, want %q", got, tc.wantSecret)
			}
		})
	}
}

// ── runCodeHost config-construction error paths ─────────────────────────────
//
// These exercise runCodeHost's build-up sequence (secret -> catalog -> pool
// -> verifier -> handler) up to (but never past) the point where it would
// start listening, so they run fast and never touch the network.

func newBareCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "host"}
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	return cmd
}

func TestRunCodeHostPropagatesSecretLookupError(t *testing.T) {
	t.Setenv("CODE_INTEL_HOST_JWT_SECRET", "")
	t.Setenv("M2M_JWT_SECRET", "")

	cmd := newBareCmd(t)
	err := runCodeHost(cmd, codeHostOptions{
		catalogPath: filepath.Join(t.TempDir(), "unused.yaml"),
		stateDir:    t.TempDir(),
		issuer:      "rensei-platform",
		audience:    "rensei-code-intel-host",
	})
	if err == nil {
		t.Fatal("runCodeHost() error = nil, want a JWT-secret error")
	}
	if !strings.Contains(err.Error(), "JWT signing secret") {
		t.Errorf("runCodeHost() error = %q, want it to mention the missing JWT signing secret", err.Error())
	}
}

func TestRunCodeHostPropagatesCatalogLoadError(t *testing.T) {
	t.Setenv("CODE_INTEL_HOST_JWT_SECRET", "test-secret")

	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	cmd := newBareCmd(t)
	err := runCodeHost(cmd, codeHostOptions{
		catalogPath: missing,
		stateDir:    t.TempDir(),
		issuer:      "rensei-platform",
		audience:    "rensei-code-intel-host",
	})
	if err == nil {
		t.Fatal("runCodeHost() error = nil, want a catalog-load error")
	}
	if !strings.Contains(err.Error(), "load catalog") {
		t.Errorf("runCodeHost() error = %q, want it to mention catalog load failure", err.Error())
	}
}

func TestRunCodeHostPropagatesVerifierValidationError(t *testing.T) {
	t.Setenv("CODE_INTEL_HOST_JWT_SECRET", "test-secret")

	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(catalogPath, []byte("repositories: []\n"), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}

	cmd := newBareCmd(t)
	err := runCodeHost(cmd, codeHostOptions{
		catalogPath:        catalogPath,
		stateDir:           t.TempDir(),
		issuer:             "", // invalid: no default, and required at the Verifier layer too
		audience:           "rensei-code-intel-host",
		maxWorkareas:       1,
		maxConcurrentCalls: 1,
	})
	if err == nil {
		t.Fatal("runCodeHost() error = nil, want a verifier build error for an empty issuer")
	}
}

func TestRunCodeHostPropagatesPoolValidationError(t *testing.T) {
	t.Setenv("CODE_INTEL_HOST_JWT_SECRET", "test-secret")

	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(catalogPath, []byte("repositories: []\n"), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}

	cmd := newBareCmd(t)
	err := runCodeHost(cmd, codeHostOptions{
		catalogPath:  catalogPath,
		stateDir:     t.TempDir(),
		issuer:       "rensei-platform",
		audience:     "rensei-code-intel-host",
		maxWorkareas: 0, // invalid: NewPool requires a positive bound
	})
	if err == nil {
		t.Fatal("runCodeHost() error = nil, want a pool build error for MaxWorkareas=0")
	}
	if !strings.Contains(err.Error(), "build pool") {
		t.Errorf("runCodeHost() error = %q, want it to mention pool construction failure", err.Error())
	}
}

// TestRunCodeHostBuildsAndDrainsOnContextCancel exercises the full happy
// construction path (valid catalog, secret, issuer/audience, pool sizing)
// through to actually listening, then cancels the command context in place
// of a SIGTERM/SIGINT and asserts runCodeHost returns promptly instead of
// hanging — the graceful-drain path with nothing in flight to drain.
func TestRunCodeHostBuildsAndDrainsOnContextCancel(t *testing.T) {
	t.Setenv("CODE_INTEL_HOST_JWT_SECRET", "test-secret")

	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(catalogPath, []byte("repositories: []\n"), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := &cobra.Command{Use: "host"}
	cmd.SetContext(ctx)
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	done := make(chan error, 1)
	go func() {
		done <- runCodeHost(cmd, codeHostOptions{
			catalogPath:        catalogPath,
			stateDir:           t.TempDir(),
			issuer:             "rensei-platform",
			audience:           "rensei-code-intel-host",
			listen:             "127.0.0.1:0",
			maxWorkareas:       1,
			maxConcurrentCalls: 1,
			shutdownTimeout:    5 * time.Second,
			warmTimeout:        time.Second,
		})
	}()

	// Give the listener goroutine a moment to start, then request a drain the
	// same way SIGTERM/SIGINT would.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runCodeHost() error = %v, want nil after a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runCodeHost() did not return after context cancellation; shutdown appears to hang")
	}
}

// ── drainCodeHost shutdown-truthfulness (Task 6) ────────────────────────────
//
// These exercise drainCodeHost directly against a real codeintelhost.Pool
// (via the exported Factory/ToolCaller/io.Closer interfaces) so a held lease
// can deterministically force pool.Close to time out, without needing a real
// Git checkout or a listening HTTP server.

type fakeCodeHostCaller struct{}

func (fakeCodeHostCaller) Call(context.Context, string, json.RawMessage) (mcpserver.ToolResult, error) {
	return mcpserver.ToolResult{}, nil
}

func (fakeCodeHostCaller) WaitReady(context.Context) error { return nil }

type fakeCodeHostCloser struct{}

func (fakeCodeHostCloser) Close() error { return nil }

type fakeCodeHostFactory struct{}

func (fakeCodeHostFactory) Create(context.Context, codeintelhost.Binding) (codeintelhost.ToolCaller, io.Closer, error) {
	return fakeCodeHostCaller{}, fakeCodeHostCloser{}, nil
}

// TestDrainCodeHostReturnsErrorWhenPoolNeverDrains proves runCodeHost's
// extracted drain step returns a non-nil error — never silently succeeds —
// when a lease is still held past shutCtx's deadline, mirroring the
// codeintelhost.Pool.Close timeout the http.Server.Shutdown call is meant to
// have already drained down to zero.
func TestDrainCodeHostReturnsErrorWhenPoolNeverDrains(t *testing.T) {
	t.Parallel()
	pool, err := codeintelhost.NewPool(fakeCodeHostFactory{}, codeintelhost.PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	binding := codeintelhost.Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "repo-1",
		RevisionKind: codeintelhost.RevisionResolvedRef, Revision: strings.Repeat("a", 40),
	}
	lease, err := pool.Acquire(context.Background(), binding)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lease.Release()

	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second} // never ListenAndServe'd: Shutdown() is a same-process no-op no-error.
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := drainCodeHost(shutCtx, srv, pool); err == nil {
		t.Error("drainCodeHost() error = nil, want a non-nil drain error while the lease is never released")
	}
}

// TestDrainCodeHostReturnsNilOnCleanDrain is the sanity-check symmetry case:
// with nothing held, drainCodeHost must report a clean, nil-error drain.
func TestDrainCodeHostReturnsNilOnCleanDrain(t *testing.T) {
	t.Parallel()
	pool, err := codeintelhost.NewPool(fakeCodeHostFactory{}, codeintelhost.PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := drainCodeHost(shutCtx, srv, pool); err != nil {
		t.Errorf("drainCodeHost() error = %v, want nil for a clean drain with nothing in flight", err)
	}
}
