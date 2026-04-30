// Package installer provides the OS-aware daemon-installer dispatcher.
//
// On macOS, calls flow through installer/launchd. On Linux, calls flow
// through installer/systemd. The host binary's `daemon run` subcommand is
// what gets registered as the service entrypoint (locked REN-1406
// decision — no separate rensei-daemon binary).
//
// This package is the single import surface for `af daemon install`,
// `rensei daemon install`, etc. It is exported so downstream binaries
// (rensei-tui, etc.) can drive the same in-process install flow without
// reimplementing it.
package installer

import (
	"fmt"
	"runtime"

	"github.com/RenseiAI/agentfactory-tui/installer/launchd"
	"github.com/RenseiAI/agentfactory-tui/installer/systemd"
)

// Scope mirrors systemd.Scope for callers that want to set the systemd
// install scope without importing the systemd subpackage directly.
type Scope = systemd.Scope

const (
	// ScopeUser installs a user-scoped systemd unit (Linux) or the
	// per-user LaunchAgent (macOS).
	ScopeUser = systemd.ScopeUser

	// ScopeSystem installs a system-scoped systemd unit (requires sudo).
	// It has no equivalent on macOS — Install returns an error if set on
	// darwin.
	ScopeSystem = systemd.ScopeSystem
)

// InstallOptions are the OS-agnostic options for Install.
type InstallOptions struct {
	// HostBinPath is the absolute path to the host binary (af / rensei /
	// afcli) that exposes `daemon run`. Empty means "use os.Executable()".
	HostBinPath string

	// Scope is the systemd unit scope (Linux only). Ignored on macOS.
	Scope Scope

	// ConfigPath is the daemon config path; sets RENSEI_DAEMON_CONFIG on
	// Linux. Currently unused on macOS.
	ConfigPath string

	// Description overrides the systemd [Unit] Description= field. Ignored
	// on macOS.
	Description string

	// SkipServiceManager skips running launchctl/systemctl after writing
	// the unit file. Useful for tests / CI.
	SkipServiceManager bool
}

// InstallResult is the OS-agnostic outcome of a successful Install.
type InstallResult struct {
	// OS is the GOOS value that was used to dispatch ("darwin" or "linux").
	OS string
	// HostBinPath is the binary path that was registered.
	HostBinPath string
	// ServicePath is the absolute path of the written unit / plist.
	ServicePath string
	// ServiceCommand is the full command line registered as the service
	// entrypoint, e.g. "/usr/local/bin/af daemon run". This is what the
	// runtime port (REN-1408) must implement.
	ServiceCommand string
	// Loaded reports whether the service was successfully registered with
	// the OS service manager. False when SkipServiceManager is set or when
	// the service manager call failed (in which case Install returned an
	// error).
	Loaded bool
}

// UninstallOptions are the OS-agnostic options for Uninstall.
type UninstallOptions struct {
	Scope              Scope
	SkipServiceManager bool
}

// UninstallResult is the OS-agnostic outcome of Uninstall.
type UninstallResult struct {
	OS          string
	ServicePath string
	Removed     bool
}

// DoctorOptions are the OS-agnostic options for Doctor.
type DoctorOptions struct {
	Scope              Scope
	SkipServiceManager bool
}

// DoctorReport is the OS-agnostic outcome of Doctor.
type DoctorReport struct {
	OS string

	// ServicePath is the unit file or plist path inspected.
	ServicePath string

	// Installed reports whether the unit file / plist exists on disk.
	Installed bool

	// Active reports whether the service manager considers the service
	// active/loaded. May be nil on platforms or modes where this can't be
	// determined (e.g. SkipServiceManager).
	Active *bool

	// Detail is a human-readable diagnostic string.
	Detail string
}

// ── Install ─────────────────────────────────────────────────────────────────

// Install dispatches to the OS-appropriate installer.
func Install(opts InstallOptions) (InstallResult, error) {
	switch runtime.GOOS {
	case "darwin":
		return installDarwin(opts)
	case "linux":
		return installLinux(opts)
	default:
		return InstallResult{}, fmt.Errorf("installer: unsupported OS %q (only darwin/linux are supported)", runtime.GOOS)
	}
}

func installDarwin(opts InstallOptions) (InstallResult, error) {
	if opts.Scope == ScopeSystem {
		return InstallResult{}, fmt.Errorf("installer: --system scope is not supported on macOS (LaunchAgents are user-scoped)")
	}
	res, err := launchd.Install(launchd.InstallOptions{
		HostBinPath:   opts.HostBinPath,
		SkipLaunchctl: opts.SkipServiceManager,
	})
	if err != nil {
		return InstallResult{}, err
	}
	return InstallResult{
		OS:             "darwin",
		HostBinPath:    res.HostBinPath,
		ServicePath:    res.PlistPath,
		ServiceCommand: fmt.Sprintf("%s %s", res.HostBinPath, launchd.DaemonSubcommand),
		Loaded:         res.Loaded,
	}, nil
}

func installLinux(opts InstallOptions) (InstallResult, error) {
	scope := opts.Scope
	if scope == "" {
		scope = ScopeUser
	}
	unitPath, err := systemd.Install(systemd.InstallOptions{
		Scope:         scope,
		BinPath:       opts.HostBinPath,
		Description:   opts.Description,
		ConfigPath:    opts.ConfigPath,
		SkipSystemctl: opts.SkipServiceManager,
	})
	if err != nil {
		return InstallResult{}, err
	}
	hostBin, _ := systemd.ResolveHostBinPath(opts.HostBinPath)
	return InstallResult{
		OS:             "linux",
		HostBinPath:    hostBin,
		ServicePath:    unitPath,
		ServiceCommand: fmt.Sprintf("%s %s", hostBin, systemd.DaemonSubcommand),
		Loaded:         !opts.SkipServiceManager,
	}, nil
}

// ── Uninstall ───────────────────────────────────────────────────────────────

// Uninstall dispatches to the OS-appropriate uninstaller.
func Uninstall(opts UninstallOptions) (UninstallResult, error) {
	switch runtime.GOOS {
	case "darwin":
		removed, err := launchd.Uninstall(launchd.UninstallOptions{
			SkipLaunchctl: opts.SkipServiceManager,
		})
		if err != nil {
			return UninstallResult{}, err
		}
		path, _ := launchd.PlistPath()
		return UninstallResult{OS: "darwin", ServicePath: path, Removed: removed}, nil

	case "linux":
		scope := opts.Scope
		if scope == "" {
			scope = ScopeUser
		}
		unitPath, err := systemd.Uninstall(systemd.UninstallOptions{
			Scope:         scope,
			SkipSystemctl: opts.SkipServiceManager,
		})
		if err != nil {
			return UninstallResult{}, err
		}
		return UninstallResult{OS: "linux", ServicePath: unitPath, Removed: true}, nil

	default:
		return UninstallResult{}, fmt.Errorf("installer: unsupported OS %q", runtime.GOOS)
	}
}

// ── Doctor ──────────────────────────────────────────────────────────────────

// Doctor returns a flattened OS-agnostic diagnostic report.
func Doctor(opts DoctorOptions) (DoctorReport, error) {
	switch runtime.GOOS {
	case "darwin":
		path, err := launchd.PlistPath()
		if err != nil {
			return DoctorReport{}, err
		}
		res, err := launchd.Doctor(launchd.DoctorOptions{})
		if err != nil {
			return DoctorReport{}, err
		}
		// Find plist-exists check.
		installed := false
		var loadedCheck *launchd.DoctorCheck
		for i := range res.Checks {
			c := res.Checks[i]
			if c.Name == "plist-exists" {
				installed = c.Passed
			}
			if c.Name == "launchctl-loaded" {
				loadedCheck = &res.Checks[i]
			}
		}
		var active *bool
		if loadedCheck != nil {
			val := loadedCheck.Passed
			active = &val
		}
		detail := "launchd installation report"
		if loadedCheck != nil {
			detail = loadedCheck.Detail
		}
		return DoctorReport{
			OS:          "darwin",
			ServicePath: path,
			Installed:   installed,
			Active:      active,
			Detail:      detail,
		}, nil

	case "linux":
		scope := opts.Scope
		if scope == "" {
			scope = ScopeUser
		}
		res, err := systemd.Doctor(systemd.DoctorOptions{
			Scope:         scope,
			SkipSystemctl: opts.SkipServiceManager,
		})
		if err != nil {
			return DoctorReport{}, err
		}
		var active *bool
		if res.IsActive != nil {
			val := *res.IsActive
			active = &val
		}
		return DoctorReport{
			OS:          "linux",
			ServicePath: res.UnitPath,
			Installed:   res.UnitExists,
			Active:      active,
			Detail:      res.StatusOutput,
		}, nil

	default:
		return DoctorReport{}, fmt.Errorf("installer: unsupported OS %q", runtime.GOOS)
	}
}
