package afcli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
	daemonRuntime "github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/installer"
	"github.com/RenseiAI/donmai/internal/anontoken"
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
	GetPoolStats() (*afclient.WorkareaPoolStats, error)
	EvictPool(req afclient.EvictPoolRequest) (*afclient.EvictPoolResponse, error)
	SetCapacityConfig(key, value string) (*afclient.SetCapacityResponse, error)
}

// daemonClientFactory is the type for a constructor that builds a daemonDoer
// from a DaemonConfig. Injected per command-tree for test isolation.
type daemonClientFactory func(cfg afclient.DaemonConfig) daemonDoer

// defaultDaemonFactory is the production factory — always returns a real client.
func defaultDaemonFactory(cfg afclient.DaemonConfig) daemonDoer {
	return afclient.NewDaemonClient(cfg)
}

// defaultDaemonLogFile is the default path for the daemon log file per 011.
const defaultDaemonLogFile = "~/.donmai/daemon.log"

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

// newDaemonCmd constructs the hidden, deprecated `daemon` alias of `host`.
// It carries the identical lifecycle leaves so every existing
// `<bin> daemon <verb>` invocation keeps working unchanged; only the
// deprecation notice is new. The canonical noun is `host` (see host.go).
//
// cfg carries the embedding binary's config (HostBinaryVersion, BinaryName, etc.).
func newDaemonCmd(cfg Config) *cobra.Command {
	return newDaemonCmdWithFactory(defaultDaemonFactory, cfg)
}

// newDaemonCmdWithFactory is the injectable variant used in tests.
func newDaemonCmdWithFactory(factory daemonClientFactory, cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Deprecated alias for `" + bin + " host`",
		Long: "Deprecated alias for `" + bin + " host`.\n\n" +
			"Every subcommand below behaves identically to its `" + bin + " host` equivalent.\n" +
			"Switch to `" + bin + " host` — this alias is removed in " + hostAliasRemovalVersion + ".",
		Hidden:       true,
		SilenceUsage: true,
	}

	addHostLifecycleCommands(cmd, factory, cfg)

	deprecateTree(cmd, bin+" host", hostAliasRemovalVersion)

	return cmd
}

// ── install / uninstall ───────────────────────────────────────────────────────

func newDaemonInstallCmd(bin string) *cobra.Command {
	var (
		binPath            string // --bin-path overrides the host binary path resolved via os.Executable()
		scopeUser          bool   // Linux systemd: --user  (user-scoped unit, default)
		scopeSystem        bool   // Linux systemd: --system (system-scoped unit, requires root)
		skipServiceManager bool   // hidden: --skip-service-manager (test/internal hermetic install)
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the daemon as a system service",
		Long: "Register the " + bin + " binary's `host run` subcommand as a launchd (macOS) or\n" +
			"systemd (Linux) service so it starts automatically at login and survives\n" +
			"reboots.\n\n" +
			"This is implemented in-process — no subprocess shell-out.\n\n" +
			"macOS:\n" +
			"  " + bin + " host install [--bin-path /path/to/" + bin + "]\n\n" +
			"Linux:\n" +
			"  " + bin + " host install --user    (user-scoped systemd unit, default)\n" +
			"  " + bin + " host install --system  (system-scoped systemd unit, requires sudo)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scope := installer.ScopeUser
			switch {
			case scopeSystem && scopeUser:
				return errors.New("cannot specify both --user and --system")
			case scopeSystem:
				scope = installer.ScopeSystem
			}

			res, err := installer.Install(installer.InstallOptions{
				HostBinPath:        binPath,
				Scope:              scope,
				SkipServiceManager: skipServiceManager,
			})
			if err != nil {
				return fmt.Errorf("daemon install: %w", err)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Service registered: %s\n", res.ServicePath)
			_, _ = fmt.Fprintf(out, "Service command: %s\n", res.ServiceCommand)

			// Wipe any cached JWT so the daemon's first boot performs a
			// fresh registration handshake with the orchestrator. Without
			// this, install on a machine that previously ran the daemon
			// would short-circuit Register() on the stale cache and
			// poll a workerId the orchestrator no longer recognizes —
			// producing a "Worker not found" 404 loop forever. The wipe
			// is non-fatal: a write-permission failure prints a warning
			// but doesn't abort install.
			wiped, wipeErr := daemonRuntime.WipeCachedJWT(daemonRuntime.DefaultJWTPath())
			if wipeErr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: failed to wipe cached daemon JWT: %v\n", wipeErr)
			} else if wiped {
				_, _ = fmt.Fprintf(out, "Cleared cached JWT at %s.\n", daemonRuntime.DefaultJWTPath())
			}

			// Mint (or read) the machine's anon token so the user can
			// connect this machine to the dashboard on first install.
			dashBaseURL := os.Getenv("DONMAI_API_URL")
			if dashBaseURL == "" {
				dashBaseURL = "https://donmai.dev/dashboard"
			}
			tok, justMinted, tokErr := anontoken.EnsureToken()
			if tokErr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: failed to ensure machine token: %v\n", tokErr)
			} else if justMinted {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "First run: connect this machine to the dashboard:")
				_, _ = fmt.Fprintf(out, "  %s\n", anontoken.ClaimURL(tok, dashBaseURL))
			}

			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintln(out,
				"The Go daemon runtime is now in-binary: the registered\n"+
					"service launches `host run` directly. Run `host status` to\n"+
					"verify the HTTP control API once the service has started.")
			return nil
		},
	}

	cmd.Flags().StringVar(&binPath, "bin-path", "", "Path to the host binary (default: current executable)")
	cmd.Flags().BoolVar(&scopeUser, "user", false, "Install as user-scoped systemd unit (Linux)")
	cmd.Flags().BoolVar(&scopeSystem, "system", false, "Install as system-scoped systemd unit, requires sudo (Linux)")
	cmd.Flags().BoolVar(&skipServiceManager, "skip-service-manager", false,
		"(internal/test) skip launchctl/systemctl invocation; write the unit file only")
	_ = cmd.Flags().MarkHidden("skip-service-manager")

	return cmd
}

func newDaemonUninstallCmd(bin string) *cobra.Command {
	var (
		scopeUser          bool // Linux systemd: --user  (user-scoped unit, default)
		scopeSystem        bool // Linux systemd: --system (system-scoped unit, requires root)
		skipServiceManager bool // hidden: --skip-service-manager (test/internal hermetic uninstall)
	)

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the daemon system service",
		Long: "Remove the launchd (macOS) or systemd (Linux) service registration.\n\n" +
			"This is implemented in-process — no subprocess shell-out.\n\n" +
			"macOS:\n" +
			"  " + bin + " host uninstall\n\n" +
			"Linux:\n" +
			"  " + bin + " host uninstall --user    (user-scoped systemd unit, default)\n" +
			"  " + bin + " host uninstall --system  (system-scoped systemd unit, requires sudo)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scope := installer.ScopeUser
			switch {
			case scopeSystem && scopeUser:
				return errors.New("cannot specify both --user and --system")
			case scopeSystem:
				scope = installer.ScopeSystem
			}

			res, err := installer.Uninstall(installer.UninstallOptions{
				Scope:              scope,
				SkipServiceManager: skipServiceManager,
			})
			if err != nil {
				return fmt.Errorf("daemon uninstall: %w", err)
			}
			out := cmd.OutOrStdout()
			if res.Removed {
				_, _ = fmt.Fprintf(out, "Service uninstalled: %s\n", res.ServicePath)
			} else {
				_, _ = fmt.Fprintf(out, "No service was registered at %s\n", res.ServicePath)
			}

			// Wipe the cached JWT so a subsequent install starts clean,
			// even when the install reuses an existing daemon.yaml. The
			// JWT cache is only meaningful while paired with a live
			// orchestrator-side worker registration; once the service is
			// gone, the cache is dead weight that re-triggers the
			// "Worker not found" 404 loop on the next install.
			wiped, wipeErr := daemonRuntime.WipeCachedJWT(daemonRuntime.DefaultJWTPath())
			if wipeErr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: failed to wipe cached daemon JWT: %v\n", wipeErr)
			} else if wiped {
				_, _ = fmt.Fprintf(out, "Cleared cached JWT at %s.\n", daemonRuntime.DefaultJWTPath())
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&scopeUser, "user", false, "Uninstall user-scoped systemd unit (Linux)")
	cmd.Flags().BoolVar(&scopeSystem, "system", false, "Uninstall system-scoped systemd unit, requires sudo (Linux)")
	cmd.Flags().BoolVar(&skipServiceManager, "skip-service-manager", false,
		"(internal/test) skip launchctl/systemctl invocation; remove the unit file only")
	_ = cmd.Flags().MarkHidden("skip-service-manager")

	return cmd
}

// ── setup ─────────────────────────────────────────────────────────────────────

func newDaemonSetupCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Interactive first-run wizard",
		Long: "Run the interactive first-run setup wizard that captures machine identity, capacity,\n" +
			"orchestrator config, project allowlist, and auto-update preferences.\n\n" +
			"In-process Go implementation: the wizard reads stdin/stdout\n" +
			"directly so prompts work correctly when stdin is a TTY.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := configPath
			if path == "" {
				path = daemonRuntime.DefaultConfigPath()
			}
			existing, err := daemonRuntime.LoadConfig(path)
			if err != nil {
				return fmt.Errorf("read existing config: %w", err)
			}
			cfg, err := daemonRuntime.RunSetupWizard(daemonRuntime.WizardOptions{
				Existing:   existing,
				ConfigPath: path,
				Stdin:      os.Stdin,
				Stdout:     cmd.OutOrStdout(),
			})
			if err != nil {
				return fmt.Errorf("daemon setup: %w", err)
			}
			_ = cfg
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to daemon.yaml (default: ~/.donmai/daemon.yaml)")
	return cmd
}

// ── status ────────────────────────────────────────────────────────────────────

func newDaemonStatusCmd(factory daemonClientFactory, bin string) *cobra.Command {
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
				return daemonDownErr(bin, err)
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

// writeDaemonStatusTable renders a simple ANSI status block for `donmai host status`.
// Uses plain ANSI (not tui-components primitives — those arrive with the shared theming pass).
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
		{"Projects:", formatStatusProjectIDs(r)},
		{"Timestamp:", r.Timestamp},
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\n", row.label, row.value); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	return tw.Flush()
}

func formatStatusProjectIDs(r *afclient.DaemonStatusResponse) string {
	if len(r.AppliedProjectIDs) == 0 {
		return fmt.Sprintf("%d allowed", r.ProjectsAllowed)
	}
	value := fmt.Sprintf("%d applied: %s", len(r.AppliedProjectIDs), strings.Join(r.AppliedProjectIDs, ", "))
	if !slices.Equal(r.EnabledProjectIDs, r.AppliedProjectIDs) {
		value += fmt.Sprintf(" (desired: %s)", strings.Join(r.EnabledProjectIDs, ", "))
	}
	return value
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
			"Uses the file at ~/.donmai/daemon.log by default (configurable with --file).\n" +
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

	cmd.Flags().StringVarP(&logFile, "file", "f", "", "Log file path (default: ~/.donmai/daemon.log)")
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

func newDaemonDoctorCmd(bin string) *cobra.Command {
	var (
		jsonOut     bool
		scopeUser   bool
		scopeSystem bool
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks on the local daemon setup",
		Long: "Run a suite of health checks and report pass/warn/fail for each.\n\n" +
			"This is implemented in-process: the command inspects the\n" +
			"installed launchd plist (macOS) or systemd unit (Linux) directly and\n" +
			"reports whether the service is registered, active, and pointing at the\n" +
			"current host binary.\n\n" +
			"Exits 0 when the unit is installed and healthy, non-zero otherwise.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scope := installer.ScopeUser
			switch {
			case scopeSystem && scopeUser:
				return errors.New("cannot specify both --user and --system")
			case scopeSystem:
				scope = installer.ScopeSystem
			}

			report, err := installer.Doctor(installer.DoctorOptions{Scope: scope})
			if err != nil {
				return fmt.Errorf("daemon doctor: %w", err)
			}

			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}

			binPath := resolveCurrentBinPath()
			binPresent := binPath != ""

			_, _ = fmt.Fprintf(out, "OS:               %s\n", report.OS)
			_, _ = fmt.Fprintf(out, "Service path:     %s\n", report.ServicePath)
			_, _ = fmt.Fprintf(out, "Service installed: %v\n", report.Installed)
			if report.Active != nil {
				_, _ = fmt.Fprintf(out, "Service active:   %v\n", *report.Active)
			} else {
				_, _ = fmt.Fprintf(out, "Service active:   (unknown)\n")
			}
			_, _ = fmt.Fprintf(out, "Host binary:      %s\n", binPath)
			_, _ = fmt.Fprintf(out, "Binary present:   %v\n", binPresent)
			if report.Detail != "" {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, report.Detail)
			}
			if !report.Installed {
				return fmt.Errorf("service is not installed — run `%s host install`", bin)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output the doctor report as JSON")
	cmd.Flags().BoolVar(&scopeUser, "user", false, "Inspect user-scoped systemd unit (Linux, default)")
	cmd.Flags().BoolVar(&scopeSystem, "system", false, "Inspect system-scoped systemd unit (Linux)")

	return cmd
}

// resolveCurrentBinPath returns the absolute path of the currently running
// executable, or "" if it cannot be resolved. Used by `host doctor` to
// report binary-presence based on the actual Go binary the installer
// would register .
func resolveCurrentBinPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if _, err := os.Stat(exe); err != nil {
		return ""
	}
	return exe
}

// ── pause ─────────────────────────────────────────────────────────────────────

func newDaemonPauseCmd(factory daemonClientFactory, bin string) *cobra.Command {
	var (
		port int
		host string
	)

	cmd := &cobra.Command{
		Use:   "pause",
		Short: "Pause the daemon (stop accepting new sessions)",
		Long: "Signal the daemon to stop accepting new session assignments while keeping\n" +
			"currently running sessions alive. Use `" + bin + " host resume` to re-enable.",
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

func newDaemonStopCmd(factory daemonClientFactory, bin string) *cobra.Command {
	var (
		port int
		host string
	)

	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon process",
		Long: "Signal the daemon to stop immediately. In-flight sessions are interrupted.\n" +
			"Use `" + bin + " host drain` first for a graceful shutdown.",
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

func newDaemonStatsCmd(factory daemonClientFactory, bin string) *cobra.Command {
	var (
		port      int
		host      string
		withPool  bool
		byMachine bool
		jsonOut   bool
	)

	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show daemon capacity, workarea-cache, and session statistics",
		Long: "Display the daemon's capacity envelope, active sessions, queue depth, and\n" +
			"optionally the workarea cache stats (--workarea) or per-machine breakdown (--by-machine).\n" +
			"Renders a human-readable ANSI table by default, or raw JSON with --json.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			warnDeprecatedFlag(cmd, "pool", "--pool", "--workarea")
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
				return daemonDownErr(bin, err)
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			return writeDaemonStatsTable(out, resp, bin)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")
	cmd.Flags().BoolVar(&withPool, "workarea", false, "Include workarea cache stats")
	// Deprecated alias of --workarea, removed in
	// afclient.WorkareaAliasRemovalVersion. Both flags write the same variable,
	// so the alias is a second name for one behaviour rather than a second code
	// path that can drift.
	//
	// MarkHidden + an explicit notice, not pflag's MarkDeprecated: pflag prints
	// its deprecation line to the command's *out* writer, which would land the
	// notice in the middle of `--json` output. Announcing on stderr is the same
	// discipline warnDeprecatedAlias applies to the `daemon` command alias.
	cmd.Flags().BoolVar(&withPool, "pool", false, "Include workarea cache stats")
	_ = cmd.Flags().MarkHidden("pool")
	cmd.Flags().BoolVar(&byMachine, "by-machine", false, "Include per-machine breakdown")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output raw JSON (indented)")

	return cmd
}

// writeDaemonStatsTable renders the daemon stats as a simple ANSI table.
// bin is the host binary name used in remediation hints (e.g. "donmai" or "rensei").
func writeDaemonStatsTable(w io.Writer, r *afclient.DaemonStatsResponse, bin string) error {
	if bin == "" {
		bin = "donmai"
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	rows := []struct{ label, value string }{
		{"Worker:", formatWorkerStat(r)},
		{"Registration:", formatRegistrationStat(r)},
		{"Allowed projects:", formatAllowedProjectsStat(r, bin)},
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

	// Workarea-cache section. Read through the accessor so a daemon that
	// still emits only the deprecated `pool` key still renders.
	if workarea := r.WorkareaStats(); workarea != nil {
		if err := writePoolStatsSection(w, workarea); err != nil {
			return err
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

// ── pool rendering ────────────────────────────────────────────────────────────

// poolKey is the (repository, toolchainKey) grouping key used for the
// per-(repo, toolchain) pool table.
type poolKey struct {
	repo      string
	toolchain string
}

// writePoolStatsSection renders the workarea pool stats block.
// It emits:
//  1. Aggregate totals.
//  2. Per-(repo, toolchain) breakdown table sorted by repo+toolchain.
//
// Simple ANSI rendering only — WorkareaPoolPanel from tui-components v0.2.0
// is deferred to the shared tui-components theming pass.
func writePoolStatsSection(w io.Writer, p *afclient.WorkareaPoolStats) error {
	if _, err := fmt.Fprintln(w, "\n  Workarea pool:"); err != nil {
		return fmt.Errorf("write pool header: %w", err)
	}

	// Aggregate summary.
	ptw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	prows := []struct{ label, value string }{
		{"    Total:", fmt.Sprintf("%d", p.TotalMembers)},
		{"    Ready:", fmt.Sprintf("%d", p.ReadyMembers)},
		{"    Acquired:", fmt.Sprintf("%d", p.AcquiredMembers)},
		{"    Warming:", fmt.Sprintf("%d", p.WarmingMembers)},
		{"    Invalid:", fmt.Sprintf("%d", p.InvalidMembers)},
		{"    Disk usage:", fmt.Sprintf("%d MB", p.TotalDiskUsageMb)},
	}
	for _, row := range prows {
		if _, err := fmt.Fprintf(ptw, "%s\t%s\n", row.label, row.value); err != nil {
			return fmt.Errorf("write pool row: %w", err)
		}
	}
	if err := ptw.Flush(); err != nil {
		return fmt.Errorf("flush pool table: %w", err)
	}

	// Per-(repo, toolchain) breakdown.
	if len(p.Members) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\n    By (repo, toolchain):"); err != nil {
		return fmt.Errorf("write pool key header: %w", err)
	}
	// Aggregate members into per-key buckets.
	type bucketStats struct {
		total    int
		ready    int
		acquired int
		warming  int
		invalid  int
		diskMb   int64
	}
	buckets := map[poolKey]*bucketStats{}
	for i := range p.Members {
		m := &p.Members[i]
		k := poolKey{repo: m.Repository, toolchain: m.ToolchainKey}
		b := buckets[k]
		if b == nil {
			b = &bucketStats{}
			buckets[k] = b
		}
		b.total++
		b.diskMb += m.DiskUsageMb
		switch m.Status {
		case afclient.PoolMemberReady:
			b.ready++
		case afclient.PoolMemberAcquired:
			b.acquired++
		case afclient.PoolMemberWarming:
			b.warming++
		case afclient.PoolMemberInvalid:
			b.invalid++
		}
	}
	// Sort keys for deterministic output.
	keys := make([]poolKey, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].toolchain < keys[j].toolchain
	})
	ktw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(ktw, "    REPO\tTOOLCHAIN\tTOTAL\tREADY\tACQUIRED\tWARMING\tINVALID\tDISK MB"); err != nil {
		return fmt.Errorf("write pool key header row: %w", err)
	}
	for _, k := range keys {
		b := buckets[k]
		if _, err := fmt.Fprintf(ktw, "    %s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\n",
			k.repo, k.toolchain,
			b.total, b.ready, b.acquired, b.warming, b.invalid, b.diskMb,
		); err != nil {
			return fmt.Errorf("write pool key row: %w", err)
		}
	}
	if err := ktw.Flush(); err != nil {
		return fmt.Errorf("flush pool key table: %w", err)
	}
	return nil
}

// ── evict ─────────────────────────────────────────────────────────────────────

// newDaemonEvictCmd returns the `donmai host evict` command.
// Usage: donmai host evict --repo <url> --older-than <duration>
func newDaemonEvictCmd(factory daemonClientFactory) *cobra.Command {
	var (
		port      int
		host      string
		repoURL   string
		olderThan string
		jsonOut   bool
	)

	cmd := &cobra.Command{
		Use:   "evict",
		Short: "Manually evict workarea pool members",
		Long: "Schedule pool members for destruction based on age and repository.\n\n" +
			"All ready (cold) pool members for --repo that were last acquired (or\n" +
			"created, if never acquired) more than --older-than ago are retired.\n" +
			"In-use (acquired) members are not evicted.\n\n" +
			"The daemon emits a Layer 6 hook event for each eviction; the correlation\n" +
			"ID is printed so observability dashboards can join it.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repoURL == "" {
				return errors.New("--repo is required")
			}
			if olderThan == "" {
				return errors.New("--older-than is required")
			}
			d, err := time.ParseDuration(olderThan)
			if err != nil {
				return fmt.Errorf("--older-than: invalid duration %q: %w", olderThan, err)
			}
			if d <= 0 {
				return fmt.Errorf("--older-than: duration must be positive, got %s", olderThan)
			}

			cfg := afclient.DefaultDaemonConfig()
			if host != "" {
				cfg.Host = host
			}
			if port != 0 {
				cfg.Port = port
			}
			client := factory(cfg)

			req := afclient.EvictPoolRequest{
				RepoURL:          repoURL,
				OlderThanSeconds: int64(d.Seconds()),
			}
			resp, err := client.EvictPool(req)
			if err != nil {
				return fmt.Errorf("daemon evict: %w", err)
			}

			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			_, _ = fmt.Fprintf(out, "evicted %d pool member(s): %s\n", resp.Evicted, resp.Message)
			if resp.CorrelationID != "" {
				_, _ = fmt.Fprintf(out, "correlation-id: %s\n", resp.CorrelationID)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")
	cmd.Flags().StringVar(&repoURL, "repo", "", "Repository URL to evict (required)")
	cmd.Flags().StringVar(&olderThan, "older-than", "", "Evict members older than this duration, e.g. 24h (required)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output raw JSON (indented)")

	return cmd
}

// ── set ───────────────────────────────────────────────────────────────────────

// allowedCapacityKeys is the set of dotted config keys accepted by `donmai host set`.
var allowedCapacityKeys = map[string]struct{}{
	"capacity.maxConcurrentSessions":    {},
	afclient.WorkareaMaxDiskGbKey:       {},
	afclient.LegacyWorkareaMaxDiskGbKey: {}, // deprecated alias, see canonicalCapacityKey
}

// warnDeprecatedFlag announces a deprecated flag alias on stderr, but only when
// the alias was actually supplied — a notice on every invocation trains
// operators to ignore it.
//
// Stderr, not stdout: these leaves support `--json`, and a notice written to
// the out stream would break the caller's parser. pflag's own MarkDeprecated
// printer writes to the command's out writer, which is why it is not used here.
func warnDeprecatedFlag(cmd *cobra.Command, name, alias, replacement string) {
	if !cmd.Flags().Changed(name) {
		return
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n",
		afclient.DeprecatedSurfaceNotice(alias, replacement))
}

// canonicalCapacityKey resolves a caller-supplied capacity key to the current
// spelling, announcing the substitution on stderr when a deprecated alias was
// used. The alias is removed in afclient.WorkareaAliasRemovalVersion.
func canonicalCapacityKey(errOut io.Writer, key string) string {
	if key == afclient.LegacyWorkareaMaxDiskGbKey {
		_, _ = fmt.Fprintf(errOut, "warning: %s\n",
			afclient.DeprecatedSurfaceNotice(
				afclient.LegacyWorkareaMaxDiskGbKey, afclient.WorkareaMaxDiskGbKey))
		return afclient.WorkareaMaxDiskGbKey
	}
	return key
}

// newDaemonSetCmd returns the `donmai host set` command.
// Usage: donmai host set <capacity key> <N>
func newDaemonSetCmd(factory daemonClientFactory) *cobra.Command {
	var (
		port    int
		host    string
		jsonOut bool
		cfgPath string
	)

	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a daemon configuration value",
		Long: "Set a daemon configuration key to a new value.\n\n" +
			"Supported keys:\n" +
			"  capacity.maxConcurrentSessions  Maximum concurrently accepted sessions\n" +
			"                                  for this local daemon (0 = accept none).\n" +
			"  capacity.workareaMaxDiskGb      Maximum total workarea-cache disk usage in GiB\n" +
			"                                  before LRU eviction triggers (0 = no limit).\n\n" +
			"The change is written atomically to ~/.donmai/daemon.yaml and the daemon\n" +
			"reloads the affected subsystem without a restart.",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			value := args[1]

			if _, ok := allowedCapacityKeys[key]; !ok {
				return fmt.Errorf("unknown config key %q — supported keys: capacity.maxConcurrentSessions, %s",
					key, afclient.WorkareaMaxDiskGbKey)
			}
			key = canonicalCapacityKey(cmd.ErrOrStderr(), key)

			// Validate integer capacity keys locally so we give a clear error
			// before hitting the daemon.
			if key == afclient.WorkareaMaxDiskGbKey || key == "capacity.maxConcurrentSessions" {
				n, err := strconv.Atoi(value)
				if err != nil || n < 0 {
					return fmt.Errorf("%s must be a non-negative integer, got %q", key, value)
				}
				// Write the value to daemon.yaml atomically via WriteDaemonYAML.
				// The daemon also accepts it via HTTP; we do the local write so
				// the config persists even if the daemon is not running.
				yamlPath := cfgPath
				if yamlPath == "" {
					yamlPath = afclient.DefaultDaemonYAMLPath()
				}
				cfg, readErr := afclient.ReadDaemonYAML(yamlPath)
				if readErr != nil {
					return fmt.Errorf("read daemon config: %w", readErr)
				}
				switch key {
				case "capacity.maxConcurrentSessions":
					cfg.Capacity.MaxConcurrentSessions = n
				case afclient.WorkareaMaxDiskGbKey:
					cfg.Capacity.PoolMaxDiskGb = n
				}
				if writeErr := afclient.WriteDaemonYAML(yamlPath, cfg); writeErr != nil {
					return fmt.Errorf("write daemon config: %w", writeErr)
				}
			}

			// Also notify the running daemon (best-effort; ignore if not reachable).
			httpCfg := afclient.DefaultDaemonConfig()
			if host != "" {
				httpCfg.Host = host
			}
			if port != 0 {
				httpCfg.Port = port
			}
			client := factory(httpCfg)
			resp, err := client.SetCapacityConfig(key, value)
			if err != nil {
				// Daemon is not running — the local YAML write is sufficient.
				out := cmd.OutOrStdout()
				_, _ = fmt.Fprintf(out, "set %s=%s (daemon not reachable; config written to disk)\n", key, value)
				return nil
			}

			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			if !resp.OK {
				return fmt.Errorf("daemon rejected set %s: %s", key, resp.Message)
			}
			_, _ = fmt.Fprintf(out, "set %s=%s: %s\n", resp.Key, resp.Value, resp.Message)
			return nil
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output raw JSON (indented)")
	cmd.Flags().StringVar(&cfgPath, "config", "", "Path to daemon.yaml (default: ~/.donmai/daemon.yaml)")

	return cmd
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
		_, _ = fmt.Fprintf(w, "daemon %s: not accepted: %s\n", action, r.Message)
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

// formatWorkerStat renders the "Worker:" row of `host stats`. Returns a
// short description of the worker id with stub annotation when applicable.
func formatWorkerStat(r *afclient.DaemonStatsResponse) string {
	if r == nil || r.WorkerID == "" {
		return "(unregistered)"
	}
	if strings.HasSuffix(r.WorkerID, "-stub") {
		return r.WorkerID + " (stub)"
	}
	return r.WorkerID
}

// formatRegistrationStat renders the "Registration:" row of `host stats`.
func formatRegistrationStat(r *afclient.DaemonStatsResponse) string {
	if r == nil || r.Registration == nil {
		return "(unknown)"
	}
	reg := r.Registration
	parts := []string{}
	if reg.Status != "" {
		parts = append(parts, reg.Status)
	}
	if reg.HeartbeatRunning {
		parts = append(parts, "heartbeat=running")
	} else {
		parts = append(parts, "heartbeat=stopped")
	}
	if reg.PollRunning {
		parts = append(parts, "poll=running")
	} else {
		parts = append(parts, "poll=stopped")
	}
	if reg.LastHeartbeatAt != "" {
		parts = append(parts, "last-heartbeat="+reg.LastHeartbeatAt)
	}
	return strings.Join(parts, " · ")
}

// formatAllowedProjectsStat renders the "Allowed projects:" row of
// `host stats`. The output is the count followed by a comma-separated
// list of applied project IDs (truncated for very long lists). Older daemons
// that do not expose IDs fall back to repository URLs.
// bin is the host binary name used in the remediation hint.
func formatAllowedProjectsStat(r *afclient.DaemonStatsResponse, bin string) string {
	if r == nil {
		return "0 (none allowed; run `" + bin + " project allow <repo-url>`)"
	}
	projects := r.AppliedProjectIDs
	if len(projects) == 0 {
		projects = r.EnabledProjectIDs
	}
	if len(projects) == 0 {
		projects = r.AllowedProjects
	}
	if len(projects) == 0 {
		return "0 (none allowed; run `" + bin + " project allow <repo-url>`)"
	}
	const maxShown = 6
	count := len(projects)
	if count <= maxShown {
		return fmt.Sprintf("%d: %s", count, strings.Join(projects, ", "))
	}
	shown := strings.Join(projects[:maxShown], ", ")
	return fmt.Sprintf("%d: %s, (+%d more)", count, shown, count-maxShown)
}
