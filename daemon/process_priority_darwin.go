package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/installer/servicepriority"
)

// darwinBackgroundPriorityCeiling is the highest `ps` PRI a background-band
// process reports. Both launchd ProcessType=Background and taskpolicy -b put a
// process at 4. A launchd job without a ProcessType is utility-clamped and
// reports 20, and a process started from a shell reports higher still, so
// everything above the ceiling is the default mode.
const darwinBackgroundPriorityCeiling = 4

type processPriorityRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func runProcessPriorityCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed system tool with a local pid argument
}

// observeProcessPriority reads the daemon's own priority with ps.
func observeProcessPriority() (processPriorityObservation, bool) {
	return observeDarwinProcessPriority(os.Getpid(), runProcessPriorityCommand), true
}

func observeDarwinProcessPriority(pid int, run processPriorityRunner) processPriorityObservation {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := run(ctx, "ps", "-o", "pri=", "-p", strconv.Itoa(pid))
	if err != nil {
		return processPriorityObservation{warning: fmt.Sprintf("could not read the live process priority: ps: %v (%s)", err, strings.TrimSpace(string(out)))}
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return processPriorityObservation{warning: "could not read the live process priority: ps returned no value"}
	}
	pri, err := strconv.Atoi(fields[0])
	if err != nil {
		return processPriorityObservation{warning: fmt.Sprintf("could not read the live process priority: parse ps PRI %q: %v", fields[0], err)}
	}
	mode := servicepriority.Default
	if pri <= darwinBackgroundPriorityCeiling {
		mode = servicepriority.Background
	}
	return processPriorityObservation{mode: mode, evidence: fmt.Sprintf("ps PRI=%d", pri)}
}
