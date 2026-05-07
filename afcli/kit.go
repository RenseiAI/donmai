// Package afcli kit.go — `af kit …` Cobra commands. The commands target
// the local daemon's HTTP control API at /api/daemon/kits* and
// /api/daemon/kit-sources* per
// ADR-2026-05-07-daemon-http-control-api.md § D1; they NEVER hit the
// SaaS platform and never attach an Authorization header (D2 —
// localhost-only).
package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/afview/kit"
)

// kitDaemonClient is the subset of *afclient.DaemonClient used by the
// kit subcommands. Defining it here lets tests inject a fake without
// depending on httptest. Mirrors providerDaemonClient / daemonDoer.
type kitDaemonClient interface {
	ListKits() (*afclient.ListKitsResponse, error)
	GetKit(id string) (*afclient.KitManifestEnvelope, error)
	VerifyKitSignature(id string) (*afclient.KitSignatureResult, error)
	InstallKit(id string, req afclient.KitInstallRequest) (*afclient.KitInstallResult, error)
	EnableKit(id string) (*afclient.Kit, error)
	DisableKit(id string) (*afclient.Kit, error)
	ListKitSources() (*afclient.ListKitSourcesResponse, error)
	EnableKitSource(name string) (*afclient.KitSourceToggleResult, error)
	DisableKitSource(name string) (*afclient.KitSourceToggleResult, error)
}

// kitClientFactory builds a kitDaemonClient from a DaemonConfig. Injected
// per command-tree for test isolation; the production default returns a
// real *afclient.DaemonClient.
type kitClientFactory func(cfg afclient.DaemonConfig) kitDaemonClient

// defaultKitClientFactory is the production factory.
func defaultKitClientFactory(cfg afclient.DaemonConfig) kitDaemonClient {
	return afclient.NewDaemonClient(cfg)
}

// resolveKitDaemonConfig honours the RENSEI_DAEMON_URL env override
// in the same shape as the provider command. Empty => default
// (127.0.0.1:7734).
func resolveKitDaemonConfig() afclient.DaemonConfig {
	cfg := afclient.DefaultDaemonConfig()
	if override := strings.TrimSpace(os.Getenv(providerEnvDaemonURL)); override != "" {
		if h, p, ok := splitHTTPHostPort(override); ok {
			cfg.Host = h
			cfg.Port = p
		}
	}
	return cfg
}

// newKitCmd returns the `af kit` subcommand tree. The ds argument is
// accepted for signature consistency with the rest of afcli's command
// factories but is not used — kit commands target the local daemon, not
// the platform.
func newKitCmd(_ func() afclient.DataSource) *cobra.Command {
	return newKitCmdWithFactory(defaultKitClientFactory)
}

// newKitCmdWithFactory is the injectable variant used in tests.
func newKitCmdWithFactory(factory kitClientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kit",
		Short: "Browse, install, and manage installed kits",
		Long: `Commands for managing kits — the buildpacks-style packaging unit for
language, framework, and domain support (see 005-kit-manifest-spec.md).

Kits are queried from the local af daemon at http://127.0.0.1:7734 by
default. Set RENSEI_DAEMON_URL to override the daemon address.

Federation order for registry sources (lowest priority number = consulted first):
  1. local         — ~/.rensei/kits/*.kit.toml
  2. bundled       — shipped with the OSS execution layer
  3. rensei        — registry.rensei.dev
  4. tessl         — registry.tessl.io (Tessl tiles as kits)
  5. agentskills   — agentskills.io (SKILL.md wrapped as kits)
  6. community     — tenant-declared registries

Only the ` + "`local`" + ` source has a working backend in this wave;
` + "`install`" + ` against remote sources currently returns 501.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newKitListCmd(factory))
	cmd.AddCommand(newKitShowCmd(factory))
	cmd.AddCommand(newKitVerifySignatureCmd(factory))
	cmd.AddCommand(newKitInstallCmd(factory))
	cmd.AddCommand(newKitEnableCmd(factory))
	cmd.AddCommand(newKitDisableCmd(factory))
	cmd.AddCommand(newKitSourcesCmd(factory))
	return cmd
}

// ── list ─────────────────────────────────────────────────────────────────────

func newKitListCmd(factory kitClientFactory) *cobra.Command {
	var (
		jsonOut bool
		plain   bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed kits",
		Long: `List installed kits with their version, scope, status, and source.

Pass --json for machine-readable output. Pass --plain to suppress ANSI
escapes (also honours NO_COLOR=1).`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := factory(resolveKitDaemonConfig())
			return runKitList(cmd.OutOrStdout(), client, jsonOut, plain)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plain, "plain", false, "Plain text (no ANSI / emoji)")
	return cmd
}

func runKitList(out io.Writer, client kitDaemonClient, jsonOut, plain bool) error {
	resp, err := client.ListKits()
	if err != nil {
		return fmt.Errorf("list kits: %w", err)
	}
	if jsonOut {
		return encodeKitJSON(out, resp)
	}
	noColor := plain || kit.NoColorEnv()
	return kit.RenderList(out, resp.Kits, noColor)
}

// ── show ─────────────────────────────────────────────────────────────────────

func newKitShowCmd(factory kitClientFactory) *cobra.Command {
	var (
		jsonOut bool
		plain   bool
	)
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a kit's manifest, providers, and signature state",
		Long: `Show the full manifest for a kit, including identity, runtime state,
detect rules, provided contributions, and composition order.

Trust symbols (NO_COLOR=1 or --plain replaces these with bracketed labels):
  ✅  signed and verified
  ⚠   signed but unverified
  🔓  unsigned`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveKitDaemonConfig())
			return runKitShow(cmd.OutOrStdout(), client, args[0], jsonOut, plain)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plain, "plain", false, "Plain text (no ANSI / emoji)")
	return cmd
}

func runKitShow(out io.Writer, client kitDaemonClient, id string, jsonOut, plain bool) error {
	env, err := client.GetKit(id)
	if err != nil {
		if errors.Is(err, afclient.ErrNotFound) {
			return fmt.Errorf("kit not found: %s", id)
		}
		return fmt.Errorf("show kit: %w", err)
	}
	if jsonOut {
		return encodeKitJSON(out, env)
	}
	noColor := plain || kit.NoColorEnv()
	return kit.RenderShow(out, &env.Kit, noColor)
}

// ── verify-signature ─────────────────────────────────────────────────────────

func newKitVerifySignatureCmd(factory kitClientFactory) *cobra.Command {
	var (
		jsonOut bool
		plain   bool
	)
	cmd := &cobra.Command{
		Use:     "verify <id>",
		Aliases: []string{"verify-signature"},
		Short:   "Run a signature check on a kit and display its trust state",
		Long: `Verify the cryptographic signature of a kit manifest and display the
resulting trust state.

Wave 9 caveat: signing is partially implemented; the daemon currently
reports trust=unsigned for all installed kits even when the manifest
declares an authorIdentity. See 005-kit-manifest-spec.md § Trust
verification.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveKitDaemonConfig())
			return runKitVerifySignature(cmd.OutOrStdout(), client, args[0], jsonOut, plain)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plain, "plain", false, "Plain text (no ANSI / emoji)")
	return cmd
}

func runKitVerifySignature(out io.Writer, client kitDaemonClient, id string, jsonOut, plain bool) error {
	res, err := client.VerifyKitSignature(id)
	if err != nil {
		if errors.Is(err, afclient.ErrNotFound) {
			return fmt.Errorf("kit not found: %s", id)
		}
		return fmt.Errorf("verify-signature: %w", err)
	}
	if jsonOut {
		return encodeKitJSON(out, res)
	}
	noColor := plain || kit.NoColorEnv()
	return kit.RenderVerifySignature(out, res, noColor)
}

// ── install ──────────────────────────────────────────────────────────────────

func newKitInstallCmd(factory kitClientFactory) *cobra.Command {
	var (
		version string
		jsonOut bool
		plain   bool
	)
	cmd := &cobra.Command{
		Use:   "install <id>",
		Short: "Install a kit from a configured registry source",
		Long: `Install a kit by id from one of the configured registry sources.

Wave 9 caveat: only locally-installed kits are supported (.kit.toml under
~/.rensei/kits). Remote-registry fetch is deferred and currently returns
HTTP 501. The command surface is finalised so the call is stable as the
backend lands.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveKitDaemonConfig())
			return runKitInstall(cmd.OutOrStdout(), client, args[0], version, jsonOut, plain)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Install a specific version (default: latest compatible)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plain, "plain", false, "Plain text (no ANSI / emoji)")
	return cmd
}

func runKitInstall(out io.Writer, client kitDaemonClient, id, version string, jsonOut, plain bool) error {
	res, err := client.InstallKit(id, afclient.KitInstallRequest{Version: version})
	if err != nil {
		if errors.Is(err, afclient.ErrNotFound) {
			return fmt.Errorf("kit not found: %s", id)
		}
		return fmt.Errorf("install kit: %w", err)
	}
	if jsonOut {
		return encodeKitJSON(out, res)
	}
	noColor := plain || kit.NoColorEnv()
	return kit.RenderInstall(out, res, noColor)
}

// ── enable / disable ─────────────────────────────────────────────────────────

func newKitEnableCmd(factory kitClientFactory) *cobra.Command {
	var (
		jsonOut bool
		plain   bool
	)
	cmd := &cobra.Command{
		Use:          "enable <id>",
		Short:        "Re-enable a previously disabled kit",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveKitDaemonConfig())
			return runKitToggle(cmd.OutOrStdout(), client, args[0], true, jsonOut, plain)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plain, "plain", false, "Plain text (no ANSI / emoji)")
	return cmd
}

func newKitDisableCmd(factory kitClientFactory) *cobra.Command {
	var (
		jsonOut bool
		plain   bool
	)
	cmd := &cobra.Command{
		Use:          "disable <id>",
		Short:        "Disable a kit without uninstalling it",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveKitDaemonConfig())
			return runKitToggle(cmd.OutOrStdout(), client, args[0], false, jsonOut, plain)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plain, "plain", false, "Plain text (no ANSI / emoji)")
	return cmd
}

func runKitToggle(out io.Writer, client kitDaemonClient, id string, enable, jsonOut, plain bool) error {
	var (
		k   *afclient.Kit
		err error
	)
	if enable {
		k, err = client.EnableKit(id)
	} else {
		k, err = client.DisableKit(id)
	}
	if err != nil {
		if errors.Is(err, afclient.ErrNotFound) {
			return fmt.Errorf("kit not found: %s", id)
		}
		verb := "disable"
		if enable {
			verb = "enable"
		}
		return fmt.Errorf("%s kit: %w", verb, err)
	}
	if jsonOut {
		return encodeKitJSON(out, k)
	}
	noColor := plain || kit.NoColorEnv()
	return kit.RenderToggle(out, k, enable, noColor)
}

// ── sources ──────────────────────────────────────────────────────────────────

func newKitSourcesCmd(factory kitClientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "Manage kit registry sources and their federation order",
		Long: `Commands for listing and toggling kit registry sources.

Sources control where kit manifests are discovered. See the parent
kit command's --help for the federation order.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newKitSourcesListCmd(factory))
	cmd.AddCommand(newKitSourcesEnableCmd(factory))
	cmd.AddCommand(newKitSourcesDisableCmd(factory))
	return cmd
}

func newKitSourcesListCmd(factory kitClientFactory) *cobra.Command {
	var (
		jsonOut bool
		plain   bool
	)
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List configured kit registry sources",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := factory(resolveKitDaemonConfig())
			return runKitSourcesList(cmd.OutOrStdout(), client, jsonOut, plain)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plain, "plain", false, "Plain text (no ANSI / emoji)")
	return cmd
}

func runKitSourcesList(out io.Writer, client kitDaemonClient, jsonOut, plain bool) error {
	resp, err := client.ListKitSources()
	if err != nil {
		return fmt.Errorf("list kit sources: %w", err)
	}
	if jsonOut {
		return encodeKitJSON(out, resp)
	}
	noColor := plain || kit.NoColorEnv()
	return kit.RenderSources(out, resp.Sources, noColor)
}

func newKitSourcesEnableCmd(factory kitClientFactory) *cobra.Command {
	var (
		jsonOut bool
		plain   bool
	)
	cmd := &cobra.Command{
		Use:          "enable <name>",
		Short:        "Enable a registry source by name",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveKitDaemonConfig())
			return runKitSourcesToggle(cmd.OutOrStdout(), client, args[0], true, jsonOut, plain)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plain, "plain", false, "Plain text (no ANSI / emoji)")
	return cmd
}

func newKitSourcesDisableCmd(factory kitClientFactory) *cobra.Command {
	var (
		jsonOut bool
		plain   bool
	)
	cmd := &cobra.Command{
		Use:          "disable <name>",
		Short:        "Disable a registry source by name",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveKitDaemonConfig())
			return runKitSourcesToggle(cmd.OutOrStdout(), client, args[0], false, jsonOut, plain)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plain, "plain", false, "Plain text (no ANSI / emoji)")
	return cmd
}

func runKitSourcesToggle(out io.Writer, client kitDaemonClient, name string, enable, jsonOut, plain bool) error {
	var (
		res *afclient.KitSourceToggleResult
		err error
	)
	if enable {
		res, err = client.EnableKitSource(name)
	} else {
		res, err = client.DisableKitSource(name)
	}
	if err != nil {
		if errors.Is(err, afclient.ErrNotFound) {
			return fmt.Errorf("kit source not found: %s", name)
		}
		verb := "disable"
		if enable {
			verb = "enable"
		}
		return fmt.Errorf("%s kit source: %w", verb, err)
	}
	if jsonOut {
		return encodeKitJSON(out, res)
	}
	action := "disabled"
	if enable {
		action = "enabled"
	}
	noColor := plain || kit.NoColorEnv()
	sym := "\033[32m✓\033[0m"
	if noColor {
		sym = "OK"
	}
	if _, ferr := fmt.Fprintf(out, "%s source %s %s\n", sym, res.Source.Name, action); ferr != nil {
		return fmt.Errorf("write toggle line: %w", ferr)
	}
	return nil
}

// encodeKitJSON writes v as pretty-printed JSON to out.
func encodeKitJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}
