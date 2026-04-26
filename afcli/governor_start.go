package afcli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// governorPIDName is the PID file name used for the governor process.
const governorPIDName = "governor"

// governorBinaryFinder finds the child binary; tests override it.
var governorBinaryFinder = findGovernorBinary

// governorForegroundRunner is the foreground runner; tests override it.
var governorForegroundRunner = runGovernorForeground

// governorBackgroundRunner is the background runner; tests override it.
var governorBackgroundRunner = runGovernorBackground

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

// validModes is the set of accepted --mode values.
var validModes = map[string]bool{
	"event-driven": true,
	"poll-only":    true,
}

// newGovernorStartCmd constructs the `governor start` subcommand.
// It finds and launches the governor binary as a subprocess, either
// in foreground mode (streaming logs) or background mode (PID tracking).
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
			if os.Getenv("LINEAR_API_KEY") == "" {
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

			binPath, err := governorBinaryFinder()
			if err != nil {
				return err
			}

			// Build the child arg slice.
			args := []string{"governor"}

			for _, p := range resolved {
				args = append(args, "--project", p)
			}

			args = append(args, "--scan-interval", scanInterval)
			args = append(args, "--max-dispatches", fmt.Sprintf("%d", maxDispatches))
			args = append(args, "--mode", mode)

			if once {
				args = append(args, "--once")
			}
			if noAutoResearch {
				args = append(args, "--no-auto-research")
			}
			if noAutoBacklogCreation {
				args = append(args, "--no-auto-backlog-creation")
			}
			if noAutoDevelopment {
				args = append(args, "--no-auto-development")
			}
			if noAutoQA {
				args = append(args, "--no-auto-qa")
			}
			if noAutoAcceptance {
				args = append(args, "--no-auto-acceptance")
			}

			if foreground {
				return governorForegroundRunner(binPath, args)
			}
			return governorBackgroundRunner(binPath, args)
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
