package afcli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/worker"
)

// fleetStartFlags holds the parsed flag values for `af fleet start`.
type fleetStartFlags struct {
	count             int
	provisioningToken string
	baseURL           string
	maxAgents         int
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	capabilities      []string
}

// buildWorkerChildArgs assembles the argv that each spawned child worker
// process will receive. The binary path is NOT included — Fleet prepends
// it itself.
func buildWorkerChildArgs(f *fleetStartFlags) []string {
	args := []string{"worker", "start"}
	if f.provisioningToken != "" {
		args = append(args, "--provisioning-token", f.provisioningToken)
	}
	if f.baseURL != "" {
		args = append(args, "--base-url", f.baseURL)
	}
	if f.maxAgents > 0 {
		args = append(args, "--max-agents", strconv.Itoa(f.maxAgents))
	}
	if f.pollInterval > 0 {
		args = append(args, "--poll-interval", f.pollInterval.String())
	}
	if f.heartbeatInterval > 0 {
		args = append(args, "--heartbeat-interval", f.heartbeatInterval.String())
	}
	for _, cap := range f.capabilities {
		args = append(args, "--capabilities", cap)
	}
	return args
}

// newFleetStartCmd constructs the `fleet start` subcommand. It resolves
// the current binary path, builds the per-child argv, and delegates to
// worker.Fleet.Start which writes the PID file on success.
func newFleetStartCmd() *cobra.Command {
	flags := &fleetStartFlags{}

	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start a fleet of worker processes",
		Long:         "Spawn --count `af worker start` processes and supervise them. The PID of each child is recorded in the fleet PID file so `fleet stop` and `fleet status` can find them.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if flags.count <= 0 {
				return fmt.Errorf("fleet start: --count must be > 0")
			}

			binaryPath, err := os.Executable()
			if err != nil {
				return fmt.Errorf("fleet start: resolve executable: %w", err)
			}

			args := buildWorkerChildArgs(flags)
			f := worker.NewFleet(binaryPath, args)
			// Children inherit the parent environment unchanged so that
			// $AF_PROVISIONING_TOKEN / $AF_BASE_URL still work when the
			// operator didn't pass --provisioning-token on the command line.
			f.Env = os.Environ()

			if err := f.Start(context.Background(), flags.count); err != nil {
				return fmt.Errorf("fleet start: %w", err)
			}

			pids := make([]int, 0, flags.count)
			for _, p := range f.Status() {
				pids = append(pids, p.PID)
			}
			fmt.Printf("Fleet started: %d workers (PIDs: %v)\n", len(pids), pids)
			return nil
		},
	}

	cmd.Flags().IntVar(&flags.count, "count", 0, "Number of worker processes to spawn (required, > 0)")
	cmd.Flags().StringVar(&flags.provisioningToken, "provisioning-token", "", "Worker provisioning token (passed to each child; defaults to $AF_PROVISIONING_TOKEN in the child)")
	cmd.Flags().StringVar(&flags.baseURL, "base-url", "", "Coordinator base URL (passed to each child)")
	cmd.Flags().IntVar(&flags.maxAgents, "max-agents", 1, "Maximum concurrent agent sessions per worker")
	cmd.Flags().DurationVar(&flags.pollInterval, "poll-interval", 5*time.Second, "Poll interval passed to each child")
	cmd.Flags().DurationVar(&flags.heartbeatInterval, "heartbeat-interval", 30*time.Second, "Heartbeat interval passed to each child")
	cmd.Flags().StringSliceVar(&flags.capabilities, "capabilities", nil, "Capability tags passed to each child")
	_ = cmd.MarkFlagRequired("count")

	return cmd
}
