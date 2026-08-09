package afcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/codesurvival"
	"github.com/RenseiAI/donmai/internal/envcompat"
	"github.com/RenseiAI/donmai/kgextract"
	"github.com/RenseiAI/donmai/worker"
)

// defaultWorkerBaseURL is used when neither --base-url nor $DONMAI_BASE_URL
// (legacy: $AF_BASE_URL) is set.
const defaultWorkerBaseURL = "http://localhost:3000"

// workerStartFlags holds the parsed flag values for `donmai worker start`.
// Factored out so tests can inspect defaults without executing RunE.
type workerStartFlags struct {
	provisioningToken string
	baseURL           string
	maxAgents         int
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	capabilities      []string
	debug             bool
	quiet             bool
}

// resolveWorkerToken picks the provisioning token from the --provisioning-token
// flag with a fallback to $DONMAI_PROVISIONING_TOKEN (legacy: $AF_PROVISIONING_TOKEN).
// Returns an error when both are empty so the caller can surface a clear message.
func resolveWorkerToken(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if env := envcompat.GetProvisioningToken(); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("worker start: provisioning token required (--provisioning-token or $DONMAI_PROVISIONING_TOKEN)")
}

// resolveWorkerBaseURL picks the base URL from the --base-url flag with
// fallback to $DONMAI_BASE_URL (legacy: $AF_BASE_URL) and finally to defaultWorkerBaseURL.
func resolveWorkerBaseURL(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := envcompat.GetBaseURL(); env != "" {
		return env
	}
	return defaultWorkerBaseURL
}

// configureWorkerLogging sets the default slog logger based on the local
// --debug/--quiet flags. The root command also has persistent --debug/--quiet
// flags that pre-configure slog; this helper lets `donmai worker start` stand
// alone when invoked directly (e.g. by a fleet-spawned child).
func configureWorkerLogging(debug, quiet bool) {
	var level slog.Level
	var w io.Writer = os.Stderr
	switch {
	case quiet:
		level = slog.LevelError
	case debug:
		level = slog.LevelDebug
	default:
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
}

// newWorkerStartCmd constructs the `worker start` subcommand. It registers
// the worker with the coordinator and then drives PollLoop and
// HeartbeatLoop concurrently until SIGINT/SIGTERM or the runtime JWT
// expires.
func newWorkerStartCmd() *cobra.Command {
	flags := &workerStartFlags{}

	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start a worker process (register, poll, heartbeat)",
		Long:         "Register with the coordinator using a provisioning token, then run Poll and Heartbeat loops concurrently until interrupted.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runWorkerStart(flags)
		},
	}

	cmd.Flags().StringVar(&flags.provisioningToken, "provisioning-token", "", "Worker provisioning token (default: $DONMAI_PROVISIONING_TOKEN)")
	cmd.Flags().StringVar(&flags.baseURL, "base-url", "", "Coordinator base URL (default: $DONMAI_BASE_URL or http://localhost:3000)")
	cmd.Flags().IntVar(&flags.maxAgents, "max-agents", 1, "Maximum concurrent agent sessions this worker will run")
	cmd.Flags().DurationVar(&flags.pollInterval, "poll-interval", 5*time.Second, "Interval between poll requests")
	cmd.Flags().DurationVar(&flags.heartbeatInterval, "heartbeat-interval", 30*time.Second, "Interval between heartbeats (overridden by server if register response carries a non-zero cadence)")
	cmd.Flags().StringSliceVar(&flags.capabilities, "capabilities", nil, "Capability tags this worker advertises (comma-separated)")
	cmd.Flags().BoolVar(&flags.debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&flags.quiet, "quiet", false, "Suppress non-error logs")

	return cmd
}

// runWorkerStart is the body of RunE. Extracted to keep the cobra wiring
// small and to let tests drive it with synthetic flags if needed.
func runWorkerStart(flags *workerStartFlags) error {
	configureWorkerLogging(flags.debug, flags.quiet)

	token, err := resolveWorkerToken(flags.provisioningToken)
	if err != nil {
		return err
	}
	baseURL := resolveWorkerBaseURL(flags.baseURL)

	hostname, err := os.Hostname()
	if err != nil {
		// Non-fatal: surface a placeholder so the coordinator still gets a
		// registration record.
		hostname = "unknown"
	}

	// TODO: wire a real version string via -ldflags "-X main.version=..."
	// when the worker binary is built. For now it's "dev" so we don't
	// lie to the coordinator about provenance.
	version := "dev"

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := worker.NewClient(baseURL, token)

	// The kg-extraction lane: the capability tag and the executor come from ONE
	// value, so this process can never advertise the tag without running the
	// work (see kgextract.Lane). The resident daemon builds the same lane from
	// its own poll path, which is why both can advertise it safely.
	kgLane := kgextract.NewLane(kgextract.Options{
		WorkerVersion:   version,
		PlatformBaseURL: baseURL,
	})

	// `donmai worker start` ALWAYS runs the in-process batch executors (the
	// batch lane below) — code-survival-scan AND kg-extraction — so it must
	// advertise BOTH capabilities at registration. The coordinator poll route
	// gates the batchWork[]/kgExtractWork[] CLAIM on the matching capability:
	// only a worker advertising it is handed (and can decode) a staged item, and
	// the claim POPS the item off the org queue — so advertising a lane this
	// process does not run would drop the work outright. Merge with any
	// operator-supplied --capabilities, deduped.
	capabilities := worker.MergeCapabilities(flags.capabilities,
		codesurvival.WorkTypeCodeSurvivalScan,
		kgLane.Capability,
	)

	resp, err := c.Register(ctx, worker.RegisterRequest{
		Hostname:     hostname,
		PID:          os.Getpid(),
		Version:      version,
		Capabilities: capabilities,
		MaxAgents:    flags.maxAgents,
	})
	if err != nil {
		return fmt.Errorf("worker start: %w", err)
	}

	heartbeatInterval := flags.heartbeatInterval
	if resp.HeartbeatIntervalSeconds > 0 {
		heartbeatInterval = resp.HeartbeatInterval()
	}

	slog.Info("worker start: entering loops",
		"worker_id", c.WorkerID,
		"poll_interval", flags.pollInterval,
		"heartbeat_interval", heartbeatInterval,
	)

	// Run PollLoop and HeartbeatLoop concurrently. First loop to error
	// cancels the shared context so the other loop also unwinds.
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)
	var wg sync.WaitGroup

	// Non-agent batch lane: the batch executors run IN this worker process, fully
	// separate from the agent path. Each batch failure is best-effort and never
	// touches the agent handler below.
	//
	//   - code-survival-scan (RW3): clone a merged PR, measure line survival, POST.
	//   - kg-extraction: per-observation constrained LLM emit → graph triples → POST.
	//
	// Both flow through a single workType mux (batchHandlerMux): the worker poll
	// loop hands every batchWork[]/kgExtractWork[] item to this one handler, which
	// dispatches on WorkType to the matching executor. An unknown work-type is
	// logged + skipped (graceful degradation; never crashes the loop).
	survivalExec := codesurvival.NewExecutor(codesurvival.Options{
		WorkerVersion: version,
	})
	batchHandler := batchHandlerMux(
		codesurvival.BatchHandler(survivalExec),
		kgLane.Handler,
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		perr := c.PollLoopWithBatch(loopCtx, flags.pollInterval, func(item worker.WorkItem) error {
			slog.Info("worker start: work item received", "item_id", item.ID, "type", item.Type)
			// No actual work dispatch here — that lives in the agent
			// runner. We simply acknowledge receipt so the fleet knows
			// polling is alive.
			return nil
		}, batchHandler)
		if perr != nil {
			errCh <- fmt.Errorf("poll loop: %w", perr)
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		// TODO: thread a real active-agent counter once the worker runs
		// sessions locally. For now the worker reports zero active agents
		// every tick.
		herr := c.HeartbeatLoop(loopCtx, heartbeatInterval, func() int { return 0 })
		if herr != nil {
			errCh <- fmt.Errorf("heartbeat loop: %w", herr)
			cancel()
		}
	}()

	wg.Wait()
	close(errCh)

	// Read only the first reported error. Subsequent errors are drained
	// but discarded — they are almost always consequences of the first
	// (ctx cancellation cascades through the sibling loop).
	var firstErr error
	for lerr := range errCh {
		if firstErr == nil {
			firstErr = lerr
		}
	}
	if firstErr != nil {
		if errors.Is(firstErr, worker.ErrRuntimeJWTExpired) {
			// Fleet managers use this signal to decide whether to re-run
			// the worker with a fresh registration.
			slog.Warn("worker start: runtime JWT expired; worker exiting", "err", firstErr)
		}
		return firstErr
	}

	// Clean shutdown from signal.
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("worker start: %w", err)
	}
	return nil
}

// batchHandlerMux fans a single worker.BatchHandler out to the per-work-type
// executors. The worker poll loop hands EVERY batchWork[]/kgExtractWork[] item to
// one handler; this mux inspects item.WorkType and dispatches to the matching
// executor's BatchHandler. An item whose work-type matches none of the wired
// executors is logged + skipped (graceful degradation — a stale worker must
// never crash the loop on a work-type a newer platform dispatched).
//
// Each executor's own BatchHandler also defaults-to-skip on a non-matching
// work-type, so routing is robust even if the mux's switch and the executors'
// switches ever drift: the mux narrows by work-type first to avoid handing
// (e.g.) a kg-extraction item to the codesurvival decoder.
func batchHandlerMux(survival, kg worker.BatchHandler) worker.BatchHandler {
	return func(ctx context.Context, item worker.BatchWorkItem) error {
		switch item.WorkType {
		case codesurvival.WorkTypeCodeSurvivalScan:
			return survival(ctx, item)
		case kgextract.WorkTypeKGExtraction:
			return kg(ctx, item)
		default:
			slog.Info("worker start: unknown batch work-type; skipping",
				"batchJobId", item.BatchJobID, "workType", item.WorkType)
			return nil
		}
	}
}
