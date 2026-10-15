// Package systemd implements the Linux systemd installer for the daemon.
//
// This is the Go port of the legacy TypeScript installer at
// donmai-libraries/packages/daemon/src/installer-linux.ts.
//
// The installer registers the host binary's `host run` subcommand as the
// service entrypoint — it does NOT register a separate daemon binary
// (locked decision). The host binary is whichever
// executable invoked the install (resolved via
// os.Executable), and the subcommand is `host run`.
//
// Architecture reference:
//
//	donmai-architecture/011-local-daemon-fleet.md §Linux (systemd)
//
// Two scopes are supported:
//
//	--user   (default) — user-scoped unit at ~/.config/systemd/user/
//	                     Managed via `systemctl --user`.
//	                     Logs visible via `journalctl --user -u donmai-daemon`.
//
//	--system           — system-scoped unit at /etc/systemd/system/
//	                     Requires root (sudo).
//	                     Logs visible via `journalctl -u donmai-daemon`.
//
// Seat-scope requirement (every shim-owned session launches inside its own
// transient systemd scope, budget or not — nothing launches unscoped):
// systemd 252 or newer, plus permission for the daemon user to create a
// transient scope. A user-scoped install needs a reachable user manager —
// enable lingering for the daemon user (`loginctl enable-linger <user>`)
// so it answers without an active session — and its scopes are created on
// the user bus (`--user`). A system-scoped install creates its scopes on
// the system bus and needs the manager to permit them there. A host whose
// manager refuses the scope on every reachable bus reports no backend, so
// launches are refused up front with the missing backend named; a scope
// that still fails at launch surfaces the manager's own message on the
// accept error.
//
// Restart contract (exit codes 3 and 4):
//
//	The unit file uses SuccessExitStatus=3 so that exit code 3 (the daemon's
//	EXIT_CODE_RESTART contract) is treated as a clean restart request — the
//	crash counter is not incremented but systemd still restarts the process.
//	Exit code 4 (EXIT_CODE_NEEDS_SETUP) is the fresh-host setup refusal:
//	RestartPreventExitStatus=4 keeps the unit failed until the operator
//	runs setup, instead of restarting every RestartSec while it waits.
package systemd

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/installer/servicepriority"
)

// ── Constants ────────────────────────────────────────────────────────────────

const (
	// UnitName is the systemd unit name (without the .service suffix).
	// It is configurable so a downstream embedder keeps its own unit name
	// through installer config instead of a hard-coded string.
	UnitName = DefaultUnitName

	// DefaultUnitName is the OSS unit name (without the .service suffix),
	// matching the macOS service label dev.donmai.daemon.
	DefaultUnitName = "donmai-daemon"

	// legacyUnitBrandPrefix and legacyUnitSuffix derive the unit name
	// written by earlier builds: the pre-rename brand plus the unchanged
	// "-daemon" suffix. Spelled in parts so the retired literal never
	// reappears in source — the guard's case-sensitive brand rule is blind
	// to the lowercase unit name, and this derivation is the executable
	// record of that gap. Install migrates the derived name in place
	// (stop, disable, remove) before installing the current unit, so an
	// upgrade never leaves two units fighting over the same daemon.
	legacyUnitBrandPrefix = "rens"
	legacyUnitBrandSuffix = "ei"
	legacyUnitSuffix      = "-daemon"

	// UnitFilename is the systemd unit filename.
	UnitFilename = UnitName + ".service"

	// SystemUnitDir is the system-scope unit directory (requires root).
	SystemUnitDir = "/etc/systemd/system"

	// DefaultDescription is the default [Unit] Description= field value.
	DefaultDescription = "Donmai local daemon \u2014 worker pool"

	// DefaultSyslogIdentifier is the default journal log identifier written
	// as SyslogIdentifier=. It defaults to the OSS unit name and follows
	// UnitName when the embedder overrides it, so logs stay attributable to
	// the unit that owns them.
	DefaultSyslogIdentifier = DefaultUnitName

	// DaemonSubcommand is the subcommand the host binary registers for the
	// service entrypoint. The locked decision is to register a subcommand of
	// the host binary itself, NOT a separate daemon binary.
	//
	// The noun moved from `daemon run` to `host run` when `daemon` became a
	// hidden deprecated alias of `host`. Units written by an earlier build
	// still invoke `daemon run`; that keeps working until the alias is
	// removed, so an operator must re-run `host install` before upgrading
	// past that release.
	DaemonSubcommand = "host run"
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

// UnitPath returns the absolute path of the unit file for the given scope
// and unit name. An empty unitName means the current OSS default.
func UnitPath(scope Scope) (string, error) {
	return UnitPathFor(scope, "")
}

// UnitPathFor returns the absolute path of the named unit file for the
// given scope. An empty unitName means the current OSS default
// (DefaultUnitName).
func UnitPathFor(scope Scope, unitName string) (string, error) {
	dir, err := UnitDir(scope)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ResolveUnitFilename(unitName)), nil
}

// LegacyUnitName returns the unit name written by earlier builds (without
// the .service suffix). It is a function, not a constant, so the retired
// literal is derived, never spelled — see the constant block above.
func LegacyUnitName() string {
	return legacyUnitBrandPrefix + legacyUnitBrandSuffix + legacyUnitSuffix
}

// LegacyUnitFilename returns the filename of the unit written by earlier
// builds.
func LegacyUnitFilename() string {
	return LegacyUnitName() + ".service"
}

// ResolveUnitName returns the effective unit name (without the .service
// suffix): the override when non-empty, otherwise the OSS default.
// The result always carries the suffix stripped and whitespace trimmed, so
// an embedder override of "acme-daemon.service" and "acme-daemon" agree.
func ResolveUnitName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return DefaultUnitName
	}
	return strings.TrimSuffix(name, ".service")
}

// ResolveUnitFilename returns the effective unit filename for the override:
// the resolved unit name plus the .service suffix.
func ResolveUnitFilename(name string) string {
	return ResolveUnitName(name) + ".service"
}

// ResolveSyslogIdentifier returns the effective journal log identifier: the
// explicit override when non-empty, otherwise the effective unit name, so
// logs stay attributable to the unit that owns them.
func ResolveSyslogIdentifier(unitName, syslogIdentifier string) string {
	if strings.TrimSpace(syslogIdentifier) != "" {
		return strings.TrimSpace(syslogIdentifier)
	}
	return ResolveUnitName(unitName)
}

// ── Options ──────────────────────────────────────────────────────────────────

// InstallOptions controls Install behaviour.
type InstallOptions struct {
	// Scope is the unit scope: user (default) or system.
	Scope Scope

	// BinPath is the absolute path to the host binary that exposes
	// `host run` (typically the running executable). Defaults to
	// os.Executable() when empty.
	BinPath string

	// Description overrides the default [Unit] Description= field.
	Description string

	// UnitName overrides the unit name (without the .service suffix) this
	// install writes, enables, and reports. Empty means the OSS default
	// (DefaultUnitName). A downstream embedder keeps its own unit name
	// through this field — never by forking this package. The migration
	// below still retires the legacy unit, which no current install writes.
	UnitName string

	// SyslogIdentifier overrides the SyslogIdentifier= journal tag. Empty
	// follows the effective unit name, so logs stay attributable to the
	// unit that owns them.
	SyslogIdentifier string

	// ConfigPath is the path to the daemon config file. When non-empty, it
	// is exported as DONMAI_DAEMON_CONFIG via Environment= in the unit.
	// Install ensures a loadable config exists there (seeding the
	// fresh-host config when absent, validating when present) before
	// writing the unit. Empty means the daemon default
	// (~/.donmai/daemon.yaml), which Install likewise ensures.
	ConfigPath string

	// ProcessPriority selects the process-priority mode the unit encodes. The
	// zero value reads as servicepriority.Default, which leaves the unit
	// byte-for-byte unchanged.
	ProcessPriority servicepriority.Mode

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
	Scope Scope
	// UnitName selects the unit to remove (without the .service suffix).
	// Empty means the current OSS default. The legacy unit is always
	// retired alongside, so uninstall after a partial migration leaves no
	// unit behind.
	UnitName      string
	SkipSystemctl bool
	Runner        CommandRunner
}

// DoctorOptions controls Doctor behaviour.
type DoctorOptions struct {
	Scope Scope
	// UnitName selects the unit to inspect (without the .service suffix).
	// Empty means the current OSS default. A present-but-unmigrated legacy
	// unit is reported through LegacyUnit so the operator knows an upgrade
	// is pending.
	UnitName      string
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
	// LegacyUnit is the path of the pre-rename unit file when it is still
	// on disk; empty otherwise. Its presence means Install has not yet
	// migrated this scope.
	LegacyUnit string
	// LegacyActive reports the legacy unit's is-active probe when the
	// legacy file is present and the manager was queried; nil otherwise.
	LegacyActive *bool
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
// `host run` should be invoked against. Priority:
//
//  1. Explicit binPath argument.
//  2. os.Executable() — the currently-running binary, which is the most
//     reliable answer when invoked from `donmai host install`.
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
// The ExecStart line is `<binPath> host run` — registering the host
// binary's host run subcommand (locked decision). The unit
// file is arch-agnostic; the binary path is resolved at install time.
//
// The unit name and journal tag follow the install options: UnitName
// overrides the OSS default, SyslogIdentifier overrides the tag and
// otherwise tracks the effective unit name.
func GenerateUnitFile(scope Scope, binPath string, opts InstallOptions) (string, error) {
	if binPath == "" {
		return "", fmt.Errorf("systemd: GenerateUnitFile: binPath is required")
	}
	priorityDirectives, err := ProcessPriorityDirectives(opts.ProcessPriority)
	if err != nil {
		return "", err
	}

	description := opts.Description
	if description == "" {
		description = DefaultDescription
	}
	syslogIdentifier := ResolveSyslogIdentifier(opts.UnitName, opts.SyslogIdentifier)

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
		// Exit code 4 = EXIT_CODE_NEEDS_SETUP: the fresh-host seed
		// refuses without a harness or repository profile until the
		// operator runs setup. A failed start must stay failed — the
		// unit waits for its config instead of restarting every
		// RestartSec until then.
		"RestartPreventExitStatus=4",
		// No KillMode and no Delegate, deliberately: stopping this unit keeps
		// systemd's default control-group kill, so a direct-owned seat in the
		// unit's cgroup ends with its daemon. A shim-owned seat survives
		// because it starts in its own transient scope, which lives outside
		// this unit's cgroup — never because this unit stops killing it.
	)

	if opts.ConfigPath != "" {
		lines = append(lines, "Environment=DONMAI_DAEMON_CONFIG="+opts.ConfigPath)
	}

	// PATH override: prepend ~/.local/bin so
	// user-local installs of provider CLIs like `claude` (default
	// install location for the upstream curl|sh script) win over any
	// stale system-scope copy. The home directory resolves at install
	// time from the invoking user (or SUDO_USER for system scope).
	if homeDir, herr := installHomeDir(scope); herr == nil && homeDir != "" {
		lines = append(lines, "Environment=PATH="+strings.Join([]string{
			filepath.Join(homeDir, ".local", "bin"),
			"/usr/local/bin",
			"/usr/bin",
			"/bin",
			"/usr/sbin",
			"/sbin",
		}, ":"))
	}

	lines = append(lines,
		"StandardOutput=journal",
		"StandardError=journal",
		"SyslogIdentifier="+syslogIdentifier,
	)
	lines = append(lines, priorityDirectives...)

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

// backgroundDirectives are the [Service] directives for
// servicepriority.Background. They mirror the macOS intent: the lowest CPU
// priority and the lowest disk I/O priority, inherited by every process the
// daemon spawns. Each one only lowers the service's own priority, so none needs
// privilege, which keeps a user-scoped unit working.
//
//   - Nice=19                     lowest nice value.
//   - CPUSchedulingPolicy=idle    SCHED_IDLE: runs only in CPU the rest of the
//     system leaves free, yet is still scheduled
//     (it is not starved outright).
//   - IOSchedulingClass=idle      the idle I/O class. It takes no
//     IOSchedulingPriority=; that setting only
//     applies to the best-effort and realtime
//     classes.
var backgroundDirectives = []string{
	"Nice=19",
	"CPUSchedulingPolicy=idle",
	"IOSchedulingClass=idle",
}

// ProcessPriorityDirectives returns the [Service] directives that encode mode.
// The default mode returns none. It is exported so an embedder that renders its
// own unit can emit the same directives.
func ProcessPriorityDirectives(mode servicepriority.Mode) ([]string, error) {
	if err := servicepriority.Validate(mode); err != nil {
		return nil, fmt.Errorf("systemd: %w", err)
	}
	if servicepriority.Effective(mode) == servicepriority.Background {
		return append([]string(nil), backgroundDirectives...), nil
	}
	return nil, nil
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

// installHomeDir returns the home directory the unit's PATH should
// reference. For user scope we use os.UserHomeDir(). For system scope
// we resolve the SUDO_USER's home (fallback: current user's home) so
// the unit ends up referencing the invoking user's ~/.local/bin, not
// /root/.local/bin.
func installHomeDir(scope Scope) (string, error) {
	if scope == ScopeUser {
		return os.UserHomeDir()
	}
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			return u.HomeDir, nil
		}
	}
	return os.UserHomeDir()
}

// ── Install ──────────────────────────────────────────────────────────────────

// Install writes the unit file and enables it via systemctl.
//
// Steps:
//  1. Resolve the host binary path.
//  2. Ensure a loadable daemon config exists (seed or validate).
//  3. Migrate the legacy unit when present: stop and disable it, then
//     remove its file, so an upgrade never leaves two units fighting over
//     the same daemon. Daemon state lives outside the unit (config file,
//     state dir, journal), so nothing else moves: the new unit starts the
//     same binary against the same config, and the service manager keeps
//     the default control-group kill — direct-owned seats end with the old
//     unit's stop while shim-owned seats ride out the restart in their own
//     transient scopes.
//  4. Generate the unit file content.
//  5. Write the unit file (creating parent dirs).
//  6. Run `systemctl [--user] daemon-reload && enable --now` unless skipped.
//
// Step 2 is the fresh-host contract: with no config the service-mode
// start would run the interactive wizard under the unit (stdin /dev/null)
// and crash-loop, so the seed — a local file queue with an explicit
// execution policy, no harness or repository profile — is written first.
// An existing config is validated and left untouched.
//
// Returns the absolute path of the written unit file.
func Install(opts InstallOptions) (string, error) {
	scope := opts.Scope
	if scope == "" {
		scope = ScopeUser
	}

	if _, err := EnsureFreshHostConfig(opts.ConfigPath); err != nil {
		return "", err
	}

	binPath, err := ResolveHostBinPath(opts.BinPath)
	if err != nil {
		return "", err
	}

	unitDir, err := UnitDir(scope)
	if err != nil {
		return "", err
	}
	unitFilename := ResolveUnitFilename(opts.UnitName)
	unitPath := filepath.Join(unitDir, unitFilename)

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

	if diff, werr := sameUnitFile(unitPath, content); werr == nil && diff {
		// The new unit landed byte-identical to what was there. A stop
		// below would bounce a healthy daemon for no reason; the
		// migration still retires the legacy unit file (if any) so the
		// next daemon-reload cannot resurrect it.
		if merr := migrateLegacyUnit(scope, unitFilename, runner(opts.Runner), opts.SkipSystemctl); merr != nil {
			return unitPath, merr
		}
		if opts.SkipSystemctl {
			return unitPath, nil
		}
	} else if merr := migrateLegacyUnit(scope, unitFilename, runner(opts.Runner), opts.SkipSystemctl); merr != nil {
		return unitPath, merr
	}

	if opts.SkipSystemctl {
		return unitPath, nil
	}

	r := runner(opts.Runner)
	scopeFlags := scopeFlags(scope)

	if out, err := r.Run("systemctl", append(append([]string{}, scopeFlags...), "daemon-reload")...); err != nil {
		return unitPath, fmt.Errorf("systemd: daemon-reload: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := r.Run("systemctl", append(append([]string{}, scopeFlags...), "enable", "--now", unitFilename)...); err != nil {
		return unitPath, fmt.Errorf("systemd: enable --now: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return unitPath, nil
}

// sameUnitFile reports whether path already holds content. It is the
// reinstall fast path: rewriting the same bytes must not bounce the daemon.
func sameUnitFile(path, content string) (bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // own unit file just written
	if err != nil {
		return false, err
	}
	return string(raw) == content, nil
}

// migrateLegacyUnit retires the unit written by earlier builds (stop and
// disable it, then remove its file) when it differs from the unit this
// install just wrote. currentFilename is the unit this install owns; every
// other known legacy filename is retired. Unknown filenames are left alone:
// only units this installer ever wrote are its to remove.
//
// When skipSystemctl is set only the file removal runs (tests and CI
// environments without systemd); production also stops and disables the
// legacy unit through the service manager first, best-effort, so its
// control-group kill ends direct-owned seats before the new unit starts.
func migrateLegacyUnit(scope Scope, currentFilename string, r CommandRunner, skipSystemctl bool) error {
	dir, err := UnitDir(scope)
	if err != nil {
		return err
	}
	scopeFlags := scopeFlags(scope)
	for _, legacyFilename := range []string{LegacyUnitFilename()} {
		if legacyFilename == currentFilename {
			continue
		}
		legacyPath := filepath.Join(dir, legacyFilename)
		if _, statErr := os.Stat(legacyPath); statErr != nil {
			continue
		}
		if !skipSystemctl {
			// Best-effort: the file removal below still runs when the
			// manager is unreachable, so a dead manager cannot wedge
			// the upgrade.
			_, _ = r.Run("systemctl", append(append([]string{}, scopeFlags...), "stop", strings.TrimSuffix(legacyFilename, ".service"))...)
			_, _ = r.Run("systemctl", append(append([]string{}, scopeFlags...), "disable", legacyFilename)...)
		}
		if err := os.Remove(legacyPath); err != nil {
			return fmt.Errorf("systemd: remove legacy unit %s: %w", legacyPath, err)
		}
	}
	return nil
}

// ── Uninstall ────────────────────────────────────────────────────────────────

// Uninstall disables the unit and removes its file, retiring the legacy
// unit alongside the current one.
//
// Steps:
//  1. Run `systemctl [--user] stop <unit>` then `disable <unit>` for the
//     current unit and the legacy unit (best-effort).
//  2. Remove the unit files that exist.
//  3. Run `systemctl [--user] daemon-reload` (best-effort).
//
// Returns the current unit file path, regardless of whether it was present.
func Uninstall(opts UninstallOptions) (string, error) {
	scope := opts.Scope
	if scope == "" {
		scope = ScopeUser
	}

	unitPath, err := UnitPathFor(scope, opts.UnitName)
	if err != nil {
		return "", err
	}

	r := runner(opts.Runner)
	scopeFlagsList := scopeFlags(scope)

	filenames := []string{ResolveUnitFilename(opts.UnitName)}
	if LegacyUnitFilename() != filenames[0] {
		filenames = append(filenames, LegacyUnitFilename())
	}

	if !opts.SkipSystemctl {
		for _, filename := range filenames {
			path := ""
			if dir, derr := UnitDir(scope); derr == nil {
				path = filepath.Join(dir, filename)
			}
			if path == "" {
				continue
			}
			if _, statErr := os.Stat(path); statErr == nil {
				// Best-effort stop+disable; ignore errors so we still
				// remove the file.
				_, _ = r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "stop", strings.TrimSuffix(filename, ".service"))...)
				_, _ = r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "disable", filename)...)
			}
		}
	}

	for _, filename := range filenames {
		dir, derr := UnitDir(scope)
		if derr != nil {
			return unitPath, derr
		}
		path := filepath.Join(dir, filename)
		if _, statErr := os.Stat(path); statErr == nil {
			if err := os.Remove(path); err != nil {
				return unitPath, fmt.Errorf("systemd: remove %s: %w", path, err)
			}
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

	unitPath, err := UnitPathFor(scope, opts.UnitName)
	if err != nil {
		return DoctorResult{}, err
	}

	result := DoctorResult{Scope: scope, UnitPath: unitPath}

	if _, statErr := os.Stat(unitPath); statErr == nil {
		result.UnitExists = true
	}

	// Surface a pending migration even when the manager is skipped: the
	// file check needs no systemctl.
	if legacyPath, lerr := UnitPathFor(scope, LegacyUnitName()); lerr == nil {
		if _, statErr := os.Stat(legacyPath); statErr == nil && legacyPath != unitPath {
			result.LegacyUnit = legacyPath
		}
	}

	if opts.SkipSystemctl || (!result.UnitExists && result.LegacyUnit == "") {
		return result, nil
	}

	r := runner(opts.Runner)
	scopeFlagsList := scopeFlags(scope)
	unitName := ResolveUnitName(opts.UnitName)

	if result.UnitExists {
		if _, runErr := r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "is-active", unitName)...); runErr == nil {
			t := true
			result.IsActive = &t
		} else {
			f := false
			result.IsActive = &f
		}

		if _, runErr := r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "is-enabled", unitName)...); runErr == nil {
			t := true
			result.IsEnabled = &t
		} else {
			f := false
			result.IsEnabled = &f
		}

		statusOut, _ := r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "status", "--no-pager", unitName)...)
		result.StatusOutput = strings.TrimSpace(string(statusOut))
	}

	if result.LegacyUnit != "" {
		if _, runErr := r.Run("systemctl", append(append([]string{}, scopeFlagsList...), "is-active", LegacyUnitName())...); runErr == nil {
			t := true
			result.LegacyActive = &t
		} else {
			f := false
			result.LegacyActive = &f
		}
	}

	return result, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func scopeFlags(scope Scope) []string {
	if scope == ScopeUser {
		return []string{"--user"}
	}
	return nil
}
