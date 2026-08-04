package afcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	afcreds "github.com/RenseiAI/donmai/afcli/credentials"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/statehome"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// logRotateCheckInterval is how often a long-lived `host run` process
// re-checks the launchd-managed log files for rotation.
const logRotateCheckInterval = 6 * time.Hour

type terminalRuntimeCredentialSource interface {
	RuntimeCredentials() (workerID, runtimeToken string)
}

// terminalReceiverAuthorization resolves the daemon's current ephemeral runtime
// token for each outbox send. The token is returned only as an HTTP header value
// and is never written into the receiver registry or immutable status body.
func terminalReceiverAuthorization(source terminalRuntimeCredentialSource) workarea.ReceiverAuthorizationResolver {
	return func(ctx context.Context, _ string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		workerID, token := source.RuntimeCredentials()
		if strings.TrimSpace(workerID) == "" && strings.TrimSpace(token) == "" {
			return "", errors.New("daemon runtime credentials are not available yet")
		}
		if strings.TrimSpace(token) == "" {
			return "", nil
		}
		return "Bearer " + token, nil
	}
}

// rotateDaemonLogs size-checks the launchd-managed daemon log files
// (daemon.log / daemon-error.log) and rotates any that exceed
// daemon.DefaultLogRotateMaxBytes. Best-effort: failures are reported to
// errOut and otherwise ignored — rotation must never block the daemon.
func rotateDaemonLogs(errOut io.Writer) {
	for _, path := range []string{statehome.LogPath(), statehome.ErrorLogPath()} {
		rotated, err := daemon.RotateLogIfOver(path, daemon.DefaultLogRotateMaxBytes)
		if err != nil {
			_, _ = fmt.Fprintf(errOut, "[daemon] log rotation failed for %s: %v\n", path, err)
			continue
		}
		if rotated {
			_, _ = fmt.Fprintf(errOut, "[daemon] rotated %s (previous generation at %s.1)\n", path, path)
		}
	}
}

// newDaemonRunCmd constructs the `host run` subcommand. This is the
// long-running entry point registered by the launchd plist / systemd unit.
//
// The installer registers `<host-binary> host run` as the service entrypoint; this
// command is what runs on those service managers.
func newDaemonRunCmd(hostVersion string) *cobra.Command {
	var (
		configPath      string
		jwtPath         string
		host            string
		port            int
		skipWizard      bool
		standaloneCreds string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon (long-running entry point)",
		Long: "Start the long-running rensei daemon process.\n\n" +
			"This is the service entry point registered by `host install` —\n" +
			"the launchd plist (macOS) and systemd unit (Linux) call this\n" +
			"subcommand. It loads ~/.donmai/daemon.yaml, registers with the\n" +
			"orchestrator, starts the heartbeat loop, and serves the local\n" +
			"control HTTP API on 127.0.0.1:7734.\n\n" +
			"Run interactively for development with `--skip-wizard` to bypass\n" +
			"the first-run setup. SIGTERM / SIGINT triggers a graceful drain.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Rotate the launchd-managed log files before anything writes
			// to them this run, then re-check periodically for long-lived
			// processes. launchd appends to daemon.log / daemon-error.log
			// forever with no rotation of its own; without this they grow
			// unbounded (tens of MB of routine token-refresh and poll
			// chatter). Best-effort: rotation failures must never block
			// daemon start. (systemd installs log to the journal; the
			// files don't exist there and this is a no-op.)
			rotateDaemonLogs(cmd.ErrOrStderr())

			// Build the in-process AgentRuntime registry once at daemon
			// startup so the local /api/daemon/providers* HTTP surface
			// can introspect it. The same registry shape is rebuilt
			// per-session by `donmai agent run`; here we surface it for
			// operator queries. Probes that fail (e.g. ollama not
			// running) emit WARN logs but do not block daemon start.
			// Wave 9 / A1.
			providerReg := BuildAgentRunRegistry(slog.Default())
			// Substitute the well-known DefaultHTTPPort here when the
			// operator did not pass `--port`. Leaving zero through to
			// daemon.New would bind an ephemeral port — correct for
			// tests but wrong for the service-managed `donmai host run`
			// entry point operators reach via launchd / systemd, which
			// must bind 7734 so the `donmai host …` CLI surface (and the
			// installed plist health checks) can find the daemon.
			// (Wave 12 / C3 — port-7734 default lives in the cobra
			// layer, not the runtime.)
			if port == 0 {
				port = daemon.DefaultHTTPPort
			}

			// Standalone-creds wiring (Lane K). When `af` is running
			// outside of rensei-tui (no daemon credential pipeline,
			// no platform session), agents inherit credentials from
			// the af process per the precedence:
			//   1. existing process env
			//   2. ${gitRoot}/.env.local (parsed once at startup)
			//
			// Auto-detect mode: absence of DONMAI_DAEMON_JWT means we
			// are NOT being driven by rensei-tui's credential socket
			// and should seed env from the local source. Operators
			// can pin the mode via --standalone-creds=on|off.
			errOut := cmd.ErrOrStderr()
			localSource, lsErr := afcreds.LoadLocalSource(resolveStandaloneGitRoot())
			if lsErr != nil {
				_, _ = fmt.Fprintf(errOut, "[creds] LoadLocalSource: %v (continuing with process env only)\n", lsErr)
				localSource = nil
			}
			mode := resolveStandaloneCredsMode(standaloneCreds, os.Getenv("DONMAI_DAEMON_JWT") != "")
			spawnerOpts := daemon.SpawnerOptions{}
			if mode && localSource != nil {
				spawnerOpts.BaseEnv = localSource.MergeIntoBaseEnv(nil)
				_, _ = fmt.Fprintf(
					errOut,
					"[creds] standalone mode active — merging process env + %s into spawner BaseEnv\n",
					displayEnvLocalPath(localSource),
				)
			} else if localSource != nil {
				// Diagnostic-only: load but don't seed. The daemon's
				// own credential pipeline (rensei-tui driven) owns
				// agent env in this mode.
				slog.Debug(
					"standalone creds disabled — LocalSource loaded read-only",
					"envLocalPath", localSource.EnvLocalPath(),
					"fileEnvKeyCount", len(localSource.FileEnvKeys()),
				)
			}

			d := daemon.New(daemon.Options{
				ConfigPath:       configPath,
				JWTPath:          jwtPath,
				HTTPHost:         host,
				HTTPPort:         port,
				SkipWizard:       skipWizard,
				ProviderRegistry: runner.NewProviderView(providerReg),
				SpawnerOptions:   spawnerOpts,
				Version:          hostVersion,
			})
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			// Reconcile terminal authorities before the daemon admits work. Receiver
			// endpoints are resolved from the separate registry on every replay;
			// no bearer is persisted by this public implementation.
			leaseManager, err := worktree.NewManager(worktree.Options{
				ParentDir: statepath.Resolve("worktrees", "/tmp/.donmai/worktrees"),
				Logger:    slog.Default(),
			})
			if err != nil {
				return fmt.Errorf("terminal lease recovery authority: %w", err)
			}
			leaseSender := leaseManager.TerminalStatusHTTPSender(
				&http.Client{Timeout: workarea.DefaultTerminalResultReplayTimeout},
				terminalReceiverAuthorization(d),
			)
			go leaseManager.RunTerminalResultReplayer(ctx, workarea.TerminalResultReplayOptions{
				OnError: func(err error) { slog.Warn("terminal status replay failed", "err", err) },
			}, leaseSender)
			go leaseManager.RunTerminalLeaseReaper(ctx, workarea.ReaperOptions{
				OnError: func(err error) { slog.Warn("terminal lease reaping failed", "err", err) },
			})
			go func() {
				cleanup := func() {
					if _, err := leaseManager.CleanupTerminalQuarantines(ctx, workarea.SchedulerOptions{
						BatchSize: workarea.DefaultReaperBatchSize, Concurrency: workarea.DefaultReaperConcurrency,
						AttemptTimeout: workarea.DefaultReleaseAttemptTimeout,
					}); err != nil && ctx.Err() == nil {
						slog.Warn("terminal quarantine cleanup failed", "err", err)
					}
				}
				cleanup()
				ticker := time.NewTicker(workarea.DefaultReaperInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						cleanup()
					}
				}
			}()

			// Periodic rotation re-check for long-lived daemon processes —
			// the boot-time rotation above only helps across restarts.
			go func() {
				tick := time.NewTicker(logRotateCheckInterval)
				defer tick.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
						rotateDaemonLogs(cmd.ErrOrStderr())
					}
				}
			}()

			out := cmd.OutOrStdout()
			if err := d.Start(ctx); err != nil {
				return fmt.Errorf("daemon start: %w", err)
			}
			_, _ = fmt.Fprintf(out, "[daemon] state -> %s\n", d.State())
			// Print the worker id only after Start() completes registration so
			// the value is the live, platform-assigned id (or a clearly-marked
			// stub fallback). Previously the log fired with a stub
			// WorkerID like "worker-<host>-stub" before any real registration
			// had a chance to run, misleading operators into thinking the
			// daemon was registered when it was not.
			if line := formatStartupWorkerLine(d.WorkerID()); line != "" {
				_, _ = fmt.Fprintln(out, line)
			}

			srv := daemon.NewServer(d)
			errCh, err := srv.Start()
			if err != nil {
				return fmt.Errorf("daemon HTTP server start: %w", err)
			}
			_, _ = fmt.Fprintf(out, "[daemon] http listening on %s\n", srv.Addr())

			// Wait for signal or HTTP error.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

			select {
			case sig := <-sigCh:
				_, _ = fmt.Fprintf(out, "[daemon] received %s, draining...\n", sig)
			case err := <-errCh:
				if err != nil {
					_, _ = fmt.Fprintf(errOut, "[daemon] http server error: %v\n", err)
				}
			case <-d.Done():
				_, _ = fmt.Fprintln(out, "[daemon] stop requested")
			}

			shutCtx, shutCancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer shutCancel()
			_ = srv.Shutdown(shutCtx)
			if err := d.Stop(shutCtx); err != nil {
				return fmt.Errorf("daemon stop: %w", err)
			}
			_, _ = fmt.Fprintln(out, "[daemon] stopped")
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "Path to daemon.yaml (default: ~/.donmai/daemon.yaml)")
	cmd.Flags().StringVar(&jwtPath, "jwt-path", "", "Path to cached JWT (default: ~/.donmai/daemon.jwt)")
	cmd.Flags().StringVar(&host, "host", "", "HTTP bind host (default: 127.0.0.1)")
	cmd.Flags().IntVar(&port, "port", 0, "HTTP bind port (default: 7734)")
	cmd.Flags().BoolVar(&skipWizard, "skip-wizard", false, "Skip the first-run setup wizard")
	cmd.Flags().StringVar(&standaloneCreds, "standalone-creds", "auto",
		"Standalone credential mode (on|off|auto). When on, donmai seeds child agent env from process env + ${gitRoot}/.env.local. When auto, on is selected when DONMAI_DAEMON_JWT is unset (i.e. not running under a downstream embedder's credential pipeline).")

	return cmd
}

// resolveStandaloneCredsMode returns true when donmai should seed agent
// env from the LocalSource. Behaviour by flag value:
//
//   - "on"   → always seed.
//   - "off"  → never seed (a downstream embedder or other credential pipeline owns env).
//   - "auto" → seed iff !daemonJWTPresent (no DONMAI_DAEMON_JWT in env).
//
// Unknown values fall back to "auto" with a slog.Warn — operators get a
// surfaced misconfiguration but the daemon does not refuse to start.
func resolveStandaloneCredsMode(flagValue string, daemonJWTPresent bool) bool {
	switch strings.ToLower(strings.TrimSpace(flagValue)) {
	case "on", "true", "yes", "1":
		return true
	case "off", "false", "no", "0":
		return false
	case "", "auto":
		return !daemonJWTPresent
	default:
		slog.Warn("--standalone-creds: unknown value, falling back to auto", "value", flagValue)
		return !daemonJWTPresent
	}
}

// resolveStandaloneGitRoot returns the gitRoot used to locate the
// daemon's .env.local. Falls back to the current working directory when
// `git rev-parse` is unavailable (which is the common case for the
// launchd / systemd entry point — they run from `/` and have no git
// state). Returning "" makes LocalSource skip the file lookup entirely,
// which is the desired behaviour.
func resolveStandaloneGitRoot() string {
	if root, err := gitRoot(); err == nil && root != "" {
		return root
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	// Only return the cwd when an .env.local actually exists there;
	// otherwise leave it empty so we don't spuriously stat a file in
	// `/` or similar.
	if _, err := os.Stat(cwd + "/.env.local"); err == nil {
		return cwd
	}
	return ""
}

// displayEnvLocalPath returns a human-readable path label for the
// startup [creds] log line — falls back to "(no .env.local)" when the
// local source did not parse a file.
func displayEnvLocalPath(s *afcreds.LocalSource) string {
	if s == nil || s.EnvLocalPath() == "" {
		return "(no .env.local)"
	}
	return s.EnvLocalPath()
}

// formatStartupWorkerLine returns the post-Start `[daemon] worker-id ...`
// line, or "" when no worker id has been assigned yet. Stub registrations
// (worker id ending in `-stub`) are annotated so operators do not mistake
// them for a successful platform registration.
func formatStartupWorkerLine(workerID string) string {
	if workerID == "" {
		return ""
	}
	if strings.HasSuffix(workerID, "-stub") {
		return fmt.Sprintf("[daemon] worker-id %s (stub registration, not registered with platform)", workerID)
	}
	return fmt.Sprintf("[daemon] worker-id %s", workerID)
}
