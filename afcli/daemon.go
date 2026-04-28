package afcli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// daemonDoer is the interface used by daemon subcommands. It is satisfied by
// *afclient.DaemonClient and by mock implementations in tests.
type daemonDoer interface {
	GetStatus() (*afclient.DaemonStatusResponse, error)
	GetStats(withPool, byMachine bool) (*afclient.DaemonStatsResponse, error)
	Pause() (*afclient.DaemonActionResponse, error)
	Resume() (*afclient.DaemonActionResponse, error)
	Stop() (*afclient.DaemonActionResponse, error)
	Drain(timeoutSeconds int) (*afclient.DaemonActionResponse, error)
	Update() (*afclient.DaemonActionResponse, error)
}

// daemonClientFactory is the type for a constructor that builds a daemonDoer
// from a DaemonConfig. Injected per command-tree for test isolation.
type daemonClientFactory func(cfg afclient.DaemonConfig) daemonDoer

// defaultDaemonFactory is the production factory — always returns a real client.
func defaultDaemonFactory(cfg afclient.DaemonConfig) daemonDoer {
	return afclient.NewDaemonClient(cfg)
}

// defaultDaemonLogFile is the default path for the daemon log file per 011.
const defaultDaemonLogFile = "~/.rensei/daemon.log"

// defaultDaemonBinary is the expected daemon binary name on PATH.
const defaultDaemonBinary = "rensei-daemon"

// expandHomePath replaces a leading ~ with the user's home directory.
func expandHomePath(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}

// newDaemonCmd constructs the `daemon` parent command. It holds no logic of
// its own; it dispatches to subcommands that manage the local rensei-daemon.
// The factory parameter is the DaemonClient constructor; passing nil selects
// the production default.
func newDaemonCmd() *cobra.Command {
	return newDaemonCmdWithFactory(defaultDaemonFactory)
}

// newDaemonCmdWithFactory is the injectable variant used in tests.
func newDaemonCmdWithFactory(factory daemonClientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the local rensei-daemon",
		Long: "Manage the local rensei-daemon process that supervises agent session pools.\n\n" +
			"The daemon replaces the per-workspace `af worker` / `af fleet` approach.\n" +
			"Install once, configure once, and sessions run automatically for allowed projects.",
		SilenceUsage: true,
	}

	cmd.AddCommand(newDaemonInstallCmd())
	cmd.AddCommand(newDaemonUninstallCmd())
	cmd.AddCommand(newDaemonSetupCmd())
	cmd.AddCommand(newDaemonStatusCmd(factory))
	cmd.AddCommand(newDaemonLogsCmd())
	cmd.AddCommand(newDaemonDoctorCmd())
	cmd.AddCommand(newDaemonPauseCmd(factory))
	cmd.AddCommand(newDaemonResumeCmd(factory))
	cmd.AddCommand(newDaemonUpdateCmd(factory))
	cmd.AddCommand(newDaemonDrainCmd(factory))
	cmd.AddCommand(newDaemonStopCmd(factory))
	cmd.AddCommand(newDaemonStatsCmd(factory))

	return cmd
}

// ── install / uninstall ───────────────────────────────────────────────────────

func newDaemonInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the daemon as a system service",
		Long: "Register rensei-daemon as a launchd (macOS) or systemd (Linux) service so it\n" +
			"starts automatically at login and survives reboots.\n\n" +
			"Platform-specific installers are not yet shipped. Use the rensei-daemon CLI directly:\n" +
			"  rensei-daemon install",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New(
				"platform installers not yet available in af — " +
					"use `rensei-daemon install` directly until REN-1292 (launchd) / REN-1293 (systemd) ship",
			)
		},
	}
}

func newDaemonUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the daemon system service",
		Long: "Remove the launchd (macOS) or systemd (Linux) service registration for rensei-daemon.\n\n" +
			"Platform-specific installers are not yet shipped. Use the rensei-daemon CLI directly:\n" +
			"  rensei-daemon uninstall",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New(
				"platform uninstallers not yet available in af — " +
					"use `rensei-daemon uninstall` directly until REN-1292 (launchd) / REN-1293 (systemd) ship",
			)
		},
	}
}

// ── setup ─────────────────────────────────────────────────────────────────────

func newDaemonSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Interactive first-run wizard",
		Long: "Run the interactive first-run setup wizard that captures machine identity, capacity,\n" +
			"orchestrator config, project allowlist, and auto-update preferences.\n\n" +
			"This invokes `rensei-daemon setup` as a subprocess so the wizard's\n" +
			"interactive prompts work correctly when stdin is a TTY.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bin, err := exec.LookPath(defaultDaemonBinary)
			if err != nil {
				return fmt.Errorf(
					"rensei-daemon not found on PATH — install it first with `brew install rensei` or equivalent: %w",
					err,
				)
			}
			proc := exec.Command(bin, "setup") //nolint:gosec // intentional subprocess
			proc.Stdin = os.Stdin
			proc.Stdout = cmd.OutOrStdout()
			proc.Stderr = cmd.ErrOrStderr()
			if err := proc.Run(); err != nil {
				return fmt.Errorf("rensei-daemon setup: %w", err)
			}
			return nil
		},
	}
}

// ── status ────────────────────────────────────────────────────────────────────

func newDaemonStatusCmd(factory daemonClientFactory) *cobra.Command {
	var (
		port    int
		host    string
		jsonOut bool
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the running daemon's status",
		Long: "Query the local daemon's HTTP status endpoint and display uptime, active sessions,\n" +
			"capacity, and lifecycle state. Renders a human-readable ANSI table by default,\n" +
			"or raw JSON with --json.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := afclient.DefaultDaemonConfig()
			if host != "" {
				cfg.Host = host
			}
			if port != 0 {
				cfg.Port = port
			}
			client := factory(cfg)
			resp, err := client.GetStatus()
			if err != nil {
				return fmt.Errorf("daemon status: %w", err)
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			return writeDaemonStatusTable(out, resp)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output raw JSON (indented)")

	return cmd
}

// writeDaemonStatusTable renders a simple ANSI status block for `af daemon status`.
// Uses plain ANSI (not tui-components primitives per issue note — those are REN-1331).
func writeDaemonStatusTable(w io.Writer, r *afclient.DaemonStatusResponse) error {
	statusColor := ansiColor(r.Status)
	uptime := formatUptimeSeconds(r.UptimeSeconds)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	rows := []struct{ label, value string }{
		{"Daemon:", statusColor + string(r.Status) + ansiReset},
		{"Machine:", r.MachineID},
		{"Version:", r.Version},
		{"PID:", fmt.Sprintf("%d", r.PID)},
		{"Uptime:", uptime},
		{"Sessions:", fmt.Sprintf("%d / %d", r.ActiveSessions, r.MaxSessions)},
		{"Projects:", fmt.Sprintf("%d allowed", r.ProjectsAllowed)},
		{"Timestamp:", r.Timestamp},
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\n", row.label, row.value); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	return tw.Flush()
}

// ── logs ──────────────────────────────────────────────────────────────────────

func newDaemonLogsCmd() *cobra.Command {
	var (
		logFile string
		follow  bool
		lines   int
		raw     bool
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail the daemon log file",
		Long: "Stream the daemon log file. NDJSON lines are pretty-printed unless --raw is set.\n" +
			"Uses the file at ~/.rensei/daemon.log by default (configurable with --file).\n" +
			"With --follow (-F) the output streams continuously like `tail -f`.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := logFile
			if path == "" {
				path = defaultDaemonLogFile
			}
			path = expandHomePath(path)

			f, err := os.Open(path) //nolint:gosec // user-supplied path is intentional
			if err != nil {
				return fmt.Errorf("open log file %q: %w", path, err)
			}
			defer func() { _ = f.Close() }()

			out := cmd.OutOrStdout()

			if lines > 0 && !follow {
				// Read the last N lines using a simple buffered scan.
				if err := tailLines(f, out, lines, !raw); err != nil {
					return fmt.Errorf("tail logs: %w", err)
				}
				return nil
			}

			// Stream from current position (beginning of file for non-follow,
			// continuously for follow).
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := scanner.Text()
				printLogLine(out, line, !raw)
			}
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read log: %w", err)
			}

			if !follow {
				return nil
			}

			// Tail -f equivalent: poll for new content.
			for {
				time.Sleep(250 * time.Millisecond)
				for scanner.Scan() {
					line := scanner.Text()
					printLogLine(out, line, !raw)
				}
				if err := scanner.Err(); err != nil {
					return fmt.Errorf("read log: %w", err)
				}
			}
		},
	}

	cmd.Flags().StringVarP(&logFile, "file", "f", "", "Log file path (default: ~/.rensei/daemon.log)")
	cmd.Flags().BoolVarP(&follow, "follow", "F", false, "Stream new log lines as they arrive")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "Number of lines to show (0 = all)")
	cmd.Flags().BoolVar(&raw, "raw", false, "Print raw NDJSON without pretty-printing")

	return cmd
}

// tailLines reads the last n lines from r and writes them to w. If parseJSON
// is true, NDJSON lines are pretty-printed.
func tailLines(r io.Reader, w io.Writer, n int, parseJSON bool) error {
	// Read all lines into a ring buffer of size n.
	var buf []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		buf = append(buf, scanner.Text())
		if len(buf) > n {
			buf = buf[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, line := range buf {
		printLogLine(w, line, parseJSON)
	}
	return nil
}

// printLogLine writes a single log line to w, pretty-printing NDJSON if
// parseJSON is true and the line looks like JSON.
func printLogLine(w io.Writer, line string, parseJSON bool) {
	if parseJSON && len(line) > 0 && line[0] == '{' {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err == nil {
			// Extract common fields for human-readable output.
			ts, _ := obj["time"].(string)
			level, _ := obj["level"].(string)
			msg, _ := obj["msg"].(string)
			if ts != "" || level != "" || msg != "" {
				_, _ = fmt.Fprintf(w, "%s [%s] %s\n",
					truncate(ts, 19),
					strings.ToUpper(padRight(level, 4)),
					msg,
				)
				return
			}
		}
	}
	_, _ = fmt.Fprintln(w, line)
}

// ── doctor ────────────────────────────────────────────────────────────────────

// doctorCheck represents a single health check.
type doctorCheck struct {
	name   string
	result doctorResult
	detail string
}

type doctorResult int

const (
	doctorPass doctorResult = iota
	doctorWarn
	doctorFail
)

func newDaemonDoctorCmd() *cobra.Command {
	var (
		port int
		host string
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks on the local daemon setup",
		Long: "Run a suite of health checks and report pass/warn/fail for each:\n" +
			"  - Daemon binary present on PATH\n" +
			"  - Daemon process reachable via HTTP\n" +
			"  - Daemon config file valid\n" +
			"  - JWT / API token cached\n" +
			"  - Project allowlist non-empty\n" +
			"  - Network reachable (orchestrator ping)\n\n" +
			"Exits 0 when all checks pass, 1 if any fail.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := afclient.DefaultDaemonConfig()
			if host != "" {
				cfg.Host = host
			}
			if port != 0 {
				cfg.Port = port
			}

			checks := runDoctorChecks(cfg)
			out := cmd.OutOrStdout()
			anyFail := false

			for _, c := range checks {
				icon, color := doctorIcon(c.result)
				if c.result == doctorFail {
					anyFail = true
				}
				_, _ = fmt.Fprintf(out, "  %s%s%s  %s",
					color, icon, ansiReset, c.name)
				if c.detail != "" {
					_, _ = fmt.Fprintf(out, " — %s", c.detail)
				}
				_, _ = fmt.Fprintln(out)
			}

			if anyFail {
				return errors.New("one or more daemon health checks failed")
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")

	return cmd
}

// runDoctorChecks executes all daemon health checks and returns results.
func runDoctorChecks(cfg afclient.DaemonConfig) []doctorCheck {
	checks := []doctorCheck{}

	// 1. Daemon binary present.
	binPath, err := exec.LookPath(defaultDaemonBinary)
	if err != nil {
		checks = append(checks, doctorCheck{
			name:   "Daemon binary",
			result: doctorFail,
			detail: fmt.Sprintf("%q not found on PATH", defaultDaemonBinary),
		})
	} else {
		checks = append(checks, doctorCheck{
			name:   "Daemon binary",
			result: doctorPass,
			detail: binPath,
		})
	}

	// 2. Daemon process reachable via HTTP.
	client := afclient.NewDaemonClient(cfg)
	status, statusErr := client.GetStatus()
	if statusErr != nil {
		checks = append(checks, doctorCheck{
			name:   "Daemon process",
			result: doctorFail,
			detail: fmt.Sprintf("not reachable at %s: %v", cfg.BaseURL(), statusErr),
		})
	} else {
		checks = append(checks, doctorCheck{
			name:   "Daemon process",
			result: doctorPass,
			detail: fmt.Sprintf("running (pid %d, uptime %s)", status.PID, formatUptimeSeconds(status.UptimeSeconds)),
		})
	}

	// 3. Daemon config file valid.
	cfgPath := expandHomePath("~/.rensei/daemon.yaml")
	if _, statErr := os.Stat(cfgPath); statErr != nil {
		checks = append(checks, doctorCheck{
			name:   "Config file",
			result: doctorWarn,
			detail: fmt.Sprintf("%s not found", cfgPath),
		})
	} else {
		checks = append(checks, doctorCheck{
			name:   "Config file",
			result: doctorPass,
			detail: cfgPath,
		})
	}

	// 4. JWT / API token cached (check env vars as a proxy).
	token := os.Getenv("RENSEI_API_TOKEN")
	if token == "" {
		token = os.Getenv("WORKER_API_KEY")
	}
	if token == "" {
		checks = append(checks, doctorCheck{
			name:   "API token",
			result: doctorWarn,
			detail: "RENSEI_API_TOKEN / WORKER_API_KEY not set",
		})
	} else {
		checks = append(checks, doctorCheck{
			name:   "API token",
			result: doctorPass,
			detail: fmt.Sprintf("found (%s…)", token[:minInt(8, len(token))]),
		})
	}

	// 5. Project allowlist non-empty (derive from daemon status if reachable).
	if statusErr == nil {
		if status.ProjectsAllowed == 0 {
			checks = append(checks, doctorCheck{
				name:   "Project allowlist",
				result: doctorWarn,
				detail: "no projects in allowlist — add one with `rensei project allow <repo>`",
			})
		} else {
			checks = append(checks, doctorCheck{
				name:   "Project allowlist",
				result: doctorPass,
				detail: fmt.Sprintf("%d project(s) allowed", status.ProjectsAllowed),
			})
		}
	} else {
		checks = append(checks, doctorCheck{
			name:   "Project allowlist",
			result: doctorWarn,
			detail: "unable to check — daemon not reachable",
		})
	}

	// 6. Network reachability: ping the orchestrator.
	orchestratorURL := resolveOrchestratorURL()
	if pingErr := pingHTTP(orchestratorURL); pingErr != nil {
		checks = append(checks, doctorCheck{
			name:   "Orchestrator network",
			result: doctorWarn,
			detail: fmt.Sprintf("cannot reach %s: %v", orchestratorURL, pingErr),
		})
	} else {
		checks = append(checks, doctorCheck{
			name:   "Orchestrator network",
			result: doctorPass,
			detail: orchestratorURL,
		})
	}

	return checks
}

// resolveOrchestratorURL returns the orchestrator URL from env or a default.
func resolveOrchestratorURL() string {
	if u := os.Getenv("WORKER_API_URL"); u != "" {
		return u
	}
	return "https://platform.rensei.dev"
}

// pingHTTP performs a lightweight HEAD request to check network reachability.
func pingHTTP(rawURL string) error {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Head(rawURL)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func doctorIcon(r doctorResult) (icon, color string) {
	switch r {
	case doctorPass:
		return "✓", "\033[32m" // green
	case doctorWarn:
		return "!", "\033[33m" // yellow
	default:
		return "✗", "\033[31m" // red
	}
}

// ── pause ─────────────────────────────────────────────────────────────────────

func newDaemonPauseCmd(factory daemonClientFactory) *cobra.Command {
	var (
		port int
		host string
	)

	cmd := &cobra.Command{
		Use:   "pause",
		Short: "Pause the daemon (stop accepting new sessions)",
		Long: "Signal the daemon to stop accepting new session assignments while keeping\n" +
			"currently running sessions alive. Use `af daemon resume` to re-enable.",
		SilenceUsage: true,
		RunE: daemonActionRunE("pause", &port, &host, factory, func(c daemonDoer) (*afclient.DaemonActionResponse, error) {
			return c.Pause()
		}),
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")

	return cmd
}

// ── resume ────────────────────────────────────────────────────────────────────

func newDaemonResumeCmd(factory daemonClientFactory) *cobra.Command {
	var (
		port int
		host string
	)

	cmd := &cobra.Command{
		Use:          "resume",
		Short:        "Resume the daemon (re-enable accepting new sessions)",
		Long:         "Signal the daemon to resume accepting new session assignments after a pause.",
		SilenceUsage: true,
		RunE: daemonActionRunE("resume", &port, &host, factory, func(c daemonDoer) (*afclient.DaemonActionResponse, error) {
			return c.Resume()
		}),
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")

	return cmd
}

// ── update ────────────────────────────────────────────────────────────────────

func newDaemonUpdateCmd(factory daemonClientFactory) *cobra.Command {
	var (
		port int
		host string
	)

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Trigger a manual daemon update check",
		Long: "Prompt the daemon to check for a new binary version immediately.\n" +
			"The daemon drains in-flight work before restarting if an update is available.",
		SilenceUsage: true,
		RunE: daemonActionRunE("update", &port, &host, factory, func(c daemonDoer) (*afclient.DaemonActionResponse, error) {
			return c.Update()
		}),
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")

	return cmd
}

// ── drain ─────────────────────────────────────────────────────────────────────

func newDaemonDrainCmd(factory daemonClientFactory) *cobra.Command {
	var (
		port    int
		host    string
		timeout int
	)

	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Gracefully drain in-flight work",
		Long: "Signal the daemon to stop accepting new sessions and wait for in-flight\n" +
			"sessions to complete before exiting. Use --timeout to cap the wait.\n" +
			"0 means use the daemon's configured drain timeout.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := afclient.DefaultDaemonConfig()
			if host != "" {
				cfg.Host = host
			}
			if port != 0 {
				cfg.Port = port
			}
			client := factory(cfg)
			resp, err := client.Drain(timeout)
			if err != nil {
				return fmt.Errorf("daemon drain: %w", err)
			}
			return writeDaemonActionResult(cmd.OutOrStdout(), "drain", resp)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "Max seconds to wait for drain (0 = daemon default)")

	return cmd
}

// ── stop ──────────────────────────────────────────────────────────────────────

func newDaemonStopCmd(factory daemonClientFactory) *cobra.Command {
	var (
		port int
		host string
	)

	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon process",
		Long: "Signal the daemon to stop immediately. In-flight sessions are interrupted.\n" +
			"Use `af daemon drain` first for a graceful shutdown.",
		SilenceUsage: true,
		RunE: daemonActionRunE("stop", &port, &host, factory, func(c daemonDoer) (*afclient.DaemonActionResponse, error) {
			return c.Stop()
		}),
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")

	return cmd
}

// ── stats ─────────────────────────────────────────────────────────────────────

func newDaemonStatsCmd(factory daemonClientFactory) *cobra.Command {
	var (
		port      int
		host      string
		withPool  bool
		byMachine bool
		jsonOut   bool
	)

	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show daemon capacity, pool, and session statistics",
		Long: "Display the daemon's capacity envelope, active sessions, queue depth, and\n" +
			"optionally the workarea pool stats (--pool) or per-machine breakdown (--by-machine).\n" +
			"Renders a human-readable ANSI table by default, or raw JSON with --json.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := afclient.DefaultDaemonConfig()
			if host != "" {
				cfg.Host = host
			}
			if port != 0 {
				cfg.Port = port
			}
			client := factory(cfg)
			resp, err := client.GetStats(withPool, byMachine)
			if err != nil {
				return fmt.Errorf("daemon stats: %w", err)
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			return writeDaemonStatsTable(out, resp)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")
	cmd.Flags().BoolVar(&withPool, "pool", false, "Include workarea pool stats")
	cmd.Flags().BoolVar(&byMachine, "by-machine", false, "Include per-machine breakdown")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output raw JSON (indented)")

	return cmd
}

// writeDaemonStatsTable renders the daemon stats as a simple ANSI table.
func writeDaemonStatsTable(w io.Writer, r *afclient.DaemonStatsResponse) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	rows := []struct{ label, value string }{
		{"Active sessions:", fmt.Sprintf("%d / %d", r.ActiveSessions, r.Capacity.MaxConcurrentSessions)},
		{"Queue depth:", fmt.Sprintf("%d", r.QueueDepth)},
		{"Max vCPU/session:", fmt.Sprintf("%d", r.Capacity.MaxVCpuPerSession)},
		{"Max mem/session:", fmt.Sprintf("%d MB", r.Capacity.MaxMemoryMbPerSession)},
		{"Reserved vCPU:", fmt.Sprintf("%d", r.Capacity.ReservedVCpu)},
		{"Reserved memory:", fmt.Sprintf("%d MB", r.Capacity.ReservedMemoryMb)},
		{"Timestamp:", r.Timestamp},
	}

	for _, row := range rows {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\n", row.label, row.value); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush table: %w", err)
	}

	// Pool section.
	if r.Pool != nil {
		if _, err := fmt.Fprintln(w, "\n  Workarea pool:"); err != nil {
			return fmt.Errorf("write pool header: %w", err)
		}
		ptw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		prows := []struct{ label, value string }{
			{"    Total:", fmt.Sprintf("%d", r.Pool.TotalMembers)},
			{"    Ready:", fmt.Sprintf("%d", r.Pool.ReadyMembers)},
			{"    Acquired:", fmt.Sprintf("%d", r.Pool.AcquiredMembers)},
			{"    Warming:", fmt.Sprintf("%d", r.Pool.WarmingMembers)},
			{"    Invalid:", fmt.Sprintf("%d", r.Pool.InvalidMembers)},
			{"    Disk usage:", fmt.Sprintf("%d MB", r.Pool.TotalDiskUsageMb)},
		}
		for _, row := range prows {
			if _, err := fmt.Fprintf(ptw, "%s\t%s\n", row.label, row.value); err != nil {
				return fmt.Errorf("write pool row: %w", err)
			}
		}
		if err := ptw.Flush(); err != nil {
			return fmt.Errorf("flush pool table: %w", err)
		}
	}

	// Per-machine section.
	if len(r.ByMachine) > 0 {
		if _, err := fmt.Fprintln(w, "\n  Per-machine:"); err != nil {
			return fmt.Errorf("write machine header: %w", err)
		}
		mtw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(mtw, "    MACHINE\tREGION\tSTATUS\tACTIVE\tMAX\tUPTIME"); err != nil {
			return fmt.Errorf("write machine table header: %w", err)
		}
		for _, m := range r.ByMachine {
			color := ansiColor(m.Status)
			if _, err := fmt.Fprintf(mtw, "    %s\t%s\t%s%s%s\t%d\t%d\t%s\n",
				m.ID, m.Region,
				color, string(m.Status), ansiReset,
				m.ActiveSessions, m.Capacity.MaxConcurrentSessions,
				formatUptimeSeconds(m.UptimeSeconds),
			); err != nil {
				return fmt.Errorf("write machine row: %w", err)
			}
		}
		if err := mtw.Flush(); err != nil {
			return fmt.Errorf("flush machine table: %w", err)
		}
	}

	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

const ansiReset = "\033[0m"

// ansiColor returns an ANSI color prefix for a DaemonStatus value.
func ansiColor(status afclient.DaemonStatus) string {
	switch status {
	case afclient.DaemonReady:
		return "\033[32m" // green
	case afclient.DaemonPaused:
		return "\033[33m" // yellow
	case afclient.DaemonDraining, afclient.DaemonUpdating:
		return "\033[36m" // cyan
	case afclient.DaemonStopped:
		return "\033[31m" // red
	default:
		return ""
	}
}

// daemonActionRunE returns a RunE closure for simple action subcommands
// (pause, resume, stop, update) that call a single DaemonClient method.
func daemonActionRunE(
	action string,
	port *int,
	host *string,
	factory daemonClientFactory,
	call func(daemonDoer) (*afclient.DaemonActionResponse, error),
) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		cfg := afclient.DefaultDaemonConfig()
		if *host != "" {
			cfg.Host = *host
		}
		if *port != 0 {
			cfg.Port = *port
		}
		client := factory(cfg)
		resp, err := call(client)
		if err != nil {
			return fmt.Errorf("daemon %s: %w", action, err)
		}
		return writeDaemonActionResult(cmd.OutOrStdout(), action, resp)
	}
}

// writeDaemonActionResult renders the outcome of an action command.
func writeDaemonActionResult(w io.Writer, action string, r *afclient.DaemonActionResponse) error {
	if !r.OK {
		_, _ = fmt.Fprintf(w, "daemon %s: not accepted — %s\n", action, r.Message)
		return fmt.Errorf("daemon %s rejected: %s", action, r.Message)
	}
	_, _ = fmt.Fprintf(w, "daemon %s: %s\n", action, r.Message)
	return nil
}

// formatUptimeSeconds converts seconds to a compact h/m/s string.
func formatUptimeSeconds(secs int64) string {
	if secs <= 0 {
		return "0s"
	}
	d := time.Duration(secs) * time.Second
	h := int64(d / time.Hour)
	m := int64((d % time.Hour) / time.Minute)
	s := int64((d % time.Minute) / time.Second)
	switch {
	case h > 0 && m > 0 && s > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0 && s > 0:
		return fmt.Sprintf("%dh%ds", h, s)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case m > 0 && s > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// truncate returns s truncated to at most n runes.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// padRight pads s with spaces to at least width w.
func padRight(s string, w int) string {
	for len(s) < w {
		s += " "
	}
	return s
}

// minInt returns the smaller of a and b.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
