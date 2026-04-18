package afcli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// governorPIDName is the PID file name used for the governor process.
const governorPIDName = "governor"

// findGovernorBinary searches PATH for the governor binary, preferring
// "agentfactory-governor" over the shorter "af-governor" alias.
func findGovernorBinary() (string, error) {
	for _, name := range []string{"agentfactory-governor", "af-governor"} {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("governor binary not found: install agentfactory-governor or af-governor")
}

// newGovernorStartCmd constructs the `governor start` subcommand.
// It finds and launches the governor binary as a subprocess, either
// in foreground mode (streaming logs) or background mode (PID tracking).
func newGovernorStartCmd() *cobra.Command {
	var (
		project       string
		foreground    bool
		scanInterval  string
		maxDispatches int
	)

	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start the governor scan loop",
		Long:         "Launch the governor process that scans Linear issues and dispatches work. Runs in background by default; use --foreground to stream logs.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			binPath, err := findGovernorBinary()
			if err != nil {
				return err
			}

			args := []string{"governor", "--project", project}
			if scanInterval != "" {
				args = append(args, "--scan-interval", scanInterval)
			}
			if maxDispatches > 0 {
				args = append(args, "--max-dispatches", fmt.Sprintf("%d", maxDispatches))
			}

			if foreground {
				return runGovernorForeground(binPath, args)
			}
			return runGovernorBackground(binPath, args)
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "Project slug (required)")
	cmd.Flags().BoolVarP(&foreground, "foreground", "f", false, "Run in foreground with log streaming")
	cmd.Flags().StringVar(&scanInterval, "scan-interval", "", "Scan loop interval (e.g. 30s, 1m)")
	cmd.Flags().IntVar(&maxDispatches, "max-dispatches", 0, "Maximum concurrent dispatches")
	_ = cmd.MarkFlagRequired("project")

	return cmd
}

// runGovernorForeground runs the governor binary in the foreground,
// piping stdout/stderr to the terminal. It waits for Ctrl+C (SIGINT)
// and then sends SIGTERM to the child process.
func runGovernorForeground(binPath string, args []string) error {
	cmd := exec.Command(binPath, args...) //nolint:gosec // binPath comes from exec.LookPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start governor: %w", err)
	}

	// Trap SIGINT/SIGTERM and forward to child.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- cmd.Wait()
	}()

	select {
	case sig := <-sigCh:
		// Forward signal to the child process.
		_ = cmd.Process.Signal(sig)
		return <-doneCh
	case err := <-doneCh:
		signal.Stop(sigCh)
		if err != nil {
			return fmt.Errorf("governor exited: %w", err)
		}
		return nil
	}
}

// runGovernorBackground starts the governor binary as a background
// process and saves its PID for later management via stop/status.
func runGovernorBackground(binPath string, args []string) error {
	cmd := exec.Command(binPath, args...) //nolint:gosec // binPath comes from exec.LookPath
	// Detach stdout/stderr so the process runs silently in background.
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	// Start in a new process group so it survives the parent exiting.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start governor: %w", err)
	}

	pid := cmd.Process.Pid
	if err := savePID(governorPIDName, pid); err != nil {
		// Kill the process if we can't track it.
		_ = cmd.Process.Kill()
		return fmt.Errorf("save pid: %w", err)
	}

	// Release the process so it isn't waited on by this parent.
	_ = cmd.Process.Release()

	fmt.Printf("Governor started (PID %d)\n", pid)
	return nil
}
