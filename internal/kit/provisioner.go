// provisioner.go — KitProvisioner (K1.3): executes a composed
// ToolchainDemand against an acquired workarea before the agent spawns.
//
// Seam 2 contract (006 § "Seam 2"; 005:357):
//   - ToolchainInstall runs first (base toolchains), then PostAcquire
//     (framework deps), in composition order (foundation-first).
//   - Each command runs in the workarea dir, output streamed under the
//     "kit.provision" log channel.
//   - The FIRST non-zero exit (or exec error) ABORTS: Provision returns a
//     wrapped error so the runner fails the session before spawning the
//     agent ("no partial toolchain"). 005:357 "failure of any aborts".
//   - PreRelease runs on teardown, best-effort (logged, never fatal).
//
// Execution is abstracted behind Execer so the local worktree path
// (exec.CommandContext "sh -c") and a future cloud-sandbox path
// (sbx.exec()) share one provisioner, and so tests can assert ordering /
// abort behaviour with a fake.
package kit

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DefaultInstallTimeout bounds a full Provision run (all install commands +
// post_acquire hooks). Toolchain installs hit the network (apt, brew,
// curl|sh) and can be slow on a cold sandbox; 10 minutes matches the K1
// user-approved default. Override via KitProvisioner.Timeout.
const DefaultInstallTimeout = 10 * time.Minute

// Execer runs a single shell command against a workarea directory. The
// local implementation is execCommandExecer (exec.CommandContext with
// cmd.Dir = dir); cloud providers can implement this over sbx.exec().
//
// Exec returns the command's exit code and a non-nil error when the
// command could not be run at all (binary missing, ctx cancelled). A
// non-zero exitCode with a nil err means the command ran and failed —
// callers treat both as an abort.
type Execer interface {
	Exec(ctx context.Context, dir, command string, env map[string]string) (exitCode int, err error)
}

// KitProvisioner executes a ToolchainDemand against a workarea. The zero
// value is usable (nil logger → slog.Default(), zero Timeout →
// DefaultInstallTimeout); prefer NewKitProvisioner.
type KitProvisioner struct {
	log *slog.Logger
	// Timeout bounds a single Provision call. Zero → DefaultInstallTimeout.
	// Negative disables the provisioner-side timeout (caller owns ctx).
	Timeout time.Duration
}

// NewKitProvisioner constructs a KitProvisioner. A nil logger falls back to
// slog.Default(); Timeout defaults to DefaultInstallTimeout.
func NewKitProvisioner(log *slog.Logger) *KitProvisioner {
	if log == nil {
		log = slog.Default()
	}
	return &KitProvisioner{log: log, Timeout: DefaultInstallTimeout}
}

// logger returns the configured logger or the package default.
func (p *KitProvisioner) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return slog.Default()
}

// Provision runs d.ToolchainInstall then d.PostAcquire in order against
// dir via x. The first non-zero exit or exec error aborts and is returned
// wrapped; remaining commands do NOT run (Seam 2: no partial toolchain).
// A nil/empty demand is a no-op (returns nil) so zero-kit sessions skip
// provisioning entirely.
func (p *KitProvisioner) Provision(ctx context.Context, x Execer, dir string, d *ToolchainDemand) error {
	if d.IsEmpty() {
		return nil
	}
	if x == nil {
		return fmt.Errorf("kit.provision: no Execer wired")
	}

	provCtx := ctx
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		provCtx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	log := p.logger()
	log.Info("kit.provision: start",
		"channel", "kit.provision",
		"dir", dir,
		"os", d.OS,
		"kits", d.Kits,
		"installSteps", len(d.ToolchainInstall),
		"postAcquireSteps", len(d.PostAcquire),
	)

	// Phase 1 — base toolchains, then Phase 2 — post_acquire hooks. One
	// ordered stream; an abort in phase 1 must skip phase 2 (a failed
	// node install must not run `npm ci`).
	steps := make([]provisionStep, 0, len(d.ToolchainInstall)+len(d.PostAcquire))
	for _, c := range d.ToolchainInstall {
		steps = append(steps, provisionStep{phase: "toolchain_install", cmd: c})
	}
	for _, c := range d.PostAcquire {
		steps = append(steps, provisionStep{phase: "post_acquire", cmd: c})
	}

	for i, s := range steps {
		log.Info("kit.provision: run",
			"channel", "kit.provision",
			"phase", s.phase,
			"step", i+1,
			"total", len(steps),
			"command", s.cmd,
		)
		code, err := x.Exec(provCtx, dir, s.cmd, d.Env)
		if err != nil {
			return fmt.Errorf("kit.provision %s failed (exec error): %q: %w", s.phase, s.cmd, err)
		}
		if code != 0 {
			return fmt.Errorf("kit.provision %s failed (exit %d): %q", s.phase, code, s.cmd)
		}
	}

	log.Info("kit.provision: complete", "channel", "kit.provision", "dir", dir)
	return nil
}

// Release runs d.PreRelease against dir, best-effort: a non-zero exit or
// exec error is logged under "kit.provision" but never returned, so
// teardown is never blocked by a flaky pre_release hook (005:218). A
// nil/empty pre_release list is a no-op.
func (p *KitProvisioner) Release(ctx context.Context, x Execer, dir string, d *ToolchainDemand) {
	if d == nil || len(d.PreRelease) == 0 || x == nil {
		return
	}
	log := p.logger()
	for i, cmd := range d.PreRelease {
		code, err := x.Exec(ctx, dir, cmd, d.Env)
		switch {
		case err != nil:
			log.Warn("kit.provision: pre_release exec error (ignored)",
				"channel", "kit.provision", "step", i+1, "command", cmd, "err", err.Error())
		case code != 0:
			log.Warn("kit.provision: pre_release non-zero exit (ignored)",
				"channel", "kit.provision", "step", i+1, "command", cmd, "exit", code)
		}
	}
}

// provisionStep is one ordered command with its phase label for logging /
// error messages.
type provisionStep struct {
	phase string
	cmd   string
}
