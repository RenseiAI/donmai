package afcli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/worker"
)

// fleetStartFlags holds the parsed flag values for `donmai fleet start`.
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
//
// The provisioning token NEVER rides this argv: any local user can read a
// process's command line (ps/pgrep, /proc/<pid>/cmdline), so a credential
// placed here is readable host-wide. When the operator passes
// --provisioning-token, the caller delivers it to the child through its
// environment instead (see fleetChildEnv); the child resolves the token
// from --provisioning-token or $DONMAI_PROVISIONING_TOKEN either way.
func buildWorkerChildArgs(f *fleetStartFlags) []string {
	args := []string{"worker", "start"}
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

// fleetProvisioningTokenEnv is the environment variable that carries an
// operator-supplied --provisioning-token to each fleet child. The child
// resolves its token from --provisioning-token or this variable (see
// resolveWorkerToken), so delivering it here keeps the secret out of the
// child's argv while leaving the child's own flag parsing untouched.
const fleetProvisioningTokenEnv = "DONMAI_PROVISIONING_TOKEN"

// fleetChildEnv builds the environment each spawned child worker receives:
// the parent environment unchanged, plus fleetProvisioningTokenEnv when the
// operator passed --provisioning-token on the command line. Appended last,
// the flag value wins over an inherited variable under exec's
// last-entry-wins semantics; when the flag is empty the parent environment
// passes through untouched, preserving the $DONMAI_PROVISIONING_TOKEN /
// $DONMAI_BASE_URL fallback the child already implements.
func fleetChildEnv(provisioningToken string) []string {
	env := os.Environ()
	if provisioningToken == "" {
		return env
	}
	return append(env, fleetProvisioningTokenEnv+"="+provisioningToken)
}

// newFleetStartCmd constructs the `fleet start` subcommand. It resolves
// the current binary path, builds the per-child argv, and delegates to
// worker.Fleet.Start which writes the PID file on success.
func newFleetStartCmd(bin string) *cobra.Command {
	flags := &fleetStartFlags{}

	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start a fleet of worker processes",
		Long:         "Spawn --count `" + bin + " worker start` processes and supervise them. The PID of each child is recorded in the fleet PID file so `fleet stop` and `fleet status` can find them.",
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
			// $DONMAI_PROVISIONING_TOKEN / $DONMAI_BASE_URL still work when the
			// operator didn't pass --provisioning-token on the command line.
			// A --provisioning-token flag value is layered onto that
			// environment (never onto the child argv, where ps could read
			// it) — see fleetChildEnv.
			f.Env = fleetChildEnv(flags.provisioningToken)

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
	cmd.Flags().StringVar(&flags.provisioningToken, "provisioning-token", "", "Worker provisioning token (delivered to each child via its environment, never its command line; defaults to $DONMAI_PROVISIONING_TOKEN in the child)")
	cmd.Flags().StringVar(&flags.baseURL, "base-url", "", "Coordinator base URL (passed to each child)")
	cmd.Flags().IntVar(&flags.maxAgents, "max-agents", 1, "Maximum concurrent agent sessions per worker")
	cmd.Flags().DurationVar(&flags.pollInterval, "poll-interval", 5*time.Second, "Poll interval passed to each child")
	cmd.Flags().DurationVar(&flags.heartbeatInterval, "heartbeat-interval", 30*time.Second, "Heartbeat interval passed to each child")
	cmd.Flags().StringSliceVar(&flags.capabilities, "capabilities", nil, "Capability tags passed to each child")
	_ = cmd.MarkFlagRequired("count")

	return cmd
}
