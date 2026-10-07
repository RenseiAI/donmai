// Package installer provides the OS-aware daemon-installer dispatcher.
//
// On macOS, calls flow through installer/launchd. On Linux, calls flow
// through installer/systemd. The host binary's `daemon run` subcommand is
// what gets registered as the service entrypoint (locked
// decision — no separate rensei-daemon binary).
//
// This package is the single import surface for `donmai daemon install`,
// `rensei daemon install`, etc. It is exported so downstream binaries
// (rensei-tui, etc.) can drive the same in-process install flow without
// reimplementing it.
package installer

import (
	"fmt"
	"runtime"

	"github.com/RenseiAI/donmai/installer/launchd"
	"github.com/RenseiAI/donmai/installer/servicepriority"
	"github.com/RenseiAI/donmai/installer/systemd"
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

// ProcessPriority is the daemon service's process-priority mode.
type ProcessPriority = servicepriority.Mode

const (
	// ProcessPriorityDefault leaves the service definition unchanged.
	ProcessPriorityDefault = servicepriority.Default
	// ProcessPriorityBackground runs the daemon and everything it spawns at
	// the lowest CPU priority with throttled disk I/O: launchd ProcessType and
	// LowPriorityIO keys on macOS, systemd Nice=/CPUSchedulingPolicy=/
	// IOSchedulingClass= on Linux.
	ProcessPriorityBackground = servicepriority.Background
)

// ProcessPrioritySource says where an install's effective process-priority
// mode came from.
type ProcessPrioritySource string

const (
	// ProcessPrioritySourceExplicit means the caller named the mode. It is
	// saved, so it also governs later installs that name none.
	ProcessPrioritySourceExplicit ProcessPrioritySource = "explicit"
	// ProcessPrioritySourceSaved means the caller named no mode and the mode
	// saved by an earlier explicit install was kept.
	ProcessPrioritySourceSaved ProcessPrioritySource = "saved"
	// ProcessPrioritySourceDefault means nothing was named or saved.
	ProcessPrioritySourceDefault ProcessPrioritySource = "default"
)

// InstallOptions are the OS-agnostic options for Install.
type InstallOptions struct {
	// HostBinPath is the absolute path to the host binary (af / rensei /
	// afcli) that exposes `daemon run`. Empty means "use os.Executable()".
	HostBinPath string

	// Scope is the systemd unit scope (Linux only). Ignored on macOS.
	Scope Scope

	// ConfigPath is the daemon config path; sets DONMAI_DAEMON_CONFIG on
	// Linux. Currently unused on macOS.
	ConfigPath string

	// Description overrides the systemd [Unit] Description= field. Ignored
	// on macOS.
	Description string

	// ProcessPriority is the daemon service's process-priority mode. Only an
	// explicit value changes the mode: the empty value keeps the mode saved by
	// the last explicit install (or the default when none was saved), so a
	// re-install that does not mention it never silently resets it. An explicit
	// value, including ProcessPriorityDefault, is saved for later installs.
	ProcessPriority ProcessPriority

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
	// runtime port must implement.
	ServiceCommand string
	// Loaded reports whether the service was successfully registered with
	// the OS service manager. False when SkipServiceManager is set or when
	// the service manager call failed (in which case Install returned an
	// error).
	Loaded bool
	// ProcessPriority is the mode the written service definition encodes.
	ProcessPriority ProcessPriority
	// ProcessPrioritySource says whether that mode was named by the caller,
	// kept from an earlier install, or is the default.
	ProcessPrioritySource ProcessPrioritySource
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
	if err := servicepriority.Validate(opts.ProcessPriority); err != nil {
		return InstallResult{}, fmt.Errorf("installer: %w", err)
	}
	priority, source, err := resolveProcessPriority(opts.ProcessPriority)
	if err != nil {
		return InstallResult{}, err
	}

	var res InstallResult
	switch runtime.GOOS {
	case "darwin":
		res, err = installDarwin(opts, priority, source)
	case "linux":
		res, err = installLinux(opts, priority, source)
	default:
		return InstallResult{}, fmt.Errorf("installer: unsupported OS %q (only darwin/linux are supported)", runtime.GOOS)
	}
	if err != nil {
		return InstallResult{}, err
	}
	res.ProcessPriority = priority
	res.ProcessPrioritySource = source
	return res, nil
}

// resolveProcessPriority picks the mode an install applies: an explicit mode
// wins, otherwise the mode saved by an earlier explicit install, otherwise the
// default. Saved state that cannot be read is an error, not a silent reset to
// the default.
func resolveProcessPriority(requested ProcessPriority) (ProcessPriority, ProcessPrioritySource, error) {
	if requested != "" {
		return requested, ProcessPrioritySourceExplicit, nil
	}
	saved, found, err := servicepriority.Load()
	if err != nil {
		return "", "", fmt.Errorf("installer: read saved process priority: %w (pass --process-priority to set it explicitly)", err)
	}
	if found {
		return saved, ProcessPrioritySourceSaved, nil
	}
	return ProcessPriorityDefault, ProcessPrioritySourceDefault, nil
}

// savePriorityChoice records an explicit mode so later installs keep it. It
// runs before the service definition is written, so a failure leaves the
// existing service untouched.
func savePriorityChoice(priority ProcessPriority, source ProcessPrioritySource) error {
	if source != ProcessPrioritySourceExplicit {
		return nil
	}
	if err := servicepriority.Save(priority); err != nil {
		return fmt.Errorf("installer: save process priority: %w", err)
	}
	return nil
}

func installDarwin(opts InstallOptions, priority ProcessPriority, source ProcessPrioritySource) (InstallResult, error) {
	if opts.Scope == ScopeSystem {
		return InstallResult{}, fmt.Errorf("installer: --system scope is not supported on macOS (LaunchAgents are user-scoped)")
	}
	if err := savePriorityChoice(priority, source); err != nil {
		return InstallResult{}, err
	}
	res, err := launchd.Install(launchd.InstallOptions{
		HostBinPath:     opts.HostBinPath,
		ProcessPriority: priority,
		SkipLaunchctl:   opts.SkipServiceManager,
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

func installLinux(opts InstallOptions, priority ProcessPriority, source ProcessPrioritySource) (InstallResult, error) {
	scope := opts.Scope
	if scope == "" {
		scope = ScopeUser
	}
	if err := savePriorityChoice(priority, source); err != nil {
		return InstallResult{}, err
	}
	unitPath, err := systemd.Install(systemd.InstallOptions{
		Scope:           scope,
		BinPath:         opts.HostBinPath,
		Description:     opts.Description,
		ConfigPath:      opts.ConfigPath,
		ProcessPriority: priority,
		SkipSystemctl:   opts.SkipServiceManager,
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
