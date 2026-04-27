package afcli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/RenseiAI/agentfactory-tui/internal/governor"
	"github.com/RenseiAI/agentfactory-tui/internal/linear"
	"github.com/RenseiAI/agentfactory-tui/internal/process"
	"github.com/RenseiAI/agentfactory-tui/internal/queue"
	"github.com/spf13/cobra"
)

// governorPIDName is the PID file name used for the governor process.
// Referenced by governor_stop.go and governor_status.go.
const governorPIDName = "governor"

// governorRunnable is the minimal interface used by governor start so tests can
// substitute a fake without importing internal/governor.
type governorRunnable interface {
	Run(ctx context.Context) error
}

// governorRunnerFactory is the package-level hook; tests override it.
var governorRunnerFactory = defaultGovernorRunnerFactory

// governorDaemonize is the package-level hook for daemonization; tests override it.
var governorDaemonize = process.Daemonize

// defaultGovernorRunnerFactory wires the real linear/queue/governor clients.
// It returns the runnable, a closer for the queue client, and any setup error.
func defaultGovernorRunnerFactory(
	cfg governor.Config,
	apiKey string,
	redisURL string,
	logger *slog.Logger,
) (governorRunnable, func() error, error) {
	linClient, err := linear.NewClient(apiKey)
	if err != nil {
		return nil, nil, fmt.Errorf("governor start: %w", err)
	}

	if redisURL == "" {
		return nil, nil, fmt.Errorf("governor start: REDIS_URL is not set")
	}

	qClient, err := queue.NewClient(redisURL)
	if err != nil {
		return nil, nil, fmt.Errorf("governor start: %w", err)
	}

	closer := func() error { return qClient.Close() }

	ctx := context.Background()
	if err := qClient.Ping(ctx); err != nil {
		_ = closer()
		return nil, nil, fmt.Errorf("governor start: %w", err)
	}

	runner, err := governor.NewRunner(cfg, linClient, qClient, logger)
	if err != nil {
		_ = closer()
		return nil, nil, fmt.Errorf("governor start: %w", err)
	}

	return runner, closer, nil
}

// validModes is the set of accepted --mode values.
var validModes = map[string]bool{
	"event-driven": true,
	"poll-only":    true,
}

// newGovernorStartCmd constructs the `governor start` subcommand.
// It builds and launches the in-process governor runner, either in foreground
// mode (streaming logs via stderr) or background/daemon mode (PID tracking).
func newGovernorStartCmd() *cobra.Command {
	var (
		projects              []string
		foreground            bool
		scanInterval          string
		maxDispatches         int
		once                  bool
		mode                  string
		noAutoResearch        bool
		noAutoBacklogCreation bool
		noAutoDevelopment     bool
		noAutoQA              bool
		noAutoAcceptance      bool
	)

	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start the governor scan loop",
		Long:         "Launch the governor process that scans Linear issues and dispatches work. Runs in background by default; use --foreground to stream logs.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			// LINEAR_API_KEY preflight.
			apiKey := os.Getenv("LINEAR_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("governor start: LINEAR_API_KEY is not set")
			}

			// Resolve project list: flag takes precedence, fall back to env.
			resolved := projects
			if len(resolved) == 0 {
				if env := os.Getenv("GOVERNOR_PROJECTS"); env != "" {
					for _, p := range strings.Split(env, ",") {
						p = strings.TrimSpace(p)
						if p != "" {
							resolved = append(resolved, p)
						}
					}
				}
			}
			if len(resolved) == 0 {
				return fmt.Errorf("governor start: at least one --project is required (or set GOVERNOR_PROJECTS env)")
			}

			// Mode enum validation.
			if !validModes[mode] {
				return fmt.Errorf("invalid --mode %q: must be \"event-driven\" or \"poll-only\"", mode)
			}

			// Parse scan interval.
			interval, err := time.ParseDuration(scanInterval)
			if err != nil {
				return fmt.Errorf("governor start: invalid --scan-interval: %w", err)
			}

			// Build governor.Config from flags.
			cfg := governor.Config{
				Projects:            resolved,
				ScanInterval:        interval,
				MaxDispatches:       maxDispatches,
				Once:                once,
				Mode:                governor.Mode(mode),
				AutoResearch:        !noAutoResearch,
				AutoBacklogCreation: !noAutoBacklogCreation,
				AutoDevelopment:     !noAutoDevelopment,
				AutoQA:              !noAutoQA,
				AutoAcceptance:      !noAutoAcceptance,
			}

			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
			redisURL := os.Getenv("REDIS_URL")

			runner, closer, err := governorRunnerFactory(cfg, apiKey, redisURL, logger)
			if err != nil {
				return err
			}
			defer func() { _ = closer() }()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if foreground {
				fmt.Fprintln(os.Stderr, "Governor starting in foreground...")
				process.InstallSignalHandlers(ctx, cancel)
				return runner.Run(ctx)
			}

			// Background / daemon path.
			isChild, childPID, err := governorDaemonize()
			if err != nil {
				return fmt.Errorf("governor start: %w", err)
			}

			if !isChild {
				// Parent: write PID file and exit.
				pf, err := process.NewPIDFile("governor")
				if err != nil {
					return fmt.Errorf("governor start: %w", err)
				}
				if err := pf.Write(childPID); err != nil {
					return fmt.Errorf("governor start: %w", err)
				}
				fmt.Printf("Governor started (PID %d)\n", childPID)
				return nil
			}

			// Child (daemon): run the loop.
			process.InstallSignalHandlers(ctx, cancel)
			return runner.Run(ctx)
		},
	}

	cmd.Flags().StringSliceVar(&projects, "project", nil, "Project slug (repeatable; falls back to GOVERNOR_PROJECTS env if empty)")
	cmd.Flags().BoolVarP(&foreground, "foreground", "f", false, "Run in foreground with log streaming")
	cmd.Flags().StringVar(&scanInterval, "scan-interval", "60s", "Scan loop interval (e.g. 30s, 1m)")
	cmd.Flags().IntVar(&maxDispatches, "max-dispatches", 3, "Maximum concurrent dispatches")
	cmd.Flags().BoolVar(&once, "once", false, "Run a single scan then exit")
	cmd.Flags().StringVar(&mode, "mode", "poll-only", "Scan mode: event-driven | poll-only")
	cmd.Flags().BoolVar(&noAutoResearch, "no-auto-research", false, "Disable automatic research dispatch")
	cmd.Flags().BoolVar(&noAutoBacklogCreation, "no-auto-backlog-creation", false, "Disable automatic backlog creation dispatch")
	cmd.Flags().BoolVar(&noAutoDevelopment, "no-auto-development", false, "Disable automatic development dispatch")
	cmd.Flags().BoolVar(&noAutoQA, "no-auto-qa", false, "Disable automatic QA dispatch")
	cmd.Flags().BoolVar(&noAutoAcceptance, "no-auto-acceptance", false, "Disable automatic acceptance dispatch")

	return cmd
}
