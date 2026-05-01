package afcli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/daemon"
)

// newDaemonRunCmd constructs the `daemon run` subcommand. This is the
// long-running entry point registered by the launchd plist / systemd unit.
//
// REN-1406 wired the installer to register `<host-binary> daemon run`; this
// command is what runs on those service managers.
func newDaemonRunCmd() *cobra.Command {
	var (
		configPath string
		jwtPath    string
		host       string
		port       int
		skipWizard bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon (long-running entry point)",
		Long: "Start the long-running rensei daemon process.\n\n" +
			"This is the service entry point registered by `daemon install` —\n" +
			"the launchd plist (macOS) and systemd unit (Linux) call this\n" +
			"subcommand. It loads ~/.rensei/daemon.yaml, registers with the\n" +
			"orchestrator, starts the heartbeat loop, and serves the local\n" +
			"control HTTP API on 127.0.0.1:7734.\n\n" +
			"Run interactively for development with `--skip-wizard` to bypass\n" +
			"the first-run setup. SIGTERM / SIGINT triggers a graceful drain.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := daemon.New(daemon.Options{
				ConfigPath: configPath,
				JWTPath:    jwtPath,
				HTTPHost:   host,
				HTTPPort:   port,
				SkipWizard: skipWizard,
			})
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()
			if err := d.Start(ctx); err != nil {
				return fmt.Errorf("daemon start: %w", err)
			}
			_, _ = fmt.Fprintf(out, "[daemon] state -> %s\n", d.State())
			// Print the worker id only after Start() completes registration so
			// the value is the live, platform-assigned id (or a clearly-marked
			// stub fallback). REN-1445 — previously the log fired with a stub
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
			_ = d.Stop(shutCtx)
			_, _ = fmt.Fprintln(out, "[daemon] stopped")
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "Path to daemon.yaml (default: ~/.rensei/daemon.yaml)")
	cmd.Flags().StringVar(&jwtPath, "jwt-path", "", "Path to cached JWT (default: ~/.rensei/daemon.jwt)")
	cmd.Flags().StringVar(&host, "host", "", "HTTP bind host (default: 127.0.0.1)")
	cmd.Flags().IntVar(&port, "port", 0, "HTTP bind port (default: 7734)")
	cmd.Flags().BoolVar(&skipWizard, "skip-wizard", false, "Skip the first-run setup wizard")

	return cmd
}

// formatStartupWorkerLine returns the post-Start `[daemon] worker-id ...`
// line, or "" when no worker id has been assigned yet. Stub registrations
// (worker id ending in `-stub`) are annotated so operators do not mistake
// them for a successful platform registration. (REN-1445.)
func formatStartupWorkerLine(workerID string) string {
	if workerID == "" {
		return ""
	}
	if strings.HasSuffix(workerID, "-stub") {
		return fmt.Sprintf("[daemon] worker-id %s (stub registration — not registered with platform)", workerID)
	}
	return fmt.Sprintf("[daemon] worker-id %s", workerID)
}
