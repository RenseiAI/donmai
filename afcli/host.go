package afcli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
)

// hostAliasRemovalVersion is the release in which the hidden `daemon` and
// `fleet-watch` aliases are deleted.
//
// Every alias this package registers MUST name a concrete version here (or in
// its own constant) rather than "the next release": an unfalsifiable promise is
// how the previous alias generation survived dozens of tags past its stated
// window. `host` and the aliases ship together in v0.57.0, and the aliases are
// removed one minor later.
//
// Removal precondition: launchd plists / systemd units written by an EARLIER
// build invoke `<host-binary> daemon run` — installer.DaemonSubcommand now emits
// `host run`, but the units already on disk are only rewritten by a re-run of
// `host install`. Deleting the alias before installed machines have re-installed
// stops the service on each of them, so the removal release must force (or
// verify) a re-install.
const hostAliasRemovalVersion = "v0.58.0"

// newHostCmd constructs the `host` parent command — the noun for *this
// machine*. It owns the local daemon's lifecycle, this machine's capacity
// envelope, its workarea pool, the providers and kits installed on it, the
// projects it admits work for, and the local live-session dashboard.
//
// It holds no logic of its own; every leaf is built by the same factory the
// standalone top-level command uses.
func newHostCmd(ds func() afclient.DataSource, cfg Config) *cobra.Command {
	return newHostCmdWithFactory(defaultDaemonFactory, ds, cfg)
}

// newHostCmdWithFactory is the injectable variant used in tests. factory is the
// daemon-client constructor for the lifecycle leaves.
func newHostCmdWithFactory(factory daemonClientFactory, ds func() afclient.DataSource, cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Manage this machine (daemon, capacity, workarea pool, providers, kits, projects)",
		Long: "Manage this machine: the local daemon that supervises agent session pools,\n" +
			"this machine's capacity envelope and workarea pool, the providers and kits\n" +
			"installed on it, the projects it admits work for, and the live dashboard of\n" +
			"sessions running on it.\n\n" +
			"`" + bin + " host` supersedes the per-workspace `" + bin + " worker` / `" + bin + " fleet`\n" +
			"approach. Install once, configure once, and sessions run automatically for\n" +
			"allowed projects.",
		SilenceUsage: true,
	}

	addHostLifecycleCommands(cmd, factory, cfg)

	// This machine's workarea pool, installed providers and kits, project
	// admission, and the local live-session dashboard.
	cmd.AddCommand(newProviderCmd(ds))
	cmd.AddCommand(newKitCmd(ds))
	cmd.AddCommand(newWorkareaCmd(ds))
	cmd.AddCommand(newProjectCmd(cfg))
	cmd.AddCommand(newHostWatchCmd())

	return cmd
}

// addHostLifecycleCommands attaches the daemon-lifecycle leaves to parent.
//
// The leaves are SHARED BY FACTORY, not re-parented: a *cobra.Command carries a
// single parent pointer, so the same instance cannot hang off both `host` and
// the `daemon` alias without corrupting `CommandPath()` (and therefore every
// usage string and every error message that renders it). Each call builds a
// fresh tree instead, which is the same discipline exports.go already documents
// for the daemon-targeted families. The behaviour is identical because the
// leaves are pure functions of (factory, cfg).
func addHostLifecycleCommands(parent *cobra.Command, factory daemonClientFactory, cfg Config) {
	bin := binaryName(cfg)

	parent.AddCommand(newDaemonInstallCmd(bin))
	parent.AddCommand(newDaemonUninstallCmd(bin))
	parent.AddCommand(newDaemonSetupCmd())
	parent.AddCommand(newDaemonRunCmd(cfg))
	parent.AddCommand(newDaemonStatusCmd(factory, bin))
	parent.AddCommand(newDaemonLogsCmd())
	parent.AddCommand(newDaemonDoctorCmd(bin))
	parent.AddCommand(newDaemonPauseCmd(factory, bin))
	parent.AddCommand(newDaemonResumeCmd(factory))
	parent.AddCommand(newDaemonUpdateCmd(factory))
	parent.AddCommand(newDaemonDrainCmd(factory))
	parent.AddCommand(newDaemonStopCmd(factory, bin))
	parent.AddCommand(newDaemonStatsCmd(factory, bin))
	parent.AddCommand(newDaemonEvictCmd(factory))
	parent.AddCommand(newDaemonSetCmd(factory))
}

// deprecateTree turns root into a deprecated alias of replacement, removed in
// removalVersion. Every alias MUST name a concrete version — "the next release"
// is unfalsifiable and is what let the previous alias generation outlive its
// stated window.
//
// The alias noun itself carries cobra's Deprecated marker. Its descendants
// deliberately do NOT: a deprecated command is excluded from its parent's help
// listing, which would leave `<bin> daemon --help` empty for the operators who
// have not migrated yet. Instead each descendant's Run/RunE is wrapped to
// announce itself on STDERR before doing the identical work, so
// `<bin> daemon status --json | jq` keeps parsing while the operator still sees
// the notice. The equivalent leaves under `host` are separate instances (see
// addHostLifecycleCommands) and stay unwrapped.
func deprecateTree(root *cobra.Command, replacement, removalVersion string) {
	aliasName := root.Name()
	root.Deprecated = "use `" + replacement + "` instead; the `" + aliasName +
		"` alias is removed in " + removalVersion + "."

	var walk func(cmd *cobra.Command, path string)
	walk = func(cmd *cobra.Command, path string) {
		if cmd != root {
			warnDeprecatedAlias(cmd, path, aliasName, removalVersion)
		}
		for _, child := range cmd.Commands() {
			walk(child, path+" "+child.Name())
		}
	}
	walk(root, replacement)
}

// warnDeprecatedAlias wraps cmd's Run/RunE so the invocation prints a one-line
// deprecation notice on stderr, naming the exact replacement path, before
// running unchanged. A command with neither Run nor RunE only prints help, and
// its children carry their own notice.
func warnDeprecatedAlias(cmd *cobra.Command, replacement, aliasName, removalVersion string) {
	notice := func(c *cobra.Command) {
		_, _ = fmt.Fprintf(c.ErrOrStderr(),
			"warning: `%s` is deprecated — use `%s` instead; the `%s` alias is removed in %s.\n",
			c.CommandPath(), replacement, aliasName, removalVersion)
	}
	switch {
	case cmd.RunE != nil:
		inner := cmd.RunE
		cmd.RunE = func(c *cobra.Command, args []string) error {
			notice(c)
			return inner(c, args)
		}
	case cmd.Run != nil:
		inner := cmd.Run
		cmd.Run = func(c *cobra.Command, args []string) {
			notice(c)
			inner(c, args)
		}
	}
}
