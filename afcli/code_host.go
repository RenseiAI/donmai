package afcli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/runtime/codeintelhost"
)

// defaultCodeHostIdleReapInterval bounds how often `code host` re-checks the
// pool for idle-TTL-eligible workareas when --idle-ttl > 0.
const defaultCodeHostIdleReapInterval = time.Minute

// codeHostOptions collects newCodeHostCmd's parsed flags for runCodeHost.
type codeHostOptions struct {
	listen             string
	catalogPath        string
	stateDir           string
	issuer             string
	audience           string
	maxWorkareas       int
	maxConcurrentCalls int
	idleTTL            time.Duration
	requestTimeout     time.Duration
	shutdownTimeout    time.Duration
	warmTimeout        time.Duration
}

// newCodeHostCmd constructs `donmai code host`: the long-lived warm-host
// process mode that serves the frozen six-tool code-intelligence contract
// over HTTP (POST /v1/tools/call) instead of the stdio transport `donmai mcp
// code-intel` uses, backed by a bounded pool of persistent Git-backed
// workareas. See runtime/codeintelhost's package doc for the full contract.
func newCodeHostCmd(_ Config) *cobra.Command {
	var opts codeHostOptions

	cmd := &cobra.Command{
		Use:   "host",
		Short: "Run the code-intelligence warm host: HTTP POST /v1/tools/call over a bounded pool of Git-backed workareas",
		Long: `Run the code-intelligence warm host: a long-lived process exposing the
frozen six-tool code-intelligence contract over HTTP (POST /v1/tools/call)
instead of the stdio transport donmai mcp code-intel uses.

Each request supplies a binding (orgId, projectId, repositoryPathId,
revisionKind, revision) identifying an exact repository revision. The host
resolves repositoryPathId through --catalog, maintains a persistent bare Git
mirror plus one detached worktree per exact binding under --state-dir, and
serves tool calls from a bounded LRU pool of warm workareas (single-flight
warming, ref-counted leases, idle-TTL eviction, fail-fast at capacity).

Every request must carry a Bearer JWT (HS256) whose claims match the request
body's invocationId and binding exactly. The signing secret is read from the
CODE_INTEL_HOST_JWT_SECRET environment variable, falling back to
M2M_JWT_SECRET only as a documented development convenience — never from a
flag, so it never appears in argv or a process listing. --issuer and
--audience have no default: this is a generic, deployment-configured host,
not tied to any specific platform identity.

GET /healthz reports process liveness; GET /readyz reports 503 while the pool
is draining/closed. SIGTERM/SIGINT triggers a graceful drain: the HTTP server
stops admitting new requests and waits for in-flight tool calls to finish
(bounded by --shutdown-timeout), then the listener closes without evicting
resident workareas — the persistent volume's warm cache survives a restart.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCodeHost(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.listen, "listen", "127.0.0.1:8085", "HTTP listen address")
	cmd.Flags().StringVar(&opts.catalogPath, "catalog", "",
		"Path to the repository catalog YAML (repositories[]: id, projectId, source, git.credentialHelper/sshKey) (required)")
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", "",
		"Absolute path to the persistent state directory for Git mirrors and workareas (required)")
	cmd.Flags().StringVar(&opts.issuer, "issuer", "", "Required JWT issuer this host verifies (no default)")
	cmd.Flags().StringVar(&opts.audience, "audience", "", "Required JWT audience this host verifies (no default)")
	cmd.Flags().IntVar(&opts.maxWorkareas, "max-workareas", 8, "Maximum resident workareas held by the pool")
	cmd.Flags().IntVar(&opts.maxConcurrentCalls, "max-concurrent-calls", 16, "Maximum in-flight tool calls admitted concurrently")
	cmd.Flags().DurationVar(&opts.idleTTL, "idle-ttl", 30*time.Minute,
		"Idle time before an unleased workarea becomes eligible for TTL eviction (0 disables TTL reaping)")
	cmd.Flags().DurationVar(&opts.requestTimeout, "request-timeout", 2*time.Minute,
		"Per-request timeout bounding acquisition wait plus tool dispatch (0 = no per-request timeout)")
	cmd.Flags().DurationVar(&opts.shutdownTimeout, "shutdown-timeout", 60*time.Second,
		"Maximum time to wait for in-flight leases to drain on shutdown")
	cmd.Flags().DurationVar(&opts.warmTimeout, "warm-timeout", 5*time.Minute,
		"Maximum time allowed for a single-flight workarea warm (clone/fetch/checkout) to complete")
	_ = cmd.MarkFlagRequired("catalog")
	_ = cmd.MarkFlagRequired("state-dir")
	_ = cmd.MarkFlagRequired("issuer")
	_ = cmd.MarkFlagRequired("audience")

	return cmd
}

// codeHostJWTSecretEnvVars is the ordered secret lookup: the host's own
// dedicated secret first, falling back to the shared machine-to-machine
// secret only as a documented development convenience. Both names are
// blocklisted from agent/git child-process environments
// (internal/credentials.AgentEnvBlocklist) — the secret only ever lives in
// this process's own env, never passed as a flag (which would leak into
// argv/process listings) and never forwarded to a spawned Git subprocess.
var codeHostJWTSecretEnvVars = []string{"CODE_INTEL_HOST_JWT_SECRET", "M2M_JWT_SECRET"}

// lookupCodeHostJWTSecret resolves the HS256 signing secret from the
// process environment in codeHostJWTSecretEnvVars order.
func lookupCodeHostJWTSecret() (string, error) {
	for _, name := range codeHostJWTSecretEnvVars {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("code host: no JWT signing secret configured (set one of %s)",
		strings.Join(codeHostJWTSecretEnvVars, ", "))
}

// runCodeHost builds the catalog, pool, verifier, and HTTP handler from opts
// and serves until a SIGTERM/SIGINT, an HTTP server error, or the command
// context ends, then drains gracefully.
func runCodeHost(cmd *cobra.Command, opts codeHostOptions) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	logf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(errOut, "[code-host] "+format+"\n", args...)
	}

	secret, err := lookupCodeHostJWTSecret()
	if err != nil {
		return err
	}
	catalog, err := codeintelhost.LoadCatalog(opts.catalogPath)
	if err != nil {
		return fmt.Errorf("code host: load catalog %q: %w", opts.catalogPath, err)
	}
	logf("loaded catalog %s (%d repositories)", opts.catalogPath, catalog.Len())

	factory := &codeintelhost.GitFactory{
		Catalog:  catalog,
		StateDir: opts.stateDir,
		Logf:     logf,
	}
	pool, err := codeintelhost.NewPool(factory, codeintelhost.PoolConfig{
		MaxWorkareas: opts.maxWorkareas,
		IdleTTL:      opts.idleTTL,
		WarmTimeout:  opts.warmTimeout,
	})
	if err != nil {
		return fmt.Errorf("code host: build pool: %w", err)
	}

	verifier, err := codeintelhost.NewVerifier(codeintelhost.VerifierConfig{
		Secret:   secret,
		Issuer:   opts.issuer,
		Audience: opts.audience,
	})
	if err != nil {
		return fmt.Errorf("code host: build verifier: %w", err)
	}

	handler, err := codeintelhost.NewHandler(codeintelhost.HandlerConfig{
		Verifier:           verifier,
		Pool:               pool,
		MaxConcurrentCalls: opts.maxConcurrentCalls,
		RequestTimeout:     opts.requestTimeout,
		Logf:               logf,
	})
	if err != nil {
		return fmt.Errorf("code host: build handler: %w", err)
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	if opts.idleTTL > 0 {
		go pool.RunIdleReaper(ctx, defaultCodeHostIdleReapInterval, func(removed []codeintelhost.Binding) {
			if len(removed) > 0 {
				logf("idle-TTL reaped %d workarea(s)", len(removed))
			}
		})
	}

	srv := &http.Server{
		Addr:              opts.listen,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logf("listening on %s", opts.listen)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		_, _ = fmt.Fprintf(out, "[code-host] received %s, draining...\n", sig)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("code host: http server error: %w", err)
		}
	case <-ctx.Done():
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), opts.shutdownTimeout)
	defer shutCancel()
	if err := drainCodeHost(shutCtx, srv, pool); err != nil {
		// A drain that times out or fails is NOT a clean shutdown: an
		// in-flight tool call/lease may still be running (or the listener may
		// not have released its port), so this must surface as a non-nil
		// error — never a "stopped" success — for the caller/supervisor to
		// act on.
		return fmt.Errorf("code host: drain: %w", err)
	}
	_, _ = fmt.Fprintln(out, "[code-host] stopped")
	return nil
}

// drainCodeHost shuts down srv and closes pool, both bounded by shutCtx, and
// reports the truthful combined outcome. Both steps always run regardless of
// whether the first fails/times out — a slow HTTP shutdown must not hide a
// pool that also failed to drain — and errors.Join preserves both errors
// when both occur rather than reporting only the first.
func drainCodeHost(shutCtx context.Context, srv *http.Server, pool *codeintelhost.Pool) error {
	// http.Server.Shutdown stops admitting new connections and waits for
	// in-flight handlers (including any held pool lease) to finish before
	// returning — the drain the design calls for. pool.Close afterward is a
	// redundant bound: by construction every lease should already be zero.
	shutdownErr := srv.Shutdown(shutCtx)
	if shutdownErr != nil {
		shutdownErr = fmt.Errorf("http shutdown: %w", shutdownErr)
	}
	closeErr := pool.Close(shutCtx)
	if closeErr != nil {
		closeErr = fmt.Errorf("pool close: %w", closeErr)
	}
	return errors.Join(shutdownErr, closeErr)
}
