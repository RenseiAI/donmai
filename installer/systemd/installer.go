// Package systemd implements the Linux systemd installer for the daemon.
//
// This is the Go port of the legacy TypeScript installer at
// agentfactory/packages/daemon/src/installer-linux.ts (REN-1293).
//
// The installer registers the host binary's `daemon run` subcommand as the
// service entrypoint — it does NOT register a separate `rensei-daemon`
// binary (locked decision per REN-1406). The host binary is whichever
// executable invoked the install (`af`, `rensei`, etc., resolved via
// os.Executable), and the subcommand is `daemon run`.
//
// Architecture reference:
//
//	rensei-architecture/011-local-daemon-fleet.md §Linux (systemd)
//
// Two scopes are supported:
//
//	--user   (default) — user-scoped unit at ~/.config/systemd/user/
//	                     Managed via `systemctl --user`.
//	                     Logs visible via `journalctl --user -u rensei-daemon`.
//
//	--system           — system-scoped unit at /etc/systemd/system/
//	                     Requires root (sudo).
//	                     Logs visible via `journalctl -u rensei-daemon`.
//
// Restart contract (exit code 3):
//
//	The unit file uses SuccessExitStatus=3 so that exit code 3 (the daemon's
//	EXIT_CODE_RESTART contract) is treated as a clean restart request — the
//	crash counter is not incremented but systemd still restarts the process.
package systemd

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// ── Constants ────────────────────────────────────────────────────────────────

const (
	// UnitName is the systemd unit name (without the .service suffix).
	UnitName = "rensei-daemon"

	// UnitFilename is the systemd unit filename.
	UnitFilename = UnitName + ".service"

	// SystemUnitDir is the system-scope unit directory (requires root).
	SystemUnitDir = "/etc/systemd/system"

	// DefaultDescription is the default [Unit] Description= field value.
	DefaultDescription = "Rensei local daemon — worker pool"

	// DaemonSubcommand is the subcommand the host binary registers for the
	// service entrypoint. The locked decision (REN-1406) is to register
	// `<host-binary> daemon run`, NOT a separate rensei-daemon binary.
	DaemonSubcommand = "daemon run"
)

// Scope is the systemd unit scope: user or system.
type Scope string

const (
	// ScopeUser is a user-scoped unit at ~/.config/systemd/user/.
	ScopeUser Scope = "user"

	// ScopeSystem is a system-scoped unit at /etc/systemd/system/. Requires root.
	ScopeSystem Scope = "system"
)

// UserUnitDir returns the user-scope unit directory: ~/.config/systemd/user.
func UserUnitDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("systemd: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// UnitDir returns the unit directory for the given scope.
func UnitDir(scope Scope) (string, error) {
	switch scope {
	case ScopeUser:
		return UserUnitDir()
	case ScopeSystem:
		return SystemUnitDir, nil
	default:
		return "", fmt.Errorf("systemd: unknown scope %q", scope)
	}
}

// UnitPath returns the absolute path of the unit file for the given scope.
func UnitPath(scope Scope) (string, error) {
	dir, err := UnitDir(scope)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, UnitFilename), nil
}

// ── Options ──────────────────────────────────────────────────────────────────

// InstallOptions controls Install behaviour.
type InstallOptions struct {
	// Scope is the unit scope: user (default) or system.
	Scope Scope

	// BinPath is the absolute path to the host binary that exposes
	// `daemon run` (typically the running executable). Defaults to
	// os.Executable() when empty.
	BinPath string

	// Description overrides the default [Unit] Description= field.
	Description string

	// ConfigPath is the path to the daemon config file. When non-empty, it
	// is exported as RENSEI_DAEMON_CONFIG via Environment= in the unit.
	ConfigPath string

	// SkipSystemctl skips running systemctl daemon-reload / enable --now after
	// writing the unit file. Useful for tests and CI environments without
	// systemd. Defaults to false (systemctl is invoked in production).
	SkipSystemctl bool

	// Runner is an optional command runner for tests; nil means run real
	// commands via os/exec.
	Runner CommandRunner
}

// UninstallOptions controls Uninstall behaviour.
type UninstallOptions struct {
	Scope         Scope
	SkipSystemctl bool
	Runner        CommandRunner
}

// DoctorOptions controls Doctor behaviour.
type DoctorOptions struct {
	Scope         Scope
	SkipSystemctl bool
	Runner        CommandRunner
}

// DoctorResult is the structured outcome of a systemd health check.
type DoctorResult struct {
	UnitPath     string
	UnitExists   bool
	IsActive     *bool // nil when SkipSystemctl or unit absent
	IsEnabled    *bool // nil when SkipSystemctl or unit absent
	StatusOutput string
	Scope        Scope
}

// CommandRunner abstracts running systemctl/which commands so tests can
// inject deterministic implementations.
type CommandRunner interface {
	// Run executes name with args, returning combined stdout+stderr and the
	// process error.
	Run(name string, args ...string) ([]byte, error)
}

// execRunner runs commands via os/exec. It is the default Runner.
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
//  1. Explicit binPath argument.
//  2. os.Executable() — the currently-running binary, which is the most
//     reliable answer when invoked from `af daemon install` or
//     `rensei daemon install`.
func ResolveHostBinPath(binPath string) (string, error) {
	if binPath != "" {
		return binPath, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("systemd: resolve host binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// Fall back to the unresolved path if EvalSymlinks fails (e.g., on
		// systems where the binary path includes a symlink we can't traverse).
		return exe, nil //nolint:nilerr // best-effort
	}
	return resolved, nil
}

// ── Unit file generation ─────────────────────────────────────────────────────

// GenerateUnitFile renders a systemd .service unit file for the daemon.
//
// The ExecStart line is `<binPath> daemon run` — registering the host
// binary's daemon run subcommand (locked REN-1406 decision). The unit
// file is arch-agnostic; the binary path is resolved at install time.
func GenerateUnitFile(scope Scope, binPath string, opts InstallOptions) (string, error) {
	if binPath == "" {
		return "", fmt.Errorf("systemd: GenerateUnitFile: binPath is required")
	}

	description := opts.Description
	if description == "" {
		description = DefaultDescription
	}

	var lines []string
	lines = append(lines,
		"[Unit]",
		"Description="+description,
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		fmt.Sprintf("ExecStart=%s %s", binPath, DaemonSubcommand),
		"Restart=on-failure",
		"RestartSec=5s",
		// Exit code 3 = EXIT_CODE_RESTART: treat as success so the crash
		// counter is not incremented; because it is NOT in
		// RestartPreventExitStatus, systemd still restarts the daemon.
		"SuccessExitStatus=3",
	)

	if opts.ConfigPath != "" {
		lines = append(lines, "Environment=RENSEI_DAEMON_CONFIG="+opts.ConfigPath)
	}

	lines = append(lines,
		"StandardOutput=journal",
		"StandardError=journal",
		"SyslogIdentifier="+UnitName,
	)

	if scope == ScopeSystem {
		// System-scope unit must run as a specific user; default to the
		// invoking user (the one running `sudo`).
		username, err := currentUsername()
		if err != nil {
			return "", err
		}
		lines = append(lines, "User="+username)
	}

	wantedBy := "default.target"
	if scope == ScopeSystem {
		wantedBy = "multi-user.target"
	}

	lines = append(lines,
		"",
		"[Install]",
		"WantedBy="+wantedBy,
	)

	out := strings.Join(lines, "\n")
	// Collapse 3+ consecutive newlines to 2 (mirrors TS behaviour).
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimRight(out, "\n") + "\n", nil
}

// currentUsername returns the SUDO_USER (when present, so we register the
// invoking user not root) or the OS-reported current user.
func currentUsername() (string, error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		return sudoUser, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("systemd: current user: %w", err)
	}
	return u.Username, nil
}

// ── Install ──────────────────────────────────────────────────────────────────

// Install writes the unit file and enables it via systemctl.
//
// Steps:
//  1. Resolve the host binary path.
//  2. Generate the unit file content.
//  3. Write the unit file (creating parent dirs).
//  4. Run `systemctl [--user] daemon-reload && enable --now` unless skipped.
//
// Returns the absolute path of the written unit file.
func Install(opts InstallOptions) (string, error) {
	scope := opts.Scope
	if scope == "" {
		scope = ScopeUser
	}

	binPath, err := ResolveHostBinPath(opts.BinPath)
	if err != nil {
		return "", err
	}

	unitDir, err := UnitDir(scope)
	if err != nil {
		return "", err
	}
	unitPath := filepath.Join(unitDir, UnitFilename)

	// #nosec G301 -- systemd unit dirs (~/.config/systemd/user, /etc/systemd/system) are 0755 by convention.
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return "", fmt.Errorf("systemd: mkdir %s: %w", unitDir, err)
	}

	content, err := GenerateUnitFile(scope, binPath, opts)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil { //nolint:gosec // unit files must be world-readable
		return "", fmt.Errorf("systemd: write %s: %w", unitPath, err)
	}

	if opts.SkipSystemctl {
		return unitPath, nil
	}

	r := runner(opts.Runner)
	scopeFlags := scopeFlags(scope)

	if out, err := r.Run("systemctl", append(append([]string{}, scopeFlags...), "daemon-reload")...); err != nil {
		return unitPath, fmt.Errorf("systemd: daemon-reload: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := r.Run("systemctl", append(append([]string{}, scopeFlags...), "enable", "--now", UnitFilename)...); err != nil {
		return unitPath, fmt.Errorf("systemd: enable --now: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return unitPath, nil
}

// ── Uninstall ────────────────────────────────────────────────────────────────

// Uninstall disables the unit and removes its file.
//
// Steps:
//  1. Run `systemctl [--user] disable --now rensei-daemon` (best-effort).
//  2. Remove the unit file if it exists.
//  3. Run `systemctl [--user] daemon-reload` (best-effort).
//
// Returns the unit file path, regardless of whether it was present.
func Uninstall(opts UninstallOptions) (string, error) {
	scope := opts.Scope
	if scope == "" {
		scope = ScopeUser
	}

	unitPath, err := UnitPath(scope)
	if err != nil {
		return "", err
	}

	r := runner(opts.Runner)
	scopeFlagsList := scopeFlags(scope)

	if !opts.SkipSystemctl {
		if _, statErr := os.Stat(unitPath); statErr == nil {
			// Best-effort disable; ignore errors so we still remove the file.
			_, _ = r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "disable", "--now", UnitFilename)...)
		}
	}

	if _, statErr := os.Stat(unitPath); statErr == nil {
		if err := os.Remove(unitPath); err != nil {
			return unitPath, fmt.Errorf("systemd: remove %s: %w", unitPath, err)
		}
	}

	if !opts.SkipSystemctl {
		_, _ = r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "daemon-reload")...)
	}

	return unitPath, nil
}

// ── Doctor ───────────────────────────────────────────────────────────────────

// Doctor reports on the systemd installation health.
func Doctor(opts DoctorOptions) (DoctorResult, error) {
	scope := opts.Scope
	if scope == "" {
		scope = ScopeUser
	}

	unitPath, err := UnitPath(scope)
	if err != nil {
		return DoctorResult{}, err
	}

	result := DoctorResult{Scope: scope, UnitPath: unitPath}

	if _, statErr := os.Stat(unitPath); statErr == nil {
		result.UnitExists = true
	}

	if opts.SkipSystemctl || !result.UnitExists {
		return result, nil
	}

	r := runner(opts.Runner)
	scopeFlagsList := scopeFlags(scope)

	if _, runErr := r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "is-active", UnitName)...); runErr == nil {
		t := true
		result.IsActive = &t
	} else {
		f := false
		result.IsActive = &f
	}

	if _, runErr := r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "is-enabled", UnitName)...); runErr == nil {
		t := true
		result.IsEnabled = &t
	} else {
		f := false
		result.IsEnabled = &f
	}

	statusOut, _ := r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "status", "--no-pager", UnitName)...)
	result.StatusOutput = strings.TrimSpace(string(statusOut))

	return result, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func scopeFlags(scope Scope) []string {
	if scope == ScopeUser {
		return []string{"--user"}
	}
	return nil
}
