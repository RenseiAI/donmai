// Package launchd implements the macOS launchd installer for the daemon.
//
// This is the Go port of the legacy TypeScript installer at
// agentfactory/packages/daemon/src/launchd-installer.ts (REN-1292).
//
// The installer registers the host binary's `daemon run` subcommand as the
// LaunchAgent's ProgramArguments — it does NOT register a separate
// `rensei-daemon` binary (locked decision per REN-1406). The host binary
// is whichever executable invoked the install (`af`, `rensei`, etc.,
// resolved via os.Executable), and the subcommand is `daemon run`.
//
// Architecture reference:
//
//	rensei-architecture/011-local-daemon-fleet.md §macOS (launchd)
//
// Plist path:    ~/Library/LaunchAgents/dev.rensei.daemon.plist
// Log path:      ~/Library/Logs/rensei/daemon.log
// Error log:     ~/Library/Logs/rensei/daemon-error.log
//
// Restart contract:
//
//	The plist sets KeepAlive=true so launchd restarts on any exit, with a
//	30s ThrottleInterval to prevent rapid-restart storms on crashes.
package launchd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ── Constants ────────────────────────────────────────────────────────────────

// LaunchdLabel is the label used in the plist and as the launchctl service ID.
const LaunchdLabel = "dev.rensei.daemon"

// DaemonSubcommand is the subcommand the host binary registers for the
// LaunchAgent entrypoint. The locked decision (REN-1406) is to register
// `<host-binary> daemon run`, NOT a separate rensei-daemon binary.
const DaemonSubcommand = "daemon run"

// PlistPath returns the absolute path to the LaunchAgent plist:
// ~/Library/LaunchAgents/dev.rensei.daemon.plist.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("launchd: resolve home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist"), nil
}

// LogDir returns the daemon log directory: ~/Library/Logs/rensei.
func LogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("launchd: resolve home dir: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", "rensei"), nil
}

// LogPath returns the daemon stdout log path: ~/Library/Logs/rensei/daemon.log.
func LogPath() (string, error) {
	dir, err := LogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}

// ErrorLogPath returns the daemon stderr log path:
// ~/Library/Logs/rensei/daemon-error.log.
func ErrorLogPath() (string, error) {
	dir, err := LogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon-error.log"), nil
}

// ── Options ──────────────────────────────────────────────────────────────────

// InstallOptions controls Install behaviour.
type InstallOptions struct {
	// HostBinPath is the absolute path to the host binary that exposes
	// `daemon run` (typically the running executable). Defaults to
	// os.Executable() when empty.
	HostBinPath string

	// PlistPath overrides the output plist path (useful for tests).
	PlistPath string

	// LogPath overrides the stdout log path.
	LogPath string

	// ErrorLogPath overrides the stderr log path.
	ErrorLogPath string

	// SkipLaunchctl skips the `launchctl bootstrap` call after writing the
	// plist (useful for tests / CI).
	SkipLaunchctl bool

	// Runner is an optional command runner for tests; nil means run real
	// commands via os/exec.
	Runner CommandRunner
}

// UninstallOptions controls Uninstall behaviour.
type UninstallOptions struct {
	PlistPath     string
	SkipLaunchctl bool
	Runner        CommandRunner
}

// DoctorOptions controls Doctor behaviour.
type DoctorOptions struct {
	PlistPath string
	Runner    CommandRunner
	// LaunchctlList overrides the function used to obtain `launchctl list
	// <label>` output. When nil, the real launchctl is invoked via Runner.
	LaunchctlList func() (string, error)
}

// DoctorCheck is a single check in a DoctorResult.
type DoctorCheck struct {
	Name   string
	Passed bool
	Detail string
}

// DoctorResult is the structured outcome of a launchd health check.
type DoctorResult struct {
	Healthy bool
	Checks  []DoctorCheck
}

// CommandRunner abstracts running launchctl/which commands so tests can
// inject deterministic implementations.
type CommandRunner interface {
	Run(name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // arguments built locally; not user-controlled
	return cmd.CombinedOutput()
}

func runner(r CommandRunner) CommandRunner {
	if r == nil {
		return execRunner{}
	}
	return r
}

// ── Bin resolution ───────────────────────────────────────────────────────────

// ResolveHostBinPath returns the absolute path to the host binary that
// `daemon run` should be invoked against. Priority:
//
//  1. Explicit hostBinPath argument.
//  2. os.Executable() — the currently-running binary.
func ResolveHostBinPath(hostBinPath string) (string, error) {
	if hostBinPath != "" {
		return hostBinPath, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("launchd: resolve host binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, nil //nolint:nilerr // best-effort
	}
	return resolved, nil
}

// ── Plist generation ─────────────────────────────────────────────────────────

// GeneratePlist returns a launchd plist XML string for the daemon
// LaunchAgent. ProgramArguments registers `<hostBinPath> daemon run` —
// the locked REN-1406 decision (no separate rensei-daemon binary).
//
// Key behaviours encoded in the plist:
//
//   - RunAtLoad = true        — daemon starts when the user logs in.
//   - KeepAlive = true        — launchd restarts the daemon if it exits.
//   - ThrottleInterval = 30   — crash restart throttle (prevents storms).
//   - StandardOutPath / Err   — routes stdio to ~/Library/Logs/rensei/.
//   - EnvironmentVariables    — sets HOME and PATH.
func GeneratePlist(hostBinPath, logPath, errorLogPath string) (string, error) {
	if hostBinPath == "" {
		return "", fmt.Errorf("launchd: GeneratePlist: hostBinPath is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("launchd: resolve home dir: %w", err)
	}

	// Build a sensible PATH covering ~/.local/bin (REN-1462: user-local
	// installs of provider CLIs like `claude` land here when installed
	// via the upstream curl|sh script), Homebrew on Apple Silicon and
	// Intel, then the system bins. Prepending ~/.local/bin keeps the
	// user-scope install winning over a stale system-scope copy, which
	// matters because `claude` self-updates only the path it was
	// invoked from.
	pathEnv := strings.Join([]string{
		filepath.Join(home, ".local", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}, ":")

	// Split daemon run subcommand into separate ProgramArguments entries
	// (launchd requires each argument as its own <string>).
	subArgs := strings.Fields(DaemonSubcommand)

	var argsXML strings.Builder
	argsXML.WriteString("    <string>")
	argsXML.WriteString(escapeXML(hostBinPath))
	argsXML.WriteString("</string>\n")
	for _, a := range subArgs {
		argsXML.WriteString("    <string>")
		argsXML.WriteString(escapeXML(a))
		argsXML.WriteString("</string>\n")
	}

	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + escapeXML(LaunchdLabel) + `</string>

  <key>ProgramArguments</key>
  <array>
` + argsXML.String() + `  </array>

  <key>RunAtLoad</key>
  <true/>

  <key>KeepAlive</key>
  <true/>

  <!-- Throttle crash-restarts to once per 30 seconds. -->
  <key>ThrottleInterval</key>
  <integer>30</integer>

  <key>StandardOutPath</key>
  <string>` + escapeXML(logPath) + `</string>

  <key>StandardErrorPath</key>
  <string>` + escapeXML(errorLogPath) + `</string>

  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>` + escapeXML(home) + `</string>
    <key>PATH</key>
    <string>` + escapeXML(pathEnv) + `</string>
  </dict>
</dict>
</plist>
`
	return plist, nil
}

// escapeXML returns s with the five XML predefined entities escaped.
func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

// ── Install ──────────────────────────────────────────────────────────────────

// InstallResult describes what Install wrote and registered.
type InstallResult struct {
	PlistPath   string
	HostBinPath string
	Loaded      bool
}

// Install writes the plist and bootstraps it via launchctl.
//
// Steps:
//  1. Resolve the host binary path.
//  2. Create ~/Library/Logs/rensei/ if missing.
//  3. Write the plist.
//  4. Run `launchctl bootstrap gui/<uid> <plist>` unless skipped.
//
// Note: we use `launchctl bootstrap` (modern, supported on macOS 10.10+)
// instead of the deprecated `launchctl load -w`.
func Install(opts InstallOptions) (InstallResult, error) {
	hostBin, err := ResolveHostBinPath(opts.HostBinPath)
	if err != nil {
		return InstallResult{}, err
	}

	plistPath := opts.PlistPath
	if plistPath == "" {
		plistPath, err = PlistPath()
		if err != nil {
			return InstallResult{}, err
		}
	}

	logPath := opts.LogPath
	if logPath == "" {
		logPath, err = LogPath()
		if err != nil {
			return InstallResult{}, err
		}
	}
	errorLogPath := opts.ErrorLogPath
	if errorLogPath == "" {
		errorLogPath, err = ErrorLogPath()
		if err != nil {
			return InstallResult{}, err
		}
	}

	// Ensure log dir exists.
	// #nosec G301 -- Library/Logs/rensei must be readable by Console.app (0755 by macOS convention).
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("launchd: mkdir log dir: %w", err)
	}
	// Ensure plist dir exists (normally present, be safe).
	// #nosec G301 -- Library/LaunchAgents is a standard 0755 directory on macOS.
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("launchd: mkdir plist dir: %w", err)
	}

	plistContent, err := GeneratePlist(hostBin, logPath, errorLogPath)
	if err != nil {
		return InstallResult{}, err
	}

	if err := os.WriteFile(plistPath, []byte(plistContent), 0o644); err != nil { //nolint:gosec // plist must be readable by launchd
		return InstallResult{}, fmt.Errorf("launchd: write plist: %w", err)
	}

	res := InstallResult{PlistPath: plistPath, HostBinPath: hostBin}

	if opts.SkipLaunchctl {
		return res, nil
	}

	r := runner(opts.Runner)
	domain := fmt.Sprintf("gui/%d", os.Getuid())

	// Defensive bootout: launchd refuses to bootstrap a label that's already
	// registered, even if the previous registration is stale (e.g. from a
	// failed earlier install attempt that didn't clean up). The exact error
	// shape varies — older macOS prints "service already loaded", current
	// versions print "Bootstrap failed: 5: Input/output error" with no
	// recognizable substring. Rather than match-and-retry, just bootout
	// up front and ignore any error (the call is idempotent — it's a no-op
	// if the label isn't loaded).
	target := fmt.Sprintf("%s/%s", domain, LaunchdLabel)
	_, _ = r.Run("launchctl", "bootout", target)

	if out, runErr := r.Run("launchctl", "bootstrap", domain, plistPath); runErr != nil {
		// Belt-and-suspenders: also accept an "already loaded" stdout
		// message in case the bootout above silently failed and the label
		// is still registered. Anything else is a real failure.
		txt := strings.ToLower(strings.TrimSpace(string(out)))
		if !strings.Contains(txt, "already") {
			return res, fmt.Errorf("launchd: bootstrap: %w (%s)", runErr, strings.TrimSpace(string(out)))
		}
	}
	res.Loaded = true
	return res, nil
}

// ── Uninstall ────────────────────────────────────────────────────────────────

// Uninstall bootstraps-out and removes the plist.
//
// Returns true when the plist was found and removed; false when not present.
func Uninstall(opts UninstallOptions) (bool, error) {
	plistPath := opts.PlistPath
	if plistPath == "" {
		var err error
		plistPath, err = PlistPath()
		if err != nil {
			return false, err
		}
	}

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("launchd: stat plist: %w", err)
	}

	if !opts.SkipLaunchctl {
		r := runner(opts.Runner)
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		// Best-effort: ignore errors so we still remove the plist file.
		_, _ = r.Run("launchctl", "bootout", domain, plistPath)
	}

	if err := os.Remove(plistPath); err != nil {
		return false, fmt.Errorf("launchd: remove plist: %w", err)
	}
	return true, nil
}

// ── Doctor ───────────────────────────────────────────────────────────────────

// pidRegexp matches `"PID" = 12345;` in launchctl list output.
var pidRegexp = regexp.MustCompile(`"PID"\s*=\s*(\d+)`)

// Doctor reports on the launchd installation health.
//
// Checks:
//  1. plist-exists       — plist file present at the expected path.
//  2. launchctl-loaded   — `launchctl list <label>` reports the label.
//  3. daemon-running     — launchctl-reported PID is nonzero.
func Doctor(opts DoctorOptions) (DoctorResult, error) {
	plistPath := opts.PlistPath
	if plistPath == "" {
		var err error
		plistPath, err = PlistPath()
		if err != nil {
			return DoctorResult{}, err
		}
	}

	checks := []DoctorCheck{}

	plistExists := false
	if _, err := os.Stat(plistPath); err == nil {
		plistExists = true
	}
	checks = append(checks, DoctorCheck{
		Name:   "plist-exists",
		Passed: plistExists,
		Detail: func() string {
			if plistExists {
				return "Found at " + plistPath
			}
			return "Not found at " + plistPath + " — run 'rensei daemon install'"
		}(),
	})

	var listOut string
	if opts.LaunchctlList != nil {
		out, err := opts.LaunchctlList()
		if err == nil {
			listOut = out
		}
	} else {
		r := runner(opts.Runner)
		out, _ := r.Run("launchctl", "list", LaunchdLabel)
		listOut = string(out)
	}

	loaded := strings.Contains(listOut, LaunchdLabel)
	loadedDetail := "Service '" + LaunchdLabel + "' is NOT loaded — try 'launchctl bootstrap gui/$(id -u) " + plistPath + "'"
	if loaded {
		loadedDetail = "Service '" + LaunchdLabel + "' is registered with launchd"
	}
	checks = append(checks, DoctorCheck{
		Name:   "launchctl-loaded",
		Passed: loaded,
		Detail: loadedDetail,
	})

	if loaded {
		var pid int
		if m := pidRegexp.FindStringSubmatch(listOut); m != nil {
			pid, _ = strconv.Atoi(m[1])
		}
		running := pid > 0
		runningDetail := "Daemon process is not running (PID = 0). Check logs: ~/Library/Logs/rensei/daemon.log"
		if running {
			runningDetail = fmt.Sprintf("Daemon running with PID %d", pid)
		}
		checks = append(checks, DoctorCheck{
			Name:   "daemon-running",
			Passed: running,
			Detail: runningDetail,
		})
	} else {
		checks = append(checks, DoctorCheck{
			Name:   "daemon-running",
			Passed: false,
			Detail: "Cannot check running state — service is not loaded",
		})
	}

	healthy := true
	for _, c := range checks {
		if !c.Passed {
			healthy = false
			break
		}
	}
	return DoctorResult{Healthy: healthy, Checks: checks}, nil
}
